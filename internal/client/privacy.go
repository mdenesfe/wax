package client

import (
	"context"
	"strconv"
	"time"

	"github.com/mdenesfe/wax/internal/wabin"
)

// Privacy ("trusted contact") tokens. A contact gives us a token when it
// writes to us; sending it back with profile and picture queries and with
// messages lets its "my contacts" privacy settings treat us as a contact.
// We issue ours to the users we write to, once per period.
//
// Tokens live in rolling weekly buckets: one is valid while it is from one
// of the last four weeks.
const (
	tokenBucket  = 7 * 24 * time.Hour
	tokenBuckets = 4
)

// tokenCutoff is the oldest time a valid token may carry.
func tokenCutoff(now time.Time) time.Time {
	bucket := now.Unix() / int64(tokenBucket.Seconds())
	return time.Unix((bucket-(tokenBuckets-1))*int64(tokenBucket.Seconds()), 0)
}

// tokenKeys are the addresses a user's token may be stored under: its LID
// (preferred) and its phone number.
func (c *Client) tokenKeys(ctx context.Context, user wabin.JID) []string {
	user = user.ToNonAD()
	keys := []string{user.String()}
	if user.Server == wabin.ServerUser {
		if lid, err := c.cfg.Store.LIDForPN(ctx, c.cfg.InstanceID, user.User); err == nil && lid != "" {
			keys = append([]string{wabin.JID{User: lid, Server: wabin.ServerLID}.String()}, keys...)
		}
	}
	return keys
}

// privacyToken returns a valid token of user, or nil.
func (c *Client) privacyToken(ctx context.Context, user wabin.JID) []byte {
	if user.Server != wabin.ServerUser && user.Server != wabin.ServerLID {
		return nil
	}
	t, err := c.cfg.Store.GetPrivacyToken(ctx, c.cfg.InstanceID, c.tokenKeys(ctx, user)...)
	if err != nil {
		c.log.Debug("privacy token lookup failed", "error", err)
		return nil
	}
	if t == nil || len(t.Token) == 0 || t.TokenAt.Before(tokenCutoff(time.Now())) {
		return nil
	}
	return t.Token
}

// tokenContent is element content carrying user's token as <tctoken>, or
// nil (no content at all, rather than an empty list) without one.
func (c *Client) tokenContent(ctx context.Context, user wabin.JID) any {
	if t := c.privacyToken(ctx, user); t != nil {
		return []wabin.Node{{Tag: "tctoken", Content: t}}
	}
	return nil
}

// onPrivacyToken stores the tokens of a notification:
//
//	<notification from="<user>" sender_lid="<lid>" type="privacy_token">
//	  <tokens><token type="trusted_contact" t="<unix>">bytes</token></tokens>
//	</notification>
func (s *session) onPrivacyToken(n *wabin.Node) {
	defer s.ack(n)
	from, _ := n.AttrJID("from")
	user, ok := n.AttrJID("sender_lid")
	if !ok || user.Server != wabin.ServerLID {
		user = from
	}
	if user.Server != wabin.ServerUser && user.Server != wabin.ServerLID {
		return
	}
	key := s.c.tokenKeys(s.ctx, user)[0]
	tokens, _ := n.Child("tokens")
	for _, t := range tokens.ChildrenByTag("token") {
		if typ, _ := t.AttrString("type"); typ != "trusted_contact" {
			continue
		}
		raw, ok := t.Bytes()
		ts, _ := t.AttrInt("t")
		if !ok || len(raw) == 0 || ts <= 0 {
			continue
		}
		if err := s.c.cfg.Store.PutPrivacyToken(s.ctx, s.c.cfg.InstanceID, key, raw, time.Unix(ts, 0)); err != nil {
			s.log.Warn("could not store privacy token", "error", err)
		}
	}
}

// issueToken gives user our token (once per bucket) after we wrote to it:
//
//	<iq to="s.whatsapp.net" type="set" xmlns="privacy">
//	  <tokens><token jid="<user>" t="<unix>" type="trusted_contact"/></tokens>
//	</iq>
func (c *Client) issueToken(ctx context.Context, s *session, user wabin.JID) {
	user = user.ToNonAD()
	if (user.Server != wabin.ServerUser && user.Server != wabin.ServerLID) || user.User == "0" { // "0": WhatsApp's own announcements
		return
	}
	keys := c.tokenKeys(ctx, user)
	now := time.Now()
	t, err := c.cfg.Store.GetPrivacyToken(ctx, c.cfg.InstanceID, keys...)
	if err != nil {
		return
	}
	if t != nil && !t.SentAt.IsZero() && t.SentAt.Unix()/int64(tokenBucket.Seconds()) == now.Unix()/int64(tokenBucket.Seconds()) {
		return // already issued in this bucket
	}
	_, err = s.sendIQ(ctx, wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"to": wabin.ServerJID, "type": "set", "xmlns": "privacy"},
		Content: []wabin.Node{{Tag: "tokens", Content: []wabin.Node{{Tag: "token",
			Attrs: wabin.Attrs{"jid": user, "t": strconv.FormatInt(now.Unix(), 10), "type": "trusted_contact"}}}}},
	}, 0)
	if err != nil {
		c.log.Debug("could not issue privacy token", "error", err)
		return
	}
	if err := c.cfg.Store.MarkPrivacyTokenSent(ctx, c.cfg.InstanceID, keys[0], now); err != nil {
		c.log.Debug("could not record privacy token issuance", "error", err)
	}
}

// pruneTokens drops expired tokens; run now and then.
func (c *Client) pruneTokens(ctx context.Context) {
	if n, err := c.cfg.Store.DeletePrivacyTokensBefore(ctx, c.cfg.InstanceID, tokenCutoff(time.Now())); err != nil {
		c.log.Debug("could not prune privacy tokens", "error", err)
	} else if n > 0 {
		c.log.Debug("expired privacy tokens removed", "count", n)
	}
}
