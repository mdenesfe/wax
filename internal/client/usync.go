package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/mdenesfe/wax/internal/signal"
	"github.com/mdenesfe/wax/internal/wabin"
)

// deviceCacheTTL bounds how long a device list is trusted without a
// "devices" notification. The server's phash check catches staleness too.
const deviceCacheTTL = 10 * time.Minute

type deviceCache struct {
	mu      sync.Mutex
	entries map[wabin.JID]deviceEntry
}

type deviceEntry struct {
	devices []wabin.JID
	fetched time.Time
}

func (d *deviceCache) get(user wabin.JID) ([]wabin.JID, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[user]
	if !ok || time.Since(e.fetched) > deviceCacheTTL {
		return nil, false
	}
	return e.devices, true
}

func (d *deviceCache) put(user wabin.JID, devices []wabin.JID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = make(map[wabin.JID]deviceEntry)
	}
	d.entries[user] = deviceEntry{devices: devices, fetched: time.Now()}
}

func (d *deviceCache) invalidate(user wabin.JID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, user.ToNonAD())
}

// usync runs a user sync query and returns the <list> of <user> results.
func (s *session) usync(ctx context.Context, users []wabin.JID, mode, purpose string, query []wabin.Node) (*wabin.Node, error) {
	list := make([]wabin.Node, len(users))
	for i, u := range users {
		list[i] = wabin.Node{Tag: "user", Attrs: wabin.Attrs{"jid": u.ToNonAD()}}
	}
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"to": wabin.ServerJID, "type": "get", "xmlns": "usync"},
		Content: []wabin.Node{{
			Tag:   "usync",
			Attrs: wabin.Attrs{"sid": s.conn.NextID(), "mode": mode, "last": "true", "index": "0", "context": purpose},
			Content: []wabin.Node{
				{Tag: "query", Content: query},
				{Tag: "list", Content: list},
			},
		}},
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("usync: %w", err)
	}
	us, ok := resp.Child("usync")
	if !ok {
		return nil, fmt.Errorf("usync: response without <usync>")
	}
	l, ok := us.Child("list")
	if !ok {
		return nil, fmt.Errorf("usync: response without <list>")
	}
	return l, nil
}

func parseDeviceList(user wabin.JID, devicesNode *wabin.Node) []wabin.JID {
	dl, ok := devicesNode.Child("device-list")
	if !ok {
		return nil
	}
	var out []wabin.JID
	for _, d := range dl.ChildrenByTag("device") {
		id, ok := d.AttrInt("id")
		if !ok || id < 0 || id > 0xffff {
			continue
		}
		j := user.ToNonAD()
		j.Device = uint16(id)
		if h, _ := d.AttrString("is_hosted"); h == "true" {
			if j.Server == wabin.ServerLID {
				j.Server = wabin.ServerHostedLID
			} else {
				j.Server = wabin.ServerHosted
			}
		}
		out = append(out, j)
	}
	return out
}

