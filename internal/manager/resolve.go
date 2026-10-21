package manager

import (
	"context"

	"github.com/mdenesfe/wax/internal/store"
	"github.com/mdenesfe/wax/internal/wabin"
)

// ResolvePeople fills in what is known (phone number, address book name,
// push name, username) about the people in messages: senders, quoted
// authors, mentioned users and users in system messages, and the other
// address of each sender and 1:1 chat (sender_alt, chat_alt: the phone
// number JID for a LID and the reverse). One lookup for all of them, so
// webhooks and listings describe people the same way.
func ResolvePeople(ctx context.Context, st *store.Store, instance string, msgs []*MessageJSON) error {
	var people []*PersonJSON
	var jids []string
	seen := make(map[string]bool)
	want := func(jid string) {
		if !seen[jid] {
			seen[jid] = true
			jids = append(jids, jid)
		}
	}
	add := func(p *PersonJSON) {
		p.JID = nonAD(p.JID)
		people = append(people, p)
		want(p.JID)
	}
	senders := make([]PersonJSON, len(msgs))
	for i, m := range msgs {
		if m.Sender != "" {
			senders[i].JID = m.Sender
			add(&senders[i])
		}
		if m.Quoted != nil && m.Quoted.Sender != nil {
			add(m.Quoted.Sender)
		}
		for j := range m.Mentions {
			add(&m.Mentions[j])
		}
		if m.Stub != nil {
			for j := range m.Stub.People {
				add(&m.Stub.People[j])
			}
		}
		if personal(m.Chat) {
			want(nonAD(m.Chat))
		}
	}
	ids, err := st.Identities(ctx, instance, jids)
	if err != nil {
		return err
	}
	for _, p := range people {
		id := ids[p.JID]
		p.Phone, p.Name, p.PushName, p.Username = id.Phone, id.Name, id.PushName, id.Username
	}
	for i, m := range msgs {
		if p := senders[i]; p.JID != "" {
			if !m.FromMe {
				m.SenderPhone, m.SenderName, m.SenderUsername = p.Phone, p.Name, p.Username
				if m.PushName == "" {
					m.PushName = p.PushName
				}
			}
			if m.SenderAlt == "" {
				m.SenderAlt = otherAddress(p.JID, ids[p.JID])
			}
		}
		if personal(m.Chat) && m.ChatAlt == "" {
			m.ChatAlt = otherAddress(nonAD(m.Chat), ids[nonAD(m.Chat)])
		}
	}
	return nil
}

// personal reports whether jid is a user (phone number or LID), not a
// group, broadcast or newsletter.
func personal(jid string) bool {
	j, err := wabin.ParseJID(jid)
	return err == nil && (j.Server == wabin.ServerUser || j.Server == wabin.ServerLID)
}

// otherAddress is the JID of a user's other address, if known.
func otherAddress(jid string, id store.Identity) string {
	j, err := wabin.ParseJID(jid)
	switch {
	case err != nil:
	case j.Server == wabin.ServerLID && id.Phone != "":
		return wabin.JID{User: id.Phone, Server: wabin.ServerUser}.String()
	case j.Server == wabin.ServerUser && id.LID != "":
		return wabin.JID{User: id.LID, Server: wabin.ServerLID}.String()
	}
	return ""
}

// nonAD drops the device part of a user JID ("1234:5@lid" -> "1234@lid").
func nonAD(jid string) string {
	if j, err := wabin.ParseJID(jid); err == nil {
		return j.ToNonAD().String()
	}
	return jid
}
