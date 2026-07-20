package watcher

import (
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndexExactPathFiltering(t *testing.T) {
	ix := NewIndex([]string{
		"/home/alex/project-a/README.md",
		"/home/alex/project-a/response.md",
		"/home/alex/project-b/notes.md",
	})

	if ix.Len() != 3 {
		t.Errorf("Len = %d", ix.Len())
	}
	want := []string{"/home/alex/project-a", "/home/alex/project-b"}
	if !reflect.DeepEqual(ix.Parents(), want) {
		t.Errorf("Parents = %v, want %v", ix.Parents(), want)
	}

	if _, ok := ix.Match("/home/alex/project-a/README.md"); !ok {
		t.Error("exact path did not match")
	}
	// Cleaning: an event path with a redundant segment still matches.
	if _, ok := ix.Match("/home/alex/project-a/./README.md"); !ok {
		t.Error("uncleaned event path did not match")
	}
	// Unrelated files in a watched directory never match.
	if _, ok := ix.Match("/home/alex/project-a/new.md"); ok {
		t.Error("unregistered file matched")
	}
	// Same basename in a nested (unwatched) directory never matches.
	if _, ok := ix.Match("/home/alex/project-a/docs/README.md"); ok {
		t.Error("nested file matched")
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
