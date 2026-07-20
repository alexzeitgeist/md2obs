package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"md2obs/internal/config"
	"md2obs/internal/database"
	"md2obs/internal/layout"
)

// syncBuffer lets the watcher goroutine and the test write/read output
// without a data race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testEnv struct {
	deps  *Deps
	out   *syncBuffer
	vault string
	now   time.Time
	mu    sync.Mutex
}

func (e *testEnv) setNow(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = t
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := &config.Config{
		VaultPath:     t.TempDir(),
		Layout:        config.DefaultLayout,
		RootDirectory: config.DefaultRootDirectory,
		StateDBPath:   filepath.Join(t.TempDir(), "state.db"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := database.Open(context.Background(), cfg.StateDBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	env := &testEnv{
		out:   &syncBuffer{},
		vault: cfg.VaultAbs,
		now:   time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local),
	}
	env.deps = &Deps{
		DB:     db,
		Config: cfg,
		Layout: layout.NewDatedFlatV1(),
		Now: func() time.Time {
			env.mu.Lock()
			defer env.mu.Unlock()
			return env.now
		},
		Out: env.out,
		Err: env.out,
	}
	return env
}

func (e *testEnv) vaultFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.vault, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read vault file %s: %v", rel, err)
	}
	return string(data)
}

func writeSource(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportFirst(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "README.md", "# one\n")

	res, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if res.Status != StatusImported {
		t.Errorf("status = %s", res.Status)
	}
	if !strings.HasPrefix(res.RelPath, "_External/2026-07-20/") || !strings.HasSuffix(res.RelPath, "README.md") {
		t.Errorf("rel path = %q", res.RelPath)
	}
	if got := env.vaultFile(t, res.RelPath); got != "# one\n" {
		t.Errorf("vault content = %q", got)
	}
}

func TestImportSameDayUpdateOverwrites(t *testing.T) {
	env := newTestEnv(t)
	dir := t.TempDir()
	src := writeSource(t, dir, "README.md", "# one\n")
	ctx := context.Background()

	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	writeSource(t, dir, "README.md", "# two\n")
	second, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != StatusUpdated {
		t.Errorf("status = %s", second.Status)
	}
	if second.RelPath != first.RelPath {
		t.Errorf("same-day path changed: %q -> %q", first.RelPath, second.RelPath)
	}
	if got := env.vaultFile(t, second.RelPath); got != "# two\n" {
		t.Errorf("vault content = %q", got)
	}
}

func TestImportUnchanged(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "README.md", "# one\n")
	ctx := context.Background()

	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUnchanged {
		t.Errorf("status = %s", res.Status)
	}
	_, snapshots, materializations, err := env.deps.DB.Counts(ctx)
	if err != nil || snapshots != 1 || materializations != 1 {
		t.Errorf("counts: snapshots %d, materializations %d, err %v", snapshots, materializations, err)
	}
}

func TestImportLaterDayCreatesNewSnapshot(t *testing.T) {
	env := newTestEnv(t)
	dir := t.TempDir()
	src := writeSource(t, dir, "README.md", "# one\n")
	ctx := context.Background()

	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	env.setNow(time.Date(2026, 7, 21, 9, 0, 0, 0, time.Local))
	writeSource(t, dir, "README.md", "# two\n")
	second, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != StatusImported {
		t.Errorf("status = %s", second.Status)
	}
	if !strings.HasPrefix(second.RelPath, "_External/2026-07-21/") {
		t.Errorf("rel path = %q", second.RelPath)
	}
	// The earlier dated snapshot is preserved untouched.
	if got := env.vaultFile(t, first.RelPath); got != "# one\n" {
		t.Errorf("day-one content = %q", got)
	}
	if got := env.vaultFile(t, second.RelPath); got != "# two\n" {
		t.Errorf("day-two content = %q", got)
	}
}

