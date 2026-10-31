//go:build integration

package journal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/journal"
	"github.com/sarpaydemir/dost/internal/store/kv"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

var t0 = time.Date(2026, 10, 31, 10, 0, 0, 123456000, time.UTC)

type fixture struct {
	t    *testing.T
	db   *pg.DB
	kv   *kv.Store
	conn *pgx.Conn
	clk  *fakeclock.Clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := testdb.New(t)
	db, err := pg.Open(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return &fixture{t: t, db: db, kv: kv.New(db, clock.Real()), conn: conn, clk: fakeclock.New(t0)}
}

func (f *fixture) journal(agent string) *journal.Store { return journal.New(f.db, agent, f.clk) }

func (f *fixture) set(key, value string) {
	f.t.Helper()
	if err := f.kv.Set(f.t.Context(), key, json.RawMessage(value)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) take(j *journal.Store, key, generation string) (json.RawMessage, bool) {
	f.t.Helper()
	v, ok, err := j.Take(f.t.Context(), key, generation)
	if err != nil {
		f.t.Fatalf("Take(%s, %s): %v", key, generation, err)
	}
	return v, ok
}

// buffer returns a buffer value with generation and one message.
func buffer(generation, postID string) string {
	return fmt.Sprintf(`{"generation": %q, "flush_after": "2026-10-31T10:00:03.123456+00:00", "payload": {"event": %q},
		"messages": [{"post_id": %q, "user_id": "u1", "message": "hi", "file_ids": []}]}`, generation, postID, postID)
}

// row is a journal row without its buffer.
type row struct {
	Agent, Generation, Key string
	TakenAt                time.Time
	Resubmissions          int
}

// rows returns every journal row, ordered by agent and generation.
func (f *fixture) rows() []row {
	f.t.Helper()
	rows, _ := f.conn.Query(f.t.Context(), `SELECT agent_id, generation, conversation_key, taken_at, resubmissions
		FROM dost_inflight_runs ORDER BY agent_id, generation`)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func assertJSON(t *testing.T, what string, got json.RawMessage, want string) {
	t.Helper()
	if eq, err := testkit.JSONEqual(got, []byte(want)); err != nil || !eq {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func TestTakeMovesTheBufferIntoTheJournalOnlyForItsGeneration(t *testing.T) {
	f := newFixture(t)
	j := f.journal("agent-a")
	for _, c := range []struct{ value, generation string }{
		{buffer("g0", "p1"), "g1"},
		{`{"generation": 1, "messages": []}`, "1"},
		{`{"generation": null}`, ""},
		{`["g1"]`, "g1"},
		{`"g1"`, "g1"},
	} {
		f.set("k", c.value)
		if v, ok := f.take(j, "k", c.generation); ok {
			t.Errorf("Take(%q) of %s: taken %s", c.generation, c.value, v)
		}
		if v, _, _ := f.kv.Get(t.Context(), "k"); v == nil {
			t.Errorf("Take(%q) of %s: buffer removed", c.generation, c.value)
		}
	}
	if v, ok := f.take(j, "missing", "g1"); ok {
		t.Errorf("missing buffer: taken %s", v)
	}
	if rows := f.rows(); len(rows) > 0 {
		t.Fatalf("records after takes that took nothing: %+v", rows)
	}

	f.set("k", buffer("g1", "p1"))
	v, ok := f.take(j, "k", "g1")
	if !ok {
		t.Fatal("buffer with the generation not taken")
	}
	assertJSON(t, "taken value", v, buffer("g1", "p1"))
	if got, ok, err := f.kv.Get(t.Context(), "k"); ok || err != nil {
		t.Errorf("buffer after the take = %s, %v; want it deleted", got, err)
	}
	var stored json.RawMessage
	if err := f.conn.QueryRow(t.Context(), "SELECT buffer FROM dost_inflight_runs").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, "journaled buffer", stored, buffer("g1", "p1"))
	if rows := f.rows(); len(rows) != 1 || rows[0].Agent != "agent-a" || rows[0].Generation != "g1" || rows[0].Key != "k" ||
		!rows[0].TakenAt.Equal(t0) || rows[0].Resubmissions != 0 {
		t.Errorf("records = %+v; want agent-a's, for g1 under k, taken at %v, not resubmitted", rows, t0)
	}
	if v, ok := f.take(j, "k", "g1"); ok {
		t.Errorf("second take of g1 returned %s", v)
	}
}

func TestAppendsRacingTakesTakeEveryMessageExactlyOnce(t *testing.T) {
	f := newFixture(t)
	j := f.journal("agent-a")
	ctx := t.Context()
	const key, rounds = "agent:buffer:dm:u1", 100

	var taken []string
	take := func() {
		raw, ok, err := f.kv.Get(ctx, key)
		var b struct{ Generation string }
		if err != nil || !ok || json.Unmarshal(raw, &b) != nil {
			return
		}
		v, ok := f.take(j, key, b.Generation)
		if !ok {
			return
		}
		var got struct {
			Messages []struct {
				PostID string `json:"post_id"`
			}
		}
		if err := json.Unmarshal(v, &got); err != nil {
			t.Error(err)
		}
		for _, m := range got.Messages {
			taken = append(taken, m.PostID)
		}
	}
	appended := make(chan struct{})
	go func() {
		defer close(appended)
		for i := range rounds {
			err := f.kv.Update(ctx, key, nil, func(cur json.RawMessage) (json.RawMessage, error) {
				var b struct {
					Messages []json.RawMessage `json:"messages"`
				}
				if err := json.Unmarshal(cur, &b); err != nil {
					return nil, err
				}
				b.Messages = append(b.Messages, json.RawMessage(fmt.Sprintf(`{"post_id": "p%03d"}`, i)))
				return json.Marshal(map[string]any{"generation": fmt.Sprintf("g%03d", i), "messages": b.Messages})
			})
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for done := false; !done; {
		select {
		case <-appended:
			done = true
		default:
		}
		take()
	}

	var want []string
	for i := range rounds {
		want = append(want, fmt.Sprintf("p%03d", i))
	}
	if !slices.Equal(taken, want) {
		t.Errorf("taken messages = %q, want each of the %d appended once, in order", taken, rounds)
	}
	var journaled int
	if err := f.conn.QueryRow(ctx, "SELECT coalesce(sum(jsonb_array_length(buffer->'messages')), 0) FROM dost_inflight_runs").
		Scan(&journaled); err != nil || journaled != rounds {
		t.Errorf("journal holds %d messages (%v), want %d", journaled, err, rounds)
	}
}

func TestReleaseDeletesOnlyThisAgentsRecordAndToleratesRepeats(t *testing.T) {
	f := newFixture(t)
	a, b := f.journal("agent-a"), f.journal("agent-b")
	f.set("a:k", buffer("g1", "p1"))
	f.set("b:k", buffer("g1", "p2"))
	f.take(a, "a:k", "g1")
	f.take(b, "b:k", "g1")

	for i, want := range []bool{true, false} {
		if released, err := a.Release(t.Context(), "g1"); err != nil || released != want {
			t.Errorf("Release %d = %v, %v; want %v", i+1, released, err, want)
		}
	}
	if rows := f.rows(); len(rows) != 1 || rows[0].Agent != "agent-b" {
		t.Errorf("records = %+v, want only agent-b's", rows)
	}
}

func TestListReturnsTheAgentsRecordsOldestFirstWithTheStartsOfTheirRuns(t *testing.T) {
	f := newFixture(t)
	a, b := f.journal("agent-a"), f.journal("agent-b")
	for _, r := range []struct{ key, generation string }{{"k1", "g-b"}, {"k2", "g-a"}} {
		f.set(r.key, buffer(r.generation, r.key))
		f.take(a, r.key, r.generation)
	}
	f.set("kb", buffer("g-a", "kb"))
	f.take(b, "kb", "g-a")
	f.clk.Advance(time.Second)
	f.set("k3", buffer("g-0", "k3"))
	f.take(a, "k3", "g-0")
	for _, generation := range []string{"g-a", "g-0", "g-a", "missing"} {
		if err := a.Started(t.Context(), generation); err != nil {
			t.Fatalf("Started(%s): %v", generation, err)
		}
	}

	records, err := a.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []string
	for _, r := range records {
		got = append(got, fmt.Sprintf("%s/%s started %d", r.Key, r.Generation, r.Starts))
		assertJSON(t, "buffer of "+r.Key, r.Buffer, buffer(r.Generation, r.Key))
	}
	if want := []string{"k2/g-a started 2", "k1/g-b started 0", "k3/g-0 started 1"}; !slices.Equal(got, want) {
		t.Errorf("List = %q, want %q", got, want)
	}
	if records, err := b.List(t.Context()); err != nil || len(records) != 1 || records[0].Key != "kb" || records[0].Starts != 0 {
		t.Errorf("agent-b's List = %+v, %v; want its own record, never started", records, err)
	}
}
