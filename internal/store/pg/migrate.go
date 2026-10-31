package pg

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// migrations holds the additive schema changes for dost-owned objects, named NNNN_name.sql.
//
//go:embed migrations/*.sql
var migrations embed.FS

// migrate applies, in version order and each in its own transaction, the migrations/*.sql files in fsys that
// dost_schema_migrations does not list. Migration 1 creates that table.
func migrate(ctx context.Context, conn *pgx.Conn, fsys fs.FS) error {
	var recorded bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('dost_schema_migrations') IS NOT NULL").Scan(&recorded); err != nil {
		return err
	}
	var applied []int
	if recorded {
		rows, _ := conn.Query(ctx, "SELECT version FROM dost_schema_migrations")
		var err error
		if applied, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
			return fmt.Errorf("read applied migrations: %w", err)
		}
	}
	files, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return err
	}
	for _, file := range files {
		name := path.Base(file)
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("migration %s: name must start with its version number", name)
		}
		if slices.Contains(applied, version) {
			continue
		}
		sql, err := fs.ReadFile(fsys, file)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO dost_schema_migrations (version, name) VALUES ($1, $2)", version, name)
			return err
		}); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}
