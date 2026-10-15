package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/proto/wae2e"
	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/store"
)

// Errors returned by SendMessage.
var (
	ErrNotLoggedIn     = errors.New("client: not logged in")
	ErrServerRejected  = errors.New("client: server rejected the message")
	ErrUnsupportedChat = errors.New("client: unsupported chat type")
)

// sendAckTimeout bounds the wait for the server's <ack> of a message.
const sendAckTimeout = 60 * time.Second

// SendResult describes a sent message.
type SendResult struct {
	ID        string
	Chat      wabin.JID // the address actually used (LID if known)
	Timestamp time.Time // server timestamp from the ack
}

// SendText sends a plain text message to a user.
func (c *Client) SendText(ctx context.Context, to wabin.JID, text string) (*SendResult, error) {
	return c.SendMessage(ctx, to, &wae2e.Message{Conversation: proto.String(text)})
}

// activeSession returns the current logged-in session.
func (c *Client) activeSession() (*session, error) {
	c.mu.Lock()
	s := c.current
	c.mu.Unlock()
	if s == nil || !s.ready.Load() {
		return nil, ErrNotLoggedIn
	}
	return s, nil
}

// SendMessage encrypts msg for every device of the recipient and every
// other device of our own account, sends it and waits for the server ack.
//
// It returns an error if the server did not acknowledge the message; the
// caller (the send queue) decides whether to retry. Retrying with the same
// message is safe: Signal sessions tolerate gaps in message counters.
func (c *Client) SendMessage(ctx context.Context, to wabin.JID, msg *wae2e.Message) (*SendResult, error) {
	s, err := c.activeSession()
	if err != nil {
		return nil, err
	}
	if to.Device != 0 {
		return nil, fmt.Errorf("%w: recipient must be a user JID, not a device", ErrUnsupportedChat)
	}
	var peerPN wabin.JID
	ownUser := s.dev.JID.ToNonAD()
	ownDevice := s.dev.JID
	switch to.Server {
	case wabin.ServerUser:
		lid, err := c.resolveLID(ctx, s, to)
		if err != nil {
			return nil, fmt.Errorf("resolve LID: %w", err)
		}
		peerPN, to = to, lid
		fallthrough
	case wabin.ServerLID:
		if s.dev.LID.IsEmpty() {
			return nil, fmt.Errorf("%w: own LID unknown", ErrUnsupportedChat)
		}
		ownUser, ownDevice = s.dev.LID.ToNonAD(), s.dev.LID
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedChat, to.Server)
	}

	id := newMessageID(s.dev.JID)
	plaintext, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	dsm, err := proto.Marshal(&wae2e.Message{
		DeviceSentMessage:  &wae2e.DeviceSentMessage{DestinationJID: proto.String(to.String()), Message: msg},
		MessageContextInfo: msg.GetMessageContextInfo(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal device sent message: %w", err)
	}
	// Keep a copy before sending so a retry receipt can always be answered.
	if err := c.cfg.Store.PutSentMessage(ctx, c.cfg.InstanceID, to.String(), id, plaintext); err != nil {
		return nil, err
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	devices, err := c.devices(ctx, s, []wabin.JID{to, ownUser})
	if err != nil {
		return nil, fmt.Errorf("device list: %w", err)
	}
	participants, includeIdentity, err := c.encryptForDevices(ctx, s, devices, ownDevice, ownUser, plaintext, dsm)
	if err != nil {
		return nil, err
	}
	if len(participants) == 0 {
		return nil, errors.New("client: message could not be encrypted for any device")
	}
	attrs := wabin.Attrs{"id": id, "type": messageType(msg), "to": to}
	if !peerPN.IsEmpty() {
		attrs["peer_recipient_pn"] = peerPN
	}
	content := []wabin.Node{{Tag: "participants", Content: participants}}
	if includeIdentity {
		content = append(content, wabin.Node{Tag: "device-identity", Content: s.dev.Account})
	}
	ack, err := s.sendAndWait(ctx, wabin.Node{Tag: "message", Attrs: attrs, Content: content}, id, sendAckTimeout)
	if err != nil {
		return nil, fmt.Errorf("send message %s: %w", id, err)
	}
	res := &SendResult{ID: id, Chat: to}
	if t, ok := ack.AttrInt("t"); ok {
		res.Timestamp = time.Unix(t, 0)
	}
	if code, ok := ack.AttrString("error"); ok && code != "" && code != "0" {
		return res, fmt.Errorf("%w: error %s", ErrServerRejected, code)
	}
	if ph, ok := ack.AttrString("phash"); ok && ph != "" && ph != participantListHash(devices) {
		s.log.Warn("server device list differs from ours; refreshing", "chat", wabin.MaskJID(to))
		c.devCache.invalidate(to)
		c.devCache.invalidate(ownUser)
	}
	return res, nil
}

func messageType(m *wae2e.Message) string {
	switch {
	case m.GetProtocolMessage() != nil, m.GetConversation() != "", m.GetExtendedTextMessage() != nil:
		return "text"
	}
	return "text"
}

// encryptForDevices encrypts for each device, fetching prekey bundles for
// devices without a session first. A device that fails is skipped (logged),
// mirroring WhatsApp clients: one broken device must not block delivery to
// the others.
func (c *Client) encryptForDevices(ctx context.Context, s *session, devices []wabin.JID, ownDevice, ownUser wabin.JID, plaintext, dsm []byte) ([]wabin.Node, bool, error) {
	var targets []wabin.JID
	var addrs []string
	for _, d := range devices {
		if d.User == ownDevice.User && d.Device == ownDevice.Device && d.Server == ownDevice.Server {
			continue // never encrypt to ourselves
		}
		if d.Server == wabin.ServerHosted || d.Server == wabin.ServerHostedLID {
			continue // business-hosted devices use a different protocol
		}
		targets = append(targets, d)
		addrs = append(addrs, signalAddress(d))
	}
	have, err := c.cfg.Store.HasSessions(ctx, c.cfg.InstanceID, addrs)
	if err != nil {
		return nil, false, err
	}
	var need []wabin.JID
	for _, d := range targets {
		if !have[signalAddress(d)] {
			need = append(need, d)
		}
	}
	bundles, err := s.fetchBundles(ctx, need)
	if err != nil {
		return nil, false, err
	}

	local := signal.LocalIdentity{Key: &s.dev.IdentityKey.KeyPair, RegistrationID: s.dev.RegistrationID}
	var nodes []wabin.Node
	includeIdentity := false
	for _, d := range targets {
		pt := plaintext
		if d.User == ownUser.User {
			pt = dsm
		}
		node, isPreKey, err := c.encryptForDevice(ctx, local, d, bundles[d], padMessage(append([]byte(nil), pt...)), nil)
		if err != nil {
			s.log.Warn("skipping device", "device", wabin.MaskJID(d), "error", err)
			continue
		}
		includeIdentity = includeIdentity || isPreKey
		nodes = append(nodes, wabin.Node{Tag: "to", Attrs: wabin.Attrs{"jid": d}, Content: []wabin.Node{*node}})
	}
	return nodes, includeIdentity, nil
}

// encryptForDevice encrypts one padded payload for one device in its own
// transaction (load, optionally start session, encrypt, save).
func (c *Client) encryptForDevice(ctx context.Context, local signal.LocalIdentity, d wabin.JID, bundle *signal.PreKeyBundle, padded []byte, extra wabin.Attrs) (*wabin.Node, bool, error) {
	addr := signalAddress(d)
	unlock := c.locks.lock(addr)
	defer unlock()
	var typ signal.MessageType
	var ct []byte
	err := c.cfg.Store.InTx(ctx, func(tx *store.Store) error {
		rec, err := tx.LoadSession(ctx, c.cfg.InstanceID, addr)
		if err != nil {
			return err
		}
		if bundle != nil {
			if err := rec.ProcessBundle(local, bundle); err != nil {
				return err
			}
			if err := tx.PutIdentity(ctx, c.cfg.InstanceID, addr, bundle.IdentityKey); err != nil {
				return err
			}
		} else if !rec.HasSession() {
			return signal.ErrNoSession
		}
		if typ, ct, err = rec.Encrypt(padded); err != nil {
			return err
		}
		return tx.PutSession(ctx, c.cfg.InstanceID, addr, rec)
	})
	if err != nil {
		return nil, false, err
	}
	attrs := wabin.Attrs{"v": "2", "type": "msg"}
	if typ == signal.TypePreKey {
		attrs["type"] = "pkmsg"
	}
	for k, v := range extra {
		attrs[k] = v
	}
	return &wabin.Node{Tag: "enc", Attrs: attrs, Content: ct}, typ == signal.TypePreKey, nil
}
