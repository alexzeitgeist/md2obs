package materialize

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// WithinRoot joins a vault-relative path (forward slashes) to the absolute
// root and verifies the result stays strictly inside the root. Traversal
// components are rejected before joining; the cleaned result is re-checked
// as the final guard.
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
	joined := filepath.Join(filepath.Clean(rootAbs), filepath.FromSlash(cleaned))
	root := filepath.Clean(rootAbs)
	if joined == root || !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", fmt.Errorf("destination %q resolves outside the vault root", rel)
	}
	return joined, nil
}
