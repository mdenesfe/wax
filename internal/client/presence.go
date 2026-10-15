package client

import (
	"context"
	"errors"
	"time"

	wabin "github.com/mdenesfe/wax/internal/binary"
)

// ErrNoPushName is returned when presence is sent without a known display name.
var ErrNoPushName = errors.New("client: push name unknown; set Config.PushName")

// SendPresence announces us as available (online) or unavailable. While
// available, delivery receipts are sent as plain "delivered" instead of
// "inactive".
func (c *Client) SendPresence(ctx context.Context, available bool) error {
	s, err := c.activeSession()
	if err != nil {
		return err
	}
	name := c.cfg.PushName
	if name == "" {
		name, _ = c.pushName.Load().(string) // learned from app state sync
	}
	if name == "" {
		return ErrNoPushName
	}
	typ := "unavailable"
	if available {
		typ = "available"
	}
	c.available.Store(available)
	return s.conn.SendNode(ctx, wabin.Node{Tag: "presence", Attrs: wabin.Attrs{"type": typ, "name": name}})
}

// SubscribePresence asks the server for a contact's presence updates.
func (c *Client) SubscribePresence(ctx context.Context, jid wabin.JID) error {
	s, err := c.activeSession()
	if err != nil {
		return err
	}
	return s.conn.SendNode(ctx, wabin.Node{Tag: "presence", Attrs: wabin.Attrs{"type": "subscribe", "to": jid.ToNonAD()}})
}

// SendChatPresence sends "composing" (typing, optionally media="audio" for
// recording) or "paused" to a chat.
func (c *Client) SendChatPresence(ctx context.Context, chat wabin.JID, composing bool, recordingAudio bool) error {
	s, err := c.activeSession()
	if err != nil {
		return err
	}
	child := wabin.Node{Tag: "paused"}
	if composing {
		child.Tag = "composing"
		if recordingAudio {
			child.Attrs = wabin.Attrs{"media": "audio"}
		}
	}
	return s.conn.SendNode(ctx, wabin.Node{Tag: "chatstate", Attrs: wabin.Attrs{"to": chat}, Content: []wabin.Node{child}})
}

func (s *session) handlePresence(n *wabin.Node) {
	from, _ := n.AttrJID("from")
	typ, _ := n.AttrString("type")
	p := Presence{From: from, Available: typ != "unavailable"}
	if last, ok := n.AttrInt("last"); ok && last > 0 {
		p.LastSeen = time.Unix(last, 0)
	}
	s.c.emit(p)
}

func (s *session) handleChatState(n *wabin.Node) {
	from, _ := n.AttrJID("from")
	cp := ChatPresence{Chat: from.ToNonAD(), Sender: from}
	if p, ok := n.AttrJID("participant"); ok {
		cp.Sender = p
	}
	for _, ch := range n.Children() {
		cp.State = ch.Tag
		cp.Media, _ = ch.AttrString("media")
	}
	s.c.emit(cp)
}
