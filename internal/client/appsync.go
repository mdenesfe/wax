package client

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/appstate"
	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/proto/waserversync"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/store"
)

// errMissingKeys means app state could not be decoded until the phone
// shares more keys; a key request was sent and a later key share retries.
var errMissingKeys = errors.New("client: app state keys missing")

// appStateSync serializes syncs per instance and remembers requested keys.
type appStateSync struct {
	mu        sync.Mutex
	reqMu     sync.Mutex
	requested map[string]time.Time // key id -> when we asked the phone
}

// shouldRequest reports whether a key may be requested now (at most once a
// minute per key, so a slow phone is not flooded).
func (a *appStateSync) shouldRequest(id string) bool {
	a.reqMu.Lock()
	defer a.reqMu.Unlock()
	if a.requested == nil {
		a.requested = map[string]time.Time{}
	}
	if t, ok := a.requested[id]; ok && time.Since(t) < time.Minute {
		return false
	}
	a.requested[id] = time.Now()
	return true
}

// storeLookup adapts the store to appstate.Lookup and records missing keys.
type storeLookup struct {
	ctx        context.Context
	tx         *store.Store
	instance   string
	collection string
	missing    map[string][]byte
}

func (l *storeLookup) Key(id []byte) ([]byte, error) {
	k, err := l.tx.AppStateKey(l.ctx, l.instance, id)
	if errors.Is(err, store.ErrNotFound) {
		if l.missing != nil {
			l.missing[string(id)] = id
		}
		return nil, appstate.ErrKeyNotFound
	}
	return k, err
}

func (l *storeLookup) ValueMAC(indexMAC []byte) ([]byte, error) {
	return l.tx.AppStateValueMAC(l.ctx, l.instance, l.collection, indexMAC)
}

