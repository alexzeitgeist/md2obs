package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"md2obs/internal/watcher"
)

func TestRunRefreshChangedAndUnchanged(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	changed := writeSource(t, dir, "changed.md", "# one\n")
	stable := writeSource(t, dir, "stable.md", "# stable\n")

	changedImport, err := ImportFile(ctx, env.deps, changed, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	stableImport, err := ImportFile(ctx, env.deps, stable, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	stableVault := filepath.Join(env.vault, filepath.FromSlash(stableImport.RelPath))
	if err := os.WriteFile(stableVault, []byte("# phone edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changed, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, OnVaultChange: PolicyOverwrite,
	})
	if err != nil {
		t.Fatalf("RunRefresh: %v", err)
	}
	if got := env.vaultFile(t, changedImport.RelPath); got != "# two\n" {
		t.Fatalf("changed vault content = %q", got)
	}
	if got := env.vaultFile(t, stableImport.RelPath); got != "# phone edit\n" {
		t.Fatalf("unchanged source evaluated overwrite policy: %q", got)
	}
	output := env.out.String()
	for _, want := range []string{
		"updated: " + changed,
		"Checked 2 sources: 1 refreshed, 0 conflicts skipped, 1 unchanged, 0 missing, 0 failed",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
	if data, err := os.ReadFile(watcher.NotificationPath(env.deps.DB.Path)); err != nil || len(data) == 0 {
		t.Fatalf("refresh notification = %q, err %v", data, err)
	}
}

func TestRunRefreshAllIncludesOlderMaterialization(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "older.md", "# old\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}

	env.setNow(time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))
	if err := os.WriteFile(src, []byte("# current\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{Days: 1, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# old\n" {
		t.Fatalf("one-day refresh selected older source: %q", got)
	}

	if err := RunRefresh(ctx, env.deps, RefreshOptions{All: true, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	if !vaultContainsContent(t, env.vault, "# current\n") {
		t.Fatalf("--all did not materialize current source; output:\n%s", env.out.String())
	}
	output := env.out.String()
	if !strings.Contains(output, "Checked 0 sources") || !strings.Contains(output, "Checked 1 sources: 1 refreshed") {
		t.Fatalf("refresh summaries do not show date-window behavior:\n%s", output)
	}
}

func TestRunRefreshDefaultsToSafeVaultConflictPolicy(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "conflict.md", "# original\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(first.RelPath))
	if err := os.WriteFile(vaultAbs, []byte("# phone edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("# revised\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RunRefresh(ctx, env.deps, RefreshOptions{Days: 1, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# phone edit\n" {
		t.Fatalf("refresh overwrote vault edit: %q", got)
	}
	output := env.out.String()
	if !strings.Contains(output, "skipped: "+src) || !strings.Contains(output, "1 conflicts skipped") {
		t.Fatalf("conflict was not reported:\n%s", output)
	}
}

func TestRunRefreshContinuesPastMissingAndIdentityFailures(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	missing := writeSource(t, dir, "a-missing.md", "# missing\n")
	retargeted := writeSource(t, dir, "b-retargeted.md", "# original\n")
	changed := writeSource(t, dir, "c-changed.md", "# one\n")

	if _, err := ImportFile(ctx, env.deps, missing, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFile(ctx, env.deps, retargeted, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	changedImport, err := ImportFile(ctx, env.deps, changed, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	other := writeSource(t, dir, "other.md", "# other\n")
	if err := os.Rename(retargeted, retargeted+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, retargeted); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(changed, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = RunRefresh(ctx, env.deps, RefreshOptions{Days: 1, OnVaultChange: PolicySkip})
	if err == nil || !strings.Contains(err.Error(), "1 of 3 refresh candidates failed") {
		t.Fatalf("RunRefresh error = %v", err)
	}
	if got := env.vaultFile(t, changedImport.RelPath); got != "# two\n" {
		t.Fatalf("later changed source was not refreshed: %q", got)
	}
	output := env.out.String()
	for _, want := range []string{
		"watch source identity changed",
		"Checked 3 sources: 1 refreshed, 0 conflicts skipped, 0 unchanged, 1 missing, 1 failed",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
}

func TestRefreshAllEnrollsOlderSourceInRunningWatcher(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "enroll.md", "# day one\n")
	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	env.setNow(time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))

	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(watchCtx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("watcher did not start empty; output:\n%s", env.out.String())
	}

	if err := os.WriteFile(src, []byte("# day two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{All: true, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	// This may race dynamic enrollment. Its activation check or the newly
	// armed directory watch must still converge the vault to the later content.
	if err := os.WriteFile(src, []byte("# after refresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool {
		return vaultContainsContent(t, env.vault, "# after refresh\n")
	}) {
		t.Fatalf("refreshed source was not enrolled; output:\n%s", env.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}
