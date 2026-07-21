package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"md2obs/internal/database"
	"md2obs/internal/source"
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

// RunWatch watches the immediate parent directories of sources materialized
// in the configured vault inside the date window. Successful explicit imports
// dynamically add sources, while startup remains passive.
func RunWatch(ctx context.Context, d *Deps, opts WatchOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	discoveryFrom, discoveryTo := dateRange(d.Now(), opts.Days)
	selected := make(map[string]database.WatchCandidate)
	load := func() ([]string, error) {
		currentFrom, currentTo := dateRange(d.Now(), opts.Days)
		if currentFrom < discoveryFrom {
			discoveryFrom = currentFrom
		}
		if currentTo > discoveryTo {
			discoveryTo = currentTo
		}
		candidates, err := database.SelectWatchCandidates(
			ctx,
			d.DB.Query(),
			d.Config.VaultAbs,
			discoveryFrom,
			discoveryTo,
		)
		if err != nil {
			return nil, err
		}
		paths := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			path := candidate.CanonicalPath
			if pinned, ok := selected[path]; ok {
				// Refresh activation facts without changing the source identity
				// pinned when this path was first discovered.
				candidate.Source = pinned.Source
			}
			selected[path] = candidate
			paths = append(paths, path)
		}
		return paths, nil
	}

	handle := func(p string) {
		// A fired debounce for a missing file means the source was removed;
		// the directory watch stays, and recreation triggers a new event.
		if _, err := os.Stat(p); err != nil {
			if !os.IsNotExist(err) {
				d.logger().Error("cannot inspect watched source", "source", p, "err", err)
			}
			return
		}
		candidate, ok := selected[p]
		if !ok {
			d.logger().Error("watch event has no registered source", "source", p)
			return
		}
		res, err := ImportWatchedSource(ctx, d, candidate.Source, opts.OnVaultChange)
		if err != nil {
			d.logger().Error("watch import failed", "source", p, "err", err)
			return
		}
		printResult(d.Out, res)
	}

	activate := func(p string) {
		candidate, ok := selected[p]
		if !ok {
			d.logger().Error("watch activation has no registered source", "source", p)
			return
		}
		canonical, _, err := source.Canonicalize(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return
			}
			d.logger().Error("cannot inspect newly watched source", "source", p, "err", err)
			return
		}
		if canonical != candidate.CanonicalPath {
			err := fmt.Errorf("watch source identity changed: registered %s now resolves to %s", candidate.CanonicalPath, canonical)
			d.logger().Error("watch activation import failed", "source", p, "err", err)
			return
		}
		_, sha, err := source.ReadAndHash(canonical)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return
			}
			d.logger().Error("cannot inspect newly watched source", "source", p, "err", err)
			return
		}
		if sha == candidate.ContentSHA {
			return
		}
		res, err := ImportWatchedSource(ctx, d, candidate.Source, opts.OnVaultChange)
		if err != nil {
			d.logger().Error("watch activation import failed", "source", p, "err", err)
			return
		}
		printResult(d.Out, res)
	}

	return watcher.Run(ctx, watcher.Options{
		NotificationPath: watcher.NotificationPath(d.DB.Path),
		SourceDebounce:   opts.Debounce,
		RefreshDebounce:  watcher.DefaultRefreshDebounce,
		Load:             load,
		Activate:         activate,
		Handle:           handle,
		Ready: func(stats watcher.Stats) {
			fmt.Fprintf(d.Out, "Watching %d imported sources from %d directories\n", stats.Sources, stats.Directories)
			fmt.Fprintf(d.Out, "Date range: %s through %s\n", discoveryFrom, discoveryTo)
		},
	}, d.logger())
}

// dateRange returns the inclusive local calendar-day window ending today:
// days=1 -> today only, days=3 -> today and the previous two days.
func dateRange(now time.Time, days int) (fromDate, toDate string) {
	toDate = now.Format(dateFormat)
	fromDate = now.AddDate(0, 0, -(days - 1)).Format(dateFormat)
	return fromDate, toDate
}
