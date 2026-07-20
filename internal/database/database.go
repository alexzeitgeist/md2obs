// Package database wraps the SQLite driver behind a small repository layer.
// The driver import is confined to this package so it can be replaced later.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB owns the SQLite connection pool for one state database.
type DB struct {
	Path string
	sq   *sql.DB
}

// Querier is satisfied by both *sql.DB and *sql.Tx so repository functions
// can run inside or outside an explicit transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open opens (creating if needed) the state database at path and applies
// pending migrations. _txlock=immediate makes every write transaction start
// as BEGIN IMMEDIATE.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	dsn := "file:" + url.PathEscape(path) +
		"?_txlock=immediate" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	sq, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// A single connection serializes writes for this local CLI.
	sq.SetMaxOpenConns(1)
	if err := migrate(ctx, sq); err != nil {
		sq.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return &DB{Path: path, sq: sq}, nil
}

func (d *DB) Close() error { return d.sq.Close() }

// Begin starts a write transaction (BEGIN IMMEDIATE via the DSN txlock).
func (d *DB) Begin(ctx context.Context) (*sql.Tx, error) {
	return d.sq.BeginTx(ctx, nil)
}

// Query exposes the pool for read-only repository calls.
func (d *DB) Query() Querier { return d.sq }

// SchemaVersion reports the current schema version from the metadata table.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	return schemaVersion(ctx, d.sq)
}

// Counts returns the number of sources, snapshots, and materializations.
func (d *DB) Counts(ctx context.Context) (sources, snapshots, materializations int, err error) {
	row := d.sq.QueryRowContext(ctx, `
		SELECT
		    (SELECT COUNT(*) FROM sources),
		    (SELECT COUNT(*) FROM snapshots),
		    (SELECT COUNT(*) FROM materializations)`)
	err = row.Scan(&sources, &snapshots, &materializations)
	return
}
