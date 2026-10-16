package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mdenesfe/wax/internal/appstate"
)

// --- app state keys -------------------------------------------------------------

const asKeyTable = "wax_appstate_keys"

// PutAppStateKey stores a key shared by the primary device.
func (s *Store) PutAppStateKey(ctx context.Context, instanceID string, keyID, keyData, fingerprint []byte, ts int64) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_appstate_keys (instance_id, key_id, key_data, fingerprint, key_ts) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (instance_id, key_id) DO UPDATE SET key_data = excluded.key_data, fingerprint = excluded.fingerprint, key_ts = excluded.key_ts`,
		instanceID, keyID, s.sealer.seal(keyData, adFor(asKeyTable, "key_data", instanceID, fmt.Sprintf("%x", keyID))), fingerprint, ts)
	if err != nil {
		return fmt.Errorf("store: put app state key: %w", err)
	}
	return nil
}

// AppStateKey returns a key, or ErrNotFound.
func (s *Store) AppStateKey(ctx context.Context, instanceID string, keyID []byte) ([]byte, error) {
	var enc []byte
	err := s.db.QueryRow(ctx, `SELECT key_data FROM wax_appstate_keys WHERE instance_id = $1 AND key_id = $2`, instanceID, keyID).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get app state key: %w", err)
	}
	return s.sealer.open(enc, adFor(asKeyTable, "key_data", instanceID, fmt.Sprintf("%x", keyID)))
}

// AppStateKeyCount returns how many keys we hold.
func (s *Store) AppStateKeyCount(ctx context.Context, instanceID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM wax_appstate_keys WHERE instance_id = $1`, instanceID).Scan(&n)
	return n, err
}

// --- versions and MACs -------------------------------------------------------------

// AppStateVersion returns a collection's hash state (the zero state if it
// was never synced).
func (s *Store) AppStateVersion(ctx context.Context, instanceID, collection string) (appstate.HashState, error) {
	var st appstate.HashState
	var v int64
	var h []byte
	q := `SELECT version, hash, unverified FROM wax_appstate_versions WHERE instance_id = $1 AND collection = $2`
	if s.inTx {
		q += ` FOR UPDATE`
	}
	err := s.db.QueryRow(ctx, q, instanceID, collection).Scan(&v, &h, &st.Unverified)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return appstate.HashState{}, nil
	case err != nil:
		return st, fmt.Errorf("store: app state version: %w", err)
	case len(h) != len(st.Hash):
		return st, fmt.Errorf("store: app state hash of %s is %d bytes", collection, len(h))
	}
	st.Version = uint64(v)
	copy(st.Hash[:], h)
	return st, nil
}

