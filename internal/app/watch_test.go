package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	err := RunWatch(context.Background(), env.deps, WatchOptions{
		Days: 1, Debounce: DefaultDebounce, OnVaultChange: PolicySkip,
	})
	if err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
	if !strings.Contains(env.out.String(), "No imported sources found for 2026-07-20") {
		t.Errorf("output:\n%s", env.out.String())
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
