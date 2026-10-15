package client

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/appstate"
	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/proto/wahistorysync"
	"github.com/mdenesfe/wax/internal/proto/waserversync"
	"github.com/mdenesfe/wax/internal/proto/wasyncaction"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/testutil/fakewa"
)

// syncServer plays WhatsApp's app state endpoints and our phone.
type syncServer struct {
	t       *testing.T
	h       *harness
	c       *fakewa.Conn
	host    string // fake media server
	ownLID  wabin.JID
	phone   *peer
	toUs    *signal.Record // phone -> our device session
	keyID   []byte
	keyData []byte
	// what the server returns for the "regular" collection
	snapshot *waserversync.ExternalBlobReference
	patches  []*waserversync.SyncdPatch
	version  uint64
	keyReqs  int
	// onPeer, if set, receives every decrypted peer message instead of the
	// default key-request check.
	onPeer func(*wae2e.Message) error
}

// uploadBlob encrypts data as an app state blob on the fake media server.
func (ss *syncServer) uploadBlob(data []byte) *waserversync.ExternalBlobReference {
	enc, err := media.Encrypt(data, media.AppState)
	must(ss.t, err)
	conn := &media.Conn{Auth: "media-auth", Hosts: []string{ss.host}}
	// Reuse the upload path of the fake server (mms type does not matter there).
	res, err := (&media.Transfer{Scheme: "http"}).Upload(ss.h.ctx, conn, enc, media.Image)
	must(ss.t, err)
	return &waserversync.ExternalBlobReference{MediaKey: enc.MediaKey, DirectPath: proto.String(res.DirectPath),
		FileSHA256: enc.FileSHA256, FileEncSHA256: enc.FileEncSHA256, FileSizeBytes: proto.Uint64(enc.FileLength)}
}

// serveUntil answers app state, media_conn, prekey and peer messages until
// done returns true for a received node. It returns errors instead of
// failing the test so it can also run on a background goroutine.
func (ss *syncServer) serveUntil(done func(*wabin.Node) bool) (*wabin.Node, error) {
	h, c := ss.h, ss.c
	for {
		n, err := c.ReadUntil(h.ctx, func(*wabin.Node) bool { return true })
		if err != nil {
			return nil, err
		}
		if done(n) {
			return n, nil
		}
		id, _ := n.AttrString("id")
		var reply *wabin.Node
		switch {
		case n.Tag == "iq" && n.Attrs["xmlns"] == "w:m":
			r := fakewa.Result(id, wabin.Node{Tag: "media_conn", Attrs: wabin.Attrs{"auth": "media-auth", "ttl": 3600},
				Content: []wabin.Node{{Tag: "host", Attrs: wabin.Attrs{"hostname": ss.host}}}})
			reply = &r
		case n.Tag == "iq" && n.Attrs["xmlns"] == "w:sync:app:state":
			sn, _ := n.Child("sync")
			coll, _ := sn.Child("collection")
			name, _ := coll.AttrString("name")
			out := wabin.Node{Tag: "collection", Attrs: wabin.Attrs{"name": name, "version": ss.version}}
			if name == appstate.Regular {
				var content []wabin.Node
				if coll.Attrs["return_snapshot"] == "true" && ss.snapshot != nil {
					raw, _ := proto.Marshal(ss.snapshot)
					content = append(content, wabin.Node{Tag: "snapshot", Content: raw})
				}
				if len(ss.patches) > 0 && coll.Attrs["return_snapshot"] != "true" {
					var ps []wabin.Node
					for _, p := range ss.patches {
						raw, _ := proto.Marshal(p)
						ps = append(ps, wabin.Node{Tag: "patch", Content: raw})
					}
					content = append(content, wabin.Node{Tag: "patches", Content: ps})
				}
				if len(content) > 0 {
					out.Content = content
				}
			}
			r := fakewa.Result(id, wabin.Node{Tag: "sync", Content: []wabin.Node{out}})
			reply = &r
		case n.Tag == "iq" && n.Attrs["xmlns"] == "encrypt":
			r := fakewa.Result(id, wabin.Node{Tag: "list", Content: []wabin.Node{ss.phone.bundleNode()}})
			reply = &r
		case n.Tag == "message" && n.Attrs["category"] == "peer":
			if err := ss.checkKeyRequest(n); err != nil {
				return nil, err
			}
			ss.keyReqs++
			r := wabin.Node{Tag: "ack", Attrs: wabin.Attrs{"class": "message", "id": id, "t": "1700000000"}}
			reply = &r
		}
		if reply != nil {
			if err := c.Send(h.ctx, *reply); err != nil {
				return nil, err
			}
		}
		if done(n) { // conditions on server-side state may hold now
			return n, nil
		}
	}
}

