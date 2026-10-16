package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
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
		s.nack(n, nackParsingError)
		return
	}
	if info.Chat.Server == wabin.ServerNewsletter || info.Sender.Server == wabin.ServerBot {
		s.deferStanza(n, "newsletters and bots not implemented yet")
		return
	}
	encJID := s.sessionAddress(info)
	encs := n.ChildrenByTag("enc")
	if len(encs) == 0 {
		if n.HasChild("unavailable") {
			s.log.Warn("message unavailable on this device", "id", info.ID)
			s.ack(n)
			return
		}
		s.nack(n, nackUnrecognizedStanza)
		return
	}
	invalid, handledAny := false, false
	var unsupported []string
	for i := range encs {
		switch s.handleEnc(n, info, encJID, &encs[i]) {
		case encHandled:
			handledAny = true
		case encInvalid:
			handledAny, invalid = true, true
		case encUnsupported:
			t, _ := encs[i].AttrString("type")
			unsupported = append(unsupported, t)
		case encStop:
			return
		}
	}
	switch {
	case !handledAny && len(unsupported) > 0:
		s.deferStanza(n, "unsupported enc types "+strings.Join(unsupported, ","))
	case invalid:
		s.nack(n, nackInvalidProtobuf)
	default:
		s.sendMessageReceipt(n, info)
	}
}

// sessionAddress learns PN<->LID mappings from a message and returns the
// address its Signal session is keyed by (the LID when known).
func (s *session) sessionAddress(info *MessageInfo) wabin.JID {
	if !info.SenderAlt.IsEmpty() {
		if info.Sender.Server == wabin.ServerLID {
			s.c.learnLID(s.ctx, info.SenderAlt, info.Sender)
		} else {
			s.c.learnLID(s.ctx, info.Sender, info.SenderAlt)
		}
	}
	encJID := info.Sender
	if encJID.Server != wabin.ServerUser {
		return encJID
	}
	if info.SenderAlt.Server == wabin.ServerLID {
		return info.SenderAlt
	}
	if lid, err := s.c.cfg.Store.LIDForPN(s.ctx, s.c.cfg.InstanceID, encJID.User); err == nil && lid != "" {
		return wabin.JID{User: lid, Device: encJID.Device, Server: wabin.ServerLID}
	}
	return encJID
}

// encOutcome is the result of handling one <enc> child.
type encOutcome int

const (
	encHandled     encOutcome = iota // decrypted and dispatched (or a duplicate)
	encInvalid                       // decrypted but unusable content
	encUnsupported                   // enc type we cannot decrypt
	encStop                          // the stanza was answered or must be redelivered
)

