//go:build linux || darwin

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"md2obs/internal/config"
)

var errWatchProcessGone = errors.New("watch process no longer exists")

// managedWatchLeasePath scopes one foreground or managed watcher to the
// configured database and resolved vault. A database shared by two vaults
// therefore gets two independent leases without putting lifecycle files
// inside either vault.
func managedWatchLeasePath(cfg *config.Config) string {
	scope := sha256.Sum256([]byte(cfg.StateDBPath + "\x00" + cfg.VaultAbs))
	return fmt.Sprintf("%s.watch.%s.json", cfg.StateDBPath, hex.EncodeToString(scope[:12]))
}

func claimManagedWatch(cfg *config.Config, mode watchInstanceMode, settings managedWatchSettings) (managedWatchRecord, func(), error) {
	path := managedWatchLeasePath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return managedWatchRecord{}, nil, fmt.Errorf("create watch state directory: %w", err)
	}

	for attempt := 0; attempt < 4; attempt++ {
		lease, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return managedWatchRecord{}, nil, fmt.Errorf("open watcher record %s: %w", path, err)
		}
		if err := lease.Chmod(0o600); err != nil {
			lease.Close()
			return managedWatchRecord{}, nil, fmt.Errorf("secure watcher record %s: %w", path, err)
		}

		err = unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if isLeaseHeld(err) {
			record, readErr := readManagedWatchRecordRetry(lease, cfg)
			lease.Close()
			if readErr != nil {
				return managedWatchRecord{}, nil, fmt.Errorf("watcher lease is held but its record is invalid: %w", readErr)
			}
			return managedWatchRecord{}, nil, watchInstanceConflict(record)
		}
		if err != nil {
			lease.Close()
			return managedWatchRecord{}, nil, fmt.Errorf("lock watcher record %s: %w", path, err)
		}
		if !leaseStillNamesPath(lease, path) {
			lease.Close()
			continue
		}

		record, err := newManagedWatchRecord(cfg, mode, settings)
		if err != nil {
			lease.Close()
			return managedWatchRecord{}, nil, err
		}
		if err := writeManagedWatchRecord(lease, record); err != nil {
			removeLeasePathIfSame(lease, path)
			lease.Close()
			return managedWatchRecord{}, nil, err
		}

		var once sync.Once
		release := func() {
			once.Do(func() {
				removeLeasePathIfSame(lease, path)
				_ = lease.Close()
			})
		}
		return record, release, nil
	}
	return managedWatchRecord{}, nil, fmt.Errorf("watcher record %s changed repeatedly while acquiring its lease", path)
}

func inspectManagedWatch(cfg *config.Config) (managedWatchState, error) {
	path := managedWatchLeasePath(cfg)
	var identityMismatch error
	for attempt := 0; attempt < 4; attempt++ {
		lease, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			return managedWatchState{}, nil
		}
		if err != nil {
			return managedWatchState{}, fmt.Errorf("open watcher record %s: %w", path, err)
		}

		err = unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if isLeaseHeld(err) {
			record, readErr := readManagedWatchRecordRetry(lease, cfg)
			lease.Close()
			if readErr != nil {
				return managedWatchState{}, fmt.Errorf("watcher lease is held but its record is invalid: %w", readErr)
			}
			identity, identityErr := managedProcessIdentity(record.PID)
			if errors.Is(identityErr, errWatchProcessGone) {
				// The daemon exited between the lease check and process lookup.
				// Retry so the now-unlocked stale record is cleaned normally.
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if identityErr != nil {
				return managedWatchState{}, fmt.Errorf("verify watch daemon PID %d: %w", record.PID, identityErr)
			}
			if identity != record.ProcessIdentity {
				identityMismatch = fmt.Errorf("watch daemon PID %d has a different process identity; refusing to manage it", record.PID)
				time.Sleep(5 * time.Millisecond)
				continue
			}
			return managedWatchState{Running: true, Record: record}, nil
		}
		if err != nil {
			lease.Close()
			return managedWatchState{}, fmt.Errorf("inspect watcher lease %s: %w", path, err)
		}
		if !leaseStillNamesPath(lease, path) {
			lease.Close()
			continue
		}

		// An unlocked file is a stale record left by an ungraceful exit. It is
		// safe to remove while this process owns the lease.
		record, _ := readManagedWatchRecord(lease, cfg)
		removeLeasePathIfSame(lease, path)
		lease.Close()
		return managedWatchState{Record: record}, nil
	}
	if identityMismatch != nil {
		return managedWatchState{}, identityMismatch
	}
	return managedWatchState{}, fmt.Errorf("watcher record %s changed repeatedly while inspecting it", path)
}

