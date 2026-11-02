package pg

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EmbeddingDims is the length of every stored embedding.
const EmbeddingDims = 1536

type column struct {
	name  string
	types []string // accepted pg_type names
}

// table is an existing table that melinda shares: created as ddl when missing, otherwise only verified.
type table struct {
	name    string
	ddl     string
	columns []column
}

var (
	integer     = []string{"int4"}
	varchar     = []string{"varchar"}
	jsonb       = []string{"jsonb"}
	anyJSON     = []string{"json", "jsonb"}
	timestamptz = []string{"timestamptz"}
	uuid        = []string{"uuid"}
	vector      = []string{"vector"}
)

// sharedTables are in dependency order.
var sharedTables = []table{
	{
		name: "chat_sessions",
		ddl: `CREATE TABLE chat_sessions (
    id SERIAL NOT NULL,
    key VARCHAR(256) NOT NULL,
    value JSONB NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY (id)
);
CREATE UNIQUE INDEX ix_chat_sessions_key ON chat_sessions (key);`,
		columns: []column{{"id", integer}, {"key", varchar}, {"value", jsonb}, {"created_at", timestamptz}, {"updated_at", timestamptz}},
	},
	{
		name: "key_value_store",
		ddl: `CREATE TABLE key_value_store (
    id SERIAL NOT NULL,
    type VARCHAR(128) NOT NULL,
    key VARCHAR(256) NOT NULL,
    value JSONB NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY (id)
);
CREATE INDEX ix_gin_key_value_store_value ON key_value_store USING gin (value);
CREATE UNIQUE INDEX ix_key_value_store_key ON key_value_store (key);`,
		columns: []column{{"id", integer}, {"type", varchar}, {"key", varchar}, {"value", jsonb}, {"created_at", timestamptz}, {"updated_at", timestamptz}},
	},
	{
		name: "langchain_pg_collection",
		ddl: `CREATE TABLE langchain_pg_collection (
    uuid UUID NOT NULL,
    name VARCHAR NOT NULL,
    cmetadata JSONB,
    PRIMARY KEY (uuid),
    UNIQUE (name)
);`,
		columns: []column{{"uuid", uuid}, {"name", varchar}, {"cmetadata", anyJSON}},
	},
	{
		name: "langchain_pg_embedding",
		ddl: `CREATE TABLE langchain_pg_embedding (
    id VARCHAR NOT NULL,
    collection_id UUID,
    embedding VECTOR(1536),
    document VARCHAR,
    cmetadata JSONB,
    PRIMARY KEY (id),
    FOREIGN KEY(collection_id) REFERENCES langchain_pg_collection (uuid) ON DELETE CASCADE
);
CREATE INDEX ix_cmetadata_gin ON langchain_pg_embedding USING gin (cmetadata jsonb_path_ops);
CREATE INDEX ix_vector_hnsw ON langchain_pg_embedding USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);`,
		columns: []column{{"id", varchar}, {"collection_id", uuid}, {"embedding", vector}, {"document", varchar}, {"cmetadata", anyJSON}},
	},
}

// prepare creates what is missing, verifies the shared tables, and applies migrations. It holds an advisory
// lock so that processes starting together run it one at a time; closing conn releases the lock.
func prepare(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext('melinda_schema_migrations'))"); err != nil {
		return fmt.Errorf("lock schema: %w", err)
	}
	if err := bootstrap(ctx, conn); err != nil {
		return err
	}
	if err := verify(ctx, conn); err != nil {
		return err
	}
	return migrate(ctx, conn, migrations)
}

// bootstrap creates the vector extension and each shared table, with its indexes, that does not exist.
// Existing tables are left exactly as they are.
func bootstrap(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("create extension vector: %w", err)
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, t := range sharedTables {
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", t.name).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			if _, err := tx.Exec(ctx, t.ddl); err != nil {
				return fmt.Errorf("create table %s: %w", t.name, err)
			}
		}
		return nil
	})
}

// verify reports every way the shared tables differ from what melinda reads and writes.
func verify(ctx context.Context, conn *pgx.Conn) error {
	var problems []string
	for _, t := range sharedTables {
		rows, _ := conn.Query(ctx, `
			SELECT a.attname, t.typname, a.atttypmod
			FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
			WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped`, t.name)
		type found struct {
			typ    string
			typmod int32
		}
		cols := map[string]found{}
		var f found
		var name string
		if _, err := pgx.ForEachRow(rows, []any{&name, &f.typ, &f.typmod}, func() error {
			cols[name] = f
			return nil
		}); err != nil {
			return fmt.Errorf("read columns of %s: %w", t.name, err)
		}
		if len(cols) == 0 {
			problems = append(problems, t.name+" is missing")
			continue
		}
		for _, c := range t.columns {
			got, ok := cols[c.name]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s.%s is missing", t.name, c.name))
			case !slices.Contains(c.types, got.typ):
				problems = append(problems, fmt.Sprintf("%s.%s is %s, want %s", t.name, c.name, got.typ, strings.Join(c.types, " or ")))
			// A vector column without a declared dimension (typmod -1) holds 1536-dimension vectors too.
			case got.typ == "vector" && got.typmod != -1 && got.typmod != EmbeddingDims:
				problems = append(problems, fmt.Sprintf("%s.%s is vector(%d), want vector(%d)", t.name, c.name, got.typmod, EmbeddingDims))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("database schema does not match: %s", strings.Join(problems, "; "))
	}
	return nil
}
