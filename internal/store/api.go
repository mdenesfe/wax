package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrConflict is returned when a unique key already exists.
var ErrConflict = errors.New("store: already exists")

func isUniqueViolation(err error) bool { return pgCode(err) == "23505" }

func isForeignKeyViolation(err error) bool { return pgCode(err) == "23503" }

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// --- instances ------------------------------------------------------------------

const instTable = "wax_instances"

// Instance is an instance's configuration. ID is the internal key used by
// every table; Name is the id a tenant sees, unique within the tenant.
type Instance struct {
	ID            string
	TenantID      string
	Name          string
	WebhookURL    string
	WebhookSecret []byte
	RatePerMinute int
	CreatedAt     time.Time
}

// CreateInstance registers a new instance (ErrConflict if the id exists).
func (s *Store) CreateInstance(ctx context.Context, in *Instance) error {
	var secret any
	if len(in.WebhookSecret) > 0 {
		secret = s.sealer.seal(in.WebhookSecret, adFor(instTable, "webhook_secret", in.ID))
	}
	if in.RatePerMinute <= 0 {
		in.RatePerMinute = 20
	}
	err := s.InTx(ctx, func(tx *Store) error {
		// Locking the tenant row serializes concurrent creates. The count
		// must be a separate statement: under READ COMMITTED a statement
		// sees the snapshot taken when it started, so a count in the locking
		// statement would miss rows committed while it waited for the lock.
		var quota, count int
		err := tx.db.QueryRow(ctx, `SELECT max_instances FROM wax_tenants WHERE id = $1 FOR UPDATE`, in.TenantID).Scan(&quota)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound // unknown tenant
		}
		if err != nil {
			return err
		}
		if quota > 0 {
			if err := tx.db.QueryRow(ctx, `SELECT count(*) FROM wax_instances WHERE tenant_id = $1`, in.TenantID).Scan(&count); err != nil {
				return err
			}
			if count >= quota {
				return ErrQuotaExceeded
			}
		}
		return tx.db.QueryRow(ctx, `INSERT INTO wax_instances (id, tenant_id, name, webhook_url, webhook_secret, rate_per_minute)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
			in.ID, in.TenantID, in.Name, in.WebhookURL, secret, in.RatePerMinute).Scan(&in.CreatedAt)
	})
	switch {
	case isUniqueViolation(err):
		return ErrConflict
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrQuotaExceeded):
		return err
	case err != nil:
		return fmt.Errorf("store: create instance: %w", err)
	}
	return nil
}

// UpdateInstance changes webhook and rate settings.
func (s *Store) UpdateInstance(ctx context.Context, in *Instance) error {
	var secret any
	if len(in.WebhookSecret) > 0 {
		secret = s.sealer.seal(in.WebhookSecret, adFor(instTable, "webhook_secret", in.ID))
	}
	tag, err := s.db.Exec(ctx, `UPDATE wax_instances SET webhook_url = $2, webhook_secret = $3, rate_per_minute = $4 WHERE id = $1`,
		in.ID, in.WebhookURL, secret, in.RatePerMinute)
	if err != nil {
		return fmt.Errorf("store: update instance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListInstances returns all instances.
func (s *Store) ListInstances(ctx context.Context) ([]Instance, error) {
	return s.instancesWhere(ctx, `true`)
}

// InstancesByID returns the configurations of the given instances.
func (s *Store) InstancesByID(ctx context.Context, ids []string) ([]Instance, error) {
	return s.instancesWhere(ctx, `id = ANY($1)`, ids)
}

// InstanceByName returns a tenant's instance by its API name.
func (s *Store) InstanceByName(ctx context.Context, tenant, name string) (*Instance, error) {
	list, err := s.instancesWhere(ctx, `tenant_id = $1 AND name = $2`, tenant, name)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

func (s *Store) instancesWhere(ctx context.Context, where string, args ...any) ([]Instance, error) {
	rows, err := s.db.Query(ctx, `SELECT id, tenant_id, name, webhook_url, webhook_secret, rate_per_minute, created_at
		FROM wax_instances WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list instances: %w", err)
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		var in Instance
		var enc []byte
		if err := rows.Scan(&in.ID, &in.TenantID, &in.Name, &in.WebhookURL, &enc, &in.RatePerMinute, &in.CreatedAt); err != nil {
			return nil, err
		}
		if enc != nil {
			if in.WebhookSecret, err = s.sealer.open(enc, adFor(instTable, "webhook_secret", in.ID)); err != nil {
				return nil, err
			}
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// DeleteInstance removes an instance and (via cascades) its device,
// sessions, keys, messages and queues.
func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	return s.InTx(ctx, func(tx *Store) error {
		if err := tx.DeleteDevice(ctx, id); err != nil {
			return err
		}
		tag, err := tx.db.Exec(ctx, `DELETE FROM wax_instances WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("store: delete instance: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// --- outbox -------------------------------------------------------------------------

const outboxTable = "wax_outbox"

// OutboxItem is a queued outgoing message.
type OutboxItem struct {
	ID        int64
	Instance  string
	ClientID  string
	Chat      string
	MessageID string
	Kind      string
	Payload   []byte
	Status    string
	Attempts  int
	LastError string
	ServerTS  *time.Time
	CreatedAt time.Time
}

// Enqueue adds a message to the outbox. With a client id already used,
// the existing item is returned and created is false.
func (s *Store) Enqueue(ctx context.Context, it *OutboxItem) (created bool, err error) {
	var cid any
	if it.ClientID != "" {
		cid = it.ClientID
	}
	err = s.InTx(ctx, func(tx *Store) error {
		if it.ClientID != "" {
			existing, err := tx.outboxBy(ctx, `instance_id = $1 AND client_id = $2`, it.Instance, it.ClientID)
			if err == nil {
				*it = *existing
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if err := tx.db.QueryRow(ctx, `INSERT INTO wax_outbox (instance_id, client_id, chat, message_id, payload, kind)
			VALUES ($1, $2, $3, $4, '', $5) RETURNING id, status, created_at`,
			it.Instance, cid, it.Chat, it.MessageID, it.Kind).Scan(&it.ID, &it.Status, &it.CreatedAt); err != nil {
			return err
		}
		created = true
		_, err := tx.db.Exec(ctx, `UPDATE wax_outbox SET payload = $2 WHERE id = $1`, it.ID,
			tx.sealer.seal(it.Payload, adFor(outboxTable, "payload", it.Instance, it.ID)))
		return err
	})
	if isUniqueViolation(err) {
		return false, ErrConflict
	}
	if err != nil {
		return false, fmt.Errorf("store: enqueue: %w", err)
	}
	return created, nil
}

func (s *Store) outboxBy(ctx context.Context, where string, args ...any) (*OutboxItem, error) {
	var it OutboxItem
	var cid *string
	var enc []byte
	err := s.db.QueryRow(ctx, `SELECT id, instance_id, client_id, chat, message_id, kind, payload, status, attempts, last_error, server_ts, created_at
		FROM wax_outbox WHERE `+where, args...).Scan(&it.ID, &it.Instance, &cid, &it.Chat, &it.MessageID, &it.Kind, &enc,
		&it.Status, &it.Attempts, &it.LastError, &it.ServerTS, &it.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: outbox: %w", err)
	}
	if cid != nil {
		it.ClientID = *cid
	}
	if len(enc) > 0 {
		if it.Payload, err = s.sealer.open(enc, adFor(outboxTable, "payload", it.Instance, it.ID)); err != nil {
			return nil, err
		}
	}
	return &it, nil
}

// GetOutbox returns one outbox item.
func (s *Store) GetOutbox(ctx context.Context, instance string, id int64) (*OutboxItem, error) {
	return s.outboxBy(ctx, `instance_id = $1 AND id = $2`, instance, id)
}

// NextOutbox claims the oldest due pending item. The row stays locked
// until the caller's transaction ends (SKIP LOCKED keeps other workers off).
func (s *Store) NextOutbox(ctx context.Context, instance string) (*OutboxItem, error) {
	it, err := s.outboxBy(ctx, `id = (SELECT id FROM wax_outbox WHERE instance_id = $1 AND status = 'pending' AND next_attempt_at <= now()
		ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)`, instance)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return it, err
}

// MarkOutboxSent records a successful send.
func (s *Store) MarkOutboxSent(ctx context.Context, id int64, serverTS time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_outbox SET status = 'sent', attempts = attempts + 1, server_ts = $2, last_error = '', updated_at = now() WHERE id = $1`, id, serverTS)
	return err
}

// MarkOutboxRetry schedules another attempt.
func (s *Store) MarkOutboxRetry(ctx context.Context, id int64, next time.Time, reason string) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_outbox SET attempts = attempts + 1, next_attempt_at = $2, last_error = $3, updated_at = now() WHERE id = $1`, id, next, reason)
	return err
}

// MarkOutboxFailed gives up on an item.
func (s *Store) MarkOutboxFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_outbox SET status = 'failed', attempts = attempts + 1, last_error = $2, updated_at = now() WHERE id = $1`, id, reason)
	return err
}

// NextOutboxDue returns when the next pending item is due (nil if none).
func (s *Store) NextOutboxDue(ctx context.Context, instance string) (*time.Time, error) {
	var t *time.Time
	err := s.db.QueryRow(ctx, `SELECT min(next_attempt_at) FROM wax_outbox WHERE instance_id = $1 AND status = 'pending'`, instance).Scan(&t)
	return t, err
}

// --- webhook queue -----------------------------------------------------------------

const hookTable = "wax_webhook_queue"

// WebhookItem is a queued webhook delivery.
type WebhookItem struct {
	ID       int64
	Instance string
	Event    string
	Payload  []byte
	Attempts int
}

// EnqueueWebhook stores an event for delivery.
func (s *Store) EnqueueWebhook(ctx context.Context, instance, event string, payload []byte) error {
	return s.InTx(ctx, func(tx *Store) error {
		var id int64
		if err := tx.db.QueryRow(ctx, `INSERT INTO wax_webhook_queue (instance_id, event, payload) VALUES ($1, $2, '') RETURNING id`,
			instance, event).Scan(&id); err != nil {
			return fmt.Errorf("store: enqueue webhook: %w", err)
		}
		_, err := tx.db.Exec(ctx, `UPDATE wax_webhook_queue SET payload = $2 WHERE id = $1`, id,
			tx.sealer.seal(payload, adFor(hookTable, "payload", instance, id)))
		return err
	})
}

// NextWebhook returns the oldest pending delivery if it is due. Deliveries
// are strictly ordered: a failing head blocks later events (by design, so
// receivers see events in order).
func (s *Store) NextWebhook(ctx context.Context, instance string) (*WebhookItem, *time.Time, error) {
	var it WebhookItem
	var enc []byte
	var due time.Time
	err := s.db.QueryRow(ctx, `SELECT id, instance_id, event, payload, attempts, next_attempt_at FROM wax_webhook_queue
		WHERE instance_id = $1 AND status = 'pending' ORDER BY id LIMIT 1`, instance).Scan(&it.ID, &it.Instance, &it.Event, &enc, &it.Attempts, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: next webhook: %w", err)
	}
	if due.After(time.Now()) {
		return nil, &due, nil
	}
	if it.Payload, err = s.sealer.open(enc, adFor(hookTable, "payload", instance, it.ID)); err != nil {
		return nil, nil, err
	}
	return &it, nil, nil
}

// MarkWebhook records a delivery result: delivered, retry at next, or dead.
func (s *Store) MarkWebhook(ctx context.Context, id int64, status string, next time.Time, reason string) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_webhook_queue SET status = $2, attempts = attempts + 1, next_attempt_at = $3, last_error = $4 WHERE id = $1`,
		id, status, next, reason)
	return err
}

// PurgeWebhooks deletes delivered rows older than age.
func (s *Store) PurgeWebhooks(ctx context.Context, age time.Duration) error {
	_, err := s.db.Exec(ctx, `DELETE FROM wax_webhook_queue WHERE status <> 'pending' AND created_at < $1`, time.Now().Add(-age))
	return err
}

// WebhookBacklog returns the number of pending deliveries.
func (s *Store) WebhookBacklog(ctx context.Context, instance string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM wax_webhook_queue WHERE instance_id = $1 AND status = 'pending'`, instance).Scan(&n)
	return n, err
}

// --- chats --------------------------------------------------------------------------

// ListChats returns chats by most recent activity.
func (s *Store) ListChats(ctx context.Context, instance string, limit, offset int) ([]Chat, error) {
	rows, err := s.db.Query(ctx, `SELECT `+chatColumns+` FROM wax_chats WHERE instance_id = $1
		ORDER BY last_message_at DESC NULLS LAST, jid LIMIT $2 OFFSET $3`, instance, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: list chats: %w", err)
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetMessage returns one stored message.
func (s *Store) GetMessage(ctx context.Context, instance, chat, id string) (*StoredMessage, error) {
	var m StoredMessage
	var enc []byte
	err := s.db.QueryRow(ctx, `SELECT chat, message_id, sender, from_me, sent_at, push_name, type, content, source
		FROM wax_messages WHERE instance_id = $1 AND chat = $2 AND message_id = $3`, instance, chat, id).Scan(
		&m.Chat, &m.ID, &m.Sender, &m.FromMe, &m.SentAt, &m.PushName, &m.Type, &enc, &m.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get message: %w", err)
	}
	if enc != nil {
		if m.Content, err = s.sealer.open(enc, adFor(msgTable, "content", instance, m.Chat, m.ID)); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// RetargetWebhooks applies a webhook URL change to the pending events of an
// instance: without a URL they are dropped (marked dead), with a new one
// they become due at once instead of waiting for their backoff.
func (s *Store) RetargetWebhooks(ctx context.Context, instance string, removed bool) (int64, error) {
	q := `UPDATE wax_webhook_queue SET next_attempt_at = now() WHERE instance_id = $1 AND status = 'pending'`
	if removed {
		q = `UPDATE wax_webhook_queue SET status = 'dead', last_error = 'webhook url removed' WHERE instance_id = $1 AND status = 'pending'`
	}
	tag, err := s.db.Exec(ctx, q, instance)
	if err != nil {
		return 0, fmt.Errorf("store: retarget webhooks: %w", err)
	}
	return tag.RowsAffected(), nil
}
