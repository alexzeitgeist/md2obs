package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"md2obs/internal/app"
	"md2obs/internal/config"
)

const (
	daemonChildEnv   = "MD2OBS_DAEMON_CHILD"
	daemonReadyFDEnv = "MD2OBS_DAEMON_READY_FD"
	daemonReadyByte  = byte(1)
)

type daemonProcess struct {
	PID     int
	LogPath string
}

const (
	legacyManagedWatchRecordVersion = 1
	managedWatchRecordVersion       = 2
)

type watchInstanceMode string

const (
	watchModeForeground watchInstanceMode = "foreground"
	watchModeManaged    watchInstanceMode = "managed"
)

// managedWatchSettings are persisted with the instance so a bare restart can
// preserve the settings of the running watcher.
type managedWatchSettings struct {
	Days          int    `json:"days"`
	Debounce      string `json:"debounce"`
	OnVaultChange string `json:"on_vault_change"`
	Log           bool   `json:"log"`
}

// managedWatchRecord is both the human-inspectable instance record and the
// contents protected by the live watcher's lease. ProcessIdentity is an OS
// start marker; InstanceID identifies this particular acquisition of the
// lease rather than merely a PID.
type managedWatchRecord struct {
	Version         int                  `json:"version"`
	Mode            watchInstanceMode    `json:"mode"`
	PID             int                  `json:"pid"`
	InstanceID      string               `json:"instance_id"`
	StartedAt       time.Time            `json:"started_at"`
	ProcessIdentity string               `json:"process_identity"`
	StateDatabase   string               `json:"state_database"`
	Vault           string               `json:"vault"`
	Settings        managedWatchSettings `json:"settings"`
}

type managedWatchState struct {
	Running     bool
	Unsupported bool
	Record      managedWatchRecord
}

var managedWatchStopTimeout = 10 * time.Second

// launchWatchDaemon is replaceable in command tests. Production calls always
// use startWatchDaemon.
var launchWatchDaemon = startWatchDaemon

func managedSettings(options commandOptions) managedWatchSettings {
	return managedWatchSettings{
		Days:          options.watch.Days,
		Debounce:      options.watch.Debounce.String(),
		OnVaultChange: string(options.watch.OnVaultChange),
		Log:           options.log,
	}
}

func applyManagedSettings(options *commandOptions, settings managedWatchSettings) {
	debounce, err := time.ParseDuration(settings.Debounce)
	if err != nil || settings.Days < 1 {
		return
	}
	policy, err := app.ParsePolicy(settings.OnVaultChange)
	if err != nil {
		return
	}
	options.watch = app.WatchOptions{
		Days:          settings.Days,
		Debounce:      debounce,
		OnVaultChange: policy,
	}
	options.log = settings.Log
}

func managedWatchArgs(options commandOptions) []string {
	settings := managedSettings(options)
	args := []string{
		"watch",
		"start",
		"--days=" + strconv.Itoa(settings.Days),
		"--debounce=" + settings.Debounce,
		"--on-vault-change=" + settings.OnVaultChange,
	}
	if settings.Log {
		args = append(args, "--log")
	}
	return args
}

func formatWatchStatus(state managedWatchState) string {
	if state.Unsupported {
		return "Watcher:           state unavailable on this platform"
	}
	if !state.Running {
		return "Watcher:           stopped"
	}
	description := "running as daemon"
	if state.Record.Mode == watchModeForeground {
		description = "running in foreground"
	}
	return fmt.Sprintf(
		"Watcher:           %s (PID %d, started %s)",
		description,
		state.Record.PID,
		state.Record.StartedAt.Local().Format(time.RFC3339),
	)
}

func watchInstanceConflict(record managedWatchRecord) error {
	if record.Mode == watchModeForeground {
		return fmt.Errorf("watcher is already running in foreground (PID %d); stop it with Ctrl-C", record.PID)
	}
	return fmt.Errorf("watcher is already running as a daemon (PID %d)", record.PID)
}

