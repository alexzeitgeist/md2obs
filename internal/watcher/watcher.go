package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Run blocks on native filesystem notifications for the index's parent
// directories until ctx is cancelled. Events on exactly-selected paths are
// debounced per source and then passed to handle, serialized on this
// goroutine. There is no periodic ticker: an idle watcher consumes no
// user-space CPU.
func Run(ctx context.Context, ix *Index, debounce time.Duration, handle func(path string), logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer w.Close()

	watched := 0
	for _, parent := range ix.Parents() {
		if err := w.Add(parent); err != nil {
			logger.Warn("cannot watch directory", "parent", parent, "err", err)
			continue
		}
		watched++
	}
	if watched == 0 {
		return errors.New("no directories could be watched")
	}

	deb := NewDebouncer(debounce)
	defer deb.Stop()

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// Create covers atomic saves (temp file renamed onto the
			// source); Write covers in-place saves; Rename marks the path
			// changing identity — the post-debounce existence check decides
			// what to do.
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
				if p, ok := ix.Match(ev.Name); ok {
					deb.Trigger(p)
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				logger.Warn("notification queue overflowed; changes may have been missed", "action", "re-run md2obs import on files you changed")
			} else {
				logger.Error("filesystem watcher error", "err", err)
			}
		case p := <-deb.C:
			handle(p)
		case <-ctx.Done():
			return nil
		}
	}
}
