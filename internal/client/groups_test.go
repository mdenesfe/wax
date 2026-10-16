package client

import (
	"errors"
	"testing"

	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
)

// TestJoinedGroups: one request lists the groups, and community links are
// read (the community, its announcement group, a linked group).
func TestJoinedGroups(t *testing.T) {
	h, c, _ := loggedIn(t, nil)
	community := wabin.JID{User: "120363000000000001", Server: wabin.ServerGroup}
	type result struct {
		groups []*GroupInfo
		err    error
	}
	got := make(chan result, 1)
	go func() {
		g, err := h.client.JoinedGroups(h.ctx)
		got <- result{g, err}
	}()
	q, err := c.ReadIQ(h.ctx, "w:g2")
	must(t, err)
	if to, _ := q.AttrJID("to"); to != (wabin.JID{Server: wabin.ServerGroup}) {
		t.Fatalf("to %v", q.Attrs["to"])
	}
	if p, ok := q.Child("participating"); !ok || len(p.Children()) != 2 {
		t.Fatalf("query %s", q.String())
	}
	group := func(id, subject string, extra ...wabin.Node) wabin.Node {
		return wabin.Node{Tag: "group", Attrs: wabin.Attrs{"id": id, "subject": subject}, Content: extra}
	}
	must(t, c.Send(h.ctx, wabin.Node{Tag: "iq", Attrs: wabin.Attrs{"from": wabin.JID{Server: wabin.ServerGroup}, "type": "result", "id": q.Attrs["id"]},
		Content: []wabin.Node{{Tag: "groups", Content: []wabin.Node{
			group(community.User, "Community", wabin.Node{Tag: "parent"}),
			group("120363000000000002", "Announcements", wabin.Node{Tag: "linked_parent", Attrs: wabin.Attrs{"jid": community}}, wabin.Node{Tag: "default_sub_group"}, wabin.Node{Tag: "announcement"}),
			group("120363000000000003", "Team", wabin.Node{Tag: "linked_parent", Attrs: wabin.Attrs{"jid": community}},
				wabin.Node{Tag: "participant", Attrs: wabin.Attrs{"jid": bobLID}}),
		}}}}))
	r := <-got
	must(t, r.err)
	if len(r.groups) != 3 || !r.groups[0].IsCommunity || r.groups[1].Community != community || !r.groups[1].DefaultSubGroup ||
		!r.groups[1].Announce || r.groups[2].Community != community || r.groups[2].DefaultSubGroup || len(r.groups[2].Participants) != 1 {
		t.Fatalf("groups %+v %+v %+v", r.groups[0], r.groups[1], r.groups[2])
	}
	// Listed groups are cached for sends.
	if gi, ok := h.client.groups.get(r.groups[2].JID); !ok || gi.Name != "Team" {
		t.Fatal("group not cached")
	}
}

// TestProfile: about text, LID and devices come from one user sync; a
// business account's profile is fetched too; the result is cached.
func TestProfile(t *testing.T) {
	h, c, _ := loggedIn(t, nil)
	user := wabin.JID{User: "905550000009", Server: wabin.ServerUser}
	lid := wabin.JID{User: "777000999", Server: wabin.ServerLID}
	type result struct {
		p   *UserProfile
		err error
	}
	got := make(chan result, 1)
	go func() {
		p, err := h.client.Profile(h.ctx, user)
		got <- result{p, err}
	}()
	q, err := c.ReadIQ(h.ctx, "usync")
	must(t, err)
	us, _ := q.Child("usync")
	query, _ := us.Child("query")
	for _, tag := range []string{"business", "status", "devices", "lid"} {
		if _, ok := query.Child(tag); !ok {
			t.Fatalf("query lacks <%s>: %s", tag, q.String())
		}
	}
	must(t, c.Send(h.ctx, fakewa.Result(q.Attrs["id"].(string), wabin.Node{Tag: "usync", Content: []wabin.Node{{Tag: "list", Content: []wabin.Node{
		{Tag: "user", Attrs: wabin.Attrs{"jid": user}, Content: []wabin.Node{
			{Tag: "status", Attrs: wabin.Attrs{"t": "1700000000"}, Content: []byte("Hey there")},
			{Tag: "lid", Attrs: wabin.Attrs{"val": lid}},
			{Tag: "devices", Content: []wabin.Node{{Tag: "device-list", Content: []wabin.Node{
				{Tag: "device", Attrs: wabin.Attrs{"id": 0}}, {Tag: "device", Attrs: wabin.Attrs{"id": 3}}}}}},
			{Tag: "business", Content: []wabin.Node{{Tag: "verified_name", Attrs: wabin.Attrs{"v": "1"}}}},
		}},
	}}}})))
	q, err = c.ReadIQ(h.ctx, "w:biz")
	must(t, err)
	must(t, c.Send(h.ctx, fakewa.Result(q.Attrs["id"].(string), wabin.Node{Tag: "business_profile", Content: []wabin.Node{{Tag: "profile", Attrs: wabin.Attrs{"jid": user},
		Content: []wabin.Node{
			{Tag: "address", Content: []byte("Istiklal Cd. 1")},
			{Tag: "website", Content: []byte("https://a.example")},
			{Tag: "website", Content: []byte("https://b.example")},
			{Tag: "categories", Content: []wabin.Node{{Tag: "category", Attrs: wabin.Attrs{"id": "1"}, Content: []byte("Bakery")}}},
			{Tag: "business_hours", Attrs: wabin.Attrs{"timezone": "Europe/Istanbul"}, Content: []wabin.Node{
				{Tag: "business_hours_config", Attrs: wabin.Attrs{"day_of_week": "mon", "mode": "open_24h"}}}},
		}}}})))
	r := <-got
	must(t, r.err)
	p := r.p
	if p.About != "Hey there" || p.AboutSetAt.Unix() != 1700000000 || p.LID != lid || p.Devices != 2 || !p.IsBusiness {
		t.Fatalf("profile %+v", p)
	}
	b := p.Business
	if b == nil || b.Fields["address"][0] != "Istiklal Cd. 1" || len(b.Fields["website"]) != 2 || b.Categories[0] != "Bakery" ||
		b.TimeZone != "Europe/Istanbul" || len(b.Hours) != 1 || b.Hours[0].Mode != "open_24h" {
		t.Fatalf("business %+v", b)
	}
	// Cached: no second query (one would block without an answer).
	if again, err := h.client.Profile(h.ctx, user); err != nil || again != p {
		t.Fatalf("not cached: %v", err)
	}

	// "rate-overlimit" pauses lookups: the next one fails without asking.
	other := wabin.JID{User: "905550000010", Server: wabin.ServerUser}
	errs := make(chan error, 1)
	go func() {
		_, err := h.client.Profile(h.ctx, other)
		errs <- err
	}()
	q, err = c.ReadIQ(h.ctx, "usync")
	must(t, err)
	must(t, c.Send(h.ctx, wabin.Node{Tag: "iq", Attrs: wabin.Attrs{"from": wabin.ServerJID, "type": "error", "id": q.Attrs["id"]},
		Content: []wabin.Node{{Tag: "error", Attrs: wabin.Attrs{"code": "429", "text": "rate-overlimit"}}}}))
	if err := <-errs; !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	if _, err := h.client.Profile(h.ctx, other); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("during the pause: %v", err)
	}
}
