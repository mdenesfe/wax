package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/wabin"
)

func TestSealer(t *testing.T) {
	mk := make([]byte, 32)
	rand.Read(mk)
	s, err := newSealer(mk)
	if err != nil {
		t.Fatal(err)
	}
	ad := adFor("t", "c", "inst")
	blob := s.seal([]byte("secret"), ad)
	if bytes.Contains(blob, []byte("secret")) {
		t.Fatal("plaintext visible in sealed blob")
	}
	pt, err := s.open(blob, ad)
	if err != nil || string(pt) != "secret" {
		t.Fatalf("open: %q %v", pt, err)
	}
	// Moving the value to another instance or column must fail.
	if _, err := s.open(blob, adFor("t", "c", "other")); !errors.Is(err, ErrSealed) {
		t.Fatal("ciphertext accepted under different associated data")
	}
	// A different master key must fail.
	other := make([]byte, 32)
	rand.Read(other)
	s2, _ := newSealer(other)
	if _, err := s2.open(blob, ad); !errors.Is(err, ErrSealed) {
		t.Fatal("ciphertext opened with a different master key")
	}
	blob[len(blob)-1] ^= 1
	if _, err := s.open(blob, ad); !errors.Is(err, ErrSealed) {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestParseMasterKey(t *testing.T) {
	if _, err := ParseMasterKey("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "short", "AAAA"} {
		if _, err := ParseMasterKey(bad); !errors.Is(err, ErrMasterKey) {
			t.Errorf("%q: expected ErrMasterKey, got %v", bad, err)
		}
	}
}

// openTestStore connects to WAX_TEST_DATABASE_URL or skips. Each test uses a
// unique instance id, so tests can share the database and run in parallel.
func openTestStore(t testing.TB) *Store {
	t.Helper()
	dsn := os.Getenv("WAX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WAX_TEST_DATABASE_URL not set")
	}
	mk := bytes.Repeat([]byte{7}, 32)
	s, err := Open(context.Background(), dsn, mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func testInstance(t *testing.T) string {
	return fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
}

func TestDeviceLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })

	if _, err := s.LoadDevice(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	d, err := NewDevice(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDevice(ctx, d); err == nil {
		t.Fatal("duplicate create should fail")
	}
	got, err := s.LoadDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.NoiseKey.Priv != d.NoiseKey.Priv || got.IdentityKey.Pub != d.IdentityKey.Pub ||
		got.SignedPreKey.Key.Priv != d.SignedPreKey.Key.Priv || got.AdvSecret != d.AdvSecret ||
		got.RegistrationID != d.RegistrationID || got.IsPaired() {
		t.Fatal("loaded device differs from created one")
	}
	if !keys.Verify(got.IdentityKey.Pub, append([]byte{keys.DJBType}, got.SignedPreKey.Key.Pub[:]...), got.SignedPreKey.Signature) {
		t.Fatal("signed prekey signature does not verify after reload")
	}

	// Pairing.
	got.JID = wabin.JID{User: "905550000000", Device: 7, Server: wabin.ServerUser}
	got.LID = wabin.JID{User: "123456789", Device: 7, Server: wabin.ServerLID}
	got.Account = []byte{1, 2, 3}
	got.Platform = "android"
	got.PairedAt = time.Now().Truncate(time.Second)
	err = s.InTx(ctx, func(tx *Store) error {
		if err := tx.SavePairing(ctx, got); err != nil {
			return err
		}
		return tx.PutIdentity(ctx, id, "123456789@lid", [32]byte{9})
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.LoadDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.JID != got.JID || again.LID != got.LID || !bytes.Equal(again.Account, got.Account) || !again.IsPaired() || !again.PairedAt.Equal(got.PairedAt) {
		t.Fatalf("pairing not persisted: %+v", again)
	}
	if k, err := s.GetIdentity(ctx, id, "123456789@lid"); err != nil || k != [32]byte{9} {
		t.Fatalf("identity: %v %v", k, err)
	}

	// Delete cascades.
	if err := s.DeleteDevice(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdentity(ctx, id, "123456789@lid"); !errors.Is(err, ErrNotFound) {
		t.Fatal("identity survived device deletion")
	}
}

func TestTxRollback(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	boom := errors.New("boom")
	err := s.InTx(ctx, func(tx *Store) error {
		if err := tx.CreateDevice(ctx, d); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.LoadDevice(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("rolled back device is visible")
	}
}

func TestPreKeys(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := testInstance(t)
	d, _ := NewDevice(id)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DeleteDevice(ctx, id) })

	// Concurrent generation must never produce duplicate ids.
	var wg sync.WaitGroup
	results := make([][]PreKey, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.GetOrGenPreKeys(ctx, id, 30)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// All callers asked for the same 30 un-uploaded keys; whichever ran
	// first generated them and the rest reused them.
	first := results[0]
	if len(first) != 30 {
		t.Fatalf("got %d keys", len(first))
	}
	for _, r := range results[1:] {
		for i := range r {
			if r[i].ID != first[i].ID || r[i].Key.Priv != first[i].Key.Priv {
				t.Fatal("concurrent callers saw different keys")
			}
		}
	}

	ids := make([]uint32, len(first))
	for i, k := range first {
		ids[i] = k.ID
	}
	if err := s.MarkPreKeysUploaded(ctx, id, ids); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UploadedPreKeyCount(ctx, id); n != 30 {
		t.Fatalf("uploaded count %d", n)
	}
	next, err := s.GetOrGenPreKeys(ctx, id, 5)
	if err != nil {
		t.Fatal(err)
	}
	if next[0].ID != 31 {
		t.Fatalf("expected new keys to start at 31, got %d", next[0].ID)
	}
	pk, err := s.GetPreKey(ctx, id, 3)
	if err != nil || pk.Key.Priv != first[2].Key.Priv {
		t.Fatalf("GetPreKey: %v", err)
	}
	if err := s.RemovePreKey(ctx, id, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPreKey(ctx, id, 3); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed prekey still present")
	}
}

func TestAPIKeys(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tenant := testInstance(t)
	if err := s.CreateTenant(ctx, &Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteTenant(ctx, tenant) })
	token, key, err := s.CreateAPIKey(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.VerifyAPIKey(ctx, token); err != nil || got != tenant {
		t.Fatalf("verify: %q %v", got, err)
	}
	// Only an HMAC is stored: the secret never appears in the row.
	var mac []byte
	if err := s.db.QueryRow(ctx, `SELECT mac FROM wax_api_keys WHERE id = $1`, key.ID).Scan(&mac); err != nil {
		t.Fatal(err)
	}
	secret := token[len(token)-43:]
	if bytes.Contains([]byte(token), mac) || bytes.Contains(mac, []byte(secret)) {
		t.Fatal("secret stored in the database")
	}
	// The secret is bound to its key id: another id with the same secret fails.
	other, _, err := s.CreateAPIKey(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	swapped := other[:len("wax_")+16] + "_" + secret
	if _, err := s.VerifyAPIKey(ctx, swapped); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("secret accepted under another key id: %v", err)
	}
	if _, _, err := s.CreateAPIKey(ctx, "no-such-tenant"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key for unknown tenant: %v", err)
	}
	if err := s.DeleteAPIKey(ctx, tenant, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAPIKey(ctx, token); !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("revoked key accepted: %v", err)
	}
}
