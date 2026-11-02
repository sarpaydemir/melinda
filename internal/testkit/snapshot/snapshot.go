// Package snapshot opens a read-only connection to a copy of a deployment's database named by DOST_COMPAT_DSN,
// for tests that check dost reads the data already stored there.
package snapshot

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/melinda/internal/config"
)

const (
	// EnvDSN names the variable holding the postgres:// URL of the database copy.
	EnvDSN = "DOST_COMPAT_DSN"
	// EnvSearches names the variable holding the path of the recorded searches, one JSON object per line.
	EnvSearches = "DOST_COMPAT_SEARCHES"
)

// Open returns a connection to the copy on which every transaction is read-only, and the settings the copy is
// read with: those in DOST_SETTINGS_FILE, if set, for the agent DOST_AGENT_ID (default dost). It skips the test
// when EnvDSN is unset.
func Open(t testing.TB) (*pgx.Conn, *config.Settings) {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("%s is not set", EnvDSN)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("%s: %v", EnvDSN, err)
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn, settings(t)
}

func settings(t testing.TB) *config.Settings {
	doc := map[string]json.RawMessage{}
	if path := os.Getenv("DOST_SETTINGS_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &doc)
		}
		if err != nil {
			t.Fatalf("DOST_SETTINGS_FILE: %v", err)
		}
	}
	if _, ok := doc["model"]; !ok {
		doc["model"] = json.RawMessage(`"unused"`)
	}
	agent := os.Getenv("DOST_AGENT_ID")
	if agent == "" {
		agent = "dost"
	}
	s, err := config.Load(doc, agent, nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// CollectionID returns the uuid of the named collection, or "" when the copy has none.
func CollectionID(t testing.TB, conn *pgx.Conn, name string) string {
	t.Helper()
	var id string
	err := conn.QueryRow(context.Background(), "SELECT uuid::text FROM langchain_pg_collection WHERE name = $1", name).Scan(&id)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	return id
}
