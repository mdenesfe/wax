package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mdenesfe/wax/internal/manager"
)

// The demo data: made-up people, groups, a community, chats and status
// updates. Numbers are in the fictional 90555 range used by the tests.

const (
	me    = "905550000001@s.whatsapp.net"
	meLID = "100000000001@lid"
)

type person struct {
	key, name, push, username, about string
	business                         bool
	noPicture                        bool
}

func (p person) phone() string { return "9055500000" + p.key }
func (p person) lid() string   { return "2000000000" + p.key + "@lid" }

var people = []person{
	{key: "10", name: "Emma Wilson", push: "Emma", about: "Building things that last."},
	{key: "11", name: "Lucas Martin", push: "Lucas", about: "Ship it."},
	{key: "12", name: "Sofia Rossi", push: "Sofia", username: "sofia.r", about: "Coffee, colors and code"},
	{key: "13", name: "Noah Becker", push: "Noah"},
	{key: "14", name: "Mia Johnson", push: "Mia", about: "On a train somewhere"},
	{key: "15", name: "Leo Garcia", push: "Leo", noPicture: true},
	{key: "16", name: "Zeynep Kaya", push: "Zeynep", about: "Istanbul"},
	{key: "17", push: "Daniel"}, // not in the address book
	{key: "18", name: "Acme Bakery", push: "Acme Bakery", business: true, about: "Fresh bread every morning"},
	{key: "19", name: "Olivia Brown", push: "Olivia"},
	{key: "20", name: "Ravi Patel", push: "Ravi", noPicture: true},
}

func who(key string) person {
	for _, p := range people {
		if p.key == key {
			return p
		}
	}
	panic("unknown person " + key)
}

type group struct {
	jid, name, topic string
	community        string // parent community JID
	isCommunity      bool
	announcements    bool // the community's announcement group
	members          []string
	admins           []string
	announce         bool
}

var groups = []group{
	{jid: "120363000000000100@g.us", name: "Acme Community", isCommunity: true, topic: "Everyone at Acme",
		members: []string{"10", "11", "12", "13", "14", "16"}, admins: []string{"11"}},
	{jid: "120363000000000104@g.us", name: "Acme Community", community: "120363000000000100@g.us", announcements: true, announce: true,
		members: []string{"10", "11", "12", "13", "14", "16"}, admins: []string{"11"}},
	{jid: "120363000000000101@g.us", name: "Product Team", community: "120363000000000100@g.us", topic: "Roadmap, releases and the occasional meme.",
		members: []string{"10", "11", "13", "14"}, admins: []string{"11", "10"}},
	{jid: "120363000000000105@g.us", name: "Customer Success", community: "120363000000000100@g.us",
		members: []string{"12", "16", "14"}, admins: []string{"12"}},
	{jid: "120363000000000102@g.us", name: "Weekend Hike", topic: "Saturday 08:00 at the north gate",
		members: []string{"12", "15", "16", "19"}, admins: []string{"15"}},
	{jid: "120363000000000103@g.us", name: "Design Review", members: []string{"10", "12", "13"}, admins: []string{"12"}},
	{jid: "120363000000000106@g.us", name: "Old Project", members: []string{"19", "20"}, admins: []string{"20"}},
}

func groupByJID(jid string) *group {
	for i := range groups {
		if groups[i].jid == jid {
			return &groups[i]
		}
	}
	return nil
}

// chatJSON matches the API's chat list entries.
type chatJSON struct {
	JID           string               `json:"jid"`
	Name          string               `json:"name,omitempty"`
	LastMessageAt *time.Time           `json:"last_message_at,omitempty"`
	UnreadCount   int                  `json:"unread_count"`
	Archived      bool                 `json:"archived"`
	Pinned        bool                 `json:"pinned"`
	MutedUntil    *time.Time           `json:"muted_until,omitempty"`
	Phone         string               `json:"phone,omitempty"`
	PushName      string               `json:"push_name,omitempty"`
	Username      string               `json:"username,omitempty"`
	LastMessage   *manager.MessageJSON `json:"last_message,omitempty"`
}

// asset is a downloadable file behind a media message.
type asset struct {
	contentType string
	data        []byte
}

