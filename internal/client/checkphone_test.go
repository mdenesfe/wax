package client

import (
	"errors"
	"testing"

	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
)

// TestCheckPhone: an account is a number the server returns a LID for;
// known numbers need no query; "rate-overlimit" pauses further checks.
func TestCheckPhone(t *testing.T) {
	h, c, _ := loggedIn(t, nil)
	type result struct {
		pc  PhoneCheck
		err error
	}
	check := func(phone string, answer func(id string, user wabin.JID)) result {
		t.Helper()
		got := make(chan result, 1)
		go func() {
			pc, err := h.client.CheckPhone(h.ctx, phone)
			got <- result{pc, err}
		}()
		if answer != nil {
			q, err := c.ReadIQ(h.ctx, "usync")
			must(t, err)
			us, _ := q.Child("usync")
			list, _ := us.Child("list")
			user, _ := list.ChildrenByTag("user")[0].AttrJID("jid")
			answer(q.Attrs["id"].(string), user)
		}
		return <-got
	}
	listOf := func(id string, users ...wabin.Node) wabin.Node {
		return fakewa.Result(id, wabin.Node{Tag: "usync", Content: []wabin.Node{{Tag: "list", Content: users}}})
	}

	// On WhatsApp: the LID comes back and is stored.
	r := check("+905550000020", func(id string, user wabin.JID) {
		if user.User != "905550000020" || user.Server != wabin.ServerUser {
			t.Fatalf("asked for %v", user)
		}
		must(t, c.Send(h.ctx, listOf(id, wabin.Node{Tag: "user", Attrs: wabin.Attrs{"jid": user}, Content: []wabin.Node{
			{Tag: "lid", Attrs: wabin.Attrs{"val": wabin.JID{User: "777000020", Server: wabin.ServerLID}}}}})))
	})
	if r.err != nil || !r.pc.OnWhatsApp || r.pc.Phone != "905550000020" || r.pc.JID.String() != "905550000020@s.whatsapp.net" || r.pc.LID.String() != "777000020@lid" {
		t.Fatalf("on WhatsApp: %+v %v", r.pc, r.err)
	}
	// Known now: answered without a query (one would block).
	if r := check("905550000020", nil); r.err != nil || !r.pc.OnWhatsApp || r.pc.LID.User != "777000020" {
		t.Fatalf("known number: %+v %v", r.pc, r.err)
	}
	// Not on WhatsApp: no LID in the answer.
	r = check("905550000021", func(id string, user wabin.JID) {
		must(t, c.Send(h.ctx, listOf(id, wabin.Node{Tag: "user", Attrs: wabin.Attrs{"jid": user}})))
	})
	if r.err != nil || r.pc.OnWhatsApp || r.pc.Phone != "905550000021" || !r.pc.JID.IsEmpty() {
		t.Fatalf("not on WhatsApp: %+v %v", r.pc, r.err)
	}
	// Rate limited: reported, and the next check does not ask.
	r = check("905550000022", func(id string, _ wabin.JID) {
		must(t, c.Send(h.ctx, wabin.Node{Tag: "iq", Attrs: wabin.Attrs{"from": wabin.ServerJID, "type": "error", "id": id},
			Content: []wabin.Node{{Tag: "error", Attrs: wabin.Attrs{"code": "429", "text": "rate-overlimit"}}}}))
	})
	if !errors.Is(r.err, ErrRateLimited) {
		t.Fatalf("429: %v", r.err)
	}
	if r := check("905550000023", nil); !errors.Is(r.err, ErrRateLimited) {
		t.Fatalf("during the pause: %v", r.err)
	}
	if _, err := h.client.CheckPhone(h.ctx, "abc@g.us"); !errors.Is(err, wabin.ErrInvalidJID) {
		t.Fatalf("a group: %v", err)
	}
}
