package app

import (
	"context"
	"fmt"

	"md2obs/internal/database"
)

// RunList prints every known source with its most recent snapshot. This is
// a database query only; sources are not stat'ed or hashed.
func RunList(ctx context.Context, d *Deps) error {
	vaultID, err := database.GetVaultIDByKey(ctx, d.DB.Query(), d.Config.VaultAbs)
	if err != nil {
		return err
	}
	entries, err := database.ListSources(ctx, d.DB.Query(), vaultID)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(d.Out, "No sources imported yet")
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(d.Out, "%s\n", e.DisplayPath)
		tracking := "not tracked in current vault"
		if e.TrackingActive.Valid {
			tracking = "inactive"
			if e.TrackingActive.Bool {
				tracking = "active"
			}
		}
		fmt.Fprintf(d.Out, "  tracking:      %s\n", tracking)
		fmt.Fprintf(d.Out, "  last snapshot: %s\n", e.SnapshotDate)
		if e.RelativePath.Valid {
			fmt.Fprintf(d.Out, "  vault path:    %s\n", e.RelativePath.String)
			content := "current"
			if !e.Current {
				content = "stale"
			}
			fmt.Fprintf(d.Out, "  content:       %s\n", content)
		} else {
			fmt.Fprintf(d.Out, "  vault path:    (not materialized in current vault)\n")
		}
	}
	return nil
}