func TestCollisionBetweenSameNameSources(t *testing.T) {
	env := newTestEnv(t)
	base := t.TempDir()
	dirA := filepath.Join(base, "project-a")
	dirB := filepath.Join(base, "project-b")
	os.MkdirAll(dirA, 0o755)
	os.MkdirAll(dirB, 0o755)
	srcA := writeSource(t, dirA, "README.md", "# a\n")
	srcB := writeSource(t, dirB, "README.md", "# b\n")
	ctx := context.Background()

	resA, err := ImportFile(ctx, env.deps, srcA, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	resB, err := ImportFile(ctx, env.deps, srcB, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if resA.RelPath == resB.RelPath {
		t.Fatalf("collision not resolved: both at %q", resA.RelPath)
	}
	if !strings.HasSuffix(resB.RelPath, "README--project-b.md") {
		t.Errorf("collision name = %q, want README--project-b.md suffix", resB.RelPath)
	}
	if got := env.vaultFile(t, resA.RelPath); got != "# a\n" {
		t.Errorf("first source content = %q", got)
	}
	if got := env.vaultFile(t, resB.RelPath); got != "# b\n" {
		t.Errorf("second source content = %q", got)
	}
}

func TestVaultEditPolicies(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*testEnv, string, string) {
		env := newTestEnv(t)
		dir := t.TempDir()
		src := writeSource(t, dir, "note.md", "# original\n")
		res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
		if err != nil {
			t.Fatal(err)
		}
		// Simulate a phone edit synced back into the vault.
		vaultAbs := filepath.Join(env.vault, filepath.FromSlash(res.RelPath))
		if err := os.WriteFile(vaultAbs, []byte("# phone edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Source changes afterwards.
		writeSource(t, dir, "note.md", "# revised\n")
		return env, src, res.RelPath
	}

	t.Run("skip", func(t *testing.T) {
		env, src, rel := setup(t)
		res, err := ImportFile(ctx, env.deps, src, PolicySkip)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusSkipped {
			t.Errorf("status = %s", res.Status)
		}
		if got := env.vaultFile(t, rel); got != "# phone edit\n" {
			t.Errorf("edited vault copy overwritten: %q", got)
		}
		// The snapshot records the new revision as intent while
		// written_revision_id still describes the stale file.
		q := env.deps.DB.Query()
		srcRow, err := database.GetSourceByPath(ctx, q, src)
		if err != nil || srcRow == nil {
			t.Fatalf("source row: %v", err)
		}
		snap, err := database.GetSnapshot(ctx, q, srcRow.ID, "2026-07-20")
		if err != nil || snap == nil {
			t.Fatalf("snapshot: %v", err)
		}
		vaultID, _ := database.GetVaultIDByKey(ctx, q, env.vault)
		mat, err := database.GetMaterialization(ctx, q, snap.ID, vaultID)
		if err != nil || mat == nil {
			t.Fatalf("materialization: %v", err)
		}
		if mat.WrittenRevisionID == snap.RevisionID {
			t.Error("skip did not leave the materialization marked stale")
		}
	})

	t.Run("preserve", func(t *testing.T) {
		env, src, rel := setup(t)
		res, err := ImportFile(ctx, env.deps, src, PolicyPreserve)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusUpdated {
			t.Errorf("status = %s", res.Status)
		}
		if res.PreservedRel != "_External-Conflicts/2026-07-20/note--vault-edit.md" {
			t.Errorf("preserved path = %q", res.PreservedRel)
		}
		if got := env.vaultFile(t, res.PreservedRel); got != "# phone edit\n" {
			t.Errorf("preserved content = %q", got)
		}
		if got := env.vaultFile(t, rel); got != "# revised\n" {
			t.Errorf("vault copy after preserve = %q", got)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		env, src, rel := setup(t)
		res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusUpdated {
			t.Errorf("status = %s", res.Status)
		}
		if got := env.vaultFile(t, rel); got != "# revised\n" {
			t.Errorf("vault copy after overwrite = %q", got)
		}
	})

	t.Run("edit identical to new source is no conflict", func(t *testing.T) {
		env, src, rel := setup(t)
		// The vault edit happens to match the new source content exactly.
		vaultAbs := filepath.Join(env.vault, filepath.FromSlash(rel))
		if err := os.WriteFile(vaultAbs, []byte("# revised\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := ImportFile(ctx, env.deps, src, PolicySkip)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusUpdated {
			t.Errorf("status = %s, want updated (identical content is not a conflict)", res.Status)
		}
	})
}

func TestDeletedVaultCopyIsRecreated(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "note.md", "# one\n")
	ctx := context.Background()

	res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(env.vault, filepath.FromSlash(res.RelPath))); err != nil {
		t.Fatal(err)
	}
	again, err := ImportFile(ctx, env.deps, src, PolicySkip)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != StatusUpdated {
		t.Errorf("status = %s", again.Status)
	}
	if got := env.vaultFile(t, res.RelPath); got != "# one\n" {
		t.Errorf("recreated content = %q", got)
	}
}

func TestImportRejects(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if _, err := ImportFile(ctx, env.deps, filepath.Join(t.TempDir(), "missing.md"), PolicyOverwrite); err == nil {
		t.Error("missing source accepted")
	}
	txt := writeSource(t, t.TempDir(), "note.txt", "x")
	if _, err := ImportFile(ctx, env.deps, txt, PolicyOverwrite); err == nil {
		t.Error("non-markdown source accepted")
	}
}

func TestImportUnicodeAndSpaces(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "räksmörgås anteckningar.md", "# hej\n")

	res, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if !strings.HasSuffix(res.RelPath, "räksmörgås anteckningar.md") {
		t.Errorf("rel path = %q", res.RelPath)
	}
	if got := env.vaultFile(t, res.RelPath); got != "# hej\n" {
		t.Errorf("content = %q", got)
	}
}

func TestRunImportContinuesPastFailures(t *testing.T) {
	env := newTestEnv(t)
	good := writeSource(t, t.TempDir(), "good.md", "# ok\n")
	missing := filepath.Join(t.TempDir(), "missing.md")

	err := RunImport(context.Background(), env.deps, []string{missing, good})
	if err == nil {
		t.Error("RunImport reported success despite a failure")
	}
	if !strings.Contains(env.out.String(), "imported: ") {
		t.Errorf("good file was not imported; output:\n%s", env.out.String())
	}
}
