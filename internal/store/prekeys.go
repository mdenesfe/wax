package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mdenesfe/wax/internal/keys"
)

// maxPreKeyID is the largest prekey id: ids travel as 3 bytes on the wire.
const maxPreKeyID = 1<<24 - 1

// PreKey is a one-time prekey.
type PreKey struct {
	ID  uint32
	Key *keys.KeyPair
}

const preKeyTable = "wax_prekeys"

// GetOrGenPreKeys returns count prekeys that have not been uploaded yet,
// generating and storing new ones as needed. Generation is atomic: the id
// counter in wax_devices is row-locked, so concurrent callers never hand out
// the same id.
func (s *Store) GetOrGenPreKeys(ctx context.Context, instanceID string, count int) ([]PreKey, error) {
	var out []PreKey
	err := s.InTx(ctx, func(tx *Store) error {
		var nextID int64
		err := tx.db.QueryRow(ctx, `SELECT next_prekey_id FROM wax_devices WHERE instance_id = $1 FOR UPDATE`, instanceID).Scan(&nextID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.db.Query(ctx, `SELECT key_id, private_key FROM wax_prekeys
			WHERE instance_id = $1 AND NOT uploaded ORDER BY key_id LIMIT $2`, instanceID, count)
		if err != nil {
			return err
		}
		out, err = tx.scanPreKeys(rows, instanceID)
		if err != nil {
			return err
		}
		if len(out) >= count {
			return nil
		}
		batch := &pgx.Batch{}
		for len(out) < count {
			kp, err := keys.NewKeyPair()
			if err != nil {
				return err
			}
			id := uint32(nextID)
			// Wrap around within the 24-bit id space, skipping 0. By the
			// time ids wrap, the old ones were consumed long ago; a stale
			// row with the same id is replaced.
			nextID = nextID%maxPreKeyID + 1
			batch.Queue(`INSERT INTO wax_prekeys (instance_id, key_id, private_key) VALUES ($1, $2, $3)
				ON CONFLICT (instance_id, key_id) DO UPDATE SET private_key = excluded.private_key, uploaded = false, created_at = now()`,
				instanceID, int64(id), tx.sealer.seal(kp.Priv[:], adFor(preKeyTable, "private_key", instanceID, id)))
			out = append(out, PreKey{ID: id, Key: kp})
		}
		batch.Queue(`UPDATE wax_devices SET next_prekey_id = $2, updated_at = now() WHERE instance_id = $1`, instanceID, nextID)
		return tx.db.SendBatch(ctx, batch).Close()
	})
	if err != nil {
		return nil, fmt.Errorf("store: get or generate prekeys: %w", err)
	}
	return out, nil
}

func (s *Store) scanPreKeys(rows pgx.Rows, instanceID string) ([]PreKey, error) {
	defer rows.Close()
	var out []PreKey
	for rows.Next() {
		var id int64
		var enc []byte
		if err := rows.Scan(&id, &enc); err != nil {
			return nil, err
		}
		priv, err := s.sealer.open(enc, adFor(preKeyTable, "private_key", instanceID, uint32(id)))
		if err != nil {
			return nil, err
		}
		kp, err := keys.KeyPairFromPrivate(priv)
		if err != nil {
			return nil, err
		}
		out = append(out, PreKey{ID: uint32(id), Key: kp})
	}
	return out, rows.Err()
}

// MarkPreKeysUploaded flags the given prekeys as present on the server.
func (s *Store) MarkPreKeysUploaded(ctx context.Context, instanceID string, ids []uint32) error {
	ids64 := make([]int64, len(ids))
	for i, id := range ids {
		ids64[i] = int64(id)
	}
	_, err := s.db.Exec(ctx, `UPDATE wax_prekeys SET uploaded = true WHERE instance_id = $1 AND key_id = ANY($2)`, instanceID, ids64)
	if err != nil {
		return fmt.Errorf("store: mark prekeys uploaded: %w", err)
	}
	return nil
}

// UploadedPreKeyCount returns how many uploaded, unconsumed prekeys we hold.
func (s *Store) UploadedPreKeyCount(ctx context.Context, instanceID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM wax_prekeys WHERE instance_id = $1 AND uploaded`, instanceID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count prekeys: %w", err)
	}
	return n, nil
}

// GetPreKey loads one prekey by id (used when a peer starts a session with it).
func (s *Store) GetPreKey(ctx context.Context, instanceID string, id uint32) (*PreKey, error) {
	rows, err := s.db.Query(ctx, `SELECT key_id, private_key FROM wax_prekeys WHERE instance_id = $1 AND key_id = $2`, instanceID, int64(id))
	if err != nil {
		return nil, fmt.Errorf("store: get prekey: %w", err)
	}
	keys, err := s.scanPreKeys(rows, instanceID)
	if err != nil {
		return nil, fmt.Errorf("store: get prekey: %w", err)
	}
	if len(keys) == 0 {
		return nil, ErrNotFound
	}
	return &keys[0], nil
}

// RemovePreKey deletes a consumed one-time prekey.
func (s *Store) RemovePreKey(ctx context.Context, instanceID string, id uint32) error {
	_, err := s.db.Exec(ctx, `DELETE FROM wax_prekeys WHERE instance_id = $1 AND key_id = $2`, instanceID, int64(id))
	if err != nil {
		return fmt.Errorf("store: remove prekey: %w", err)
	}
	return nil
}
