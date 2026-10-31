//go:build integration

package pg

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/testkit/golden"
	"github.com/sarpaydemir/dost/internal/testkit/testdb"
)

var allTables = []string{"chat_sessions", "key_value_store", "langchain_pg_collection", "langchain_pg_embedding"}

func open(t *testing.T, dsn string) *DB {
	t.Helper()
	db, err := Open(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
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

func execSQL(t *testing.T, dsn, sql string) {
	t.Helper()
	if _, err := dial(t, dsn).Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func queryStrings(t *testing.T, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, _ := conn.Query(context.Background(), sql, args...)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// describe renders the columns, constraints, and indexes of tables as PostgreSQL reports them.
func describe(t *testing.T, dsn string, tables ...string) string {
	t.Helper()
	conn := dial(t, dsn)
	var lines []string
	lines = append(lines, queryStrings(t, conn, `
		SELECT c.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
		       || CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
		       || COALESCE(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE c.relname = ANY($1) AND c.relnamespace = current_schema()::regnamespace
		  AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY c.relname, a.attnum`, tables)...)
	lines = append(lines, queryStrings(t, conn, `
		SELECT conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid::regclass::text = ANY($1) AND contype IN ('p', 'u', 'f', 'c')
		ORDER BY conrelid::regclass::text, conname`, tables)...)
	lines = append(lines, queryStrings(t, conn, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = ANY($1)
		ORDER BY tablename, indexname`, tables)...)
	return strings.Join(lines, "\n") + "\n"
}

func TestOpenCreatesTheExactSchemaInAnEmptyDatabase(t *testing.T) {
	dsn := testdb.New(t)
	open(t, dsn)
	golden.Assert(t, "empty_database_schema", []byte(describe(t, dsn, allTables...)))

	conn := dial(t, dsn)
	if got := queryStrings(t, conn, "SELECT extname::text FROM pg_extension WHERE extname = 'vector'"); len(got) != 1 {
		t.Error("vector extension not created")
	}
	if got := queryStrings(t, conn, "SELECT version::text FROM dost_schema_migrations ORDER BY version"); len(got) == 0 || got[0] != "1" {
		t.Errorf("recorded migrations = %v, want the embedded ones starting at 1", got)
	}
}

func TestOpenLeavesExistingTablesAsTheyAre(t *testing.T) {
	cases := map[string]string{
		"without ix_vector_hnsw": `
			CREATE EXTENSION IF NOT EXISTS vector;
			CREATE TABLE langchain_pg_collection (
			    uuid UUID NOT NULL, name VARCHAR NOT NULL, cmetadata JSONB, PRIMARY KEY (uuid), UNIQUE (name));
			CREATE TABLE langchain_pg_embedding (
			    id VARCHAR NOT NULL, collection_id UUID, embedding VECTOR(1536), document VARCHAR, cmetadata JSONB,
			    PRIMARY KEY (id),
			    FOREIGN KEY (collection_id) REFERENCES langchain_pg_collection (uuid) ON DELETE CASCADE);
			CREATE INDEX ix_cmetadata_gin ON langchain_pg_embedding USING gin (cmetadata jsonb_path_ops);`,
		"json metadata, undeclared dimension, extra index": `
			CREATE EXTENSION IF NOT EXISTS vector;
			CREATE TABLE langchain_pg_collection (uuid UUID PRIMARY KEY, name VARCHAR NOT NULL UNIQUE, cmetadata JSON);
			CREATE TABLE langchain_pg_embedding (
			    id VARCHAR PRIMARY KEY, collection_id UUID REFERENCES langchain_pg_collection (uuid) ON DELETE CASCADE,
			    embedding VECTOR, document VARCHAR, cmetadata JSON);
			CREATE UNIQUE INDEX ix_langchain_pg_embedding_id ON langchain_pg_embedding (id);`,
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			dsn := testdb.New(t)
			execSQL(t, dsn, fixture)
			existing := []string{"langchain_pg_collection", "langchain_pg_embedding"}
			before := describe(t, dsn, existing...)

			open(t, dsn)
			if after := describe(t, dsn, existing...); after != before {
				t.Errorf("existing tables changed\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if got := describe(t, dsn, "chat_sessions", "key_value_store"); !strings.Contains(got, "ix_key_value_store_key") {
				t.Errorf("missing tables were not created:\n%s", got)
			}
		})
	}
}

func TestOpenReportsEveryMismatchInOneError(t *testing.T) {
	dsn := testdb.New(t)
	execSQL(t, dsn, `
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE chat_sessions (id SERIAL PRIMARY KEY, key VARCHAR(256) NOT NULL, value JSONB NOT NULL,
		    created_at TIMESTAMPTZ NOT NULL);
		CREATE TABLE key_value_store (id SERIAL PRIMARY KEY, type CHAR(128) NOT NULL, key VARCHAR(256) NOT NULL,
		    value JSON NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
		CREATE TABLE langchain_pg_embedding (id VARCHAR PRIMARY KEY, collection_id UUID, embedding VECTOR(768),
		    document VARCHAR, cmetadata JSONB);`)

	db, err := Open(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded on a mismatched schema")
	}
	for _, want := range []string{
		"chat_sessions.updated_at is missing",
		"key_value_store.type is bpchar, want varchar",
		"key_value_store.value is json, want jsonb",
		"langchain_pg_embedding.embedding is vector(768), want vector(1536)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not report %q", err, want)
		}
	}
}

func TestVerifyReportsAMissingSchemaAndCreatesNothing(t *testing.T) {
	dsn := testdb.New(t)
	err := Verify(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
	want := "database schema does not match: chat_sessions is missing; key_value_store is missing; " +
		"langchain_pg_collection is missing; langchain_pg_embedding is missing"
	if err == nil || err.Error() != want {
		t.Errorf("Verify = %v, want %q", err, want)
	}
	conn := dial(t, dsn)
	if got := queryStrings(t, conn, `SELECT relname::text FROM pg_class WHERE relnamespace = 'public'::regnamespace`); len(got) > 0 {
		t.Errorf("Verify created %q", got)
	}
	if got := queryStrings(t, conn, `SELECT extname::text FROM pg_extension WHERE extname = 'vector'`); len(got) > 0 {
		t.Error("Verify created the vector extension")
	}
}

func TestRejectedOnlyForStatementsTheServerRefuses(t *testing.T) {
	dsn := testdb.New(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		change func(*url.URL)
		sql    string
		code   string
		want   bool
	}{
		{"password rotated", func(u *url.URL) { u.User = url.UserPassword(u.User.Username(), "rotated") },
			"SELECT 1", "28P01", false},
		{"database not there during a failover", func(u *url.URL) { u.Path = "/dost_missing_database" },
			"SELECT 1", "3D000", false},
		{"server turned read-only by a failover", func(u *url.URL) { u.RawQuery += "&default_transaction_read_only=on" },
			"CREATE TABLE written (n integer)", "25006", false},
		{"statement the server refuses", func(*url.URL) {}, "SELECT 1/0", "22012", true},
	}
	for _, c := range cases {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		c.change(u)
		pool, err := pgxpool.New(ctx, u.String())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		err = (&DB{pool: pool}).WithTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, c.sql)
			return err
		})
		pool.Close()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != c.code {
			t.Errorf("%s: error %v, want SQLSTATE %s", c.name, err, c.code)
		}
		if got := Rejected(err); got != c.want {
			t.Errorf("%s: Rejected = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWithTx(t *testing.T) {
	dsn := testdb.New(t)
	execSQL(t, dsn, `
		CREATE TABLE scratch (n integer);
		DO $$ BEGIN
		    EXECUTE format('ALTER DATABASE %I SET default_transaction_isolation = serializable', current_database());
		END $$;`)
	db := open(t, dsn)
	ctx := context.Background()
	insert := func(n int) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO scratch VALUES ($1)", n)
			return err
		}
	}

	if err := db.WithTx(ctx, insert(1)); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	failure := errors.New("fn failed")
	err := db.WithTx(ctx, func(tx pgx.Tx) error {
		_ = insert(2)(tx)
		return failure
	})
	if !errors.Is(err, failure) {
		t.Errorf("WithTx error = %v, want the error fn returned", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic in fn did not propagate")
			}
		}()
		_ = db.WithTx(ctx, func(tx pgx.Tx) error {
			_ = insert(3)(tx)
			panic("boom")
		})
	}()
	var level string
	if err := db.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&level)
	}); err != nil || level != "read committed" {
		t.Errorf("isolation = %q, %v; want read committed", level, err)
	}

	if got := queryStrings(t, dial(t, dsn), "SELECT n::text FROM scratch ORDER BY n"); strings.Join(got, ",") != "1" {
		t.Errorf("committed rows = %v, want only the one from the transaction that returned nil", got)
	}
}

func TestMigrateAppliesPendingMigrationsInOrderOnce(t *testing.T) {
	dsn := testdb.New(t)
	conn := dial(t, dsn)
	ctx := context.Background()
	first, err := fs.ReadFile(migrations, "migrations/0001_schema_migrations.sql")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"migrations/0001_schema_migrations.sql": {Data: first},
		"migrations/0002_create.sql":            {Data: []byte("CREATE TABLE dost_scratch (n integer)")},
		"migrations/0003_fill.sql":              {Data: []byte("INSERT INTO dost_scratch VALUES (3)")},
	}
	rows := func() string {
		return strings.Join(queryStrings(t, conn, "SELECT n::text FROM dost_scratch ORDER BY n"), ",")
	}
	versions := func() string {
		return strings.Join(queryStrings(t, conn, "SELECT version || ' ' || name FROM dost_schema_migrations ORDER BY version"), ",")
	}

	for range 2 {
		if err := migrate(ctx, conn, fsys); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	if got := rows(); got != "3" {
		t.Errorf("rows = %q, want 3 inserted once", got)
	}
	const applied = "1 0001_schema_migrations.sql,2 0002_create.sql,3 0003_fill.sql"
	if got := versions(); got != applied {
		t.Errorf("recorded = %q, want %q", got, applied)
	}

	fsys["migrations/0004_broken.sql"] = &fstest.MapFile{Data: []byte("INSERT INTO dost_scratch VALUES (4); SELECT * FROM no_such_table")}
	err = migrate(ctx, conn, fsys)
	if err == nil || !strings.Contains(err.Error(), "0004_broken.sql") {
		t.Errorf("migrate error = %v, want one naming the failed migration", err)
	}
	if got, gotVersions := rows(), versions(); got != "3" || gotVersions != applied {
		t.Errorf("after a failed migration: rows %q, recorded %q; want its changes rolled back and it unrecorded", got, gotVersions)
	}
}

func TestConcurrentOpensOnAnEmptyDatabaseAllSucceed(t *testing.T) {
	dsn := testdb.New(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			db, err := Open(context.Background(), dsn, clock.Real(), slog.New(slog.DiscardHandler))
			if err == nil {
				db.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Open %d: %v", i, err)
		}
	}
}
