package client

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/media"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/testutil/fakewa"
)

var testGroup = wabin.JID{User: "120363000000000001", Server: wabin.ServerGroup}

// loggedIn pairs, logs in and returns the connection and our uploaded bundle.
func loggedIn(t *testing.T, tweak func(*Config)) (*harness, *fakewa.Conn, *signal.PreKeyBundle) {
	h := newHarness(t, tweak)
	_, c := h.pair()
	up := h.serveLoginUpload(c, 0)
	return h, c, ourBundle(t, up)
}

func (p *peer) senderKeyMessage(t *testing.T, rec *signal.SenderKeyRecord, msg *wae2e.Message) wabin.Node {
	t.Helper()
	raw, _ := proto.Marshal(msg)
	ct, err := rec.EncryptGroup(padMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return wabin.Node{Tag: "enc", Attrs: wabin.Attrs{"v": "2", "type": "skmsg"}, Content: ct}
}

func TestReceiveGroupMessage(t *testing.T) {
	h, c, ours := loggedIn(t, nil)
	bob := newPeer(t, wabin.JID{User: bobLID.User, Device: 0, Server: wabin.ServerLID}, 4321)
	toUs := &signal.Record{}
	must(t, toUs.ProcessBundle(bob.local(), ours))
	bobSK := &signal.SenderKeyRecord{}
	skdm, err := bobSK.CreateDistribution()
	must(t, err)

	stanza := func(id string, encs ...wabin.Node) wabin.Node {
		return wabin.Node{Tag: "message", Attrs: wabin.Attrs{
			"from": testGroup, "participant": bob.jid, "id": id, "t": "1700000000", "type": "text", "addressing_mode": "lid",
		}, Content: encs}
	}
	// First message: sender key distribution (1:1) + the group message.
	dist := encNode(t, toUs, &wae2e.Message{SenderKeyDistributionMessage: &wae2e.SenderKeyDistributionMessage{
		GroupID: proto.String(testGroup.String()), AxolotlSenderKeyDistributionMessage: skdm}})
	must(t, c.Send(h.ctx, stanza("G1", dist, bob.senderKeyMessage(t, bobSK, &wae2e.Message{Conversation: proto.String("grup selam")}))))
	m := waitEvent[Message](h)
	if m.Info.Chat != testGroup || !m.Info.IsGroup || m.Message.GetConversation() != "grup selam" || m.Info.Sender.User != bobLID.User {
		t.Fatalf("unexpected group message %+v / %v", m.Info, m.Message)
	}
	readNodeWith(t, h, c, "receipt", "G1")

	// Second message: only skmsg.
	must(t, c.Send(h.ctx, stanza("G2", bob.senderKeyMessage(t, bobSK, &wae2e.Message{Conversation: proto.String("ikinci")}))))
	if m := waitEvent[Message](h); m.Info.ID != "G2" || m.Message.GetConversation() != "ikinci" {
		t.Fatalf("got %s %v", m.Info.ID, m.Message)
	}
	readNodeWith(t, h, c, "receipt", "G2")

	// A member whose sender key we never got: retry receipt.
	carol := wabin.JID{User: "777000222", Server: wabin.ServerLID}
	carolSK := &signal.SenderKeyRecord{}
	_, _ = carolSK.CreateDistribution()
	n := stanza("G3", bob.senderKeyMessage(t, carolSK, &wae2e.Message{Conversation: proto.String("?")}))
	n.Attrs["participant"] = carol
	must(t, c.Send(h.ctx, n))
	r := readNodeWith(t, h, c, "receipt", "G3")
	if typ, _ := r.AttrString("type"); typ != "retry" {
		t.Fatalf("expected retry receipt, got %s", r.String())
	}
}

// groupServer answers the IQs a group send needs.
type groupServer struct {
	t       *testing.T
	h       *harness
	c       *fakewa.Conn
	ownLID  wabin.JID
	bob     *peer
	myPhone *peer
}

func (g *groupServer) answerUntilMessage() *wabin.Node {
	t, h, c := g.t, g.h, g.c
	for {
		n, err := c.ReadUntil(h.ctx, func(n *wabin.Node) bool { return n.Tag == "iq" || n.Tag == "message" })
		must(t, err)
		if n.Tag == "message" {
			return n
		}
		id, _ := n.AttrString("id")
		switch ns, _ := n.AttrString("xmlns"); ns {
		case "w:g2":
			must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "group", Attrs: wabin.Attrs{
				"id": testGroup.User, "subject": "Test", "addressing_mode": "lid",
			}, Content: []wabin.Node{
				{Tag: "participant", Attrs: wabin.Attrs{"jid": g.ownLID, "type": "superadmin"}},
				{Tag: "participant", Attrs: wabin.Attrs{"jid": bobLID, "phone_number": bobPN}},
			}})))
		case "usync":
			dev := func(ids ...int) wabin.Node {
				var ds []wabin.Node
				for _, i := range ids {
					ds = append(ds, wabin.Node{Tag: "device", Attrs: wabin.Attrs{"id": i}})
				}
				return wabin.Node{Tag: "devices", Content: []wabin.Node{{Tag: "device-list", Content: ds}}}
			}
			must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "usync", Content: []wabin.Node{{Tag: "list", Content: []wabin.Node{
				{Tag: "user", Attrs: wabin.Attrs{"jid": g.ownLID}, Content: []wabin.Node{dev(0, 12)}},
				{Tag: "user", Attrs: wabin.Attrs{"jid": bobLID}, Content: []wabin.Node{dev(0)}},
			}}}})))
		case "encrypt":
			must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "list", Content: []wabin.Node{g.bob.bundleNode(), g.myPhone.bundleNode()}})))
		default:
			t.Fatalf("unexpected iq %s", n.String())
		}
	}
}

