package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Materialization records where a snapshot was physically written inside a
// vault, and which revision that write actually contained.
type Materialization struct {
	ID                int64
	SnapshotID        int64
	VaultID           int64
	LayoutID          int64
	RelativePath      string
	WrittenRevisionID int64
}

// GetMaterialization returns nil when the snapshot has not been materialized
// in the vault.
func GetMaterialization(ctx context.Context, q Querier, snapshotID, vaultID int64) (*Materialization, error) {
	m := Materialization{SnapshotID: snapshotID, VaultID: vaultID}
	err := q.QueryRowContext(ctx, `
		SELECT materialization_id, layout_id, relative_path, written_revision_id
		FROM materializations
		WHERE snapshot_id = ? AND vault_id = ?`,
		snapshotID, vaultID).Scan(&m.ID, &m.LayoutID, &m.RelativePath, &m.WrittenRevisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get materialization for snapshot %d: %w", snapshotID, err)
	}
	return &m, nil
}

// IsPathOwned reports whether any materialization already claims the
// vault-relative path.
func IsPathOwned(ctx context.Context, q Querier, vaultID int64, relativePath string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM materializations
		WHERE vault_id = ? AND relative_path = ?`,
		vaultID, relativePath).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check path ownership %s: %w", relativePath, err)
	}
	return true, nil
}

// CreateMaterialization reserves a vault-relative path for a snapshot. The
// UNIQUE constraints on (snapshot_id, vault_id) and (vault_id, relative_path)
// are the final race-condition protection.
func CreateMaterialization(ctx context.Context, q Querier, snapshotID, vaultID, layoutID int64, relativePath string, writtenRevisionID int64, nowUTC string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO materializations
		    (snapshot_id, vault_id, layout_id, relative_path, written_revision_id, written_at_utc)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING materialization_id`,
		snapshotID, vaultID, layoutID, relativePath, writtenRevisionID, nowUTC).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create materialization %s: %w", relativePath, err)
	}
	return id, nil
}

// UpdateMaterializationWritten records which revision a successful physical
// write placed at the materialization's path.
func UpdateMaterializationWritten(ctx context.Context, q Querier, materializationID, writtenRevisionID int64, nowUTC string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE materializations SET written_revision_id = ?, written_at_utc = ?
		WHERE materialization_id = ?`,
		writtenRevisionID, nowUTC, materializationID)
	if err != nil {
		return fmt.Errorf("update materialization %d: %w", materializationID, err)
	}
	return nil
}
