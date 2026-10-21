package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
	"github.com/mdenesfe/wax/proto/wae2e"
)

// Errors returned by SendMessage.
var (
	ErrNotLoggedIn     = errors.New("client: not logged in")
	ErrServerRejected  = errors.New("client: server rejected the message")
	ErrUnsupportedChat = errors.New("client: unsupported chat type")
	ErrNotOnWhatsApp   = errors.New("client: recipient is not on WhatsApp")
)

// sendAckTimeout bounds the wait for the server's <ack> of a message.
const sendAckTimeout = 60 * time.Second

// SendResult describes a sent message.
type SendResult struct {
	ID        string
	Chat      wabin.JID // the address actually used (LID if known)
	Timestamp time.Time // server timestamp from the ack
}

// SendText sends a plain text message to a user or group.
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

// SendMessage encrypts msg for every recipient device (and every other
// device of our own account), sends it and waits for the server ack.
//
// It returns an error if the server did not acknowledge the message; the
// caller (the send queue) decides whether to retry. Retrying with the same
// message is safe: Signal sessions tolerate gaps in message counters.
func (c *Client) SendMessage(ctx context.Context, to wabin.JID, msg *wae2e.Message) (*SendResult, error) {
	return c.SendMessageWithID(ctx, to, msg, "")
}

// GenerateMessageID returns a new random message id in WhatsApp Web's
// format. Callers that may retry a send (the outbox) generate the id once
// and pass it to every attempt, so a resend after a crash is recognized as
// the same message by recipients instead of appearing twice.
func GenerateMessageID() string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	return webMessageIDPrefix + strings.ToUpper(hex.EncodeToString(b[:]))
}

// SendMessageWithID is SendMessage with a caller-chosen message id ("" =
// generate one).
func (c *Client) SendMessageWithID(ctx context.Context, to wabin.JID, msg *wae2e.Message, id string) (*SendResult, error) {
	s, err := c.activeSession()
	if err != nil {
		return nil, err
	}
	if to.Device != 0 {
		return nil, fmt.Errorf("%w: recipient must be a user or group JID, not a device", ErrUnsupportedChat)
	}
	if id == "" {
		id = newMessageID(s.dev.JID)
	}
	switch to.Server {
	case wabin.ServerUser, wabin.ServerLID:
		return c.sendDirect(ctx, s, to, id, msg)
	case wabin.ServerGroup:
		return c.sendGroup(ctx, s, to, id, msg)
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedChat, to.Server)
}

func (c *Client) sendDirect(ctx context.Context, s *session, to wabin.JID, id string, msg *wae2e.Message) (*SendResult, error) {
	var peerPN wabin.JID
	if to.Server == wabin.ServerUser {
		lid, err := c.resolveLID(ctx, s, to)
		if err != nil {
			return nil, fmt.Errorf("resolve LID: %w", err)
		}
		peerPN, to = to, lid
	}
	if s.dev.LID.IsEmpty() {
		return nil, fmt.Errorf("%w: own LID unknown", ErrUnsupportedChat)
	}
	ownUser, ownDevice := s.dev.LID.ToNonAD(), s.dev.LID

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
	participants, includeIdentity, _, err := c.encryptForDevices(ctx, s, devices, ownDevice, ownUser, plaintext, dsm, withMediaType(wabin.Attrs{}, msg))
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
	if t := c.privacyToken(ctx, to); t != nil {
		content = append(content, wabin.Node{Tag: "tctoken", Content: t})
	}
	res, err := s.sendMessageNode(ctx, wabin.Node{Tag: "message", Attrs: attrs, Content: content}, id, to)
	if err != nil {
		return res.result(), err
	}
	// Writing to someone gives them our privacy token (once a week).
	s.goSafe("issue-privacy-token", func() { c.issueToken(s.ctx, s, to) })
	c.storeLiveMessage(ctx, to, ownDevice, id, true, res.Timestamp, "", msg)
	if res.phash != "" && res.phash != participantListHash(devices) {
		s.log.Warn("server device list differs from ours; refreshing", "chat", wabin.MaskJID(to))
		c.devCache.invalidate(to.ToNonAD())
		c.devCache.invalidate(ownUser.ToNonAD())
	}
	return &res.SendResult, nil
}

