//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"md2obs/internal/config"
)

const (
	managedHelperModeEnv  = "MD2OBS_MANAGED_HELPER_MODE"
	managedHelperDBEnv    = "MD2OBS_MANAGED_HELPER_DB"
	managedHelperVaultEnv = "MD2OBS_MANAGED_HELPER_VAULT"
	foregroundHelperEnv   = "MD2OBS_FOREGROUND_HELPER"
)

// TestManagedWatchHelperProcess owns a real lease in a subprocess so stop can
// be exercised without ever signaling the test runner itself.
func TestManagedWatchHelperProcess(t *testing.T) {
	mode := os.Getenv(managedHelperModeEnv)
	if mode == "" {
		return
	}
	cfg := &config.Config{
		StateDBPath: os.Getenv(managedHelperDBEnv),
		VaultAbs:    os.Getenv(managedHelperVaultEnv),
	}
	if mode == "ignore" {
		signal.Ignore(syscall.SIGTERM)
	}
	_, release, err := claimManagedWatch(cfg, watchModeManaged, testManagedSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if mode == "ignore" {
		select {}
	}
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	defer signal.Stop(terminated)
	<-terminated
}

// TestForegroundWatchHelperProcess runs the real foreground command in a
// subprocess so its lease and SIGTERM cleanup can be tested end to end.
func TestForegroundWatchHelperProcess(t *testing.T) {
	if os.Getenv(foregroundHelperEnv) != "1" {
		return
	}
	if code := run([]string{"watch"}); code != 0 {
		t.Fatalf("foreground watch exit code = %d", code)
	}
}

func TestManagedWatchLeaseRejectsDuplicatesAndCleansUp(t *testing.T) {
	cfg := testManagedConfig(t)
	record, release, err := claimManagedWatch(cfg, watchModeManaged, testManagedSettings())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})

	if record.Version != managedWatchRecordVersion ||
		record.Mode != watchModeManaged ||
		record.PID != os.Getpid() ||
		record.InstanceID == "" ||
		record.ProcessIdentity == "" ||
		record.StartedAt.IsZero() {
		t.Fatalf("incomplete managed record: %+v", record)
	}
	state, err := inspectManagedWatch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.Record.InstanceID != record.InstanceID {
		t.Fatalf("managed state = %+v, want running instance %s", state, record.InstanceID)
	}
	if got := formatWatchStatus(state); !strings.Contains(got, "running as daemon") {
		t.Fatalf("managed status = %q", got)
	}

	if _, duplicateRelease, err := claimManagedWatch(cfg, watchModeManaged, testManagedSettings()); err == nil {
		if duplicateRelease != nil {
			duplicateRelease()
		}
		t.Fatal("duplicate managed watcher acquired the lease")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("duplicate claim error = %v", err)
	}

	info, err := os.Stat(managedWatchLeasePath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record permissions = %o, want 600", info.Mode().Perm())
	}

	release()
	released = true
	state, err = inspectManagedWatch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state.Running {
		t.Fatalf("managed state after release = %+v", state)
	}
	if _, err := os.Stat(managedWatchLeasePath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released record still exists: %v", err)
	}
}

func TestConcurrentWatchClaimsAcrossModesHaveOneWinner(t *testing.T) {
	cfg := testManagedConfig(t)
	const contenders = 8
	type result struct {
		release func()
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, contenders)
	for contender := range contenders {
		mode := watchModeManaged
		if contender%2 == 0 {
			mode = watchModeForeground
		}
		go func(mode watchInstanceMode) {
			<-start
			_, release, err := claimManagedWatch(cfg, mode, testManagedSettings())
			results <- result{release: release, err: err}
		}(mode)
	}
	close(start)

	winners := 0
	var winnerRelease func()
	for range contenders {
		result := <-results
		if result.err == nil {
			winners++
			winnerRelease = result.release
			continue
		}
		if !strings.Contains(result.err.Error(), "already running") {
			t.Errorf("losing claim error = %v", result.err)
		}
	}
	if winners != 1 {
		if winnerRelease != nil {
			winnerRelease()
		}
		t.Fatalf("successful concurrent claims = %d, want 1", winners)
	}
	winnerRelease()
}

func TestForegroundWatchUsesSharedLeaseAndRefusesRemoteStop(t *testing.T) {
	cfg := testManagedConfig(t)
	cmd, done := startForegroundHelper(t, cfg)
	state := waitForManagedHelper(t, cfg)
	if state.Record.Mode != watchModeForeground {
		t.Fatalf("foreground helper mode = %q", state.Record.Mode)
	}
	if got := formatWatchStatus(state); !strings.Contains(got, "running in foreground") {
		t.Fatalf("foreground status = %q", got)
	}

	if _, release, err := claimManagedWatch(cfg, watchModeManaged, testManagedSettings()); err == nil {
		release()
		t.Fatal("managed watcher acquired a foreground watcher's lease")
	} else if !strings.Contains(err.Error(), "running in foreground") {
		t.Fatalf("managed claim conflict = %v", err)
	}

	_, stopped, err := stopManagedWatch(context.Background(), cfg, time.Second)
	if err == nil || !strings.Contains(err.Error(), "stop it with Ctrl-C") {
		t.Fatalf("foreground stop error = %v", err)
	}
	if stopped {
		t.Fatal("foreground watcher reported remotely stopped")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("foreground helper exit: %v", err)
	}
	state, err = inspectManagedWatch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state.Running {
		t.Fatalf("foreground lease remained after SIGTERM: %+v", state)
	}
}

