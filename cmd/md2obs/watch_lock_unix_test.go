//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"md2obs/internal/config"
	"md2obs/internal/watcher"
)

const (
	watchLockHelperEnv   = "MD2OBS_WATCH_LOCK_HELPER"
	watchLockHelperDBEnv = "MD2OBS_WATCH_LOCK_HELPER_DB"
	watchLockHelperVEnv  = "MD2OBS_WATCH_LOCK_HELPER_VAULT"
)

func TestWatchLockHelperProcess(t *testing.T) {
	if os.Getenv(watchLockHelperEnv) != "1" {
		return
	}
	cfg := &config.Config{
		StateDBPath: os.Getenv(watchLockHelperDBEnv),
		VaultAbs:    os.Getenv(watchLockHelperVEnv),
	}
	release, err := acquireWatchLock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestWatchLockRejectsSameScopeAndReleases(t *testing.T) {
	cfg := testWatchLockConfig(t, "state.db", "vault")
	wantScope := sha256.Sum256([]byte(cfg.StateDBPath + "\x00" + cfg.VaultAbs))
	wantPath := fmt.Sprintf("%s.watch.%s.json", cfg.StateDBPath, hex.EncodeToString(wantScope[:12]))
	if got := watchLockPath(cfg); got != wantPath {
		t.Fatalf("watch lock path = %q, want legacy-compatible %q", got, wantPath)
	}

	release, err := acquireWatchLock(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := acquireWatchLock(cfg); !errors.Is(err, errWatchAlreadyRunning) {
		t.Fatalf("second lock error = %v, want %v", err, errWatchAlreadyRunning)
	}

	info, err := os.Stat(watchLockPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("watch lock permissions = %o, want 600", got)
	}

	release()
	release() // Release is intentionally idempotent.

	releaseAgain, err := acquireWatchLock(cfg)
	if err != nil {
		t.Fatalf("reacquire released watch lock: %v", err)
	}
	releaseAgain()
	if _, err := os.Stat(watchLockPath(cfg)); err != nil {
		t.Fatalf("stable watch lock file was removed: %v", err)
	}
}

func TestWatchLockScopesDatabaseAndVault(t *testing.T) {
	root := t.TempDir()
	stateA := filepath.Join(root, "state-a.db")
	stateB := filepath.Join(root, "state-b.db")
	vaultA := filepath.Join(root, "vault-a")
	vaultB := filepath.Join(root, "vault-b")

	configs := []*config.Config{
		{StateDBPath: stateA, VaultAbs: vaultA},
		{StateDBPath: stateA, VaultAbs: vaultB},
		{StateDBPath: stateB, VaultAbs: vaultA},
	}
	paths := make(map[string]struct{}, len(configs))
	var releases []func()
	for _, cfg := range configs {
		path := watchLockPath(cfg)
		if _, duplicate := paths[path]; duplicate {
			t.Fatalf("distinct watch scope reused lock path %s", path)
		}
		paths[path] = struct{}{}

		release, err := acquireWatchLock(cfg)
		if err != nil {
			t.Fatalf("acquire independent watch scope %s: %v", path, err)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

func TestWatchIdentityUsesCanonicalDatabasePath(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateAlias := filepath.Join(root, "state-link")
	if err := os.Symlink(stateDir, stateAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	newConfig := func(statePath string) *config.Config {
		return &config.Config{
			VaultPath:     vault,
			Layout:        config.DefaultLayout,
			RootDirectory: config.DefaultRootDirectory,
			StateDBPath:   statePath,
		}
	}
	realConfig := newConfig(filepath.Join(stateDir, "state.db"))
	aliasConfig := newConfig(filepath.Join(stateAlias, "state.db"))
	for _, cfg := range []*config.Config{realConfig, aliasConfig} {
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if realConfig.StateDBPath != aliasConfig.StateDBPath {
		t.Fatalf("database identities differ: %q != %q", realConfig.StateDBPath, aliasConfig.StateDBPath)
	}
	if watchLockPath(realConfig) != watchLockPath(aliasConfig) {
		t.Fatalf("lock paths differ: %q != %q", watchLockPath(realConfig), watchLockPath(aliasConfig))
	}
	if watcher.NotificationPath(realConfig.StateDBPath) != watcher.NotificationPath(aliasConfig.StateDBPath) {
		t.Fatal("notification paths differ for the same physical database")
	}

	release, err := acquireWatchLock(realConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := acquireWatchLock(aliasConfig); !errors.Is(err, errWatchAlreadyRunning) {
		t.Fatalf("aliased database lock error = %v, want %v", err, errWatchAlreadyRunning)
	}
}

func TestWatchLockIsReleasedWhenOwnerProcessExits(t *testing.T) {
	cfg := testWatchLockConfig(t, "state.db", "vault")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWatchLockHelperProcess$")
	cmd.Env = append(
		os.Environ(),
		watchLockHelperEnv+"=1",
		watchLockHelperDBEnv+"="+cfg.StateDBPath,
		watchLockHelperVEnv+"="+cfg.VaultAbs,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	if scanner := bufio.NewScanner(stdout); !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("watch lock helper did not become ready: stderr=%q", stderr.String())
	}
	if _, err := acquireWatchLock(cfg); !errors.Is(err, errWatchAlreadyRunning) {
		t.Fatalf("lock held by helper error = %v, want %v", err, errWatchAlreadyRunning)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed watch lock helper exited successfully")
	}
	exited = true
	_ = stdin.Close()

	release, err := acquireWatchLock(cfg)
	if err != nil {
		t.Fatalf("acquire watch lock after owner process exit: %v", err)
	}
	release()
}

func TestRunWatchRejectsHeldLockBeforeOpeningDatabase(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDB := filepath.Join(root, "state", "state.db")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("MD2OBS_VAULT", vault)
	t.Setenv("MD2OBS_STATE_DB", stateDB)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireWatchLock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	code, stdout, stderr := captureRun(t, []string{"watch"})
	if code != 1 || stdout != "" {
		t.Fatalf("watch with held lock = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, errWatchAlreadyRunning.Error()) {
		t.Fatalf("held-lock stderr = %q", stderr)
	}
	if _, err := os.Stat(stateDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting watcher unexpectedly opened database: %v", err)
	}
}

func testWatchLockConfig(t *testing.T, databaseName, vaultName string) *config.Config {
	t.Helper()
	root := t.TempDir()
	return &config.Config{
		StateDBPath: filepath.Join(root, databaseName),
		VaultAbs:    filepath.Join(root, vaultName),
	}
}
