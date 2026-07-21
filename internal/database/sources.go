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

// TrackingEntry describes one source's automatic watch/refresh eligibility
// in a vault together with its newest materialized snapshot there.
type TrackingEntry struct {
	Source
	SnapshotDate string
	Active       bool
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

// TouchSource refreshes an already-registered source without creating a new
// row. The watcher uses this after pinning the source identity at startup.
func TouchSource(ctx context.Context, q Querier, sourceID int64, canonicalPath, nowUTC string) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sources SET last_seen_at_utc = ?
		WHERE source_id = ? AND canonical_path = ?`,
		nowUTC, sourceID, canonicalPath)
	if err != nil {
		return fmt.Errorf("touch source %s: %w", canonicalPath, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count touched source rows: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("registered source %s no longer exists", canonicalPath)
	}
	return nil
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
	// TrackingActive is null when this source has never been materialized in
	// the configured vault. A missing watch_tracking row means active for
	// sources imported before explicit tracking state was introduced.
	TrackingActive sql.NullBool
	// Current compares database intent with the last recorded write; it does
	// not describe a live filesystem check.
	Current bool
}

// ListSources returns every source with its most recent snapshot and, when
// one exists in the given vault, that snapshot's materialization.
func ListSources(ctx context.Context, q Querier, vaultID int64) ([]ListEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT
		    s.display_path,
		    sn.snapshot_date,
		    m.relative_path,
		    CASE WHEN EXISTS (
		        SELECT 1
		        FROM snapshots AS tracked_sn
		        JOIN materializations AS tracked_m
		            ON tracked_m.snapshot_id = tracked_sn.snapshot_id
		        WHERE tracked_sn.source_id = s.source_id
		          AND tracked_m.vault_id = ?
		    ) THEN COALESCE((
		        SELECT wt.active
		        FROM watch_tracking AS wt
		        WHERE wt.source_id = s.source_id AND wt.vault_id = ?
		    ), 1) END,
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
		ORDER BY s.display_path`, vaultID, vaultID, vaultID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var entries []ListEntry
	for rows.Next() {
		var e ListEntry
		if err := rows.Scan(&e.DisplayPath, &e.SnapshotDate, &e.RelativePath, &e.TrackingActive, &e.Current); err != nil {
			return nil, fmt.Errorf("scan list row: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// WatchCandidate is one source eligible for a vault's watch session together
// with its newest materialized snapshot inside the discovery window.
type WatchCandidate struct {
	Source
	SnapshotDate string
	ContentSHA   string
}

// SetWatchActive records whether a source remains eligible for automatic
// watching and refresh in a vault. Explicit imports reactivate a source;
// explicit untracking deactivates it without deleting any journal history.
func SetWatchActive(ctx context.Context, q Querier, sourceID, vaultID int64, active bool, nowUTC string) error {
	value := 0
	if active {
		value = 1
	}
	_, err := q.ExecContext(ctx, `INSERT INTO watch_tracking (source_id, vault_id, active, updated_at_utc)
		VALUES (?, ?, ?, ?) ON CONFLICT (source_id, vault_id) DO UPDATE SET active = excluded.active, updated_at_utc = excluded.updated_at_utc`, sourceID, vaultID, value, nowUTC)
	if err != nil {
		return fmt.Errorf("set watch tracking for source %d: %w", sourceID, err)
	}
	return nil
}

// IsWatchActive reports the vault-scoped automatic-tracking state. Sources
// without an explicit row predate watch_tracking and remain active by default.
func IsWatchActive(ctx context.Context, q Querier, sourceID, vaultID int64) (bool, error) {
	var active bool
	err := q.QueryRowContext(ctx, `SELECT COALESCE((
		SELECT active FROM watch_tracking WHERE source_id = ? AND vault_id = ?
	), 1)`, sourceID, vaultID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("get watch tracking for source %d: %w", sourceID, err)
	}
	return active, nil
}

// ListTrackingEntries returns every source ever materialized in vaultKey with
// its current automatic-tracking state and newest materialized snapshot date.
func ListTrackingEntries(ctx context.Context, q Querier, vaultKey string) ([]TrackingEntry, error) {
	return selectTrackingEntries(ctx, q, vaultKey, "", "")
}

// FindTrackingEntriesByPath returns sources materialized in vaultKey whose
// canonical identity matches canonicalPath or whose last display path matches
// displayPath. Multiple rows are possible when a reused symlink display path
// has referred to more than one canonical source over time.
func FindTrackingEntriesByPath(ctx context.Context, q Querier, vaultKey, canonicalPath, displayPath string) ([]TrackingEntry, error) {
	return selectTrackingEntries(ctx, q, vaultKey, canonicalPath, displayPath)
}

func selectTrackingEntries(ctx context.Context, q Querier, vaultKey, canonicalPath, displayPath string) ([]TrackingEntry, error) {
	pathFilter := ""
	args := []any{vaultKey}
	if canonicalPath != "" || displayPath != "" {
		pathFilter = " AND (s.canonical_path = ? OR s.display_path = ?)"
		args = append(args, canonicalPath, displayPath)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT
		    s.source_id,
		    s.canonical_path,
		    s.display_path,
		    MAX(sn.snapshot_date),
		    COALESCE(wt.active, 1)
		FROM sources AS s
		JOIN snapshots AS sn ON sn.source_id = s.source_id
		JOIN materializations AS m ON m.snapshot_id = sn.snapshot_id
		JOIN vaults AS v ON v.vault_id = m.vault_id
		LEFT JOIN watch_tracking AS wt
		    ON wt.source_id = s.source_id AND wt.vault_id = v.vault_id
		WHERE v.vault_key = ?`+pathFilter+`
		GROUP BY s.source_id, s.canonical_path, s.display_path, wt.active
		ORDER BY s.canonical_path`, args...)
	if err != nil {
		return nil, fmt.Errorf("select tracking entries for vault %s: %w", vaultKey, err)
	}
	defer rows.Close()

	var entries []TrackingEntry
	for rows.Next() {
		var entry TrackingEntry
		if err := rows.Scan(
			&entry.ID,
			&entry.CanonicalPath,
			&entry.DisplayPath,
			&entry.SnapshotDate,
			&entry.Active,
		); err != nil {
			return nil, fmt.Errorf("scan tracking entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// SelectWatchCandidates returns sources whose newest snapshot inside the
// inclusive date range was materialized in vaultKey. A snapshot belonging
// only to another vault never confers watch membership.
func SelectWatchCandidates(ctx context.Context, q Querier, vaultKey, fromDate, toDate string) ([]WatchCandidate, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT
		    s.source_id,
		    s.canonical_path,
		    s.display_path,
		    sn.snapshot_date,
		    r.content_sha256
		FROM sources AS s
		JOIN snapshots AS sn
		    ON sn.source_id = s.source_id
		JOIN revisions AS r
		    ON r.revision_id = sn.revision_id
		JOIN materializations AS m
		    ON m.snapshot_id = sn.snapshot_id
		JOIN vaults AS v
		    ON v.vault_id = m.vault_id
		LEFT JOIN watch_tracking AS wt ON wt.source_id = s.source_id AND wt.vault_id = v.vault_id
		WHERE v.vault_key = ?
		  AND COALESCE(wt.active, 1) = 1
		  AND sn.snapshot_date >= ?
		  AND sn.snapshot_date <= ?
		  AND sn.snapshot_date = (
		      SELECT MAX(sn2.snapshot_date)
		      FROM snapshots AS sn2
		      JOIN materializations AS m2
		          ON m2.snapshot_id = sn2.snapshot_id
		      WHERE sn2.source_id = s.source_id
		        AND m2.vault_id = v.vault_id
		        AND sn2.snapshot_date >= ?
		        AND sn2.snapshot_date <= ?)
		ORDER BY s.canonical_path`, vaultKey, fromDate, toDate, fromDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("select watch candidates for vault %s: %w", vaultKey, err)
	}
	defer rows.Close()

	var candidates []WatchCandidate
	for rows.Next() {
		var candidate WatchCandidate
		if err := rows.Scan(
			&candidate.ID,
			&candidate.CanonicalPath,
			&candidate.DisplayPath,
			&candidate.SnapshotDate,
			&candidate.ContentSHA,
		); err != nil {
			return nil, fmt.Errorf("scan watch candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// SelectAllWatchCandidates returns every source ever materialized in vaultKey,
// together with its newest materialized snapshot in that vault.
func SelectAllWatchCandidates(ctx context.Context, q Querier, vaultKey string) ([]WatchCandidate, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT
		    s.source_id,
		    s.canonical_path,
		    s.display_path,
		    sn.snapshot_date,
		    r.content_sha256
		FROM sources AS s
		JOIN snapshots AS sn
		    ON sn.source_id = s.source_id
		JOIN revisions AS r
		    ON r.revision_id = sn.revision_id
		JOIN materializations AS m
		    ON m.snapshot_id = sn.snapshot_id
		JOIN vaults AS v
		    ON v.vault_id = m.vault_id
		LEFT JOIN watch_tracking AS wt ON wt.source_id = s.source_id AND wt.vault_id = v.vault_id
		WHERE v.vault_key = ?
		  AND COALESCE(wt.active, 1) = 1
		  AND sn.snapshot_date = (
		      SELECT MAX(sn2.snapshot_date)
		      FROM snapshots AS sn2
		      JOIN materializations AS m2
		          ON m2.snapshot_id = sn2.snapshot_id
		      WHERE sn2.source_id = s.source_id
		        AND m2.vault_id = v.vault_id)
		ORDER BY s.canonical_path`, vaultKey)
	if err != nil {
		return nil, fmt.Errorf("select all watch candidates for vault %s: %w", vaultKey, err)
	}
	defer rows.Close()

	var candidates []WatchCandidate
	for rows.Next() {
		var candidate WatchCandidate
		if err := rows.Scan(
			&candidate.ID,
			&candidate.CanonicalPath,
			&candidate.DisplayPath,
			&candidate.SnapshotDate,
			&candidate.ContentSHA,
		); err != nil {
			return nil, fmt.Errorf("scan all watch candidates: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}
