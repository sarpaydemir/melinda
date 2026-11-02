// Package testdb gives each test its own PostgreSQL database on the server named by MELINDA_TEST_DATABASE_URL.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvURL names the variable holding a postgres:// URL of a server where tests may create databases.
const EnvURL = "MELINDA_TEST_DATABASE_URL"

// New creates an empty, uniquely named database, drops it when the test ends, and returns its URL. It skips
// the test when EnvURL is unset.
func New(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvURL)
	if base == "" {
		t.Skipf("%s is not set", EnvURL)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("%s must be a postgres:// URL", EnvURL)
	}
	name := dbName(t.Name())
	exec(t, base, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	t.Cleanup(func() { exec(t, base, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)") })
	u.Path = "/" + name
	return u.String()
}

// dbName derives a database name from a test name; it is unique per call and fits PostgreSQL's 63-byte limit.
func dbName(testName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(testName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return "dost_test_" + b.String()[:min(b.Len(), 44)] + "_" + hex.EncodeToString(suffix)
}

func exec(t testing.TB, dsn, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvURL, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
