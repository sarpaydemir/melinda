package kv

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/pg"
)

const (
	// insertSQL writes every column, because the table has no defaults.
	insertSQL = `INSERT INTO key_value_store (type, key, value, created_at, updated_at) VALUES ('none', $1, $2, $3, $3) `
	lockSQL   = `SELECT value FROM key_value_store WHERE key = $1 FOR UPDATE`
)

// Store reads and writes key_value_store rows. Values are opaque JSON, written as given.
type Store struct {
	db  *pg.DB
	clk clock.Clock
}

// New returns a Store on db that timestamps its writes with clk.
func New(db *pg.DB, clk clock.Clock) *Store { return &Store{db: db, clk: clk} }

// Get returns the value under key; ok is false when there is none or it is JSON null.
func (s *Store) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var v json.RawMessage
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT value FROM key_value_store WHERE key = $1`, key).Scan(&v)
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && isNull(v)) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// Set stores value under key, keeping the row's created_at and type when it exists.
func (s *Store) Set(ctx context.Context, key string, value json.RawMessage) error {
	now := s.clk.Now()
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertSQL+`ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
			key, value, now)
		return err
	})
}

// Update replaces the value under key with fn's result in one transaction that holds the row lock while fn runs.
// A missing key is first created with seed ({} when nil); fn also receives seed when the stored value is JSON
// null. The result is always written. An error from fn is returned and nothing is written.
func (s *Store) Update(ctx context.Context, key string, seed json.RawMessage, fn func(json.RawMessage) (json.RawMessage, error)) error {
	if seed == nil {
		seed = json.RawMessage(`{}`)
	}
	now := s.clk.Now()
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		var cur json.RawMessage
		for {
			if _, err := tx.Exec(ctx, insertSQL+`ON CONFLICT (key) DO NOTHING`, key, seed, now); err != nil {
				return err
			}
			err := tx.QueryRow(ctx, lockSQL, key).Scan(&cur)
			if err == nil {
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			// A concurrent delete removed the row between the insert and the lock.
		}
		if isNull(cur) {
			cur = seed
		}
		next, err := fn(cur)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE key_value_store SET value = $2, updated_at = $3 WHERE key = $1`, key, next, now)
		return err
	})
}

// DeleteIf removes key if pred holds for its value, holding the row lock while pred runs, and returns the
// removed value.
func (s *Store) DeleteIf(ctx context.Context, key string, pred func(json.RawMessage) bool) (json.RawMessage, bool, error) {
	var v json.RawMessage
	deleted := false
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, lockSQL, key).Scan(&v)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || !pred(v) {
			return err
		}
		deleted = true
		_, err = tx.Exec(ctx, `DELETE FROM key_value_store WHERE key = $1`, key)
		return err
	})
	if err != nil || !deleted {
		return nil, false, err
	}
	return v, true, nil
}

// List returns the entries whose keys start with prefix; `_` and `%` match only themselves.
func (s *Store) List(ctx context.Context, prefix string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT key, value FROM key_value_store WHERE starts_with(key, $1)`, prefix)
		var k string
		var v json.RawMessage
		_, err := pgx.ForEachRow(rows, []any{&k, &v}, func() error {
			out[k] = v
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isNull reports whether v is JSON null, as jsonb renders it.
func isNull(v json.RawMessage) bool { return string(v) == "null" }
