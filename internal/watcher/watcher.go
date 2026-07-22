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

const (
	// DefaultRefreshDebounce coalesces bursts of import and untrack membership
	// notifications. It is independent of the source quiet period exposed by
	// --debounce.
	DefaultRefreshDebounce = 50 * time.Millisecond
	parentRetryInitial     = 100 * time.Millisecond
	parentRetryAttempts    = 6
)

// Stats describes the exact sources and source directories armed at startup.
type Stats struct {
	Sources     int
	Directories int
}

// Options configures one dynamically reconciled watch session. All callbacks,
// index mutations, and watched imports run serially on the event-loop
// goroutine. Timers only signal the event-loop goroutine through channels.
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
	type parentRecovery struct {
		warned bool
	}
	recoveringParents := make(map[string]parentRecovery)

	reconcile := func(paths []string, activate bool, remove func(string)) bool {
		// Re-add every candidate parent once per refresh. Native watches are
		// discarded when a directory is deleted; refreshing the watch before the
		// membership check heals a directory that was later recreated.
		desired := make(map[string]struct{}, len(paths))
		desiredParents := make(map[string]struct{})
		parentResults := make(map[string]error)
		rearmedParents := make(map[string]struct{})
		for _, path := range paths {
			clean := filepath.Clean(path)
			if _, duplicate := desired[clean]; duplicate {
				continue
			}
			desired[clean] = struct{}{}
			parent := filepath.Dir(clean)
			desiredParents[parent] = struct{}{}
			armErr, attempted := parentResults[parent]
			if !attempted {
				state, recovering := recoveringParents[parent]
				armErr = w.Add(parent)
				parentResults[parent] = armErr
				if armErr != nil {
					if !state.warned {
						logger.Warn("cannot watch source directory; retrying briefly", "source", clean, "parent", parent, "err", armErr)
						state.warned = true
					}
					recoveringParents[parent] = state
				} else if recovering {
					delete(recoveringParents, parent)
					rearmedParents[parent] = struct{}{}
					if state.warned {
						logger.Info("restored source directory watch", "parent", parent)
					}
				}
			}
			if armErr != nil {
				continue
			}
			_, exists := ix.Match(clean)
			if !exists && !ix.Add(clean) {
				continue
			}
			_, rearmed := rearmedParents[parent]
			if activate && opts.Activate != nil && (!exists || rearmed) {
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
		for parent := range recoveringParents {
			if _, keep := desiredParents[parent]; !keep {
				delete(recoveringParents, parent)
			}
		}
		return len(recoveringParents) > 0
	}

	initial, err := opts.Load()
	if err != nil {
		return err
	}
	needsParentRetry := reconcile(initial, false, nil)
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

	var parentRetryTimer *time.Timer
	var parentRetryC <-chan time.Time
	parentRetryAttempt := 0
	parentRetryExhausted := false
	stopParentRetry := func() {
		if parentRetryTimer != nil {
			parentRetryTimer.Stop()
		}
		parentRetryTimer = nil
		parentRetryC = nil
		parentRetryAttempt = 0
		parentRetryExhausted = false
	}
	scheduleParentRetry := func() {
		if parentRetryC != nil {
			return
		}
		delay, retry := sourceParentRetryDelay(parentRetryAttempt)
		if !retry {
			if !parentRetryExhausted {
				logger.Warn(
					"source directory watches remain unavailable; automatic retries stopped",
					"parents", len(recoveringParents),
					"action", "run md2obs import for restored sources or restart the watcher",
				)
				parentRetryExhausted = true
			}
			return
		}
		parentRetryAttempt++
		parentRetryTimer = time.NewTimer(delay)
		parentRetryC = parentRetryTimer.C
	}
	defer stopParentRetry()

	refresh := func() error {
		paths, loadErr := opts.Load()
		if loadErr == nil {
			needsRetry := reconcile(paths, true, func(path string) {
				sourceDebouncer.Cancel(path)
				if opts.Remove != nil {
					opts.Remove(path)
				}
			})
			stopRetry()
			if needsRetry {
				scheduleParentRetry()
			} else {
				stopParentRetry()
			}
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
	if needsParentRetry {
		scheduleParentRetry()
	}

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			clean := filepath.Clean(ev.Name)
			if clean == notificationPath && ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
				if len(recoveringParents) > 0 {
					stopParentRetry()
				}
				refreshDebouncer.Trigger(notificationPath)
				continue
			}
			if clean == notificationParent && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				return fmt.Errorf("watch notification directory %s was removed or renamed; restart the watcher", notificationParent)
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && ix.HasParent(clean) {
				if _, recovering := recoveringParents[clean]; !recovering {
					stopParentRetry()
					recoveringParents[clean] = parentRecovery{}
				}
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
				logger.Warn("notification queue overflowed; refreshing membership, but source changes may have been missed", "action", "re-run affected imports or restart the watcher")
				refreshDebouncer.Trigger(notificationPath)
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
		case <-parentRetryC:
			parentRetryTimer = nil
			parentRetryC = nil
			if err := refresh(); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func sourceParentRetryDelay(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= parentRetryAttempts {
		return 0, false
	}
	return parentRetryInitial << attempt, true
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
