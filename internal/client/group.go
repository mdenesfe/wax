package client

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
)

// GroupParticipant is a member of a group.
type GroupParticipant struct {
	JID          wabin.JID `json:"jid"` // as addressed in the group (LID or phone number)
	LID          wabin.JID `json:"lid,omitzero"`
	PhoneNumber  wabin.JID `json:"phone_number,omitzero"`
	IsAdmin      bool      `json:"is_admin"`
	IsSuperAdmin bool      `json:"is_super_admin"`
}

// GroupInfo is group metadata.
type GroupInfo struct {
	JID            wabin.JID          `json:"jid"`
	Name           string             `json:"name"`
	Topic          string             `json:"topic,omitempty"`
	AddressingMode string             `json:"addressing_mode"` // "lid" or "pn"
	Created        time.Time          `json:"created"`
	Owner          wabin.JID          `json:"owner,omitzero"`
	Announce       bool               `json:"announce"` // only admins may send
	Locked         bool               `json:"locked"`   // only admins may edit info
	Participants   []GroupParticipant `json:"participants"`
}

// ErrGroupNotFound is returned for unknown groups or groups we are not in.
var ErrGroupNotFound = errors.New("client: group not found or not a member")

// groupCacheTTL bounds staleness when a w:gp2 notification is missed.
const groupCacheTTL = time.Hour

// groupCache maps a group JID to its metadata.
type groupCache = ttlCache[wabin.JID, *GroupInfo]

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
	c.groups.put(gi.JID, gi)
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
			gi.Participants = append(gi.Participants, parseParticipant(&ch))
		case "description":
			body, _ := ch.Child("body")
			if raw, ok := body.Bytes(); ok {
				gi.Topic = string(raw)
			}
		case "announcement":
			gi.Announce = true
		case "locked":
			gi.Locked = true
		}
	}
	return gi
}

// parseParticipant reads a <participant>; the alternate address (phone
// number or LID) comes from an attribute.
func parseParticipant(n *wabin.Node) GroupParticipant {
	p := GroupParticipant{}
	p.JID, _ = n.AttrJID("jid")
	typ, _ := n.AttrString("type")
	p.IsAdmin = typ == "admin" || typ == "superadmin"
	p.IsSuperAdmin = typ == "superadmin"
	switch p.JID.Server {
	case wabin.ServerLID:
		p.LID = p.JID
		p.PhoneNumber, _ = n.AttrJID("phone_number")
	case wabin.ServerUser:
		p.PhoneNumber = p.JID
		p.LID, _ = n.AttrJID("lid")
	}
	return p
}

// handleGroupNotification processes w:gp2 notifications (membership and
// metadata changes).
func (s *session) handleGroupNotification(n *wabin.Node) {
	group, _ := n.AttrJID("from")
	s.c.groups.invalidate(group)
	var changes []string
	for _, ch := range n.Children() {
		changes = append(changes, ch.Tag)
	}
	if slices.Contains(changes, "remove") || slices.Contains(changes, "leave") {
		if err := s.rotateSenderKey(group); err != nil {
			s.log.Warn("could not rotate sender key", "group", wabin.MaskJID(group), "error", err)
			return // unacked: retried on redelivery
		}
	}
	s.c.emit(GroupUpdate{JID: group, Changes: changes})
	s.ack(n)
}

// rotateSenderKey drops our sender key for group after a member left, so
// they cannot read future messages; the next send creates a new key and
// distributes it to everyone.
func (s *session) rotateSenderKey(group wabin.JID) error {
	inst, gid := s.c.cfg.InstanceID, group.String()
	return s.c.cfg.Store.InTx(s.ctx, func(tx *store.Store) error {
		if err := tx.DeleteSenderKey(s.ctx, inst, gid, signalAddress(s.ownDeviceForGroups())); err != nil {
			return err
		}
		return tx.ForgetSenderKeyRecipients(s.ctx, inst, gid, "")
	})
}

// ownDeviceForGroups is the device identity we use for sender keys: our LID
// device (WhatsApp addresses group senders by LID).
func (s *session) ownDeviceForGroups() wabin.JID {
	if !s.dev.LID.IsEmpty() {
		return s.dev.LID
	}
	return s.dev.JID
}
