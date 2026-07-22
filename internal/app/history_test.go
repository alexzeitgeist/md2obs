package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHistoryAcceptsMissingSymlinkDisplayPath(t *testing.T) {
	env := newTestEnv(t)
	dir := t.TempDir()
	target := writeSource(t, dir, "target.md", "# target\n")
	display := filepath.Join(dir, "display.md")
	if err := os.Symlink(target, display); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ImportFile(context.Background(), env.deps, display, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if err := RunHistory(context.Background(), env.deps, display); err != nil {
		t.Fatalf("history for missing symlink source: %v", err)
	}
	if output := env.out.String(); !strings.Contains(output, "Source: "+display) || !strings.Contains(output, "2026-07-20") {
		t.Fatalf("history output missing source or snapshot:\n%s", output)
	}
}

func TestRunHistoryRejectsAmbiguousReusedDisplayPath(t *testing.T) {
	env := newTestEnv(t)
	dir := t.TempDir()
	targetA := writeSource(t, dir, "target-a.md", "# a\n")
	targetB := writeSource(t, dir, "target-b.md", "# b\n")
	display := filepath.Join(dir, "display.md")

	for _, target := range []string{targetA, targetB} {
		if err := os.Symlink(target, display); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := ImportFile(context.Background(), env.deps, display, PolicyOverwrite); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(display); err != nil {
			t.Fatal(err)
		}
	}

	err := RunHistory(context.Background(), env.deps, display)
	if err == nil || !strings.Contains(err.Error(), "path is ambiguous") {
		t.Fatalf("history error = %v, want ambiguity", err)
	}
	for _, identity := range []string{targetA, targetB} {
		if !strings.Contains(err.Error(), identity) {
			t.Fatalf("history error %q does not mention %q", err, identity)
		}
	}
}
