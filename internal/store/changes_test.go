package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestChangeMessage: a sender's edit replaces a text message and marks it
// edited, a revoke turns it into a deleted message; other people's changes
// and edits of media are not applied.
func TestChangeMessage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateDevice(ctx, d))
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })

	const chat, alice = "1234@lid", "1234:7@lid"
	at := time.Now().Truncate(time.Second)
	put := func(msgID, typ string, fromMe bool) {
		sender := alice
		if fromMe {
			sender = ""
		}
		must(s.PutMessage(ctx, id, &StoredMessage{Chat: chat, ID: msgID, Sender: sender, FromMe: fromMe, SentAt: at, Type: typ, Content: []byte("old"), Source: "live"}))
	}
	put("T1", "text", false)
	put("I1", "image", false)
	put("M1", "text", true)

	change := func(msgID, sender string, fromMe bool, content []byte) bool {
		t.Helper()
		ok, err := s.ChangeMessage(ctx, id, chat, msgID, sender, fromMe, content, at.Add(time.Minute))
		must(err)
		return ok
	}
	if change("T1", "9999@lid", false, []byte("forged")) {
		t.Fatal("someone else edited the message")
	}
	if change("T1", "1234@lid", true, []byte("forged")) {
		t.Fatal("we edited someone else's message")
	}
	if !change("T1", "1234@lid", false, []byte("new")) {
		t.Fatal("edit not applied")
	}
	m, err := s.GetMessage(ctx, id, chat, "T1")
	must(err)
	if string(m.Content) != "new" || m.EditedAt == nil || m.Type != "text" {
		t.Fatalf("edited: %q, edited_at %v, type %s", m.Content, m.EditedAt, m.Type)
	}
	if change("I1", "1234@lid", false, []byte("caption")) {
		t.Fatal("media edit applied")
	}
	if !change("T1", "1234@lid", false, nil) || !change("M1", "", true, nil) {
		t.Fatal("revoke not applied")
	}
	for _, msgID := range []string{"T1", "M1"} {
		m, err := s.GetMessage(ctx, id, chat, msgID)
		must(err)
		if m.Type != RevokedType || m.Content != nil {
			t.Fatalf("%s revoked: type %s, content %q", msgID, m.Type, m.Content)
		}
	}
	if change("missing", "1234@lid", false, nil) {
		t.Fatal("changed a message that does not exist")
	}
}

// TestHistoryKeepsAppStateSettings: history sync sets a chat's archive,
// pin and mute until app state does; after that, history leaves them.
func TestHistoryKeepsAppStateSettings(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })
	yes, no := true, false
	get := func(jid string) Chat {
		t.Helper()
		c, err := s.GetChat(ctx, id, jid)
		if err != nil {
			t.Fatal(err)
		}
		return *c
	}

	// History first, then app state, then older history again.
	if err := s.UpdateChat(ctx, id, "a@s.whatsapp.net", ChatUpdate{Archived: &yes, Pinned: &yes, FromHistory: true}); err != nil {
		t.Fatal(err)
	}
	if c := get("a@s.whatsapp.net"); !c.Archived || !c.Pinned {
		t.Fatalf("history not applied to a new chat: %+v", c)
	}
	if err := s.UpdateChat(ctx, id, "a@s.whatsapp.net", ChatUpdate{Archived: &no}); err != nil {
		t.Fatal(err)
	}
	unread := 3
	if err := s.UpdateChat(ctx, id, "a@s.whatsapp.net", ChatUpdate{Archived: &yes, Pinned: &no, UnreadCount: &unread, FromHistory: true}); err != nil {
		t.Fatal(err)
	}
	if c := get("a@s.whatsapp.net"); c.Archived || !c.Pinned || c.UnreadCount != 3 {
		t.Fatalf("history overrode app state settings: %+v", c)
	}
}

// TestOutboxOrderPerChat: an item waiting for a retry holds back later
// items to its chat, not those to other chats.
func TestOutboxOrderPerChat(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	tenant := fmt.Sprintf("t%d", time.Now().UnixNano())
	if err := s.CreateTenant(ctx, &Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInstance(ctx, &Instance{ID: id, TenantID: tenant, Name: id, RatePerMinute: 20}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.DeleteInstance(ctx, id)
		_ = s.DeleteTenant(ctx, tenant)
	})
	enqueue := func(chat, msgID string) int64 {
		t.Helper()
		it := &OutboxItem{Instance: id, Chat: chat, MessageID: msgID, Kind: "message", Payload: []byte("x")}
		if _, err := s.Enqueue(ctx, it); err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	a1 := enqueue("a@s.whatsapp.net", "A1")
	enqueue("a@s.whatsapp.net", "A2")
	b1 := enqueue("b@s.whatsapp.net", "B1")

	later := time.Now().Add(time.Hour)
	if err := s.MarkOutboxRetry(ctx, a1, later, "offline"); err != nil {
		t.Fatal(err)
	}
	next, err := s.NextOutbox(ctx, id)
	if err != nil || next == nil || next.ID != b1 {
		t.Fatalf("next: %+v, %v (want B1: A2 must wait for A1)", next, err)
	}
	if err := s.MarkOutboxSent(ctx, b1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextOutbox(ctx, id); err != nil || next != nil {
		t.Fatalf("A2 overtook A1: %+v, %v", next, err)
	}
	due, err := s.NextOutboxDue(ctx, id)
	if err != nil || due == nil || due.Sub(later).Abs() > time.Second {
		t.Fatalf("next due %v, want A1's retry time", due)
	}
}

// TestWebhookSettledStays: an attempt that ends after the URL was removed
// (the event dropped) does not bring the event back.
func TestWebhookSettledStays(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	tenant := fmt.Sprintf("t%d", time.Now().UnixNano())
	if err := s.CreateTenant(ctx, &Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInstance(ctx, &Instance{ID: id, TenantID: tenant, Name: id}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.DeleteInstance(ctx, id)
		_ = s.DeleteTenant(ctx, tenant)
	})
	if err := s.EnqueueWebhook(ctx, id, "presence", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	it, _, err := s.NextWebhook(ctx, id) // an attempt starts
	if err != nil || it == nil {
		t.Fatalf("next: %v %v", it, err)
	}
	if _, err := s.RetargetWebhooks(ctx, id, true); err != nil { // the URL is removed meanwhile
		t.Fatal(err)
	}
	if err := s.MarkWebhook(ctx, it.ID, "pending", time.Now(), "http 500"); err != nil { // the attempt fails
		t.Fatal(err)
	}
	d, err := s.QueueDepths(ctx, []string{id})
	if err != nil || d[id].Webhooks != 0 {
		t.Fatalf("pending after removal: %+v, %v", d[id], err)
	}
}
