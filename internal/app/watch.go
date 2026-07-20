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

// Validate checks the command-line watch settings without touching config,
// SQLite, or the filesystem.
func (o WatchOptions) Validate() error {
	if o.Days < 1 {
		return fmt.Errorf("--days must be at least 1, got %d", o.Days)
	}
	if o.Debounce <= 0 {
		return fmt.Errorf("--debounce must be positive, got %s", o.Debounce)
	}
	if _, err := ParsePolicy(string(o.OnVaultChange)); err != nil {
		return err
	}
	return nil
}

// RunWatch watches the immediate parent directories of sources that have
// snapshots inside the date window, re-importing a source after its events
// settle. It never scans for files, registers new sources, or rewrites
// anything at startup.
func RunWatch(ctx context.Context, d *Deps, opts WatchOptions) error {
	if err := opts.Validate(); err != nil {
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

	selected := make(map[string]database.Source, len(sources))
	var paths []string
	for _, s := range sources {
		parent := filepath.Dir(s.CanonicalPath)
		if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
			d.logger().Warn("parent directory unavailable; source skipped", "source", s.CanonicalPath, "parent", parent, "err", err)
			continue
		}
		paths = append(paths, s.CanonicalPath)
		selected[s.CanonicalPath] = s
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
			if !os.IsNotExist(err) {
				d.logger().Error("cannot inspect watched source", "source", p, "err", err)
			}
			return
		}
		registered, ok := selected[p]
		if !ok {
			d.logger().Error("watch event has no registered source", "source", p)
			return
		}
		res, err := ImportWatchedSource(ctx, d, registered, opts.OnVaultChange)
		if err != nil {
			d.logger().Error("watch import failed", "source", p, "err", err)
			return
		}
		printResult(d.Out, res)
	}
	return watcher.Run(ctx, ix, opts.Debounce, handle, d.logger())
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
