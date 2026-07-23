package app

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexzeitgeist/md2obs/internal/config"
	"github.com/alexzeitgeist/md2obs/internal/database"
	"github.com/alexzeitgeist/md2obs/internal/layout"
	"github.com/alexzeitgeist/md2obs/internal/render"
	"github.com/alexzeitgeist/md2obs/internal/source"
	"github.com/alexzeitgeist/md2obs/internal/watcher"
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
	canonical, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
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

func materializationForSource(t *testing.T, env *testEnv, canonical, date string) (*database.Snapshot, *database.Materialization) {
	t.Helper()
	ctx := context.Background()
	q := env.deps.DB.Query()
	src, err := database.GetSourceByPath(ctx, q, canonical)
	if err != nil || src == nil {
		t.Fatalf("source row = %+v, err %v", src, err)
	}
	snap, err := database.GetSnapshot(ctx, q, src.ID, date)
	if err != nil || snap == nil {
		t.Fatalf("snapshot row = %+v, err %v", snap, err)
	}
	vaultID, err := database.GetVaultIDByKey(ctx, q, env.vault)
	if err != nil || vaultID == 0 {
		t.Fatalf("vault ID = %d, err %v", vaultID, err)
	}
	mat, err := database.GetMaterialization(ctx, q, snap.ID, vaultID)
	if err != nil || mat == nil {
		t.Fatalf("materialization row = %+v, err %v", mat, err)
	}
	return snap, mat
}

