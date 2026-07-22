package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultRefreshDebounce coalesces bursts of import and untrack membership
// notifications. It is independent of the source quiet period exposed by
// --debounce.
const DefaultRefreshDebounce = 50 * time.Millisecond

// Stats describes the exact sources and source directories armed at startup.
type Stats struct {
	Sources     int
	Directories int
}

// Options configures one dynamically reconciled watch session. All callbacks,
// index mutations, and watched imports run serially on the event-loop
// goroutine. Timer goroutines only deliver paths on channels.
type Options struct {
	NotificationPath string
	SourceDebounce   time.Duration
	RefreshDebounce  time.Duration
	Load             func() ([]string, error)
	Activate         func(path string)
	Handle           func(path string)
	Remove           func(path string)
	Ready            func(Stats)
}

// Run watches exact source paths loaded from SQLite and reconciles membership
// when an explicit import or untrack updates NotificationPath. Filesystem
// absence does not change durable membership; unrelated paths in armed
// directories never reach callbacks.
func Run(ctx context.Context, opts Options, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.NotificationPath == "" {
		return errors.New("watch notification path is empty")
	}
	if opts.SourceDebounce <= 0 {
		return fmt.Errorf("source debounce must be positive, got %s", opts.SourceDebounce)
	}
	if opts.RefreshDebounce <= 0 {
		opts.RefreshDebounce = DefaultRefreshDebounce
	}
	if opts.Load == nil || opts.Handle == nil {
		return errors.New("watch load and handle callbacks are required")
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer w.Close()

	notificationPath := filepath.Clean(opts.NotificationPath)
	notificationParent := filepath.Dir(notificationPath)
	if err := w.Add(notificationParent); err != nil {
		return fmt.Errorf("watch notification directory %s: %w", notificationParent, err)
	}

	ix := NewIndex(nil)

	reconcile := func(paths []string, activate bool, remove func(string)) {
		// Re-add every candidate parent once per refresh. Native watches are
		// discarded when a directory is deleted; refreshing the watch before the
		// membership check heals a directory that was later recreated.
		desired := make(map[string]struct{}, len(paths))
		parentResults := make(map[string]error)
		for _, path := range paths {
			clean := filepath.Clean(path)
			desired[clean] = struct{}{}
			parent := filepath.Dir(clean)
			armErr, attempted := parentResults[parent]
			if !attempted {
				armErr = w.Add(parent)
				parentResults[parent] = armErr
				if armErr != nil {
					logger.Warn("cannot watch source directory", "source", clean, "parent", parent, "err", armErr)
				}
			}
			if armErr != nil {
				continue
			}
			if _, exists := ix.Match(clean); exists {
				continue
			}
			if !ix.Add(clean) {
				continue
			}
			if activate && opts.Activate != nil {
				opts.Activate(clean)
			}
		}
		if remove != nil {
			for _, path := range ix.Paths() {
				if _, keep := desired[path]; keep {
					continue
				}
				removeWatchedSource(w, ix, notificationParent, path, remove, logger)
			}
		}
	}

	initial, err := opts.Load()
	if err != nil {
		return err
	}
	reconcile(initial, false, nil)
	if opts.Ready != nil {
		opts.Ready(Stats{Sources: ix.Len(), Directories: len(ix.Parents())})
	}

	sourceDebouncer := NewDebouncer(opts.SourceDebounce)
	defer sourceDebouncer.Stop()
	refreshDebouncer := NewDebouncer(opts.RefreshDebounce)
	defer refreshDebouncer.Stop()

	var retryTimer *time.Timer
	var retryC <-chan time.Time
	refreshAttempt := 0
	stopRetry := func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
		retryTimer = nil
		retryC = nil
		refreshAttempt = 0
	}
	defer stopRetry()

	refresh := func() error {
		paths, loadErr := opts.Load()
		if loadErr == nil {
			reconcile(paths, true, func(path string) {
				sourceDebouncer.Cancel(path)
				if opts.Remove != nil {
					opts.Remove(path)
				}
			})
			stopRetry()
			return nil
		}

		refreshAttempt++
		var delay time.Duration
		switch refreshAttempt {
		case 1:
			delay = 100 * time.Millisecond
		case 2:
			delay = 500 * time.Millisecond
		default:
			stopRetry()
			return fmt.Errorf("refresh watch membership after 3 attempts: %w", loadErr)
		}
		logger.Warn("cannot refresh watch membership; retrying", "attempt", refreshAttempt, "retry_in", delay, "err", loadErr)
		if retryTimer == nil {
			retryTimer = time.NewTimer(delay)
		} else {
			retryTimer.Reset(delay)
		}
		retryC = retryTimer.C
		return nil
	}

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			clean := filepath.Clean(ev.Name)
			if clean == notificationPath && ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
				refreshDebouncer.Trigger(notificationPath)
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				if path, matched := ix.Match(clean); matched {
					sourceDebouncer.Trigger(path)
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				logger.Warn("notification queue overflowed; source changes or membership updates may have been missed", "action", "restart the watcher or re-run the affected import/untrack command")
			} else {
				logger.Error("filesystem watcher error", "err", err)
			}
		case path := <-sourceDebouncer.C:
			if _, matched := ix.Match(path); matched {
				opts.Handle(path)
			}
		case <-refreshDebouncer.C:
			if retryC == nil {
				if err := refresh(); err != nil {
					return err
				}
			}
		case <-retryC:
			retryC = nil
			if err := refresh(); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

type watchRemover interface {
	Remove(string) error
}

func removeWatchedSource(
	w watchRemover,
	ix *Index,
	notificationParent string,
	path string,
	remove func(string),
	logger *slog.Logger,
) {
	parent := filepath.Dir(filepath.Clean(path))
	if !ix.Remove(path) {
		return
	}
	remove(path)
	if ix.HasParent(parent) || parent == notificationParent {
		return
	}
	if err := w.Remove(parent); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		logger.Warn("cannot release source directory watch", "parent", parent, "err", err)
	}
}