func runWatchStop(ctx context.Context, cfg *config.Config) int {
	record, stopped, err := stopManagedWatch(ctx, cfg, managedWatchStopTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	if !stopped {
		fmt.Fprintln(os.Stdout, "No md2obs watch daemon is running.")
		return 0
	}
	fmt.Fprintf(os.Stdout, "Stopped md2obs watch daemon (PID %d)\n", record.PID)
	return 0
}

// startWatchDaemon re-executes md2obs with the same watch arguments. The
// readiness pipe is inherited as fd 3; the child acknowledges only after the
// fsnotify watches and initial membership are armed.
func startWatchDaemon(ctx context.Context, executable string, args []string, cfg *config.Config, logging bool) (daemonProcess, error) {
	if err := ctx.Err(); err != nil {
		return daemonProcess{}, err
	}
	cmd := exec.Command(executable, args...)
	cmd.Dir = string(os.PathSeparator)
	cmd.Env = daemonEnvironment(cfg)
	if err := configureDaemonProcess(cmd); err != nil {
		return daemonProcess{}, err
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return daemonProcess{}, fmt.Errorf("open %s for daemon input and output: %w", os.DevNull, err)
	}
	defer devNull.Close()

	logPath := ""
	output := devNull
	if logging {
		if err := os.MkdirAll(filepath.Dir(cfg.StateDBPath), 0o755); err != nil {
			return daemonProcess{}, fmt.Errorf("create state directory for daemon log: %w", err)
		}
		logPath = daemonLogPath(cfg.StateDBPath)
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return daemonProcess{}, fmt.Errorf("open daemon log %s: %w", logPath, err)
		}
		defer logFile.Close()
		if err := logFile.Chmod(0o600); err != nil {
			return daemonProcess{}, fmt.Errorf("secure daemon log %s: %w", logPath, err)
		}
		output = logFile
	}

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return daemonProcess{}, fmt.Errorf("create daemon readiness pipe: %w", err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()

	cmd.Stdin = devNull
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.ExtraFiles = []*os.File{readyWrite}
	if err := cmd.Start(); err != nil {
		return daemonProcess{}, fmt.Errorf("start watch daemon: %w", err)
	}
	// Only the child may retain the write side. Otherwise EOF cannot report an
	// early child exit to the parent.
	if err := readyWrite.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return daemonProcess{}, fmt.Errorf("close parent readiness pipe: %w", err)
	}

	ready := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, readErr := io.ReadFull(readyRead, marker[:])
		if readErr == nil && marker[0] != daemonReadyByte {
			readErr = fmt.Errorf("unexpected readiness marker %d", marker[0])
		}
		ready <- readErr
	}()

	select {
	case err := <-ready:
		if err != nil {
			// A readiness error does not guarantee that the child exited: it may
			// have written a bad marker or closed the protocol fd and kept running.
			_ = cmd.Process.Kill()
			waitErr := cmd.Wait()
			if waitErr == nil {
				waitErr = errors.New("process exited without reporting an error")
			}
			// A concurrent start may have won the lease after the caller's
			// preflight check but before this child acquired it. Surface that
			// actionable result even when daemon logging is disabled.
			if state, inspectErr := inspectManagedWatch(cfg); inspectErr == nil && state.Running && state.Record.PID != cmd.Process.Pid {
				return daemonProcess{}, watchInstanceConflict(state.Record)
			}
			if logPath != "" {
				return daemonProcess{}, fmt.Errorf("watch daemon exited before becoming ready: %w (log: %s)", waitErr, logPath)
			}
			return daemonProcess{}, fmt.Errorf("watch daemon exited before becoming ready: %w", waitErr)
		}
		pid := cmd.Process.Pid
		if err := cmd.Process.Release(); err != nil {
			return daemonProcess{}, fmt.Errorf("release watch daemon process %d: %w", pid, err)
		}
		return daemonProcess{PID: pid, LogPath: logPath}, nil

	case <-ctx.Done():
		_ = readyRead.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return daemonProcess{}, ctx.Err()
	}
}

func daemonLogPath(databasePath string) string {
	return databasePath + ".watch.log"
}

func daemonEnvironment(cfg *config.Config) []string {
	overrides := map[string]string{
		daemonChildEnv:    "1",
		daemonReadyFDEnv:  "3",
		"MD2OBS_VAULT":    cfg.VaultAbs,
		"MD2OBS_STATE_DB": cfg.StateDBPath,
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func isDaemonChild() bool {
	return os.Getenv(daemonChildEnv) == "1"
}

// daemonReadySignal adopts the inherited readiness fd. cleanup must be
// deferred so any startup failure closes the pipe and wakes the parent.
func daemonReadySignal() (signal func(), cleanup func(), err error) {
	rawFD := os.Getenv(daemonReadyFDEnv)
	fd, err := strconv.ParseUint(rawFD, 10, 32)
	if err != nil || fd < 3 {
		return nil, nil, fmt.Errorf("invalid internal daemon readiness fd %q", rawFD)
	}
	readyFile := os.NewFile(uintptr(fd), "md2obs-daemon-ready")
	if readyFile == nil {
		return nil, nil, fmt.Errorf("open internal daemon readiness fd %d", fd)
	}

	var once sync.Once
	closeReady := func(write bool) {
		once.Do(func() {
			if write {
				_, _ = readyFile.Write([]byte{daemonReadyByte})
			}
			_ = readyFile.Close()
		})
	}
	return func() { closeReady(true) }, func() { closeReady(false) }, nil
}
