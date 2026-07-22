package layout

import (
	"path"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var testDate = time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)

func candidates(t *testing.T, in CandidateInput) []string {
	t.Helper()
	got, err := NewDatedFlatV1().CandidatePaths(in)
	if err != nil {
		t.Fatalf("CandidatePaths: %v", err)
	}
	return got
}

func TestCollisionProgression(t *testing.T) {
	got := candidates(t, CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/home/alice/project-b/README.md",
		Basename:      "README.md",
		ParentParts:   []string{"project-b", "alice", "home"},
		RootDirectory: "_External",
	})
	want := []string{
		"_External/2026-07-20/README.md",
		"_External/2026-07-20/README--project-b.md",
		"_External/2026-07-20/README--project-b--alice.md",
		"_External/2026-07-20/README--project-b--alice--home.md",
	}
	if len(got) != len(want)+1 {
		t.Fatalf("got %d candidates, want %d: %v", len(got), len(want)+1, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("candidate %d = %q, want %q", i, got[i], w)
		}
	}
	last := got[len(got)-1]
	if !strings.HasPrefix(last, "_External/2026-07-20/README--project-b--alice--home--") ||
		!strings.HasSuffix(last, ".md") {
		t.Errorf("hash fallback candidate = %q", last)
	}
}

func TestHashFallbackDeterministicAndDistinct(t *testing.T) {
	in := CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/a/x/README.md",
		Basename:      "README.md",
		ParentParts:   []string{"x", "a"},
		RootDirectory: "_External",
	}
	first := candidates(t, in)
	second := candidates(t, in)
	if first[len(first)-1] != second[len(second)-1] {
		t.Error("hash fallback is not deterministic")
	}
	in.SourcePath = "/b/x/README.md"
	other := candidates(t, in)
	if first[len(first)-1] == other[len(other)-1] {
		t.Error("hash fallback identical for different source paths")
	}
}

func TestMultiDotStemPreserved(t *testing.T) {
	got := candidates(t, CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/p/project/architecture.design.md",
		Basename:      "architecture.design.md",
		ParentParts:   []string{"project", "p"},
		RootDirectory: "_External",
	})
	if got[0] != "_External/2026-07-20/architecture.design.md" {
		t.Errorf("first candidate = %q", got[0])
	}
	if got[1] != "_External/2026-07-20/architecture.design--project.md" {
		t.Errorf("second candidate = %q", got[1])
	}
}

func TestUnicodePreserved(t *testing.T) {
	got := candidates(t, CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/home/alice/anteckningar/räksmörgås.md",
		Basename:      "räksmörgås.md",
		ParentParts:   []string{"anteckningar", "alice", "home"},
		RootDirectory: "_External",
	})
	if got[0] != "_External/2026-07-20/räksmörgås.md" {
		t.Errorf("unicode stem mangled: %q", got[0])
	}
}

func TestSanitization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pro/ject", "pro_ject"},
		{`pro\ject`, "pro_ject"},
		{"a:b", "a_b"},
		{`a<b>c"d|e?f*g`, "a_b_c_d_e_f_g"},
		{"trailing. ", "trailing"},
		{"ctrl\x01char", "ctrl_char"},
		{"...", ""},
	}
	for _, c := range cases {
		if got := sanitizeComponent(c.in); got != c.want {
			t.Errorf("sanitizeComponent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEmptySanitizedPartsSkipped(t *testing.T) {
	got := candidates(t, CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/x/.../README.md",
		Basename:      "README.md",
		ParentParts:   []string{"...", "x"},
		RootDirectory: "_External",
	})
	if got[1] != "_External/2026-07-20/README--x.md" {
		t.Errorf("empty part not skipped: %v", got)
	}
}

func TestHiddenNamesMadeVisible(t *testing.T) {
	got := candidates(t, CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/x/.hidden.md",
		Basename:      ".hidden.md",
		ParentParts:   []string{"x"},
		RootDirectory: "_External",
	})
	if got[0] != "_External/2026-07-20/_hidden.md" {
		t.Errorf("hidden name kept: %q", got[0])
	}
}

func TestContainment(t *testing.T) {
	got := candidates(t, CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/home/alice/../../etc/notes.md",
		Basename:      "notes.md",
		ParentParts:   []string{"..", "etc"},
		RootDirectory: "_External",
	})
	for _, c := range got {
		for _, part := range strings.Split(c, "/") {
			if part == ".." {
				t.Errorf("candidate %q contains traversal", c)
			}
		}
		if !strings.HasPrefix(c, "_External/2026-07-20/") {
			t.Errorf("candidate %q escapes the date folder", c)
		}
	}
}

func TestLongCandidatesStayWithinComponentLimit(t *testing.T) {
	stem := strings.Repeat("é", 130)
	got := candidates(t, CandidateInput{
		SnapshotDate:  testDate,
		SourcePath:    "/long/project/" + stem + ".md",
		Basename:      stem + ".md",
		ParentParts:   []string{strings.Repeat("parent", 50), "long"},
		RootDirectory: "_External",
	})
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	for _, candidate := range got {
		name := path.Base(candidate)
		if len(name) > maxFilenameBytes {
			t.Errorf("candidate is %d bytes: %q", len(name), name)
		}
		if !utf8.ValidString(name) {
			t.Errorf("candidate is not valid UTF-8: %q", name)
		}
	}
}

func TestNumberedSiblingStaysWithinFilenameLimit(t *testing.T) {
	name := strings.Repeat("é", 126) + ".md"
	if len(name) != maxFilenameBytes {
		t.Fatalf("test filename is %d bytes", len(name))
	}
	got, err := NumberedSibling(path.Join("_External/2026-07-20", name), 1)
	if err != nil {
		t.Fatal(err)
	}
	base := path.Base(got)
	if len(base) > maxFilenameBytes {
		t.Fatalf("numbered filename is %d bytes", len(base))
	}
	if !utf8.ValidString(base) {
		t.Fatalf("numbered filename is not valid UTF-8: %q", base)
	}
	if !strings.HasSuffix(base, "-1.md") {
		t.Fatalf("numbered filename = %q", base)
	}
}
