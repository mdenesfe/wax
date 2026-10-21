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

// TestMaskedPhoneName: the masked number WhatsApp gives as the name of a
// contact who hides their number is not a name.
func TestMaskedPhoneName(t *testing.T) {
	for name, want := range map[string][2]string{
		"+90∙∙∙∙∙∙∙∙02":  {"", "+90∙∙∙∙∙∙∙∙02"},
		"+90 •••• •• 02": {"", "+90 •••• •• 02"},
		"Ali Veli":       {"Ali Veli", ""},
		"+905550000002":  {"+905550000002", ""},
		"∙∙∙":            {"∙∙∙", ""},
		"Ali ∙∙ 02":      {"Ali ∙∙ 02", ""},
	} {
		real, masked := splitMaskedPhone(name)
		if real != want[0] || masked != want[1] {
			t.Errorf("%q: name %q, masked %q", name, real, masked)
		}
	}

	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })
	const lid, masked = "5678@lid", "+90∙∙∙∙∙∙∙∙02"
	name, at := masked, time.Now()
	if err := s.UpdateChat(ctx, id, lid, ChatUpdate{Name: &name, LastMessageAt: &at}); err != nil {
		t.Fatal(err)
	}
	chats, err := s.ListChats(ctx, id, 50, 0)
	if err != nil || len(chats) != 1 || chats[0].Name != "" || chats[0].MaskedPhone != masked {
		t.Fatalf("listed %+v %v", chats, err)
	}
	c, err := s.GetChat(ctx, id, lid)
	if err != nil || c.Name != "" || c.MaskedPhone != masked {
		t.Fatalf("get %+v %v", c, err)
	}
	// With an address book name, that name is used.
	if err := s.PutContactName(ctx, id, lid, ContactName{Full: "Ayşe"}); err != nil {
		t.Fatal(err)
	}
	if chats, _ := s.ListChats(ctx, id, 50, 0); chats[0].Name != "Ayşe" || chats[0].MaskedPhone != masked {
		t.Fatalf("with a contact name %+v", chats[0])
	}
}
