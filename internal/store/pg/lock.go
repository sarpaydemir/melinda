package pg

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/clock"
)

const (
	// lockRetry is how often Acquire tries again while another session holds the lock.
	lockRetry = time.Second
	// lockWarnEvery is how often Acquire logs that it is still waiting.
	lockWarnEvery = 30 * time.Second
	// keepaliveSQL makes the server notice a silently lost lock session, and free the lock, within about a minute.
	keepaliveSQL = "SET tcp_keepalives_idle = 30; SET tcp_keepalives_interval = 10; SET tcp_keepalives_count = 3"
)

// Lock is a session-level advisory lock that only one session at a time can hold, kept on a connection of its own.
type Lock struct {
	db   *DB
	name string
	key  int64
	clk  clock.Clock
	log  *slog.Logger
	conn *pgx.Conn
}

// NewLock returns the lock keyed by the 64-bit FNV-1a hash of name.
func (db *DB) NewLock(name string, clk clock.Clock, log *slog.Logger) *Lock {
	h := fnv.New64a()
	h.Write([]byte(name))
	return &Lock{db: db, name: name, key: int64(h.Sum64()), clk: clk, log: log}
}

// Acquire returns once l is held on a connection taken out of the pool. While another session holds it, Acquire
// tries again every second and logs a warning when it starts waiting and every 30 s after.
func (l *Lock) Acquire(ctx context.Context) error {
	pooled, err := l.db.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	if err := l.take(ctx, conn); err != nil {
		_ = conn.Close(context.Background())
		return err
	}
	l.conn = conn
	return nil
}

// take waits until the session of conn holds the lock.
func (l *Lock) take(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, keepaliveSQL); err != nil {
		return err
	}
	start := l.clk.Now()
	var warned time.Time
	for {
		var held bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", l.key).Scan(&held); err != nil || held {
			return err
		}
		if now := l.clk.Now(); warned.IsZero() || now.Sub(warned) >= lockWarnEvery {
			l.log.Warn("another instance holds the lock; waiting", "lock", l.name, "waited", now.Sub(start))
			warned = now
		}
		if err := clock.Sleep(ctx, l.clk, lockRetry); err != nil {
			return err
		}
	}
}

// Wait blocks until ctx ends, returning nil, or until the lock's connection is lost, returning why.
func (l *Lock) Wait(ctx context.Context) error {
	for {
		err := l.conn.PgConn().WaitForNotification(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock %s lost: %w", l.name, err)
		}
	}
}

// Release closes the lock's connection, which releases the lock. It does nothing when l is not held. A Wait in
// progress must have returned first.
func (l *Lock) Release() {
	if l.conn != nil {
		_ = l.conn.Close(context.Background())
		l.conn = nil
	}
}