// account is one demo WhatsApp account and everything in it.
type account struct {
	id, status, jid, lid, webhook string
	qr                            bool
	chats                         []*chatJSON
	msgs                          map[string][]manager.MessageJSON // per chat, oldest first
	media                         map[string]asset                 // per message id
}

// clock places demo messages relative to today.
type clock struct{ now, today time.Time }

func newClock() clock {
	now := time.Now()
	return clock{now: now, today: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())}
}

// at is hh:mm, days ago; never later than now.
func (c clock) at(days, hh, mm int) time.Time {
	t := c.today.AddDate(0, 0, -days).Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
	if latest := c.now.Add(-time.Minute); t.After(latest) {
		return latest
	}
	return t
}

// builder assembles an account's chats and messages.
type builder struct {
	a     *account
	c     clock
	voice []byte
	n     int
}

func (b *builder) id() string {
	b.n++
	return fmt.Sprintf("3EB0DEMO%04d", b.n)
}

func (b *builder) base() string { return "/v1/instances/" + b.a.id }

// msg adds a message to chat; from is a person key, "" for us.
func (b *builder) msg(chat, from string, t time.Time, text string, opts ...func(*manager.MessageJSON)) *manager.MessageJSON {
	m := manager.MessageJSON{ID: b.id(), Chat: chat, FromMe: from == "", IsGroup: strings.HasSuffix(chat, "@g.us"),
		Timestamp: t, Type: "text", Text: text, Source: "live"}
	if from == "" {
		m.Sender = meLID
	} else {
		p := who(from)
		m.Sender, m.PushName, m.SenderPhone, m.SenderName = p.lid(), p.push, p.phone(), p.name
	}
	for _, o := range opts {
		o(&m)
	}
	b.a.msgs[chat] = append(b.a.msgs[chat], m)
	return &b.a.msgs[chat][len(b.a.msgs[chat])-1]
}

func (b *builder) mediaPath(chat, id string) string {
	return b.base() + "/chats/" + chat + "/messages/" + id + "/media"
}

// image makes the message a photo.
func (b *builder) image(key string, w, h int) func(*manager.MessageJSON) {
	return func(m *manager.MessageJSON) {
		m.Type = "image"
		m.Media = &manager.MediaJSON{Kind: "image", MimeType: "image/jpeg", Caption: m.Text, Download: b.mediaPath(m.Chat, m.ID)}
		b.a.media[m.ID] = asset{"image/jpeg", photo(key, w, h)}
	}
}

// voiceNote makes the message a voice note with a waveform.
func (b *builder) voiceNote(seconds uint32) func(*manager.MessageJSON) {
	return func(m *manager.MessageJSON) {
		wave := make([]byte, 64)
		for i := range wave {
			wave[i] = byte(20 + (seed(m.ID+fmt.Sprint(i)) % 80))
		}
		m.Type = "ptt"
		m.Media = &manager.MediaJSON{Kind: "voice", MimeType: "audio/ogg; codecs=opus", Seconds: seconds, Waveform: wave, Download: b.mediaPath(m.Chat, m.ID)}
		b.a.media[m.ID] = asset{"audio/ogg", b.voice}
	}
}

func (b *builder) document(name string, size uint64) func(*manager.MessageJSON) {
	return func(m *manager.MessageJSON) {
		m.Type = "document"
		m.Media = &manager.MediaJSON{Kind: "document", MimeType: "application/pdf", FileName: name, FileLength: size, Download: b.mediaPath(m.Chat, m.ID)}
		b.a.media[m.ID] = asset{"application/pdf", []byte("%PDF-1.4\n% wax demo document\n")}
	}
}

func personOf(key string) manager.PersonJSON {
	p := who(key)
	return manager.PersonJSON{JID: p.lid(), Phone: p.phone(), Name: p.name, PushName: p.push, Username: p.username}
}

func quoting(q *manager.MessageJSON) func(*manager.MessageJSON) {
	return func(m *manager.MessageJSON) {
		qt := &manager.QuotedJSON{ID: q.ID, Type: q.Type, Text: q.Text}
		if q.Media != nil {
			qt.Type = q.Media.Kind
		}
		if !q.FromMe {
			s := manager.PersonJSON{JID: q.Sender, Phone: q.SenderPhone, Name: q.SenderName, PushName: q.PushName}
			qt.Sender = &s
		}
		m.QuotedID, m.Quoted = q.ID, qt
	}
}

