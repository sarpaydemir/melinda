package journal

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/store/pg"
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

	listSQL = `SELECT conversation_key, generation, buffer, resubmissions FROM dost_inflight_runs WHERE agent_id = $1
ORDER BY taken_at, generation`
)

// Record is a taken buffer whose run has not been released.
type Record struct {
	Key        string
	Generation string
	Buffer     json.RawMessage
	Starts     int // runs started from the record, kept in its resubmissions column
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

// Started counts one more start of the run of generation's record; a missing record is not an error.
func (s *Store) Started(ctx context.Context, generation string) error {
	_, err := s.db.Exec(ctx, `UPDATE dost_inflight_runs SET resubmissions = resubmissions + 1 WHERE agent_id = $1 AND generation = $2`,
		s.agent, generation)
	return err
}

// List returns every record, oldest first.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	rows, _ := s.db.Query(ctx, listSQL, s.agent)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Record])
}