// PutAppStateVersion stores a collection's hash state.
func (s *Store) PutAppStateVersion(ctx context.Context, instanceID, collection string, st appstate.HashState) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_appstate_versions (instance_id, collection, version, hash, unverified) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (instance_id, collection) DO UPDATE
		SET version = excluded.version, hash = excluded.hash, unverified = excluded.unverified, updated_at = now()`,
		instanceID, collection, int64(st.Version), st.Hash[:], st.Unverified)
	if err != nil {
		return fmt.Errorf("store: put app state version: %w", err)
	}
	return nil
}

// ResetAppState forgets a collection (version and MACs), forcing a full
// snapshot sync.
func (s *Store) ResetAppState(ctx context.Context, instanceID, collection string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_appstate_versions WHERE instance_id = $1 AND collection = $2`, instanceID, collection); err != nil {
		return fmt.Errorf("store: reset app state: %w", err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM wax_appstate_macs WHERE instance_id = $1 AND collection = $2`, instanceID, collection); err != nil {
		return fmt.Errorf("store: reset app state macs: %w", err)
	}
	return nil
}

// AppStateValueMAC returns the stored value MAC for an index MAC, or nil.
func (s *Store) AppStateValueMAC(ctx context.Context, instanceID, collection string, indexMAC []byte) ([]byte, error) {
	var v []byte
	err := s.db.QueryRow(ctx, `SELECT value_mac FROM wax_appstate_macs WHERE instance_id = $1 AND collection = $2 AND index_mac = $3`,
		instanceID, collection, indexMAC).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: app state value mac: %w", err)
	}
	return v, nil
}

// PutAppStateMAC stores (valueMAC != nil) or deletes an index's value MAC.
func (s *Store) PutAppStateMAC(ctx context.Context, instanceID, collection string, indexMAC, valueMAC []byte) error {
	var err error
	if valueMAC == nil {
		_, err = s.db.Exec(ctx, `DELETE FROM wax_appstate_macs WHERE instance_id = $1 AND collection = $2 AND index_mac = $3`,
			instanceID, collection, indexMAC)
	} else {
		_, err = s.db.Exec(ctx, `INSERT INTO wax_appstate_macs (instance_id, collection, index_mac, value_mac) VALUES ($1, $2, $3, $4)
			ON CONFLICT (instance_id, collection, index_mac) DO UPDATE SET value_mac = excluded.value_mac`,
			instanceID, collection, indexMAC, valueMAC)
	}
	if err != nil {
		return fmt.Errorf("store: put app state mac: %w", err)
	}
	return nil
}

// --- contacts and chats -------------------------------------------------------------

// ContactName is an address book entry (app state).
type ContactName struct {
	First, Full string
	// Username is the contact's WhatsApp username (without "@"), if any.
	Username string
}

// PutContactName stores names from the address book (app state).
func (s *Store) PutContactName(ctx context.Context, instanceID, jid string, n ContactName) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_contacts (instance_id, jid, first_name, full_name, username) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (instance_id, jid) DO UPDATE SET first_name = excluded.first_name, full_name = excluded.full_name,
			username = excluded.username, updated_at = now()`,
		instanceID, jid, n.First, n.Full, n.Username)
	if err != nil {
		return fmt.Errorf("store: put contact: %w", err)
	}
	return nil
}

