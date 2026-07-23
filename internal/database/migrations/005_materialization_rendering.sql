CREATE TABLE materializations_v5 (
    materialization_id INTEGER PRIMARY KEY,
    snapshot_id INTEGER NOT NULL
        REFERENCES snapshots(snapshot_id)
        ON DELETE CASCADE,
    vault_id INTEGER NOT NULL
        REFERENCES vaults(vault_id)
        ON DELETE CASCADE,
    layout_id INTEGER NOT NULL
        REFERENCES layouts(layout_id),
    relative_path TEXT NOT NULL,
    written_revision_id INTEGER NOT NULL
        REFERENCES revisions(revision_id),
    written_content_sha256 TEXT NOT NULL,
    written_render_profile TEXT NOT NULL,
    written_at_utc TEXT NOT NULL,
    UNIQUE (snapshot_id, vault_id),
    UNIQUE (vault_id, relative_path)
);

INSERT INTO materializations_v5 (
    materialization_id,
    snapshot_id,
    vault_id,
    layout_id,
    relative_path,
    written_revision_id,
    written_content_sha256,
    written_render_profile,
    written_at_utc
)
SELECT
    m.materialization_id,
    m.snapshot_id,
    m.vault_id,
    m.layout_id,
    m.relative_path,
    m.written_revision_id,
    r.content_sha256,
    'source-v1',
    m.written_at_utc
FROM materializations AS m
LEFT JOIN revisions AS r
  ON r.revision_id = m.written_revision_id;

DROP TABLE materializations;
ALTER TABLE materializations_v5 RENAME TO materializations;

CREATE INDEX idx_materializations_snapshot
    ON materializations(snapshot_id);