// syncAllAppState syncs every collection (best effort, logged).
func (c *Client) syncAllAppState(ctx context.Context, s *session) {
	for _, name := range appstate.AllCollections {
		if err := c.syncAppState(ctx, s, name, false); err != nil {
			if errors.Is(err, errMissingKeys) {
				s.log.Info("app state waiting for keys from the phone", "collection", name)
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("app state sync failed", "collection", name, "error", err)
		}
	}
}

// syncAppState brings one collection up to date. On an integrity failure
// it resets the collection and retries once from a full snapshot.
func (c *Client) syncAppState(ctx context.Context, s *session, collection string, full bool) error {
	c.appState.mu.Lock()
	defer c.appState.mu.Unlock()
	err := c.syncAppStateLocked(ctx, s, collection, full)
	if !full && (errors.Is(err, appstate.ErrSnapshotMAC) || errors.Is(err, appstate.ErrPatchMAC) ||
		errors.Is(err, appstate.ErrContentMAC) || errors.Is(err, appstate.ErrIndexMAC)) {
		s.log.Warn("app state integrity check failed; resyncing from snapshot", "collection", collection, "error", err)
		if rerr := c.cfg.Store.ResetAppState(ctx, c.cfg.InstanceID, collection); rerr != nil {
			return rerr
		}
		err = c.syncAppStateLocked(ctx, s, collection, true)
		if errors.Is(err, appstate.ErrSnapshotMAC) || errors.Is(err, appstate.ErrPatchMAC) {
			// The server's copy itself does not verify; only the phone can help.
			c.requestRecovery(ctx, s, collection)
		}
	}
	if err != nil && !errors.Is(err, errMissingKeys) && ctx.Err() == nil {
		c.emit(AppStateSyncError{Collection: collection, Err: err})
	}
	return err
}

func (c *Client) syncAppStateLocked(ctx context.Context, s *session, collection string, full bool) error {
	version, hash, err := c.cfg.Store.AppStateVersion(ctx, c.cfg.InstanceID, collection)
	if err != nil {
		return err
	}
	if version == 0 {
		full = true
	}
	state := appstate.HashState{Version: version, Hash: hash}
	wantSnapshot := full
	applied := 0
	for {
		snapshot, patches, hasMore, err := s.fetchAppStatePatches(ctx, collection, state.Version, wantSnapshot)
		if err != nil {
			return err
		}
		wantSnapshot = false
		missing := map[string][]byte{}
		if snapshot != nil {
			if state, err = c.applyAppState(ctx, s, collection, state, snapshot, nil, full, missing); err != nil {
				return c.maybeRequestKeys(ctx, s, missing, err)
			}
			applied += len(snapshot.GetRecords())
		}
		for _, p := range patches {
			if state, err = c.applyAppState(ctx, s, collection, state, nil, p, full, missing); err != nil {
				return c.maybeRequestKeys(ctx, s, missing, err)
			}
			applied += len(p.GetMutations())
		}
		if !hasMore {
			break
		}
	}
	s.log.Info("app state synced", "collection", collection, "version", state.Version, "mutations", applied, "full", full)
	if full {
		c.emit(AppStateSyncComplete{Collection: collection, Version: state.Version})
	}
	return nil
}

func (c *Client) maybeRequestKeys(ctx context.Context, s *session, missing map[string][]byte, err error) error {
	if len(missing) == 0 || !errors.Is(err, appstate.ErrKeyNotFound) {
		return err
	}
	var ids [][]byte
	for k, id := range missing {
		if c.appState.shouldRequest(k) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		req := &wae2e.Message{ProtocolMessage: &wae2e.ProtocolMessage{
			Type:                   wae2e.ProtocolMessage_APP_STATE_SYNC_KEY_REQUEST.Enum(),
			AppStateSyncKeyRequest: &wae2e.AppStateSyncKeyRequest{},
		}}
		for _, id := range ids {
			req.ProtocolMessage.AppStateSyncKeyRequest.KeyIDs = append(req.ProtocolMessage.AppStateSyncKeyRequest.KeyIDs, &wae2e.AppStateSyncKeyId{KeyID: id})
		}
		s.log.Info("requesting app state keys from the phone", "count", len(ids))
		if serr := c.sendPeerMessage(ctx, s, req); serr != nil {
			s.log.Warn("app state key request failed", "error", serr)
		}
	}
	return fmt.Errorf("%w: %w", errMissingKeys, err)
}

// applyAppState verifies one snapshot or patch and commits it together with
// everything derived from it, in one transaction.
func (c *Client) applyAppState(ctx context.Context, s *session, collection string, state appstate.HashState,
	snapshot *waserversync.SyncdSnapshot, patch *waserversync.SyncdPatch, fullSync bool, missing map[string][]byte) (appstate.HashState, error) {
	var res *appstate.Result
	err := c.cfg.Store.InTx(ctx, func(tx *store.Store) error {
		l := &storeLookup{ctx: ctx, tx: tx, instance: c.cfg.InstanceID, collection: collection, missing: missing}
		var err error
		if snapshot != nil {
			if err := tx.ResetAppState(ctx, c.cfg.InstanceID, collection); err != nil {
				return err
			}
			res, err = appstate.ApplySnapshot(collection, snapshot, l)
		} else {
			res, err = appstate.ApplyPatch(collection, state, patch, l)
		}
		if err != nil {
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
		return tx.PutAppStateVersion(ctx, c.cfg.InstanceID, collection, res.State.Version, res.State.Hash)
	})
	if err != nil {
		return state, err
	}
	for _, m := range res.Mutations {
		c.emit(AppStateChange{Collection: collection, Index: m.Index, Action: m.Action,
			Removed: m.Operation == waserversync.SyncdMutation_REMOVE, FromFullSync: fullSync})
	}
	return res.State, nil
}

// applyMutation updates derived tables for one mutation.
func (c *Client) applyMutation(ctx context.Context, s *session, tx *store.Store, m *appstate.Mutation) error {
	inst := c.cfg.InstanceID
	removed := m.Operation == waserversync.SyncdMutation_REMOVE
	jid := ""
	if len(m.Index) > 1 {
		jid = m.Index[1]
	}
	a := m.Action
	switch m.Index[0] {
	case "nct_salt_sync":
		if removed {
			return tx.PutNCTSalt(ctx, inst, nil)
		}
		return tx.PutNCTSalt(ctx, inst, a.GetNctSaltSyncAction().GetSalt())
	}
	if removed || a == nil {
		return nil
	}
	switch m.Index[0] {
	case "contact":
		ca := a.GetContactAction()
		if err := tx.PutContactName(ctx, inst, jid, ca.GetFirstName(), ca.GetFullName()); err != nil {
			return err
		}
		if pn, lid := ca.GetPnJID(), ca.GetLidJID(); pn != "" && lid != "" {
			pj, e1 := wabin.ParseJID(pn)
			lj, e2 := wabin.ParseJID(lid)
			if e1 == nil && e2 == nil && pj.Server == wabin.ServerUser && lj.Server == wabin.ServerLID {
				if _, err := tx.PutLIDMapping(ctx, inst, pj.User, lj.User); err != nil {
					return err
				}
			}
		}
	case "lid_contact":
		la := a.GetLidContactAction()
		return tx.PutContactName(ctx, inst, jid, la.GetFirstName(), la.GetFullName())
	case "mute":
		var until time.Time
		if ma := a.GetMuteAction(); ma.GetMuted() {
			if end := ma.GetMuteEndTimestamp(); end < 0 {
				until = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			} else {
				until = time.UnixMilli(end)
			}
		}
		return tx.UpdateChat(ctx, inst, jid, store.ChatUpdate{MutedUntil: &until})
	case "pin_v1":
		pinned := a.GetPinAction().GetPinned()
		return tx.UpdateChat(ctx, inst, jid, store.ChatUpdate{Pinned: &pinned})
	case "archive":
		archived := a.GetArchiveChatAction().GetArchived()
		return tx.UpdateChat(ctx, inst, jid, store.ChatUpdate{Archived: &archived})
	case "markChatAsRead":
		if a.GetMarkChatAsReadAction().GetRead() {
			zero := 0
			return tx.UpdateChat(ctx, inst, jid, store.ChatUpdate{UnreadCount: &zero})
		}
	case "setting_pushName":
		name := a.GetPushNameSetting().GetName()
		if name != "" {
			if err := tx.UpdatePushName(ctx, inst, name); err != nil {
				return err
			}
			c.pushName.Store(name)
		}
	}
	return nil
}

// fetchAppStatePatches asks the server for a collection's snapshot and/or
// patches after a version, downloading external blobs.
func (s *session) fetchAppStatePatches(ctx context.Context, collection string, from uint64, snapshot bool) (*waserversync.SyncdSnapshot, []*waserversync.SyncdPatch, bool, error) {
	attrs := wabin.Attrs{"name": collection, "return_snapshot": strconv.FormatBool(snapshot)}
	if !snapshot {
		attrs["version"] = strconv.FormatUint(from, 10)
	}
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:     "iq",
		Attrs:   wabin.Attrs{"to": wabin.ServerJID, "type": "set", "xmlns": "w:sync:app:state"},
		Content: []wabin.Node{{Tag: "sync", Content: []wabin.Node{{Tag: "collection", Attrs: attrs}}}},
	}, 0)
	if err != nil {
		return nil, nil, false, fmt.Errorf("fetch %s: %w", collection, err)
	}
	sn, ok := resp.Child("sync")
	if !ok {
		return nil, nil, false, fmt.Errorf("fetch %s: no <sync>", collection)
	}
	coll, ok := sn.Child("collection")
	if !ok {
		return nil, nil, false, fmt.Errorf("fetch %s: no <collection>", collection)
	}
	if e, ok := coll.Child("error"); ok {
		return nil, nil, false, fmt.Errorf("fetch %s: server error %s", collection, e.String())
	}
	hasMore, _ := coll.AttrString("has_more_patches")
	var ss *waserversync.SyncdSnapshot
	if snapNode, ok := coll.Child("snapshot"); ok {
		raw, _ := snapNode.Bytes()
		var ref waserversync.ExternalBlobReference
		if err := proto.Unmarshal(raw, &ref); err != nil {
			return nil, nil, false, fmt.Errorf("snapshot reference: %w", err)
		}
		blob, err := s.c.downloadExternal(ctx, &ref)
		if err != nil {
			return nil, nil, false, fmt.Errorf("download %s snapshot: %w", collection, err)
		}
		ss = &waserversync.SyncdSnapshot{}
		if err := proto.Unmarshal(blob, ss); err != nil {
			return nil, nil, false, fmt.Errorf("snapshot: %w", err)
		}
	}
	var patches []*waserversync.SyncdPatch
	if pn, ok := coll.Child("patches"); ok {
		for _, p := range pn.ChildrenByTag("patch") {
			raw, _ := p.Bytes()
			var patch waserversync.SyncdPatch
			if err := proto.Unmarshal(raw, &patch); err != nil {
				return nil, nil, false, fmt.Errorf("patch: %w", err)
			}
			if ext := patch.GetExternalMutations(); ext != nil {
				blob, err := s.c.downloadExternal(ctx, ext)
				if err != nil {
					return nil, nil, false, fmt.Errorf("download external mutations: %w", err)
				}
				var muts waserversync.SyncdMutations
				if err := proto.Unmarshal(blob, &muts); err != nil {
					return nil, nil, false, fmt.Errorf("external mutations: %w", err)
				}
				patch.Mutations = muts.GetMutations()
			}
			patches = append(patches, &patch)
		}
	}
	return ss, patches, hasMore == "true", nil
}

