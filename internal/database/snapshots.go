package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Snapshot selects one revision of one source for one calendar date.
// CreatedAtUTC is stable for the lifetime of the dated snapshot.
type Snapshot struct {
	ID           int64
	SourceID     int64
	RevisionID   int64
	Date         string
	CreatedAtUTC string
}

// GetSnapshot returns nil when the source has no snapshot for the date.
func GetSnapshot(ctx context.Context, q Querier, sourceID int64, date string) (*Snapshot, error) {
	s := Snapshot{SourceID: sourceID, Date: date}
	err := q.QueryRowContext(ctx, `
		SELECT snapshot_id, revision_id, created_at_utc FROM snapshots
		WHERE source_id = ? AND snapshot_date = ?`,
		sourceID, date).Scan(&s.ID, &s.RevisionID, &s.CreatedAtUTC)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get snapshot %s: %w", date, err)
	}
	return &s, nil
}

func CreateSnapshot(ctx context.Context, q Querier, sourceID, revisionID int64, date, nowUTC string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO snapshots (source_id, revision_id, snapshot_date, created_at_utc, updated_at_utc)
		VALUES (?, ?, ?, ?, ?)
		RETURNING snapshot_id`,
		sourceID, revisionID, date, nowUTC, nowUTC).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create snapshot %s: %w", date, err)
	}
	return id, nil
}

// UpdateSnapshotRevision re-points an existing snapshot at a newer revision.
func UpdateSnapshotRevision(ctx context.Context, q Querier, snapshotID, revisionID int64, nowUTC string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE snapshots SET revision_id = ?, updated_at_utc = ?
		WHERE snapshot_id = ?`,
		revisionID, nowUTC, snapshotID)
	if err != nil {
		return fmt.Errorf("update snapshot %d: %w", snapshotID, err)
	}
	return nil
}

// HistoryEntry is one dated snapshot of a source with its materialization
// in the current vault, when one exists.
type HistoryEntry struct {
	Date         string
	RelativePath sql.NullString
}

// History lists a source's snapshots, newest first.
func History(ctx context.Context, q Querier, sourceID, vaultID int64) ([]HistoryEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT sn.snapshot_date, m.relative_path
		FROM snapshots AS sn
		LEFT JOIN materializations AS m
		    ON m.snapshot_id = sn.snapshot_id AND m.vault_id = ?
		WHERE sn.source_id = ?
		ORDER BY sn.snapshot_date DESC`, vaultID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("history for source %d: %w", sourceID, err)
	}
	defer rows.Close()

	var entries []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(&e.Date, &e.RelativePath); err != nil {
			return nil, fmt.Errorf("scan history row: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
