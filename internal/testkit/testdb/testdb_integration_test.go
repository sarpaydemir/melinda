//go:build integration

package testdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestNewCreatesAndDropsOnlyItsDatabase(t *testing.T) {
	ctx := context.Background()
	var name string
	t.Run("owner", func(t *testing.T) {
		dsn := New(t)
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(dsn)
		if want := strings.TrimPrefix(u.Path, "/"); name != want {
			t.Errorf("connected to %q, want %q", name, want)
		}
	})
	if name == "" {
		t.Fatal("owner subtest did not connect")
	}

	admin, err := pgx.Connect(ctx, os.Getenv(EnvURL))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var dropped, adminDB bool
	if err := admin.QueryRow(ctx,
		"SELECT NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1), EXISTS (SELECT 1 FROM pg_database WHERE datname = current_database())",
		name).Scan(&dropped, &adminDB); err != nil {
		t.Fatal(err)
	}
	if !dropped || !adminDB {
		t.Errorf("after cleanup: test database dropped = %v, server database kept = %v; want both", dropped, adminDB)
	}
}
