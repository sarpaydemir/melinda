//go:build integration

package pg

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/testkit/testdb"
)

func acquired(t *testing.T, l *Lock) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testkit.AwaitTimeout)
	defer cancel()
	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(l.Release)
}

func receive(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	timeout := time.NewTimer(testkit.AwaitTimeout)
	defer timeout.Stop()
	select {
	case err := <-ch:
		return err
	case <-timeout.C:
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func TestLockIsHeldByOneSessionPerNameAndPassesOnRelease(t *testing.T) {
	dsn := testdb.New(t)
	discard := slog.New(slog.DiscardHandler)
	first := open(t, dsn).NewLock("agent-a", clock.Real(), discard)
	acquired(t, first)
	acquired(t, open(t, dsn).NewLock("agent-b", clock.Real(), discard))

	clk := fakeclock.New(time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC))
	var logs bytes.Buffer
	second := open(t, dsn).NewLock("agent-a", clk, slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(second.Release)
	got := make(chan error, 1)
	go func() { got <- second.Acquire(t.Context()) }()
	for range 60 {
		clk.AwaitTimers(t, 1)
		select {
		case err := <-got:
			t.Fatalf("second Acquire returned %v while the first session holds the lock", err)
		default:
		}
		clk.Advance(time.Second)
	}
	clk.AwaitTimers(t, 1)
	warnings := strings.Count(logs.String(), "level=WARN")
	if warnings != 3 || strings.Count(logs.String(), "lock=agent-a") != 3 {
		t.Errorf("after 60 s of waiting the log is\n%s\nwant 3 warnings naming the lock, at 0, 30, and 60 s", logs.String())
	}

	ctx, cancel := context.WithCancel(t.Context())
	waited := make(chan error, 1)
	go func() { waited <- first.Wait(ctx) }()
	cancel()
	if err := receive(t, waited, "Wait after its context ended"); err != nil {
		t.Errorf("Wait after its context ended = %v, want nil", err)
	}
	first.Release()
	// The server frees the lock once the first session's backend has exited, which may take more than one try.
	timeout := time.NewTimer(testkit.AwaitTimeout)
	defer timeout.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for done := false; !done; {
		if clk.Timers() > 0 {
			clk.Advance(time.Second)
		}
		select {
		case err := <-got:
			if err != nil {
				t.Fatalf("second Acquire after the first released = %v", err)
			}
			done = true
		case <-poll.C:
		case <-timeout.C:
			t.Fatal("second Acquire did not return after the first released")
		}
	}

	var idle, interval, count string
	if err := second.conn.QueryRow(t.Context(), `SELECT current_setting('tcp_keepalives_idle'),
		current_setting('tcp_keepalives_interval'), current_setting('tcp_keepalives_count')`).Scan(&idle, &interval, &count); err != nil {
		t.Fatal(err)
	}
	if idle != "30" || interval != "10" || count != "3" {
		t.Errorf("lock session keepalives: idle %s, interval %s, count %s; want 30, 10, 3", idle, interval, count)
	}
}

func TestLockConnectionLossEndsWaitWithAnError(t *testing.T) {
	dsn := testdb.New(t)
	l := open(t, dsn).NewLock("agent-a", clock.Real(), slog.New(slog.DiscardHandler))
	acquired(t, l)
	waited := make(chan error, 1)
	go func() { waited <- l.Wait(t.Context()) }()

	var terminated bool
	if err := dial(t, dsn).QueryRow(t.Context(), `SELECT bool_and(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).
		Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate the lock's session: %v, %v", terminated, err)
	}
	if err := receive(t, waited, "Wait after the connection was lost"); err == nil {
		t.Error("Wait after the connection was lost = nil, want an error")
	}
}
