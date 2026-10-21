package client

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/proto/wae2e"
)

// Placeholder identities for a contact ("Bob"); not real accounts.
var (
	bobPN  = wabin.JID{User: "905551111111", Server: wabin.ServerUser}
	bobLID = wabin.JID{User: "777000111", Server: wabin.ServerLID}
)

// peer is a remote Signal device driven by the test.
type peer struct {
	jid      wabin.JID
	ident    *keys.SigningKey
	reg      uint32
	spk      *keys.KeyPair
	opk      *keys.KeyPair
	sessions map[string]*signal.Record // by remote address
}

func newPeer(t testing.TB, jid wabin.JID, reg uint32) *peer {
	ident, _ := keys.NewSigningKey()
	spk, _ := keys.NewKeyPair()
	opk, _ := keys.NewKeyPair()
	return &peer{jid: jid, ident: ident, reg: reg, spk: spk, opk: opk, sessions: map[string]*signal.Record{}}
}

func (p *peer) local() signal.LocalIdentity {
	return signal.LocalIdentity{Key: &p.ident.KeyPair, RegistrationID: p.reg}
}

func (p *peer) SignedPreKey(id uint32) (*keys.KeyPair, error) {
	if id != 1 {
		return nil, signal.ErrMissingPreKey
	}
	return p.spk, nil
}

func (p *peer) PreKey(id uint32) (*keys.KeyPair, error) {
	if id != 7 || p.opk == nil {
		return nil, signal.ErrMissingPreKey
	}
	return p.opk, nil
}

func id3(id uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], id)
	return b[1:]
}

// bundleNode is the server's answer for this peer in a prekey fetch.
func (p *peer) bundleNode() wabin.Node {
	var reg [4]byte
	binary.BigEndian.PutUint32(reg[:], p.reg)
	sig := p.ident.SignPublicKey(p.spk.Pub)
	return wabin.Node{Tag: "user", Attrs: wabin.Attrs{"jid": p.jid}, Content: []wabin.Node{
		{Tag: "registration", Content: reg[:]},
		{Tag: "type", Content: []byte{5}},
		{Tag: "identity", Content: p.ident.Pub[:]},
		{Tag: "skey", Content: []wabin.Node{{Tag: "id", Content: id3(1)}, {Tag: "value", Content: p.spk.Pub[:]}, {Tag: "signature", Content: sig[:]}}},
		{Tag: "key", Content: []wabin.Node{{Tag: "id", Content: id3(7)}, {Tag: "value", Content: p.opk.Pub[:]}}},
	}}
}

