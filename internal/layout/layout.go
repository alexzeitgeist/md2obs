// Package layout turns source facts into ordered candidate vault-relative
// destination paths. Vault paths are a materialization detail, never source
// identity, so layouts are replaceable behind this interface.
package layout

import "time"

// CandidateInput carries the facts a layout may use to build paths.
type CandidateInput struct {
	// SnapshotDate is the local date of the snapshot being materialized.
	SnapshotDate time.Time
	// SourcePath is the canonical absolute source path, used only for the
	// deterministic hash fallback.
	SourcePath string
	// Basename is the source file name, e.g. "README.md".
	Basename string
	// ParentParts are the source's parent directory names, nearest first,
	// e.g. ["project-b", "alice", "home"] for /home/alice/project-b/README.md.
	ParentParts []string
	// RootDirectory is the vault-relative destination root, e.g. "_External".
	RootDirectory string
}

// Layout produces ordered candidate vault-relative paths (forward slashes).
// The database layer reserves the first candidate not owned by another
// snapshot.
type Layout interface {
	Name() string
	Version() int
	CandidatePaths(input CandidateInput) ([]string, error)
}
