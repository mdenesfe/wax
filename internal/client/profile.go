package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdenesfe/wax/internal/wabin"
)

// UserProfile is what WhatsApp tells about a user.
type UserProfile struct {
	JID wabin.JID `json:"jid"`
	LID wabin.JID `json:"lid,omitzero"`
	// About is the profile's "about" text; AboutHidden is set when the
	// user's privacy settings hide it from us.
	About       string    `json:"about,omitempty"`
	AboutSetAt  time.Time `json:"about_set_at,omitzero"`
	AboutHidden bool      `json:"about_hidden"`
	Devices     int       `json:"devices"` // linked devices including the phone
	IsBusiness  bool      `json:"is_business"`
	Business    *Business `json:"business,omitempty"`
}

// Business is a business account's public profile. Fields holds the
// profile's text elements by name as the server sends them (for example
// "address", "email", "website", "description"); a name may repeat.
type Business struct {
	Categories []string            `json:"categories,omitempty"`
	Fields     map[string][]string `json:"fields,omitempty"`
	TimeZone   string              `json:"time_zone,omitempty"`
	Hours      []BusinessHours     `json:"hours,omitempty"`
}

// BusinessHours is one day's opening hours, as the server reports them.
type BusinessHours struct {
	Day   string `json:"day"`
	Mode  string `json:"mode"`
	Open  string `json:"open,omitempty"`
	Close string `json:"close,omitempty"`
}

const (
	profileTTL = 10 * time.Minute
	// profileHold is how long profile lookups pause after WhatsApp
	// answered "rate-overlimit"; it allows only a few in a row.
	profileHold = time.Minute
)

// ErrRateLimited means WhatsApp asked us to slow down; try again later.
var ErrRateLimited = errors.New("client: rate limited by WhatsApp")