func mentioning(keys ...string) func(*manager.MessageJSON) {
	return func(m *manager.MessageJSON) {
		for _, k := range keys {
			m.Mentions = append(m.Mentions, personOf(k))
		}
	}
}

func edited(m *manager.MessageJSON) { m.Edited = true }

func forwarded(score uint32) func(*manager.MessageJSON) {
	return func(m *manager.MessageJSON) { m.Forwarded, m.ForwardingScore = true, score }
}

// stub makes the message a system message about people.
func stub(name string, keys ...string) func(*manager.MessageJSON) {
	return func(m *manager.MessageJSON) {
		m.Type, m.Text = "stub", ""
		m.Stub = &manager.StubJSON{Name: name}
		for _, k := range keys {
			m.Stub.People = append(m.Stub.People, personOf(k))
		}
	}
}

// chat registers a chat; its last message and time come from its messages.
func (b *builder) chat(jid string, opts ...func(*chatJSON)) {
	c := &chatJSON{JID: jid}
	if g := groupByJID(jid); g != nil {
		c.Name = g.name
	} else {
		for _, p := range people {
			if p.lid() == jid {
				c.Name, c.Phone, c.PushName, c.Username = p.name, p.phone(), p.push, p.username
			}
		}
	}
	if ms := b.a.msgs[jid]; len(ms) > 0 {
		last := ms[len(ms)-1]
		c.LastMessage, c.LastMessageAt = &last, &last.Timestamp
	}
	for _, o := range opts {
		o(c)
	}
	b.a.chats = append(b.a.chats, c)
}

func unread(n int) func(*chatJSON) { return func(c *chatJSON) { c.UnreadCount = n } }
func pinned(c *chatJSON)           { c.Pinned = true }
func archived(c *chatJSON)         { c.Archived = true }
func muted(c *chatJSON) {
	t := time.Now().AddDate(1, 0, 0)
	c.MutedUntil = &t
}

