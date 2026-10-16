package client

import (
	"bytes"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/testutil/fakewa"
	"github.com/mdenesfe/wax/internal/wabin"
)

// TestPrivacyTokens: a token from a notification is stored under the
// sender's LID, sent with profile and picture queries for its phone
// number too, and ours is issued once per bucket.
func TestPrivacyTokens(t *testing.T) {
	h, c, _ := loggedIn(t, nil)
	pn := wabin.JID{User: "905550000011", Server: wabin.ServerUser}
	lid := wabin.JID{User: "777000011", Server: wabin.ServerLID}
	_, err := h.client.cfg.Store.PutLIDMapping(h.ctx, h.inst, pn.User, lid.User)
	must(t, err)
	token := []byte{1, 2, 3, 4}
	now := time.Now().Unix()

	must(t, c.Send(h.ctx, wabin.Node{Tag: "notification", Attrs: wabin.Attrs{"id": "N1", "from": pn, "sender_lid": lid, "type": "privacy_token", "t": now},
		Content: []wabin.Node{{Tag: "tokens", Content: []wabin.Node{{Tag: "token", Attrs: wabin.Attrs{"type": "trusted_contact", "t": now}, Content: token}}}}}))
	deadline := time.Now().Add(5 * time.Second)
	for h.client.privacyToken(h.ctx, pn) == nil {
		if time.Now().After(deadline) {
			t.Fatal("token not stored")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The profile query carries it for the user.
	errs := make(chan error, 2)
	go func() {
		_, err := h.client.Profile(h.ctx, pn)
		errs <- err
	}()
	q, err := c.ReadIQ(h.ctx, "usync")
	must(t, err)
	us, _ := q.Child("usync")
	list, _ := us.Child("list")
	user, _ := list.Child("user")
	tc, ok := user.Child("tctoken")
	if raw, _ := tc.Bytes(); !ok || !bytes.Equal(raw, token) {
		t.Fatalf("usync user %s", user.String())
	}
	must(t, c.Send(h.ctx, fakewa.Result(q.Attrs["id"].(string), wabin.Node{Tag: "usync", Content: []wabin.Node{{Tag: "list"}}})))
	must(t, <-errs)

	// So does the picture query.
	go func() {
		_, err := h.client.ProfilePicture(h.ctx, pn)
		errs <- err
	}()
	q, err = c.ReadIQ(h.ctx, "w:profile:picture")
	must(t, err)
	pic, _ := q.Child("picture")
	if tc, ok := pic.Child("tctoken"); !ok {
		t.Fatalf("picture query %s", q.String())
	} else if raw, _ := tc.Bytes(); !bytes.Equal(raw, token) {
		t.Fatal("picture token differs")
	}
	must(t, c.Send(h.ctx, wabin.Node{Tag: "iq", Attrs: wabin.Attrs{"from": wabin.ServerJID, "type": "error", "id": q.Attrs["id"]},
		Content: []wabin.Node{{Tag: "error", Attrs: wabin.Attrs{"code": "404"}}}}))
	<-errs

	// Ours is issued once per bucket.
	s, err := h.client.activeSession()
	must(t, err)
	done := make(chan struct{})
	go func() {
		h.client.issueToken(h.ctx, s, lid)
		close(done)
	}()
	q, err = c.ReadIQ(h.ctx, "privacy")
	must(t, err)
	tokens, _ := q.Child("tokens")
	tk, _ := tokens.Child("token")
	if j, _ := tk.AttrJID("jid"); j != lid || tk.Attrs["type"] != "trusted_contact" {
		t.Fatalf("issue %s", q.String())
	}
	must(t, c.Send(h.ctx, fakewa.Result(q.Attrs["id"].(string))))
	<-done
	again := make(chan struct{})
	go func() {
		h.client.issueToken(h.ctx, s, pn) // same user by number: already issued this bucket
		close(again)
	}()
	select {
	case <-again:
	case <-time.After(2 * time.Second):
		t.Fatal("issued twice in one bucket")
	}
}
