package client

import (
	"context"
	"fmt"
	"time"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/wabin"
)

// Prekey pool sizes. Each one-time prekey is consumed by one peer device
// starting a session with us, so the pool is refilled when it runs low.
const (
	minPreKeys     = 5
	refillPreKeys  = 50
	initialPreKeys = 812 // first upload after pairing, as WhatsApp Web does
)

// ensurePreKeys tops up the server's prekey pool if it is low.
func (c *Client) ensurePreKeys(ctx context.Context, s *session) error {
	serverCount, err := s.serverPreKeyCount(ctx)
	if err != nil {
		return err
	}
	dbCount, err := c.cfg.Store.UploadedPreKeyCount(ctx, c.cfg.InstanceID)
	if err != nil {
		return err
	}
	s.log.Debug("prekey counts", "server", serverCount, "local", dbCount)
	if serverCount >= minPreKeys && dbCount >= minPreKeys {
		return nil
	}
	return c.uploadPreKeys(ctx, s, serverCount == 0 && dbCount == 0)
}

func (s *session) serverPreKeyCount(ctx context.Context) (int, error) {
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:     "iq",
		Attrs:   wabin.Attrs{"to": wabin.ServerJID, "type": "get", "xmlns": "encrypt"},
		Content: []wabin.Node{{Tag: "count"}},
	}, 0)
	if err != nil {
		return 0, fmt.Errorf("query prekey count: %w", err)
	}
	cnt, ok := resp.Child("count")
	if !ok {
		return 0, fmt.Errorf("query prekey count: no <count> in response")
	}
	v, ok := cnt.AttrInt("value")
	if !ok {
		return 0, fmt.Errorf("query prekey count: bad value")
	}
	return int(v), nil
}

// uploadPreKeys generates (or reuses not-yet-uploaded) prekeys, uploads them
// with our identity and signed prekey, and marks them uploaded only after
// the server confirmed. If anything fails, the same keys are retried later,
// so the server and the database never disagree about which keys exist.
func (c *Client) uploadPreKeys(ctx context.Context, s *session, initial bool) error {
	c.preKeyMu.Lock()
	defer c.preKeyMu.Unlock()
	count := refillPreKeys
	if initial {
		count = initialPreKeys
	}
	pks, err := c.cfg.Store.GetOrGenPreKeys(ctx, c.cfg.InstanceID, count)
	if err != nil {
		return err
	}
	dev := s.dev
	list := make([]wabin.Node, len(pks))
	ids := make([]uint32, len(pks))
	for i, pk := range pks {
		list[i] = preKeyNode("key", pk.ID, pk.Key.Pub, nil)
		ids[i] = pk.ID
	}
	sig := dev.SignedPreKey.Signature
	start := time.Now()
	_, err = s.sendIQ(ctx, wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"to": wabin.ServerJID, "type": "set", "xmlns": "encrypt"},
		Content: []wabin.Node{
			{Tag: "registration", Content: be32(dev.RegistrationID)},
			{Tag: "type", Content: []byte{keys.DJBType}},
			{Tag: "identity", Content: dev.IdentityKey.Pub[:]},
			{Tag: "list", Content: list},
			preKeyNode("skey", dev.SignedPreKey.ID, dev.SignedPreKey.Key.Pub, sig[:]),
		},
	}, 0)
	if err != nil {
		return fmt.Errorf("upload %d prekeys: %w", len(pks), err)
	}
	if err := c.cfg.Store.MarkPreKeysUploaded(ctx, c.cfg.InstanceID, ids); err != nil {
		return err
	}
	s.log.Info("uploaded prekeys", "count", len(pks), "first_id", ids[0], "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}

// preKeyNode encodes a prekey: ids are 3 bytes big-endian on the wire.
func preKeyNode(tag string, id uint32, pub [32]byte, signature []byte) wabin.Node {
	children := []wabin.Node{
		{Tag: "id", Content: keyID3(id)},
		{Tag: "value", Content: pub[:]},
	}
	if signature != nil {
		children = append(children, wabin.Node{Tag: "signature", Content: signature})
	}
	return wabin.Node{Tag: tag, Content: children}
}
