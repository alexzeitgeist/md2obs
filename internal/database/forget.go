package database

import (
	"context"
	"fmt"
)

// ForgetResult reports the bookkeeping removed when a source is untracked
// from one vault. It never represents physical vault-file deletion.
type ForgetResult struct {
	MaterializationsDeleted int64
	SnapshotsDeleted        int64
	RevisionsDeleted        int64
	SourceDeleted           bool
}

// Changed reports whether the source was still tracked in the selected vault.
func (r ForgetResult) Changed() bool { return r.MaterializationsDeleted > 0 }

// PreviewForgetSourceInVault reports whether ForgetSourceInVault would change
// anything: whether any materialization still associates the source with the
// vault. It is read-only so dry runs do not acquire an immediate write lock.
func PreviewForgetSourceInVault(ctx context.Context, q Querier, sourceID int64, canonicalPath string, vaultID int64) (bool, error) {
	var tracked bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM materializations AS m
		JOIN snapshots AS sn ON sn.snapshot_id = m.snapshot_id
		JOIN sources AS s ON s.source_id = sn.source_id
		WHERE s.source_id = ? AND s.canonical_path = ? AND m.vault_id = ?
	)`, sourceID, canonicalPath, vaultID,
	).Scan(&tracked); err != nil {
		return false, fmt.Errorf("preview untrack for source %d: %w", sourceID, err)
	}
	return tracked, nil
}

// ForgetSourceInVault deletes every materialization matching a source and
// vault, then garbage-collects source bookkeeping that no remaining vault
// materialization references. All deletes are predicate-based so an import
// committed after selection but before this transaction is included. The
// canonical path prevents a reused row ID for a different source from matching.
// The caller must run this inside a write transaction.
func ForgetSourceInVault(ctx context.Context, q Querier, sourceID int64, canonicalPath string, vaultID int64) (ForgetResult, error) {
	var result ForgetResult

	deleted, err := q.ExecContext(ctx, `
		DELETE FROM materializations
		WHERE vault_id = ?
		  AND snapshot_id IN (
		      SELECT sn.snapshot_id
		      FROM snapshots AS sn
		      JOIN sources AS s ON s.source_id = sn.source_id
		      WHERE s.source_id = ? AND s.canonical_path = ?
		  )`, vaultID, sourceID, canonicalPath)
	if err != nil {
		return ForgetResult{}, fmt.Errorf("forget materializations for source %d in vault %d: %w", sourceID, vaultID, err)
	}
	if result.MaterializationsDeleted, err = deleted.RowsAffected(); err != nil {
		return ForgetResult{}, fmt.Errorf("count forgotten materializations for source %d: %w", sourceID, err)
	}

	deleted, err = q.ExecContext(ctx, `
		DELETE FROM snapshots
		WHERE source_id = ?
		  AND EXISTS (
		      SELECT 1 FROM sources AS s
		      WHERE s.source_id = snapshots.source_id AND s.canonical_path = ?
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM materializations AS m
		      WHERE m.snapshot_id = snapshots.snapshot_id
		  )`, sourceID, canonicalPath)
	if err != nil {
		return ForgetResult{}, fmt.Errorf("collect snapshots for source %d: %w", sourceID, err)
	}
	if result.SnapshotsDeleted, err = deleted.RowsAffected(); err != nil {
		return ForgetResult{}, fmt.Errorf("count collected snapshots for source %d: %w", sourceID, err)
	}

	deleted, err = q.ExecContext(ctx, `
		DELETE FROM revisions
		WHERE source_id = ?
		  AND EXISTS (
		      SELECT 1 FROM sources AS s
		      WHERE s.source_id = revisions.source_id AND s.canonical_path = ?
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM snapshots AS sn
		      WHERE sn.revision_id = revisions.revision_id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM materializations AS m
		      WHERE m.written_revision_id = revisions.revision_id
		  )`, sourceID, canonicalPath)
	if err != nil {
		return ForgetResult{}, fmt.Errorf("collect revisions for source %d: %w", sourceID, err)
	}
	if result.RevisionsDeleted, err = deleted.RowsAffected(); err != nil {
		return ForgetResult{}, fmt.Errorf("count collected revisions for source %d: %w", sourceID, err)
	}

	deleted, err = q.ExecContext(ctx, `
		DELETE FROM sources
		WHERE source_id = ? AND canonical_path = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM snapshots AS sn
		      WHERE sn.source_id = sources.source_id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM revisions AS r
		      WHERE r.source_id = sources.source_id
		  )`, sourceID, canonicalPath)
	if err != nil {
		return ForgetResult{}, fmt.Errorf("collect source %d: %w", sourceID, err)
	}
	sourceRows, err := deleted.RowsAffected()
	if err != nil {
		return ForgetResult{}, fmt.Errorf("count collected source %d: %w", sourceID, err)
	}
	result.SourceDeleted = sourceRows > 0
	return result, nil
}
