package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
)

func TestDSNPrefersDatabaseURL(t *testing.T) {
	const url = "postgres://u:p@db:5432/x?sslmode=disable"
	got, err := DSN(config.Env{DatabaseURL: url, DBHost: "other", DBPort: 1, DBUser: "v", DBName: "y"})
	if err != nil || got != url {
		t.Errorf("DSN = %q, %v; want %q", got, err, url)
	}
}

func TestDSNEscapesComponents(t *testing.T) {
	cases := []config.Env{
		{DBHost: "db", DBPort: 6543, DBUser: "app", DBPassword: "p@ss/w:rd", DBName: "data"},
		{DBHost: "db", DBPort: 5432, DBUser: "a:b@c", DBPassword: "#?% &=+'\"\\", DBName: "my db?"},
		{DBHost: "::1", DBPort: 5432, DBUser: "app", DBName: "data"},
	}
	for _, env := range cases {
		dsn, err := DSN(env)
		if err != nil {
			t.Fatalf("DSN(%+v): %v", env, err)
		}
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse %q: %v", dsn, err)
		}
		got := []any{cfg.Host, int(cfg.Port), cfg.User, cfg.Password, cfg.Database}
		want := []any{env.DBHost, env.DBPort, env.DBUser, env.DBPassword, env.DBName}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("DSN %q parses to %q, want %q", dsn, got, want)
		}
	}
}

func TestDSNRequiresUserAndDatabaseName(t *testing.T) {
	cases := []struct {
		env     config.Env
		missing []string
		present []string
	}{
		{config.Env{DBHost: "db", DBPort: 5432}, []string{"DOST_DB_USER", "DOST_DB_NAME"}, nil},
		{config.Env{DBHost: "db", DBPort: 5432, DBUser: "app"}, []string{"DOST_DB_NAME"}, []string{"DOST_DB_USER"}},
		{config.Env{DBHost: "db", DBPort: 5432, DBName: "data"}, []string{"DOST_DB_USER"}, []string{"DOST_DB_NAME"}},
	}
	for _, c := range cases {
		_, err := DSN(c.env)
		if err == nil {
			t.Errorf("DSN(%+v) succeeded, want an error naming %v", c.env, c.missing)
			continue
		}
		for _, name := range c.missing {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("DSN(%+v) error %q does not name %s", c.env, err, name)
			}
		}
		for _, name := range c.present {
			if strings.Contains(err.Error(), name) {
				t.Errorf("DSN(%+v) error %q names %s, which is set", c.env, err, name)
			}
		}
	}
}

const secretPassword = "s3cret-pw"

var discard = slog.New(slog.DiscardHandler)

// listen passes each accepted connection to handle and returns a DSN that points at the listener.
func listen(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			handle(c)
		}
	}()
	return "postgres://u:" + secretPassword + "@" + ln.Addr().String() + "/x?sslmode=disable"
}

// closingServer closes every connection at once and counts them, so every connection attempt fails.
func closingServer(t *testing.T) (dsn string, attempts *atomic.Int32) {
	attempts = new(atomic.Int32)
	return listen(t, func(c net.Conn) {
		attempts.Add(1)
		c.Close()
	}), attempts
}

func TestOpenRetriesForSixtySecondsThenReturnsTheLastError(t *testing.T) {
	dsn, attempts := closingServer(t)
	clk := fakeclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	start := clk.Now()
	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := Open(context.Background(), dsn, clk, slog.New(slog.NewTextHandler(&logs, nil)))
		done <- err
	}()

	for clk.Now().Before(start.Add(connectWindow)) {
		clk.AwaitTimers(t, 1)
		select {
		case err := <-done:
			t.Fatalf("Open returned after %v: %v; want retries for %v", clk.Now().Sub(start), err, connectWindow)
		default:
		}
		clk.Advance(time.Second)
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("Open error = %v; want the last connection error", err)
	}
	if n := attempts.Load(); n < 3 {
		t.Errorf("%d connections, want several attempts", n)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) < 2 {
		t.Errorf("logs:\n%s\nwant a warning for each retry", logs.String())
	}
	for i, line := range lines {
		for _, want := range []string{"level=WARN", fmt.Sprintf("attempt=%d ", i+1), "error="} {
			if !strings.Contains(line, want) {
				t.Errorf("log line %d does not contain %q: %s", i+1, want, line)
			}
		}
	}
	if strings.Contains(logs.String(), secretPassword) {
		t.Errorf("logs contain the password:\n%s", logs.String())
	}
}

func TestConnectionAttemptsTimeOutByDefault(t *testing.T) {
	cases := map[string]time.Duration{
		"postgres://u@db/x":                   10 * time.Second,
		"postgres://u@db/x?connect_timeout=3": 3 * time.Second,
	}
	for dsn, want := range cases {
		cfg, err := poolConfig(dsn)
		if err != nil {
			t.Fatalf("poolConfig(%q): %v", dsn, err)
		}
		if got := cfg.ConnConfig.ConnectTimeout; got != want {
			t.Errorf("poolConfig(%q): connect timeout %v, want %v", dsn, got, want)
		}
	}
}

func TestOpenGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	accepted := make(chan net.Conn, 1)
	dsn := listen(t, func(c net.Conn) { accepted <- c }) + "&connect_timeout=1"
	clk := fakeclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	done := make(chan error, 1)
	go func() {
		_, err := Open(context.Background(), dsn, clk, discard)
		done <- err
	}()
	c := <-accepted
	defer c.Close()
	clk.Advance(connectWindow)

	limit := 5 * time.Second
	select {
	case err := <-done:
		if err == nil {
			t.Error("Open succeeded against a server that never answers")
		}
	case <-time.After(limit):
		t.Fatalf("Open still blocked %v after connecting to a server that never answers", limit)
	}
}

func TestOpenStopsRetryingWhenTheContextEnds(t *testing.T) {
	dsn, _ := closingServer(t)
	clk := fakeclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Open(ctx, dsn, clk, discard)
		done <- err
	}()
	clk.AwaitTimers(t, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Open error = %v, want context.Canceled", err)
	}
}

func TestMigrationsAreNumberedAndLeaveSharedTablesAlone(t *testing.T) {
	names, err := fs.Glob(migrations, "migrations/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations")
	}
	valid := regexp.MustCompile(`^migrations/(\d{4})_[a-z0-9_]+\.sql$`)
	for i, name := range names {
		m := valid.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%s: want migrations/NNNN_name.sql", name)
			continue
		}
		if want := fmt.Sprintf("%04d", i+1); m[1] != want {
			t.Errorf("%s: version %s, want %s (versions run 0001, 0002, ... without gaps)", name, m[1], want)
		}
		sql, err := fs.ReadFile(migrations, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range sharedTables {
			if regexp.MustCompile(`(?i)\b` + table.name + `\b`).Match(sql) {
				t.Errorf("%s mentions %s; migrations must not touch existing tables", name, table.name)
			}
		}
	}
}