// PutPushName stores a contact's self-chosen display name.
func (s *Store) PutPushName(ctx context.Context, instanceID, jid, pushName string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_contacts (instance_id, jid, push_name) VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, jid) DO UPDATE SET push_name = excluded.push_name, updated_at = now()
		WHERE wax_contacts.push_name <> excluded.push_name`, instanceID, jid, pushName)
	if err != nil {
		return fmt.Errorf("store: put push name: %w", err)
	}
	return nil
}

// ChatUpdate is a partial update of a chat row; nil fields are unchanged.
type ChatUpdate struct {
	Name          *string
	LastMessageAt *time.Time
	UnreadCount   *int
	Archived      *bool
	Pinned        *bool
	MutedUntil    *time.Time // zero time = not muted
}

// UpdateChat upserts a chat with the given fields.
func (s *Store) UpdateChat(ctx context.Context, instanceID, jid string, u ChatUpdate) error {
	var muted any
	setMuted := u.MutedUntil != nil
	if setMuted && !u.MutedUntil.IsZero() {
		muted = *u.MutedUntil
	}
	_, err := s.db.Exec(ctx, `INSERT INTO wax_chats (instance_id, jid, name, last_message_at, unread_count, archived, pinned, muted_until)
		VALUES ($1, $2, COALESCE($3, ''), $4, COALESCE($5, 0), COALESCE($6, false), COALESCE($7, false), $8)
		ON CONFLICT (instance_id, jid) DO UPDATE SET
			name            = COALESCE($3, wax_chats.name),
			last_message_at = GREATEST(wax_chats.last_message_at, $4),
			unread_count    = COALESCE($5, wax_chats.unread_count),
			archived        = COALESCE($6, wax_chats.archived),
			pinned          = COALESCE($7, wax_chats.pinned),
			muted_until     = CASE WHEN $9 THEN $8 ELSE wax_chats.muted_until END,
			updated_at      = now()`,
		instanceID, jid, u.Name, u.LastMessageAt, u.UnreadCount, u.Archived, u.Pinned, muted, setMuted)
	if err != nil {
		return fmt.Errorf("store: update chat: %w", err)
	}
	return nil
}

// --- messages -----------------------------------------------------------------------

const msgTable = "wax_messages"

// StoredMessage is a message row.
type StoredMessage struct {
	Chat, ID, Sender string
	FromMe           bool
	SentAt           time.Time
	PushName, Type   string
	Content          []byte // serialized wae2e.Message; for "stub:<n>" types, the JSON array of the stub parameters
	Source           string
}

// PutMessage stores a message; an existing row (same chat and id) is kept.
func (s *Store) PutMessage(ctx context.Context, instanceID string, m *StoredMessage) error {
	var content []byte
	if m.Content != nil {
		content = s.sealer.seal(m.Content, adFor(msgTable, "content", instanceID, m.Chat, m.ID))
	}
	_, err := s.db.Exec(ctx, `INSERT INTO wax_messages (instance_id, chat, message_id, sender, from_me, sent_at, push_name, type, content, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (instance_id, chat, message_id) DO NOTHING`,
		instanceID, m.Chat, m.ID, m.Sender, m.FromMe, m.SentAt, m.PushName, m.Type, content, m.Source)
	if err != nil {
		return fmt.Errorf("store: put message: %w", err)
	}
	return nil
}

// Messages lists a chat's messages, newest first, before the given time;
// types, if given, keeps only those message types (e.g. "image").
func (s *Store) Messages(ctx context.Context, instanceID, chat string, before time.Time, limit int, types ...string) ([]StoredMessage, error) {
	rows, err := s.db.Query(ctx, aliasCTE+`
		SELECT chat, message_id, sender, from_me, sent_at, push_name, type, content, source
		FROM wax_messages WHERE instance_id = $1 AND chat IN (SELECT jid FROM alias) AND sent_at < $3
			AND (COALESCE(cardinality($5::text[]), 0) = 0 OR type = ANY($5))
		ORDER BY sent_at DESC LIMIT $4`,
		instanceID, chat, before, limit, types)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var enc []byte
		if err := rows.Scan(&m.Chat, &m.ID, &m.Sender, &m.FromMe, &m.SentAt, &m.PushName, &m.Type, &enc, &m.Source); err != nil {
			return nil, err
		}
		if enc != nil {
			if m.Content, err = s.sealer.open(enc, adFor(msgTable, "content", instanceID, m.Chat, m.ID)); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MessageTypes counts a chat's stored messages by type (under either
// address of a contact).
func (s *Store) MessageTypes(ctx context.Context, instanceID, chat string) (map[string]int, error) {
	rows, err := s.db.Query(ctx, aliasCTE+`SELECT type, count(*) FROM wax_messages
		WHERE instance_id = $1 AND chat IN (SELECT jid FROM alias) GROUP BY type`, instanceID, chat)
	if err != nil {
		return nil, fmt.Errorf("store: message types: %w", err)
	}
	out := map[string]int{}
	var typ string
	var n int
	_, err = pgx.ForEachRow(rows, []any{&typ, &n}, func() error {
		out[typ] = n
		return nil
	})
	return out, err
}

// CountMessages returns the number of stored messages (all chats).
func (s *Store) CountMessages(ctx context.Context, instanceID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM wax_messages WHERE instance_id = $1`, instanceID).Scan(&n)
	return n, err
}

// --- history queue ------------------------------------------------------------------

const histTable = "wax_history_queue"

// HistoryJob is a queued history sync notification.
type HistoryJob struct {
	ID           int64
	Notification []byte
	Attempts     int
}

