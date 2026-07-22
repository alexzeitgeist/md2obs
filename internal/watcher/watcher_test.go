package watcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndexExactPathFiltering(t *testing.T) {
	ix := NewIndex([]string{
		"/home/alice/project-a/README.md",
		"/home/alice/project-a/response.md",
		"/home/alice/project-b/notes.md",
	})

	if ix.Len() != 3 {
		t.Errorf("Len = %d", ix.Len())
	}
	want := []string{"/home/alice/project-a", "/home/alice/project-b"}
	if !reflect.DeepEqual(ix.Parents(), want) {
		t.Errorf("Parents = %v, want %v", ix.Parents(), want)
	}

	if !ix.Has("/home/alice/project-a/README.md") {
		t.Error("exact path did not match")
	}
	// Cleaning: an event path with a redundant segment still matches.
	if !ix.Has("/home/alice/project-a/./README.md") {
		t.Error("uncleaned event path did not match")
	}
	// Unrelated files in a watched directory never match.
	if ix.Has("/home/alice/project-a/new.md") {
		t.Error("unregistered file matched")
	}
	// Same basename in a nested (unwatched) directory never matches.
	if ix.Has("/home/alice/project-a/docs/README.md") {
		t.Error("nested file matched")
	}
}

func TestIndexDynamicAdditionIsIdempotent(t *testing.T) {
	ix := NewIndex(nil)
	if !ix.Add("/home/alice/project-a/one.md") {
		t.Fatal("first addition was not reported as new")
	}
	if ix.Add("/home/alice/project-a/./one.md") {
		t.Fatal("cleaned duplicate was reported as new")
	}
	if !ix.Add("/home/alice/project-a/two.md") {
		t.Fatal("second source in existing parent was not added")
	}
	if !ix.Add("/home/alice/project-b/three.md") {
		t.Fatal("source in new parent was not added")
	}
	if ix.Len() != 3 {
		t.Fatalf("Len = %d, want 3", ix.Len())
	}
	wantParents := []string{"/home/alice/project-a", "/home/alice/project-b"}
	if !reflect.DeepEqual(ix.Parents(), wantParents) {
		t.Errorf("Parents = %v, want %v", ix.Parents(), wantParents)
	}
	if ix.Has("/home/alice/project-a/unrelated.md") {
		t.Error("dynamic parent caused unrelated path to match")
	}
	if !ix.Remove("/home/alice/project-a/two.md") {
		t.Fatal("existing source was not removed")
	}
	wantPaths := []string{"/home/alice/project-a/one.md", "/home/alice/project-b/three.md"}
	if got := ix.Paths(); !reflect.DeepEqual(got, wantPaths) {
		t.Fatalf("Paths after removal = %v, want %v", got, wantPaths)
	}
	if !ix.HasParent("/home/alice/project-a") {
		t.Fatal("parent with a remaining source was removed")
	}
	if !ix.Remove("/home/alice/project-a/one.md") {
		t.Fatal("last source in parent was not removed")
	}
	wantParents = []string{"/home/alice/project-b"}
	if got := ix.Parents(); !reflect.DeepEqual(got, wantParents) {
		t.Fatalf("Parents after last source removal = %v, want %v", got, wantParents)
	}
	if ix.HasParent("/home/alice/project-a") {
		t.Fatal("parent without sources remained indexed")
	}
	if !ix.Add("/home/alice/project-a/readded.md") {
		t.Fatal("source in released parent was not re-added")
	}
	wantParents = []string{"/home/alice/project-a", "/home/alice/project-b"}
	if got := ix.Parents(); !reflect.DeepEqual(got, wantParents) {
		t.Fatalf("Parents after re-addition = %v, want %v", got, wantParents)
	}
}

type recordingWatchRemover struct {
	paths []string
}

func (r *recordingWatchRemover) Remove(path string) error {
	r.paths = append(r.paths, path)
	return nil
}

