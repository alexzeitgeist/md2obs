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

// FindSourcesByPath returns sources whose canonical identity or last display
// path matches path. Multiple sources can share a reused symlink display path.
func FindSourcesByPath(ctx context.Context, q Querier, path string) ([]Source, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT source_id, canonical_path, display_path
		FROM sources
		WHERE canonical_path = ? OR display_path = ?
		ORDER BY canonical_path`, path, path)
	if err != nil {
		return nil, fmt.Errorf("find sources by path %s: %w", path, err)
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var source Source
		if err := rows.Scan(&source.ID, &source.CanonicalPath, &source.DisplayPath); err != nil {
			return nil, fmt.Errorf("scan source by path %s: %w", path, err)
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

// ListEntry is one row of `md2obs debug list`: a source with its latest snapshot.
type ListEntry struct {
	DisplayPath      string
	SnapshotDate     string
	RelativePath     string
	SourceCurrent    bool
	RenderingCurrent bool
}

// ListSources returns each source currently tracked in the given vault with
// its newest materialized snapshot there. Both state flags are database facts;
// this function performs no live filesystem checks.
func ListSources(ctx context.Context, q Querier, vaultID int64, desiredProfile string) ([]ListEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT
		    s.display_path,
		    sn.snapshot_date,
		    m.relative_path,
		    m.written_revision_id = sn.revision_id,
		    m.written_render_profile = ?
		FROM materializations AS m
		JOIN snapshots AS sn ON sn.snapshot_id = m.snapshot_id
		JOIN sources AS s ON s.source_id = sn.source_id
		WHERE m.vault_id = ?
		  AND sn.snapshot_date = (
		      SELECT MAX(sn2.snapshot_date)
		      FROM snapshots AS sn2
		      JOIN materializations AS m2 ON m2.snapshot_id = sn2.snapshot_id
		      WHERE sn2.source_id = s.source_id
		        AND m2.vault_id = m.vault_id)
		ORDER BY s.display_path`, desiredProfile, vaultID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var entries []ListEntry
	for rows.Next() {
		var e ListEntry
		if err := rows.Scan(
			&e.DisplayPath,
			&e.SnapshotDate,
			&e.RelativePath,
			&e.SourceCurrent,
			&e.RenderingCurrent,
		); err != nil {
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

// IsSourceTrackedInVault reports whether any materialization still associates
// a source with a vault. This predicate is also the watched-import race gate:
// untrack deletes every matching materialization in the same transaction that
// garbage-collects now-unreachable bookkeeping.
func IsSourceTrackedInVault(ctx context.Context, q Querier, sourceID, vaultID int64) (bool, error) {
	var tracked bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM snapshots AS sn
		JOIN materializations AS m ON m.snapshot_id = sn.snapshot_id
		WHERE sn.source_id = ? AND m.vault_id = ?
	)`, sourceID, vaultID).Scan(&tracked)
	if err != nil {
		return false, fmt.Errorf("check vault tracking for source %d: %w", sourceID, err)
	}
	return tracked, nil
}

// ListTrackingEntries returns every source currently materialized in vaultKey
// with its newest materialized snapshot date.
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
		    MAX(sn.snapshot_date)
		FROM sources AS s
		JOIN snapshots AS sn ON sn.source_id = s.source_id
		JOIN materializations AS m ON m.snapshot_id = sn.snapshot_id
		JOIN vaults AS v ON v.vault_id = m.vault_id
		WHERE v.vault_key = ?`+pathFilter+`
		GROUP BY s.source_id, s.canonical_path, s.display_path
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
	return selectWatchCandidates(ctx, q, vaultKey, fromDate, toDate)
}

// SelectAllWatchCandidates returns every source currently tracked in vaultKey,
// together with its newest materialized snapshot in that vault.
func SelectAllWatchCandidates(ctx context.Context, q Querier, vaultKey string) ([]WatchCandidate, error) {
	return selectWatchCandidates(ctx, q, vaultKey, "", "")
}

func selectWatchCandidates(ctx context.Context, q Querier, vaultKey, fromDate, toDate string) ([]WatchCandidate, error) {
	outerFilter, innerFilter := "", ""
	args := []any{vaultKey}
	if fromDate != "" || toDate != "" {
		outerFilter = `
		  AND sn.snapshot_date >= ?
		  AND sn.snapshot_date <= ?`
		innerFilter = `
		        AND sn2.snapshot_date >= ?
		        AND sn2.snapshot_date <= ?`
		args = append(args, fromDate, toDate, fromDate, toDate)
	}
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
		WHERE v.vault_key = ?`+outerFilter+`
		  AND sn.snapshot_date = (
		      SELECT MAX(sn2.snapshot_date)
		      FROM snapshots AS sn2
		      JOIN materializations AS m2
		          ON m2.snapshot_id = sn2.snapshot_id
		      WHERE sn2.source_id = s.source_id
		        AND m2.vault_id = v.vault_id`+innerFilter+`)
		ORDER BY s.canonical_path`, args...)
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
