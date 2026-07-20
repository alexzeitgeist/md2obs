package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"
)

// DatedFlatV1 materializes snapshots as <root>/YYYY-MM-DD/<name>, resolving
// filename collisions by appending progressively more parent-directory
// context and, as a last resort, a deterministic short hash of the source
// path:
//
//	README.md
//	README--project-b.md
//	README--project-b--alex.md
//	README--project-b--alex--home.md
//	README--project-b--alex--home--a1b2c3.md
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

	names := []string{stem + suffix}
	acc := stem
	for _, p := range parts {
		acc += "--" + p
		names = append(names, acc+suffix)
	}
	names = append(names, acc+"--"+shortHash(in.SourcePath)+suffix)

	dateDir := in.SnapshotDate.Format("2006-01-02")
	candidates := make([]string, len(names))
	for i, n := range names {
		candidates[i] = path.Join(in.RootDirectory, dateDir, avoidHidden(n))
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

// shortHash is the deterministic collision fallback: the first 6 hex digits
// of the SHA-256 of the canonical source path.
func shortHash(sourcePath string) string {
	sum := sha256.Sum256([]byte(sourcePath))
	return hex.EncodeToString(sum[:])[:6]
}
