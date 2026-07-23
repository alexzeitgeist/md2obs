package app

import (
	"context"
	"fmt"

	"github.com/alexzeitgeist/md2obs/internal/database"
)

// RefreshOptions are the validated `md2obs refresh` flags.
type RefreshOptions struct {
	// Days is the inclusive local calendar-day window. It is ignored when All
	// is selected.
	Days int
	// DaysSet records that --days was given explicitly, so combining it with
	// --all can be rejected while the flag default is tolerated.
	DaysSet bool
	// All selects every source currently tracked in the configured vault.
	All bool
	// OnVaultChange decides what happens when a changed source would overwrite
	// a vault copy edited since the last import.
	OnVaultChange Policy
	// Rerender bypasses the passive source-hash gate for selected candidates so
	// the central import path can apply the current renderer profile.
	Rerender bool
}

// Validate checks refresh settings without touching config, SQLite, or the
// filesystem.
func (o RefreshOptions) Validate() error {
	if o.All {
		if o.DaysSet {
			return fmt.Errorf("--all cannot be combined with --days")
		}
	} else if o.Days < 1 {
		return fmt.Errorf("--days must be at least 1, got %d", o.Days)
	}
	if _, err := ParsePolicy(string(o.OnVaultChange)); err != nil {
		return err
	}
	return nil
}

// RunRefresh performs a one-shot source catch-up for candidates materialized
// in the configured vault. Missing registered sources are reported in the
// summary but are not failures, allowing a later source recreation to be
// picked up by the watcher or another refresh.
func RunRefresh(ctx context.Context, d *Deps, opts RefreshOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	var (
		candidates []database.WatchCandidate
		err        error
	)
	if opts.All {
		candidates, err = database.SelectAllWatchCandidates(ctx, d.DB.Query(), d.Config.VaultAbs)
	} else {
		fromDate, toDate := dateRange(d.Now(), opts.Days)
		candidates, err = database.SelectWatchCandidates(ctx, d.DB.Query(), d.Config.VaultAbs, fromDate, toDate)
	}
	if err != nil {
		return err
	}

	var refreshed, conflicts, unchanged, missing, untracked, failed int
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		outcome, err := reconcileWatchCandidate(ctx, d, candidate, opts.OnVaultChange, opts.Rerender)
		if err != nil {
			fmt.Fprintf(d.Err, "error: refresh: %v\n", err)
			failed++
			continue
		}
		if outcome.Missing {
			missing++
			continue
		}
		if outcome.Untracked {
			untracked++
			continue
		}
		if outcome.Import == nil {
			unchanged++
			continue
		}

		printResult(d.Out, *outcome.Import)
		switch outcome.Import.Status {
		case StatusSkipped:
			conflicts++
		case StatusUnchanged:
			// Another importer may have won the race after the hash gate.
			unchanged++
		default:
			refreshed++
		}
	}

	// One notification is sufficient to enroll sources that became current and
	// to let a running watcher retry directory watches. A notification failure
	// does not undo successful imports.
	if len(candidates) > 0 {
		notifyWatchers(d, "refresh completed")
	}
	fmt.Fprintf(
		d.Out,
		"Checked %d %s: %d refreshed, %d conflicts skipped, %d unchanged, %d missing, %d untracked during refresh, %d failed\n",
		len(candidates), plural(len(candidates), "source", "sources"), refreshed, conflicts, unchanged, missing, untracked, failed,
	)

	if failed > 0 {
		return fmt.Errorf("%d of %d refresh candidates failed", failed, len(candidates))
	}
	return nil
}
