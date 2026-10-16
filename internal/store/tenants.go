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

// Tenant owns instances and API keys. Zero limits mean "no quota" and
// "the server's default API rate".
type Tenant struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	MaxInstances      int       `json:"max_instances"`
	RequestsPerSecond int       `json:"requests_per_second"`
	CreatedAt         time.Time `json:"created_at"`
}

// TenantAuth is what a verified key yields: the tenant and its API rate.
type TenantAuth struct {
	KeyID             string // the key's public id, for logs
	TenantID          string
	RequestsPerSecond int
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
	ErrQuotaExceeded  = errors.New("store: instance quota reached")
)

// apiKeyPrefix starts every tenant key: wax_<id>_<secret>. The id selects
// the row; only an HMAC of the secret is stored.
const apiKeyPrefix = "wax_"

// CreateTenant registers a tenant (ErrConflict if the id exists).
func (s *Store) CreateTenant(ctx context.Context, t *Tenant) error {
	err := s.db.QueryRow(ctx, `INSERT INTO wax_tenants (id, name, max_instances, requests_per_second) VALUES ($1, $2, $3, $4) RETURNING created_at`,
		t.ID, t.Name, t.MaxInstances, t.RequestsPerSecond).Scan(&t.CreatedAt)
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
	rows, err := s.db.Query(ctx, `SELECT id, name, max_instances, requests_per_second, created_at FROM wax_tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		return t, r.Scan(&t.ID, &t.Name, &t.MaxInstances, &t.RequestsPerSecond, &t.CreatedAt)
	})
}

// UpdateTenant changes a tenant's name and limits. Lowering the instance
// quota below the current count only blocks new instances.
func (s *Store) UpdateTenant(ctx context.Context, t *Tenant) error {
	err := s.db.QueryRow(ctx, `UPDATE wax_tenants SET name = $2, max_instances = $3, requests_per_second = $4 WHERE id = $1 RETURNING created_at`,
		t.ID, t.Name, t.MaxInstances, t.RequestsPerSecond).Scan(&t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: update tenant: %w", err)
	}
	return nil
}

// GetTenant returns one tenant.
func (s *Store) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	t := Tenant{ID: id}
	err := s.db.QueryRow(ctx, `SELECT name, max_instances, requests_per_second, created_at FROM wax_tenants WHERE id = $1`, id).
		Scan(&t.Name, &t.MaxInstances, &t.RequestsPerSecond, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get tenant: %w", err)
	}
	return &t, nil
}

// DeleteTenant removes a tenant and its API keys. Instances must be
// deleted first (ErrTenantNotEmpty), so no account is dropped by accident.
func (s *Store) DeleteTenant(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *Store) error {
		// Lock the tenant first (as CreateInstance does), then count in a
		// separate statement, so a concurrent create is either counted or
		// waits and then fails on the deleted tenant.
		if _, err := tx.db.Exec(ctx, `SELECT 1 FROM wax_tenants WHERE id = $1 FOR UPDATE`, id); err != nil {
			return fmt.Errorf("store: delete tenant: %w", err)
		}
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

// VerifyAPIKey returns the tenant of a valid key (with its API rate, read
// in the same query), or ErrInvalidAPIKey. Keys are checked against the
// database on every call, so a revoked key stops working at once.
func (s *Store) VerifyAPIKey(ctx context.Context, token string) (TenantAuth, error) {
	rest, ok := strings.CutPrefix(token, apiKeyPrefix)
	id, enc, ok2 := strings.Cut(rest, "_")
	secret, err := base64.RawURLEncoding.DecodeString(enc)
	if !ok || !ok2 || err != nil || len(secret) != 32 || len(id) != 16 {
		return TenantAuth{}, ErrInvalidAPIKey
	}
	a := TenantAuth{KeyID: id}
	var mac []byte
	err = s.db.QueryRow(ctx, `SELECT k.tenant_id, k.mac, t.requests_per_second
		FROM wax_api_keys k JOIN wax_tenants t ON t.id = k.tenant_id WHERE k.id = $1`, id).Scan(&a.TenantID, &mac, &a.RequestsPerSecond)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantAuth{}, ErrInvalidAPIKey
	}
	if err != nil {
		return TenantAuth{}, fmt.Errorf("store: verify api key: %w", err)
	}
	if !hmac.Equal(mac, s.keyMAC(id, secret)) {
		return TenantAuth{}, ErrInvalidAPIKey
	}
	return a, nil
}

// NodeMAC authenticates data exchanged between nodes (forwarded
// requests) with a key derived from the shared master key.
func (s *Store) NodeMAC(parts ...[]byte) []byte {
	return cryptoutil.HMACSHA256(s.sealer.nodeMAC, parts...)
}

// keyMAC binds the secret to its key id, so rows cannot be swapped.
func (s *Store) keyMAC(id string, secret []byte) []byte {
	return cryptoutil.HMACSHA256(s.sealer.keyMAC, []byte(id), secret)
}
