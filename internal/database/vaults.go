package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EnsureVault registers a vault by its stable key (the canonical vault root
// path) or refreshes its location, returning the vault id.
func EnsureVault(ctx context.Context, q Querier, vaultKey, displayName, localRootPath, nowUTC string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO vaults (vault_key, display_name, local_root_path, registered_at_utc)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (vault_key) DO UPDATE SET
		    display_name = excluded.display_name,
		    local_root_path = excluded.local_root_path
		RETURNING vault_id`,
		vaultKey, displayName, localRootPath, nowUTC).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure vault %s: %w", vaultKey, err)
	}
	return id, nil
}

// GetVaultIDByKey returns (0, nil) when the vault has never been registered.
func GetVaultIDByKey(ctx context.Context, q Querier, vaultKey string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT vault_id FROM vaults WHERE vault_key = ?`, vaultKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get vault %s: %w", vaultKey, err)
	}
	return id, nil
}

// EnsureLayout registers a layout name/version pair once. The configuration
// JSON recorded at first registration is kept as-is afterwards.
func EnsureLayout(ctx context.Context, q Querier, name string, version int, configurationJSON, nowUTC string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO layouts (layout_name, layout_version, configuration_json, created_at_utc)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (layout_name, layout_version) DO UPDATE SET
		    layout_name = excluded.layout_name
		RETURNING layout_id`,
		name, version, configurationJSON, nowUTC).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure layout %s v%d: %w", name, version, err)
	}
	return id, nil
}
