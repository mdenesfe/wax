package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	wabin "github.com/mdenesfe/wax/internal/binary"
	"github.com/mdenesfe/wax/internal/store"
)

// GroupParticipant is a member of a group.
type GroupParticipant struct {
	JID          wabin.JID // as addressed in the group (LID or phone number)
	LID          wabin.JID
	PhoneNumber  wabin.JID
	IsAdmin      bool
	IsSuperAdmin bool
}

// GroupInfo is group metadata.
type GroupInfo struct {
	JID            wabin.JID
	Name           string
	Topic          string
	AddressingMode string // "lid" or "pn"
	Created        time.Time
	Owner          wabin.JID
	Announce       bool // only admins may send
	Locked         bool // only admins may edit info
	Participants   []GroupParticipant
}

// ErrGroupNotFound is returned for unknown groups or groups we are not in.
var ErrGroupNotFound = errors.New("client: group not found or not a member")

// groupCacheTTL bounds staleness when a w:gp2 notification is missed.
const groupCacheTTL = time.Hour

type groupCache struct {
	mu      sync.Mutex
	entries map[wabin.JID]groupEntry
}

type groupEntry struct {
	info    *GroupInfo
	fetched time.Time
}

func (g *groupCache) get(j wabin.JID) (*GroupInfo, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[j]
	if !ok || time.Since(e.fetched) > groupCacheTTL {
		return nil, false
	}
	return e.info, true
}

func (g *groupCache) put(info *GroupInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entries == nil {
		g.entries = make(map[wabin.JID]groupEntry)
	}
	g.entries[info.JID] = groupEntry{info: info, fetched: time.Now()}
}

func (g *groupCache) invalidate(j wabin.JID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, j)
}

// GetGroupInfo fetches group metadata from the server (and caches it).
func (c *Client) GetGroupInfo(ctx context.Context, group wabin.JID) (*GroupInfo, error) {
	s, err := c.activeSession()
	if err != nil {
		return nil, err
	}
	return c.fetchGroupInfo(ctx, s, group)
}

func (c *Client) groupInfo(ctx context.Context, s *session, group wabin.JID) (*GroupInfo, error) {
	if gi, ok := c.groups.get(group); ok {
		return gi, nil
	}
	return c.fetchGroupInfo(ctx, s, group)
}

func (c *Client) fetchGroupInfo(ctx context.Context, s *session, group wabin.JID) (*GroupInfo, error) {
	if group.Server != wabin.ServerGroup {
		return nil, fmt.Errorf("%w: %s is not a group", ErrUnsupportedChat, wabin.MaskJID(group))
	}
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:     "iq",
		Attrs:   wabin.Attrs{"to": group, "type": "get", "xmlns": "w:g2"},
		Content: []wabin.Node{{Tag: "query", Attrs: wabin.Attrs{"request": "interactive"}}},
	}, 0)
	var iqErr *IQError
	if errors.As(err, &iqErr) && (iqErr.Code == "404" || iqErr.Code == "403" || iqErr.Code == "401") {
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, iqErr.Code)
	}
	if err != nil {
		return nil, fmt.Errorf("group info: %w", err)
	}
	gn, ok := resp.Child("group")
	if !ok {
		return nil, errors.New("group info: response without <group>")
	}
	gi := parseGroup(gn)
	for _, p := range gi.Participants {
		if !p.LID.IsEmpty() && !p.PhoneNumber.IsEmpty() {
			c.learnLID(ctx, p.PhoneNumber, p.LID)
		}
	}
	c.groups.put(gi)
	return gi, nil
}

func parseGroup(n *wabin.Node) *GroupInfo {
	gi := &GroupInfo{}
	id, _ := n.AttrString("id")
	gi.JID = wabin.JID{User: id, Server: wabin.ServerGroup}
	gi.Name, _ = n.AttrString("subject")
	gi.AddressingMode, _ = n.AttrString("addressing_mode")
	gi.Owner, _ = n.AttrJID("creator")
	if t, ok := n.AttrInt("creation"); ok {
		gi.Created = time.Unix(t, 0)
	}
	for _, ch := range n.Children() {
		switch ch.Tag {
		case "participant":
			p := GroupParticipant{}
			p.JID, _ = ch.AttrJID("jid")
			typ, _ := ch.AttrString("type")
			p.IsAdmin = typ == "admin" || typ == "superadmin"
			p.IsSuperAdmin = typ == "superadmin"
			switch p.JID.Server {
			case wabin.ServerLID:
				p.LID = p.JID
				p.PhoneNumber, _ = ch.AttrJID("phone_number")
			case wabin.ServerUser:
				p.PhoneNumber = p.JID
				p.LID, _ = ch.AttrJID("lid")
			}
			gi.Participants = append(gi.Participants, p)
		case "description":
			if b, ok := ch.Child("body"); ok {
				if raw, ok := b.Bytes(); ok {
					gi.Topic = string(raw)
				}
			}
		case "announcement":
			gi.Announce = true
		case "locked":
			gi.Locked = true
		}
	}
	return gi
}

// handleGroupNotification processes w:gp2 notifications (membership and
// metadata changes).
func (s *session) handleGroupNotification(n *wabin.Node) {
	group, _ := n.AttrJID("from")
	s.c.groups.invalidate(group)
	var changes []string
	rotate := false
	for _, ch := range n.Children() {
		changes = append(changes, ch.Tag)
		if ch.Tag == "remove" || ch.Tag == "leave" {
			rotate = true
		}
	}
	if rotate {
		// A member left: start a new sender key so they cannot read
		// future messages, and redistribute it to everyone.
		own := s.ownDeviceForGroups()
		if err := s.c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
			if err := tx.DeleteSenderKey(s.ctx, s.c.cfg.InstanceID, group.String(), signalAddress(own)); err != nil {
				return err
			}
			return tx.ForgetSenderKeyRecipients(s.ctx, s.c.cfg.InstanceID, group.String(), "")
		}); err != nil {
			s.log.Warn("could not rotate sender key", "group", wabin.MaskJID(group), "error", err)
			return // unacked: retried on redelivery
		}
	}
	s.c.emit(GroupUpdate{JID: group, Changes: changes})
	s.ack(n)
}

// ownDeviceForGroups is the device identity we use for sender keys: our LID
// device (WhatsApp addresses group senders by LID).
func (s *session) ownDeviceForGroups() wabin.JID {
	if !s.dev.LID.IsEmpty() {
		return s.dev.LID
	}
	return s.dev.JID
}
