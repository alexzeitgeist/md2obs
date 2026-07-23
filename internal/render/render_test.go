package render

import (
	"bytes"
	"strings"
	"testing"
)

const (
	testPath = "/home/alice/projects/project-b/README.md"
	testTime = "2026-07-22T22:30:00Z"
)

func TestDisabledRenderingIsByteIdentical(t *testing.T) {
	for _, source := range [][]byte{
		nil,
		{},
		[]byte("# LF\n"),
		[]byte("# CRLF\r\n"),
		[]byte{0xef, 0xbb, 0xbf, '#', ' ', 'B', 'O', 'M', '\r', '\n'},
		[]byte("no final newline"),
	} {
		source := source
		t.Run(strings.ReplaceAll(string(source), "\n", `\n`), func(t *testing.T) {
			out, err := Render(Input{
				SourceContent: source,
				CanonicalPath: testPath,
				SnapshotTime:  testTime,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Content, source) {
				t.Fatalf("content = %q, want byte-identical %q", out.Content, source)
			}
			if out.Profile != SourceProfile {
				t.Fatalf("profile = %q", out.Profile)
			}
			if out.SHA256 != sha(source) {
				t.Fatalf("hash = %q, want %q", out.SHA256, sha(source))
			}
		})
	}
}

func TestProvenanceRenderingGoldens(t *testing.T) {
	headerLF := "---\n" +
		"md2obs_source_path: \"" + testPath + "\"\n" +
		"md2obs_imported_at: " + testTime + "\n" +
		"---\n"
	headerCRLF := strings.ReplaceAll(headerLF, "\n", "\r\n")

	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "new provenance block",
			source: "# Project B\n",
			want:   headerLF + "# Project B\n",
		},
		{
			name:   "empty source",
			source: "",
			want:   headerLF,
		},
		{
			name:   "missing final newline",
			source: "body",
			want:   headerLF + "body",
		},
		{
			name:   "existing YAML mapping and two-space indentation",
			source: "---\ntitle: Project B\nnested:\n    value: yes\n---\n# Body\n",
			want: "---\n" +
				"title: Project B\n" +
				"nested:\n" +
				"  value: yes\n" +
				"md2obs_source_path: \"" + testPath + "\"\n" +
				"md2obs_imported_at: " + testTime + "\n" +
				"---\n# Body\n",
		},
		{
			name:   "empty frontmatter",
			source: "---\n---\nbody\n",
			want:   headerLF + "body\n",
		},
		{
			name:   "comment-only frontmatter normalized as empty mapping",
			source: "---\n# note\n---\nbody\n",
			want:   headerLF + "body\n",
		},
		{
			name: "duplicate keys and all reserved occurrences",
			source: "---\n" +
				"tag: one\n" +
				"md2obs_source_path: old-a\n" +
				"tag: two\n" +
				"md2obs_imported_at: old-time\n" +
				"md2obs_source_path: old-b\n" +
				"---\nbody\n",
			want: "---\n" +
				"tag: one\n" +
				"tag: two\n" +
				"md2obs_source_path: \"" + testPath + "\"\n" +
				"md2obs_imported_at: " + testTime + "\n" +
				"---\nbody\n",
		},
		{
			name:   "non-mapping candidate retained as body",
			source: "---\n- one\n- two\n---\nbody\n",
			want:   headerLF + "---\n- one\n- two\n---\nbody\n",
		},
		{
			name:   "thematic-break scalar retained as body",
			source: "---\ntext\n---\nbody\n",
			want:   headerLF + "---\ntext\n---\nbody\n",
		},
		{
			name:   "malformed candidate retained as body",
			source: "---\nkey: [broken\n---\nbody\n",
			want:   headerLF + "---\nkey: [broken\n---\nbody\n",
		},
		{
			name:   "unclosed candidate retained as body",
			source: "---\nkey: value\nbody\n",
			want:   headerLF + "---\nkey: value\nbody\n",
		},
		{
			name:   "delimiter trailing whitespace is body",
			source: "--- \nkey: value\n---\nbody\n",
			want:   headerLF + "--- \nkey: value\n---\nbody\n",
		},
		{
			name:   "CRLF",
			source: "# Project B\r\nbody\r\n",
			want:   headerCRLF + "# Project B\r\nbody\r\n",
		},
		{
			name:   "existing CRLF frontmatter",
			source: "---\r\ntitle: Project B\r\n---\r\nbody\r\n",
			want: "---\r\n" +
				"title: Project B\r\n" +
				"md2obs_source_path: \"" + testPath + "\"\r\n" +
				"md2obs_imported_at: " + testTime + "\r\n" +
				"---\r\nbody\r\n",
		},
		{
			name:   "closing delimiter trailing whitespace is not a close",
			source: "---\nkey: value\n--- \nbody\n",
			want:   headerLF + "---\nkey: value\n--- \nbody\n",
		},
		{
			name:   "document-end marker is not a close",
			source: "---\nkey: value\n...\nbody\n",
			want:   headerLF + "---\nkey: value\n...\nbody\n",
		},
		{
			name:   "valid frontmatter followed by body delimiter",
			source: "---\ntitle: Test\n---\n---\nbody\n",
			want: "---\n" +
				"title: Test\n" +
				"md2obs_source_path: \"" + testPath + "\"\n" +
				"md2obs_imported_at: " + testTime + "\n" +
				"---\n---\nbody\n",
		},
		{
			name:   "JSON mapping",
			source: "---\n{\"title\":\"Project B\",\"tags\":[\"one\",\"two\"]}\n---\nbody\n",
			want: "---\n" +
				"{\"title\": \"Project B\", \"tags\": [\"one\", \"two\"], md2obs_source_path: \"" + testPath + "\", md2obs_imported_at: '" + testTime + "'}\n" +
				"---\nbody\n",
		},
		{
			name:   "YAML-sensitive Unicode path",
			source: "body\n",
			want: "---\n" +
				"md2obs_source_path: \"/home/alice/notes/colon: # 雪 \\\\ note.md\"\n" +
				"md2obs_imported_at: " + testTime + "\n" +
				"---\nbody\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := testPath
			if tc.name == "YAML-sensitive Unicode path" {
				path = "/home/alice/notes/colon: # 雪 \\ note.md"
			}
			out, err := Render(Input{
				SourceContent:  []byte(tc.source),
				CanonicalPath:  path,
				SnapshotTime:   testTime,
				WithProvenance: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(out.Content); got != tc.want {
				t.Fatalf("rendered bytes:\n%q\nwant:\n%q", got, tc.want)
			}
			if out.Profile != ProvenanceProfile {
				t.Fatalf("profile = %q", out.Profile)
			}
			if out.SHA256 != sha(out.Content) {
				t.Fatalf("hash = %q, want exact content hash %q", out.SHA256, sha(out.Content))
			}
		})
	}
}

func TestProvenancePreservesBOMExactlyOnce(t *testing.T) {
	source := append(append([]byte{}, bom...), []byte("# body\n")...)
	out, err := Render(Input{
		SourceContent:  source,
		CanonicalPath:  testPath,
		SnapshotTime:   testTime,
		WithProvenance: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out.Content, bom) {
		t.Fatal("BOM was not preserved at the beginning")
	}
	if got := bytes.Count(out.Content, bom); got != 1 {
		t.Fatalf("BOM count = %d, want 1", got)
	}
	if !bytes.HasSuffix(out.Content, []byte("# body\n")) {
		t.Fatalf("body was not preserved: %q", out.Content)
	}
}

func sha(content []byte) string {
	out, err := Render(Input{SourceContent: content})
	if err != nil {
		panic(err)
	}
	return out.SHA256
}
