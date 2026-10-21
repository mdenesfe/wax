package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/appstate"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/proto/wae2e"
	"github.com/mdenesfe/wax/proto/wasyncaction"
)

// recoveryInterval limits fatal-recovery requests per collection.
const recoveryInterval = time.Hour

// requestRecovery asks our primary device for a verified copy of a
// collection whose server-side state does not verify. WhatsApp clients do
// the same: a corrupt server snapshot cannot be repaired otherwise.
func (c *Client) requestRecovery(ctx context.Context, s *session, collection string) {
	if !c.appState.shouldRequestRecovery(collection) {
		return
	}
	msg := &wae2e.Message{ProtocolMessage: &wae2e.ProtocolMessage{
		Type: wae2e.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE.Enum(),
		PeerDataOperationRequestMessage: &wae2e.PeerDataOperationRequestMessage{
			PeerDataOperationRequestType: wae2e.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY.Enum(),
			SyncdCollectionFatalRecoveryRequest: &wae2e.PeerDataOperationRequestMessage_SyncDCollectionFatalRecoveryRequest{
				CollectionName: proto.String(collection),
				Timestamp:      proto.Int64(time.Now().Unix()),
			},
		},
	}}
	s.log.Info("requesting app state recovery from the phone", "collection", collection)
	if err := c.sendPeerMessage(ctx, s, msg); err != nil {
		s.log.Warn("app state recovery request failed", "collection", collection, "error", err)
	}
}

func (a *appStateSync) shouldRequestRecovery(collection string) bool {
	return a.shouldRequest("recovery:"+collection, recoveryInterval)
}

// handlePeerDataResponse processes our phone's answer to a peer data
// request. Only fatal recovery responses are handled.
func (s *session) handlePeerDataResponse(resp *wae2e.PeerDataOperationRequestResponseMessage) error {
	if resp.GetPeerDataOperationRequestType() != wae2e.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY {
		return nil
	}
	for _, r := range resp.GetPeerDataOperationResult() {
		fr := r.GetSyncdSnapshotFatalRecoveryResponse()
		if fr == nil {
			continue
		}
		data := fr.GetCollectionSnapshot()
		if fr.GetIsCompressed() {
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return fmt.Errorf("recovery: gzip: %w", err)
			}
			data, err = io.ReadAll(io.LimitReader(zr, maxHistorySize))
			zr.Close()
			if err != nil {
				return fmt.Errorf("recovery: inflate: %w", err)
			}
		}
		var rec wasyncaction.SyncdSnapshotRecovery
		if err := proto.Unmarshal(data, &rec); err != nil {
			return fmt.Errorf("recovery: decode: %w", err)
		}
		if err := s.c.applyRecovery(s.ctx, s, &rec); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) applyRecovery(ctx context.Context, s *session, rec *wasyncaction.SyncdSnapshotRecovery) error {
	collection := rec.GetCollectionName()
	known := false
	for _, n := range appstate.AllCollections {
		known = known || n == collection
	}
	if !known {
		return fmt.Errorf("recovery for unknown collection %q", collection)
	}
	c.appState.mu.Lock()
	defer c.appState.mu.Unlock()
	cur, err := c.cfg.Store.AppStateVersion(ctx, c.cfg.InstanceID, collection)
	if err != nil {
		return err
	}
	// The phone's copy replaces an unverified state of the same version.
	got := rec.GetVersion().GetVersion()
	if cur.Version != 0 && (cur.Version > got || cur.Version == got && !cur.Unverified) {
		s.log.Info("ignoring stale app state recovery", "collection", collection, "have", cur.Version, "got", got)
		return nil
	}
	var res *appstate.Result
	err = c.cfg.Store.InTx(ctx, func(tx *store.Store) error {
		var err error
		l := &storeLookup{ctx: ctx, tx: tx, instance: c.cfg.InstanceID, collection: collection}
		if res, err = appstate.ApplyRecovery(rec, l); err != nil {
			return err
		}
		if err := tx.ResetAppState(ctx, c.cfg.InstanceID, collection); err != nil {
			return err
		}
		for _, m := range res.MACs {
			if err := tx.PutAppStateMAC(ctx, c.cfg.InstanceID, collection, m.IndexMAC, m.ValueMAC); err != nil {
				return err
			}
		}
		for i := range res.Mutations {
			if err := c.applyMutation(ctx, s, tx, &res.Mutations[i]); err != nil {
				return err
			}
		}
		return tx.PutAppStateVersion(ctx, c.cfg.InstanceID, collection, res.State)
	})
	if errors.Is(err, appstate.ErrKeyNotFound) {
		return nil // keys arrive separately; the recovery can be requested again
	}
	if err != nil {
		return fmt.Errorf("apply recovery: %w", err)
	}
	s.log.Info("app state recovered from the phone", "collection", collection, "version", res.State.Version, "records", len(res.Mutations))
	c.emit(AppStateSyncComplete{Collection: collection, Version: res.State.Version})
	// Continue with any patches newer than the recovered version.
	s.goSafe("appstate-after-recovery", func() {
		if err := c.syncAppState(s.ctx, s, collection, false); err != nil && !errors.Is(err, errMissingKeys) {
			s.log.Warn("app state sync after recovery failed", "collection", collection, "error", err)
		}
	})
	return nil
}
