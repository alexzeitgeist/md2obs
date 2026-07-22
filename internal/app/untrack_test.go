package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/md2obs/internal/database"
	"github.com/alexzeitgeist/md2obs/internal/watcher"
)

func TestRunUntrackNamedForgetsBookkeepingAndReimportRegistersFresh(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "named.md", "# one\n")
	result, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	vaultPath := filepath.Join(env.vault, filepath.FromSlash(result.RelPath))
	if err := os.WriteFile(vaultPath, []byte("# manual vault edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{src}}); err != nil {
		t.Fatalf("RunUntrack: %v", err)
	}
	if !strings.Contains(env.out.String(), "untracked: "+src) {
		t.Fatalf("untrack result missing:\n%s", env.out.String())
	}
	if data, err := os.ReadFile(vaultPath); err != nil || string(data) != "# manual vault edit\n" {
		t.Fatalf("vault file changed: %q, err %v", data, err)
	}
	if sources, snapshots, materializations, err := env.deps.DB.Counts(ctx); err != nil || sources != 0 || snapshots != 0 || materializations != 0 {
		t.Fatalf("bookkeeping counts = %d sources/%d snapshots/%d materializations, err %v", sources, snapshots, materializations, err)
	}
	if got := trackedInVault(t, env, src); got {
		t.Fatal("named source remained tracked")
	}
	if err := os.WriteFile(src, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{All: true, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	if got := env.vaultFile(t, result.RelPath); got != "# manual vault edit\n" {
		t.Fatalf("refresh handled explicitly untracked source: %q", got)
	}
	if err := RunList(ctx, env.deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.out.String(), "No sources tracked in configured vault") {
		t.Fatalf("list retained forgotten source:\n%s", env.out.String())
	}

	second, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if second.RelPath == result.RelPath {
		t.Fatalf("same-day reimport reclaimed handed-off path %s", result.RelPath)
	}
	if data, err := os.ReadFile(vaultPath); err != nil || string(data) != "# manual vault edit\n" {
		t.Fatalf("same-day reimport changed handed-off file: %q, err %v", data, err)
	}
	if got := env.vaultFile(t, second.RelPath); got != "# two\n" {
		t.Fatalf("fresh registration content = %q", got)
	}
	if got := trackedInVault(t, env, src); !got {
		t.Fatal("explicit import did not register source again")
	}
}

func TestRunUntrackNamedAcceptsMissingDisplayPath(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	target := writeSource(t, dir, "target.md", "# target\n")
	display := filepath.Join(dir, "display.md")
	if err := os.Symlink(target, display); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ImportFile(ctx, env.deps, display, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if err := RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{display}}); err != nil {
		t.Fatalf("untrack missing display path: %v", err)
	}
	entries, err := database.FindTrackingEntriesByPath(ctx, env.deps.DB.Query(), env.vault, target, display)
	if err != nil || len(entries) != 0 {
		t.Fatalf("tracking entries = %+v, err %v", entries, err)
	}
}

func TestWatchedImportCannotReactivateExplicitlyUntrackedSource(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "race.md", "# one\n")
	result, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := database.GetSourceByPath(ctx, env.deps.DB.Query(), src)
	if err != nil || registered == nil {
		t.Fatalf("registered source = %+v, err %v", registered, err)
	}
	if err := RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ImportWatchedSource(ctx, env.deps, *registered, PolicySkip); !errors.Is(err, errSourceUntracked) {
		t.Fatalf("watched import error = %v, want explicit-untrack guard", err)
	}
	if trackedInVault(t, env, src) {
		t.Fatal("stale watched import reactivated source")
	}
	if got := env.vaultFile(t, result.RelPath); got != "# one\n" {
		t.Fatalf("stale watched import changed vault: %q", got)
	}
}

func TestRunUntrackMissingIsConservativeAndSupportsDryRun(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	present := writeSource(t, dir, "present.md", "# present\n")
	missing := writeSource(t, dir, "missing.md", "# missing\n")
	unavailableParent := filepath.Join(t.TempDir(), "offline")
	if err := os.Mkdir(unavailableParent, 0o755); err != nil {
		t.Fatal(err)
	}
	unavailable := writeSource(t, unavailableParent, "unavailable.md", "# unavailable\n")
	for _, path := range []string{present, missing, unavailable} {
		if _, err := ImportFile(ctx, env.deps, path, PolicyOverwrite); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(unavailableParent); err != nil {
		t.Fatal(err)
	}

	if err := RunUntrack(ctx, env.deps, UntrackOptions{Missing: true, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.out.String(), "would untrack: "+missing) || !strings.Contains(env.out.String(), "unavailable, still tracked: "+unavailable) {
		t.Fatalf("dry-run diagnostics missing:\n%s", env.out.String())
	}
	for _, path := range []string{present, missing, unavailable} {
		if !trackedInVault(t, env, path) {
			t.Fatalf("dry-run untracked %s", path)
		}
	}

	if err := RunUntrack(ctx, env.deps, UntrackOptions{Missing: true}); err != nil {
		t.Fatal(err)
	}
	if trackedInVault(t, env, missing) {
		t.Fatal("definitely missing source remained tracked")
	}
	if !trackedInVault(t, env, present) || !trackedInVault(t, env, unavailable) {
		t.Fatal("batch missing untracked a present or unavailable source")
	}
}

func TestRunUntrackDryRunDoesNotTakeWriteLock(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "dry-lock.md", "# one\n")
	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}

	other, err := database.Open(ctx, env.deps.DB.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value = value WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{src}, DryRun: true})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dry-run while writer held lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dry-run blocked behind an immediate write transaction")
	}
	if !trackedInVault(t, env, src) {
		t.Fatal("dry-run changed tracking")
	}
}

