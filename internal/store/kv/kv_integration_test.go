//go:build integration

package kv_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/store/kv"
	"github.com/sarpaydemir/dost/internal/store/pg"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/kvtest"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

// open returns a store on a new database, and a direct connection to that database.
func open(t *testing.T, clk clock.Clock) (*kv.Store, *pgx.Conn) {
	t.Helper()
	dsn := testdb.New(t)
	db, err := pg.Open(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return kv.New(db, clk), dial(t, dsn)
}

func dial(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func TestStorePassesTheContract(t *testing.T) {
	kvtest.Contract(t, func(t *testing.T) kvtest.Store {
		s, _ := open(t, clock.Real())
		return s
	})
}

func TestWritesSetTypeAndTimestampsAndKeepThemOnUpdate(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 9, 30, 0, 123456000, time.UTC)
	clk := fakeclock.New(t0)
	s, conn := open(t, clk)
	ctx := t.Context()
	same := func(cur json.RawMessage) (json.RawMessage, error) { return cur, nil }
	check := func(key, wantType string, wantCreated, wantUpdated time.Time) {
		t.Helper()
		var typ string
		var created, updated time.Time
		if err := conn.QueryRow(ctx, "SELECT type, created_at, updated_at FROM key_value_store WHERE key = $1", key).
			Scan(&typ, &created, &updated); err != nil {
			t.Fatalf("row %s: %v", key, err)
		}
		if typ != wantType || !created.Equal(wantCreated) || !updated.Equal(wantUpdated) {
			t.Errorf("row %s: type %q, created_at %v, updated_at %v; want %q, %v, %v",
				key, typ, created.UTC(), updated.UTC(), wantType, wantCreated, wantUpdated)
		}
	}

	if err := s.Set(ctx, "set", json.RawMessage(`{"v": 1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "update", nil, same); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"set", "update"} {
		check(key, "none", t0, t0)
	}

	if _, err := conn.Exec(ctx, "UPDATE key_value_store SET type = 'other'"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	t1 := t0.Add(time.Minute)
	if err := s.Set(ctx, "set", json.RawMessage(`{"v": 2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "update", nil, same); err != nil {
		t.Fatal(err)
	}
	check("set", "other", t0, t1)
	check("update", "other", t0, t1)
}

func TestUpdateRetriesWhenTheRowIsDeletedBeforeItIsLocked(t *testing.T) {
	s, conn := open(t, clock.Real())
	ctx := t.Context()
	if err := s.Set(ctx, "k", json.RawMessage(`{"stale": true}`)); err != nil {
		t.Fatal(err)
	}
	locker, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(ctx, "SELECT 1 FROM key_value_store WHERE key = 'k' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	const seed = `{"messages": []}`
	var seen []json.RawMessage
	done := make(chan error, 1)
	go func() {
		done <- s.Update(ctx, "k", json.RawMessage(seed), func(cur json.RawMessage) (json.RawMessage, error) {
			seen = append(seen, cur)
			return json.RawMessage(`{"messages": [1]}`), nil
		})
	}()

	// Wait until the update has passed its insert and is waiting for the row lock.
	watcher := dial(t, conn.Config().ConnString())
	for {
		var waiting bool
		if err := watcher.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FOR UPDATE%')`).
			Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Update returned before waiting for the row lock: %v", err)
		default:
		}
		if err := clock.Sleep(ctx, clock.Real(), 10*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := locker.Exec(ctx, "DELETE FROM key_value_store WHERE key = 'k'"); err != nil {
		t.Fatal(err)
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(seen) != 1 || string(seen[0]) != seed {
		t.Errorf("fn saw %q, want only the seed %s", seen, seed)
	}
	if got, _, err := s.Get(ctx, "k"); err != nil || string(got) != `{"messages": [1]}` {
		t.Errorf("stored value = %s, %v; want the update's result", got, err)
	}
}
