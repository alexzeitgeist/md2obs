package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithinRoot(t *testing.T) {
	root := t.TempDir()
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	good := []string{"a.md", "a/b.md", "a/./b.md", "_External/2026-07-20/x.md"}
	for _, rel := range good {
		abs, err := WithinRoot(root, rel)
		if err != nil {
			t.Errorf("WithinRoot(%q) rejected: %v", rel, err)
			continue
		}
		if !strings.HasPrefix(abs, physicalRoot+string(filepath.Separator)) {
			t.Errorf("WithinRoot(%q) = %q outside root", rel, abs)
		}
	}

	bad := []string{"", ".", "..", "../x.md", "a/../../x.md", "/etc/passwd", "a/../.."}
	for _, rel := range bad {
		if _, err := WithinRoot(root, rel); err == nil {
			t.Errorf("WithinRoot(%q) accepted", rel)
		}
	}
}

func TestWithinRootRejectsSymlinkedAncestorOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := WithinRoot(root, "redirect/missing/note.md"); err == nil {
		t.Fatal("WithinRoot accepted a destination redirected outside by a symlink")
	}
}

func TestWithinRootAllowsSymlinkedAncestorInsideRoot(t *testing.T) {
	root := t.TempDir()
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(root, "redirect")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := WithinRoot(root, "redirect/note.md")
	if err != nil {
		t.Fatalf("WithinRoot rejected an internal symlink: %v", err)
	}
	if got != filepath.Join(physicalRoot, "redirect", "note.md") {
		t.Fatalf("destination = %q", got)
	}
}
