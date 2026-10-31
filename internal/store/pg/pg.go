package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
)

const (
	// connectWindow is how long Open keeps retrying the first connection.
	connectWindow = 60 * time.Second
	// connectTimeout bounds each connection attempt when the DSN sets no connect_timeout.
	connectTimeout = 10 * time.Second
)

// DB is the connection pool the stores share.
type DB struct {
	pool *pgxpool.Pool
}

// DSN returns DOST_DATABASE_URL when set, and otherwise a URL built from the DOST_DB_* components.
func DSN(env config.Env) (string, error) {
	if env.DatabaseURL != "" {
		return env.DatabaseURL, nil
	}
	var missing []string
	if env.DBUser == "" {
		missing = append(missing, "DOST_DB_USER")
	}
	if env.DBName == "" {
		missing = append(missing, "DOST_DB_NAME")
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%s required when DOST_DATABASE_URL is not set", strings.Join(missing, " and "))
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(env.DBUser, env.DBPassword),
		Host:   net.JoinHostPort(env.DBHost, strconv.Itoa(env.DBPort)),
		Path:   "/" + env.DBName,
	}
	return u.String(), nil
}

// Open connects to dsn, retrying for up to a minute, prepares and verifies the schema, and returns the pool.
func Open(ctx context.Context, dsn string, clk clock.Clock, log *slog.Logger) (*DB, error) {
	cfg, err := poolConfig(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := connect(ctx, cfg.ConnConfig, clk, log)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	defer conn.Close(context.Background())
	if err := prepare(ctx, conn); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// poolConfig parses dsn and bounds each connection attempt when dsn sets no connect_timeout.
func poolConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = connectTimeout
	}
	return cfg, nil
}

func connect(ctx context.Context, cfg *pgx.ConnConfig, clk clock.Clock, log *slog.Logger) (*pgx.Conn, error) {
	deadline := clk.Now().Add(connectWindow)
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err == nil {
			return conn, nil
		}
		left := deadline.Sub(clk.Now())
		if left <= 0 {
			return nil, err
		}
		wait := min(backoff, left)
		log.Warn("database connection failed; retrying", "attempt", attempt, "retry_in", wait, "error", err)
		if sleepErr := clock.Sleep(ctx, clk, wait); sleepErr != nil {
			return nil, errors.Join(sleepErr, err)
		}
		backoff = min(2*backoff, 10*time.Second)
	}
}

// Close closes every connection in the pool.
func (db *DB) Close() { db.pool.Close() }

// WithTx runs fn in a READ COMMITTED transaction that commits when fn returns nil and rolls back when it
// returns an error or panics.
func (db *DB) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}
