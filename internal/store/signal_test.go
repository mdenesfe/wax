package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/signal"
)

func newSessionRecord(t *testing.T) *signal.Record {
	t.Helper()
	ours, _ := keys.NewSigningKey()
	theirs, _ := keys.NewSigningKey()
	spk, _ := keys.NewKeyPair()
	r := &signal.Record{}
	err := r.ProcessBundle(signal.LocalIdentity{Key: &ours.KeyPair, RegistrationID: 1}, &signal.PreKeyBundle{
		RegistrationID: 2, IdentityKey: theirs.Pub, SignedPreKeyID: 1, SignedPreKey: spk.Pub,
		SignedPreKeySignature: theirs.SignPublicKey(spk.Pub),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSessionsAndMigration(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })

	empty, err := s.LoadSession(ctx, id, "905550000001.0")
	if err != nil || !empty.IsFresh() {
		t.Fatalf("expected fresh record, got %v", err)
	}
	rec := newSessionRecord(t)
	for _, addr := range []string{"905550000001.0", "905550000001.3", "9055500000012.0"} {
		if err := s.PutSession(ctx, id, addr, rec); err != nil {
			t.Fatal(err)
		}
		if err := s.PutIdentity(ctx, id, addr, [32]byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadSession(ctx, id, "905550000001.3")
	if err != nil || !got.HasSession() {
		t.Fatalf("load: %v", err)
	}
	have, _ := s.HasSessions(ctx, id, []string{"905550000001.0", "nobody.0"})
	if !have["905550000001.0"] || have["nobody.0"] {
		t.Fatalf("HasSessions: %v", have)
	}
	if err := s.MigrateSessions(ctx, id, "905550000001", "1234_1"); err != nil {
		t.Fatal(err)
	}
	have, _ = s.HasSessions(ctx, id, []string{"1234_1.0", "1234_1.3", "905550000001.0", "9055500000012.0"})
	if !have["1234_1.0"] || !have["1234_1.3"] || have["905550000001.0"] || !have["9055500000012.0"] {
		t.Fatalf("migration wrong (prefix must not match longer users): %v", have)
	}
	// The re-sealed record must open under its new address.
	if r, err := s.LoadSession(ctx, id, "1234_1.3"); err != nil || !r.HasSession() {
		t.Fatalf("migrated record unreadable: %v", err)
	}
	if _, err := s.GetIdentity(ctx, id, "1234_1.0"); err != nil {
		t.Fatalf("identity not migrated: %v", err)
	}
	if err := s.DeleteUserSessions(ctx, id, "1234_1"); err != nil {
		t.Fatal(err)
	}
	have, _ = s.HasSessions(ctx, id, []string{"1234_1.0", "9055500000012.0"})
	if have["1234_1.0"] || !have["9055500000012.0"] {
		t.Fatalf("DeleteUserSessions wrong: %v", have)
	}
}

func TestLIDMapBufferAndSent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })

	if changed, err := s.PutLIDMapping(ctx, id, "905550000001", "1234"); err != nil || !changed {
		t.Fatalf("first mapping: %v %v", changed, err)
	}
	if changed, _ := s.PutLIDMapping(ctx, id, "905550000001", "1234"); changed {
		t.Fatal("unchanged mapping reported as changed")
	}
	if lid, _ := s.LIDForPN(ctx, id, "905550000001"); lid != "1234" {
		t.Fatal("LIDForPN")
	}
	if pn, _ := s.PNForLID(ctx, id, "1234"); pn != "905550000001" {
		t.Fatal("PNForLID")
	}

	hash := []byte("h1")
	if ev, _ := s.GetBufferedEvent(ctx, id, hash); ev != nil {
		t.Fatal("unexpected event")
	}
	if err := s.PutBufferedEvent(ctx, id, hash, []byte("plain"), time.Now()); err != nil {
		t.Fatal(err)
	}
	ev, err := s.GetBufferedEvent(ctx, id, hash)
	if err != nil || !bytes.Equal(ev.Plaintext, []byte("plain")) {
		t.Fatalf("buffered event: %v", err)
	}
	if err := s.MarkEventHandled(ctx, id, hash); err != nil {
		t.Fatal(err)
	}
	if ev, _ := s.GetBufferedEvent(ctx, id, hash); ev == nil || ev.Plaintext != nil {
		t.Fatal("handled event should keep its hash but drop plaintext")
	}

	if err := s.PutSentMessage(ctx, id, "1234@lid", "3EB0AA", []byte("msg")); err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetSentMessage(ctx, id, "1234@lid", "3EB0AA"); err != nil || string(b) != "msg" {
		t.Fatalf("sent message: %v", err)
	}
	if _, err := s.GetSentMessage(ctx, id, "other@lid", "3EB0AA"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected ErrNotFound")
	}
	if err := s.PurgeOld(ctx, id, -time.Second, -time.Second); err != nil {
		t.Fatal(err)
	}
	if ev, _ := s.GetBufferedEvent(ctx, id, hash); ev != nil {
		t.Fatal("handled event not purged")
	}
	if _, err := s.GetSentMessage(ctx, id, "1234@lid", "3EB0AA"); !errors.Is(err, ErrNotFound) {
		t.Fatal("sent message not purged")
	}
}