// handleEnc decrypts one <enc> and dispatches its content.
func (s *session) handleEnc(n *wabin.Node, info *MessageInfo, encJID wabin.JID, enc *wabin.Node) encOutcome {
	encType, _ := enc.AttrString("type")
	var plaintext, hash []byte
	var err error
	switch {
	case encType == "pkmsg" || encType == "msg":
		plaintext, hash, err = s.decryptEnc(info, encJID, enc, encType)
	case encType == "skmsg" && info.IsGroup:
		plaintext, hash, err = s.decryptGroupEnc(info, encJID, enc)
	default:
		return encUnsupported
	}
	if errors.Is(err, errAlreadyHandled) || errors.Is(err, signal.ErrDuplicateMessage) {
		s.log.Debug("duplicate message ignored", "id", info.ID, "reason", err)
		return encHandled
	}
	if err != nil {
		// A missing session or sender key is the case retry receipts exist
		// for (the sender re-sends with a fresh key): expected, not a fault.
		// Anything else (bad MAC, malformed message) is worth a warning.
		level := slog.LevelWarn
		if errors.Is(err, signal.ErrNoSession) || errors.Is(err, signal.ErrMissingPreKey) {
			level = slog.LevelInfo
		}
		s.log.Log(s.ctx, level, "decryption failed; requesting a retry", "id", info.ID, "type", encType,
			"from", wabin.MaskJID(info.Sender), "error", err)
		if s.ctx.Err() == nil { // when shutting down, leave it for redelivery
			s.sendRetryReceipt(n, info)
			s.ack(n)
			s.c.emit(UndecryptableMessage{Info: *info, Err: err})
		}
		return encStop
	}
	var msg wae2e.Message
	if v, _ := enc.AttrString("v"); v != "2" {
		err = fmt.Errorf("unsupported message version %q", v)
	} else {
		err = proto.Unmarshal(plaintext, &msg)
	}
	if err != nil {
		s.log.Warn("invalid message content", "id", info.ID, "error", err)
		s.markHandled(hash)
		return encInvalid
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
		if err := s.dispatchMessage(info, &msg, int(retries)); err != nil {
			// Leave it unacknowledged: the server redelivers it and the
			// buffered plaintext is reused.
			s.log.Warn("message handling failed; leaving it for redelivery", "id", info.ID, "error", err)
			return encStop
		}
	}
	s.markHandled(hash)
	return encHandled
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
	hash := eventHash(ct, encType, addr)

	c := s.c
	unlock := c.locks.lock(addr)
	defer unlock()

	var padded []byte
	var identityChanged bool
	err := c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
		// One round trip for the reads, one for the writes.
		st, err := tx.LoadDecryptState(s.ctx, c.cfg.InstanceID, addr, hash)
		switch {
		case err != nil:
			return err
		case st.Buffered != nil && st.Buffered.Plaintext == nil:
			return errAlreadyHandled
		case st.Buffered != nil:
			padded = st.Buffered.Plaintext
			return nil
		}
		var remoteIdentity [32]byte
		if padded, remoteIdentity, err = s.decryptSession(tx, st.Session, encType, ct); err != nil {
			return err
		}
		// Trust on first use, auto-accepting changes like WhatsApp clients
		// do (users reinstall often); a change is reported as an event.
		identityChanged = st.Identity != nil && *st.Identity != remoteIdentity
		return tx.Batch(s.ctx, func(b *store.Store) error {
			if st.Identity == nil || identityChanged {
				if err := b.PutIdentity(s.ctx, c.cfg.InstanceID, addr, remoteIdentity); err != nil {
					return err
				}
			}
			if err := b.PutSession(s.ctx, c.cfg.InstanceID, addr, st.Session); err != nil {
				return err
			}
			return b.PutBufferedEvent(s.ctx, c.cfg.InstanceID, hash, padded, info.Timestamp)
		})
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

// eventHash identifies an encrypted payload from one sender, for the
// decrypted-event buffer.
func eventHash(ct []byte, encType, addr string) []byte {
	h := sha256.New()
	for _, p := range [][]byte{ct, {0}, []byte(encType), {0}, []byte(addr)} {
		h.Write(p)
	}
	return h.Sum(nil)
}

// decryptSession decrypts a pkmsg or msg with rec, consuming the one-time
// prekey a pkmsg used. It returns the padded plaintext and the sender's
// identity key.
func (s *session) decryptSession(tx *store.Store, rec *signal.Record, encType string, ct []byte) ([]byte, [32]byte, error) {
	if encType != "pkmsg" {
		padded, err := rec.DecryptMessage(ct)
		if err != nil {
			return nil, [32]byte{}, err
		}
		id, _ := rec.RemoteIdentity()
		return padded, id, nil
	}
	res, err := rec.DecryptPreKeyMessage(
		signal.LocalIdentity{Key: &s.dev.IdentityKey.KeyPair, RegistrationID: s.dev.RegistrationID},
		store.PreKeyStore{S: tx, Ctx: s.ctx, Device: s.dev}, ct)
	if err != nil {
		return nil, [32]byte{}, err
	}
	if res.UsedPreKeyID != nil {
		if err := tx.RemovePreKey(s.ctx, s.c.cfg.InstanceID, *res.UsedPreKeyID); err != nil {
			return nil, [32]byte{}, err
		}
	}
	return res.Plaintext, res.RemoteIdentity, nil
}

func (s *session) markHandled(hash []byte) {
	if err := s.c.cfg.Store.MarkEventHandled(context.WithoutCancel(s.ctx), s.c.cfg.InstanceID, hash); err != nil {
		s.log.Warn("failed to mark event handled", "error", err)
	}
}

// dispatchMessage unwraps container messages and emits the event.
func (s *session) dispatchMessage(info *MessageInfo, msg *wae2e.Message, retries int) error {
	evt := Message{Info: *info, Raw: msg, Message: msg, RetryCount: retries}
	if dsm := msg.GetDeviceSentMessage(); dsm != nil && dsm.GetMessage() != nil {
		// Sent by our own phone: the real content is wrapped.
		evt.Message = dsm.GetMessage()
		if dest, err := wabin.ParseJID(dsm.GetDestinationJID()); err == nil && !dest.IsEmpty() {
			evt.Info.Chat = dest
		}
	}
	// Stored with its wrappers, so "view once" is not lost.
	stored := evt.Message
	evt.Message, evt.ViewOnce = Unwrap(evt.Message)

	// Protocol messages from our own primary device carry sync data. Only
	// trust them from our own account. They are stored before returning so
	// the stanza is not acknowledged until they are safe.
	if pm := evt.Message.GetProtocolMessage(); pm != nil && info.IsFromMe {
		if share := pm.GetAppStateSyncKeyShare(); share != nil {
			if err := s.handleKeyShare(share); err != nil {
				return fmt.Errorf("store app state keys: %w", err)
			}
		}
		if pr := pm.GetPeerDataOperationRequestResponseMessage(); pr != nil {
			// Applying a recovery waits for the app state lock, which a sync
			// may hold across network round trips: never on the handler.
			// A lost response is harmless; recovery is requested again.
			s.goSafe("peer-data-response", func() {
				if err := s.handlePeerDataResponse(pr); err != nil {
					s.log.Warn("could not process peer data response", "error", err)
				}
			})
		}
		if hn := pm.GetHistorySyncNotification(); hn != nil {
			if err := s.enqueueHistory(hn); err != nil {
				return fmt.Errorf("queue history sync: %w", err)
			}
		}
	}
	if pm := evt.Message.GetProtocolMessage(); pm == nil || pm.GetType() == wae2e.ProtocolMessage_REVOKE || pm.GetType() == wae2e.ProtocolMessage_MESSAGE_EDIT {
		s.c.storeLiveMessage(s.ctx, evt.Info.Chat, info.Sender, info.ID, info.IsFromMe, info.Timestamp, info.PushName, stored)
	}
	if sender := info.Sender.ToNonAD().String(); info.PushName != "" && !info.IsFromMe && !s.c.nameSeen.unchanged(sender, info.PushName) {
		if err := s.c.cfg.Store.PutPushName(s.ctx, s.c.cfg.InstanceID, sender, info.PushName); err != nil {
			s.c.nameSeen.forget(sender)
			s.log.Debug("push name not stored", "error", err)
		}
	}
	s.c.emit(evt)
	return nil
}

// unwrapMessage strips wrapper containers (ephemeral, view once, ...).
func unwrapMessage(m *wae2e.Message) *wae2e.Message {
	inner, _ := Unwrap(m)
	return inner
}

// Unwrap strips wrapper containers (ephemeral, view once, document with
// caption) and reports whether the content was sent as view once (by its
// wrapper or the media's own flag).
func Unwrap(m *wae2e.Message) (inner *wae2e.Message, viewOnce bool) {
	for range 4 {
		var next *wae2e.Message
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			next = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			next, viewOnce = m.GetViewOnceMessage().GetMessage(), true
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			next, viewOnce = m.GetViewOnceMessageV2().GetMessage(), true
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			next, viewOnce = m.GetViewOnceMessageV2Extension().GetMessage(), true
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			next = m.GetDocumentWithCaptionMessage().GetMessage()
		}
		if next == nil {
			break
		}
		m = next
	}
	viewOnce = viewOnce || m.GetImageMessage().GetViewOnce() || m.GetVideoMessage().GetViewOnce() || m.GetAudioMessage().GetViewOnce()
	return m, viewOnce
}

// sendMessageReceipt confirms delivery. It also acts as the stanza ack.
func (s *session) sendMessageReceipt(n *wabin.Node, info *MessageInfo) {
	attrs := wabin.Attrs{"id": info.ID, "to": n.Attrs["from"]}.CopyFrom(n.Attrs, "recipient", "participant")
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
