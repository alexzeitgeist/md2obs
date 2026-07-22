package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"md2obs/internal/database"
	"md2obs/internal/source"
	"md2obs/internal/watcher"
)

// UntrackOptions selects sources whose automatic watch/refresh eligibility
// should be disabled in the configured vault. Journal history and vault files
// are never removed.
type UntrackOptions struct {
	Files         []string
	Missing       bool
	OlderThanDays int
	DryRun        bool
}

// Validate rejects ambiguous mixtures of explicit paths and batch selectors.
func (o UntrackOptions) Validate() error {
	if o.OlderThanDays < 0 {
		return fmt.Errorf("--older-than must be positive")
	}
	batch := o.Missing || o.OlderThanDays > 0
	if len(o.Files) > 0 && batch {
		return fmt.Errorf("source paths cannot be combined with --missing or --older-than")
	}
	if len(o.Files) == 0 && !batch {
		return fmt.Errorf("usage: md2obs untrack FILE... or md2obs untrack [--missing] [--older-than AGE]")
	}
	return nil
}

type untrackSelection struct {
	entry database.TrackingEntry
}

// RunUntrack explicitly disables automatic watch and refresh for selected
// sources in the configured vault. A later explicit import reactivates them.
func RunUntrack(ctx context.Context, d *Deps, opts UntrackOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	vaultID, err := database.GetVaultIDByKey(ctx, d.DB.Query(), d.Config.VaultAbs)
	if err != nil {
		return err
	}
	if len(opts.Files) > 0 {
		return runNamedUntrack(ctx, d, opts, vaultID)
	}
	return runBatchUntrack(ctx, d, opts, vaultID)
}

func runNamedUntrack(ctx context.Context, d *Deps, opts UntrackOptions, vaultID int64) error {
	selected := make([]untrackSelection, 0, len(opts.Files))
	seen := make(map[int64]struct{}, len(opts.Files))
	failed := 0

	for _, file := range opts.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := trackingEntriesForInput(ctx, d, file)
		if err != nil {
			fmt.Fprintf(d.Err, "error: untrack %s: %v\n", file, err)
			failed++
			continue
		}
		if len(entries) == 0 || vaultID == 0 {
			fmt.Fprintf(d.Err, "error: untrack %s: not tracked in configured vault\n", file)
			failed++
			continue
		}
		if len(entries) > 1 {
			var identities []string
			for _, entry := range entries {
				identities = append(identities, entry.CanonicalPath)
			}
			fmt.Fprintf(d.Err, "error: untrack %s: path is ambiguous; matches %s\n", file, strings.Join(identities, ", "))
			failed++
			continue
		}

		entry := entries[0]
		if _, duplicate := seen[entry.ID]; duplicate {
			continue
		}
		seen[entry.ID] = struct{}{}
		if !entry.Active {
			fmt.Fprintf(d.Out, "already untracked: %s\n", entry.DisplayPath)
			continue
		}
		selected = append(selected, untrackSelection{entry: entry})
	}
	changed, err := applyUntrack(ctx, d, vaultID, selected, opts.DryRun)
	if err != nil {
		return err
	}
	if !opts.DryRun && changed > 0 {
		notifyUntrack(d)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d untrack arguments failed", failed, len(opts.Files))
	}
	return nil
}

func trackingEntriesForInput(ctx context.Context, d *Deps, input string) ([]database.TrackingEntry, error) {
	abs, err := filepath.Abs(input)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	displayPath := filepath.Clean(abs)
	canonicalPath := displayPath
	if canonical, _, canonicalErr := source.Canonicalize(input); canonicalErr == nil {
		canonicalPath = canonical
	}
	return database.FindTrackingEntriesByPath(
		ctx,
		d.DB.Query(),
		d.Config.VaultAbs,
		canonicalPath,
		displayPath,
	)
}

func runBatchUntrack(ctx context.Context, d *Deps, opts UntrackOptions, vaultID int64) error {
	if vaultID == 0 {
		printBatchUntrackSummary(d.Out, opts.DryRun, 0, 0, 0, 0)
		return nil
	}
	entries, err := database.ListTrackingEntries(ctx, d.DB.Query(), d.Config.VaultAbs)
	if err != nil {
		return err
	}

	cutoff := ""
	if opts.OlderThanDays > 0 {
		cutoff = d.Now().AddDate(0, 0, -opts.OlderThanDays).Format(dateFormat)
	}
	selected := make([]untrackSelection, 0, len(entries))
	checked, unavailable, failed := 0, 0, 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Active {
			continue
		}
		checked++
		if cutoff != "" && entry.SnapshotDate >= cutoff {
			continue
		}
		if opts.Missing {
			missing, unavailableErr, inspectErr := sourceMissingWithAccessibleParent(entry.CanonicalPath)
			switch {
			case unavailableErr != nil:
				fmt.Fprintf(d.Out, "unavailable, still tracked: %s: %v\n", entry.DisplayPath, unavailableErr)
				unavailable++
				continue
			case inspectErr != nil:
				fmt.Fprintf(d.Err, "error: untrack: inspect %s: %v\n", entry.DisplayPath, inspectErr)
				failed++
				continue
			case !missing:
				continue
			}
		}
		selected = append(selected, untrackSelection{entry: entry})
	}
	if opts.Missing && len(selected) > 0 {
		confirmed := selected[:0]
		for _, selection := range selected {
			missing, unavailableErr, inspectErr := sourceMissingWithAccessibleParent(selection.entry.CanonicalPath)
			switch {
			case unavailableErr != nil:
				fmt.Fprintf(d.Out, "unavailable, still tracked: %s: %v\n", selection.entry.DisplayPath, unavailableErr)
				unavailable++
			case inspectErr != nil:
				fmt.Fprintf(d.Err, "error: untrack: recheck %s: %v\n", selection.entry.DisplayPath, inspectErr)
				failed++
			case missing:
				confirmed = append(confirmed, selection)
			}
		}
		selected = confirmed
	}

	changed, err := applyUntrack(ctx, d, vaultID, selected, opts.DryRun)
	if err != nil {
		return err
	}
	if !opts.DryRun && changed > 0 {
		notifyUntrack(d)
	}
	printBatchUntrackSummary(d.Out, opts.DryRun, checked, changed, unavailable, failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d active sources could not be checked for untracking", failed, checked)
	}
	return nil
}