func TestReadLegacyManagedRecordDefaultsToManagedMode(t *testing.T) {
	cfg := testManagedConfig(t)
	record, err := newManagedWatchRecord(cfg, watchModeManaged, testManagedSettings())
	if err != nil {
		t.Fatal(err)
	}
	record.Version = legacyManagedWatchRecordVersion
	record.Mode = ""
	path := filepath.Join(t.TempDir(), "legacy-watch.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeManagedWatchRecord(file, record); err != nil {
		t.Fatal(err)
	}
	decoded, err := readManagedWatchRecord(file, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Mode != watchModeManaged {
		t.Fatalf("legacy record mode = %q, want managed", decoded.Mode)
	}
}

func TestManagedWatchScopesLeaseByDatabaseAndVault(t *testing.T) {
	cfgA := testManagedConfig(t)
	cfgB := *cfgA
	cfgB.VaultAbs = filepath.Join(t.TempDir(), "other-vault")
	if err := os.Mkdir(cfgB.VaultAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	if managedWatchLeasePath(cfgA) == managedWatchLeasePath(&cfgB) {
		t.Fatal("different vaults sharing a database received the same lease path")
	}

	_, releaseA, err := claimManagedWatch(cfgA, watchModeManaged, testManagedSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	_, releaseB, err := claimManagedWatch(&cfgB, watchModeManaged, testManagedSettings())
	if err != nil {
		t.Fatalf("second scope could not acquire an independent lease: %v", err)
	}
	defer releaseB()
}

func TestInspectManagedWatchRemovesStaleRecord(t *testing.T) {
	cfg := testManagedConfig(t)
	path := managedWatchLeasePath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("an interrupted partial record"), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := inspectManagedWatch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state.Running {
		t.Fatalf("stale record reported running: %+v", state)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale record was not removed: %v", err)
	}
}

func TestInspectManagedWatchRejectsChangedProcessIdentity(t *testing.T) {
	cfg := testManagedConfig(t)
	record, release, err := claimManagedWatch(cfg, watchModeManaged, testManagedSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	record.ProcessIdentity = "not-this-process"
	file, err := os.OpenFile(managedWatchLeasePath(cfg), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManagedWatchRecord(file, record); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()

	_, err = inspectManagedWatch(cfg)
	if err == nil || !strings.Contains(err.Error(), "different process identity") {
		t.Fatalf("identity mismatch error = %v", err)
	}
}

func TestStopManagedWatchSignalsAndWaitsForGracefulExit(t *testing.T) {
	cfg := testManagedConfig(t)
	_, done := startManagedHelper(t, cfg, "graceful")
	state := waitForManagedHelper(t, cfg)

	record, stopped, err := stopManagedWatch(context.Background(), cfg, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped || record.InstanceID != state.Record.InstanceID {
		t.Fatalf("stop result = record %+v, stopped %v", record, stopped)
	}
	if err := <-done; err != nil {
		t.Fatalf("managed helper exit: %v", err)
	}
	if _, err := os.Stat(managedWatchLeasePath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record after graceful stop: %v", err)
	}
}

func TestStopManagedWatchReportsTimeout(t *testing.T) {
	cfg := testManagedConfig(t)
	cmd, done := startManagedHelper(t, cfg, "ignore")
	waitForManagedHelper(t, cfg)

	_, stopped, err := stopManagedWatch(context.Background(), cfg, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Fatalf("timeout error = %v", err)
	}
	if stopped {
		t.Fatal("timed-out managed watcher reported stopped")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("killed helper exited successfully")
	}
	state, inspectErr := inspectManagedWatch(cfg)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if state.Running {
		t.Fatal("killed helper still reported running")
	}
}

func testManagedConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		StateDBPath: filepath.Join(root, "state", "state.db"),
		VaultAbs:    vault,
	}
}

func testManagedSettings() managedWatchSettings {
	return managedWatchSettings{
		Days:          3,
		Debounce:      "750ms",
		OnVaultChange: "preserve",
		Log:           true,
	}
}

func startManagedHelper(t *testing.T, cfg *config.Config, mode string) (*exec.Cmd, <-chan error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestManagedWatchHelperProcess$")
	cmd.Env = append(os.Environ(),
		managedHelperModeEnv+"="+mode,
		managedHelperDBEnv+"="+cfg.StateDBPath,
		managedHelperVaultEnv+"="+cfg.VaultAbs,
		daemonChildEnv+"=",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return cmd, done
}

func startForegroundHelper(t *testing.T, cfg *config.Config) (*exec.Cmd, <-chan error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestForegroundWatchHelperProcess$")
	outputPath := filepath.Join(t.TempDir(), "foreground-helper.log")
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { output.Close() })
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Env = append(os.Environ(),
		foregroundHelperEnv+"=1",
		"MD2OBS_STATE_DB="+cfg.StateDBPath,
		"MD2OBS_VAULT="+cfg.VaultAbs,
		daemonChildEnv+"=",
		managedHelperModeEnv+"=",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, _ := os.ReadFile(outputPath)
		if strings.Contains(string(contents), "Watching ") {
			return cmd, done
		}
		time.Sleep(10 * time.Millisecond)
	}
	contents, _ := os.ReadFile(outputPath)
	t.Fatalf("foreground helper did not become ready; output:\n%s", contents)
	return nil, nil
}

func waitForManagedHelper(t *testing.T, cfg *config.Config) managedWatchState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := inspectManagedWatch(cfg)
		if err == nil && state.Running {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("managed helper did not acquire its lease")
	return managedWatchState{}
}
