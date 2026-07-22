// Package source resolves, validates, and hashes original Markdown files.
package source

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ErrSourceIdentityChanged reports that a registered canonical path no longer
// resolves to itself, e.g. because a symlink was retargeted.
var ErrSourceIdentityChanged = errors.New("source identity changed")

// Canonicalize resolves a user-supplied path to its canonical absolute form
// (symlinks resolved), which is the source identity, plus a display path
// (absolute, symlinks preserved) for output and the database display column.
func Canonicalize(p string) (canonical, display string, err error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", p, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", abs, err)
	}
	return resolved, abs, nil
}

// VerifyRegisteredIdentity re-resolves a registered canonical path and errors
// when it no longer resolves to itself. Resolution failures are returned as-is
// (os.ErrNotExist stays detectable); a mismatch wraps ErrSourceIdentityChanged.
func VerifyRegisteredIdentity(registered string) error {
	canonical, _, err := Canonicalize(registered)
	if err != nil {
		return err
	}
	if canonical != filepath.Clean(registered) {
		return fmt.Errorf("%w: registered %s now resolves to %s", ErrSourceIdentityChanged, registered, canonical)
	}
	return nil
}

// ParentParts returns the parent directory names of a canonical path,
// nearest first: /home/alice/project-b/README.md -> [project-b alice home].
func ParentParts(canonical string) []string {
	var parts []string
	dir := filepath.Dir(canonical)
	for {
		base := filepath.Base(dir)
		parent := filepath.Dir(dir)
		if parent == dir || base == string(filepath.Separator) || base == "." {
			break
		}
		parts = append(parts, base)
		dir = parent
	}
	return parts
}
