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
	_, release, err := claimManagedWatch(cfg, testManagedSettings())
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

func TestManagedWatchLeaseRejectsDuplicatesAndCleansUp(t *testing.T) {
	cfg := testManagedConfig(t)
	record, release, err := claimManagedWatch(cfg, testManagedSettings())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})

	if record.PID != os.Getpid() || record.InstanceID == "" || record.ProcessIdentity == "" || record.StartedAt.IsZero() {
		t.Fatalf("incomplete managed record: %+v", record)
	}
	state, err := inspectManagedWatch(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.Record.InstanceID != record.InstanceID {
		t.Fatalf("managed state = %+v, want running instance %s", state, record.InstanceID)
	}

	if _, duplicateRelease, err := claimManagedWatch(cfg, testManagedSettings()); err == nil {
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

func TestConcurrentManagedWatchClaimsHaveOneWinner(t *testing.T) {
	cfg := testManagedConfig(t)
	const contenders = 8
	type result struct {
		release func()
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, contenders)
	for range contenders {
		go func() {
			<-start
			_, release, err := claimManagedWatch(cfg, testManagedSettings())
			results <- result{release: release, err: err}
		}()
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

	_, releaseA, err := claimManagedWatch(cfgA, testManagedSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	_, releaseB, err := claimManagedWatch(&cfgB, testManagedSettings())
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
	record, release, err := claimManagedWatch(cfg, testManagedSettings())
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
