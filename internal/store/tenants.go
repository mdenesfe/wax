package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mdenesfe/wax/internal/cryptoutil"
)

// Tenant owns instances and API keys.
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKey describes a tenant API key; the secret itself is never stored.
type APIKey struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// Errors of tenant and key management.
var (
	ErrTenantNotEmpty = errors.New("store: tenant still has instances")
	ErrInvalidAPIKey  = errors.New("store: invalid API key")
)

// apiKeyPrefix starts every tenant key: wax_<id>_<secret>. The id selects
// the row; only an HMAC of the secret is stored.
const apiKeyPrefix = "wax_"

// CreateTenant registers a tenant (ErrConflict if the id exists).
func (s *Store) CreateTenant(ctx context.Context, t *Tenant) error {
	err := s.db.QueryRow(ctx, `INSERT INTO wax_tenants (id, name) VALUES ($1, $2) RETURNING created_at`, t.ID, t.Name).Scan(&t.CreatedAt)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("store: create tenant: %w", err)
	}
	return nil
}

// ListTenants returns all tenants ordered by id.
func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, created_at FROM wax_tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		return t, r.Scan(&t.ID, &t.Name, &t.CreatedAt)
	})
}

// DeleteTenant removes a tenant and its API keys. Instances must be
// deleted first (ErrTenantNotEmpty), so no account is dropped by accident.
func (s *Store) DeleteTenant(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *Store) error {
		var n int
		if err := tx.db.QueryRow(ctx, `SELECT count(*) FROM wax_instances WHERE tenant_id = $1`, id).Scan(&n); err != nil {
			return fmt.Errorf("store: delete tenant: %w", err)
		}
		if n > 0 {
			return ErrTenantNotEmpty
		}
		tag, err := tx.db.Exec(ctx, `DELETE FROM wax_tenants WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("store: delete tenant: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CreateAPIKey issues a key for a tenant and returns it in plaintext; this
// is the only time the secret is available.
func (s *Store) CreateAPIKey(ctx context.Context, tenantID string) (token string, key APIKey, err error) {
	var id [8]byte
	var secret [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", key, err
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return "", key, err
	}
	key.ID = hex.EncodeToString(id[:])
	err = s.db.QueryRow(ctx, `INSERT INTO wax_api_keys (id, tenant_id, mac) VALUES ($1, $2, $3) RETURNING created_at`,
		key.ID, tenantID, s.keyMAC(key.ID, secret[:])).Scan(&key.CreatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return "", key, ErrNotFound
		}
		return "", key, fmt.Errorf("store: create api key: %w", err)
	}
	return apiKeyPrefix + key.ID + "_" + base64.RawURLEncoding.EncodeToString(secret[:]), key, nil
}

// ListAPIKeys returns a tenant's keys (without secrets).
func (s *Store) ListAPIKeys(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := s.db.Query(ctx, `SELECT id, created_at FROM wax_api_keys WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		return k, r.Scan(&k.ID, &k.CreatedAt)
	})
}

// DeleteAPIKey revokes a key; it stops working immediately.
func (s *Store) DeleteAPIKey(ctx context.Context, tenantID, keyID string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM wax_api_keys WHERE tenant_id = $1 AND id = $2`, tenantID, keyID)
	if err != nil {
		return fmt.Errorf("store: delete api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// VerifyAPIKey returns the tenant of a valid key, or ErrInvalidAPIKey.
// Keys are checked against the database on every call, so a revoked key
// stops working at once.
func (s *Store) VerifyAPIKey(ctx context.Context, token string) (string, error) {
	rest, ok := strings.CutPrefix(token, apiKeyPrefix)
	id, enc, ok2 := strings.Cut(rest, "_")
	secret, err := base64.RawURLEncoding.DecodeString(enc)
	if !ok || !ok2 || err != nil || len(secret) != 32 || len(id) != 16 {
		return "", ErrInvalidAPIKey
	}
	var tenant string
	var mac []byte
	err = s.db.QueryRow(ctx, `SELECT tenant_id, mac FROM wax_api_keys WHERE id = $1`, id).Scan(&tenant, &mac)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidAPIKey
	}
	if err != nil {
		return "", fmt.Errorf("store: verify api key: %w", err)
	}
	if !hmac.Equal(mac, s.keyMAC(id, secret)) {
		return "", ErrInvalidAPIKey
	}
	return tenant, nil
}

// keyMAC binds the secret to its key id, so rows cannot be swapped.
func (s *Store) keyMAC(id string, secret []byte) []byte {
	return cryptoutil.HMACSHA256(s.sealer.keyMAC, []byte(id), secret)
}
