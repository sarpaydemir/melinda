package journal

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/pg"
)

const (
	// takeSQL deletes the buffer under $1 if its generation is the string $2 and records the deleted value for
	// agent $3, taken at $4.
	takeSQL = `WITH taken AS (
	DELETE FROM key_value_store WHERE key = $1 AND value->'generation' = to_jsonb($2::text) RETURNING value
)
INSERT INTO dost_inflight_runs (agent_id, generation, conversation_key, buffer, taken_at)
SELECT $3, $2, $1, value, $4 FROM taken
RETURNING buffer`

	expireSQL = `WITH expired AS (
	DELETE FROM dost_inflight_runs WHERE agent_id = $1 AND resubmissions >= $2
	RETURNING conversation_key, generation, buffer, taken_at
)
SELECT conversation_key, generation, buffer FROM expired ORDER BY taken_at, generation`

	countSQL = `WITH counted AS (
	UPDATE dost_inflight_runs SET resubmissions = resubmissions + 1 WHERE agent_id = $1
	RETURNING conversation_key, generation, buffer, taken_at
)
SELECT conversation_key, generation, buffer FROM counted ORDER BY taken_at, generation`
)

// Record is a taken buffer whose run has not been released.
type Record struct {
	Key        string
	Generation string
	Buffer     json.RawMessage
}

// Store keeps one agent's records.
type Store struct {
	db    *pg.DB
	agent string
	clk   clock.Clock
}

// New returns the store of agentID's records on db; clk timestamps takes.
func New(db *pg.DB, agentID string, clk clock.Clock) *Store {
	return &Store{db: db, agent: agentID, clk: clk}
}

// Take deletes the key-value entry under key if its generation is generation and records the deleted value, in
// one transaction. It returns the taken value; ok is false when there is no entry or it has another generation.
func (s *Store) Take(ctx context.Context, key, generation string) (json.RawMessage, bool, error) {
	var v json.RawMessage
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, takeSQL, key, generation, s.agent, s.clk.Now()).Scan(&v)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// Release deletes the record of generation and reports whether it was there; a missing record is not an error.
// A run makes no visible change after false: another process has taken the run over, or it expired.
func (s *Store) Release(ctx context.Context, generation string) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM dost_inflight_runs WHERE agent_id = $1 AND generation = $2`, s.agent, generation)
	return err == nil && tag.RowsAffected() == 1, err
}

// Resubmit counts one more resubmission of every record and returns the records oldest first. Records already
// resubmitted limit times are deleted instead and returned as expired, also oldest first.
func (s *Store) Resubmit(ctx context.Context, limit int) (records, expired []Record, err error) {
	err = s.db.WithTx(ctx, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, expireSQL, s.agent, limit)
		if expired, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Record]); err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, countSQL, s.agent)
		records, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Record])
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return records, expired, nil
}
