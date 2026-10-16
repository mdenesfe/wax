package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mdenesfe/wax/internal/keys"
	"github.com/mdenesfe/wax/internal/signal"
)

const sessTable = "wax_sessions"

// LoadSession returns the Signal record for address, or an empty record if
// none exists. Inside a transaction the row is locked (FOR UPDATE) until
// commit, so concurrent decrypt/encrypt on the same session serialize.
func (s *Store) LoadSession(ctx context.Context, instanceID, address string) (*signal.Record, error) {
	return queryRow(ctx, s, s.sessionQuery(instanceID, address))
}

func (s *Store) sessionQuery(instanceID, address string) rowQuery[*signal.Record] {
	q := `SELECT record FROM wax_sessions WHERE instance_id = $1 AND address = $2`
	if s.inTx {
		q += ` FOR UPDATE`
	}
	return rowQuery[*signal.Record]{sql: q, args: []any{instanceID, address}, scan: func(row pgx.Row) (*signal.Record, error) {
		var enc []byte
		err := row.Scan(&enc)
		if errors.Is(err, pgx.ErrNoRows) {
			return &signal.Record{}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("store: load session %s: %w", address, err)
		}
		raw, err := s.sealer.open(enc, adFor(sessTable, "record", instanceID, address))
		if err != nil {
			return nil, err
		}
		return signal.UnmarshalRecord(raw)
	}}
}

// PutSession stores a Signal record.
func (s *Store) PutSession(ctx context.Context, instanceID, address string, r *signal.Record) error {
	raw, err := r.Marshal()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO wax_sessions (instance_id, address, record) VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, address) DO UPDATE SET record = excluded.record, updated_at = now()`,
		instanceID, address, s.sealer.seal(raw, adFor(sessTable, "record", instanceID, address)))
	if err != nil {
		return fmt.Errorf("store: put session %s: %w", address, err)
	}
	return nil
}

// HasSessions reports which of the addresses have a usable session.
func (s *Store) HasSessions(ctx context.Context, instanceID string, addresses []string) (map[string]bool, error) {
	out := make(map[string]bool, len(addresses))
	for _, a := range addresses {
		out[a] = false
	}
	rows, err := s.db.Query(ctx, `SELECT address FROM wax_sessions WHERE instance_id = $1 AND address = ANY($2)`, instanceID, addresses)
	if err != nil {
		return nil, fmt.Errorf("store: has sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}

// DeleteUserSessions removes all sessions and identities of one user (all
// devices), e.g. after the user re-registered and changed identity.
func (s *Store) DeleteUserSessions(ctx context.Context, instanceID, addressName string) error {
	prefix := escapeLike(addressName) + `.%`
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_sessions WHERE instance_id = $1 AND address LIKE $2`, instanceID, prefix); err != nil {
		return fmt.Errorf("store: delete sessions: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_identities WHERE instance_id = $1 AND address LIKE $2`, instanceID, prefix); err != nil {
		return fmt.Errorf("store: delete identities: %w", err)
	}
	return nil
}

