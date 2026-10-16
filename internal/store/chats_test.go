package store

import (
	"context"
	"testing"
	"time"
)

// TestChatAliases: a contact's chats under its phone number and its LID
// are listed once, the history covers both, and its names are found under
// either address.
func TestChatAliases(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })

	const pn, lid = "905550000001@s.whatsapp.net", "1234@lid"
	if _, err := s.PutLIDMapping(ctx, id, "905550000001", "1234"); err != nil {
		t.Fatal(err)
	}
	// The address book knows the number, the push name came with the LID.
	if err := s.PutContactName(ctx, id, pn, ContactName{First: "Ali", Full: "Ali Veli", Username: "aliveli"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPushName(ctx, id, lid, "Ali V"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for i, jid := range []string{pn, lid} {
		at, unread := now.Add(time.Duration(i)*time.Minute), 1
		if err := s.UpdateChat(ctx, id, jid, ChatUpdate{LastMessageAt: &at, UnreadCount: &unread}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutMessage(ctx, id, &StoredMessage{Chat: jid, ID: "m" + jid, Sender: jid, SentAt: at, Type: "text"}); err != nil {
			t.Fatal(err)
		}
	}

	chats, err := s.ListChats(ctx, id, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || chats[0].JID != lid || chats[0].Phone != "905550000001" || chats[0].UnreadCount != 2 {
		t.Fatalf("chats %+v", chats)
	}
	if c := chats[0]; c.Name != "Ali Veli" || c.PushName != "Ali V" || c.Username != "aliveli" {
		t.Fatalf("names %q %q %q", c.Name, c.PushName, c.Username)
	}
	ids, err := s.Identities(ctx, id, []string{lid, pn, "999@lid"})
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Phone: "905550000001", LID: "1234", Name: "Ali Veli", PushName: "Ali V", Username: "aliveli"}
	if ids[lid] != want || ids[pn] != want || ids["999@lid"] != (Identity{LID: "999"}) {
		t.Fatalf("identities %+v", ids)
	}
	if chats[0].Last == nil || chats[0].Last.ID != "m"+lid {
		t.Fatalf("last message %+v", chats[0].Last)
	}
	for _, jid := range []string{pn, lid} {
		msgs, err := s.Messages(ctx, id, jid, now.Add(time.Hour), 10)
		if err != nil || len(msgs) != 2 {
			t.Fatalf("history of %s: %d messages, %v", jid, len(msgs), err)
		}
	}
}
