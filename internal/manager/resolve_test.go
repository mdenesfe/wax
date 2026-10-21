package manager

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/store"
)

// TestResolvePeople: a message from a contact who hides their number (a
// LID) gets the number and names the store knows, as sender_alt, chat_alt
// and on its mentions, the same way for webhooks and listings.
func TestResolvePeople(t *testing.T) {
	dsn := os.Getenv("WAX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAX_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	id := fmt.Sprintf("test-resolve-%d", time.Now().UnixNano())
	d, _ := store.NewDevice(id)
	if err := st.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DeleteDevice(ctx, id) })
	for _, m := range [][2]string{{"905550000002", "2222"}, {"905550000003", "3333"}} {
		if _, err := st.PutLIDMapping(ctx, id, m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutContactName(ctx, id, "905550000003@s.whatsapp.net", store.ContactName{Full: "Ayşe"}); err != nil {
		t.Fatal(err)
	}

	m := MessageJSON{Chat: "2222@lid", Sender: "2222:3@lid", Mentions: []PersonJSON{{JID: "3333@lid"}}}
	group := MessageJSON{Chat: "120363000000000001@g.us", Sender: "905550000003@s.whatsapp.net"}
	if err := ResolvePeople(ctx, st, id, []*MessageJSON{&m, &group}); err != nil {
		t.Fatal(err)
	}
	if m.SenderAlt != "905550000002@s.whatsapp.net" || m.ChatAlt != "905550000002@s.whatsapp.net" || m.SenderPhone != "905550000002" {
		t.Fatalf("1:1 message %+v", m)
	}
	if p := m.Mentions[0]; p.Phone != "905550000003" || p.Name != "Ayşe" {
		t.Fatalf("mention %+v", p)
	}
	if group.SenderAlt != "3333@lid" || group.ChatAlt != "" || group.SenderName != "Ayşe" {
		t.Fatalf("group message %+v", group)
	}
}