func TestSendGroupMessage(t *testing.T) {
	h, c, _ := loggedIn(t, nil)
	ownLID := wabin.JID{User: "1" + testUser, Server: wabin.ServerLID}
	g := &groupServer{t: t, h: h, c: c, ownLID: ownLID,
		bob:     newPeer(t, wabin.JID{User: bobLID.User, Server: wabin.ServerLID}, 4321),
		myPhone: newPeer(t, wabin.JID{User: ownLID.User, Server: wabin.ServerLID}, 1111)}
	ourAddr := signalAddress(wabin.JID{User: ownLID.User, Device: 12, Server: wabin.ServerLID})
	bobSK, phoneSK := &signal.SenderKeyRecord{}, &signal.SenderKeyRecord{}

	send := func(text string) *wabin.Node {
		done := make(chan error, 1)
		go func() { _, err := h.client.SendText(h.ctx, testGroup, text); done <- err }()
		msg := g.answerUntilMessage()
		id, _ := msg.AttrString("id")
		if ph, _ := msg.AttrString("phash"); ph == "" {
			t.Fatal("group message without phash")
		}
		if am, _ := msg.AttrString("addressing_mode"); am != "lid" {
			t.Fatal("lid group without addressing_mode")
		}
		// Peers pick up sender key distributions addressed to them.
		if parts, ok := msg.Child("participants"); ok {
			for _, to := range parts.ChildrenByTag("to") {
				jid, _ := to.AttrJID("jid")
				enc, _ := to.Child("enc")
				var p *peer
				var rec *signal.SenderKeyRecord
				switch jid.User {
				case bobLID.User:
					p, rec = g.bob, bobSK
				case ownLID.User:
					p, rec = g.myPhone, phoneSK
				default:
					t.Fatalf("unexpected participant %s", jid)
				}
				m := p.decrypt(t, ourAddr, enc)
				if m.GetSenderKeyDistributionMessage().GetGroupID() != testGroup.String() {
					t.Fatalf("participant payload is not a distribution: %v", m)
				}
				must(t, rec.ProcessDistribution(m.GetSenderKeyDistributionMessage().GetAxolotlSenderKeyDistributionMessage()))
			}
		}
		sk, ok := msg.Child("enc")
		if !ok {
			t.Fatal("no skmsg")
		}
		ct, _ := sk.Bytes()
		for _, rec := range []*signal.SenderKeyRecord{bobSK, phoneSK} {
			padded, err := rec.DecryptGroup(ct)
			must(t, err)
			pt, _ := unpadMessage(padded)
			var m wae2e.Message
			must(t, proto.Unmarshal(pt, &m))
			if m.GetConversation() != text {
				t.Fatalf("decrypted %q want %q", m.GetConversation(), text)
			}
		}
		must(t, c.Send(h.ctx, wabin.Node{Tag: "ack", Attrs: wabin.Attrs{"class": "message", "id": id, "t": "1700000001", "from": testGroup}}))
		must(t, <-done)
		return msg
	}

	first := send("ilk grup mesajı")
	if parts, ok := first.Child("participants"); !ok || len(parts.ChildrenByTag("to")) != 2 {
		t.Fatalf("first message must distribute the sender key to 2 devices: %s", first.String())
	}
	second := send("ikinci")
	if _, ok := second.Child("participants"); ok {
		t.Fatal("sender key redistributed although every device has it")
	}

	// A member leaves: we must rotate our sender key.
	must(t, c.Send(h.ctx, wabin.Node{Tag: "notification", Attrs: wabin.Attrs{"from": testGroup, "id": "N-9", "type": "w:gp2", "t": "1700000002"},
		Content: []wabin.Node{{Tag: "remove", Content: []wabin.Node{{Tag: "participant", Attrs: wabin.Attrs{"jid": wabin.JID{User: "999", Server: wabin.ServerLID}}}}}}}))
	readNodeWith(t, h, c, "ack", "N-9")
	waitEvent[GroupUpdate](h)
	bobSK, phoneSK = &signal.SenderKeyRecord{}, &signal.SenderKeyRecord{} // they must receive the new key
	third := send("rotasyondan sonra")
	if parts, ok := third.Child("participants"); !ok || len(parts.ChildrenByTag("to")) != 2 {
		t.Fatal("rotated sender key was not distributed")
	}
}

