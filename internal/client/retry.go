package client

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/store"
)

// maxRetryReceipts bounds how often we ask a sender to resend one message.
const maxRetryReceipts = 5

// retryCounter counts per message id, in memory. Losing it on restart only
// means a sender may be asked a few extra times.
type retryCounter struct {
	mu sync.Mutex
	m  map[string]int
}

func (r *retryCounter) inc(key string, floor int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil || len(r.m) > 10000 {
		r.m = make(map[string]int)
	}
	r.m[key]++
	if r.m[key] == 1 && floor > 0 {
		r.m[key] = floor + 1
	}
	return r.m[key]
}

// sendRetryReceipt asks the sender to re-encrypt a message we could not
// decrypt. From the second attempt on we attach a fresh prekey bundle so the
// sender can rebuild the session from scratch.
func (s *session) sendRetryReceipt(n *wabin.Node, info *MessageInfo) {
	var inMsg int64
	if encs := n.ChildrenByTag("enc"); len(encs) == 1 {
		inMsg, _ = encs[0].AttrInt("count")
	}
	count := s.c.retries.inc(info.ID, int(inMsg))
	if count >= maxRetryReceipts {
		s.log.Warn("giving up on retry receipts", "id", info.ID)
		return
	}
	var reg [4]byte
	binary.BigEndian.PutUint32(reg[:], s.dev.RegistrationID)
	attrs := wabin.Attrs{"id": info.ID, "to": n.Attrs["from"], "type": "retry"}
	for _, k := range []string{"recipient", "participant"} {
		if v, ok := n.Attrs[k]; ok {
			attrs[k] = v
		}
	}
	if info.Type == "peer_msg" && info.IsFromMe {
		attrs["category"] = "peer"
	}
	content := []wabin.Node{
		{Tag: "retry", Attrs: wabin.Attrs{"count": count, "id": info.ID, "t": n.Attrs["t"], "v": 1}},
		{Tag: "registration", Content: reg[:]},
	}
	if count > 1 {
		pks, err := s.c.cfg.Store.GetOrGenPreKeys(s.ctx, s.c.cfg.InstanceID, 1)
		if err == nil && len(pks) == 1 {
			// The key is handed out directly, so it counts as uploaded.
			_ = s.c.cfg.Store.MarkPreKeysUploaded(s.ctx, s.c.cfg.InstanceID, []uint32{pks[0].ID})
			sig := s.dev.SignedPreKey.Signature
			content = append(content, wabin.Node{Tag: "keys", Content: []wabin.Node{
				{Tag: "type", Content: []byte{keys.DJBType}},
				{Tag: "identity", Content: s.dev.IdentityKey.Pub[:]},
				preKeyNode("key", pks[0].ID, pks[0].Key.Pub, nil),
				preKeyNode("skey", s.dev.SignedPreKey.ID, s.dev.SignedPreKey.Key.Pub, sig[:]),
				{Tag: "device-identity", Content: s.dev.Account},
			}})
		} else {
			s.log.Warn("no prekey for retry receipt", "error", err)
		}
	}
	s.sendOrLog(wabin.Node{Tag: "receipt", Attrs: attrs, Content: content})
}

// handleReceipt processes delivery/read/retry receipts for our messages.
func (s *session) handleReceipt(n *wabin.Node) {
	defer s.ackReceipt(n)
	from, _ := n.AttrJID("from")
	typ, _ := n.AttrString("type")
	r := Receipt{Type: typ, Sender: from, Chat: from.ToNonAD()}
	if p, ok := n.AttrJID("participant"); ok {
		r.Sender = p
	}
	if rc, ok := n.AttrJID("recipient"); ok {
		r.Chat = rc
	}
	if t, ok := n.AttrInt("t"); ok {
		r.Timestamp = time.Unix(t, 0)
	}
	if id, ok := n.AttrString("id"); ok {
		r.IDs = append(r.IDs, id)
	}
	if list, ok := n.Child("list"); ok {
		for _, it := range list.ChildrenByTag("item") {
			if id, ok := it.AttrString("id"); ok {
				r.IDs = append(r.IDs, id)
			}
		}
	}
	if typ == "retry" {
		if err := s.handleRetryReceipt(n, &r); err != nil {
			s.log.Warn("could not answer retry receipt", "id", r.IDs, "error", err)
		}
	}
	s.c.emit(r)
}

