//go:build integration

package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/store/pg"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/testkit/testdb"
)

var start = time.Date(2026, 10, 30, 12, 0, 0, 123456000, time.UTC)

// newStore returns a store on a fresh database, its clock, and a direct connection for inspecting rows.
func newStore(t *testing.T) (*Store, *fakeclock.Clock, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.New(t)
	db, err := pg.Open(ctx, dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	clk := fakeclock.New(start)
	return New(db, clk), clk, conn
}

func appendItem(item string) func([]json.RawMessage) ([]json.RawMessage, error) {
	return func(items []json.RawMessage) ([]json.RawMessage, error) {
		return append(items, json.RawMessage(item)), nil
	}
}

type row struct {
	value            []byte
	created, updated time.Time
}

func readRow(t *testing.T, conn *pgx.Conn, key string) (row, bool) {
	t.Helper()
	var r row
	err := conn.QueryRow(context.Background(),
		"SELECT value, created_at, updated_at FROM chat_sessions WHERE key = $1", key).Scan(&r.value, &r.created, &r.updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, true
}

func assertJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	if eq, err := testkit.JSONEqual(got, []byte(want)); err != nil || !eq {
		t.Errorf("value = %s, want %s (err %v)", got, want, err)
	}
}

func TestUpdateCreatesAMissingRowWithBothTimestamps(t *testing.T) {
	s, _, conn := newStore(t)
	if err := s.Update(context.Background(), "agent:u1", appendItem(`{"user": "hi"}`)); err != nil {
		t.Fatal(err)
	}
	r, ok := readRow(t, conn, "agent:u1")
	if !ok {
		t.Fatal("row not created")
	}
	assertJSON(t, r.value, `[{"user": "hi"}]`)
	if !r.created.Equal(start) || !r.updated.Equal(start) {
		t.Errorf("created_at, updated_at = %v, %v; want both %v", r.created, r.updated, start)
	}
}

func TestUpdateKeepsCreatedAtAndAdvancesUpdatedAt(t *testing.T) {
	s, clk, conn := newStore(t)
	ctx := context.Background()
	if err := s.Update(ctx, "agent:u1", appendItem(`{"n": 1}`)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	if err := s.Update(ctx, "agent:u1", appendItem(`{"n": 2}`)); err != nil {
		t.Fatal(err)
	}
	r, _ := readRow(t, conn, "agent:u1")
	assertJSON(t, r.value, `[{"n": 1}, {"n": 2}]`)
	if !r.created.Equal(start) || !r.updated.Equal(start.Add(time.Hour)) {
		t.Errorf("created_at, updated_at = %v, %v; want %v, %v", r.created, r.updated, start, start.Add(time.Hour))
	}
}

func TestUpdatePassesExistingItemsVerbatim(t *testing.T) {
	s, _, conn := newStore(t)
	ctx := context.Background()
	existing := `[{"ts": "2026-02-01T00:00:00", "user": "old", "scope": "public", "assistant": "reply"},
		{"user": "/cmd x", "assistant": "**t:**\nm", "ts": "2026-02-01T00:00:00+00:00", "scope": "public", "context_id": "", "step": 2},
		{"big": 123456789012345678901234567890, "f": 1.50, "nested": {"a": [null, true]}}]`
	if _, err := conn.Exec(ctx, `INSERT INTO chat_sessions (key, value, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
		"agent:legacy", existing, start); err != nil {
		t.Fatal(err)
	}
	before, _ := readRow(t, conn, "agent:legacy")

	var seen []json.RawMessage
	err := s.Update(ctx, "agent:legacy", func(items []json.RawMessage) ([]json.RawMessage, error) {
		seen = items
		return items, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("Update saw %d items, want 3", len(seen))
	}
	after, _ := readRow(t, conn, "agent:legacy")
	if string(after.value) != string(before.value) {
		t.Errorf("stored value changed\nbefore: %s\nafter:  %s", before.value, after.value)
	}
}

func TestUpdateLeavesTheRowUnchangedWhenTheFunctionFails(t *testing.T) {
	s, _, conn := newStore(t)
	ctx := context.Background()
	failed := errors.New("failed")
	if err := s.Update(ctx, "agent:new", func([]json.RawMessage) ([]json.RawMessage, error) { return nil, failed }); !errors.Is(err, failed) {
		t.Fatalf("Update error = %v, want %v", err, failed)
	}
	if _, ok := readRow(t, conn, "agent:new"); ok {
		t.Error("a failed update created the row")
	}
}

func TestConcurrentUpdatesToOneKeyKeepEveryItem(t *testing.T) {
	s, _, conn := newStore(t)
	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Go(func() {
			errs <- s.Update(context.Background(), "agent:thread:t1", appendItem(fmt.Sprintf(`{"n": %d}`, i)))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	r, _ := readRow(t, conn, "agent:thread:t1")
	var items []struct{ N int }
	if err := json.Unmarshal(r.value, &items); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, it := range items {
		seen[it.N] = true
	}
	if len(items) != writers || len(seen) != writers {
		t.Errorf("stored %d items with %d distinct values, want %d of each: %s", len(items), len(seen), writers, r.value)
	}
}

func TestEmptyObjectOrNullValueReadsAsNoItemsAndBecomesAList(t *testing.T) {
	for _, value := range []string{`{}`, `null`} {
		t.Run(value, func(t *testing.T) {
			s, _, conn := newStore(t)
			ctx := context.Background()
			if _, err := conn.Exec(ctx, `INSERT INTO chat_sessions (key, value, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
				"agent:u1", value, start); err != nil {
				t.Fatal(err)
			}
			items, err := s.Get(ctx, "agent:u1")
			if err != nil || len(items) != 0 {
				t.Fatalf("Get = %s, %v; want no items", items, err)
			}
			if err := s.Update(ctx, "agent:u1", appendItem(`{"n": 1}`)); err != nil {
				t.Fatal(err)
			}
			r, _ := readRow(t, conn, "agent:u1")
			assertJSON(t, r.value, `[{"n": 1}]`)
		})
	}
}

func TestUpdateTakesARowIDOnlyWhenItCreatesTheRow(t *testing.T) {
	s, _, conn := newStore(t)
	ctx := context.Background()
	for _, key := range []string{"agent:u1", "agent:u1", "agent:u1", "agent:u2"} {
		if err := s.Update(ctx, key, appendItem(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	var id int
	if err := conn.QueryRow(ctx, "SELECT id FROM chat_sessions WHERE key = 'agent:u2'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Errorf("second row id = %d, want 2", id)
	}
}
