package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const schemaVersionKey = "schema_version"

// migrate ensures the metadata table exists and applies every embedded
// migration with a number greater than the recorded schema version, each in
// its own transaction.
func migrate(ctx context.Context, sq *sql.DB) error {
	if _, err := sq.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS metadata (
		    key TEXT PRIMARY KEY,
		    value TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("ensure metadata table: %w", err)
	}

	current, err := schemaVersion(ctx, sq)
	if err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		num, err := migrationNumber(name)
		if err != nil {
			return err
		}
		if num <= current {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := applyMigration(ctx, sq, num, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		current = num
	}
	return nil
}

func applyMigration(ctx context.Context, sq *sql.DB, num int, body string) error {
	tx, err := sq.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO metadata (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		schemaVersionKey, strconv.Itoa(num)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrationNumber extracts the leading integer from a name like 001_initial.sql.
func migrationNumber(name string) (int, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("migration %s: name must look like 001_description.sql", name)
	}
	num, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("migration %s: invalid numeric prefix: %w", name, err)
	}
	return num, nil
}

func schemaVersion(ctx context.Context, q Querier) (int, error) {
	var value string
	err := q.QueryRowContext(ctx,
		`SELECT value FROM metadata WHERE key = ?`, schemaVersionKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	num, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid schema version %q: %w", value, err)
	}
	return num, nil
}