func TestDeferredReplay(t *testing.T) {
	h, c, ours := loggedIn(t, nil)
	bob := newPeer(t, wabin.JID{User: bobLID.User, Server: wabin.ServerLID}, 4321)
	toUs := &signal.Record{}
	must(t, toUs.ProcessBundle(bob.local(), ours))
	// As if stored by phase 3: a 1:1 message deferred and acked.
	stanza := wabin.Node{Tag: "message", Attrs: wabin.Attrs{"from": bob.jid, "id": "OLD-1", "t": "1700000000", "type": "text"},
		Content: []wabin.Node{encNode(t, toUs, &wae2e.Message{Conversation: proto.String("eski mesaj")})}}
	raw, err := wabin.Marshal(stanza)
	must(t, err)
	must(t, h.store.PutDeferred(h.ctx, h.inst, "message", "OLD-1", "test", raw))
	// Reconnect: replay runs right after login.
	c.Close()
	c2 := h.accept()
	h.serveLoginUpload(c2, 812)
	m := waitEvent[Message](h)
	if m.Info.ID != "OLD-1" || m.Message.GetConversation() != "eski mesaj" {
		t.Fatalf("replayed %s %v", m.Info.ID, m.Message)
	}
	if n, _ := h.store.CountDeferred(h.ctx, h.inst); n != 0 {
		t.Fatalf("%d deferred left", n)
	}
}

func TestCallsPresenceAndReceipts(t *testing.T) {
	h, c, _ := loggedIn(t, func(cfg *Config) { cfg.PushName = "Test" })
	must(t, c.Send(h.ctx, wabin.Node{Tag: "call", Attrs: wabin.Attrs{"from": bobLID, "id": "C-1", "t": "1700000000", "platform": "android"},
		Content: []wabin.Node{{Tag: "offer", Attrs: wabin.Attrs{"call-id": "CALL123", "call-creator": bobLID}, Content: []wabin.Node{{Tag: "audio"}}}}}))
	readNodeWith(t, h, c, "ack", "C-1")
	ev := waitEvent[CallEvent](h)
	if ev.Type != "offer" || ev.CallID != "CALL123" || ev.Video {
		t.Fatalf("call event %+v", ev)
	}
	must(t, h.client.RejectCall(h.ctx, ev.Creator, ev.CallID))
	rej, err := c.ReadUntil(h.ctx, func(n *wabin.Node) bool { return n.Tag == "call" })
	must(t, err)
	if r, ok := rej.Child("reject"); !ok || r.Attrs["call-id"] != "CALL123" {
		t.Fatalf("reject node %s", rej.String())
	}

	must(t, c.Send(h.ctx, wabin.Node{Tag: "presence", Attrs: wabin.Attrs{"from": bobLID, "type": "unavailable", "last": "1700000000"}}))
	if p := waitEvent[Presence](h); p.Available || p.LastSeen.Unix() != 1700000000 {
		t.Fatalf("presence %+v", p)
	}
	must(t, h.client.SendPresence(h.ctx, true))
	pn, err := c.ReadUntil(h.ctx, func(n *wabin.Node) bool { return n.Tag == "presence" })
	must(t, err)
	if pn.Attrs["type"] != "available" || pn.Attrs["name"] != "Test" {
		t.Fatalf("presence node %s", pn.String())
	}

	must(t, h.client.MarkRead(h.ctx, testGroup, bobLID, []string{"A", "B"}, time.Unix(1700000005, 0)))
	rr, err := c.ReadUntil(h.ctx, func(n *wabin.Node) bool { return n.Tag == "receipt" && n.Attrs["type"] == "read" })
	must(t, err)
	if rr.Attrs["id"] != "A" || rr.Attrs["participant"] != bobLID {
		t.Fatalf("read receipt %s", rr.String())
	}
	if list, ok := rr.Child("list"); !ok || len(list.Children()) != 1 {
		t.Fatalf("read receipt list %s", rr.String())
	}
}

// fakeMediaServer stores uploads and serves them back.
func fakeMediaServer(t *testing.T) (*httptest.Server, string) {
	var mu sync.Mutex
	files := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			if r.URL.Query().Get("auth") != "media-auth" {
				w.WriteHeader(401)
				return
			}
			b, _ := io.ReadAll(r.Body)
			path := "/v/" + strings.TrimPrefix(r.URL.Path, "/mms/")
			files[path] = b
			w.Write([]byte(`{"url":"https://example` + path + `","direct_path":"` + path + `?ccb=1"}`))
		default:
			b, ok := files[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(b)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "http://")
}