// decrypt opens an <enc> addressed to this peer and returns the Message.
func (p *peer) decrypt(t testing.TB, fromAddr string, enc *wabin.Node) *wae2e.Message {
	t.Helper()
	ct, _ := enc.Bytes()
	rec := p.sessions[fromAddr]
	if rec == nil {
		rec = &signal.Record{}
		p.sessions[fromAddr] = rec
	}
	var padded []byte
	if typ, _ := enc.AttrString("type"); typ == "pkmsg" {
		res, err := rec.DecryptPreKeyMessage(p.local(), p, ct)
		if err != nil {
			t.Fatalf("peer %s: decrypt pkmsg: %v", p.jid, err)
		}
		padded = res.Plaintext
	} else {
		var err error
		if padded, err = rec.DecryptMessage(ct); err != nil {
			t.Fatalf("peer %s: decrypt msg: %v", p.jid, err)
		}
	}
	pt, err := unpadMessage(padded)
	if err != nil {
		t.Fatal(err)
	}
	var m wae2e.Message
	if err := proto.Unmarshal(pt, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

// ourBundle rebuilds our prekey bundle from the upload the client made.
func ourBundle(t testing.TB, upload *wabin.Node) *signal.PreKeyBundle {
	t.Helper()
	reg, _ := upload.Child("registration")
	regB, _ := reg.Bytes()
	ident, _ := upload.Child("identity")
	identB, _ := ident.Bytes()
	skey, _ := upload.Child("skey")
	list, _ := upload.Child("list")
	n := wabin.Node{Tag: "user", Content: []wabin.Node{
		{Tag: "registration", Content: regB}, {Tag: "identity", Content: identB}, *skey, list.Children()[0],
	}}
	b, err := parseBundle(&n)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func encNode(t testing.TB, rec *signal.Record, msg *wae2e.Message) wabin.Node {
	t.Helper()
	raw, _ := proto.Marshal(msg)
	typ, ct, err := rec.Encrypt(padMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	encType := "msg"
	if typ == signal.TypePreKey {
		encType = "pkmsg"
	}
	return wabin.Node{Tag: "enc", Attrs: wabin.Attrs{"v": "2", "type": encType}, Content: ct}
}

func readNodeWith(t testing.TB, h *harness, c *fakewa.Conn, tag, id string) *wabin.Node {
	t.Helper()
	n, err := c.ReadUntil(h.ctx, func(n *wabin.Node) bool {
		nid, _ := n.AttrString("id")
		return n.Tag == tag && (id == "" || nid == id)
	})
	must(t, err)
	return n
}

func TestReceiveMessage(t *testing.T) {
	h := newHarness(t, nil)
	_, c := h.pair()
	upload := h.serveLoginUpload(c, 0)

	bob := newPeer(t, wabin.JID{User: bobLID.User, Device: 0, Server: wabin.ServerLID}, 4321)
	toUs := &signal.Record{}
	must(t, toUs.ProcessBundle(bob.local(), ourBundle(t, upload)))
	stanza := func(id string, enc wabin.Node) wabin.Node {
		return wabin.Node{Tag: "message", Attrs: wabin.Attrs{
			"from": bob.jid, "id": id, "t": fmt.Sprint(time.Now().Unix()), "type": "text", "notify": "Bob",
			"sender_pn": bobPN,
		}, Content: []wabin.Node{enc}}
	}
	first := stanza("MSG-1", encNode(t, toUs, &wae2e.Message{Conversation: proto.String("merhaba")}))
	must(t, c.Send(h.ctx, first))

	m := waitEvent[Message](h)
	if m.Message.GetConversation() != "merhaba" || m.Info.Sender.User != bobLID.User || m.Info.SenderAlt.User != bobPN.User || m.Info.PushName != "Bob" {
		t.Fatalf("unexpected message event %+v", m.Info)
	}
	r := readNodeWith(t, h, c, "receipt", "MSG-1")
	if typ, _ := r.AttrString("type"); typ != "inactive" {
		t.Fatalf("receipt type %q", typ)
	}
	if lid, _ := h.store.LIDForPN(h.ctx, h.inst, bobPN.User); lid != bobLID.User {
		t.Fatalf("LID mapping not learned: %q", lid)
	}

	// Redelivery of an already handled stanza: receipt again, but no
	// second event.
	must(t, c.Send(h.ctx, first))
	readNodeWith(t, h, c, "receipt", "MSG-1")

	// Second message (still pkmsg: we never replied) plus an extended text.
	must(t, c.Send(h.ctx, stanza("MSG-2", encNode(t, toUs, &wae2e.Message{ExtendedTextMessage: &wae2e.ExtendedTextMessage{Text: proto.String("ikinci")}}))))
	m2 := waitEvent[Message](h)
	if m2.Info.ID != "MSG-2" || m2.Message.GetExtendedTextMessage().GetText() != "ikinci" {
		t.Fatalf("expected MSG-2, got %s (duplicate delivered?)", m2.Info.ID)
	}
	readNodeWith(t, h, c, "receipt", "MSG-2")

	// A "msg" we cannot decrypt (no such session): retry receipt + ack,
	// and an undecryptable event.
	carol := wabin.JID{User: "777000222", Server: wabin.ServerLID}
	must(t, c.Send(h.ctx, wabin.Node{Tag: "message", Attrs: wabin.Attrs{"from": carol, "id": "MSG-3", "t": "1700000000", "type": "text"},
		Content: []wabin.Node{{Tag: "enc", Attrs: wabin.Attrs{"v": "2", "type": "msg"}, Content: []byte("\x33not a signal message at all")}}}))
	retry := readNodeWith(t, h, c, "receipt", "MSG-3")
	if typ, _ := retry.AttrString("type"); typ != "retry" {
		t.Fatalf("expected retry receipt, got %s", retry.String())
	}
	readNodeWith(t, h, c, "ack", "MSG-3")
	waitEvent[UndecryptableMessage](h)
}

func TestSendMessageAndRetry(t *testing.T) {
	h := newHarness(t, nil)
	_, c := h.pair()
	h.serveLoginUpload(c, 0)

	bob := newPeer(t, wabin.JID{User: bobLID.User, Device: 0, Server: wabin.ServerLID}, 4321)
	ownLID := wabin.JID{User: "1" + testUser, Server: wabin.ServerLID}
	myPhone := newPeer(t, wabin.JID{User: ownLID.User, Device: 0, Server: wabin.ServerLID}, 1111)

	type sendRes struct {
		r   *SendResult
		err error
	}
	done := make(chan sendRes, 1)
	go func() {
		r, err := h.client.SendText(h.ctx, bobPN, "selam")
		done <- sendRes{r, err}
	}()

	// 1. LID lookup.
	q := readNodeWith(t, h, c, "iq", "")
	for ns, _ := q.AttrString("xmlns"); ns != "usync"; ns, _ = q.AttrString("xmlns") {
		q = readNodeWith(t, h, c, "iq", "")
	}
	id, _ := q.AttrString("id")
	must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "usync", Content: []wabin.Node{{Tag: "list", Content: []wabin.Node{
		{Tag: "user", Attrs: wabin.Attrs{"jid": bobPN}, Content: []wabin.Node{{Tag: "lid", Attrs: wabin.Attrs{"val": bobLID}}}},
	}}}})))
	// 2. Device lists of Bob (by LID) and of ourselves.
	q, err := c.ReadIQ(h.ctx, "usync")
	must(t, err)
	id, _ = q.AttrString("id")
	devList := func(ids ...int) wabin.Node {
		var ds []wabin.Node
		for _, i := range ids {
			ds = append(ds, wabin.Node{Tag: "device", Attrs: wabin.Attrs{"id": i}})
		}
		return wabin.Node{Tag: "devices", Content: []wabin.Node{{Tag: "device-list", Content: ds}}}
	}
	must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "usync", Content: []wabin.Node{{Tag: "list", Content: []wabin.Node{
		{Tag: "user", Attrs: wabin.Attrs{"jid": bobLID}, Content: []wabin.Node{devList(0)}},
		{Tag: "user", Attrs: wabin.Attrs{"jid": ownLID}, Content: []wabin.Node{devList(0, 12)}},
	}}}})))
	// 3. Prekey bundles for Bob's phone and our own phone (not our device 12).
	q, err = c.ReadIQ(h.ctx, "encrypt")
	must(t, err)
	keyNode, _ := q.Child("key")
	if n := len(keyNode.ChildrenByTag("user")); n != 2 {
		t.Fatalf("expected bundle request for 2 devices, got %d: %s", n, q.String())
	}
	id, _ = q.AttrString("id")
	must(t, c.Send(h.ctx, fakewa.Result(id, wabin.Node{Tag: "list", Content: []wabin.Node{bob.bundleNode(), myPhone.bundleNode()}})))
	// 4. The message itself.
	msg := readNodeWith(t, h, c, "message", "")
	msgID, _ := msg.AttrString("id")
	if pn, _ := msg.AttrJID("peer_recipient_pn"); pn.User != bobPN.User {
		t.Fatalf("missing peer_recipient_pn: %s", msg.String())
	}
	if _, ok := msg.Child("device-identity"); !ok {
		t.Fatal("pkmsg without device-identity")
	}
	parts, _ := msg.Child("participants")
	ourAddr := signalAddress(wabin.JID{User: ownLID.User, Device: 12, Server: wabin.ServerLID})
	got := map[string]bool{}
	for _, to := range parts.ChildrenByTag("to") {
		jid, _ := to.AttrJID("jid")
		enc, _ := to.Child("enc")
		switch jid.User {
		case bobLID.User:
			if bob.decrypt(t, ourAddr, enc).GetConversation() != "selam" {
				t.Fatal("bob got wrong text")
			}
		case ownLID.User:
			m := myPhone.decrypt(t, ourAddr, enc)
			if m.GetDeviceSentMessage().GetMessage().GetConversation() != "selam" || m.GetDeviceSentMessage().GetDestinationJID() != bobLID.String() {
				t.Fatalf("own phone got %v", m)
			}
		}
		got[jid.User] = true
	}
	if !got[bobLID.User] || !got[ownLID.User] || len(got) != 2 {
		t.Fatalf("unexpected participants: %v", got)
	}
	must(t, c.Send(h.ctx, wabin.Node{Tag: "ack", Attrs: wabin.Attrs{"class": "message", "id": msgID, "t": "1700000123", "from": bobLID}}))
	res := <-done
	must(t, res.err)
	if res.r.ID != msgID || res.r.Timestamp.Unix() != 1700000123 {
		t.Fatalf("send result %+v", res.r)
	}

	// Bob's device could not decrypt: it sends a retry receipt.
	var reg [4]byte
	binary.BigEndian.PutUint32(reg[:], bob.reg)
	must(t, c.Send(h.ctx, wabin.Node{Tag: "receipt", Attrs: wabin.Attrs{"from": bob.jid, "id": msgID, "type": "retry", "t": "1700000124"},
		Content: []wabin.Node{
			{Tag: "retry", Attrs: wabin.Attrs{"count": 1, "id": msgID, "t": "1700000123", "v": 1}},
			{Tag: "registration", Content: reg[:]},
		}}))
	resent := readNodeWith(t, h, c, "message", msgID)
	enc, ok := resent.Child("enc")
	if !ok {
		t.Fatalf("resend without enc: %s", resent.String())
	}
	if cnt, _ := enc.AttrString("count"); cnt != "1" {
		t.Fatalf("resend count %q", cnt)
	}
	if bob.decrypt(t, ourAddr, enc).GetConversation() != "selam" {
		t.Fatal("bob could not decrypt the resent message")
	}
	readNodeWith(t, h, c, "ack", msgID) // client acknowledged the receipt
	if r := waitEvent[Receipt](h); r.Type != "retry" {
		t.Fatalf("receipt event %+v", r)
	}
}

