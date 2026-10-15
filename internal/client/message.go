package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/store"
)

// Nack codes for <ack error="...">, as used by WhatsApp clients.
const (
	nackParsingError       = 487
	nackUnrecognizedStanza = 488
	nackInvalidProtobuf    = 491
)

// MessageInfo describes an incoming message stanza.
type MessageInfo struct {
	ID        string
	Chat      wabin.JID // conversation (user or group, non-AD)
	Sender    wabin.JID // sending device
	SenderAlt wabin.JID // the sender's other identity (PN for a LID sender and vice versa), if known
	IsFromMe  bool      // sent by another device of our own account
	IsGroup   bool
	Timestamp time.Time
	PushName  string
	Type      string // "text", "media", ...
}

// errAlreadyHandled marks a redelivered message that was fully processed.
var errAlreadyHandled = errors.New("client: message already handled")

// addrLocks serializes Signal operations per address inside this process.
// The database row lock (FOR UPDATE) covers other processes; this avoids
// two goroutines here racing to create the same new session.
type addrLocks struct{ m [64]sync.Mutex }

func (l *addrLocks) lock(addr string) func() {
	h := fnv.New32a()
	h.Write([]byte(addr))
	mu := &l.m[h.Sum32()%uint32(len(l.m))]
	mu.Lock()
	return mu.Unlock
}

func (s *session) parseMessageInfo(n *wabin.Node) (*MessageInfo, error) {
	from, ok := n.AttrJID("from")
	if !ok {
		return nil, errors.New("message without from")
	}
	info := &MessageInfo{}
	info.ID, _ = n.AttrString("id")
	if info.ID == "" {
		return nil, errors.New("message without id")
	}
	if t, ok := n.AttrInt("t"); ok {
		info.Timestamp = time.Unix(t, 0)
	}
	info.PushName, _ = n.AttrString("notify")
	info.Type, _ = n.AttrString("type")

	own, ownLID := s.dev.JID, s.dev.LID
	switch {
	case from.Server == wabin.ServerGroup || from.Server == wabin.ServerBroadcast:
		info.IsGroup = true
		info.Chat = from
		if info.Sender, ok = n.AttrJID("participant"); !ok {
			return nil, errors.New("group message without participant")
		}
		if alt, ok := n.AttrJID("participant_pn"); ok {
			info.SenderAlt = alt
		} else if alt, ok := n.AttrJID("participant_lid"); ok {
			info.SenderAlt = alt
		}
		info.IsFromMe = info.Sender.User == own.User || info.Sender.User == ownLID.User
	case from.User == own.User || (ownLID.User != "" && from.User == ownLID.User):
		info.IsFromMe = true
		info.Sender = from
		if r, ok := n.AttrJID("recipient"); ok {
			info.Chat = r
		} else {
			info.Chat = from.ToNonAD()
		}
	default:
		info.Sender = from
		info.Chat = from.ToNonAD()
		key := "sender_lid"
		if from.Server == wabin.ServerLID {
			key = "sender_pn"
		}
		info.SenderAlt, _ = n.AttrJID(key)
	}
	if !info.SenderAlt.IsEmpty() && info.SenderAlt.Device == 0 {
		info.SenderAlt.Device = info.Sender.Device
	}
	return info, nil
}