func TestRemoveWatchedSourceReleasesOnlyUnusedSourceParent(t *testing.T) {
	const (
		sourceParent       = "/home/alice/project"
		notificationParent = "/home/alice/state"
	)
	one := filepath.Join(sourceParent, "one.md")
	two := filepath.Join(sourceParent, "two.md")
	notificationSibling := filepath.Join(notificationParent, "source.md")
	ix := NewIndex([]string{one, two, notificationSibling})
	remover := &recordingWatchRemover{}
	removedSources := make([]string, 0, 3)
	remove := func(path string) { removedSources = append(removedSources, path) }

	removeWatchedSource(remover, ix, notificationParent, one, remove, discardLogger())
	if len(remover.paths) != 0 {
		t.Fatalf("shared parent released with one source remaining: %v", remover.paths)
	}
	if !ix.HasParent(sourceParent) {
		t.Fatal("shared parent was pruned with one source remaining")
	}

	removeWatchedSource(remover, ix, notificationParent, two, remove, discardLogger())
	if want := []string{sourceParent}; !reflect.DeepEqual(remover.paths, want) {
		t.Fatalf("released parents = %v, want %v", remover.paths, want)
	}
	if ix.HasParent(sourceParent) {
		t.Fatal("unused source parent remained indexed")
	}

	removeWatchedSource(remover, ix, notificationParent, notificationSibling, remove, discardLogger())
	if want := []string{sourceParent}; !reflect.DeepEqual(remover.paths, want) {
		t.Fatalf("notification parent watch was released: %v", remover.paths)
	}
	if ix.HasParent(notificationParent) {
		t.Fatal("notification parent remained in the source index")
	}
	if want := []string{one, two, notificationSibling}; !reflect.DeepEqual(removedSources, want) {
		t.Fatalf("removed source callbacks = %v, want %v", removedSources, want)
	}
}

func TestSourceParentRetryDelayIsBounded(t *testing.T) {
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		3200 * time.Millisecond,
	}
	for attempt, wantDelay := range want {
		delay, retry := sourceParentRetryDelay(attempt)
		if !retry || delay != wantDelay {
			t.Fatalf("attempt %d = (%s, %t), want (%s, true)", attempt, delay, retry, wantDelay)
		}
	}
	for _, attempt := range []int{-1, len(want)} {
		if delay, retry := sourceParentRetryDelay(attempt); retry || delay != 0 {
			t.Fatalf("attempt %d = (%s, %t), want (0, false)", attempt, delay, retry)
		}
	}
}

