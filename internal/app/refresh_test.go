package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/md2obs/internal/database"
	"github.com/alexzeitgeist/md2obs/internal/render"
	"github.com/alexzeitgeist/md2obs/internal/watcher"
)

func TestRunRefreshChangedAndUnchanged(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	changed := writeSource(t, dir, "changed.md", "# one\n")
	stable := writeSource(t, dir, "stable.md", "# stable\n")

	changedImport, err := ImportFile(ctx, env.deps, changed, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	stableImport, err := ImportFile(ctx, env.deps, stable, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	stableVault := filepath.Join(env.vault, filepath.FromSlash(stableImport.RelPath))
	if err := os.WriteFile(stableVault, []byte("# phone edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changed, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, OnVaultChange: PolicyOverwrite,
	})
	if err != nil {
		t.Fatalf("RunRefresh: %v", err)
	}
	if got := env.vaultFile(t, changedImport.RelPath); got != "# two\n" {
		t.Fatalf("changed vault content = %q", got)
	}
	if got := env.vaultFile(t, stableImport.RelPath); got != "# phone edit\n" {
		t.Fatalf("unchanged source evaluated overwrite policy: %q", got)
	}
	output := env.out.String()
	for _, want := range []string{
		"updated: " + changed,
		"Checked 2 sources: 1 refreshed, 0 conflicts skipped, 1 unchanged, 0 missing, 0 untracked during refresh, 0 failed",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
	if data, err := os.ReadFile(watcher.NotificationPath(env.deps.DB.Path)); err != nil || len(data) == 0 {
		t.Fatalf("refresh notification = %q, err %v", data, err)
	}
}

func TestRefreshAppliesProfileChangesOnlyWhenRerenderIsExplicit(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "profile-toggle.md", "# raw\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	env.deps.Config.ProvenanceFrontmatter = true

	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# raw\n" {
		t.Fatalf("plain refresh applied a config-only change:\n%s", got)
	}
	candidates, err := database.SelectAllWatchCandidates(ctx, env.deps.DB.Query(), env.vault)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %+v, err %v", candidates, err)
	}
	outcome, err := reconcileWatchCandidate(ctx, env.deps, candidates[0], PolicySkip, false)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Import != nil || outcome.Missing || outcome.Untracked {
		t.Fatalf("passive watch-style activation outcome = %+v", outcome)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# raw\n" {
		t.Fatalf("passive activation applied a config-only change:\n%s", got)
	}

	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, Rerender: true, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	rendered := env.vaultFile(t, first.RelPath)
	if !strings.Contains(rendered, "md2obs_source_path:") || !strings.HasSuffix(rendered, "# raw\n") {
		t.Fatalf("forced rerender did not apply provenance:\n%s", rendered)
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, Rerender: true, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	output := env.out.String()
	if !strings.Contains(output, "updated: "+src) {
		t.Fatalf("profile-changing rerender was not updated:\n%s", output)
	}
	if strings.Count(output, "Checked 1 source:") != 3 {
		t.Fatalf("expected three one-source refresh passes:\n%s", output)
	}
	if !strings.Contains(output, "0 refreshed, 0 conflicts skipped, 1 unchanged") {
		t.Fatalf("converged rerender was not unchanged:\n%s", output)
	}
}

func TestSkippedRerenderLeavesEveryWrittenFactAndPlainRefreshGoesQuiet(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "rerender-conflict.md", "# raw\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	_, before := materializationForSource(t, env, src, "2026-07-20")
	if err := os.WriteFile(
		filepath.Join(env.vault, filepath.FromSlash(first.RelPath)),
		[]byte("# phone edit\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	env.deps.Config.ProvenanceFrontmatter = true

	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, Rerender: true, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# phone edit\n" {
		t.Fatalf("skipped rerender changed vault edit: %q", got)
	}
	_, after := materializationForSource(t, env, src, "2026-07-20")
	if after.WrittenRevisionID != before.WrittenRevisionID ||
		after.WrittenContentSHA256 != before.WrittenContentSHA256 ||
		after.WrittenRenderProfile != before.WrittenRenderProfile ||
		after.WrittenAtUTC != before.WrittenAtUTC {
		t.Fatalf("skip changed written metadata:\nbefore %+v\nafter  %+v", before, after)
	}

	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	output := env.out.String()
	if strings.Count(output, "skipped: "+src) != 1 {
		t.Fatalf("ordinary refresh repeated an explicit rerender conflict:\n%s", output)
	}
	if !strings.Contains(output, "Checked 1 source: 0 refreshed, 0 conflicts skipped, 1 unchanged") {
		t.Fatalf("ordinary refresh did not return to the passive source gate:\n%s", output)
	}
}

func TestRerenderTreatsLiveDesiredBytesAsConvergedNotEdited(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	raw := []byte("# raw\n")
	src := writeSource(t, t.TempDir(), "already-desired.md", string(raw))
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := materializationForSource(t, env, src, "2026-07-20")
	env.deps.Config.ProvenanceFrontmatter = true
	desired, err := render.Render(render.Input{
		SourceContent:  raw,
		CanonicalPath:  src,
		SnapshotTime:   snap.CreatedAtUTC,
		WithProvenance: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(env.vault, filepath.FromSlash(first.RelPath)),
		desired.Content,
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, Rerender: true, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env.out.String(), "skipped: "+src) {
		t.Fatalf("desired live bytes were treated as an independent edit:\n%s", env.out.String())
	}
	if got := env.vaultFile(t, first.RelPath); got != string(desired.Content) {
		t.Fatalf("desired content changed unexpectedly:\n%s", got)
	}
}

func TestAllRerenderCreatesTodayWithoutMutatingHistoricalMaterialization(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "historical-profile.md", "# raw\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	_, historicalBefore := materializationForSource(t, env, src, "2026-07-20")
	env.setNow(time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))
	env.deps.Config.ProvenanceFrontmatter = true

	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		Days: 1, Rerender: true, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.out.String(), "Checked 0 sources") {
		t.Fatalf("default selection unexpectedly included an older source:\n%s", env.out.String())
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{
		All: true, Rerender: true, OnVaultChange: PolicySkip,
	}); err != nil {
		t.Fatal(err)
	}
	todaySnap, todayMat := materializationForSource(t, env, src, "2026-07-21")
	if todaySnap == nil || todayMat.WrittenRenderProfile != render.ProvenanceProfile {
		t.Fatalf("today's rerender materialization = %+v / %+v", todaySnap, todayMat)
	}
	if got := env.vaultFile(t, todayMat.RelativePath); !strings.Contains(got, "md2obs_source_path:") {
		t.Fatalf("today's materialization was not rerendered:\n%s", got)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# raw\n" {
		t.Fatalf("historical vault file was rewritten:\n%s", got)
	}
	_, historicalAfter := materializationForSource(t, env, src, "2026-07-20")
	if historicalAfter.WrittenRenderProfile != historicalBefore.WrittenRenderProfile ||
		historicalAfter.WrittenContentSHA256 != historicalBefore.WrittenContentSHA256 ||
		historicalAfter.WrittenAtUTC != historicalBefore.WrittenAtUTC {
		t.Fatalf("historical metadata changed:\nbefore %+v\nafter  %+v", historicalBefore, historicalAfter)
	}
}

func TestReconcileWatchCandidateReportsSourceUntrackedAfterSelection(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "concurrently-untracked.md", "# original\n")
	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}

	candidates, err := database.SelectAllWatchCandidates(ctx, env.deps.DB.Query(), env.deps.Config.VaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("selected %d candidates, want 1", len(candidates))
	}
	if err := os.WriteFile(src, []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunUntrack(ctx, env.deps, UntrackOptions{Files: []string{src}}); err != nil {
		t.Fatal(err)
	}

	outcome, err := reconcileWatchCandidate(ctx, env.deps, candidates[0], PolicySkip, false)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Untracked || outcome.Missing || outcome.Import != nil {
		t.Fatalf("reconcile outcome = %+v, want untracked", outcome)
	}
}

func TestRunRefreshAllIncludesOlderMaterialization(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "older.md", "# old\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}

	env.setNow(time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))
	if err := os.WriteFile(src, []byte("# current\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{Days: 1, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# old\n" {
		t.Fatalf("one-day refresh selected older source: %q", got)
	}

	if err := RunRefresh(ctx, env.deps, RefreshOptions{All: true, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	if !vaultContainsContent(t, env.vault, "# current\n") {
		t.Fatalf("--all did not materialize current source; output:\n%s", env.out.String())
	}
	output := env.out.String()
	if !strings.Contains(output, "Checked 0 sources") || !strings.Contains(output, "Checked 1 source: 1 refreshed") {
		t.Fatalf("refresh summaries do not show date-window behavior:\n%s", output)
	}
}

func TestRunRefreshAllCreatesNewDayAlongsideOlderVaultEdit(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "cross-day.md", "# day one\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	firstVault := filepath.Join(env.vault, filepath.FromSlash(first.RelPath))
	if err := os.WriteFile(firstVault, []byte("# phone edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env.setNow(time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))
	if err := os.WriteFile(src, []byte("# day two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{All: true, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}

	if got := env.vaultFile(t, first.RelPath); got != "# phone edit\n" {
		t.Fatalf("older vault edit was changed: %q", got)
	}
	if !vaultContainsContent(t, env.vault, "# day two\n") {
		t.Fatalf("current source was not materialized on the new day; output:\n%s", env.out.String())
	}
	output := env.out.String()
	for _, want := range []string{
		"imported: " + src,
		"Checked 1 source: 1 refreshed, 0 conflicts skipped",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "skipped: "+src) {
		t.Fatalf("older vault edit was reported as a same-day conflict:\n%s", output)
	}
}

func TestRunRefreshDefaultsToSafeVaultConflictPolicy(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "conflict.md", "# original\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(first.RelPath))
	if err := os.WriteFile(vaultAbs, []byte("# phone edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("# revised\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RunRefresh(ctx, env.deps, RefreshOptions{Days: 1, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# phone edit\n" {
		t.Fatalf("refresh overwrote vault edit: %q", got)
	}
	output := env.out.String()
	if !strings.Contains(output, "skipped: "+src) || !strings.Contains(output, "1 conflicts skipped") {
		t.Fatalf("conflict was not reported:\n%s", output)
	}
}

func TestRunRefreshContinuesPastMissingAndIdentityFailures(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	missing := writeSource(t, dir, "a-missing.md", "# missing\n")
	retargeted := writeSource(t, dir, "b-retargeted.md", "# original\n")
	changed := writeSource(t, dir, "c-changed.md", "# one\n")

	if _, err := ImportFile(ctx, env.deps, missing, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFile(ctx, env.deps, retargeted, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	changedImport, err := ImportFile(ctx, env.deps, changed, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	other := writeSource(t, dir, "other.md", "# other\n")
	if err := os.Rename(retargeted, retargeted+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, retargeted); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(changed, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = RunRefresh(ctx, env.deps, RefreshOptions{Days: 1, OnVaultChange: PolicySkip})
	if err == nil || !strings.Contains(err.Error(), "1 of 3 refresh candidates failed") {
		t.Fatalf("RunRefresh error = %v", err)
	}
	if got := env.vaultFile(t, changedImport.RelPath); got != "# two\n" {
		t.Fatalf("later changed source was not refreshed: %q", got)
	}
	output := env.out.String()
	for _, want := range []string{
		"error: refresh: source identity changed",
		"Checked 3 sources: 1 refreshed, 0 conflicts skipped, 0 unchanged, 1 missing, 0 untracked during refresh, 1 failed",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
}

func TestRefreshAllEnrollsOlderSourceInRunningWatcher(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "enroll.md", "# day one\n")
	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	env.setNow(time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local))

	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunWatch(watchCtx, env.deps, WatchOptions{
			Days: 1, Debounce: 50 * time.Millisecond, OnVaultChange: PolicySkip,
		})
	}()
	defer cancel()
	if !waitUntil(5*time.Second, func() bool {
		return strings.Contains(env.out.String(), "Watching 0 sources")
	}) {
		t.Fatalf("watcher did not start empty; output:\n%s", env.out.String())
	}

	if err := os.WriteFile(src, []byte("# day two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunRefresh(ctx, env.deps, RefreshOptions{All: true, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	// This may race dynamic enrollment. Its activation check or the newly
	// armed directory watch must still converge the vault to the later content.
	if err := os.WriteFile(src, []byte("# after refresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(5*time.Second, func() bool {
		return vaultContainsContent(t, env.vault, "# after refresh\n")
	}) {
		t.Fatalf("refreshed source was not enrolled; output:\n%s", env.out.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWatch: %v", err)
	}
}