// handleMessage processes an incoming <message>. The stanza is acknowledged
// (by receipt or ack) only after its content was decrypted, stored and
// handed to the event handler; otherwise the server redelivers it later.
func (s *session) handleMessage(n *wabin.Node) {
	info, err := s.parseMessageInfo(n)
	if err != nil {
		s.log.Warn("unparseable message", "error", err)
		s.sendAckCode(n, nackParsingError)
		return
	}
	if info.Chat.Server == wabin.ServerNewsletter || info.Sender.Server == wabin.ServerBot {
		s.deferStanza(n, "newsletters and bots not implemented yet")
		return
	}
	// Learn PN<->LID mappings as they appear.
	if !info.SenderAlt.IsEmpty() {
		if info.Sender.Server == wabin.ServerLID {
			s.c.learnLID(s.ctx, info.SenderAlt, info.Sender)
		} else {
			s.c.learnLID(s.ctx, info.Sender, info.SenderAlt)
		}
	}
	// Sessions are keyed by LID when we know it.
	encJID := info.Sender
	if encJID.Server == wabin.ServerUser {
		if info.SenderAlt.Server == wabin.ServerLID {
			encJID = info.SenderAlt
		} else if lid, err := s.c.cfg.Store.LIDForPN(s.ctx, s.c.cfg.InstanceID, encJID.User); err == nil && lid != "" {
			encJID = wabin.JID{User: lid, Device: encJID.Device, Server: wabin.ServerLID}
		}
	}

	encs := n.ChildrenByTag("enc")
	if len(encs) == 0 {
		if _, ok := n.Child("unavailable"); ok {
			s.log.Warn("message unavailable on this device", "id", info.ID)
			s.sendAck(n)
			return
		}
		s.sendAckCode(n, nackUnrecognizedStanza)
		return
	}
	protobufFailed, handledAny := false, false
	var unsupported []string
	for i := range encs {
		enc := &encs[i]
		encType, _ := enc.AttrString("type")
		var plaintext, hash []byte
		var err error
		switch {
		case encType == "pkmsg" || encType == "msg":
			plaintext, hash, err = s.decryptEnc(info, encJID, enc, encType)
		case encType == "skmsg" && info.IsGroup:
			plaintext, hash, err = s.decryptGroupEnc(info, encJID, enc)
		default:
			unsupported = append(unsupported, encType)
			continue
		}
		if errors.Is(err, errAlreadyHandled) || errors.Is(err, signal.ErrDuplicateMessage) {
			s.log.Debug("duplicate message ignored", "id", info.ID, "reason", err)
			handledAny = true
			continue
		}
		if err != nil {
			s.log.Warn("decryption failed", "id", info.ID, "type", encType, "from", wabin.MaskJID(info.Sender), "error", err)
			if s.ctx.Err() != nil {
				return // shutting down: leave it for redelivery
			}
			s.sendRetryReceipt(n, info)
			s.sendAck(n)
			s.c.emit(UndecryptableMessage{Info: *info, Err: err})
			return
		}
		handledAny = true
		if v, _ := enc.AttrString("v"); v != "2" {
			s.log.Warn("unsupported message version", "v", v, "id", info.ID)
			protobufFailed = true
			s.markHandled(hash)
			continue
		}
		var msg wae2e.Message
		if err := proto.Unmarshal(plaintext, &msg); err != nil {
			s.log.Warn("invalid message protobuf", "id", info.ID, "error", err)
			protobufFailed = true
			s.markHandled(hash)
			continue
		}
		if skdm := msg.GetSenderKeyDistributionMessage(); skdm != nil {
			if err := s.processSKDM(info, encJID, skdm); err != nil {
				// Not fatal: a later skmsg from this sender will fail and
				// trigger a retry receipt, which brings a fresh distribution.
				s.log.Warn("could not store sender key", "id", info.ID, "error", err)
			}
		}
		if hasContent(&msg) {
			retries, _ := enc.AttrInt("count")
			s.dispatchMessage(info, &msg, int(retries))
		}
		s.markHandled(hash)
	}
	if !handledAny && len(unsupported) > 0 {
		s.deferStanza(n, "unsupported enc types "+strings.Join(unsupported, ","))
		return
	}
	if protobufFailed {
		s.sendAckCode(n, nackInvalidProtobuf)
		return
	}
	s.sendMessageReceipt(n, info)
}

// hasContent reports whether a message carries anything besides key
// distribution / context metadata (group "pkmsg"s often carry only the
// sender key and must not surface as empty messages).
func hasContent(m *wae2e.Message) bool {
	c := proto.Clone(m).(*wae2e.Message)
	c.SenderKeyDistributionMessage = nil
	c.MessageContextInfo = nil
	return proto.Size(c) > 0
}