func TestProvenanceImportSeparatesRawAndWrittenHashes(t *testing.T) {
	env := newTestEnv(t)
	env.deps.Config.ProvenanceFrontmatter = true
	ctx := context.Background()
	raw := []byte("# Project B\n")
	src := writeSource(t, t.TempDir(), "README.md", string(raw))

	res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusImported {
		t.Fatalf("status = %s", res.Status)
	}
	vaultBytes, err := os.ReadFile(filepath.Join(env.vault, filepath.FromSlash(res.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`md2obs_source_path: "` + src + `"`,
		"md2obs_imported_at: " + utc(env.now),
		"# Project B\n",
	} {
		if !strings.Contains(string(vaultBytes), want) {
			t.Errorf("vault bytes do not contain %q:\n%s", want, vaultBytes)
		}
	}
	snap, mat := materializationForSource(t, env, src, "2026-07-20")
	var rawSHA string
	if err := env.deps.DB.Query().QueryRowContext(ctx,
		`SELECT content_sha256 FROM revisions WHERE revision_id = ?`, snap.RevisionID,
	).Scan(&rawSHA); err != nil {
		t.Fatalf("query raw revision hash: %v", err)
	}
	if rawSHA != source.HashBytes(raw) {
		t.Fatalf("raw revision hash = %q, want %q", rawSHA, source.HashBytes(raw))
	}
	if mat.WrittenContentSHA256 != source.HashBytes(vaultBytes) {
		t.Fatalf("written hash = %q, want exact vault hash %q", mat.WrittenContentSHA256, source.HashBytes(vaultBytes))
	}
	if mat.WrittenContentSHA256 == rawSHA {
		t.Fatal("raw and rendered hashes unexpectedly match")
	}
	if mat.WrittenRenderProfile != render.ProvenanceProfile {
		t.Fatalf("written profile = %q", mat.WrittenRenderProfile)
	}

	before, err := os.Stat(filepath.Join(env.vault, filepath.FromSlash(res.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	again, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(env.vault, filepath.FromSlash(res.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != StatusUnchanged {
		t.Fatalf("second status = %s", again.Status)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("unchanged import rewrote vault file: %s -> %s", before.ModTime(), after.ModTime())
	}
}

func TestProvenanceTimestampIsStableWithinDayAndRenewsOnLaterDay(t *testing.T) {
	env := newTestEnv(t)
	env.deps.Config.ProvenanceFrontmatter = true
	ctx := context.Background()
	dir := t.TempDir()
	src := writeSource(t, dir, "note.md", "# one\n")
	firstTime := utc(env.now)

	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	env.setNow(time.Date(2026, 7, 20, 18, 30, 0, 0, time.Local))
	if err := os.WriteFile(src, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if second.RelPath != first.RelPath {
		t.Fatalf("same-day path changed: %q -> %q", first.RelPath, second.RelPath)
	}
	sameDay := env.vaultFile(t, first.RelPath)
	if !strings.Contains(sameDay, "md2obs_imported_at: "+firstTime) {
		t.Fatalf("same-day update changed provenance time:\n%s", sameDay)
	}
	if strings.Contains(sameDay, "md2obs_imported_at: "+utc(env.now)) && utc(env.now) != firstTime {
		t.Fatalf("same-day update used latest rewrite time:\n%s", sameDay)
	}

	env.setNow(time.Date(2026, 7, 21, 1, 15, 0, 0, time.Local))
	third, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if third.Status != StatusImported || third.RelPath == first.RelPath {
		t.Fatalf("later-day result = %+v", third)
	}
	laterDay := env.vaultFile(t, third.RelPath)
	if !strings.Contains(laterDay, "md2obs_imported_at: "+utc(env.now)) {
		t.Fatalf("later-day snapshot did not get its own timestamp:\n%s", laterDay)
	}
}

func TestSameDayProfileOnlyConfirmationDoesNotRewrite(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "profile.md", "# same bytes\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeMat := materializationForSource(t, env, src, "2026-07-20")
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(first.RelPath))
	beforeInfo, err := os.Stat(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.deps.DB.Query().ExecContext(ctx, `
		UPDATE materializations SET written_render_profile = ?
		WHERE materialization_id = ?`,
		render.ProvenanceProfile, beforeMat.ID,
	); err != nil {
		t.Fatal(err)
	}

	res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUnchanged {
		t.Fatalf("status = %s", res.Status)
	}
	afterInfo, err := os.Stat(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("profile-only confirmation changed file mtime: %s -> %s", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	_, afterMat := materializationForSource(t, env, src, "2026-07-20")
	if afterMat.WrittenRenderProfile != render.SourceProfile {
		t.Fatalf("profile = %q, want %q", afterMat.WrittenRenderProfile, render.SourceProfile)
	}
	if afterMat.WrittenAtUTC != beforeMat.WrittenAtUTC {
		t.Fatalf("written_at_utc changed from %q to %q", beforeMat.WrittenAtUTC, afterMat.WrittenAtUTC)
	}
}

func TestSchema4UpgradeAndDisabledImportRemainByteIdentical(t *testing.T) {
	ctx := context.Background()
	vault := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	cfg := &config.Config{
		VaultPath:     vault,
		Layout:        config.DefaultLayout,
		RootDirectory: config.DefaultRootDirectory,
		StateDBPath:   statePath,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	raw := []byte{0xef, 0xbb, 0xbf, '#', ' ', 'l', 'e', 'g', 'a', 'c', 'y', '\r', '\n'}
	src := filepath.Join(sourceDir, "legacy.md")
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := filepath.EvalSymlinks(src)
	if err != nil {
		t.Fatal(err)
	}
	rel := "_External/2026-07-20/legacy.md"
	vaultAbs := filepath.Join(vault, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(vaultAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultAbs, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	fixedMtime := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(vaultAbs, fixedMtime, fixedMtime); err != nil {
		t.Fatal(err)
	}

	sq, err := sql.Open("sqlite", "file:"+url.PathEscape(statePath)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.ReadFile("../database/migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sq.ExecContext(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`
		CREATE TABLE metadata (
		    key TEXT PRIMARY KEY,
		    value TEXT NOT NULL
		)`,
		`INSERT INTO metadata (key, value) VALUES ('schema_version', '4')`,
		`
		INSERT INTO layouts
		    (layout_id, layout_name, layout_version, configuration_json, created_at_utc)
		    VALUES (1, 'dated-flat-v1', 1, '{}', '2026-07-20T08:00:00Z')`,
	} {
		if _, err := sq.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sq.ExecContext(ctx, `
		INSERT INTO vaults
		    (vault_id, vault_key, display_name, local_root_path, registered_at_utc)
		    VALUES (1, ?, 'vault', ?, '2026-07-20T08:00:00Z')`,
		cfg.VaultAbs,
		cfg.VaultAbs,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.ExecContext(ctx, `
		INSERT INTO sources
		    (source_id, canonical_path, display_path, first_seen_at_utc, last_seen_at_utc)
		    VALUES (1, ?, ?, '2026-07-20T08:00:00Z', '2026-07-20T08:00:00Z')`,
		src,
		src,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.ExecContext(ctx, `
		INSERT INTO revisions
		    (revision_id, source_id, content_sha256, byte_size, source_mtime_ns, observed_at_utc)
		    VALUES (1, 1, ?, ?, 1, '2026-07-20T08:00:00Z')`,
		source.HashBytes(raw),
		len(raw),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.ExecContext(ctx, `
		INSERT INTO snapshots
		    (snapshot_id, source_id, revision_id, snapshot_date, created_at_utc, updated_at_utc)
		    VALUES (1, 1, 1, '2026-07-20', '2026-07-20T08:00:00Z', '2026-07-20T08:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.ExecContext(ctx, `
		INSERT INTO materializations
		    (materialization_id, snapshot_id, vault_id, layout_id, relative_path, written_revision_id, written_at_utc)
		    VALUES (1, 1, 1, 1, ?, 1, '2026-07-20T08:00:00Z')`,
		rel,
	); err != nil {
		t.Fatal(err)
	}
	if err := sq.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(ctx, cfg.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	legacySource, err := database.GetSourceByPath(ctx, db.Query(), src)
	if err != nil || legacySource == nil {
		t.Fatalf("schema-v4 source after migration = %+v, err %v", legacySource, err)
	}
	legacySnapshot, err := database.GetSnapshot(ctx, db.Query(), legacySource.ID, "2026-07-20")
	if err != nil || legacySnapshot == nil {
		t.Fatalf("schema-v4 snapshot after migration = %+v, err %v", legacySnapshot, err)
	}
	out := &syncBuffer{}
	deps := &Deps{
		DB:     db,
		Config: cfg,
		Layout: layout.NewDatedFlatV1(),
		Now: func() time.Time {
			return time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local)
		},
		Out: out,
		Err: out,
	}

	res, err := ImportFile(ctx, deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUnchanged {
		t.Fatalf("post-upgrade import status = %s", res.Status)
	}
	info, err := os.Stat(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) || !info.ModTime().Equal(fixedMtime) {
		t.Fatalf("upgrade/import changed raw vault copy: bytes=%q mtime=%s", got, info.ModTime())
	}
	mat, err := database.GetMaterialization(ctx, db.Query(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if mat.WrittenContentSHA256 != source.HashBytes(raw) || mat.WrittenRenderProfile != render.SourceProfile {
		t.Fatalf("backfilled materialization = %+v", mat)
	}

	if err := RunRefresh(ctx, deps, RefreshOptions{Days: 1, OnVaultChange: PolicySkip}); err != nil {
		t.Fatal(err)
	}
	afterRefresh, err := os.Stat(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(vaultAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) || !afterRefresh.ModTime().Equal(fixedMtime) {
		t.Fatalf("ordinary refresh changed upgraded raw copy: bytes=%q mtime=%s", got, afterRefresh.ModTime())
	}
	if bytes.Contains(got, []byte("md2obs_")) {
		t.Fatalf("disabled compatibility copy gained provenance:\n%s", got)
	}
	if !strings.Contains(out.String(), "1 unchanged") {
		t.Fatalf("refresh did not count upgraded source unchanged:\n%s", out.String())
	}
}

func TestImportPreservesUnownedVaultFiles(t *testing.T) {
	env := newTestEnv(t)
	dateDir := filepath.Join(env.vault, "_External", "2026-07-20")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"README.md":   "# manual\n",
		"README-1.md": "# restored\n",
	} {
		if err := os.WriteFile(filepath.Join(dateDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := writeSource(t, t.TempDir(), "README.md", "# source\n")

	res, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if want := "_External/2026-07-20/README-2.md"; res.RelPath != want {
		t.Fatalf("relative path = %q, want %q", res.RelPath, want)
	}
	if got := env.vaultFile(t, "_External/2026-07-20/README.md"); got != "# manual\n" {
		t.Fatalf("unowned vault file changed to %q", got)
	}
	if got := env.vaultFile(t, "_External/2026-07-20/README-1.md"); got != "# restored\n" {
		t.Fatalf("numbered vault file changed to %q", got)
	}
	if got := env.vaultFile(t, res.RelPath); got != "# source\n" {
		t.Fatalf("imported content = %q", got)
	}
}

func TestImportPreservesUnownedVaultSymlink(t *testing.T) {
	env := newTestEnv(t)
	dateDir := filepath.Join(env.vault, "_External", "2026-07-20")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "manual.md")
	if err := os.WriteFile(target, []byte("# manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(dateDir, "note.md")
	if err := os.Symlink(target, occupied); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	src := writeSource(t, t.TempDir(), "note.md", "# source\n")

	res, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if want := "_External/2026-07-20/note-1.md"; res.RelPath != want {
		t.Fatalf("relative path = %q, want %q", res.RelPath, want)
	}
	info, err := os.Lstat(occupied)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("unowned vault symlink was replaced")
	}
	if got := env.vaultFile(t, res.RelPath); got != "# source\n" {
		t.Fatalf("imported content = %q", got)
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

func TestExplicitImportRestoresEditedVaultCopyWhenSourceUnchanged(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "note.md", "# source\n")
	ctx := context.Background()

	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	vaultAbs := filepath.Join(env.vault, filepath.FromSlash(first.RelPath))
	if err := os.WriteFile(vaultAbs, []byte("# phone edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	again, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != StatusUpdated {
		t.Fatalf("status = %s, want updated", again.Status)
	}
	if got := env.vaultFile(t, first.RelPath); got != "# source\n" {
		t.Fatalf("vault content = %q, want source content", got)
	}
}

func TestVaultEditWithUnchangedSourceHonorsWatchPolicy(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []Policy{PolicySkip, PolicyPreserve} {
		t.Run(string(policy), func(t *testing.T) {
			env := newTestEnv(t)
			src := writeSource(t, t.TempDir(), "note.md", "# source\n")
			first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
			if err != nil {
				t.Fatal(err)
			}
			vaultAbs := filepath.Join(env.vault, filepath.FromSlash(first.RelPath))
			if err := os.WriteFile(vaultAbs, []byte("# phone edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			result, err := ImportFile(ctx, env.deps, src, policy)
			if err != nil {
				t.Fatal(err)
			}
			switch policy {
			case PolicySkip:
				if result.Status != StatusSkipped || env.vaultFile(t, first.RelPath) != "# phone edit\n" {
					t.Fatalf("skip result = %+v, content = %q", result, env.vaultFile(t, first.RelPath))
				}
			case PolicyPreserve:
				if result.Status != StatusUpdated || result.PreservedRel == "" {
					t.Fatalf("preserve result = %+v", result)
				}
				if got := env.vaultFile(t, result.PreservedRel); got != "# phone edit\n" {
					t.Fatalf("preserved content = %q", got)
				}
				if got := env.vaultFile(t, first.RelPath); got != "# source\n" {
					t.Fatalf("restored content = %q", got)
				}
			}
		})
	}
}

func TestVaultReadErrorsPreserveOverwriteVersusNonOverwriteBehavior(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []Policy{PolicySkip, PolicyPreserve, PolicyOverwrite} {
		t.Run(string(policy), func(t *testing.T) {
			env := newTestEnv(t)
			src := writeSource(t, t.TempDir(), "read-error.md", "# source\n")
			first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
			if err != nil {
				t.Fatal(err)
			}
			vaultAbs := filepath.Join(env.vault, filepath.FromSlash(first.RelPath))
			if err := os.Remove(vaultAbs); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(vaultAbs, 0o755); err != nil {
				t.Fatal(err)
			}

			_, err = ImportFile(ctx, env.deps, src, policy)
			if err == nil {
				t.Fatal("directory at materialization path unexpectedly imported")
			}
			if policy == PolicyOverwrite {
				if strings.Contains(err.Error(), "read vault copy") {
					t.Fatalf("overwrite stopped at the read error instead of attempting atomic replacement: %v", err)
				}
			} else if !strings.Contains(err.Error(), "read vault copy") {
				t.Fatalf("%s error = %v, want vault-read failure", policy, err)
			}
		})
	}
}

func TestProvenanceImportToleratesInvalidFrontmatterAndSupersedesReservedKeys(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid mapping candidate is body", func(t *testing.T) {
		env := newTestEnv(t)
		env.deps.Config.ProvenanceFrontmatter = true
		raw := "---\nkey: [broken\n---\nbody\n"
		src := writeSource(t, t.TempDir(), "invalid.md", raw)
		res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
		if err != nil {
			t.Fatal(err)
		}
		got := env.vaultFile(t, res.RelPath)
		if !strings.Contains(got, raw) || !strings.HasPrefix(got, "---\nmd2obs_source_path:") {
			t.Fatalf("invalid candidate was not retained as body:\n%s", got)
		}
	})

	t.Run("reserved values are authoritative", func(t *testing.T) {
		env := newTestEnv(t)
		env.deps.Config.ProvenanceFrontmatter = true
		raw := "---\n" +
			"md2obs_source_path: false-origin-a\n" +
			"md2obs_imported_at: 2000-01-01T00:00:00Z\n" +
			"md2obs_source_path: false-origin-b\n" +
			"title: Keep\n" +
			"---\nbody\n"
		src := writeSource(t, t.TempDir(), "reserved.md", raw)
		res, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
		if err != nil {
			t.Fatal(err)
		}
		got := env.vaultFile(t, res.RelPath)
		if strings.Count(got, "md2obs_source_path:") != 1 ||
			strings.Count(got, "md2obs_imported_at:") != 1 ||
			!strings.Contains(got, `md2obs_source_path: "`+src+`"`) ||
			!strings.Contains(got, "title: Keep") {
			t.Fatalf("managed properties did not converge authoritatively:\n%s", got)
		}
	})
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

func TestCollisionBetweenLongSameNameSources(t *testing.T) {
	env := newTestEnv(t)
	base := t.TempDir()
	dirA := filepath.Join(base, "p1")
	dirB := filepath.Join(base, "p2")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("a", 249) + ".md"
	srcA := writeSource(t, dirA, name, "# a\n")
	srcB := writeSource(t, dirB, name, "# b\n")

	first, err := ImportFile(context.Background(), env.deps, srcA, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ImportFile(context.Background(), env.deps, srcB, PolicyOverwrite)
	if err != nil {
		t.Fatalf("second long-name import: %v", err)
	}
	if first.RelPath == second.RelPath {
		t.Fatal("long-name collision reused the first path")
	}
	if got := len(filepath.Base(second.RelPath)); got > 255 {
		t.Fatalf("allocated filename is %d bytes", got)
	}
	if got := env.vaultFile(t, second.RelPath); got != "# b\n" {
		t.Fatalf("second content = %q", got)
	}
}

func TestConcurrentCollisionAllocation(t *testing.T) {
	env := newTestEnv(t)
	base := t.TempDir()
	paths := make([]string, 2)
	for i, dirName := range []string{"left", "right"} {
		dir := filepath.Join(base, dirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		paths[i] = writeSource(t, dir, "README.md", "# "+dirName+"\n")
	}

	type outcome struct {
		result Result
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(paths))
	for _, sourcePath := range paths {
		go func(p string) {
			<-start
			result, err := ImportFile(context.Background(), env.deps, p, PolicyOverwrite)
			outcomes <- outcome{result: result, err: err}
		}(sourcePath)
	}
	close(start)

	var results []Result
	for range paths {
		out := <-outcomes
		if out.err != nil {
			t.Fatal(out.err)
		}
		results = append(results, out.result)
	}
	if results[0].RelPath == results[1].RelPath {
		t.Fatalf("concurrent imports allocated %q twice", results[0].RelPath)
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
		// written_revision_id still identifies md2obs's last recorded write.
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

func TestWatchedImportRejectsRetargetedSourceSymlink(t *testing.T) {
	env := newTestEnv(t)
	dir := t.TempDir()
	src := writeSource(t, dir, "source.md", "# registered\n")
	ctx := context.Background()

	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	registered, err := database.GetSourceByPath(ctx, env.deps.DB.Query(), src)
	if err != nil || registered == nil {
		t.Fatalf("registered source: %v", err)
	}
	other := writeSource(t, dir, "other.md", "# unrelated\n")
	if err := os.Rename(src, filepath.Join(dir, "source.old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, src); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := ImportWatchedSource(ctx, env.deps, *registered, PolicySkip); err == nil || !strings.HasPrefix(err.Error(), "source identity changed:") {
		t.Fatalf("watched import error = %v, want identity-change rejection", err)
	}
	sources, snapshots, materializations, err := env.deps.DB.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sources != 1 || snapshots != 1 || materializations != 1 {
		t.Fatalf("counts after rejected retarget = %d/%d/%d", sources, snapshots, materializations)
	}
}

func TestFailedMaterializationRollsBackDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory semantics differ on Windows")
	}
	env := newTestEnv(t)
	dateDir := filepath.Join(env.vault, "_External", "2026-07-20")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dateDir, 0o755) })
	src := writeSource(t, t.TempDir(), "note.md", "# source\n")

	if _, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite); err == nil {
		t.Skip("filesystem permitted a write to the read-only test directory")
	}
	sources, snapshots, materializations, err := env.deps.DB.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sources != 0 || snapshots != 0 || materializations != 0 {
		t.Fatalf("failed import left database rows: %d/%d/%d", sources, snapshots, materializations)
	}
}

func TestImportRejectsDateDirectorySymlinkEscape(t *testing.T) {
	env := newTestEnv(t)
	outside := t.TempDir()
	root := filepath.Join(env.vault, "_External")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "2026-07-20")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	src := writeSource(t, t.TempDir(), "note.md", "# source\n")

	if _, err := ImportFile(context.Background(), env.deps, src, PolicyOverwrite); err == nil || !strings.Contains(err.Error(), "outside the vault") {
		t.Fatalf("import error = %v, want symlink-escape rejection", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("import wrote outside the vault: %v", entries)
	}
	sources, snapshots, materializations, err := env.deps.DB.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sources != 0 || snapshots != 0 || materializations != 0 {
		t.Fatalf("rejected import left database rows: %d/%d/%d", sources, snapshots, materializations)
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
	if !strings.Contains(env.out.String(), "error: import ") {
		t.Errorf("failed file was not reported as user-facing output:\n%s", env.out.String())
	}
	if !strings.Contains(env.out.String(), "imported: ") {
		t.Errorf("good file was not imported; output:\n%s", env.out.String())
	}
}

func TestRunImportNotifiesWatchersAfterSuccess(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "notify.md", "# notify\n")

	if err := RunImport(context.Background(), env.deps, []string{src}); err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	path := watcher.NotificationPath(env.deps.DB.Path)
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("notification sidecar = %q, err %v", data, err)
	}
}

func TestRunImportNotificationFailureIsWarning(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "notify-warning.md", "# notify\n")
	// The live SQL connection remains usable, but this synthetic public path
	// cannot host the sidecar.
	env.deps.DB.Path = filepath.Join(t.TempDir(), "missing", "state.db")

	if err := RunImport(context.Background(), env.deps, []string{src}); err != nil {
		t.Fatalf("notification failure changed import result: %v", err)
	}
	if !strings.Contains(env.out.String(), "warning: import succeeded, but running watchers may need to be restarted") {
		t.Fatalf("missing notification warning; output:\n%s", env.out.String())
	}
	if !strings.Contains(env.out.String(), "imported: ") {
		t.Fatalf("successful import result missing; output:\n%s", env.out.String())
	}
}

func TestQueryCommands(t *testing.T) {
	env := newTestEnv(t)
	src := writeSource(t, t.TempDir(), "query.md", "# query\n")
	ctx := context.Background()
	if _, err := ImportFile(ctx, env.deps, src, PolicyOverwrite); err != nil {
		t.Fatal(err)
	}
	if err := RunList(ctx, env.deps); err != nil {
		t.Fatal(err)
	}
	if err := RunHistory(ctx, env.deps, src); err != nil {
		t.Fatal(err)
	}
	if err := RunStatus(ctx, env.deps); err != nil {
		t.Fatal(err)
	}
	output := env.out.String()
	for _, want := range []string{src, "last snapshot:  2026-07-20", "Source: ", "Schema version:    5", "Sources:           1"} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
}

func TestRunListReportsSourceAndRenderingStateIndependently(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sourceStale    bool
		renderingStale bool
	}{
		{name: "both current"},
		{name: "source current rendering stale", renderingStale: true},
		{name: "source stale rendering current", sourceStale: true},
		{name: "both stale", sourceStale: true, renderingStale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			ctx := context.Background()
			dir := t.TempDir()
			src := writeSource(t, dir, "states.md", "# one\n")
			first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
			if err != nil {
				t.Fatal(err)
			}
			if tc.sourceStale {
				if err := os.WriteFile(
					filepath.Join(env.vault, filepath.FromSlash(first.RelPath)),
					[]byte("# phone edit\n"),
					0o644,
				); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(src, []byte("# two\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				res, err := ImportFile(ctx, env.deps, src, PolicySkip)
				if err != nil {
					t.Fatal(err)
				}
				if res.Status != StatusSkipped {
					t.Fatalf("setup status = %s", res.Status)
				}
			}
			if tc.renderingStale {
				env.deps.Config.ProvenanceFrontmatter = true
			}

			if err := RunList(ctx, env.deps); err != nil {
				t.Fatal(err)
			}
			wantSource := "current"
			if tc.sourceStale {
				wantSource = "stale"
			}
			wantRendering := "current"
			if tc.renderingStale {
				wantRendering = "stale"
			}
			output := env.out.String()
			if !strings.Contains(output, "  source content: "+wantSource) ||
				!strings.Contains(output, "  rendering:      "+wantRendering) {
				t.Fatalf("debug list state mismatch:\n%s", output)
			}
		})
	}
}

func TestRunListIsDatabaseOnlyAndDoesNotDetectPhoneEdit(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	src := writeSource(t, t.TempDir(), "database-only.md", "# raw\n")
	first, err := ImportFile(ctx, env.deps, src, PolicyOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(env.vault, filepath.FromSlash(first.RelPath)),
		[]byte("# unrecorded phone edit\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	if err := RunList(ctx, env.deps); err != nil {
		t.Fatal(err)
	}
	output := env.out.String()
	wantFields := "  last snapshot:  2026-07-20\n" +
		"  vault path:     " + first.RelPath + "\n" +
		"  source content: current\n" +
		"  rendering:      current\n"
	if !strings.Contains(output, wantFields) {
		t.Fatalf("debug list performed or implied a live check:\n%s", output)
	}
}

func TestRunStatusReportsResolvedProvenanceMode(t *testing.T) {
	env := newTestEnv(t)
	env.deps.Config.ProvenanceFrontmatter = true
	if err := RunStatus(context.Background(), env.deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env.out.String(), "Provenance:        enabled") {
		t.Fatalf("status did not report enabled provenance:\n%s", env.out.String())
	}
}
