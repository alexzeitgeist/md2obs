package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"md2obs/internal/database"
	"md2obs/internal/layout"
	"md2obs/internal/materialize"
	"md2obs/internal/safepath"
	"md2obs/internal/source"
)

// Policy decides what happens when the vault copy about to be overwritten
// was edited (e.g. on a phone) since md2obs last wrote it. Explicit imports
// always overwrite; the watcher defaults to skip because unattended
// automation should behave more cautiously than an explicit command.
type Policy string

const (
	// PolicyOverwrite replaces the edited vault copy with the source.
	PolicyOverwrite Policy = "overwrite"
	// PolicySkip leaves the edited vault copy alone and reports the conflict.
	PolicySkip Policy = "skip"
	// PolicyPreserve saves the edited vault copy into <root>-Conflicts/,
	// then updates the materialization from the source.
	PolicyPreserve Policy = "preserve"
)

var errSourceUntracked = errors.New("source is no longer tracked")

// ParsePolicy validates an --on-vault-change value.
func ParsePolicy(s string) (Policy, error) {
	switch Policy(s) {
	case PolicyOverwrite, PolicySkip, PolicyPreserve:
		return Policy(s), nil
	}
	return "", fmt.Errorf("invalid --on-vault-change value %q (want overwrite, skip, or preserve)", s)
}

// Status classifies the outcome of one import operation.
type Status string

const (
	StatusImported  Status = "imported"  // new snapshot materialized
	StatusUpdated   Status = "updated"   // existing same-day materialization rewritten
	StatusUnchanged Status = "unchanged" // content already current; no vault write
	StatusSkipped   Status = "skipped"   // vault copy edited; left alone per policy
)

// Result reports one import operation for user-facing output.
type Result struct {
	Status       Status
	DisplayPath  string
	RelPath      string
	PreservedRel string
}

// RunImport imports each named file, printing per-file results. It continues
// past per-file failures and returns an error if any file failed.
func RunImport(ctx context.Context, d *Deps, files []string) error {
	failed := 0
	for _, f := range files {
		res, err := ImportFile(ctx, d, f, PolicyOverwrite)
		if err != nil {
			fmt.Fprintf(d.Err, "error: %v\n", err)
			failed++
			continue
		}
		printResult(d.Out, res)
	}
	if failed < len(files) {
		notifyWatchers(d, "import succeeded")
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d imports failed", failed, len(files))
	}
	return nil
}

// ImportFile runs the full explicit import operation for one source file.
func ImportFile(ctx context.Context, d *Deps, file string, policy Policy) (Result, error) {
	return importFile(ctx, d, file, policy, nil, nil)
}

// ImportWatchedSource imports a source selected from SQLite at watcher
// startup. Re-resolving the event path must produce the same canonical
// identity; the watcher is never allowed to register a different source or
// recreate bookkeeping that an explicit untrack removed concurrently.
func ImportWatchedSource(ctx context.Context, d *Deps, registered database.Source, policy Policy) (Result, error) {
	return importFile(ctx, d, registered.CanonicalPath, policy, &registered, nil)
}

// sourceFacts carries one verified inspect-and-read of a source so a caller
// that already gathered them (the reconcile path) does not resolve, read, and
// hash the same file a second time inside importFile.
type sourceFacts struct {
	info    source.Info
	content []byte
	sha     string
}