// processSKDM stores a sender key received from a group member.
func (s *session) processSKDM(info *MessageInfo, from wabin.JID, skdm *wae2e.SenderKeyDistributionMessage) error {
	group := info.Chat
	if g, err := wabin.ParseJID(skdm.GetGroupID()); err == nil && !g.IsEmpty() {
		group = g
	}
	if group.Server != wabin.ServerGroup && group.Server != wabin.ServerBroadcast {
		return fmt.Errorf("sender key for non-group chat %s", group.Server)
	}
	addr := signalAddress(from)
	unlock := s.c.locks.lock("sk|" + group.String() + "|" + addr)
	defer unlock()
	return s.c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
		rec, err := tx.LoadSenderKey(s.ctx, s.c.cfg.InstanceID, group.String(), addr)
		if err != nil {
			return err
		}
		if err := rec.ProcessDistribution(skdm.GetAxolotlSenderKeyDistributionMessage()); err != nil {
			return err
		}
		return tx.PutSenderKey(s.ctx, s.c.cfg.InstanceID, group.String(), addr, rec)
	})
}

// decryptGroupEnc decrypts a sender-key message, transactionally like
// decryptEnc.
func (s *session) decryptGroupEnc(info *MessageInfo, from wabin.JID, enc *wabin.Node) ([]byte, []byte, error) {
	ct, ok := enc.Bytes()
	if !ok || len(ct) == 0 {
		return nil, nil, errors.New("enc without payload")
	}
	group := info.Chat.String()
	addr := signalAddress(from)
	hh := sha256.New()
	for _, part := range [][]byte{ct, {0}, []byte("skmsg"), {0}, []byte(group), {0}, []byte(addr)} {
		hh.Write(part)
	}
	hash := hh.Sum(nil)
	unlock := s.c.locks.lock("sk|" + group + "|" + addr)
	defer unlock()
	var padded []byte
	err := s.c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
		buf, err := tx.GetBufferedEvent(s.ctx, s.c.cfg.InstanceID, hash)
		if err != nil {
			return err
		}
		if buf != nil {
			if buf.Plaintext == nil {
				return errAlreadyHandled
			}
			padded = buf.Plaintext
			return nil
		}
		rec, err := tx.LoadSenderKey(s.ctx, s.c.cfg.InstanceID, group, addr)
		if err != nil {
			return err
		}
		if padded, err = rec.DecryptGroup(ct); err != nil {
			return err
		}
		if err := tx.PutSenderKey(s.ctx, s.c.cfg.InstanceID, group, addr, rec); err != nil {
			return err
		}
		return tx.PutBufferedEvent(s.ctx, s.c.cfg.InstanceID, hash, padded, info.Timestamp)
	})
	if err != nil {
		return nil, nil, err
	}
	pt, err := unpadMessage(padded)
	if err != nil {
		s.markHandled(hash)
		return nil, nil, err
	}
	return pt, hash, nil
}

// decryptEnc decrypts one <enc> inside a single database transaction:
// session update, prekey consumption, identity update and the plaintext
// buffer commit together or not at all.
func (s *session) decryptEnc(info *MessageInfo, from wabin.JID, enc *wabin.Node, encType string) ([]byte, []byte, error) {
	ct, ok := enc.Bytes()
	if !ok || len(ct) == 0 {
		return nil, nil, errors.New("enc without payload")
	}
	addr := signalAddress(from)
	hh := sha256.New()
	hh.Write(ct)
	hh.Write([]byte{0})
	hh.Write([]byte(encType))
	hh.Write([]byte{0})
	hh.Write([]byte(addr))
	hash := hh.Sum(nil)

	c := s.c
	unlock := c.locks.lock(addr)
	defer unlock()

	var padded []byte
	var identityChanged bool
	err := c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
		buf, err := tx.GetBufferedEvent(s.ctx, c.cfg.InstanceID, hash)
		if err != nil {
			return err
		}
		if buf != nil {
			if buf.Plaintext == nil {
				return errAlreadyHandled
			}
			padded = buf.Plaintext
			return nil
		}
		rec, err := tx.LoadSession(s.ctx, c.cfg.InstanceID, addr)
		if err != nil {
			return err
		}
		var remoteIdentity [32]byte
		switch encType {
		case "pkmsg":
			res, err := rec.DecryptPreKeyMessage(
				signal.LocalIdentity{Key: &s.dev.IdentityKey.KeyPair, RegistrationID: s.dev.RegistrationID},
				store.PreKeyStore{S: tx, Ctx: s.ctx, Device: s.dev}, ct)
			if err != nil {
				return err
			}
			padded, remoteIdentity = res.Plaintext, res.RemoteIdentity
			if res.UsedPreKeyID != nil {
				if err := tx.RemovePreKey(s.ctx, c.cfg.InstanceID, *res.UsedPreKeyID); err != nil {
					return err
				}
			}
		default:
			if padded, err = rec.DecryptMessage(ct); err != nil {
				return err
			}
			remoteIdentity, _ = rec.RemoteIdentity()
		}
		// Trust on first use, auto-accepting changes like WhatsApp clients
		// do (users reinstall often); a change is reported as an event.
		prev, err := tx.GetIdentity(s.ctx, c.cfg.InstanceID, addr)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case prev != remoteIdentity:
			identityChanged = true
		}
		if err := tx.PutIdentity(s.ctx, c.cfg.InstanceID, addr, remoteIdentity); err != nil {
			return err
		}
		if err := tx.PutSession(s.ctx, c.cfg.InstanceID, addr, rec); err != nil {
			return err
		}
		return tx.PutBufferedEvent(s.ctx, c.cfg.InstanceID, hash, padded, info.Timestamp)
	})
	if err != nil {
		return nil, nil, err
	}
	if identityChanged {
		s.log.Info("identity key changed", "jid", wabin.MaskJID(from))
		c.emit(IdentityChanged{JID: from})
	}
	pt, err := unpadMessage(padded)
	if err != nil {
		// The session advanced but the payload is unusable; report it as
		// handled to avoid an endless redelivery loop.
		s.markHandled(hash)
		return nil, nil, err
	}
	return pt, hash, nil
}

