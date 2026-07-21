package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"md2obs/internal/database"
	"md2obs/internal/source"
)

// candidateReconcileResult describes the source-side outcome of reconciling
// one database-selected candidate. A nil Import with Missing false means the
// source content already matches the selected snapshot.
type candidateReconcileResult struct {
	Import  *Result
	Missing bool
}

// reconcileWatchCandidate checks that the registered source identity is still
// intact, compares its current content with the selected snapshot, and imports
// only when the content differs. Watch activation and one-shot refresh share
// this path so their identity and hash gates cannot drift apart.
func reconcileWatchCandidate(
	ctx context.Context,
	d *Deps,
	candidate database.WatchCandidate,
	policy Policy,
) (candidateReconcileResult, error) {
	canonical, _, err := source.Canonicalize(candidate.CanonicalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return candidateReconcileResult{Missing: true}, nil
		}
		return candidateReconcileResult{}, fmt.Errorf("inspect registered source %s: %w", candidate.DisplayPath, err)
	}
	if canonical != candidate.CanonicalPath {
		return candidateReconcileResult{}, fmt.Errorf(
			"watch source identity changed: registered %s now resolves to %s",
			candidate.CanonicalPath,
			canonical,
		)
	}
	_, sha, err := source.ReadAndHash(canonical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return candidateReconcileResult{Missing: true}, nil
		}
		return candidateReconcileResult{}, fmt.Errorf("inspect registered source %s: %w", candidate.DisplayPath, err)
	}
	if sha == candidate.ContentSHA {
		return candidateReconcileResult{}, nil
	}

	res, err := ImportWatchedSource(ctx, d, candidate.Source, policy)
	if err != nil {
		return candidateReconcileResult{}, err
	}
	return candidateReconcileResult{Import: &res}, nil
}
