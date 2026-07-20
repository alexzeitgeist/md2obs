package materialize

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"md2obs/internal/safepath"
)

// WithinRoot joins a vault-relative path (forward slashes) to the absolute
// root and verifies the result stays strictly inside the physical root.
// Traversal components are rejected before joining, then symlinks in the
// nearest existing ancestors are resolved so a directory symlink cannot
// redirect the write outside the vault.
func WithinRoot(rootAbs, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty vault-relative path")
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("vault-relative path %q is absolute", rel)
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("vault-relative path %q escapes the vault", rel)
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return "", fmt.Errorf("vault-relative path %q contains a traversal component", rel)
		}
	}
	root, err := safepath.ResolveExistingAncestor(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve vault root: %w", err)
	}
	joined := filepath.Join(root, filepath.FromSlash(cleaned))
	resolved, err := safepath.ResolveExistingAncestor(joined)
	if err != nil {
		return "", fmt.Errorf("resolve destination %q: %w", rel, err)
	}
	inside, err := safepath.Within(root, resolved, false)
	if err != nil {
		return "", err
	}
	if !inside {
		return "", fmt.Errorf("destination %q resolves outside the vault root", rel)
	}
	return joined, nil
}