// buildSupport is the main demo account.
func buildSupport(voice []byte) *account {
	a := &account{id: "support", status: "connected", jid: "905550000001:4@s.whatsapp.net", lid: "100000000001:4@lid",
		webhook: "https://example.com/wax/hooks", msgs: map[string][]manager.MessageJSON{}, media: map[string]asset{}}
	b := &builder{a: a, c: newClock(), voice: voice}
	c := b.c
	emma, sofia, daniel := who("10").lid(), who("12").lid(), who("17").lid()

	// Emma: the conversation of the main screenshot.
	b.msg(emma, "10", c.at(1, 18, 2), "Hi! Are we still on for the launch review tomorrow?")
	b.msg(emma, "", c.at(1, 18, 5), "Yes, 10:00 in the small meeting room.")
	b.msg(emma, "", c.at(1, 18, 6), "", b.document("launch-plan-v3.pdf", 482_133))
	b.msg(emma, "10", c.at(1, 18, 20), "Perfect, I'll read it tonight 👍")
	shot := b.msg(emma, "10", c.at(0, 9, 12), "The new storefront mockup", b.image("storefront", 1200, 800))
	b.msg(emma, "", c.at(0, 9, 15), "Looks great! Can we try a darker header?", quoting(shot))
	b.msg(emma, "10", c.at(0, 9, 40), "", b.voiceNote(14))
	b.msg(emma, "", c.at(0, 9, 42), "Got it, I'll update the colors and send a new version.", edited)
	b.msg(emma, "10", c.at(0, 9, 50), "Can you send the invoice as well?")
	b.chat(emma, pinned, unread(2))

	// Product Team: a group with names, pictures, mentions, quotes.
	pt := "120363000000000101@g.us"
	b.msg(pt, "11", c.at(1, 16, 30), "", stub("group_participant_add", "13"))
	b.msg(pt, "13", c.at(1, 16, 32), "Hi everyone, happy to be here!")
	rel := b.msg(pt, "11", c.at(0, 8, 55), "Morning all! Release 2.4 is ready for QA.")
	b.msg(pt, "14", c.at(0, 9, 1), "@200000000010 can you review the changelog?", mentioning("10"))
	b.msg(pt, "10", c.at(0, 9, 5), "On it, will finish before lunch.", quoting(rel))
	b.msg(pt, "13", c.at(0, 9, 20), "Reminder: the office is closed on Friday.", forwarded(1))
	b.msg(pt, "11", c.at(0, 9, 58), "All green", b.image("dashboard", 1280, 720))
	b.msg(pt, "", c.at(0, 10, 3), "Great work everyone! 🎉")
	b.msg(pt, "14", c.at(0, 10, 4), "🚀🚀")
	b.chat(pt, unread(5))

	// Sofia: a voice note.
	b.msg(sofia, "", c.at(0, 8, 10), "Did you get the color palette?")
	b.msg(sofia, "12", c.at(0, 8, 31), "", b.voiceNote(27))
	b.chat(sofia)

	hike := "120363000000000102@g.us"
	b.msg(hike, "15", c.at(0, 7, 45), "Trail conditions look perfect this weekend.")
	b.msg(hike, "16", c.at(0, 7, 58), "View from last time", b.image("summit", 1080, 1350))
	b.chat(hike, muted)

	b.msg(daniel, "17", c.at(0, 7, 20), "Hi! Is the blue jacket still available in size M?")
	b.chat(daniel, unread(1))

	noah := who("13").lid()
	b.msg(noah, "", c.at(1, 21, 4), "Sent you the access details by email.")
	b.msg(noah, "13", c.at(1, 21, 9), "Thanks 🙏")
	b.chat(noah)

	design := "120363000000000103@g.us"
	b.msg(design, "12", c.at(1, 15, 2), "New icons are in the shared folder.")
	b.msg(design, "10", c.at(1, 15, 30), "Love the rounded corners.")
	b.chat(design)

	bakery := who("18").lid()
	b.msg(bakery, "18", c.at(1, 11, 0), "Your order #1042 is ready for pickup 🥐")
	b.chat(bakery)

	ann := "120363000000000104@g.us"
	b.msg(ann, "11", c.at(2, 9, 0), "Welcome to the Acme Community! Group links are in the description.")
	b.chat(ann)

	cs := "120363000000000105@g.us"
	b.msg(cs, "16", c.at(2, 14, 12), "Ticket backlog is down to 12 🎯")
	b.chat(cs)

	for _, k := range []string{"14", "15", "16"} {
		jid := who(k).lid()
		b.msg(jid, k, c.at(3, 12, 0), map[string]string{"14": "See you at the station!", "15": "Saturday works for me.", "16": "Thank you, talk soon."}[k])
		b.chat(jid)
	}

	// The archive.
	olivia, ravi, old := who("19").lid(), who("20").lid(), "120363000000000106@g.us"
	b.msg(olivia, "19", c.at(5, 10, 0), "Let's catch up next month.")
	b.chat(olivia, archived)
	b.msg(ravi, "20", c.at(8, 17, 0), "Invoice paid, thanks.")
	b.chat(ravi, archived)
	b.msg(old, "20", c.at(1, 9, 0), "Archiving the repo this week.")
	b.chat(old, archived, unread(1))

	// Status updates of the last day.
	st := "status@broadcast"
	b.msg(st, "12", c.at(0, 7, 5), "Morning light", b.image("status-sofia-1", 1080, 1920))
	b.msg(st, "12", c.at(0, 7, 6), "", b.image("status-sofia-2", 1080, 1920))
	b.msg(st, "16", c.at(0, 8, 40), "Bosphorus", b.image("status-zeynep", 1080, 1920))
	b.msg(st, "15", c.at(0, 6, 30), "Out of office until Monday")
	b.msg(st, "", c.at(0, 6, 0), "Launching something new this week ✨")

	slices.SortStableFunc(a.chats, func(x, y *chatJSON) int { return y.LastMessageAt.Compare(*x.LastMessageAt) })
	return a
}

// buildMarketing is a second, quieter account.
func buildMarketing(voice []byte) *account {
	a := &account{id: "marketing", status: "connected", jid: "905550000002:2@s.whatsapp.net", lid: "100000000002:2@lid",
		msgs: map[string][]manager.MessageJSON{}, media: map[string]asset{}}
	b := &builder{a: a, c: newClock(), voice: voice, n: 500}
	mia := who("14").lid()
	b.msg(mia, "14", b.c.at(0, 11, 0), "The campaign images are approved.")
	b.chat(mia)
	return a
}
