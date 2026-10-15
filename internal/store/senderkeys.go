package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mdenesfe/wax/internal/signal"
)

const skTable = "wax_sender_keys"

// LoadSenderKey returns the sender key record for (group, sender), or an
// empty record. Inside a transaction the row is locked.
func (s *Store) LoadSenderKey(ctx context.Context, instanceID, group, sender string) (*signal.SenderKeyRecord, error) {
	q := `SELECT record FROM wax_sender_keys WHERE instance_id = $1 AND group_jid = $2 AND sender = $3`
	if s.inTx {
		q += ` FOR UPDATE`
	}
	var enc []byte
	err := s.db.QueryRow(ctx, q, instanceID, group, sender).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return &signal.SenderKeyRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: load sender key: %w", err)
	}
	raw, err := s.sealer.open(enc, adFor(skTable, "record", instanceID, group, sender))
	if err != nil {
		return nil, err
	}
	return signal.UnmarshalSenderKeyRecord(raw)
}

// PutSenderKey stores a sender key record.
func (s *Store) PutSenderKey(ctx context.Context, instanceID, group, sender string, r *signal.SenderKeyRecord) error {
	raw, err := r.Marshal()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO wax_sender_keys (instance_id, group_jid, sender, record) VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance_id, group_jid, sender) DO UPDATE SET record = excluded.record, updated_at = now()`,
		instanceID, group, sender, s.sealer.seal(raw, adFor(skTable, "record", instanceID, group, sender)))
	if err != nil {
		return fmt.Errorf("store: put sender key: %w", err)
	}
	return nil
}

// SenderKeyRecipients returns which of the devices already have our
// sender key for the group.
func (s *Store) SenderKeyRecipients(ctx context.Context, instanceID, group string, devices []string) (map[string]bool, error) {
	out := make(map[string]bool, len(devices))
	rows, err := s.db.Query(ctx, `SELECT device FROM wax_sender_key_sent WHERE instance_id = $1 AND group_jid = $2 AND device = ANY($3)`,
		instanceID, group, devices)
	if err != nil {
		return nil, fmt.Errorf("store: sender key recipients: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out[d] = true
	}
	return out, rows.Err()
}

// MarkSenderKeySent records that devices received our sender key.
func (s *Store) MarkSenderKeySent(ctx context.Context, instanceID, group string, devices []string) error {
	if len(devices) == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `INSERT INTO wax_sender_key_sent (instance_id, group_jid, device)
		SELECT $1, $2, unnest($3::text[]) ON CONFLICT DO NOTHING`, instanceID, group, devices)
	if err != nil {
		return fmt.Errorf("store: mark sender key sent: %w", err)
	}
	return nil
}

// ForgetSenderKeyRecipients drops the delivery tracking of one device (it
// asked for a resend, so it evidently lost our key) or of all devices.
func (s *Store) ForgetSenderKeyRecipients(ctx context.Context, instanceID, group string, device string) error {
	q := `DELETE FROM wax_sender_key_sent WHERE instance_id = $1 AND group_jid = $2`
	args := []any{instanceID, group}
	if device != "" {
		q += ` AND device = $3`
		args = append(args, device)
	}
	if _, err := s.db.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("store: forget sender key recipients: %w", err)
	}
	return nil
}

// DeleteSenderKey removes the sender key record of (group, sender).
func (s *Store) DeleteSenderKey(ctx context.Context, instanceID, group, sender string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM wax_sender_keys WHERE instance_id = $1 AND group_jid = $2 AND sender = $3`, instanceID, group, sender)
	if err != nil {
		return fmt.Errorf("store: delete sender key: %w", err)
	}
	return nil
}