// sendGroup encrypts the message once with our sender key ("skmsg") and
// distributes the sender key over 1:1 sessions to the devices that do not
// have it yet.
func (c *Client) sendGroup(ctx context.Context, s *session, group wabin.JID, id string, msg *wae2e.Message) (*SendResult, error) {
	gi, err := c.groupInfo(ctx, s, group)
	if err != nil {
		return nil, err
	}
	ownDevice := s.ownDeviceForGroups()
	ownUser := ownDevice.ToNonAD()
	if gi.AddressingMode != "lid" {
		ownUser = s.dev.JID.ToNonAD()
	}
	users := []wabin.JID{ownUser}
	for _, p := range gi.Participants {
		if !slices.Contains([]string{ownUser.User, ownDevice.User, s.dev.JID.User}, p.JID.User) {
			users = append(users, p.JID.ToNonAD())
		}
	}
	plaintext, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	if err := c.cfg.Store.PutSentMessage(ctx, c.cfg.InstanceID, group.String(), id, plaintext); err != nil {
		return nil, err
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	devices, err := c.devices(ctx, s, users)
	if err != nil {
		return nil, fmt.Errorf("device list: %w", err)
	}
	skdm, skmsg, err := c.senderKeyEncrypt(ctx, group, ownDevice, plaintext)
	if err != nil {
		return nil, fmt.Errorf("sender key: %w", err)
	}
	need, needAddrs, err := c.senderKeyTargets(ctx, group, c.filterTargets(devices, ownDevice))
	if err != nil {
		return nil, err
	}
	skdmMsg, err := proto.Marshal(&wae2e.Message{SenderKeyDistributionMessage: &wae2e.SenderKeyDistributionMessage{
		GroupID: proto.String(group.String()), AxolotlSenderKeyDistributionMessage: skdm,
	}})
	if err != nil {
		return nil, err
	}
	participants, includeIdentity, delivered, err := c.encryptForDevices(ctx, s, need, ownDevice, wabin.JID{}, skdmMsg, nil, nil)
	if err != nil {
		return nil, err
	}

	attrs := wabin.Attrs{"id": id, "type": messageType(msg), "to": group, "phash": participantListHash(devices)}
	if gi.AddressingMode == "lid" {
		attrs["addressing_mode"] = "lid"
	}
	skAttrs := withMediaType(wabin.Attrs{"v": "2", "type": "skmsg"}, msg)
	var content []wabin.Node
	if len(participants) > 0 {
		content = append(content, wabin.Node{Tag: "participants", Content: participants})
	}
	content = append(content, wabin.Node{Tag: "enc", Attrs: skAttrs, Content: skmsg})
	if includeIdentity {
		content = append(content, wabin.Node{Tag: "device-identity", Content: s.dev.Account})
	}
	res, err := s.sendMessageNode(ctx, wabin.Node{Tag: "message", Attrs: attrs, Content: content}, id, group)
	if err != nil {
		return res.result(), err
	}
	c.storeLiveMessage(ctx, group, ownDevice, id, true, res.Timestamp, "", msg)
	// Only now do these devices verifiably have our key.
	var deliveredAddrs []string
	for i, d := range need {
		if delivered[d] {
			deliveredAddrs = append(deliveredAddrs, needAddrs[i])
		}
	}
	if err := c.cfg.Store.MarkSenderKeySent(ctx, c.cfg.InstanceID, group.String(), deliveredAddrs); err != nil {
		s.log.Warn("could not record sender key delivery", "error", err)
	}
	if res.phash != "" && res.phash != attrs["phash"] {
		s.log.Warn("server group device list differs from ours; refreshing", "group", wabin.MaskJID(group))
		c.groups.invalidate(group)
		for _, u := range users {
			c.devCache.invalidate(u.ToNonAD())
		}
	}
	return &res.SendResult, nil
}

// senderKeyEncrypt encrypts plaintext with our sender key for group,
// creating the key on first use. It also returns the key's distribution
// message for devices that do not have it yet.
func (c *Client) senderKeyEncrypt(ctx context.Context, group, ownDevice wabin.JID, plaintext []byte) (skdm, skmsg []byte, err error) {
	inst, gid, addr := c.cfg.InstanceID, group.String(), signalAddress(ownDevice)
	unlock := c.locks.lock("sk|" + gid + "|" + addr)
	defer unlock()
	err = c.cfg.Store.InTx(ctx, func(tx *store.Store) error {
		rec, err := tx.LoadSenderKey(ctx, inst, gid, addr)
		if err != nil {
			return err
		}
		created := rec.IsEmpty()
		if skdm, err = rec.CreateDistribution(); err != nil {
			return err
		}
		if skmsg, err = rec.EncryptGroup(padMessage(slices.Clone(plaintext))); err != nil {
			return err
		}
		// A new key must be distributed to everyone again.
		if created {
			if err := tx.ForgetSenderKeyRecipients(ctx, inst, gid, ""); err != nil {
				return err
			}
		}
		return tx.PutSenderKey(ctx, inst, gid, addr, rec)
	})
	return skdm, skmsg, err
}

// senderKeyDistribution returns a distribution message for our current
// sender key in group.
func (c *Client) senderKeyDistribution(ctx context.Context, group, ownDevice wabin.JID) (skdm []byte, err error) {
	inst, gid, addr := c.cfg.InstanceID, group.String(), signalAddress(ownDevice)
	unlock := c.locks.lock("sk|" + gid + "|" + addr)
	defer unlock()
	err = c.cfg.Store.InTx(ctx, func(tx *store.Store) error {
		rec, err := tx.LoadSenderKey(ctx, inst, gid, addr)
		if err != nil {
			return err
		}
		if skdm, err = rec.CreateDistribution(); err != nil {
			return err
		}
		return tx.PutSenderKey(ctx, inst, gid, addr, rec)
	})
	return skdm, err
}

// senderKeyTargets returns the devices (and their signal addresses) that
// do not have our sender key for group yet.
func (c *Client) senderKeyTargets(ctx context.Context, group wabin.JID, targets []wabin.JID) ([]wabin.JID, []string, error) {
	encJIDs := c.encryptionJIDs(ctx, targets)
	addrs := make([]string, len(targets))
	for i, d := range targets {
		addrs[i] = signalAddress(encJIDs[d])
	}
	have, err := c.cfg.Store.SenderKeyRecipients(ctx, c.cfg.InstanceID, group.String(), addrs)
	if err != nil {
		return nil, nil, err
	}
	var need []wabin.JID
	var needAddrs []string
	for i, d := range targets {
		if !have[addrs[i]] {
			need = append(need, d)
			needAddrs = append(needAddrs, addrs[i])
		}
	}
	return need, needAddrs, nil
}

// withMediaType sets the "mediatype" attribute for media messages.
func withMediaType(attrs wabin.Attrs, msg *wae2e.Message) wabin.Attrs {
	if mt := mediaType(msg); mt != "" {
		attrs["mediatype"] = mt
	}
	return attrs
}

type sentNode struct {
	SendResult
	phash string
}

func (r *sentNode) result() *SendResult {
	if r == nil {
		return nil
	}
	return &r.SendResult
}

// sendMessageNode sends a <message> and waits for its server ack.
func (s *session) sendMessageNode(ctx context.Context, n wabin.Node, id string, chat wabin.JID) (*sentNode, error) {
	ack, err := s.sendAndWait(ctx, n, id, sendAckTimeout)
	if err != nil {
		return nil, fmt.Errorf("send message %s: %w", id, err)
	}
	res := &sentNode{SendResult: SendResult{ID: id, Chat: chat}}
	if t, ok := ack.AttrInt("t"); ok {
		res.Timestamp = time.Unix(t, 0)
	}
	res.phash, _ = ack.AttrString("phash")
	if code, ok := ack.AttrString("error"); ok && code != "" && code != "0" {
		return res, fmt.Errorf("%w: error %s", ErrServerRejected, code)
	}
	return res, nil
}

// messageType is the stanza "type" attribute.
func messageType(m *wae2e.Message) string {
	m = unwrapMessage(m)
	switch {
	case m.GetReactionMessage() != nil:
		return "reaction"
	case mediaType(m) != "":
		return "media"
	}
	return "text"
}

// mediaType is the "mediatype" attribute for media messages.
func mediaType(m *wae2e.Message) string {
	m = unwrapMessage(m)
	switch {
	case m.GetImageMessage() != nil:
		return "image"
	case m.GetStickerMessage() != nil:
		return "sticker"
	case m.GetDocumentMessage() != nil:
		return "document"
	case m.GetAudioMessage() != nil:
		if m.GetAudioMessage().GetPTT() {
			return "ptt"
		}
		return "audio"
	case m.GetVideoMessage() != nil:
		if m.GetVideoMessage().GetGifPlayback() {
			return "gif"
		}
		return "video"
	}
	return ""
}

// filterTargets drops our own device and business-hosted devices (which
// use a different protocol).
func (c *Client) filterTargets(devices []wabin.JID, ownDevice wabin.JID) []wabin.JID {
	var out []wabin.JID
	for _, d := range devices {
		if d.User == ownDevice.User && d.Device == ownDevice.Device && d.Server == ownDevice.Server {
			continue
		}
		if d.Server == wabin.ServerHosted || d.Server == wabin.ServerHostedLID {
			continue
		}
		out = append(out, d)
	}
	return out
}

// encryptionJIDs maps each device to the identity whose Signal session is
// used: the LID device when the phone number's LID is known.
func (c *Client) encryptionJIDs(ctx context.Context, devices []wabin.JID) map[wabin.JID]wabin.JID {
	out := make(map[wabin.JID]wabin.JID, len(devices))
	lids := map[string]string{}
	for _, d := range devices {
		e := d
		if d.Server == wabin.ServerUser {
			lid, ok := lids[d.User]
			if !ok {
				lid, _ = c.cfg.Store.LIDForPN(ctx, c.cfg.InstanceID, d.User)
				lids[d.User] = lid
			}
			if lid != "" {
				e = wabin.JID{User: lid, Device: d.Device, Server: wabin.ServerLID}
			}
		}
		out[d] = e
	}
	return out
}

// encryptForDevices encrypts for each device, fetching prekey bundles for
// devices without a session first. A device that fails is skipped (logged),
// mirroring WhatsApp clients: one broken device must not block delivery to
// the others. dsm, if set, is used instead of plaintext for ownUser's
// devices. It returns the <to> nodes, whether any is a pkmsg, and the set of
// devices that got a payload.
func (c *Client) encryptForDevices(ctx context.Context, s *session, devices []wabin.JID, ownDevice, ownUser wabin.JID, plaintext, dsm []byte, encAttrs wabin.Attrs) ([]wabin.Node, bool, map[wabin.JID]bool, error) {
	targets := c.filterTargets(devices, ownDevice)
	encJIDs := c.encryptionJIDs(ctx, targets)
	addrs := make([]string, len(targets))
	for i, d := range targets {
		addrs[i] = signalAddress(encJIDs[d])
	}
	have, err := c.cfg.Store.HasSessions(ctx, c.cfg.InstanceID, addrs)
	if err != nil {
		return nil, false, nil, err
	}
	var need []wabin.JID
	for i, d := range targets {
		if !have[addrs[i]] {
			need = append(need, encJIDs[d])
		}
	}
	bundles, err := s.fetchBundles(ctx, need)
	if err != nil {
		return nil, false, nil, err
	}

	local := signal.LocalIdentity{Key: &s.dev.IdentityKey.KeyPair, RegistrationID: s.dev.RegistrationID}
	var nodes []wabin.Node
	includeIdentity := false
	delivered := make(map[wabin.JID]bool, len(targets))
	for _, d := range targets {
		pt := plaintext
		if dsm != nil && d.User == ownUser.User {
			pt = dsm
		}
		e := encJIDs[d]
		node, isPreKey, err := c.encryptForDevice(ctx, local, e, bundles[e], padMessage(append([]byte(nil), pt...)), encAttrs)
		if err != nil {
			s.log.Warn("skipping device", "device", wabin.MaskJID(d), "error", err)
			continue
		}
		includeIdentity = includeIdentity || isPreKey
		delivered[d] = true
		nodes = append(nodes, wabin.Node{Tag: "to", Attrs: wabin.Attrs{"jid": d}, Content: []wabin.Node{*node}})
	}
	return nodes, includeIdentity, delivered, nil
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