// checkKeyRequest decrypts a peer message on the phone and verifies it is
// a request for our key.
func (ss *syncServer) checkKeyRequest(n *wabin.Node) error {
	enc, _ := n.Child("enc")
	ct, _ := enc.Bytes()
	addr := signalAddress(wabin.JID{User: ss.ownLID.User, Device: 12, Server: wabin.ServerLID})
	rec := ss.phone.sessions[addr]
	if rec == nil {
		rec = &signal.Record{}
		ss.phone.sessions[addr] = rec
	}
	var padded []byte
	if enc.Attrs["type"] == "pkmsg" {
		res, err := rec.DecryptPreKeyMessage(ss.phone.local(), ss.phone, ct)
		if err != nil {
			return err
		}
		padded = res.Plaintext
	} else {
		var err error
		if padded, err = rec.DecryptMessage(ct); err != nil {
			return err
		}
	}
	pt, err := unpadMessage(padded)
	if err != nil {
		return err
	}
	var m wae2e.Message
	if err := proto.Unmarshal(pt, &m); err != nil {
		return err
	}
	if ss.onPeer != nil {
		return ss.onPeer(&m)
	}
	req := m.GetProtocolMessage().GetAppStateSyncKeyRequest()
	if req == nil || len(req.GetKeyIDs()) != 1 || !bytes.Equal(req.GetKeyIDs()[0].GetKeyID(), ss.keyID) {
		return fmt.Errorf("unexpected peer message %v", &m)
	}
	return nil
}

// fromPhone wraps a protocol message from our primary device.
func (ss *syncServer) fromPhone(id string, pm *wae2e.ProtocolMessage) wabin.Node {
	return wabin.Node{Tag: "message", Attrs: wabin.Attrs{
		"from": wabin.JID{User: ss.ownLID.User, Device: 0, Server: wabin.ServerLID}, "id": id, "t": "1700000000", "type": "protocol", "category": "peer",
	}, Content: []wabin.Node{encNode(ss.t, ss.toUs, &wae2e.Message{ProtocolMessage: pm})}}
}

