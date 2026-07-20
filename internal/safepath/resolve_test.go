package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExistingAncestorThroughSymlink(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveExistingAncestor(filepath.Join(link, "missing", "file.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(outside, "missing", "file.md")
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
}

func TestResolveExistingAncestorThroughDanglingSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "not-created", "state.db")
	link := filepath.Join(base, "state-link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := ResolveExistingAncestor(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("resolved path = %q, want %q", got, target)
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		target     string
		allowEqual bool
		want       bool
	}{
		{"child", filepath.Join(root, "a", "b.md"), false, true},
		{"equal rejected", root, false, false},
		{"equal allowed", root, true, true},
		{"sibling", root + "-other", false, false},
		{"parent", filepath.Dir(root), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Within(root, tc.target, tc.allowEqual)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Within() = %v, want %v", got, tc.want)
			}
		})
	}
}
