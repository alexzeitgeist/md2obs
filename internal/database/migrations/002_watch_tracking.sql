CREATE TABLE watch_tracking (
    source_id INTEGER NOT NULL REFERENCES sources(source_id) ON DELETE CASCADE,
    vault_id INTEGER NOT NULL REFERENCES vaults(vault_id) ON DELETE CASCADE,
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    updated_at_utc TEXT NOT NULL,
    PRIMARY KEY (source_id, vault_id)
);