func TestSendAndDownloadMedia(t *testing.T) {
	h, c, _ := loggedIn(t, nil)
	h.client.mediaScheme = "http"
	_, host := fakeMediaServer(t)
	ownLID := wabin.JID{User: "1" + testUser, Server: wabin.ServerLID}
	bob := newPeer(t, wabin.JID{User: bobLID.User, Server: wabin.ServerLID}, 4321)
	myPhone := newPeer(t, wabin.JID{User: ownLID.User, Server: wabin.ServerLID}, 1111)
	must(t, h.store.PutDeferred(h.ctx, h.inst, "x", "x", "x", []byte{0})) // garbage row must not break anything
	_, _ = h.store.PutLIDMapping(h.ctx, h.inst, bobPN.User, bobLID.User)

	photo := bytes.Repeat([]byte("JPEGDATA"), 5000)
	done := make(chan error, 1)
	go func() {
		_, err := h.client.SendMedia(h.ctx, bobLID, SendMediaRequest{Kind: KindImage, Data: photo, MimeType: "image/jpeg", Caption: "foto"})
		done <- err
	}()
	var msg *wabin.Node
	for msg == nil {
		n, err := c.ReadUntil(h.ctx, func(n *wabin.Node) bool { return n.Tag == "iq" || n.Tag == "message" })
		must(t, err)
		if n.Tag == "message" {
			msg = n
			break
		}
		id, _ := n.AttrString("id")
		switch ns, _ := n.AttrString("xmlns"); ns {
		case "w:m":
			must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "media_conn", Attrs: wabin.Attrs{"auth": "media-auth", "ttl": 3600},
				Content: []wabin.Node{{Tag: "host", Attrs: wabin.Attrs{"hostname": host}}}})))
		case "usync":
			dl := func(ids ...int) wabin.Node {
				var ds []wabin.Node
				for _, i := range ids {
					ds = append(ds, wabin.Node{Tag: "device", Attrs: wabin.Attrs{"id": i}})
				}
				return wabin.Node{Tag: "devices", Content: []wabin.Node{{Tag: "device-list", Content: ds}}}
			}
			must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "usync", Content: []wabin.Node{{Tag: "list", Content: []wabin.Node{
				{Tag: "user", Attrs: wabin.Attrs{"jid": bobLID}, Content: []wabin.Node{dl(0)}},
				{Tag: "user", Attrs: wabin.Attrs{"jid": ownLID}, Content: []wabin.Node{dl(0, 12)}},
			}}}})))
		case "encrypt":
			must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "list", Content: []wabin.Node{bob.bundleNode(), myPhone.bundleNode()}})))
		}
	}
	if msg.Attrs["type"] != "media" {
		t.Fatalf("message type %v", msg.Attrs["type"])
	}
	parts, _ := msg.Child("participants")
	var img *wae2e.ImageMessage
	for _, to := range parts.ChildrenByTag("to") {
		if jid, _ := to.AttrJID("jid"); jid.User == bobLID.User {
			enc, _ := to.Child("enc")
			if enc.Attrs["mediatype"] != "image" {
				t.Fatalf("enc mediatype %v", enc.Attrs["mediatype"])
			}
			img = bob.decrypt(t, signalAddress(wabin.JID{User: ownLID.User, Device: 12, Server: wabin.ServerLID}), enc).GetImageMessage()
		}
	}
	if img == nil || img.GetCaption() != "foto" || img.GetFileLength() != uint64(len(photo)) {
		t.Fatalf("image message %v", img)
	}
	id, _ := msg.AttrString("id")
	must(t, c.Send(h.ctx, wabin.Node{Tag: "ack", Attrs: wabin.Attrs{"class": "message", "id": id, "t": "1700000001"}}))
	must(t, <-done)

	// The recipient side: download and decrypt with what the message carries.
	tr := &media.Transfer{Scheme: "http"}
	blob, err := tr.Download(h.ctx, &media.Conn{Hosts: []string{host}}, img.GetDirectPath(), img.GetFileEncSHA256(), media.Image)
	must(t, err)
	got, err := media.Decrypt(blob, img.GetMediaKey(), media.Image, img.GetFileEncSHA256(), img.GetFileSHA256())
	must(t, err)
	if !bytes.Equal(got, photo) {
		t.Fatal("downloaded photo differs")
	}
	// And our own Download path for an incoming media message.
	data, err := h.client.Download(h.ctx, &wae2e.Message{ImageMessage: img})
	must(t, err)
	if !bytes.Equal(data, photo) {
		t.Fatal("client.Download returned different bytes")
	}
}
