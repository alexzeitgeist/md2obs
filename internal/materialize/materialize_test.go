package materialize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomic(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "sub", "note.md")

	if err := WriteAtomic(dest, []byte("one"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "one" {
		t.Errorf("content = %q", got)
	}

	if err := WriteAtomic(dest, []byte("two"), 0o644); err != nil {
		t.Fatalf("WriteAtomic replace: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "two" {
		t.Errorf("replaced content = %q", got)
	}

	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".md2obs-tmp-") {
			t.Errorf("leftover temporary file %s", e.Name())
		}
	}
}

func TestWriteAtomicCleansTempAfterReplaceFailure(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "note.md")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(dest, []byte("content"), 0o644); err == nil {
		t.Fatal("WriteAtomic unexpectedly replaced a directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".md2obs-tmp-") {
			t.Fatalf("left temporary file %s after failed replace", entry.Name())
		}
	}
}

func TestWithinRoot(t *testing.T) {
	root := t.TempDir()

	good := []string{"a.md", "a/b.md", "a/./b.md", "_External/2026-07-20/x.md"}
	for _, rel := range good {
		abs, err := WithinRoot(root, rel)
		if err != nil {
			t.Errorf("WithinRoot(%q) rejected: %v", rel, err)
			continue
		}
		if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
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
	if got != filepath.Join(root, "redirect", "note.md") {
		t.Fatalf("destination = %q", got)
	}
}