// MigrateSessions renames sessions/identities stored under the phone-number
// address name to the LID address name, once the mapping is known. Rows that
// already exist under the LID name win (they are the newer ones).
func (s *Store) MigrateSessions(ctx context.Context, instanceID, fromName, toName string) error {
	if fromName == toName {
		return nil
	}
	return s.InTx(ctx, func(tx *Store) error {
		rows, err := tx.db.Query(ctx, `SELECT address, record FROM wax_sessions WHERE instance_id = $1 AND address LIKE $2`,
			instanceID, escapeLike(fromName)+`.%`)
		if err != nil {
			return err
		}
		type row struct {
			addr string
			rec  []byte
		}
		var moved []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.addr, &r.rec); err != nil {
				rows.Close()
				return err
			}
			moved = append(moved, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range moved {
			newAddr := toName + strings.TrimPrefix(r.addr, fromName)
			// The sealed record is bound to its address, so re-seal it.
			raw, err := tx.sealer.open(r.rec, adFor(sessTable, "record", instanceID, r.addr))
			if err != nil {
				return err
			}
			if _, err := tx.db.Exec(ctx, `INSERT INTO wax_sessions (instance_id, address, record) VALUES ($1, $2, $3)
				ON CONFLICT (instance_id, address) DO NOTHING`,
				instanceID, newAddr, tx.sealer.seal(raw, adFor(sessTable, "record", instanceID, newAddr))); err != nil {
				return err
			}
			if _, err := tx.db.Exec(ctx, `DELETE FROM wax_sessions WHERE instance_id = $1 AND address = $2`, instanceID, r.addr); err != nil {
				return err
			}
		}
		_, err = tx.db.Exec(ctx, `UPDATE wax_identities SET address = $3 || substr(address, length($2) + 1), updated_at = now()
			WHERE instance_id = $1 AND address LIKE $4
			AND NOT EXISTS (SELECT 1 FROM wax_identities o WHERE o.instance_id = $1 AND o.address = $3 || substr(wax_identities.address, length($2) + 1))`,
			instanceID, fromName, toName, escapeLike(fromName)+`.%`)
		return err
	})
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// PreKeyStore adapts the store (usually a transaction) to signal.PreKeyStore.
type PreKeyStore struct {
	S      *Store
	Ctx    context.Context
	Device *Device
}

// SignedPreKey implements signal.PreKeyStore.
func (p PreKeyStore) SignedPreKey(id uint32) (*keys.KeyPair, error) {
	if id != p.Device.SignedPreKey.ID {
		return nil, fmt.Errorf("%w: signed prekey %d (ours is %d)", signal.ErrMissingPreKey, id, p.Device.SignedPreKey.ID)
	}
	return p.Device.SignedPreKey.Key, nil
}

// PreKey implements signal.PreKeyStore.
func (p PreKeyStore) PreKey(id uint32) (*keys.KeyPair, error) {
	pk, err := p.S.GetPreKey(p.Ctx, p.Device.InstanceID, id)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: %d", signal.ErrMissingPreKey, id)
	}
	if err != nil {
		return nil, err
	}
	return pk.Key, nil
}

// --- LID mapping --------------------------------------------------------------

// PutLIDMapping records that phone-number user pn is LID user lid.
func (s *Store) PutLIDMapping(ctx context.Context, instanceID, pnUser, lidUser string) (changed bool, err error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO wax_lid_map (instance_id, pn_user, lid_user) VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, pn_user) DO UPDATE SET lid_user = excluded.lid_user, updated_at = now()
		WHERE wax_lid_map.lid_user <> excluded.lid_user`, instanceID, pnUser, lidUser)
	if err != nil {
		return false, fmt.Errorf("store: put lid mapping: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// LIDForPN returns the LID user of a phone-number user ("" if unknown).
func (s *Store) LIDForPN(ctx context.Context, instanceID, pnUser string) (string, error) {
	var lid string
	err := s.db.QueryRow(ctx, `SELECT lid_user FROM wax_lid_map WHERE instance_id = $1 AND pn_user = $2`, instanceID, pnUser).Scan(&lid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: lid for pn: %w", err)
	}
	return lid, nil
}

// PNForLID returns the phone-number user of a LID user ("" if unknown).
func (s *Store) PNForLID(ctx context.Context, instanceID, lidUser string) (string, error) {
	var pn string
	err := s.db.QueryRow(ctx, `SELECT pn_user FROM wax_lid_map WHERE instance_id = $1 AND lid_user = $2 ORDER BY updated_at DESC LIMIT 1`, instanceID, lidUser).Scan(&pn)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: pn for lid: %w", err)
	}
	return pn, nil
}

// --- event buffer -------------------------------------------------------------

const bufTable = "wax_event_buffer"

// BufferedEvent is a previously decrypted payload.
type BufferedEvent struct {
	Plaintext []byte // nil if already handled
	Inserted  time.Time
}

// GetBufferedEvent looks up a ciphertext hash. It returns nil if unseen.
func (s *Store) GetBufferedEvent(ctx context.Context, instanceID string, hash []byte) (*BufferedEvent, error) {
	return queryRow(ctx, s, s.bufferedEventQuery(instanceID, hash))
}

func (s *Store) bufferedEventQuery(instanceID string, hash []byte) rowQuery[*BufferedEvent] {
	return rowQuery[*BufferedEvent]{
		sql:  `SELECT plaintext, inserted_at FROM wax_event_buffer WHERE instance_id = $1 AND hash = $2`,
		args: []any{instanceID, hash},
		scan: func(row pgx.Row) (*BufferedEvent, error) {
			var enc []byte
			var ev BufferedEvent
			err := row.Scan(&enc, &ev.Inserted)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, fmt.Errorf("store: get buffered event: %w", err)
			}
			if enc != nil {
				if ev.Plaintext, err = s.sealer.open(enc, adFor(bufTable, "plaintext", instanceID, fmt.Sprintf("%x", hash))); err != nil {
					return nil, err
				}
			}
			return &ev, nil
		},
	}
}

// DecryptState is what decrypting one message needs from the database.
type DecryptState struct {
	Buffered *BufferedEvent // nil if this ciphertext was never seen
	Session  *signal.Record // empty record if there is none
	Identity *[32]byte      // nil if the peer's identity is unknown
}

// LoadDecryptState reads the buffered event, session and stored identity
// for one incoming message in a single round trip. Inside a transaction
// the session row is locked (FOR UPDATE), as with LoadSession.
func (s *Store) LoadDecryptState(ctx context.Context, instanceID, address string, hash []byte) (*DecryptState, error) {
	bq, sq, iq := s.bufferedEventQuery(instanceID, hash), s.sessionQuery(instanceID, address), s.identityQuery(instanceID, address)
	var b pgx.Batch
	b.Queue(bq.sql, bq.args...)
	b.Queue(sq.sql, sq.args...)
	b.Queue(iq.sql, iq.args...)
	br := s.db.SendBatch(ctx, &b)
	defer br.Close()
	var st DecryptState
	var err error
	if st.Buffered, err = bq.scan(br.QueryRow()); err != nil {
		return nil, err
	}
	if st.Session, err = sq.scan(br.QueryRow()); err != nil {
		return nil, err
	}
	switch id, err := iq.scan(br.QueryRow()); {
	case err == nil:
		st.Identity = &id
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}
	return &st, br.Close()
}

// PutBufferedEvent stores a decrypted payload under its ciphertext hash.
func (s *Store) PutBufferedEvent(ctx context.Context, instanceID string, hash, plaintext []byte, serverTS time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_event_buffer (instance_id, hash, plaintext, server_ts) VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance_id, hash) DO NOTHING`,
		instanceID, hash, s.sealer.seal(plaintext, adFor(bufTable, "plaintext", instanceID, fmt.Sprintf("%x", hash))), serverTS)
	if err != nil {
		return fmt.Errorf("store: put buffered event: %w", err)
	}
	return nil
}

