package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"md2obs/internal/database"
	"md2obs/internal/source"
)

// RunHistory prints the dated snapshots and materializations of one source.
// A source whose file no longer exists is looked up by its canonical or last
// display path so history stays available after deletion.
func RunHistory(ctx context.Context, d *Deps, file string) error {
	lookup, display, err := source.Canonicalize(file)
	var matches []database.Source
	if err == nil {
		src, lookupErr := database.GetSourceByPath(ctx, d.DB.Query(), lookup)
		if lookupErr != nil {
			return lookupErr
		}
		if src != nil {
			matches = append(matches, *src)
		}
	} else {
		// The file may be gone; fall back to the cleaned absolute path.
		abs, absErr := filepath.Abs(file)
		if absErr != nil {
			return fmt.Errorf("resolve %s: %w", file, absErr)
		}
		display = filepath.Clean(abs)
		matches, err = database.FindSourcesByPath(ctx, d.DB.Query(), display)
		if err != nil {
			return err
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("not imported: %s", display)
	}
	if len(matches) > 1 {
		identities := make([]string, len(matches))
		for i, match := range matches {
			identities[i] = match.CanonicalPath
		}
		return fmt.Errorf("history %s: path is ambiguous; matches %s", display, strings.Join(identities, ", "))
	}
	src := matches[0]

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
