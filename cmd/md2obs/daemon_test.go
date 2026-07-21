//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"md2obs/internal/config"
)

const daemonTestModeEnv = "MD2OBS_DAEMON_TEST_MODE"

// TestDaemonHelperProcess is re-executed by the daemon tests below. In the
// ordinary test process it is a no-op.
func TestDaemonHelperProcess(t *testing.T) {
	if !isDaemonChild() {
		return
	}
	fmt.Fprintln(os.Stdout, "daemon helper started")
	mode := os.Getenv(daemonTestModeEnv)
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "intentional startup failure")
		os.Exit(23)
	}
	if mode == "bad-marker" {
		fd, err := strconv.ParseUint(os.Getenv(daemonReadyFDEnv), 10, 32)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(26)
		}
		ready := os.NewFile(uintptr(fd), "md2obs-bad-daemon-ready")
		if ready == nil {
			os.Exit(27)
		}
		_, _ = ready.Write([]byte{daemonReadyByte + 1})
		_ = ready.Close()
		// Without parent-side termination, an invalid marker would leave the
		// launcher blocked in Wait until this deliberately live child exits.
		time.Sleep(3 * time.Second)
		return
	}

	signalReady, cleanup, err := daemonReadySignal()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(24)
	}
	defer cleanup()
	workingDirectory, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(25)
	}
	fmt.Fprintf(os.Stdout, "working directory: %s\n", workingDirectory)
	signalReady()
	// Stay alive long enough for the parent test to verify and terminate the
	// released daemon process.
	time.Sleep(30 * time.Second)
}

func TestStartWatchDaemonWaitsForReadyWithoutCreatingLogByDefault(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		VaultAbs:    root,
		StateDBPath: filepath.Join(root, "state", "state.db"),
	}
	t.Setenv(daemonTestModeEnv, "ready")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := startWatchDaemon(
		context.Background(),
		executable,
		[]string{"-test.run=^TestDaemonHelperProcess$"},
		cfg,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if process.PID <= 0 {
		t.Fatalf("daemon PID = %d", process.PID)
	}
	daemon, err := os.FindProcess(process.PID)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = daemon.Kill()
		}
	})
	if process.LogPath != "" {
		t.Fatalf("log path = %q, want empty", process.LogPath)
	}

	if err := daemon.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("daemon was not alive after readiness: %v", err)
	}

	if _, err := os.Stat(daemonLogPath(cfg.StateDBPath)); !os.IsNotExist(err) {
		t.Fatalf("daemon created a log by default: %v", err)
	}
	if err := daemon.Kill(); err != nil {
		t.Fatalf("stop helper daemon: %v", err)
	}
	stopped = true
}

func TestStartWatchDaemonRedirectsLogsWhenEnabled(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		VaultAbs:    root,
		StateDBPath: filepath.Join(root, "state", "state.db"),
	}
	t.Setenv(daemonTestModeEnv, "ready")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := startWatchDaemon(
		context.Background(),
		executable,
		[]string{"-test.run=^TestDaemonHelperProcess$"},
		cfg,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := os.FindProcess(process.PID)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = daemon.Kill()
		}
	})

	if process.LogPath != daemonLogPath(cfg.StateDBPath) {
		t.Fatalf("log path = %q", process.LogPath)
	}
	log := waitForFileContaining(t, process.LogPath, "working directory: /\n")
	if !strings.Contains(log, "daemon helper started") {
		t.Fatalf("daemon stdout was not redirected to log:\n%s", log)
	}
	if info, err := os.Stat(process.LogPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("daemon log permissions = %o, want 600", info.Mode().Perm())
	}
	if err := daemon.Kill(); err != nil {
		t.Fatalf("stop helper daemon: %v", err)
	}
	stopped = true
}

func TestStartWatchDaemonHonorsCanceledContext(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		VaultAbs:    root,
		StateDBPath: filepath.Join(root, "state", "state.db"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := startWatchDaemon(ctx, "/does/not/matter", []string{"watch", "start"}, cfg, false)
	if err == nil || err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(daemonLogPath(cfg.StateDBPath)); !os.IsNotExist(err) {
		t.Fatalf("canceled launch created a daemon log: %v", err)
	}
}

func TestStartWatchDaemonReportsEarlyExit(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		VaultAbs:    root,
		StateDBPath: filepath.Join(root, "state", "state.db"),
	}
	t.Setenv(daemonTestModeEnv, "fail")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = startWatchDaemon(
		context.Background(),
		executable,
		[]string{"-test.run=^TestDaemonHelperProcess$"},
		cfg,
		true,
	)
	if err == nil {
		t.Fatal("early daemon exit was accepted")
	}
	for _, want := range []string{"exited before becoming ready", daemonLogPath(cfg.StateDBPath)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q: %v", want, err)
		}
	}
	log := waitForFileContaining(t, daemonLogPath(cfg.StateDBPath), "intentional startup failure")
	if !strings.Contains(log, "daemon helper started") {
		t.Fatalf("daemon output missing from log:\n%s", log)
	}
}

func TestStartWatchDaemonReportsEarlyExitWithoutLogByDefault(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		VaultAbs:    root,
		StateDBPath: filepath.Join(root, "state", "state.db"),
	}
	t.Setenv(daemonTestModeEnv, "fail")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = startWatchDaemon(
		context.Background(),
		executable,
		[]string{"-test.run=^TestDaemonHelperProcess$"},
		cfg,
		false,
	)
	if err == nil {
		t.Fatal("early daemon exit was accepted")
	}
	if !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Fatalf("unexpected startup error: %v", err)
	}
	if strings.Contains(err.Error(), "log:") {
		t.Fatalf("startup error reports a disabled log: %v", err)
	}
	if _, statErr := os.Stat(daemonLogPath(cfg.StateDBPath)); !os.IsNotExist(statErr) {
		t.Fatalf("failed daemon created a log by default: %v", statErr)
	}
}

func TestStartWatchDaemonKillsChildAfterUnexpectedReadyMarker(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		VaultAbs:    root,
		StateDBPath: filepath.Join(root, "state", "state.db"),
	}
	t.Setenv(daemonTestModeEnv, "bad-marker")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = startWatchDaemon(
		context.Background(),
		executable,
		[]string{"-test.run=^TestDaemonHelperProcess$"},
		cfg,
		false,
	)
	if err == nil {
		t.Fatal("unexpected readiness marker was accepted")
	}
	if !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Fatalf("unexpected startup error: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("startup error does not retain child exit status: %v", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child wait status = %v, want SIGKILL", exitErr.Sys())
	}
}

func waitForFileContaining(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		last, _ = os.ReadFile(path)
		if strings.Contains(string(last), want) {
			return string(last)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not contain %q; contents:\n%s", path, want, last)
	return ""
}