func stopManagedWatch(ctx context.Context, cfg *config.Config, timeout time.Duration) (managedWatchRecord, bool, error) {
	state, err := inspectManagedWatch(cfg)
	if err != nil {
		return managedWatchRecord{}, false, err
	}
	if !state.Running {
		return managedWatchRecord{}, false, nil
	}
	if state.Record.Mode == watchModeForeground {
		return state.Record, false, fmt.Errorf(
			"watcher PID %d is running in the foreground; stop it with Ctrl-C",
			state.Record.PID,
		)
	}

	err = signalManagedProcess(state.Record)
	if err != nil && !errors.Is(err, errWatchProcessGone) {
		return state.Record, false, fmt.Errorf("stop watch daemon PID %d: %w", state.Record.PID, err)
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, inspectErr := inspectManagedWatch(cfg)
		if inspectErr != nil {
			return state.Record, false, inspectErr
		}
		if !current.Running {
			return state.Record, true, nil
		}
		if current.Record.InstanceID != state.Record.InstanceID {
			return state.Record, false, fmt.Errorf(
				"watch daemon PID %d exited but was replaced by PID %d while stopping",
				state.Record.PID,
				current.Record.PID,
			)
		}

		select {
		case <-ctx.Done():
			return state.Record, false, ctx.Err()
		case <-deadline.C:
			return state.Record, false, fmt.Errorf(
				"timed out after %s waiting for watch daemon PID %d to stop",
				timeout,
				state.Record.PID,
			)
		case <-ticker.C:
		}
	}
}

func newManagedWatchRecord(cfg *config.Config, mode watchInstanceMode, settings managedWatchSettings) (managedWatchRecord, error) {
	if mode != watchModeForeground && mode != watchModeManaged {
		return managedWatchRecord{}, fmt.Errorf("invalid watch instance mode %q", mode)
	}
	identity, err := managedProcessIdentity(os.Getpid())
	if err != nil {
		return managedWatchRecord{}, fmt.Errorf("identify watcher process: %w", err)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return managedWatchRecord{}, fmt.Errorf("generate watcher instance ID: %w", err)
	}
	return managedWatchRecord{
		Version:         managedWatchRecordVersion,
		Mode:            mode,
		PID:             os.Getpid(),
		InstanceID:      hex.EncodeToString(nonce),
		StartedAt:       time.Now().UTC(),
		ProcessIdentity: identity,
		StateDatabase:   cfg.StateDBPath,
		Vault:           cfg.VaultAbs,
		Settings:        settings,
	}, nil
}

func writeManagedWatchRecord(file *os.File, record managedWatchRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode watcher record: %w", err)
	}
	data = append(data, '\n')
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("reset watcher record: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind watcher record: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write watcher record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync watcher record: %w", err)
	}
	return nil
}

func readManagedWatchRecordRetry(file *os.File, cfg *config.Config) (managedWatchRecord, error) {
	deadline := time.Now().Add(250 * time.Millisecond)
	var record managedWatchRecord
	var err error
	for {
		record, err = readManagedWatchRecord(file, cfg)
		if err == nil || time.Now().After(deadline) {
			return record, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readManagedWatchRecord(file *os.File, cfg *config.Config) (managedWatchRecord, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return managedWatchRecord{}, fmt.Errorf("rewind record: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return managedWatchRecord{}, fmt.Errorf("read record: %w", err)
	}
	var record managedWatchRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return managedWatchRecord{}, fmt.Errorf("decode record: %w", err)
	}
	switch record.Version {
	case legacyManagedWatchRecordVersion:
		// Version 1 records predate foreground lease participation, so every
		// live version 1 owner is necessarily a managed daemon.
		record.Mode = watchModeManaged
	case managedWatchRecordVersion:
		if record.Mode != watchModeForeground && record.Mode != watchModeManaged {
			return managedWatchRecord{}, fmt.Errorf("invalid watch instance mode %q", record.Mode)
		}
	default:
		return managedWatchRecord{}, fmt.Errorf("unsupported record version %d", record.Version)
	}
	if record.PID <= 0 || record.InstanceID == "" || record.ProcessIdentity == "" || record.StartedAt.IsZero() {
		return managedWatchRecord{}, errors.New("record is missing process identity fields")
	}
	if record.StateDatabase != cfg.StateDBPath || record.Vault != cfg.VaultAbs {
		return managedWatchRecord{}, errors.New("record belongs to a different database or vault")
	}
	return record, nil
}

func isLeaseHeld(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

func leaseStillNamesPath(file *os.File, path string) bool {
	fileInfo, err := file.Stat()
	if err != nil {
		return false
	}
	pathInfo, err := os.Stat(path)
	return err == nil && os.SameFile(fileInfo, pathInfo)
}

func removeLeasePathIfSame(file *os.File, path string) {
	if leaseStillNamesPath(file, path) {
		_ = os.Remove(path)
	}
}
