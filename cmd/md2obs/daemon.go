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

// launchWatchDaemon is replaceable in command tests. Production calls always
// use startWatchDaemon.
var launchWatchDaemon = startWatchDaemon

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
			waitErr := cmd.Wait()
			if waitErr == nil {
				waitErr = errors.New("process exited without reporting an error")
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
