package app

import (
	"context"
	"fmt"

	"github.com/alexzeitgeist/md2obs/internal/database"
	"github.com/alexzeitgeist/md2obs/internal/render"
)

// RunList prints every known source with its most recent snapshot. This is
// a database query only; sources are not stat'ed or hashed.
func RunList(ctx context.Context, d *Deps) error {
	vaultID, err := database.GetVaultIDByKey(ctx, d.DB.Query(), d.Config.VaultAbs)
	if err != nil {
		return err
	}
	entries, err := database.ListSources(ctx, d.DB.Query(), vaultID, render.Profile(d.Config))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(d.Out, "No sources tracked in configured vault")
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(d.Out, "%s\n", e.DisplayPath)
		fmt.Fprintf(d.Out, "  last snapshot:  %s\n", e.SnapshotDate)
		fmt.Fprintf(d.Out, "  vault path:     %s\n", e.RelativePath)
		sourceContent := "current"
		if !e.SourceCurrent {
			sourceContent = "stale"
		}
		rendering := "current"
		if !e.RenderingCurrent {
			rendering = "stale"
		}
		fmt.Fprintf(d.Out, "  source content: %s\n", sourceContent)
		fmt.Fprintf(d.Out, "  rendering:      %s\n", rendering)
	}
	return nil
}