// Stanzas we cannot handle yet must be stored and then acknowledged, so the
// server's offline queue keeps flowing.
func TestUnsupportedStanzasAreDeferredAndAcked(t *testing.T) {
	h := newHarness(t, nil)
	_, c := h.pair()
	h.serveLoginUpload(c, 0)
	must(t, c.Send(h.ctx, wabin.Node{Tag: "message", Attrs: wabin.Attrs{"from": bobLID, "id": "G-1", "t": "1700000000", "type": "text"},
		Content: []wabin.Node{{Tag: "enc", Attrs: wabin.Attrs{"v": "2", "type": "msmsg"}, Content: []byte{1, 2, 3}}}}))
	ack := readNodeWith(t, h, c, "ack", "G-1")
	if cls, _ := ack.AttrString("class"); cls != "message" {
		t.Fatalf("ack %s", ack.String())
	}
	must(t, c.Send(h.ctx, wabin.Node{Tag: "notification", Attrs: wabin.Attrs{"from": wabin.ServerJID, "id": "N-1", "type": "some_future_type", "t": "1700000000"}}))
	readNodeWith(t, h, c, "ack", "N-1")
	n, err := h.store.CountDeferred(h.ctx, h.inst)
	must(t, err)
	if n != 2 {
		t.Fatalf("deferred %d stanzas, want 2", n)
	}
	list, err := h.store.ListDeferred(h.ctx, h.inst, 10)
	must(t, err)
	stored, err := wabin.Unmarshal(list[0].Raw[1:])
	must(t, err)
	if stored.Tag != "message" || list[0].StanzaID != "G-1" {
		t.Fatalf("stored stanza %s", stored.String())
	}
}