func TestRunUntrackLaterDayReimportCopiesForgottenContentAgain(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "later.md", "# same\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{src}}); err != nil {
		t.Fatal(err)
	}

	env.setNow(time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))
	second, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if first.RelPath == second.RelPath || !strings.Contains(second.RelPath, "/2026-07-21/") {
		t.Fatalf("later-day reimport paths = %q then %q", first.RelPath, second.RelPath)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# same\n" {
		t.Fatalf("handed-off first copy = %q", got)
	}
	if got := env.vaultFile(t, second.RelPath); got != "# same\n" {
		t.Fatalf("later-day copy = %q", got)
	}
}

func TestRunUntrackBatchFiltersIntersect(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	oldMissing := writeSource(t, dir, "old-missing.md", "# old missing\n")
	oldPresent := writeSource(t, dir, "old-present.md", "# old present\n")
	recentMissing := writeSource(t, dir, "recent-missing.md", "# recent missing\n")

	env.setNow(time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local))
	for _, path := range []string{oldMissing, oldPresent} {
		if _, err := ImportFile(ctx, env.deps, path, PolicyOverwrite); err != nil {
			t.Fatal(err)
		}
	}
	env.setNow(time.Date(2026, 7, 15, 10, 0, 0, 0, time.Local))
	if _, err := ImportFile(ctx, env.deps, recentMissing, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	env.setNow(time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local))
	if err := os.Remove(oldMissing); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(recentMissing); err != nil {
		t.Fatal(err)
	}

	if err := RunUntrack(ctx, env.deps, UntrackOptions{Missing: true, OlderThanDays: 30}); err != nil {
		t.Fatal(err)
	}
	if trackedInVault(t, env, oldMissing) {
		t.Fatal("old missing source did not satisfy combined filters")
	}
	if !trackedInVault(t, env, oldPresent) || !trackedInVault(t, env, recentMissing) {
		t.Fatal("combined filters did not use intersection semantics")
	}

	if err := RunUntrack(ctx, env.deps, UntrackOptions{OlderThanDays: 30}); err != nil {
		t.Fatal(err)
	}
	if trackedInVault(t, env, oldPresent) {
		t.Fatal("age-only batch did not untrack old present source")
	}
	if !trackedInVault(t, env, recentMissing) {
		t.Fatal("age-only batch untracked recent source")
	}
}

func TestRunUntrackMissingReportsIdentityChangeWithoutUntracking(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := writeSource(t, dir, "identity.md", "# original\n")
	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	other := writeSource(t, dir, "other.md", "# other\n")
	if err := os.Rename(src, src+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, src); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := RunUntrack(ctx, env.deps, UntrackOptions{Missing: true})
	if err == nil || !strings.Contains(err.Error(), "could not be checked") {
		t.Fatalf("RunUntrack error = %v", err)
	}
	if !strings.Contains(env.out.String(), "source identity changed") {
		t.Fatalf("identity change was not reported:\n%s", env.out.String())
	}
	if !trackedInVault(t, env, src) {
		t.Fatal("identity change untracked source")
	}
}

func TestRunUntrackNotifiesRunningWatcherMembership(t *testing.T) {
	env := newTestEnv(t)
	env.setNow(time.Now())
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "live.md", "# one\n")
	result, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	vaultPath := filepath.Join(env.vault, filepath.FromSlash(result.RelPath))

	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(watchCtx, env.deps, WatchOptions{
			Days: 1, Debounce: 25 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 1 source")
	}) {
		t.Fatalf("watcher did not start:\n%s", env.out.String())
	}

	if err := RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(watcher.NotificationPath(env.deps.DB.Path)); err != nil || len(data) == 0 {
		t.Fatalf("untrack notification = %q, err %v", data, err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := os.WriteFile(src, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	data, err := os.ReadFile(vaultPath)
	if err != nil || string(data) != "# one\n" {
		t.Fatalf("running watcher handled explicitly untracked source: %q, err %v", data, err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestRunUntrackNotificationFailureDoesNotUndoState(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "notify-warning.md", "# one\n")
	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	env.deps.DB.Path = filepath.Join(t.TempDir(), "missing", "state.db")

	if err := RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{src}}); err != nil {
		t.Fatalf("notification failure changed untrack result: %v", err)
	}
	if trackedInVault(t, env, src) {
		t.Fatal("notification failure rolled back untracking")
	}
	if !strings.Contains(env.out.String(), "warning: sources were untracked, but running watchers may need to be restarted") {
		t.Fatalf("notification warning missing:\n%s", env.out.String())
	}
}

func trackedInVault(t *testing.T, env *testEnv, canonicalPath string) bool {
	t.Helper()
	entries, err := database.FindTrackingEntriesByPath(
		context.Background(), env.deps.DB.Query(), env.vault, canonicalPath, canonicalPath,
	)
	if err != nil {
		t.Fatalf("tracking entry for %s = %+v, err %v", canonicalPath, entries, err)
	}
	return len(entries) > 0
}