func TestDebouncerCoalescesBursts(t *testing.T) {
	d := NewDebouncer(50 * time.Millisecond)
	defer d.Stop()

	var fired atomic.Int32
	go func() {
		for range d.C {
			fired.Add(1)
		}
	}()

	p := filepath.Join("/tmp", "x.md")
	for range 5 {
		d.Trigger(p)
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := fired.Load(); got != 1 {
		t.Errorf("burst fired %d times, want 1", got)
	}

	d.Trigger(p)
	time.Sleep(200 * time.Millisecond)
	if got := fired.Load(); got != 2 {
		t.Errorf("second event fired %d times total, want 2", got)
	}
}

func TestDebouncerPerSourceTimers(t *testing.T) {
	d := NewDebouncer(50 * time.Millisecond)
	defer d.Stop()

	got := make(chan string, 4)
	go func() {
		for p := range d.C {
			got <- p
		}
	}()

	d.Trigger("/a/x.md")
	d.Trigger("/b/y.md")

	seen := map[string]bool{}
	for range 2 {
		select {
		case p := <-got:
			seen[p] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; seen %v", seen)
		}
	}
	if !seen["/a/x.md"] || !seen["/b/y.md"] {
		t.Errorf("seen = %v", seen)
	}
}

func TestDebouncerStopCancelsPending(t *testing.T) {
	d := NewDebouncer(50 * time.Millisecond)
	d.Trigger("/a/x.md")
	d.Stop()
	select {
	case p := <-d.C:
		t.Errorf("fired %q after Stop", p)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestDebouncerCancelPath(t *testing.T) {
	d := NewDebouncer(25 * time.Millisecond)
	defer d.Stop()
	d.Trigger("/a/x.md")
	d.Cancel("/a/x.md")
	select {
	case path := <-d.C:
		t.Fatalf("cancelled path fired: %s", path)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestDebouncerIgnoresStaleTimer(t *testing.T) {
	d := NewDebouncer(time.Hour)
	defer d.Stop()

	const path = "/a/x.md"
	d.Trigger(path)
	d.mu.Lock()
	stale := d.timers[path]
	d.mu.Unlock()
	d.Trigger(path)

	// This models an expired AfterFunc callback that was already queued when a
	// newer event replaced the source's timer entry.
	d.fire(path, stale)
	select {
	case got := <-d.C:
		t.Fatalf("stale timer fired for %q", got)
	default:
	}
}

func TestRunDynamicallyEnrollsNewSource(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	initialDir := t.TempDir()
	dynamicDir := t.TempDir()
	initial := filepath.Join(initialDir, "initial.md")
	dynamic := filepath.Join(dynamicDir, "dynamic.md")
	if err := os.WriteFile(initial, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dynamic, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}

	var includeDynamic atomic.Bool
	ready := make(chan Stats, 1)
	activated := make(chan string, 1)
	handled := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			NotificationPath: NotificationPath(databasePath),
			SourceDebounce:   10 * time.Millisecond,
			RefreshDebounce:  10 * time.Millisecond,
			Load: func() ([]string, error) {
				if includeDynamic.Load() {
					return []string{initial, dynamic}, nil
				}
				return []string{initial}, nil
			},
			Activate: func(path string) { activated <- path },
			Handle:   func(path string) { handled <- path },
			Ready:    func(stats Stats) { ready <- stats },
		}, discardLogger())
	}()
	defer cancel()

	select {
	case stats := <-ready:
		if stats.Sources != 1 || stats.Directories != 1 {
			t.Fatalf("startup stats = %+v", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}
	includeDynamic.Store(true)
	if err := NotifyImport(databasePath); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-activated:
		if path != dynamic {
			t.Fatalf("activated %q, want %q", path, dynamic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dynamic source was not activated")
	}
	if err := os.WriteFile(dynamic, []byte("three"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-handled:
		if path != dynamic {
			t.Fatalf("handled %q, want %q", path, dynamic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dynamic source change was not handled")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunRefreshRemovesUntrackedSourceAndCancelsPendingEvent(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	sourcePath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(sourcePath, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	var include atomic.Bool
	include.Store(true)
	ready := make(chan Stats, 1)
	removed := make(chan string, 1)
	handled := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			NotificationPath: NotificationPath(databasePath),
			SourceDebounce:   200 * time.Millisecond,
			RefreshDebounce:  10 * time.Millisecond,
			Load: func() ([]string, error) {
				if include.Load() {
					return []string{sourcePath}, nil
				}
				return nil, nil
			},
			Handle: func(path string) { handled <- path },
			Remove: func(path string) { removed <- path },
			Ready:  func(stats Stats) { ready <- stats },
		}, discardLogger())
	}()
	defer cancel()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}

	if err := os.WriteFile(sourcePath, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	include.Store(false)
	if err := NotifyImport(databasePath); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-removed:
		if path != sourcePath {
			t.Fatalf("removed %q, want %q", path, sourcePath)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("source was not removed during membership refresh")
	}
	select {
	case path := <-handled:
		t.Fatalf("pending event handled after explicit removal: %s", path)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunRearmsRecreatedSourceDirectoryWithoutNotification(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	sourceParent := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(sourceParent, 0o755); err != nil {
		t.Fatal(err)
	}
	initial := filepath.Join(sourceParent, "initial.md")
	if err := os.WriteFile(initial, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}

	ready := make(chan Stats, 1)
	activated := make(chan string, 2)
	handledContent := make(chan string, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			NotificationPath: NotificationPath(databasePath),
			SourceDebounce:   10 * time.Millisecond,
			RefreshDebounce:  10 * time.Millisecond,
			Load:             func() ([]string, error) { return []string{initial}, nil },
			Activate:         func(path string) { activated <- path },
			Handle: func(path string) {
				content, err := os.ReadFile(path)
				if err == nil {
					handledContent <- string(content)
				}
			},
			Ready: func(stats Stats) { ready <- stats },
		}, discardLogger())
	}()
	defer cancel()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}

	if err := os.RemoveAll(sourceParent); err != nil {
		t.Fatal(err)
	}
	// Leave the desired parent absent long enough for the watch-death refresh
	// and at least one recovery attempt to fail. No membership notification is
	// sent before or after recreation.
	time.Sleep(250 * time.Millisecond)
	if err := os.Mkdir(sourceParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initial, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-activated:
		if path != initial {
			t.Fatalf("activated %q, want %q", path, initial)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("source in recreated directory was not activated")
	}

	// Activation catches content recreated before the watch was restored. A
	// later write proves that recovery also restored the native watch.
	if err := os.WriteFile(initial, []byte("three"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case content := <-handledContent:
		if content != "three" {
			t.Fatalf("handled content = %q, want %q", content, "three")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recreated source directory was not re-armed")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunRetriesMembershipRefresh(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	sourceDir := t.TempDir()
	dynamic := filepath.Join(sourceDir, "dynamic.md")
	if err := os.WriteFile(dynamic, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	firstFailure := make(chan struct{}, 1)
	ready := make(chan Stats, 1)
	activated := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			NotificationPath: NotificationPath(databasePath),
			SourceDebounce:   10 * time.Millisecond,
			RefreshDebounce:  10 * time.Millisecond,
			Load: func() ([]string, error) {
				call := calls.Add(1)
				if call == 1 {
					return nil, nil
				}
				if call == 2 {
					firstFailure <- struct{}{}
				}
				if call < 4 {
					return nil, errors.New("transient load failure")
				}
				return []string{dynamic}, nil
			},
			Activate: func(path string) { activated <- path },
			Handle:   func(string) {},
			Ready:    func(stats Stats) { ready <- stats },
		}, discardLogger())
	}()
	defer cancel()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}
	if err := NotifyImport(databasePath); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstFailure:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh failure was not observed")
	}
	// This notification lands during the 100 ms retry backoff. It must be
	// covered by the pending retry rather than starting a parallel refresh.
	if err := NotifyImport(databasePath); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-activated:
		if path != dynamic {
			t.Fatalf("activated %q, want %q", path, dynamic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not succeed after retries")
	}
	if calls.Load() != 4 {
		t.Fatalf("load calls = %d, want 4", calls.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunStopsAfterThreeRefreshFailures(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	var calls atomic.Int32
	persistentErr := errors.New("persistent load failure")
	ready := make(chan Stats, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			NotificationPath: NotificationPath(databasePath),
			SourceDebounce:   10 * time.Millisecond,
			RefreshDebounce:  10 * time.Millisecond,
			Load: func() ([]string, error) {
				if calls.Add(1) == 1 {
					return nil, nil
				}
				return nil, persistentErr
			},
			Handle: func(string) {},
			Ready:  func(stats Stats) { ready <- stats },
		}, discardLogger())
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}
	if err := NotifyImport(databasePath); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, persistentErr) || calls.Load() != 4 {
			t.Fatalf("Run error = %v, load calls = %d", err, calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop after persistent refresh failures")
	}
}

func TestRunFailsWhenNotificationDirectoryCannotBeWatched(t *testing.T) {
	root := t.TempDir()
	err := Run(context.Background(), Options{
		NotificationPath: filepath.Join(root, "missing", "state.db.watch-notify"),
		SourceDebounce:   10 * time.Millisecond,
		RefreshDebounce:  10 * time.Millisecond,
		Load:             func() ([]string, error) { return nil, nil },
		Handle:           func(string) {},
	}, discardLogger())
	if err == nil {
		t.Fatal("missing notification directory was accepted")
	}
}

func TestRunFailsWhenNotificationDirectoryIsRenamed(t *testing.T) {
	root := t.TempDir()
	notificationParent := filepath.Join(root, "state")
	if err := os.Mkdir(notificationParent, 0o755); err != nil {
		t.Fatal(err)
	}
	ready := make(chan Stats, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			NotificationPath: filepath.Join(notificationParent, "state.db.watch-notify"),
			SourceDebounce:   10 * time.Millisecond,
			RefreshDebounce:  10 * time.Millisecond,
			Load:             func() ([]string, error) { return nil, nil },
			Handle:           func(string) {},
			Ready:            func(stats Stats) { ready <- stats },
		}, discardLogger())
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}

	if err := os.Rename(notificationParent, notificationParent+"-moved"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "notification directory") || !strings.Contains(err.Error(), "restart the watcher") {
			t.Fatalf("Run error = %v, want notification-directory restart error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop after notification directory was renamed")
	}
}

func TestRunRetriesSourceWhoseParentBecomesAvailable(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.db")
	missingParent := filepath.Join(t.TempDir(), "later")
	sourcePath := filepath.Join(missingParent, "later.md")
	ready := make(chan Stats, 1)
	activated := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			NotificationPath: NotificationPath(databasePath),
			SourceDebounce:   10 * time.Millisecond,
			RefreshDebounce:  10 * time.Millisecond,
			Load:             func() ([]string, error) { return []string{sourcePath}, nil },
			Activate:         func(path string) { activated <- path },
			Handle:           func(string) {},
			Ready:            func(stats Stats) { ready <- stats },
		}, discardLogger())
	}()
	defer cancel()
	select {
	case stats := <-ready:
		if stats.Sources != 0 || stats.Directories != 0 {
			t.Fatalf("unwatchable startup source was counted: %+v", stats)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}
	if err := os.Mkdir(missingParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("available"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-activated:
		if path != sourcePath {
			t.Fatalf("activated %q, want %q", path, sourcePath)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("source was not retried automatically after its parent became available")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
