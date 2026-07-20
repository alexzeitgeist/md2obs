package database

import (
	"context"
	"fmt"
)

// FindOrCreateRevision records one observed content state of a source.
// Re-observing identical content refreshes the observation metadata and
// returns the existing revision id.
func FindOrCreateRevision(ctx context.Context, q Querier, sourceID int64, sha256Hex string, byteSize int64, mtimeNS int64, nowUTC string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO revisions (source_id, content_sha256, byte_size, source_mtime_ns, observed_at_utc)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (source_id, content_sha256) DO UPDATE SET
		    byte_size = excluded.byte_size,
		    source_mtime_ns = excluded.source_mtime_ns,
		    observed_at_utc = excluded.observed_at_utc
		RETURNING revision_id`,
		sourceID, sha256Hex, byteSize, mtimeNS, nowUTC).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert revision: %w", err)
	}
	return id, nil
}

// RevisionSHA returns the content hash recorded for a revision.
func RevisionSHA(ctx context.Context, q Querier, revisionID int64) (string, error) {
	var sha string
	err := q.QueryRowContext(ctx,
		`SELECT content_sha256 FROM revisions WHERE revision_id = ?`, revisionID).Scan(&sha)
	if err != nil {
		return "", fmt.Errorf("get revision %d: %w", revisionID, err)
	}
	return sha, nil
}
