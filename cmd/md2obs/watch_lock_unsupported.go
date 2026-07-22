//go:build !linux && !darwin

package main

import "github.com/alexzeitgeist/md2obs/internal/config"

// Native foreground-watcher locking is currently implemented on the two
// platforms md2obs supports for watch operation. Other platforms retain the
// foreground watcher without cross-process exclusivity.
func acquireWatchLock(*config.Config) (func(), error) {
	return func() {}, nil
}
