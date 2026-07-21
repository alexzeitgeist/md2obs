package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"md2obs/internal/config"
	"md2obs/internal/database"
	"md2obs/internal/layout"
	"md2obs/internal/watcher"
)

func TestDateRange(t *testing.T) {
	now := time.Date(2026, 7, 20, 15, 0, 0, 0, time.Local)
	cases := []struct {
		days     int
		from, to string
	}{
		{1, "2026-07-20", "2026-07-20"},
		{2, "2026-07-19", "2026-07-20"},
		{3, "2026-07-18", "2026-07-20"},
	}
	for _, c := range cases {
		from, to := dateRange(now, c.days)
		if from != c.from || to != c.to {
			t.Errorf("dateRange(days=%d) = %s..%s, want %s..%s", c.days, from, to, c.from, c.to)
		}
	}
	// Month boundary.
	from, _ := dateRange(time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local), 2)
	if from != "2026-07-31" {
		t.Errorf("month boundary from = %s", from)
	}
}

func TestWatchNoSources(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: DefaultDebounce, OnVaultChange: PolicySkip,
		})
	}()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources from 0 directories")
	}) {
		cancel()
		<-done
		t.Fatalf("empty watcher did not stay running; output:\n%s", env.out.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchValidation(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	if err := RunWatch(ctx, env.deps, WatchOptions{Days: 0, Debounce: DefaultDebounce, OnVaultChange: PolicySkip}); err == nil {
		t.Error("--days 0 accepted")
	}
	if err := RunWatch(ctx, env.deps, WatchOptions{Days: 1, Debounce: 0, OnVaultChange: PolicySkip}); err == nil {
		t.Error("--debounce 0 accepted")
	}
	if err := RunWatch(ctx, env.deps, WatchOptions{Days: 1, Debounce: DefaultDebounce, OnVaultChange: "bogus"}); err == nil {
		t.Error("bogus policy accepted")
	}
}

// TestWatchEndToEnd exercises the full loop against real fsnotify events:
// startup is passive, atomic replacement and delete/recreate are imported,
// and unrelated or nested Markdown files are ignored.
func TestWatchEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	// The watcher runs against the real clock so today's date must match
	// the snapshot date used at import time.
	env.setNow(time.Now())
	dir := t.TempDir()
	src := writeSource(t, dir, "note.md", "# v1\n")
	ctx := context.Background()

	res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(res.RelPath))
	beforeWatch, err := os.Stat(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(watchCtx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()

	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 1 imported sources")
	}) {
		t.Fatalf("watcher did not start; output:\n%s", env.out.String())
	}
	// RunWatch prints its summary immediately before arming fsnotify. Give that
	// final setup a moment, then verify startup itself did not rewrite the copy.
	time.Sleep(200 * time.Millisecond)
	afterStartup, err := os.Stat(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStartup.ModTime().Equal(beforeWatch.ModTime()) {
		t.Fatalf("watch startup rewrote the vault copy: %s -> %s", beforeWatch.ModTime(), afterStartup.ModTime())
	}

	// Replace the source atomically, as editors commonly do. Repeating covers
	// any small watcher-arming race without switching to an in-place write.
	if !waitUntil(10*time.Second, func() bool {
		tmp := filepath.Join(dir, ".note-save.tmp")
		if err := os.WriteFile(tmp, []byte("# v2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, src); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
		data, err := os.ReadFile(vaultAbs)
		return err == nil && string(data) == "# v2\n"
	}) {
		t.Fatalf("watched change never imported; output:\n%s", env.out.String())
	}

	// Deletion alone is ignored, while recreating the exact registered path is
	// imported after debounce.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	writeSource(t, dir, "note.md", "# v3\n")
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(vaultAbs)
		return err == nil && string(data) == "# v3\n"
	}) {
		t.Fatalf("recreated source was not imported; output:\n%s", env.out.String())
	}

	// An unrelated Markdown file in the same watched directory must be
	// ignored: no new vault file may appear for it.
	writeSource(t, dir, "unrelated.md", "# nope\n")
	nestedDir := filepath.Join(dir, "nested")
	if err := os.Mkdir(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSource(t, nestedDir, "nested.md", "# nope\n")
	time.Sleep(500 * time.Millisecond)
	dateDir := filepath.Dir(vaultAbs)
	entries, err := os.ReadDir(dateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "unrelated") || strings.Contains(e.Name(), "nested") {
			t.Errorf("unregistered file was imported: %s", e.Name())
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchDaysIncludesOlderSnapshot(t *testing.T) {
	env := newTestEnv(t)
	env.setNow(time.Date(2026, 7, 19, 12, 0, 0, 0, time.Local))
	src := writeSource(t, t.TempDir(), "older.md", "# older\n")
	if _, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	env.setNow(time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 2, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Date range: 2026-07-19 through 2026-07-20")
	}) {
		cancel()
		<-done
		t.Fatalf("older source was not selected; output:\n%s", env.out.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWatchEnrollsImportAfterEmptyStartup(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("empty watcher did not start; output:\n%s", env.out.String())
	}

	importer := openWatchTestDeps(t, env.deps.Config, env.deps.Now)
	src := writeSource(t, t.TempDir(), "dynamic.md", "# v1\n")
	res, err := ImportFile(ctx, importer, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.NotifyImport(importer.DB.Path); err != nil {
		t.Fatal(err)
	}

	// This edit may race enrollment. Activation reconciliation or the newly
	// armed directory watch must still bring the vault to v2.
	if err := os.WriteFile(src, []byte("# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(res.RelPath))
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(vaultAbs)
		return err == nil && string(data) == "# v2\n"
	}) {
		t.Fatalf("dynamically enrolled source was not reconciled; output:\n%s", env.out.String())
	}

	if err := os.WriteFile(src, []byte("# v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(vaultAbs)
		return err == nil && string(data) == "# v3\n"
	}) {
		t.Fatalf("later change to dynamic source was not watched; output:\n%s", env.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchRapidImportsAllBecomeEnrolled(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("empty watcher did not start; output:\n%s", env.out.String())
	}

	importer := openWatchTestDeps(t, env.deps.Config, env.deps.Now)
	type importedSource struct {
		path     string
		vaultAbs string
		want     string
	}
	var imported []importedSource
	for i, name := range []string{"rapid-a.md", "rapid-b.md", "rapid-c.md"} {
		src := writeSource(t, t.TempDir(), name, "# initial\n")
		res, err := ImportFile(ctx, importer, src, PolicyOverwrite)
		if err != nil {
			t.Fatal(err)
		}
		if err := watcher.NotifyImport(importer.DB.Path); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("# changed-%d\n", i)
		if err := os.WriteFile(src, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		imported = append(imported, importedSource{
			path:     src,
			vaultAbs: filepath.Join(env.vault, filepath.FromSlash(res.RelPath)),
			want:     want,
		})
	}

	for _, item := range imported {
		if !waitUntil(5*time.Second, func() bool {
			data, err := os.ReadFile(item.vaultAbs)
			return err == nil && string(data) == item.want
		}) {
			t.Fatalf("rapid import %s was not enrolled; output:\n%s", item.path, env.out.String())
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchDynamicEnrollmentIsVaultScoped(t *testing.T) {
	envA := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, envA.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(envA.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("vault-a watcher did not start; output:\n%s", envA.out.String())
	}

	cfgB := &config.Config{
		VaultPath:     t.TempDir(),
		Layout:        config.DefaultLayout,
		RootDirectory: config.DefaultRootDirectory,
		StateDBPath:   envA.deps.Config.StateDBPath,
	}
	if err := cfgB.Validate(); err != nil {
		t.Fatal(err)
	}
	importerB := openWatchTestDeps(t, cfgB, envA.deps.Now)
	src := writeSource(t, t.TempDir(), "scoped.md", "# b1\n")
	if _, err := ImportFile(ctx, importerB, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	if err := watcher.NotifyImport(importerB.DB.Path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := os.WriteFile(src, []byte("# b2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if files := vaultMarkdownFiles(t, envA.vault); len(files) != 0 {
		t.Fatalf("vault-b import leaked into vault-a watcher: %v", files)
	}

	importerA := openWatchTestDeps(t, envA.deps.Config, envA.deps.Now)
	resA, err := ImportFile(ctx, importerA, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.NotifyImport(importerA.DB.Path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := os.WriteFile(src, []byte("# a3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vaultAPath := filepath.Join(envA.vault, filepath.FromSlash(resA.RelPath))
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(vaultAPath)
		return err == nil && string(data) == "# a3\n"
	}) {
		t.Fatalf("same-vault import was not enrolled; output:\n%s", envA.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchEnrollmentSurvivesMidnightNotification(t *testing.T) {
	env := newTestEnv(t)
	env.setNow(time.Date(2026, 7, 20, 23, 59, 59, 0, time.Local))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("watcher did not start; output:\n%s", env.out.String())
	}

	importer := openWatchTestDeps(t, env.deps.Config, env.deps.Now)
	src := writeSource(t, t.TempDir(), "midnight.md", "# day-one\n")
	if _, err := ImportFile(ctx, importer, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	if err := watcher.NotifyImport(importer.DB.Path); err != nil {
		t.Fatal(err)
	}
	// Move the watcher's discovery clock past midnight before its 50 ms
	// notification debounce expires.
	env.setNow(time.Date(2026, 7, 21, 0, 0, 0, 0, time.Local))
	time.Sleep(250 * time.Millisecond)
	if err := os.WriteFile(src, []byte("# day-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool {
		return vaultContainsContent(t, env.vault, "# day-two\n")
	}) {
		t.Fatalf("pre-midnight import was not enrolled after midnight; output:\n%s", env.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchUnchangedActivationLeavesVaultEditAlone(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicyOverwrite,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("watcher did not start; output:\n%s", env.out.String())
	}

	importer := openWatchTestDeps(t, env.deps.Config, env.deps.Now)
	sourceDir := t.TempDir()
	src := writeSource(t, sourceDir, "a-vault-edit.md", "# source\n")
	res, err := ImportFile(ctx, importer, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	var lastSeenBefore string
	if err := importer.DB.Query().QueryRowContext(ctx,
		`SELECT last_seen_at_utc FROM sources WHERE canonical_path = ?`, src,
	).Scan(&lastSeenBefore); err != nil {
		t.Fatal(err)
	}

	// A changed source sorted after the unchanged source serves as a positive
	// acknowledgement that the same membership refresh completed, avoiding a
	// fixed sleep for the negative assertions below.
	sentinel := writeSource(t, sourceDir, "z-sentinel.md", "# sentinel-one\n")
	sentinelRes, err := ImportFile(ctx, importer, sentinel, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(res.RelPath))
	if err := os.WriteFile(vaultAbs, []byte("# phone edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("# sentinel-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.setNow(time.Date(2026, 7, 20, 11, 0, 0, 0, time.Local))
	if err := watcher.NotifyImport(importer.DB.Path); err != nil {
		t.Fatal(err)
	}
	sentinelVault := filepath.Join(env.vault, filepath.FromSlash(sentinelRes.RelPath))
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(sentinelVault)
		return err == nil && string(data) == "# sentinel-two\n"
	}) {
		t.Fatalf("sentinel activation did not complete; output:\n%s", env.out.String())
	}
	data, err := os.ReadFile(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# phone edit\n" {
		t.Fatalf("unchanged activation evaluated overwrite policy: %q", data)
	}
	if strings.Contains(env.out.String(), "unchanged:") {
		t.Fatalf("unchanged activation produced import output:\n%s", env.out.String())
	}
	var lastSeenAfter string
	if err := importer.DB.Query().QueryRowContext(ctx,
		`SELECT last_seen_at_utc FROM sources WHERE canonical_path = ?`, src,
	).Scan(&lastSeenAfter); err != nil {
		t.Fatal(err)
	}
	if lastSeenAfter != lastSeenBefore {
		t.Fatalf("unchanged activation mutated source last_seen: %s -> %s", lastSeenBefore, lastSeenAfter)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchMissingActivationIsSilentAndRecreationWorks(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("watcher did not start; output:\n%s", env.out.String())
	}

	importer := openWatchTestDeps(t, env.deps.Config, env.deps.Now)
	sourceDir := t.TempDir()
	missing := writeSource(t, sourceDir, "a-missing.md", "# one\n")
	missingRes, err := ImportFile(ctx, importer, missing, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := writeSource(t, sourceDir, "z-sentinel.md", "# sentinel-one\n")
	sentinelRes, err := ImportFile(ctx, importer, sentinel, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("# sentinel-two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := watcher.NotifyImport(importer.DB.Path); err != nil {
		t.Fatal(err)
	}
	sentinelVault := filepath.Join(env.vault, filepath.FromSlash(sentinelRes.RelPath))
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(sentinelVault)
		return err == nil && string(data) == "# sentinel-two\n"
	}) {
		t.Fatalf("sentinel activation did not complete; output:\n%s", env.out.String())
	}
	if strings.Contains(env.out.String(), "cannot inspect newly watched source") {
		t.Fatalf("missing activation was logged as an error:\n%s", env.out.String())
	}

	if err := os.WriteFile(missing, []byte("# recreated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingVault := filepath.Join(env.vault, filepath.FromSlash(missingRes.RelPath))
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(missingVault)
		return err == nil && string(data) == "# recreated\n"
	}) {
		t.Fatalf("recreated missing source was not handled; output:\n%s", env.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchDynamicIdentityChangeStaysEnrolled(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("watcher did not start; output:\n%s", env.out.String())
	}

	importer := openWatchTestDeps(t, env.deps.Config, env.deps.Now)
	dir := t.TempDir()
	src := writeSource(t, dir, "identity.md", "# original\n")
	res, err := ImportFile(ctx, importer, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	other := writeSource(t, t.TempDir(), "other.md", "# replacement\n")
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, src); err != nil {
		t.Fatal(err)
	}
	if err := watcher.NotifyImport(importer.DB.Path); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "source identity changed")
	}) {
		t.Fatalf("dynamic identity change was not reported; output:\n%s", env.out.String())
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(res.RelPath))
	data, err := os.ReadFile(vaultAbs)
	if err != nil || string(data) != "# original\n" {
		t.Fatalf("replacement identity was imported: %q, err %v", data, err)
	}

	// Restoring the original canonical path must allow the retained enrollment
	// to process a later event.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("# restored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(vaultAbs)
		return err == nil && string(data) == "# restored\n"
	}) {
		t.Fatalf("restored dynamic identity was no longer watched; output:\n%s", env.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchActivationImportFailureKeepsSourceEnrolled(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 imported sources")
	}) {
		t.Fatalf("watcher did not start; output:\n%s", env.out.String())
	}

	importer := openWatchTestDeps(t, env.deps.Config, env.deps.Now)
	src := writeSource(t, t.TempDir(), "activation-error.md", "# one\n")
	res, err := ImportFile(ctx, importer, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Corrupt the recorded path just long enough to make the activation's
	// watched import fail its containment check after the hash gate.
	if _, err := importer.DB.Query().ExecContext(ctx,
		`UPDATE materializations SET relative_path = ? WHERE relative_path = ?`,
		"../activation-error.md", res.RelPath,
	); err != nil {
		t.Fatal(err)
	}
	if err := watcher.NotifyImport(importer.DB.Path); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "watch activation import failed")
	}) {
		t.Fatalf("activation import failure was not reported; output:\n%s", env.out.String())
	}

	if _, err := importer.DB.Query().ExecContext(ctx,
		`UPDATE materializations SET relative_path = ? WHERE relative_path = ?`,
		res.RelPath, "../activation-error.md",
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("# three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(res.RelPath))
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(vaultAbs)
		return err == nil && string(data) == "# three\n"
	}) {
		t.Fatalf("source was not retained after activation failure; output:\n%s", env.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func TestWatchCountsDatabaseDirectoryWhenItIsASourceParent(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, filepath.Dir(env.deps.DB.Path), "beside-db.md", "# source\n")
	if _, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(ctx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 1 imported sources from 1 directories")
	}) {
		cancel()
		<-done
		t.Fatalf("source directory count was wrong; output:\n%s", env.out.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}

func openWatchTestDeps(t *testing.T, cfg *config.Config, now func() time.Time) *Deps {
	t.Helper()
	db, err := database.Open(context.Background(), cfg.StateDBPath)
	if err != nil {
		t.Fatalf("open second database connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Deps{
		DB:     db,
		Config: cfg,
		Layout: layout.NewDatedFlatV1(),
		Now:    now,
		Out:    &syncBuffer{},
		Err:    &syncBuffer{},
	}
}

func vaultMarkdownFiles(t *testing.T, vault string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(vault, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func vaultContainsContent(t *testing.T, vault, want string) bool {
	t.Helper()
	for _, path := range vaultMarkdownFiles(t, vault) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) == want {
			return true
		}
	}
	return false
}

func waitUntil(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return condition()
}