// Profile returns a user's profile: about text, LID, devices and, for
// business accounts, the business profile. Results are cached briefly.
//
// The user part comes from a user sync query:
//
//	<usync mode="full" context="background">
//	  <query><business><verified_name/></business><status/><devices version="2"/><lid/></query>
//	  <list><user jid="…"/></list>
//	</usync>
//
// answered per user with <status t="…">text</status>, <devices>, <lid val="…"/>
// and, for businesses, <business><verified_name …/></business>.
func (c *Client) Profile(ctx context.Context, jid wabin.JID) (*UserProfile, error) {
	jid = jid.ToNonAD()
	if p, ok := c.profiles.get(jid); ok {
		return p, nil
	}
	s, err := c.activeSession()
	if err != nil {
		return nil, err
	}
	// One lookup at a time, and none while WhatsApp asks us to wait.
	select {
	case c.profileSlot <- struct{}{}:
		defer func() { <-c.profileSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if time.Now().UnixNano() < c.profileHoldUntil.Load() {
		return nil, ErrRateLimited
	}
	// The user's privacy token, if we have one, lets its "my contacts"
	// settings show us the about text.
	user := wabin.Node{Tag: "user", Attrs: wabin.Attrs{"jid": jid}, Content: c.tokenContent(ctx, jid)}
	list, err := s.usyncList(ctx, []wabin.Node{user}, "full", "background", []wabin.Node{
		{Tag: "business", Content: []wabin.Node{{Tag: "verified_name"}}},
		{Tag: "status"},
		{Tag: "devices", Attrs: wabin.Attrs{"version": "2"}},
		{Tag: "lid"},
	})
	if iqErr := (*IQError)(nil); errors.As(err, &iqErr) && iqErr.Code == "429" {
		c.profileHoldUntil.Store(time.Now().Add(profileHold).UnixNano())
		return nil, ErrRateLimited
	}
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	p := &UserProfile{JID: jid}
	// One user was asked for; the answer may name it by its other address.
	if users := list.ChildrenByTag("user"); len(users) > 0 {
		readUser(p, &users[0])
	}
	if jid.Server == wabin.ServerLID {
		p.LID = jid
	} else if !p.LID.IsEmpty() {
		c.learnLID(ctx, jid, p.LID)
	}
	if p.IsBusiness {
		if p.Business, err = c.businessProfile(ctx, s, jid); err != nil {
			c.log.Debug("business profile unavailable", "error", err)
		}
	}
	c.profiles.put(jid, p)
	return p, nil
}

// readUser reads one <user> of a profile query.
func readUser(p *UserProfile, u *wabin.Node) {
	if st, ok := u.Child("status"); ok {
		if code, _ := st.AttrString("code"); code == "401" || code == "403" {
			p.AboutHidden = true
		}
		if raw, ok := st.Bytes(); ok {
			p.About = string(raw)
		}
		if t, ok := st.AttrInt("t"); ok && t > 0 {
			p.AboutSetAt = time.Unix(t, 0)
		}
	}
	if dn, ok := u.Child("devices"); ok {
		p.Devices = len(parseDeviceList(p.JID, dn))
	}
	if ln, ok := u.Child("lid"); ok {
		if lid, ok := ln.AttrJID("val"); ok && lid.Server == wabin.ServerLID {
			p.LID = lid.ToNonAD()
		}
	}
	if bn, ok := u.Child("business"); ok {
		_, p.IsBusiness = bn.Child("verified_name")
	}
}

// businessProfile fetches a business account's profile:
//
//	<iq to="s.whatsapp.net" type="get" xmlns="w:biz">
//	  <business_profile v="244"><profile jid="…"/></business_profile>
//	</iq>
func (c *Client) businessProfile(ctx context.Context, s *session, jid wabin.JID) (*Business, error) {
	resp, err := s.sendIQ(ctx, wabin.Node{
		Tag:   "iq",
		Attrs: wabin.Attrs{"to": wabin.ServerJID, "type": "get", "xmlns": "w:biz"},
		Content: []wabin.Node{{Tag: "business_profile", Attrs: wabin.Attrs{"v": "244"},
			Content: []wabin.Node{{Tag: "profile", Attrs: wabin.Attrs{"jid": jid}}}}},
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("business profile: %w", err)
	}
	bp, _ := resp.Child("business_profile")
	pn, ok := bp.Child("profile")
	if !ok {
		return nil, errors.New("business profile: response without <profile>")
	}
	return readBusiness(pn), nil
}

// readBusiness reads a <profile>: categories, opening hours, and every
// other element with text content.
func readBusiness(pn *wabin.Node) *Business {
	b := &Business{Fields: map[string][]string{}}
	for _, ch := range pn.Children() {
		switch ch.Tag {
		case "categories":
			for _, cat := range ch.ChildrenByTag("category") {
				if raw, ok := cat.Bytes(); ok && len(raw) > 0 {
					b.Categories = append(b.Categories, string(raw))
				}
			}
		case "business_hours":
			b.TimeZone, _ = ch.AttrString("timezone")
			for _, h := range ch.ChildrenByTag("business_hours_config") {
				var d BusinessHours
				d.Day, _ = h.AttrString("day_of_week")
				d.Mode, _ = h.AttrString("mode")
				d.Open, _ = h.AttrString("open_time")
				d.Close, _ = h.AttrString("close_time")
				b.Hours = append(b.Hours, d)
			}
		default:
			if raw, ok := ch.Bytes(); ok && len(raw) > 0 {
				b.Fields[ch.Tag] = append(b.Fields[ch.Tag], string(raw))
			}
		}
	}
	return b
}

const joinedTTL = 2 * time.Minute

// CommonGroups lists the joined groups that user (by phone number or LID)
// is in. The joined group list is reused for a short while.
func (c *Client) CommonGroups(ctx context.Context, user ...wabin.JID) ([]*GroupInfo, error) {
	groups, ok := c.joined.get(struct{}{})
	if !ok {
		var err error
		if groups, err = c.JoinedGroups(ctx); err != nil {
			return nil, err
		}
	}
	match := func(j wabin.JID) bool {
		for _, u := range user {
			if !j.IsEmpty() && j.User == u.User && j.Server == u.Server {
				return true
			}
		}
		return false
	}
	var out []*GroupInfo
	for _, g := range groups {
		for _, p := range g.Participants {
			if match(p.JID.ToNonAD()) || match(p.LID.ToNonAD()) || match(p.PhoneNumber.ToNonAD()) {
				out = append(out, g)
				break
			}
		}
	}
	return out, nil
}