func TestAppStateAndHistorySync(t *testing.T) {
	h, c, ours := loggedIn(t, nil)
	h.client.mediaScheme = "http"
	_, host := fakeMediaServer(t)
	ownLID := wabin.JID{User: "1" + testUser, Server: wabin.ServerLID}
	ss := &syncServer{t: t, h: h, c: c, host: host, ownLID: ownLID,
		phone: newPeer(t, wabin.JID{User: ownLID.User, Server: wabin.ServerLID}, 1111),
		toUs:  &signal.Record{}, keyID: []byte{0, 0, 0, 7}, keyData: bytes.Repeat([]byte{9}, 32), version: 3}
	must(t, ss.toUs.ProcessBundle(ss.phone.local(), ours))

	// The phone's view of the "regular" collection: a contact, a mute and
	// our push name.
	type rec struct {
		index []string
		value *wasyncaction.SyncActionValue
	}
	ts := proto.Int64(1700000000000)
	records := []rec{
		{[]string{"contact", bobPN.String()}, &wasyncaction.SyncActionValue{Timestamp: ts, ContactAction: &wasyncaction.ContactAction{FirstName: proto.String("Bob"), FullName: proto.String("Bob Test")}}},
		{[]string{"mute", bobPN.String()}, &wasyncaction.SyncActionValue{Timestamp: ts, MuteAction: &wasyncaction.MuteAction{Muted: proto.Bool(true), MuteEndTimestamp: proto.Int64(-1)}}},
		{[]string{"setting_pushName"}, &wasyncaction.SyncActionValue{Timestamp: ts, PushNameSetting: &wasyncaction.PushNameSetting{Name: proto.String("Wax Test")}}},
	}
	phoneMACs := &memMACs{keys: map[string][]byte{string(ss.keyID): ss.keyData}, macs: map[string][]byte{}}
	var srecs []*waserversync.SyncdRecord
	state := appstate.HashState{Version: 3}
	var added [][]byte
	for _, r := range records {
		m, err := appstate.EncodeMutation(waserversync.SyncdMutation_SET, r.index, r.value, 2, ss.keyID, ss.keyData)
		must(t, err)
		srecs = append(srecs, m.Record)
		blob := m.Record.Value.Blob
		added = append(added, blob[len(blob)-32:])
		phoneMACs.macs[string(m.Record.Index.Blob)] = blob[len(blob)-32:]
	}
	state.SubtractThenAdd(nil, added)
	snap := &waserversync.SyncdSnapshot{Version: &waserversync.SyncdVersion{Version: proto.Uint64(3)}, Records: srecs,
		KeyID: &waserversync.KeyId{ID: ss.keyID}, Mac: appstate.SnapshotMAC(state, appstate.Regular, appstate.ExpandKeys(ss.keyData).SnapshotMAC)}
	raw, _ := proto.Marshal(snap)
	ss.snapshot = ss.uploadBlob(raw)

	// 1. Initial sync after login: key unknown -> key request to the phone.
	_, err := ss.serveUntil(func(*wabin.Node) bool { return ss.keyReqs == 1 })
	must(t, err)

	// 2. The phone shares the key; the client re-syncs and applies it.
	must(t, c.Send(h.ctx, ss.fromPhone("KS-1", &wae2e.ProtocolMessage{
		Type: wae2e.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE.Enum(),
		AppStateSyncKeyShare: &wae2e.AppStateSyncKeyShare{Keys: []*wae2e.AppStateSyncKey{{
			KeyID:   &wae2e.AppStateSyncKeyId{KeyID: ss.keyID},
			KeyData: &wae2e.AppStateSyncKeyData{KeyData: ss.keyData, Timestamp: proto.Int64(1)},
		}}},
	})))
	go ss.serveUntil(func(*wabin.Node) bool { return false }) // ends with the test
	done := waitEvent[AppStateSyncComplete](h)
	for done.Collection != appstate.Regular {
		done = waitEvent[AppStateSyncComplete](h) // empty collections complete at v0
	}
	if done.Version != 3 {
		t.Fatalf("sync complete %+v", done)
	}
	contact, err := h.store.GetContact(h.ctx, h.inst, bobPN.String())
	must(t, err)
	if contact.FirstName != "Bob" || contact.FullName != "Bob Test" {
		t.Fatalf("contact %+v", contact)
	}
	chat, err := h.store.GetChat(h.ctx, h.inst, bobPN.String())
	must(t, err)
	if chat.MutedUntil == nil || chat.MutedUntil.Year() != 9999 {
		t.Fatalf("muted until %v", chat.MutedUntil)
	}
	if name, _ := h.client.pushName.Load().(string); name != "Wax Test" {
		t.Fatalf("push name %q", name)
	}

	// 3. A patch (unmute) announced by server_sync.
	m, err := appstate.EncodeMutation(waserversync.SyncdMutation_SET, []string{"mute", bobPN.String()},
		&wasyncaction.SyncActionValue{Timestamp: ts, MuteAction: &wasyncaction.MuteAction{Muted: proto.Bool(false)}}, 2, ss.keyID, ss.keyData)
	must(t, err)
	patch, _, err := appstate.SealPatch(appstate.Regular, state, []*waserversync.SyncdMutation{m}, phoneMACs, ss.keyID, ss.keyData)
	must(t, err)
	ss.patches = []*waserversync.SyncdPatch{patch}
	ss.version = 4
	must(t, c.Send(h.ctx, wabin.Node{Tag: "notification", Attrs: wabin.Attrs{"from": wabin.ServerJID, "id": "SS-1", "type": "server_sync", "t": "1700000001"},
		Content: []wabin.Node{{Tag: "collection", Attrs: wabin.Attrs{"name": appstate.Regular, "version": 4}}}}))
	for {
		ch := waitEvent[AppStateChange](h)
		if ch.Index[0] == "mute" && !ch.FromFullSync {
			if ch.Action.GetMuteAction().GetMuted() {
				t.Fatal("expected unmute")
			}
			break
		}
	}
	if v, _, err := h.store.AppStateVersion(h.ctx, h.inst, appstate.Regular); err != nil || v != 4 {
		t.Fatalf("version %d %v", v, err)
	}
	if chat, _ := h.store.GetChat(h.ctx, h.inst, bobPN.String()); chat.MutedUntil != nil {
		t.Fatalf("still muted until %v", chat.MutedUntil)
	}

	// 4. History sync with an inline payload.
	hs := &wahistorysync.HistorySync{
		SyncType:   wahistorysync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		ChunkOrder: proto.Uint32(1),
		Conversations: []*wahistorysync.Conversation{{
			ID: proto.String(bobLID.String()), Name: proto.String("Bob"), UnreadCount: proto.Uint32(2),
			ConversationTimestamp: proto.Uint64(1690000000),
			Messages: []*wahistorysync.HistorySyncMsg{
				{Message: &wahistorysync.WebMessageInfo{Key: &wae2e.MessageKey{RemoteJID: proto.String(bobLID.String()), ID: proto.String("H1")},
					Message: &wae2e.Message{Conversation: proto.String("eski 1")}, MessageTimestamp: proto.Uint64(1690000000)}},
				{Message: &wahistorysync.WebMessageInfo{Key: &wae2e.MessageKey{RemoteJID: proto.String(bobLID.String()), ID: proto.String("H2"), FromMe: proto.Bool(true)},
					Message: &wae2e.Message{Conversation: proto.String("eski 2")}, MessageTimestamp: proto.Uint64(1690000100)}},
			},
		}},
		Pushnames:                []*wahistorysync.Pushname{{ID: proto.String(bobLID.String()), Pushname: proto.String("Bobby")}},
		PhoneNumberToLidMappings: []*wahistorysync.PhoneNumberToLIDMapping{{PnJID: proto.String("905552222222@s.whatsapp.net"), LidJID: proto.String("888@lid")}},
	}
	rawHS, _ := proto.Marshal(hs)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(rawHS)
	zw.Close()
	must(t, c.Send(h.ctx, ss.fromPhone("HS-1", &wae2e.ProtocolMessage{
		Type:                    wae2e.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &wae2e.HistorySyncNotification{InitialHistBootstrapInlinePayload: zbuf.Bytes(), SyncType: proto.Int32(0), ChunkOrder: proto.Uint32(1)},
	})))
	ev := waitEvent[HistorySync](h)
	if ev.Conversations != 1 || ev.Messages != 2 || ev.PushNames != 1 {
		t.Fatalf("history event %+v", ev)
	}
	msgs, err := h.store.Messages(h.ctx, h.inst, bobLID.String(), time.Now(), 10)
	must(t, err)
	if len(msgs) != 2 || msgs[0].ID != "H2" || !msgs[0].FromMe {
		t.Fatalf("stored history messages %+v", msgs)
	}
	var stored wae2e.Message
	must(t, proto.Unmarshal(msgs[1].Content, &stored))
	if stored.GetConversation() != "eski 1" {
		t.Fatalf("content %v", &stored)
	}
	if lid, _ := h.store.LIDForPN(h.ctx, h.inst, "905552222222"); lid != "888" {
		t.Fatal("history LID mapping not stored")
	}
	if queued, _ := h.store.HistoryQueueLength(h.ctx, h.inst); queued != 0 {
		t.Fatalf("%d history jobs left", queued)
	}
}

