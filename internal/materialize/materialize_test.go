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