func (s *session) markHandled(hash []byte) {
	if err := s.c.cfg.Store.MarkEventHandled(context.WithoutCancel(s.ctx), s.c.cfg.InstanceID, hash); err != nil {
		s.log.Warn("failed to mark event handled", "error", err)
	}
}

// dispatchMessage unwraps container messages and emits the event.
func (s *session) dispatchMessage(info *MessageInfo, msg *wae2e.Message, retries int) {
	evt := Message{Info: *info, Raw: msg, Message: msg, RetryCount: retries}
	if dsm := msg.GetDeviceSentMessage(); dsm != nil && dsm.GetMessage() != nil {
		// Sent by our own phone: the real content is wrapped.
		evt.Message = dsm.GetMessage()
		if dest, err := wabin.ParseJID(dsm.GetDestinationJID()); err == nil && !dest.IsEmpty() {
			evt.Info.Chat = dest
		}
	}
	evt.Message = unwrapMessage(evt.Message)
	s.c.emit(evt)
}

// unwrapMessage strips wrapper containers (ephemeral, view once, ...).
func unwrapMessage(m *wae2e.Message) *wae2e.Message {
	for range 4 {
		var inner *wae2e.Message
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			inner = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			inner = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			inner = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			inner = m.GetViewOnceMessageV2Extension().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			inner = m.GetDocumentWithCaptionMessage().GetMessage()
		default:
			return m
		}
		m = inner
	}
	return m
}

// sendMessageReceipt confirms delivery. It also acts as the stanza ack.
func (s *session) sendMessageReceipt(n *wabin.Node, info *MessageInfo) {
	attrs := wabin.Attrs{"id": info.ID, "to": n.Attrs["from"]}
	for _, k := range []string{"recipient", "participant"} {
		if v, ok := n.Attrs[k]; ok {
			attrs[k] = v
		}
	}
	if info.IsFromMe {
		attrs["type"] = "sender"
		if info.Type == "peer_msg" {
			attrs["type"] = "peer_msg"
		}
	} else if !s.c.available.Load() {
		// "inactive": delivered to a device that is not online. Read
		// receipts are sent explicitly by the application (MarkRead).
		attrs["type"] = "inactive"
	}
	s.sendOrLog(wabin.Node{Tag: "receipt", Attrs: attrs})
}

func (s *session) sendAckCode(n *wabin.Node, code int) {
	if s.replaying {
		return
	}
	attrs := wabin.Attrs{"class": n.Tag, "id": n.Attrs["id"], "to": n.Attrs["from"], "error": code}
	for _, k := range []string{"participant", "recipient"} {
		if v, ok := n.Attrs[k]; ok {
			attrs[k] = v
		}
	}
	s.sendOrLog(wabin.Node{Tag: "ack", Attrs: attrs})
}

// sendAck is ack(n) under the name used by the message code.
func (s *session) sendAck(n *wabin.Node) { s.ack(n) }