func applyUntrack(ctx context.Context, d *Deps, vaultID int64, selected []untrackSelection, dryRun bool) (int, error) {
	if dryRun {
		for _, selection := range selected {
			fmt.Fprintf(d.Out, "would untrack: %s\n", selection.entry.DisplayPath)
		}
		return len(selected), nil
	}
	if len(selected) == 0 {
		return 0, nil
	}

	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("untrack: begin transaction: %w", err)
	}
	defer tx.Rollback()
	nowUTC := utc(d.Now())
	for _, selection := range selected {
		if err := database.SetWatchActive(ctx, tx, selection.entry.ID, vaultID, false, nowUTC); err != nil {
			return 0, fmt.Errorf("untrack %s: %w", selection.entry.DisplayPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("untrack: commit: %w", err)
	}
	for _, selection := range selected {
		fmt.Fprintf(d.Out, "untracked: %s\n", selection.entry.DisplayPath)
	}
	return len(selected), nil
}

func notifyUntrack(d *Deps) {
	if err := watcher.NotifyImport(d.DB.Path); err != nil {
		fmt.Fprintf(d.Err, "warning: sources were untracked, but running watchers may need to be restarted: %v\n", err)
	}
}

func printBatchUntrackSummary(out io.Writer, dryRun bool, checked, changed, unavailable, failed int) {
	verb := "untracked"
	if dryRun {
		verb = "would untrack"
	}
	sourceWord := "sources"
	if checked == 1 {
		sourceWord = "source"
	}
	fmt.Fprintf(out, "Checked %d active %s: %d %s, %d unavailable, %d failed\n", checked, sourceWord, changed, verb, unavailable, failed)
}

// sourceMissingWithAccessibleParent reports a definite exact-path absence only
// after proving that the immediate parent is a readable directory. A second
// source stat narrows the recreation race; batch callers re-run the complete
// check immediately before changing tracking.
func sourceMissingWithAccessibleParent(path string) (missing bool, unavailableErr, inspectErr error) {
	_, sourceErr := os.Stat(path)
	if sourceErr == nil {
		return false, nil, inspectTrackingIdentity(path)
	}

	parent := filepath.Dir(path)
	info, parentErr := os.Stat(parent)
	if parentErr != nil {
		return false, fmt.Errorf("source parent %s: %w", parent, parentErr), nil
	}
	if !info.IsDir() {
		return false, fmt.Errorf("source parent %s is not a directory", parent), nil
	}
	resolvedParent, resolveErr := filepath.EvalSymlinks(parent)
	if resolveErr != nil {
		return false, nil, fmt.Errorf("resolve source parent %s: %w", parent, resolveErr)
	}
	if resolvedParent != filepath.Clean(parent) {
		return false, nil, fmt.Errorf("source parent identity changed: registered %s now resolves to %s", parent, resolvedParent)
	}
	dir, openErr := os.Open(parent)
	if openErr != nil {
		return false, fmt.Errorf("open source parent %s: %w", parent, openErr), nil
	}
	_, readErr := dir.Readdirnames(1)
	closeErr := dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, fmt.Errorf("read source parent %s: %w", parent, readErr), nil
	}
	if closeErr != nil {
		return false, fmt.Errorf("close source parent %s: %w", parent, closeErr), nil
	}
	if !errors.Is(sourceErr, os.ErrNotExist) {
		return false, nil, sourceErr
	}

	_, sourceErr = os.Stat(path)
	if sourceErr == nil {
		return false, nil, inspectTrackingIdentity(path)
	}
	if errors.Is(sourceErr, os.ErrNotExist) {
		return true, nil, nil
	}
	return false, nil, sourceErr
}

func inspectTrackingIdentity(path string) error {
	canonical, _, err := source.Canonicalize(path)
	if err != nil {
		return err
	}
	if canonical != filepath.Clean(path) {
		return fmt.Errorf("source identity changed: registered %s now resolves to %s", path, canonical)
	}
	return nil
}
