package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Source is one explicitly imported file, identified by its canonical path.
type Source struct {
	ID            int64
	CanonicalPath string
	DisplayPath   string
}

// UpsertSource registers a source or refreshes its display path and
// last-seen timestamp, returning the source id either way.
func UpsertSource(ctx context.Context, q Querier, canonicalPath, displayPath, nowUTC string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO sources (canonical_path, display_path, first_seen_at_utc, last_seen_at_utc)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (canonical_path) DO UPDATE SET
		    display_path = excluded.display_path,
		    last_seen_at_utc = excluded.last_seen_at_utc
		RETURNING source_id`,
		canonicalPath, displayPath, nowUTC, nowUTC).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert source %s: %w", canonicalPath, err)
	}
	return id, nil
}

// GetSourceByPath returns nil when the canonical path is not registered.
func GetSourceByPath(ctx context.Context, q Querier, canonicalPath string) (*Source, error) {
	s := Source{CanonicalPath: canonicalPath}
	err := q.QueryRowContext(ctx, `
		SELECT source_id, display_path FROM sources WHERE canonical_path = ?`,
		canonicalPath).Scan(&s.ID, &s.DisplayPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get source %s: %w", canonicalPath, err)
	}
	return &s, nil
}

// ListEntry is one row of `md2obs list`: a source with its latest snapshot.
type ListEntry struct {
	DisplayPath  string
	SnapshotDate string
	RelativePath sql.NullString
	Current      bool
}

// ListSources returns every source with its most recent snapshot and, when
// one exists in the given vault, that snapshot's materialization.
func ListSources(ctx context.Context, q Querier, vaultID int64) ([]ListEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT
		    s.display_path,
		    sn.snapshot_date,
		    m.relative_path,
		    COALESCE(m.written_revision_id = sn.revision_id, 0)
		FROM sources AS s
		JOIN snapshots AS sn
		    ON sn.snapshot_id = (
		        SELECT snapshot_id FROM snapshots
		        WHERE source_id = s.source_id
		        ORDER BY snapshot_date DESC
		        LIMIT 1)
		LEFT JOIN materializations AS m
		    ON m.snapshot_id = sn.snapshot_id AND m.vault_id = ?
		ORDER BY s.display_path`, vaultID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var entries []ListEntry
	for rows.Next() {
		var e ListEntry
		if err := rows.Scan(&e.DisplayPath, &e.SnapshotDate, &e.RelativePath, &e.Current); err != nil {
			return nil, fmt.Errorf("scan list row: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// SelectSourcesWithSnapshotsBetween returns distinct sources that have at
// least one snapshot inside the inclusive date range. Used by `watch`.
func SelectSourcesWithSnapshotsBetween(ctx context.Context, q Querier, fromDate, toDate string) ([]Source, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT
		    s.source_id,
		    s.canonical_path,
		    s.display_path
		FROM sources AS s
		JOIN snapshots AS sn
		    ON sn.source_id = s.source_id
		WHERE sn.snapshot_date >= ?
		  AND sn.snapshot_date <= ?
		ORDER BY s.canonical_path`, fromDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("select watch sources: %w", err)
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.ID, &s.CanonicalPath, &s.DisplayPath); err != nil {
			return nil, fmt.Errorf("scan watch source: %w", err)
		}
		sources = append(sources, s)
	}
	return sources, rows.Err()
}
