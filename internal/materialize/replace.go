package materialize

import "os"

// Replace atomically replaces dst with src. On POSIX systems rename(2)
// replaces an existing destination atomically. Windows rename-over-existing
// semantics differ and are not yet integration-tested, so Windows is not a
// claimed-supported platform; this helper is the single place to add a
// Windows-specific implementation behind a build tag.
func Replace(src, dst string) error {
	return os.Rename(src, dst)
}
