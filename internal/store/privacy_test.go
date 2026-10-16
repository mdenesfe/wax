package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestPrivacyTokens: tokens are sealed, a newer one wins, the freshest of
// a user's addresses is found, and expired ones are pruned.
func TestPrivacyTokens(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })

	const lid, pn = "1234@lid", "905550000001@s.whatsapp.net"
	now := time.Now().Truncate(time.Second)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutPrivacyToken(ctx, id, lid, []byte("new"), now))
	must(s.PutPrivacyToken(ctx, id, lid, []byte("old"), now.Add(-time.Hour))) // older: ignored
	must(s.PutPrivacyToken(ctx, id, pn, []byte("by-number"), now.Add(-2*time.Hour)))
	must(s.MarkPrivacyTokenSent(ctx, id, lid, now))

	got, err := s.GetPrivacyToken(ctx, id, lid, pn)
	must(err)
	if got == nil || !bytes.Equal(got.Token, []byte("new")) || !got.TokenAt.Equal(now) || !got.SentAt.Equal(now) || got.User != lid {
		t.Fatalf("token %+v", got)
	}
	var raw []byte
	must(s.db.QueryRow(ctx, `SELECT token FROM wax_privacy_tokens WHERE instance_id = $1 AND user_jid = $2`, id, lid).Scan(&raw))
	if bytes.Contains(raw, []byte("new")) {
		t.Fatal("token stored in the clear")
	}
	if none, err := s.GetPrivacyToken(ctx, id, "999@lid"); err != nil || none != nil {
		t.Fatalf("unknown user: %+v %v", none, err)
	}

	// Pruning keeps rows with a recent token or issuance.
	n, err := s.DeletePrivacyTokensBefore(ctx, id, now.Add(-90*time.Minute))
	must(err)
	if n != 1 {
		t.Fatalf("pruned %d, want 1 (the number's old token)", n)
	}
}
