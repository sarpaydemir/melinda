package history

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/pg"
)

// Store reads and writes the JSON item list kept in one chat_sessions row per key.
type Store struct {
	db  *pg.DB
	clk clock.Clock
}

// New returns a Store on db that timestamps rows with clk.
func New(db *pg.DB, clk clock.Clock) *Store {
	return &Store{db: db, clk: clk}
}

// Get returns the items stored under key, oldest first; a missing row, or a value of {} or null, has none.
func (s *Store) Get(ctx context.Context, key string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "SELECT NULLIF(value, '{}') FROM chat_sessions WHERE key = $1", key).Scan(&items)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return items, err
}

// Update replaces the items under key with what fn returns for the current items, holding the row lock so that
// concurrent updates of one key apply one after another. A missing row is created with no items first. When fn
// fails, nothing changes.
func (s *Store) Update(ctx context.Context, key string, fn func(items []json.RawMessage) ([]json.RawMessage, error)) error {
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		now := s.clk.Now()
		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_sessions (key, value, created_at, updated_at)
			SELECT $1::varchar, '[]'::jsonb, $2::timestamptz, $2::timestamptz
			WHERE NOT EXISTS (SELECT 1 FROM chat_sessions WHERE key = $1)
			ON CONFLICT (key) DO NOTHING`, key, now); err != nil {
			return err
		}
		var items []json.RawMessage
		if err := tx.QueryRow(ctx, "SELECT NULLIF(value, '{}') FROM chat_sessions WHERE key = $1 FOR UPDATE", key).Scan(&items); err != nil {
			return err
		}
		items, err := fn(items)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE chat_sessions SET value = $2, updated_at = $3 WHERE key = $1", key, items, now)
		return err
	})
}