// EnqueueHistory stores a history sync notification for processing.
func (s *Store) EnqueueHistory(ctx context.Context, instanceID string, notification []byte) error {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO wax_history_queue (instance_id, notification) VALUES ($1, '') RETURNING id`, instanceID).Scan(&id)
	if err != nil {
		return fmt.Errorf("store: enqueue history: %w", err)
	}
	_, err = s.db.Exec(ctx, `UPDATE wax_history_queue SET notification = $2 WHERE id = $1`, id,
		s.sealer.seal(notification, adFor(histTable, "notification", instanceID, id)))
	if err != nil {
		return fmt.Errorf("store: enqueue history: %w", err)
	}
	return nil
}

// NextHistoryJob returns the oldest pending job (nil if none).
func (s *Store) NextHistoryJob(ctx context.Context, instanceID string, maxAttempts int) (*HistoryJob, error) {
	var j HistoryJob
	var enc []byte
	err := s.db.QueryRow(ctx, `SELECT id, notification, attempts FROM wax_history_queue
		WHERE instance_id = $1 AND attempts < $2 ORDER BY id LIMIT 1`, instanceID, maxAttempts).Scan(&j.ID, &enc, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: next history job: %w", err)
	}
	if j.Notification, err = s.sealer.open(enc, adFor(histTable, "notification", instanceID, j.ID)); err != nil {
		return nil, err
	}
	return &j, nil
}

// FailHistoryJob records a failed attempt.
func (s *Store) FailHistoryJob(ctx context.Context, id int64, reason string) error {
	_, err := s.db.Exec(ctx, `UPDATE wax_history_queue SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, id, reason)
	return err
}

// DeleteHistoryJob removes a completed job.
func (s *Store) DeleteHistoryJob(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM wax_history_queue WHERE id = $1`, id)
	return err
}

// PutNCTSalt stores our NCT salt (nil clears it).
func (s *Store) PutNCTSalt(ctx context.Context, instanceID string, salt []byte) error {
	var v any
	if salt != nil {
		v = s.sealer.seal(salt, adFor(devTable, "nct_salt", instanceID))
	}
	_, err := s.db.Exec(ctx, `UPDATE wax_devices SET nct_salt = $2, updated_at = now() WHERE instance_id = $1`, instanceID, v)
	return err
}

// Contact is a stored contact.
type Contact struct {
	JID                           string
	FirstName, FullName, PushName string
}

// GetContact returns a contact, or ErrNotFound.
func (s *Store) GetContact(ctx context.Context, instanceID, jid string) (*Contact, error) {
	c := Contact{JID: jid}
	err := s.db.QueryRow(ctx, `SELECT first_name, full_name, push_name FROM wax_contacts WHERE instance_id = $1 AND jid = $2`,
		instanceID, jid).Scan(&c.FirstName, &c.FullName, &c.PushName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get contact: %w", err)
	}
	return &c, nil
}

// Chat is a stored chat.
type Chat struct {
	JID           string     `json:"jid"`
	Name          string     `json:"name,omitempty"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	UnreadCount   int        `json:"unread_count"`
	Archived      bool       `json:"archived"`
	Pinned        bool       `json:"pinned"`
	MutedUntil    *time.Time `json:"muted_until,omitempty"`
	// Phone is the contact's phone number (digits) for 1:1 chats, also
	// when the chat is addressed by LID and the mapping is known.
	Phone string `json:"phone,omitempty"`
	// PushName and Username are the contact's own display name and
	// WhatsApp username (without "@") of a 1:1 chat, if known.
	PushName string `json:"push_name,omitempty"`
	Username string `json:"username,omitempty"`
	// Last is the newest message worth showing in a chat list (protocol
	// and reaction messages are skipped); only ListChats fills it.
	Last *StoredMessage `json:"-"`
}

const chatColumns = `jid, name, last_message_at, unread_count, archived, pinned, muted_until`

func scanChat(row pgx.Row) (*Chat, error) {
	var c Chat
	err := row.Scan(&c.JID, &c.Name, &c.LastMessageAt, &c.UnreadCount, &c.Archived, &c.Pinned, &c.MutedUntil)
	return &c, err
}

// GetChat returns a chat, or ErrNotFound.
func (s *Store) GetChat(ctx context.Context, instanceID, jid string) (*Chat, error) {
	c, err := scanChat(s.db.QueryRow(ctx, `SELECT `+chatColumns+` FROM wax_chats WHERE instance_id = $1 AND jid = $2`, instanceID, jid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat: %w", err)
	}
	return c, nil
}

// HistoryQueueLength returns the number of queued history jobs.
func (s *Store) HistoryQueueLength(ctx context.Context, instanceID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM wax_history_queue WHERE instance_id = $1`, instanceID).Scan(&n)
	return n, err
}