func (s *session) ackReceipt(n *wabin.Node) {
	attrs := wabin.Attrs{"class": "receipt", "id": n.Attrs["id"], "to": n.Attrs["from"]}
	if t, ok := n.Attrs["type"]; ok {
		attrs["type"] = t
	}
	if p, ok := n.Attrs["participant"]; ok {
		attrs["participant"] = p
	}
	s.sendOrLog(wabin.Node{Tag: "ack", Attrs: attrs})
}

var errRetryNotFound = errors.New("message not found for retry")

// handleRetryReceipt re-encrypts and resends one of our messages to the
// device that failed to decrypt it.
func (s *session) handleRetryReceipt(n *wabin.Node, r *Receipt) error {
	retry, ok := n.Child("retry")
	if !ok {
		return errors.New("retry receipt without <retry>")
	}
	msgID, _ := retry.AttrString("id")
	count, _ := retry.AttrInt("count")
	if count >= 10 {
		return errors.New("too many retries")
	}
	chat := r.Chat
	plaintext, err := s.c.cfg.Store.GetSentMessage(s.ctx, s.c.cfg.InstanceID, chat.String(), msgID)
	if errors.Is(err, store.ErrNotFound) && chat.Server == wabin.ServerUser {
		// We store sent messages under the LID chat.
		if lid, _ := s.c.cfg.Store.LIDForPN(s.ctx, s.c.cfg.InstanceID, chat.User); lid != "" {
			chat = wabin.JID{User: lid, Server: wabin.ServerLID}
			plaintext, err = s.c.cfg.Store.GetSentMessage(s.ctx, s.c.cfg.InstanceID, chat.String(), msgID)
		}
	}
	if err != nil {
		return errRetryNotFound
	}
	device := r.Sender
	isOwn := device.User == s.dev.JID.User || device.User == s.dev.LID.User
	if isOwn {
		var msg wae2e.Message
		if err := proto.Unmarshal(plaintext, &msg); err != nil {
			return err
		}
		if plaintext, err = proto.Marshal(&wae2e.Message{DeviceSentMessage: &wae2e.DeviceSentMessage{
			DestinationJID: proto.String(chat.String()), Message: &msg,
		}}); err != nil {
			return err
		}
	}
	var bundle *signal.PreKeyBundle
	if _, hasKeys := n.Child("keys"); hasKeys {
		if bundle, err = parseBundle(n); err != nil {
			return err
		}
	} else if count >= 2 {
		// The sender's session is probably broken: start a fresh one.
		bundles, err := s.fetchBundles(s.ctx, []wabin.JID{device})
		if err != nil {
			return err
		}
		bundle = bundles[device]
	}
	local := signal.LocalIdentity{Key: &s.dev.IdentityKey.KeyPair, RegistrationID: s.dev.RegistrationID}
	enc, isPreKey, err := s.c.encryptForDevice(s.ctx, local, device, bundle, padMessage(plaintext), wabin.Attrs{"count": count})
	if err != nil {
		return err
	}
	attrs := wabin.Attrs{"to": n.Attrs["from"], "type": "text", "id": msgID, "t": retry.Attrs["t"], "device_fanout": "false"}
	for _, k := range []string{"participant", "recipient"} {
		if v, ok := n.Attrs[k]; ok {
			attrs[k] = v
		}
	}
	content := []wabin.Node{*enc}
	if isPreKey {
		content = append(content, wabin.Node{Tag: "device-identity", Content: s.dev.Account})
	}
	s.log.Info("resending message after retry receipt", "id", msgID, "count", count, "device", wabin.MaskJID(device))
	return s.sendNode(wabin.Node{Tag: "message", Attrs: attrs, Content: content})
}
