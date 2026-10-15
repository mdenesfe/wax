package client

import (
	"context"
	"time"

	"github.com/mdenesfe/wax/internal/wabin"
)

// handleCall reports call signalling. We are not a calling device: the
// stanza is acknowledged and surfaced as an event; offers can be rejected
// with RejectCall.
func (s *session) handleCall(n *wabin.Node) {
	defer s.ack(n)
	from, _ := n.AttrJID("from")
	ev := CallEvent{From: from}
	if t, ok := n.AttrInt("t"); ok {
		ev.Timestamp = time.Unix(t, 0)
	}
	ev.Platform, _ = n.AttrString("platform")
	children := n.Children()
	if len(children) == 0 {
		return
	}
	ch := children[0]
	ev.Type = ch.Tag
	ev.CallID, _ = ch.AttrString("call-id")
	ev.Creator, _ = ch.AttrJID("call-creator")
	ev.Group, _ = ch.AttrJID("group-jid")
	ev.Reason, _ = ch.AttrString("reason")
	for _, sub := range ch.Children() {
		if sub.Tag == "video" {
			ev.Video = true
		}
	}
	if m, ok := ch.AttrString("media"); ok && m == "video" {
		ev.Video = true
	}
	s.c.emit(ev)
}

// RejectCall declines an incoming call offer.
func (c *Client) RejectCall(ctx context.Context, from wabin.JID, callID string) error {
	s, err := c.activeSession()
	if err != nil {
		return err
	}
	own := s.dev.JID.ToNonAD()
	if from.Server == wabin.ServerLID && !s.dev.LID.IsEmpty() {
		own = s.dev.LID.ToNonAD()
	}
	return s.conn.SendNode(ctx, wabin.Node{
		Tag:   "call",
		Attrs: wabin.Attrs{"id": newMessageID(s.dev.JID), "from": own, "to": from.ToNonAD()},
		Content: []wabin.Node{{Tag: "reject", Attrs: wabin.Attrs{
			"call-id": callID, "call-creator": from.ToNonAD(), "count": "0",
		}}},
	})
}