func (c *Client) downloadExternal(ctx context.Context, ref *waserversync.ExternalBlobReference) ([]byte, error) {
	return c.DownloadMedia(ctx, ref.GetDirectPath(), ref.GetMediaKey(), ref.GetFileEncSHA256(), ref.GetFileSHA256(), media.AppState)
}

// handleKeyShare stores app state keys shared by our primary device. It runs
// before the carrying message is acknowledged.
func (s *session) handleKeyShare(share *wae2e.AppStateSyncKeyShare) error {
	n := 0
	for _, k := range share.GetKeys() {
		id, data := k.GetKeyID().GetKeyID(), k.GetKeyData().GetKeyData()
		if len(id) == 0 || len(data) == 0 {
			continue
		}
		fp, _ := proto.Marshal(k.GetKeyData().GetFingerprint())
		if err := s.c.cfg.Store.PutAppStateKey(s.ctx, s.c.cfg.InstanceID, id, data, fp, k.GetKeyData().GetTimestamp()); err != nil {
			return err
		}
		n++
	}
	s.log.Info("stored app state keys", "count", n)
	s.goSafe("appstate-sync", func() { s.c.syncAllAppState(s.ctx, s) })
	return nil
}

// sendPeerMessage sends a protocol message to our own primary device
// (category "peer"), e.g. an app state key request.
func (c *Client) sendPeerMessage(ctx context.Context, s *session, msg *wae2e.Message) error {
	primary := s.dev.JID.ToNonAD()
	enc := c.encryptionJIDs(ctx, []wabin.JID{primary})[primary]
	plaintext, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	var bundle *signal.PreKeyBundle
	have, err := c.cfg.Store.HasSessions(ctx, c.cfg.InstanceID, []string{signalAddress(enc)})
	if err != nil {
		return err
	}
	if !have[signalAddress(enc)] {
		bundles, err := s.fetchBundles(ctx, []wabin.JID{enc})
		if err != nil {
			return err
		}
		bundle = bundles[enc]
	}
	node, isPreKey, err := c.encryptForDevice(ctx, signal.LocalIdentity{Key: &s.dev.IdentityKey.KeyPair, RegistrationID: s.dev.RegistrationID}, enc, bundle, padMessage(plaintext), nil)
	if err != nil {
		return err
	}
	id := newMessageID(s.dev.JID)
	content := []wabin.Node{{Tag: "meta", Attrs: wabin.Attrs{"appdata": "default"}}, *node}
	if isPreKey {
		content = append(content, wabin.Node{Tag: "device-identity", Content: s.dev.Account})
	}
	_, err = s.sendMessageNode(ctx, wabin.Node{Tag: "message", Attrs: wabin.Attrs{
		"id": id, "type": "text", "category": "peer", "to": primary, "push_priority": "high",
	}, Content: content}, id, primary)
	return err
}