// memMACs is the phone-side MAC store for building patches.
type memMACs struct {
	keys map[string][]byte
	macs map[string][]byte
}

func (m *memMACs) Key(id []byte) ([]byte, error)            { return m.keys[string(id)], nil }
func (m *memMACs) ValueMAC(indexMAC []byte) ([]byte, error) { return m.macs[string(indexMAC)], nil }

func TestAppStateFatalRecovery(t *testing.T) {
	h, c, ours := loggedIn(t, nil)
	h.client.mediaScheme = "http"
	_, host := fakeMediaServer(t)
	ownLID := wabin.JID{User: "1" + testUser, Server: wabin.ServerLID}
	ss := &syncServer{t: t, h: h, c: c, host: host, ownLID: ownLID,
		phone: newPeer(t, wabin.JID{User: ownLID.User, Server: wabin.ServerLID}, 1111),
		toUs:  &signal.Record{}, keyID: []byte{0, 0, 0, 8}, keyData: bytes.Repeat([]byte{5}, 32), version: 10}
	must(t, ss.toUs.ProcessBundle(ss.phone.local(), ours))
	must(t, h.store.PutAppStateKey(h.ctx, h.inst, ss.keyID, ss.keyData, nil, 1))

	// A server snapshot whose MAC does not verify (as seen on real accounts).
	m, err := appstate.EncodeMutation(waserversync.SyncdMutation_SET, []string{"pin_v1", bobPN.String()},
		&wasyncaction.SyncActionValue{PinAction: &wasyncaction.PinAction{Pinned: proto.Bool(true)}}, 2, ss.keyID, ss.keyData)
	must(t, err)
	bad := &waserversync.SyncdSnapshot{Version: &waserversync.SyncdVersion{Version: proto.Uint64(10)}, Records: []*waserversync.SyncdRecord{m.Record},
		KeyID: &waserversync.KeyId{ID: ss.keyID}, Mac: bytes.Repeat([]byte{1}, 32)}
	raw, _ := proto.Marshal(bad)
	ss.snapshot = ss.uploadBlob(raw)

	gotRequest := false
	ss.onPeer = func(m *wae2e.Message) error {
		r := m.GetProtocolMessage().GetPeerDataOperationRequestMessage()
		if r.GetPeerDataOperationRequestType() == wae2e.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY &&
			r.GetSyncdCollectionFatalRecoveryRequest().GetCollectionName() == appstate.Regular {
			gotRequest = true
		}
		return nil
	}
	_, err = ss.serveUntil(func(*wabin.Node) bool { return gotRequest })
	must(t, err)

	// The phone answers with its copy: one pinned chat, version 12.
	indexJSON := []byte(`["pin_v1","` + bobPN.String() + `"]`)
	var lthash appstate.HashState
	lthash.SubtractThenAdd(nil, [][]byte{bytes.Repeat([]byte{3}, 32)})
	rec := &wasyncaction.SyncdSnapshotRecovery{
		Version:        &wasyncaction.SyncdSnapshotRecovery_SyncdVersion{Version: proto.Uint64(12)},
		CollectionName: proto.String(appstate.Regular),
		MutationRecords: []*wasyncaction.SyncdPlainTextRecord{{
			Value: &wasyncaction.SyncActionData{Index: indexJSON, Value: &wasyncaction.SyncActionValue{PinAction: &wasyncaction.PinAction{Pinned: proto.Bool(true)}}},
			KeyID: ss.keyID, Mac: bytes.Repeat([]byte{3}, 32),
		}},
		CollectionLthash: lthash.Hash[:],
	}
	recRaw, _ := proto.Marshal(rec)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(recRaw)
	zw.Close()
	ss.snapshot, ss.version = nil, 12
	go ss.serveUntil(func(*wabin.Node) bool { return false })
	must(t, c.Send(h.ctx, ss.fromPhone("PDR-1", &wae2e.ProtocolMessage{
		Type: wae2e.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE.Enum(),
		PeerDataOperationRequestResponseMessage: &wae2e.PeerDataOperationRequestResponseMessage{
			PeerDataOperationRequestType: wae2e.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY.Enum(),
			PeerDataOperationResult: []*wae2e.PeerDataOperationRequestResponseMessage_PeerDataOperationResult{{
				SyncdSnapshotFatalRecoveryResponse: &wae2e.PeerDataOperationRequestResponseMessage_PeerDataOperationResult_SyncDSnapshotFatalRecoveryResponse{
					CollectionSnapshot: gz.Bytes(), IsCompressed: proto.Bool(true)},
			}},
		},
	})))
	for {
		ev := waitEvent[AppStateSyncComplete](h)
		if ev.Collection == appstate.Regular && ev.Version == 12 {
			break
		}
	}
	v, hash, err := h.store.AppStateVersion(h.ctx, h.inst, appstate.Regular)
	must(t, err)
	if v != 12 || hash != lthash.Hash {
		t.Fatalf("recovered state v%d", v)
	}
	chat, err := h.store.GetChat(h.ctx, h.inst, bobPN.String())
	must(t, err)
	if !chat.Pinned {
		t.Fatal("recovered pin not applied")
	}
}
