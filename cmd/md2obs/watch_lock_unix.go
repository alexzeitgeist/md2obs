//go:build linux || darwin

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"md2obs/internal/config"
)

var errWatchAlreadyRunning = errors.New("another watcher is already running for this database and vault")

// watchLockPath retains the former watcher lease path so a foreground watcher
// cannot overlap an older background watcher that is still running during an
// upgrade. The file is now only a stable flock rendezvous point; its
// contents, if any, are ignored.
func watchLockPath(cfg *config.Config) string {
	scope := sha256.Sum256([]byte(cfg.StateDBPath + "\x00" + cfg.VaultAbs))
	return fmt.Sprintf("%s.watch.%s.json", cfg.StateDBPath, hex.EncodeToString(scope[:12]))
}

// acquireWatchLock enforces one foreground watcher for the resolved database
// and vault pair. The lock file remains in place between runs; closing the
// descriptor releases the kernel lock, including on abnormal process exit.
func acquireWatchLock(cfg *config.Config) (func(), error) {
	path := watchLockPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create watch state directory: %w", err)
	}

	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open watcher lock %s: %w", path, err)
	}
	if err := lock.Chmod(0o600); err != nil {
		lock.Close()
		return nil, fmt.Errorf("secure watcher lock %s: %w", path, err)
	}

	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errWatchAlreadyRunning
		}
		return nil, fmt.Errorf("lock watcher file %s: %w", path, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() { _ = lock.Close() })
	}, nil
}
