-- Materializations now define vault-scoped tracking membership. Convert each
-- explicitly inactive schema-v3 (source, vault) pair into the new forget
-- semantics, garbage-collect only bookkeeping made unreachable by those
-- pairs, then remove the redundant tracking table.
CREATE TEMP TABLE md2obs_m004_inactive_tracking AS
SELECT source_id, vault_id
FROM watch_tracking
WHERE active = 0;

DELETE FROM materializations
WHERE EXISTS (
    SELECT 1
    FROM snapshots AS sn
    JOIN md2obs_m004_inactive_tracking AS inactive
      ON inactive.source_id = sn.source_id
     AND inactive.vault_id = materializations.vault_id
    WHERE sn.snapshot_id = materializations.snapshot_id
);

DELETE FROM snapshots
WHERE source_id IN (
    SELECT source_id FROM md2obs_m004_inactive_tracking
)
AND NOT EXISTS (
    SELECT 1
    FROM materializations AS m
    WHERE m.snapshot_id = snapshots.snapshot_id
);

DELETE FROM revisions
WHERE source_id IN (
    SELECT source_id FROM md2obs_m004_inactive_tracking
)
AND NOT EXISTS (
    SELECT 1
    FROM snapshots AS sn
    WHERE sn.revision_id = revisions.revision_id
)
AND NOT EXISTS (
    SELECT 1
    FROM materializations AS m
    WHERE m.written_revision_id = revisions.revision_id
);

DROP TABLE watch_tracking;

DELETE FROM sources
WHERE source_id IN (
    SELECT source_id FROM md2obs_m004_inactive_tracking
)
AND NOT EXISTS (
    SELECT 1
    FROM snapshots AS sn
    WHERE sn.source_id = sources.source_id
)
AND NOT EXISTS (
    SELECT 1
    FROM revisions AS r
    WHERE r.source_id = sources.source_id
);

DROP TABLE md2obs_m004_inactive_tracking;
