package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alexzeitgeist/md2obs/internal/database"
	"github.com/alexzeitgeist/md2obs/internal/source"
)

// candidateReconcileResult describes the source-side outcome of reconciling
// one database-selected candidate. A nil Import with Missing and Untracked
// both false means the source content already matches the selected snapshot.
type candidateReconcileResult struct {
	Import    *Result
	Missing   bool
	Untracked bool
}

// reconcileWatchCandidate checks that the registered source identity is still
// intact, compares its current content with the selected snapshot, and imports
// only when the content differs. Watch activation and one-shot refresh share
// this path so their identity and hash gates cannot drift apart. The facts
// gathered here are handed to importFile so the source is read and hashed
// only once per reconciliation.
func reconcileWatchCandidate(
	ctx context.Context,
	d *Deps,
	candidate database.WatchCandidate,
	policy Policy,
	rerender bool,
) (candidateReconcileResult, error) {
	if missing, err := verifyCandidateIdentity(candidate); missing || err != nil {
		return candidateReconcileResult{Missing: missing}, err
	}
	info, err := source.Inspect(candidate.CanonicalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return candidateReconcileResult{Missing: true}, nil
		}
		return candidateReconcileResult{}, fmt.Errorf("inspect registered source %s: %w", candidate.DisplayPath, err)
	}
	content, sha, err := source.ReadAndHash(candidate.CanonicalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return candidateReconcileResult{Missing: true}, nil
		}
		return candidateReconcileResult{}, fmt.Errorf("inspect registered source %s: %w", candidate.DisplayPath, err)
	}
	if !rerender && sha == candidate.ContentSHA {
		return candidateReconcileResult{}, nil
	}

	// The bytes just read are committed under the registered identity, and
	// importFile skips its own verification when facts are supplied, so verify
	// again here: a retarget that persisted through the read is rejected
	// instead of imported under the original registration.
	if missing, err := verifyCandidateIdentity(candidate); missing || err != nil {
		return candidateReconcileResult{Missing: missing}, err
	}

	facts := &sourceFacts{info: info, content: content, sha: sha}
	res, err := importFile(ctx, d, candidate.CanonicalPath, policy, &candidate.Source, facts)
	if errors.Is(err, errSourceUntracked) {
		return candidateReconcileResult{Untracked: true}, nil
	}
	if err != nil {
		return candidateReconcileResult{}, err
	}
	return candidateReconcileResult{Import: &res}, nil
}

// verifyCandidateIdentity classifies a registered-identity check for the
// reconcile flow: a vanished path is a missing source, not a failure, and an
// identity change keeps its own error unwrapped.
func verifyCandidateIdentity(candidate database.WatchCandidate) (missing bool, err error) {
	if err := source.VerifyRegisteredIdentity(candidate.CanonicalPath); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return true, nil
		case errors.Is(err, source.ErrSourceIdentityChanged):
			return false, err
		}
		return false, fmt.Errorf("inspect registered source %s: %w", candidate.DisplayPath, err)
	}
	return false, nil
}
