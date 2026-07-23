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
