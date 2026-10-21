package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestEnsureTenantKey: a provisioned key creates its tenant, works like an
// issued one, can be provisioned again, and cannot move to another tenant.
func TestEnsureTenantKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tenant := fmt.Sprintf("env%d", time.Now().UnixNano())
	other := tenant + "-b"
	t.Cleanup(func() {
		_ = s.DeleteTenant(ctx, tenant)
		_ = s.DeleteTenant(ctx, other)
	})
	key, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // idempotent
		if err := s.EnsureTenantKey(ctx, tenant, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetTenant(ctx, tenant); err != nil {
		t.Fatalf("tenant not created: %v", err)
	}
	a, err := s.VerifyAPIKey(ctx, key)
	if err != nil || a.TenantID != tenant {
		t.Fatalf("verify: %+v %v", a, err)
	}
	if keys, _ := s.ListAPIKeys(ctx, tenant); len(keys) != 1 {
		t.Fatalf("%d keys, want 1", len(keys))
	}
	if err := s.EnsureTenantKey(ctx, other, key); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key for another tenant: %v", err)
	}
	if a, _ := s.VerifyAPIKey(ctx, key); a.TenantID != tenant {
		t.Fatalf("key moved to %q", a.TenantID)
	}
	for _, bad := range []string{"", "wax_", "wax_zzzzzzzzzzzzzzzz_" + key[len(key)-43:], key[:len(key)-1]} {
		if err := s.EnsureTenantKey(ctx, tenant, bad); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("malformed key %q accepted: %v", bad, err)
		}
	}
}