// devices returns all devices of the given users (non-AD JIDs), using the
// cache where possible.
func (c *Client) devices(ctx context.Context, s *session, users []wabin.JID) ([]wabin.JID, error) {
	var out, missing []wabin.JID
	for _, u := range users {
		if d, ok := c.devCache.get(u.ToNonAD()); ok {
			out = append(out, d...)
		} else {
			missing = append(missing, u.ToNonAD())
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	list, err := s.usync(ctx, missing, "query", "message", []wabin.Node{{Tag: "devices", Attrs: wabin.Attrs{"version": "2"}}})
	if err != nil {
		return nil, err
	}
	for _, u := range list.ChildrenByTag("user") {
		jid, ok := u.AttrJID("jid")
		if !ok {
			continue
		}
		dn, ok := u.Child("devices")
		if !ok {
			continue
		}
		devs := parseDeviceList(jid, dn)
		c.devCache.put(jid.ToNonAD(), devs)
		out = append(out, devs...)
	}
	return out, nil
}

// resolveLID maps a phone-number JID to its LID, asking the server if the
// mapping is not stored yet.
func (c *Client) resolveLID(ctx context.Context, s *session, pn wabin.JID) (wabin.JID, error) {
	lidUser, err := c.cfg.Store.LIDForPN(ctx, c.cfg.InstanceID, pn.User)
	if err != nil {
		return wabin.JID{}, err
	}
	if lidUser != "" {
		return wabin.JID{User: lidUser, Server: wabin.ServerLID}, nil
	}
	list, err := s.usync(ctx, []wabin.JID{pn}, "full", "background", []wabin.Node{
		{Tag: "devices", Attrs: wabin.Attrs{"version": "2"}},
		{Tag: "lid"},
	})
	if err != nil {
		return wabin.JID{}, err
	}
	for _, u := range list.ChildrenByTag("user") {
		jid, _ := u.AttrJID("jid")
		if jid.User != pn.User {
			continue
		}
		ln, ok := u.Child("lid")
		if !ok {
			continue
		}
		lid, ok := ln.AttrJID("val")
		if !ok || lid.Server != wabin.ServerLID {
			continue
		}
		c.learnLID(ctx, pn, lid)
		if dn, ok := u.Child("devices"); ok {
			c.devCache.put(jid.ToNonAD(), parseDeviceList(jid, dn))
		}
		return lid.ToNonAD(), nil
	}
	return wabin.JID{}, fmt.Errorf("%w: server returned no LID for %s", ErrNotOnWhatsApp, wabin.MaskJID(pn))
}

// learnLID stores a PN<->LID mapping and moves existing phone-number
// sessions to the LID address (WhatsApp encrypts to LID identities).
func (c *Client) learnLID(ctx context.Context, pn, lid wabin.JID) {
	if pn.Server != wabin.ServerUser || lid.Server != wabin.ServerLID || pn.User == "" || lid.User == "" {
		return
	}
	changed, err := c.cfg.Store.PutLIDMapping(ctx, c.cfg.InstanceID, pn.User, lid.User)
	if err != nil {
		c.log.Warn("failed to store LID mapping", "error", err)
		return
	}
	if changed {
		from, to := signalAddressName(pn.ToNonAD()), signalAddressName(lid.ToNonAD())
		if err := c.cfg.Store.MigrateSessions(ctx, c.cfg.InstanceID, from, to); err != nil {
			c.log.Warn("failed to migrate sessions to LID", "error", err)
		}
	}
}

// fetchBundles requests prekey bundles for devices without a session.
func (s *session) fetchBundles(ctx context.Context, devices []wabin.JID) (map[wabin.JID]*signal.PreKeyBundle, error) {
	if len(devices) == 0 {
		return nil, nil
	}
	users := make([]wabin.Node, len(devices))
	for i, d := range devices {
		users[i] = wabin.Node{Tag: "user", Attrs: wabin.Attrs{"jid": d, "reason": "identity"}}
	}
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:     "iq",
		Attrs:   wabin.Attrs{"to": wabin.ServerJID, "type": "get", "xmlns": "encrypt"},
		Content: []wabin.Node{{Tag: "key", Content: users}},
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("fetch prekeys: %w", err)
	}
	list, ok := resp.Child("list")
	if !ok {
		return nil, fmt.Errorf("fetch prekeys: response without <list>")
	}
	out := make(map[wabin.JID]*signal.PreKeyBundle)
	for _, u := range list.ChildrenByTag("user") {
		jid, ok := u.AttrJID("jid")
		if !ok {
			continue
		}
		b, err := parseBundle(&u)
		if err != nil {
			s.log.Warn("bad prekey bundle", "jid", wabin.MaskJID(jid), "error", err)
			continue
		}
		out[jid] = b
	}
	return out, nil
}

func childBytes(n *wabin.Node, tag string, size int) ([]byte, error) {
	c, ok := n.Child(tag)
	if !ok {
		return nil, fmt.Errorf("missing <%s>", tag)
	}
	b, ok := c.Bytes()
	if !ok || (size > 0 && len(b) != size) {
		return nil, fmt.Errorf("<%s> has %d bytes, want %d", tag, len(b), size)
	}
	return b, nil
}

// be32 encodes a registration id: 4 bytes big-endian.
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// keyID3 encodes a prekey id: 3 bytes big-endian on the wire.
func keyID3(id uint32) []byte { return be32(id)[1:] }

// keyID decodes a big-endian prekey id of up to 4 bytes.
func keyID(b []byte) uint32 {
	var buf [4]byte
	copy(buf[4-len(b):], b)
	return binary.BigEndian.Uint32(buf[:])
}

// parseBundle reads a prekey bundle from a <user> (prekey fetch) or a retry
// receipt; key material may be wrapped in <keys>.
func parseBundle(n *wabin.Node) (*signal.PreKeyBundle, error) {
	if e, ok := n.Child("error"); ok {
		return nil, fmt.Errorf("server error: %s", e.String())
	}
	reg, err := childBytes(n, "registration", 4)
	if err != nil {
		return nil, err
	}
	kn := n
	if k, ok := n.Child("keys"); ok {
		kn = k
	}
	ident, err := childBytes(kn, "identity", 32)
	if err != nil {
		return nil, err
	}
	b := &signal.PreKeyBundle{RegistrationID: binary.BigEndian.Uint32(reg), IdentityKey: [32]byte(ident)}
	skey, ok := kn.Child("skey")
	if !ok {
		return nil, fmt.Errorf("missing <skey>")
	}
	id, err := childBytes(skey, "id", 3)
	if err != nil {
		return nil, err
	}
	val, err := childBytes(skey, "value", 32)
	if err != nil {
		return nil, err
	}
	sig, err := childBytes(skey, "signature", 64)
	if err != nil {
		return nil, err
	}
	b.SignedPreKeyID, b.SignedPreKey, b.SignedPreKeySignature = keyID(id), [32]byte(val), [64]byte(sig)
	if key, ok := kn.Child("key"); ok {
		id, err := childBytes(key, "id", 3)
		if err != nil {
			return nil, err
		}
		val, err := childBytes(key, "value", 32)
		if err != nil {
			return nil, err
		}
		pid := keyID(id)
		b.PreKeyID, b.PreKey = &pid, [32]byte(val)
	}
	return b, nil
}
