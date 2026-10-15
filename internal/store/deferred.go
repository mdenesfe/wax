package store

import (
	"context"
	"fmt"
	"strconv"
)

const deferredTable = "wax_deferred_stanzas"

// DeferredStanza is a stored, not yet processed stanza.
type DeferredStanza struct {
	ID       int64
	Tag      string
	StanzaID string
	Reason   string
	Raw      []byte // binary-encoded node (binary.Marshal output)
}

// PutDeferred stores a stanza for later processing. Storing the same stanza
// twice (redelivery) is a no-op.
func (s *Store) PutDeferred(ctx context.Context, instanceID, tag, stanzaID, reason string, raw []byte) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_deferred_stanzas (instance_id, tag, stanza_id, reason, raw)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (instance_id, tag, stanza_id) DO NOTHING`,
		instanceID, tag, stanzaID, reason, s.sealer.seal(raw, adFor(deferredTable, "raw", instanceID, tag, stanzaID)))
	if err != nil {
		return fmt.Errorf("store: put deferred stanza: %w", err)
	}
	return nil
}

// ListDeferred returns up to limit deferred stanzas, oldest first.
func (s *Store) ListDeferred(ctx context.Context, instanceID string, limit int) ([]DeferredStanza, error) {
	rows, err := s.db.Query(ctx, `SELECT id, tag, stanza_id, reason, raw FROM wax_deferred_stanzas
		WHERE instance_id = $1 ORDER BY id LIMIT $2`, instanceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list deferred: %w", err)
	}
	defer rows.Close()
	var out []DeferredStanza
	for rows.Next() {
		var d DeferredStanza
		var enc []byte
		if err := rows.Scan(&d.ID, &d.Tag, &d.StanzaID, &d.Reason, &enc); err != nil {
			return nil, err
		}
		if d.Raw, err = s.sealer.open(enc, adFor(deferredTable, "raw", instanceID, d.Tag, d.StanzaID)); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDeferred removes a processed stanza.
func (s *Store) DeleteDeferred(ctx context.Context, instanceID string, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM wax_deferred_stanzas WHERE instance_id = $1 AND id = $2`, instanceID, id)
	if err != nil {
		return fmt.Errorf("store: delete deferred %s: %w", strconv.FormatInt(id, 10), err)
	}
	return nil
}

// CountDeferred returns how many stanzas wait for processing.
func (s *Store) CountDeferred(ctx context.Context, instanceID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM wax_deferred_stanzas WHERE instance_id = $1`, instanceID).Scan(&n)
	return n, err
}
