package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"md2obs/internal/database"
	"md2obs/internal/watcher"
)

// WatchOptions are the validated `md2obs watch` flags.
type WatchOptions struct {
	// Days is the inclusive local calendar-day window: 1 = today,
	// 2 = today and yesterday.
	Days int
	// Debounce is the per-source quiet period before a changed file is
	// re-imported.
	Debounce time.Duration
	// OnVaultChange decides what happens when the vault copy was edited
	// since the last import.
	OnVaultChange Policy
}

// DefaultDebounce is the per-source debounce interval when --debounce is
// not given.
const DefaultDebounce = 500 * time.Millisecond

// RunWatch watches the immediate parent directories of sources that have
// snapshots inside the date window, re-importing a source after its events
// settle. It never scans for files, registers new sources, or rewrites
// anything at startup.
func RunWatch(ctx context.Context, d *Deps, opts WatchOptions) error {
	if opts.Days < 1 {
		return fmt.Errorf("--days must be at least 1, got %d", opts.Days)
	}
	if opts.Debounce <= 0 {
		return fmt.Errorf("--debounce must be positive, got %s", opts.Debounce)
	}
	if _, err := ParsePolicy(string(opts.OnVaultChange)); err != nil {
		return err
	}

	fromDate, toDate := dateRange(d.Now(), opts.Days)
	sources, err := database.SelectSourcesWithSnapshotsBetween(ctx, d.DB.Query(), fromDate, toDate)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		fmt.Fprintf(d.Out, "No imported sources found for %s\n", describeRange(fromDate, toDate))
		return nil
	}

	var paths []string
	for _, s := range sources {
		parent := filepath.Dir(s.CanonicalPath)
		if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
			fmt.Fprintf(d.Err, "warning: parent directory missing, skipping %s\n", s.CanonicalPath)
			continue
		}
		paths = append(paths, s.CanonicalPath)
	}
	if len(paths) == 0 {
		fmt.Fprintf(d.Out, "No watchable sources for %s (all parent directories missing)\n", describeRange(fromDate, toDate))
		return nil
	}

	ix := watcher.NewIndex(paths)
	fmt.Fprintf(d.Out, "Watching %d imported sources from %d directories\n", ix.Len(), len(ix.Parents()))
	fmt.Fprintf(d.Out, "Date range: %s through %s\n", fromDate, toDate)

	handle := func(p string) {
		// A fired debounce for a missing file means the source was removed;
		// the directory watch stays, and recreation triggers a new event.
		if _, err := os.Stat(p); err != nil {
			return
		}
		res, err := ImportFile(ctx, d, p, opts.OnVaultChange)
		if err != nil {
			fmt.Fprintf(d.Err, "error: %v\n", err)
			return
		}
		printResult(d.Out, res)
	}
	return watcher.Run(ctx, ix, opts.Debounce, handle, d.Err)
}

// dateRange returns the inclusive local calendar-day window ending today:
// days=1 -> today only, days=3 -> today and the previous two days.
func dateRange(now time.Time, days int) (fromDate, toDate string) {
	toDate = now.Format(dateFormat)
	fromDate = now.AddDate(0, 0, -(days - 1)).Format(dateFormat)
	return fromDate, toDate
}

func describeRange(fromDate, toDate string) string {
	if fromDate == toDate {
		return fromDate
	}
	return fromDate + " through " + toDate
}
