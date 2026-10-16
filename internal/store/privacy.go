package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const privacyTable = "wax_privacy_tokens"

// PrivacyToken is a contact's privacy token and when we last issued ours
// to that contact.
type PrivacyToken struct {
	User    string
	Token   []byte // nil when we only issued ours
	TokenAt time.Time
	SentAt  time.Time
}

// PutPrivacyToken stores a token received from user, unless a newer one
// is stored already.
func (s *Store) PutPrivacyToken(ctx context.Context, instance, user string, token []byte, at time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_privacy_tokens (instance_id, user_jid, token, token_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance_id, user_jid) DO UPDATE SET token = excluded.token, token_at = excluded.token_at
		WHERE wax_privacy_tokens.token_at IS NULL OR wax_privacy_tokens.token_at <= excluded.token_at`,
		instance, user, s.sealer.seal(token, adFor(privacyTable, "token", instance, user)), at)
	if err != nil {
		return fmt.Errorf("store: put privacy token: %w", err)
	}
	return nil
}

// MarkPrivacyTokenSent records that we issued our token to user.
func (s *Store) MarkPrivacyTokenSent(ctx context.Context, instance, user string, at time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO wax_privacy_tokens (instance_id, user_jid, sent_at) VALUES ($1, $2, $3)
		ON CONFLICT (instance_id, user_jid) DO UPDATE SET sent_at = excluded.sent_at`, instance, user, at)
	if err != nil {
		return fmt.Errorf("store: mark privacy token sent: %w", err)
	}
	return nil
}

// GetPrivacyToken returns the freshest token stored under any of the
// user's addresses (LID or phone number), or nil.
func (s *Store) GetPrivacyToken(ctx context.Context, instance string, users ...string) (*PrivacyToken, error) {
	var t PrivacyToken
	var enc []byte
	var tokenAt, sentAt *time.Time
	err := s.db.QueryRow(ctx, `SELECT user_jid, token, token_at, sent_at FROM wax_privacy_tokens
		WHERE instance_id = $1 AND user_jid = ANY($2) ORDER BY token_at DESC NULLS LAST LIMIT 1`, instance, users).
		Scan(&t.User, &enc, &tokenAt, &sentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get privacy token: %w", err)
	}
	if enc != nil {
		if t.Token, err = s.sealer.open(enc, adFor(privacyTable, "token", instance, t.User)); err != nil {
			return nil, err
		}
	}
	if tokenAt != nil {
		t.TokenAt = *tokenAt
	}
	if sentAt != nil {
		t.SentAt = *sentAt
	}
	return &t, nil
}

// DeletePrivacyTokensBefore drops tokens (and issuance records) older than
// cutoff; they are no longer valid.
func (s *Store) DeletePrivacyTokensBefore(ctx context.Context, instance string, cutoff time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM wax_privacy_tokens WHERE instance_id = $1
		AND COALESCE(token_at, '-infinity') < $2 AND COALESCE(sent_at, '-infinity') < $2`, instance, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: delete privacy tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}