// importFile resolves and hashes a source, updates its revision and snapshot
// facts, applies the vault-change policy, atomically materializes the content,
// and commits the database transaction. A successful physical rename can
// outlive a later database failure; retrying the import converges on the
// source's current content.
func importFile(ctx context.Context, d *Deps, file string, policy Policy, registered *database.Source, facts *sourceFacts) (Result, error) {
	var canonical, display string
	if registered != nil {
		// facts != nil means reconcileWatchCandidate verified the registered
		// identity again after reading the content it supplies here, binding
		// those bytes to a still-valid registration.
		if facts == nil {
			if err := source.VerifyRegisteredIdentity(registered.CanonicalPath); err != nil {
				return Result{}, err
			}
		}
		canonical, display = registered.CanonicalPath, registered.DisplayPath
	} else {
		var err error
		canonical, display, err = source.Canonicalize(file)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", file, err)
		}
	}
	res := Result{DisplayPath: display}

	if facts == nil {
		info, err := source.Inspect(canonical)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		content, sha, err := source.ReadAndHash(canonical)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		facts = &sourceFacts{info: info, content: content, sha: sha}
	}
	info, content, sha := facts.info, facts.content, facts.sha

	now := d.Now()
	date := now.Format(dateFormat)
	nowUTC := utc(now)
	vaultRoot := d.Config.VaultAbs

	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("import %s: begin transaction: %w", display, err)
	}
	defer tx.Rollback()

	var vaultID int64
	if registered != nil {
		vaultID, err = database.GetVaultIDByKey(ctx, tx, vaultRoot)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		if vaultID == 0 {
			return Result{}, errSourceUntracked
		}
		tracked, err := database.IsSourceTrackedInVault(ctx, tx, registered.ID, vaultID)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		if !tracked {
			return Result{}, errSourceUntracked
		}
	}

	var srcID int64
	if registered == nil {
		srcID, err = database.UpsertSource(ctx, tx, canonical, display, nowUTC)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
	} else {
		srcID = registered.ID
		if err := database.TouchSource(ctx, tx, srcID, canonical, nowUTC); err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
	}
	revID, err := database.FindOrCreateRevision(ctx, tx, srcID, sha, info.ByteSize, info.MtimeNS, nowUTC)
	if err != nil {
		return Result{}, fmt.Errorf("import %s: %w", display, err)
	}
	if registered == nil {
		vaultID, err = database.EnsureVault(ctx, tx, vaultRoot, filepath.Base(vaultRoot), vaultRoot, nowUTC)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
	}
	layoutJSON, _ := json.Marshal(map[string]string{"root_directory": d.Config.RootDirectory})
	layoutID, err := database.EnsureLayout(ctx, tx, d.Layout.Name(), d.Layout.Version(), string(layoutJSON), nowUTC)
	if err != nil {
		return Result{}, fmt.Errorf("import %s: %w", display, err)
	}

	snap, err := database.GetSnapshot(ctx, tx, srcID, date)
	if err != nil {
		return Result{}, fmt.Errorf("import %s: %w", display, err)
	}
	var mat *database.Materialization
	if snap != nil {
		mat, err = database.GetMaterialization(ctx, tx, snap.ID, vaultID)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
	}

	// WithinRoot validates current filesystem state only, so its result is
	// never carried across other filesystem or database operations: every
	// physical vault read and write below re-verifies containment immediately
	// before the operation, or an ancestor symlink retargeted in between could
	// redirect it outside the vault.

	// Unchanged: the database and the physical vault copy must all contain the
	// same revision. Merely finding the destination is insufficient because an
	// Obsidian edit may have changed its content since md2obs last wrote it.
	if snap != nil && snap.RevisionID == revID && mat != nil && mat.WrittenRevisionID == revID {
		destAbs, err := safepath.WithinRoot(vaultRoot, mat.RelativePath)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		current, readErr := os.ReadFile(destAbs)
		if readErr == nil && source.HashBytes(current) == sha {
			if err := tx.Commit(); err != nil {
				return Result{}, fmt.Errorf("import %s: commit: %w", display, err)
			}
			res.Status = StatusUnchanged
			res.RelPath = mat.RelativePath
			return res, nil
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) && policy != PolicyOverwrite {
			return Result{}, fmt.Errorf("import %s: read vault copy: %w", display, readErr)
		}
	}

	// Vault-change policy: before overwriting an existing materialization,
	// compare the current vault content with the revision last written. A
	// mismatch means someone edited the vault copy since the last import.
	if mat != nil && policy != PolicyOverwrite {
		destAbs, err := safepath.WithinRoot(vaultRoot, mat.RelativePath)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		current, readErr := os.ReadFile(destAbs)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return Result{}, fmt.Errorf("import %s: read vault copy: %w", display, readErr)
		}
		if readErr == nil {
			currentSHA := source.HashBytes(current)
			writtenSHA, err := database.RevisionSHA(ctx, tx, mat.WrittenRevisionID)
			if err != nil {
				return Result{}, fmt.Errorf("import %s: %w", display, err)
			}
			if currentSHA != writtenSHA && currentSHA != sha {
				switch policy {
				case PolicySkip:
					// Record the observed revision as today's intent but do
					// not touch the edited file. written_revision_id continues
					// to identify the last revision md2obs recorded as written;
					// it is not a claim about the current edited bytes.
					if snap.RevisionID != revID {
						if err := database.UpdateSnapshotRevision(ctx, tx, snap.ID, revID, nowUTC); err != nil {
							return Result{}, fmt.Errorf("import %s: %w", display, err)
						}
					}
					if err := tx.Commit(); err != nil {
						return Result{}, fmt.Errorf("import %s: commit: %w", display, err)
					}
					res.Status = StatusSkipped
					res.RelPath = mat.RelativePath
					return res, nil
				case PolicyPreserve:
					preservedRel, err := preserveVaultEdit(d, mat.RelativePath, current, date)
					if err != nil {
						return Result{}, fmt.Errorf("import %s: preserve vault edit: %w", display, err)
					}
					res.PreservedRel = preservedRel
				}
			}
		}
	}

	// Snapshot upsert: create today's snapshot or re-point it at the new
	// revision.
	created := false
	var snapID int64
	if snap == nil {
		snapID, err = database.CreateSnapshot(ctx, tx, srcID, revID, date, nowUTC)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		created = true
	} else {
		snapID = snap.ID
		if snap.RevisionID != revID {
			if err := database.UpdateSnapshotRevision(ctx, tx, snap.ID, revID, nowUTC); err != nil {
				return Result{}, fmt.Errorf("import %s: %w", display, err)
			}
		}
	}

	// Materialize: reuse the reserved path, or reserve the first free
	// layout candidate.
	if mat != nil {
		destAbs, err := safepath.WithinRoot(vaultRoot, mat.RelativePath)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		if err := materialize.WriteAtomic(destAbs, content, 0o644); err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		if err := database.UpdateMaterializationWritten(ctx, tx, mat.ID, revID, nowUTC); err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		res.RelPath = mat.RelativePath
	} else {
		relPath, err := reserveCandidate(ctx, d, tx, vaultID, now, canonical)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		destAbs, err := safepath.WithinRoot(vaultRoot, relPath)
		if err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		if err := materialize.WriteAtomic(destAbs, content, 0o644); err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		if _, err := database.CreateMaterialization(ctx, tx, snapID, vaultID, layoutID, relPath, revID, nowUTC); err != nil {
			return Result{}, fmt.Errorf("import %s: %w", display, err)
		}
		res.RelPath = relPath
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("import %s: commit: %w", display, err)
	}
	if created {
		res.Status = StatusImported
	} else {
		res.Status = StatusUpdated
	}
	return res, nil
}

