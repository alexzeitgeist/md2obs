// Package safepath resolves paths through existing filesystem ancestors and
// compares their physical locations.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveExistingAncestor resolves every symlink in the existing portion of
// p. Components below the nearest existing ancestor are appended unchanged.
// This permits checking a destination before its final directories or file
// have been created.
func ResolveExistingAncestor(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}

	current := filepath.Clean(abs)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return appendMissing(resolved, missing), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve %s: %w", p, err)
		}
		// EvalSymlinks reports not-exist for a dangling symlink even though
		// the link itself exists. Resolve its target explicitly so a database
		// file symlink cannot hide a not-yet-created target inside the vault.
		if info, lstatErr := os.Lstat(current); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return "", fmt.Errorf("read symlink %s: %w", current, err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(current), target)
			}
			resolved, err := ResolveExistingAncestor(target)
			if err != nil {
				return "", err
			}
			return appendMissing(resolved, missing), nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve %s: no existing ancestor", p)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func appendMissing(base string, missing []string) string {
	for i := len(missing) - 1; i >= 0; i-- {
		base = filepath.Join(base, missing[i])
	}
	return filepath.Clean(base)
}

// Within reports whether target is physically below root. Both paths should
// already have had their existing ancestors resolved.
func Within(root, target string, allowEqual bool) (bool, error) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false, fmt.Errorf("compare %s with %s: %w", target, root, err)
	}
	if rel == "." {
		return allowEqual, nil
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}
