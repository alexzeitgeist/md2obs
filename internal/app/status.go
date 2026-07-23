package app

import (
	"context"
	"fmt"
)

// RunStatus reports configuration and database facts without touching the
// filesystem beyond the database itself.
func RunStatus(ctx context.Context, d *Deps) error {
	version, err := d.DB.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	sources, snapshots, materializations, err := d.DB.Counts(ctx)
	if err != nil {
		return err
	}

	configFile := d.Config.ConfigPath
	if configFile == "" {
		configFile = "(none; environment only)"
	}
	fmt.Fprintf(d.Out, "Config file:       %s\n", configFile)
	fmt.Fprintf(d.Out, "Database:          %s\n", d.DB.Path)
	fmt.Fprintf(d.Out, "Vault:             %s\n", d.Config.VaultAbs)
	fmt.Fprintf(d.Out, "Layout:            %s (version %d)\n", d.Layout.Name(), d.Layout.Version())
	fmt.Fprintf(d.Out, "Destination root:  %s\n", d.Config.RootDirectory)
	provenance := "disabled"
	if d.Config.ProvenanceFrontmatter {
		provenance = "enabled"
	}
	fmt.Fprintf(d.Out, "Provenance:        %s\n", provenance)
	fmt.Fprintf(d.Out, "Schema version:    %d\n", version)
	fmt.Fprintf(d.Out, "Sources:           %d\n", sources)
	fmt.Fprintf(d.Out, "Snapshots:         %d\n", snapshots)
	fmt.Fprintf(d.Out, "Materializations:  %d\n", materializations)
	return nil
}