// reserveCandidate walks the layout's ordered candidates and picks the first
// vault-relative path not owned by another materialization. An existing path
// unknown to the database is preserved by allocating a numbered sibling.
// Candidates that fail the containment check are skipped rather than fatal
// because a later candidate may still be valid. The caller re-verifies
// containment when it joins the returned path for the physical write.
func reserveCandidate(ctx context.Context, d *Deps, q database.Querier, vaultID int64, now time.Time, canonical string) (string, error) {
	input := layout.CandidateInput{
		SnapshotDate:  now,
		SourcePath:    canonical,
		Basename:      filepath.Base(canonical),
		ParentParts:   source.ParentParts(canonical),
		RootDirectory: d.Config.RootDirectory,
	}
	candidates, err := d.Layout.CandidatePaths(input)
	if err != nil {
		return "", err
	}
	var containmentErr error
	for _, rel := range candidates {
		owned, occupied, containErr, err := probeCandidate(ctx, d, q, vaultID, rel)
		switch {
		case err != nil:
			return "", err
		case owned:
			continue
		case occupied:
			return reserveNumberedCandidate(ctx, d, q, vaultID, rel)
		case containErr != nil:
			containmentErr = containErr
			continue
		}
		return rel, nil
	}
	if containmentErr != nil {
		return "", fmt.Errorf("no safe destination for %s: %w", canonical, containmentErr)
	}
	return "", fmt.Errorf("no available destination filename for %s", canonical)
}