// MarkEventHandled drops the stored plaintext, keeping the hash for dedupe.
func (s *Store) MarkEventHandled(ctx context.Context, instanceID string, hash []byte) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_event_buffer SET plaintext = NULL WHERE instance_id = $1 AND hash = $2`, instanceID, hash)
	if err != nil {
		return fmt.Errorf("store: mark event handled: %w", err)
	}
	return nil
}

// PurgeOld removes dedupe hashes and sent-message copies older than the
// given ages. Unhandled buffered events are kept regardless of age.
func (s *Store) PurgeOld(ctx context.Context, instanceID string, eventAge, sentAge time.Duration) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_event_buffer WHERE instance_id = $1 AND plaintext IS NULL AND inserted_at < $2`,
		instanceID, time.Now().Add(-eventAge)); err != nil {
		return fmt.Errorf("store: purge event buffer: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_sent_messages WHERE instance_id = $1 AND created_at < $2`,
		instanceID, time.Now().Add(-sentAge)); err != nil {
		return fmt.Errorf("store: purge sent messages: %w", err)
	}
	return nil
}

// --- sent messages ------------------------------------------------------------

const sentTable = "wax_sent_messages"

// PutSentMessage keeps a copy of an outgoing message for retry handling.
func (s *Store) PutSentMessage(ctx context.Context, instanceID, chat, id string, plaintext []byte) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_sent_messages (instance_id, chat, message_id, plaintext) VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance_id, chat, message_id) DO UPDATE SET plaintext = excluded.plaintext`,
		instanceID, chat, id, s.sealer.seal(plaintext, adFor(sentTable, "plaintext", instanceID, chat, id)))
	if err != nil {
		return fmt.Errorf("store: put sent message: %w", err)
	}
	return nil
}

// GetSentMessage returns a stored outgoing message, or ErrNotFound.
func (s *Store) GetSentMessage(ctx context.Context, instanceID, chat, id string) ([]byte, error) {
	var enc []byte
	err := s.db.QueryRow(ctx, `SELECT plaintext FROM wax_sent_messages WHERE instance_id = $1 AND chat = $2 AND message_id = $3`,
		instanceID, chat, id).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get sent message: %w", err)
	}
	return s.sealer.open(enc, adFor(sentTable, "plaintext", instanceID, chat, id))
}
