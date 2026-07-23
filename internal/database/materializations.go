package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Materialization records where a snapshot was written, the raw revision
// represented by the write, the exact vault-byte hash, and the renderer
// profile those bytes are confirmed to match. The profile can advance without
// a physical write after an exact live-byte confirmation.
type Materialization struct {
	ID                   int64
	SnapshotID           int64
	VaultID              int64
	LayoutID             int64
	RelativePath         string
	WrittenRevisionID    int64
	WrittenContentSHA256 string
	WrittenRenderProfile string
	WrittenAtUTC         string
}

// GetMaterialization returns nil when the snapshot has not been materialized
// in the vault.
func GetMaterialization(ctx context.Context, q Querier, snapshotID, vaultID int64) (*Materialization, error) {
	m := Materialization{SnapshotID: snapshotID, VaultID: vaultID}
	err := q.QueryRowContext(ctx, `
		SELECT
		    materialization_id,
		    layout_id,
		    relative_path,
		    written_revision_id,
		    written_content_sha256,
		    written_render_profile,
		    written_at_utc
		FROM materializations
		WHERE snapshot_id = ? AND vault_id = ?`,
		snapshotID, vaultID).Scan(
		&m.ID,
		&m.LayoutID,
		&m.RelativePath,
		&m.WrittenRevisionID,
		&m.WrittenContentSHA256,
		&m.WrittenRenderProfile,
		&m.WrittenAtUTC,
	)
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
func CreateMaterialization(
	ctx context.Context,
	q Querier,
	snapshotID, vaultID, layoutID int64,
	relativePath string,
	writtenRevisionID int64,
	writtenContentSHA256, writtenRenderProfile, nowUTC string,
) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO materializations
		    (
		        snapshot_id,
		        vault_id,
		        layout_id,
		        relative_path,
		        written_revision_id,
		        written_content_sha256,
		        written_render_profile,
		        written_at_utc
		    )
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING materialization_id`,
		snapshotID,
		vaultID,
		layoutID,
		relativePath,
		writtenRevisionID,
		writtenContentSHA256,
		writtenRenderProfile,
		nowUTC,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create materialization %s: %w", relativePath, err)
	}
	return id, nil
}

// UpdateMaterializationWritten atomically records every fact established by a
// successful physical write.
func UpdateMaterializationWritten(
	ctx context.Context,
	q Querier,
	materializationID, writtenRevisionID int64,
	writtenContentSHA256, writtenRenderProfile, nowUTC string,
) error {
	_, err := q.ExecContext(ctx, `
		UPDATE materializations SET
		    written_revision_id = ?,
		    written_content_sha256 = ?,
		    written_render_profile = ?,
		    written_at_utc = ?
		WHERE materialization_id = ?`,
		writtenRevisionID,
		writtenContentSHA256,
		writtenRenderProfile,
		nowUTC,
		materializationID,
	)
	if err != nil {
		return fmt.Errorf("update materialization %d: %w", materializationID, err)
	}
	return nil
}

// UpdateMaterializationRenderProfile records that the existing vault bytes
// were confirmed byte-for-byte under profile. It deliberately leaves the
// physical-write timestamp and all other write facts unchanged.
func UpdateMaterializationRenderProfile(ctx context.Context, q Querier, materializationID int64, profile string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE materializations SET written_render_profile = ?
		WHERE materialization_id = ?`,
		profile, materializationID)
	if err != nil {
		return fmt.Errorf("update materialization %d render profile: %w", materializationID, err)
	}
	return nil
}
