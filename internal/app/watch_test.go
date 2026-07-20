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
// a watched source change is re-imported after debounce, and an unrelated
// Markdown file in the same directory is ignored.
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

	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(watchCtx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()

	// Wait for the watch to be armed, then repeatedly touch the source
	// until the change lands (retrying covers the startup race).
	deadline := time.Now().Add(10 * time.Second)
	updated := false
	for time.Now().Before(deadline) {
		writeSource(t, dir, "note.md", "# v2\n")
		time.Sleep(200 * time.Millisecond)
		if data, err := os.ReadFile(vaultAbs); err == nil && string(data) == "# v2\n" {
			updated = true
			break
		}
	}
	if !updated {
		cancel()
		<-done
		t.Fatalf("watched change never imported; output:\n%s", env.out.String())
	}

	// An unrelated Markdown file in the same watched directory must be
	// ignored: no new vault file may appear for it.
	writeSource(t, dir, "unrelated.md", "# nope\n")
	time.Sleep(500 * time.Millisecond)
	dateDir := filepath.Dir(vaultAbs)
	entries, err := os.ReadDir(dateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "unrelated") {
			t.Errorf("unrelated file was imported: %s", e.Name())
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}
