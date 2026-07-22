package source

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParentParts(t *testing.T) {
	got := ParentParts("/home/alice/project-b/README.md")
	want := []string{"project-b", "alice", "home"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParentParts = %v, want %v", got, want)
	}
	if got := ParentParts("/README.md"); len(got) != 0 {
		t.Errorf("ParentParts at root = %v", got)
	}
}

func TestCanonicalizeResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.md")
	if err := os.WriteFile(real, []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	canonical, display, err := Canonicalize(link)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	realResolved, _ := filepath.EvalSymlinks(real)
	if canonical != realResolved {
		t.Errorf("canonical = %q, want %q", canonical, realResolved)
	}
	if display != link {
		t.Errorf("display = %q, want %q", display, link)
	}
}

func TestCanonicalizeMissing(t *testing.T) {
	if _, _, err := Canonicalize(filepath.Join(t.TempDir(), "nope.md")); err == nil {
		t.Error("Canonicalize accepted a missing file")
	}
}

func TestInspect(t *testing.T) {
	dir := t.TempDir()
	md := filepath.Join(dir, "note.md")
	if err := os.WriteFile(md, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := Inspect(md)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ByteSize != 4 || info.MtimeNS == 0 {
		t.Errorf("Info = %+v", info)
	}

	txt := filepath.Join(dir, "note.txt")
	os.WriteFile(txt, []byte("x"), 0o644)
	if _, err := Inspect(txt); err == nil {
		t.Error("Inspect accepted .txt")
	}

	mdDir := filepath.Join(dir, "dir.md")
	os.Mkdir(mdDir, 0o755)
	if _, err := Inspect(mdDir); err == nil {
		t.Error("Inspect accepted a directory")
	}
}

func TestReadAndHash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, sha, err := ReadAndHash(p)
	if err != nil {
		t.Fatalf("ReadAndHash: %v", err)
	}
	if string(content) != "hello\n" {
		t.Errorf("content = %q", content)
	}
	if sha != HashBytes([]byte("hello\n")) || len(sha) != 64 {
		t.Errorf("sha = %q", sha)
	}
}
