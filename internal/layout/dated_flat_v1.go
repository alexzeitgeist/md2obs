package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"unicode/utf8"
)

// maxFilenameBytes is the common NAME_MAX component limit on supported Unix
// filesystems. Layout v1 bounds every generated filename to this value.
const maxFilenameBytes = 255

// DatedFlatV1 materializes snapshots as <root>/YYYY-MM-DD/<name>, resolving
// filename collisions by appending progressively more parent-directory
// context and, as a last resort, a deterministic short hash of the source
// path:
//
//	README.md
//	README--project-b.md
//	README--project-b--alice.md
//	README--project-b--alice--home.md
//	README--project-b--alice--home--a1b2c3.md
type DatedFlatV1 struct{}

func NewDatedFlatV1() *DatedFlatV1 { return &DatedFlatV1{} }

func (*DatedFlatV1) Name() string { return "dated-flat-v1" }

func (*DatedFlatV1) Version() int { return 1 }

func (*DatedFlatV1) CandidatePaths(in CandidateInput) ([]string, error) {
	if in.Basename == "" {
		return nil, errors.New("layout: empty basename")
	}
	if in.RootDirectory == "" {
		return nil, errors.New("layout: empty root directory")
	}

	stem, suffix := splitName(in.Basename)
	stem = sanitizeComponent(stem)
	if stem == "" {
		stem = "unnamed"
	}

	var parts []string
	for _, p := range in.ParentParts {
		if s := sanitizeComponent(p); s != "" {
			parts = append(parts, s)
		}
	}

	var names []string
	seen := make(map[string]struct{})
	addName := func(candidateStem string) {
		name := boundedName(candidateStem, suffix, in.SourcePath)
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	addName(stem)
	acc := stem
	for _, p := range parts {
		acc += "--" + p
		addName(acc)
	}
	addName(acc + "--" + shortHash(in.SourcePath))

	dateDir := in.SnapshotDate.Format("2006-01-02")
	candidates := make([]string, len(names))
	for i, n := range names {
		candidates[i] = path.Join(in.RootDirectory, dateDir, n)
	}
	return candidates, nil
}

// splitName separates the final extension, preserving multi-dot stems:
// "architecture.design.md" -> ("architecture.design", ".md").
func splitName(basename string) (stem, suffix string) {
	suffix = path.Ext(basename)
	return strings.TrimSuffix(basename, suffix), suffix
}

// sanitizeComponent makes one path component filename-safe: path separators,
// control characters, and characters forbidden on common platforms become
// "_"; unsafe trailing spaces and periods are trimmed. Unicode is preserved.
// This policy is versioned with the layout — changing it means a new layout
// version.
func sanitizeComponent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " .")
}

// avoidHidden keeps generated files visible to Obsidian, which ignores
// dotfiles.
func avoidHidden(name string) string {
	if strings.HasPrefix(name, ".") {
		return "_" + strings.TrimPrefix(name, ".")
	}
	return name
}

// boundedName keeps a filename within the layout's byte budget. When context
// would make a candidate too long, it truncates on a UTF-8 boundary and keeps
// a source-path hash so the shortened name remains deterministic and useful
// for collision allocation.
func boundedName(stem, suffix, sourcePath string) string {
	stem = avoidHidden(stem)
	if len(stem)+len(suffix) <= maxFilenameBytes {
		return stem + suffix
	}

	marker := "--" + shortHash(sourcePath)
	budget := maxFilenameBytes - len(marker) - len(suffix)
	stem = strings.TrimRight(truncateUTF8(stem, budget), " .")
	if stem == "" {
		stem = "unnamed"
	}
	return stem + marker + suffix
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// shortHash is the deterministic collision fallback: the first 6 hex digits
// of the SHA-256 of the canonical source path.
func shortHash(sourcePath string) string {
	sum := sha256.Sum256([]byte(sourcePath))
	return hex.EncodeToString(sum[:])[:6]
}
