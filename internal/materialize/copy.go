// Package materialize performs the physical vault writes: atomic file
// replacement and destination containment checks.
package materialize

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteAtomic writes content to destAbs by creating a temporary file in the
// destination directory, flushing it, and renaming it over the destination.
// The temporary file lives beside the destination because rename is reliably
// atomic only within one filesystem.
func WriteAtomic(destAbs string, content []byte, perm fs.FileMode) error {
	dir := filepath.Dir(destAbs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create destination directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".md2obs-tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(content); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := Replace(tmpName, destAbs); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", destAbs, err)
	}
	syncDir(dir)
	return nil
}

// syncDir makes the rename durable; failure to sync is not fatal.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}
