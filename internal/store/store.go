// Package store is the PostgreSQL persistence layer. All state needed to
// resume a session (identity keys, prekeys, Signal state, sync versions) lives
// here, and sensitive keys are sealed with a master key before writing.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("store: not found")

// dbtx is satisfied by both *pgxpool.Pool and pgx.Tx, so every query method
// works inside or outside a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Store provides access to persistent state. A Store obtained from InTx is
// bound to that transaction.
type Store struct {
	pool   *pgxpool.Pool
	db     dbtx
	sealer *sealer
	inTx   bool
}

// Open connects to PostgreSQL, applies pending migrations and returns a Store.
func Open(ctx context.Context, dsn string, masterKey []byte) (*Store, error) {
	sl, err := newSealer(masterKey)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{pool: pool, db: pool, sealer: sl}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// PoolSize is the maximum number of database connections (set with the
// pool_max_conns parameter of the DSN; pgx defaults to max(4, CPUs)).
func (s *Store) PoolSize() int { return int(s.pool.Config().MaxConns) }

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// InTx runs fn in a transaction. fn receives a Store bound to the
// transaction; the transaction commits if fn returns nil and rolls back
// otherwise (including on panic). Nested calls reuse the outer transaction.
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.inTx {
		return fn(s)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(&Store{pool: s.pool, db: tx, sealer: s.sealer, inTx: true})
	})
}

// Batch runs fn against a Store that queues its writes instead of
// executing them, then sends them in one round trip. PostgreSQL runs a
// batch as one implicit transaction (or inside the current one), so the
// writes are atomic. fn may only call write methods whose result does not
// depend on the database (reads fail with ErrBatchRead).
func (s *Store) Batch(ctx context.Context, fn func(b *Store) error) error {
	q := &batchDB{}
	if err := fn(&Store{pool: s.pool, db: q, sealer: s.sealer, inTx: true}); err != nil {
		return err
	}
	if q.b.Len() == 0 {
		return nil
	}
	return s.db.SendBatch(ctx, &q.b).Close()
}

// rowQuery is a single-row read split into its SQL and its decoding, so the
// same read can run alone (queryRow) or as part of a batched round trip.
type rowQuery[T any] struct {
	sql  string
	args []any
	scan func(pgx.Row) (T, error)
}

func queryRow[T any](ctx context.Context, s *Store, q rowQuery[T]) (T, error) {
	return q.scan(s.db.QueryRow(ctx, q.sql, q.args...))
}

// ErrBatchRead is returned for reads attempted inside Batch.
var ErrBatchRead = errors.New("store: read inside a batch")

// batchDB queues Exec calls into a pgx.Batch.
type batchDB struct{ b pgx.Batch }

func (q *batchDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.b.Queue(sql, args...)
	return pgconn.CommandTag{}, nil
}

func (q *batchDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, ErrBatchRead }

func (q *batchDB) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

func (q *batchDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("store: nested batch")
}

type errRow struct{}

func (errRow) Scan(...any) error { return ErrBatchRead }

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is an arbitrary constant identifying our advisory lock.
const migrationLockID = 0x7761780001 // "wax" + 1

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("store: bad migration file name %q", name)
		}
		body, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(body)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out, nil
}

// migrate applies pending migrations, each in its own transaction. A
// transaction-scoped advisory lock serializes concurrent starters, so several
// processes can boot against the same database safely.
func (s *Store) migrate(ctx context.Context) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS wax_migrations (
		version INT PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("store: create migrations table: %w", err)
	}
	for _, m := range migs {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wax_migrations WHERE version = $1)`, m.version).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return nil
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO wax_migrations (version, name) VALUES ($1, $2)`, m.version, m.name)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
	}
	return nil
}
