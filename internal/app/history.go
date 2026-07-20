package app

import (
	"context"
	"fmt"
	"path/filepath"

	"md2obs/internal/database"
	"md2obs/internal/source"
)

// RunHistory prints the dated snapshots and materializations of one source.
// A source whose file no longer exists is looked up by its absolute path so
// history stays available after deletion.
func RunHistory(ctx context.Context, d *Deps, file string) error {
	lookup, display, err := source.Canonicalize(file)
	if err != nil {
		// The file may be gone; fall back to the cleaned absolute path.
		abs, absErr := filepath.Abs(file)
		if absErr != nil {
			return fmt.Errorf("resolve %s: %w", file, absErr)
		}
		lookup, display = filepath.Clean(abs), abs
	}

	src, err := database.GetSourceByPath(ctx, d.DB.Query(), lookup)
	if err != nil {
		return err
	}
	if src == nil {
		return fmt.Errorf("not imported: %s", display)
	}

	vaultID, err := database.GetVaultIDByKey(ctx, d.DB.Query(), d.Config.VaultAbs)
	if err != nil {
		return err
	}
	entries, err := database.History(ctx, d.DB.Query(), src.ID, vaultID)
	if err != nil {
		return err
	}

	fmt.Fprintf(d.Out, "Source: %s\n\n", src.DisplayPath)
	for _, e := range entries {
		rel := "(not materialized in current vault)"
		if e.RelativePath.Valid {
			rel = e.RelativePath.String
		}
		fmt.Fprintf(d.Out, "%s  %s\n", e.Date, rel)
	}
	return nil
}
