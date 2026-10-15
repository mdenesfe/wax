package client

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
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
	attrs := wabin.Attrs{"id": info.ID, "to": n.Attrs["from"], "type": "retry"}.CopyFrom(n.Attrs, "recipient", "participant")
	if info.Type == "peer_msg" && info.IsFromMe {
		attrs["category"] = "peer"
	}
	content := []wabin.Node{
		{Tag: "retry", Attrs: wabin.Attrs{"count": count, "id": info.ID, "t": n.Attrs["t"], "v": 1}},
		{Tag: "registration", Content: be32(s.dev.RegistrationID)},
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
	base := Receipt{Type: typ, Sender: from, Chat: from.ToNonAD()}
	if p, ok := n.AttrJID("participant"); ok {
		base.Sender = p
	}
	if rc, ok := n.AttrJID("recipient"); ok {
		base.Chat = rc
	}
	if t, ok := n.AttrInt("t"); ok {
		base.Timestamp = time.Unix(t, 0)
	}
	// Aggregated group receipts: <participants key="msgid"><user jid t/>...
	if parts := n.ChildrenByTag("participants"); len(parts) > 0 {
		s.emitAggregatedReceipts(parts, base)
		return
	}
	r := base
	r.IDs = receiptIDs(n)
	if typ == "retry" {
		if err := s.handleRetryReceipt(n, &r); err != nil {
			s.log.Warn("could not answer retry receipt", "id", r.IDs, "error", err)
		}
	}
	s.c.emit(r)
}

// emitAggregatedReceipts emits one receipt per user and message of a
// group receipt.
func (s *session) emitAggregatedReceipts(parts []wabin.Node, base Receipt) {
	for _, p := range parts {
		key, _ := p.AttrString("key")
		for _, u := range p.ChildrenByTag("user") {
			r := base
			r.IDs = []string{key}
			r.Sender, _ = u.AttrJID("jid")
			if t, ok := u.AttrInt("t"); ok {
				r.Timestamp = time.Unix(t, 0)
			}
			s.c.emit(r)
		}
	}
}

// receiptIDs collects the message ids of a receipt: the id attribute plus
// any <list><item id/></list>.
func receiptIDs(n *wabin.Node) []string {
	var ids []string
	if id, ok := n.AttrString("id"); ok {
		ids = append(ids, id)
	}
	list, _ := n.Child("list")
	for _, it := range list.ChildrenByTag("item") {
		if id, ok := it.AttrString("id"); ok {
			ids = append(ids, id)
		}
	}
	return ids
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
	chat, msg, err := s.loadSentMessage(r.Chat, msgID)
	if err != nil {
		return err
	}
	device := r.Sender
	isGroup := chat.Server == wabin.ServerGroup || chat.Server == wabin.ServerBroadcast
	payload, err := s.retryPayload(chat, device, isGroup, msg)
	if err != nil {
		return err
	}
	plaintext, err := proto.Marshal(payload)
	if err != nil {
		return err
	}
	// Sessions live under the LID identity when it is known.
	encDevice := s.c.encryptionJIDs(s.ctx, []wabin.JID{device})[device]
	bundle, err := s.retryBundle(n, encDevice, count)
	if err != nil {
		return err
	}
	local := signal.LocalIdentity{Key: &s.dev.IdentityKey.KeyPair, RegistrationID: s.dev.RegistrationID}
	enc, isPreKey, err := s.c.encryptForDevice(s.ctx, local, encDevice, bundle, padMessage(plaintext), wabin.Attrs{"count": count})
	if err != nil {
		return err
	}
	attrs := wabin.Attrs{"to": n.Attrs["from"], "type": messageType(msg), "id": msgID, "t": retry.Attrs["t"]}.
		CopyFrom(n.Attrs, "participant", "recipient")
	if !isGroup {
		attrs["device_fanout"] = "false"
	}
	content := []wabin.Node{*enc}
	if isPreKey {
		content = append(content, wabin.Node{Tag: "device-identity", Content: s.dev.Account})
	}
	s.log.Info("resending message after retry receipt", "id", msgID, "count", count, "device", wabin.MaskJID(device))
	return s.sendNode(wabin.Node{Tag: "message", Attrs: attrs, Content: content})
}

// loadSentMessage finds a message we sent, also under the LID chat (where
// sent messages are stored) when the receipt names the PN chat.
func (s *session) loadSentMessage(chat wabin.JID, msgID string) (wabin.JID, *wae2e.Message, error) {
	st, inst := s.c.cfg.Store, s.c.cfg.InstanceID
	plaintext, err := st.GetSentMessage(s.ctx, inst, chat.String(), msgID)
	if errors.Is(err, store.ErrNotFound) && chat.Server == wabin.ServerUser {
		if lid, _ := st.LIDForPN(s.ctx, inst, chat.User); lid != "" {
			chat = wabin.JID{User: lid, Server: wabin.ServerLID}
			plaintext, err = st.GetSentMessage(s.ctx, inst, chat.String(), msgID)
		}
	}
	if err != nil {
		return chat, nil, errRetryNotFound
	}
	var msg wae2e.Message
	if err := proto.Unmarshal(plaintext, &msg); err != nil {
		return chat, nil, fmt.Errorf("stored message: %w", err)
	}
	return chat, &msg, nil
}

// retryPayload is what a retry resends to device: in groups the content
// plus a fresh sender key distribution (the device lost our key), to our
// own devices a DeviceSentMessage, otherwise the message itself.
func (s *session) retryPayload(chat, device wabin.JID, isGroup bool, msg *wae2e.Message) (*wae2e.Message, error) {
	switch {
	case isGroup:
		skdm, err := s.c.senderKeyDistribution(s.ctx, chat, s.ownDeviceForGroups())
		if err != nil {
			return nil, err
		}
		out := proto.Clone(msg).(*wae2e.Message)
		out.SenderKeyDistributionMessage = &wae2e.SenderKeyDistributionMessage{
			GroupID: proto.String(chat.String()), AxolotlSenderKeyDistributionMessage: skdm,
		}
		return out, nil
	case device.User == s.dev.JID.User || device.User == s.dev.LID.User:
		return &wae2e.Message{DeviceSentMessage: &wae2e.DeviceSentMessage{
			DestinationJID: proto.String(chat.String()), Message: msg,
		}}, nil
	}
	return msg, nil
}

// retryBundle returns the prekey bundle to start a new session with, or
// nil to keep the existing session.
func (s *session) retryBundle(n *wabin.Node, encDevice wabin.JID, count int64) (*signal.PreKeyBundle, error) {
	if n.HasChild("keys") {
		// The receipt carries a fresh bundle from the failing device.
		return parseBundle(n)
	}
	if count < 2 {
		return nil, nil
	}
	// The device's session is probably broken: start a fresh one.
	bundles, err := s.fetchBundles(s.ctx, []wabin.JID{encDevice})
	if err != nil {
		return nil, err
	}
	return bundles[encDevice], nil
}