func reserveNumberedCandidate(ctx context.Context, d *Deps, q database.Querier, vaultID int64, rel string) (string, error) {
	for n := 1; ; n++ {
		candidate, err := layout.NumberedSibling(rel, n)
		if err != nil {
			return "", err
		}
		owned, occupied, containErr, err := probeCandidate(ctx, d, q, vaultID, candidate)
		switch {
		case err != nil:
			return "", err
		case owned, occupied:
			continue
		case containErr != nil:
			return "", containErr
		}
		return candidate, nil
	}
}

// probeCandidate classifies one destination candidate: owned by another
// materialization, physically occupied, or unsafe (containErr). Occupancy is
// probed on the lexical path without following symlinks so an unowned symlink
// counts as occupied and is preserved rather than resolved; containment is
// verified only for a candidate that would actually be written.
func probeCandidate(ctx context.Context, d *Deps, q database.Querier, vaultID int64, rel string) (owned, occupied bool, containErr, err error) {
	owned, err = database.IsPathOwned(ctx, q, vaultID, rel)
	if err != nil || owned {
		return owned, false, nil, err
	}
	occupied, err = destinationOccupied(filepath.Join(d.Config.VaultAbs, filepath.FromSlash(rel)))
	if err != nil || occupied {
		return false, occupied, nil, err
	}
	if _, containErr = safepath.WithinRoot(d.Config.VaultAbs, rel); containErr != nil {
		return false, false, containErr, nil
	}
	return false, false, nil, nil
}

func destinationOccupied(destAbs string) (bool, error) {
	_, err := os.Lstat(destAbs)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect destination %s: %w", destAbs, err)
	}
}

// printResult renders one import result with the arrow aligned under the
// status word.
func printResult(w io.Writer, res Result) {
	switch res.Status {
	case StatusUnchanged:
		fmt.Fprintf(w, "unchanged: %s\n", res.DisplayPath)
		return
	case StatusSkipped:
		fmt.Fprintf(w, "skipped: %s (vault copy edited; kept)\n", res.DisplayPath)
		fmt.Fprintf(w, "%s-> %s\n", strings.Repeat(" ", len(StatusSkipped)-2), res.RelPath)
		return
	}
	fmt.Fprintf(w, "%s: %s\n", res.Status, res.DisplayPath)
	indent := strings.Repeat(" ", len(res.Status)-2)
	fmt.Fprintf(w, "%s-> %s\n", indent, res.RelPath)
	if res.PreservedRel != "" {
		fmt.Fprintf(w, "%spreserved vault edit -> %s\n", indent, res.PreservedRel)
	}
}

// preserveVaultEdit saves an edited vault copy under
// <root>-Conflicts/YYYY-MM-DD/<stem>--vault-edit<suffix> before it is
// overwritten. An identical already-preserved copy is reused; otherwise a
// numeric suffix avoids clobbering earlier preserved edits.
func preserveVaultEdit(d *Deps, matRelPath string, content []byte, date string) (string, error) {
	name := path.Base(matRelPath)
	suffix := path.Ext(name)
	stem := strings.TrimSuffix(name, suffix)
	conflictsRoot := d.Config.RootDirectory + "-Conflicts"

	for n := 1; ; n++ {
		base := stem + "--vault-edit"
		if n > 1 {
			base = fmt.Sprintf("%s--vault-edit--%d", stem, n)
		}
		rel := path.Join(conflictsRoot, date, base+suffix)
		abs, err := safepath.WithinRoot(d.Config.VaultAbs, rel)
		if err != nil {
			return "", err
		}
		existing, err := os.ReadFile(abs)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := materialize.WriteAtomic(abs, content, 0o644); err != nil {
				return "", err
			}
			return rel, nil
		case err != nil:
			return "", err
		case bytes.Equal(existing, content):
			return rel, nil
		}
	}
}
