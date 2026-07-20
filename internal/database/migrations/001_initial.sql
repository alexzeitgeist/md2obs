CREATE TABLE vaults (
    vault_id INTEGER PRIMARY KEY,
    vault_key TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    local_root_path TEXT NOT NULL,
    registered_at_utc TEXT NOT NULL
);

CREATE TABLE layouts (
    layout_id INTEGER PRIMARY KEY,
    layout_name TEXT NOT NULL,
    layout_version INTEGER NOT NULL,
    configuration_json TEXT NOT NULL,
    created_at_utc TEXT NOT NULL,
    UNIQUE (layout_name, layout_version)
);

CREATE TABLE sources (
    source_id INTEGER PRIMARY KEY,
    canonical_path TEXT NOT NULL UNIQUE,
    display_path TEXT NOT NULL,
    first_seen_at_utc TEXT NOT NULL,
    last_seen_at_utc TEXT NOT NULL
);

CREATE TABLE revisions (
    revision_id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL
        REFERENCES sources(source_id)
        ON DELETE CASCADE,
    content_sha256 TEXT NOT NULL,
    byte_size INTEGER NOT NULL,
    source_mtime_ns INTEGER,
    observed_at_utc TEXT NOT NULL,
    UNIQUE (source_id, content_sha256)
);

CREATE TABLE snapshots (
    snapshot_id INTEGER PRIMARY KEY,
    source_id INTEGER NOT NULL
        REFERENCES sources(source_id)
        ON DELETE CASCADE,
    revision_id INTEGER NOT NULL
        REFERENCES revisions(revision_id),
    snapshot_date TEXT NOT NULL,
    created_at_utc TEXT NOT NULL,
    updated_at_utc TEXT NOT NULL,
    UNIQUE (source_id, snapshot_date)
);

CREATE TABLE materializations (
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
    written_at_utc TEXT NOT NULL,
    UNIQUE (snapshot_id, vault_id),
    UNIQUE (vault_id, relative_path)
);

CREATE INDEX idx_snapshots_date_source
    ON snapshots(snapshot_date, source_id);

CREATE INDEX idx_snapshots_source_date
    ON snapshots(source_id, snapshot_date DESC);

CREATE INDEX idx_revisions_source_observed
    ON revisions(source_id, observed_at_utc DESC);

CREATE INDEX idx_materializations_snapshot
    ON materializations(snapshot_id);
