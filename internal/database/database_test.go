package database

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationsApplyAndAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	v, err := db.SchemaVersion(ctx)
	if err != nil || v != 4 {
		t.Fatalf("schema version = %d, err %v", v, err)
	}
	db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if v, err = db.SchemaVersion(ctx); err != nil || v != 4 {
		t.Fatalf("schema version after reopen = %d, err %v", v, err)
	}
}

func TestMigrationForgetsInactivePairsWithoutAffectingOtherVaults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	q := db.Query()
	vaultA, _ := EnsureVault(ctx, q, "/vault-a", "a", "/vault-a", "t")
	vaultB, _ := EnsureVault(ctx, q, "/vault-b", "b", "/vault-b", "t")
	layoutID, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, "{}", "t")

	shared, _ := UpsertSource(ctx, q, "/src/shared.md", "/src/shared.md", "t")
	sharedRevision, _ := FindOrCreateRevision(ctx, q, shared, "shared", 1, 1, "t")
	sharedSnapshot, _ := CreateSnapshot(ctx, q, shared, sharedRevision, "2026-07-20", "t")
	CreateMaterialization(ctx, q, sharedSnapshot, vaultA, layoutID, "_External/a/shared.md", sharedRevision, "t")
	CreateMaterialization(ctx, q, sharedSnapshot, vaultB, layoutID, "_External/b/shared.md", sharedRevision, "t")

	aOnly, _ := UpsertSource(ctx, q, "/src/a-only.md", "/src/a-only.md", "t")
	aOnlyRevision, _ := FindOrCreateRevision(ctx, q, aOnly, "a-only", 1, 1, "t")
	aOnlySnapshot, _ := CreateSnapshot(ctx, q, aOnly, aOnlyRevision, "2026-07-20", "t")
	CreateMaterialization(ctx, q, aOnlySnapshot, vaultA, layoutID, "_External/a/only.md", aOnlyRevision, "t")

	activeA, _ := UpsertSource(ctx, q, "/src/active-a.md", "/src/active-a.md", "t")
	activeRevision, _ := FindOrCreateRevision(ctx, q, activeA, "active", 1, 1, "t")
	activeSnapshot, _ := CreateSnapshot(ctx, q, activeA, activeRevision, "2026-07-20", "t")
	CreateMaterialization(ctx, q, activeSnapshot, vaultA, layoutID, "_External/a/active.md", activeRevision, "t")

	if _, err := q.ExecContext(ctx, `CREATE TABLE watch_tracking (
		source_id INTEGER NOT NULL REFERENCES sources(source_id) ON DELETE CASCADE,
		vault_id INTEGER NOT NULL REFERENCES vaults(vault_id) ON DELETE CASCADE,
		active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
		updated_at_utc TEXT NOT NULL,
		PRIMARY KEY (source_id, vault_id)
	)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		sourceID int64
		vaultID  int64
		active   int
	}{
		{shared, vaultA, 0},
		{shared, vaultB, 1},
		{aOnly, vaultA, 0},
		{activeA, vaultA, 1},
	} {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO watch_tracking (source_id, vault_id, active, updated_at_utc) VALUES (?, ?, ?, 't')`,
			row.sourceID, row.vaultID, row.active,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.ExecContext(ctx, `UPDATE metadata SET value = '3' WHERE key = ?`, schemaVersionKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q = db.Query()
	if version, err := db.SchemaVersion(ctx); err != nil || version != 4 {
		t.Fatalf("schema version = %d, err %v", version, err)
	}
	if tracked, err := IsSourceTrackedInVault(ctx, q, shared, vaultA); err != nil || tracked {
		t.Fatalf("shared source remained in vault A: tracked %v, err %v", tracked, err)
	}
	if tracked, err := IsSourceTrackedInVault(ctx, q, shared, vaultB); err != nil || !tracked {
		t.Fatalf("shared source was removed from vault B: tracked %v, err %v", tracked, err)
	}
	if source, err := GetSourceByPath(ctx, q, "/src/shared.md"); err != nil || source == nil {
		t.Fatalf("shared source bookkeeping = %+v, err %v", source, err)
	}
	if source, err := GetSourceByPath(ctx, q, "/src/a-only.md"); err != nil || source != nil {
		t.Fatalf("vault-A-only source survived = %+v, err %v", source, err)
	}
	if tracked, err := IsSourceTrackedInVault(ctx, q, activeA, vaultA); err != nil || !tracked {
		t.Fatalf("active vault-A source was removed: tracked %v, err %v", tracked, err)
	}
	var trackingTable int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'watch_tracking'`,
	).Scan(&trackingTable); err != nil || trackingTable != 0 {
		t.Fatalf("watch_tracking table count = %d, err %v", trackingTable, err)
	}
	assertDatabaseIntegrity(t, q)
}

func TestMigrationFromSchema2RetainsImplicitlyInactiveMaterialization(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	q := db.Query()
	vaultID, _ := EnsureVault(ctx, q, "/vault", "vault", "/vault", "t")
	layoutID, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, "{}", "t")
	sourceID, _ := UpsertSource(ctx, q, "/src/note.md", "/src/note.md", "t")
	revisionID, _ := FindOrCreateRevision(ctx, q, sourceID, "sha", 1, 1, "t")
	snapshotID, _ := CreateSnapshot(ctx, q, sourceID, revisionID, "2026-07-20", "t")
	CreateMaterialization(ctx, q, snapshotID, vaultID, layoutID, "_External/note.md", revisionID, "t")
	if _, err := q.ExecContext(ctx, `CREATE TABLE watch_tracking (
		source_id INTEGER NOT NULL REFERENCES sources(source_id) ON DELETE CASCADE,
		vault_id INTEGER NOT NULL REFERENCES vaults(vault_id) ON DELETE CASCADE,
		active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
		updated_at_utc TEXT NOT NULL,
		PRIMARY KEY (source_id, vault_id)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO watch_tracking (source_id, vault_id, active, updated_at_utc) VALUES (?, ?, 0, 't')`,
		sourceID, vaultID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExecContext(ctx, `UPDATE metadata SET value = '2' WHERE key = ?`, schemaVersionKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if tracked, err := IsSourceTrackedInVault(ctx, db.Query(), sourceID, vaultID); err != nil || !tracked {
		t.Fatalf("schema-v2 implicit inactive materialization was forgotten: tracked %v, err %v", tracked, err)
	}
	assertDatabaseIntegrity(t, db.Query())
}

func TestSourceUniqueness(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	id1, err := UpsertSource(ctx, q, "/a/x.md", "/a/x.md", "2026-07-20T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := UpsertSource(ctx, q, "/a/x.md", "/a/x.md", "2026-07-20T01:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("upsert created a second source: %d vs %d", id1, id2)
	}
	sources, _, _, err := db.Counts(ctx)
	if err != nil || sources != 1 {
		t.Errorf("source count = %d, err %v", sources, err)
	}
}

func TestTouchSourceRequiresExistingIdentity(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	id, err := UpsertSource(ctx, q, "/a/x.md", "/a/x.md", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := TouchSource(ctx, q, id, "/a/x.md", "t2"); err != nil {
		t.Fatalf("TouchSource existing identity: %v", err)
	}
	if err := TouchSource(ctx, q, id, "/a/other.md", "t3"); err == nil {
		t.Fatal("TouchSource accepted a different canonical identity")
	}
	if err := TouchSource(ctx, q, id+100, "/a/x.md", "t3"); err == nil {
		t.Fatal("TouchSource accepted a missing source ID")
	}
	sources, _, _, err := db.Counts(ctx)
	if err != nil || sources != 1 {
		t.Fatalf("source count = %d, err %v", sources, err)
	}
}

func TestRevisionUniquenessPerSourceAndHash(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	srcID, _ := UpsertSource(ctx, q, "/a/x.md", "/a/x.md", "t")
	r1, err := FindOrCreateRevision(ctx, q, srcID, "aaa", 3, 1, "t")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := FindOrCreateRevision(ctx, q, srcID, "aaa", 3, 2, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Errorf("same hash produced two revisions: %d vs %d", r1, r2)
	}
	r3, err := FindOrCreateRevision(ctx, q, srcID, "bbb", 4, 3, "t3")
	if err != nil {
		t.Fatal(err)
	}
	if r3 == r1 {
		t.Error("different hash reused a revision")
	}
	if sha, err := RevisionSHA(ctx, q, r3); err != nil || sha != "bbb" {
		t.Errorf("RevisionSHA = %q, err %v", sha, err)
	}
}

func TestOneSnapshotPerSourcePerDate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	srcID, _ := UpsertSource(ctx, q, "/a/x.md", "/a/x.md", "t")
	revID, _ := FindOrCreateRevision(ctx, q, srcID, "aaa", 3, 1, "t")

	if _, err := CreateSnapshot(ctx, q, srcID, revID, "2026-07-20", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSnapshot(ctx, q, srcID, revID, "2026-07-20", "t"); err == nil {
		t.Error("duplicate (source, date) snapshot accepted")
	}
	if _, err := CreateSnapshot(ctx, q, srcID, revID, "2026-07-21", "t"); err != nil {
		t.Errorf("later-day snapshot rejected: %v", err)
	}
}

func TestSnapshotUpdateAndGet(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	srcID, _ := UpsertSource(ctx, q, "/a/x.md", "/a/x.md", "t")
	r1, _ := FindOrCreateRevision(ctx, q, srcID, "aaa", 3, 1, "t")
	r2, _ := FindOrCreateRevision(ctx, q, srcID, "bbb", 3, 2, "t")
	snapID, _ := CreateSnapshot(ctx, q, srcID, r1, "2026-07-20", "t")

	if err := UpdateSnapshotRevision(ctx, q, snapID, r2, "t2"); err != nil {
		t.Fatal(err)
	}
	snap, err := GetSnapshot(ctx, q, srcID, "2026-07-20")
	if err != nil || snap == nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.RevisionID != r2 {
		t.Errorf("revision = %d, want %d", snap.RevisionID, r2)
	}
	if missing, err := GetSnapshot(ctx, q, srcID, "2000-01-01"); err != nil || missing != nil {
		t.Errorf("GetSnapshot for missing date = %v, err %v", missing, err)
	}
}

func TestMaterializationPathUniquePerVault(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	srcID, _ := UpsertSource(ctx, q, "/a/x.md", "/a/x.md", "t")
	revID, _ := FindOrCreateRevision(ctx, q, srcID, "aaa", 3, 1, "t")
	snap1, _ := CreateSnapshot(ctx, q, srcID, revID, "2026-07-20", "t")
	snap2, _ := CreateSnapshot(ctx, q, srcID, revID, "2026-07-21", "t")
	vaultID, err := EnsureVault(ctx, q, "/vault", "vault", "/vault", "t")
	if err != nil {
		t.Fatal(err)
	}
	layoutID, err := EnsureLayout(ctx, q, "dated-flat-v1", 1, "{}", "t")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := CreateMaterialization(ctx, q, snap1, vaultID, layoutID, "_External/2026-07-20/x.md", revID, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateMaterialization(ctx, q, snap2, vaultID, layoutID, "_External/2026-07-20/x.md", revID, "t"); err == nil {
		t.Error("duplicate relative path in one vault accepted")
	}
	if _, err := CreateMaterialization(ctx, q, snap1, vaultID, layoutID, "_External/other.md", revID, "t"); err == nil {
		t.Error("second materialization for one (snapshot, vault) accepted")
	}

	owned, err := IsPathOwned(ctx, q, vaultID, "_External/2026-07-20/x.md")
	if err != nil || !owned {
		t.Errorf("IsPathOwned = %v, err %v", owned, err)
	}
	free, err := IsPathOwned(ctx, q, vaultID, "_External/2026-07-20/free.md")
	if err != nil || free {
		t.Errorf("IsPathOwned for free path = %v, err %v", free, err)
	}
}

func TestEnsureVaultAndLayoutStable(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	v1, _ := EnsureVault(ctx, q, "/vault", "vault", "/vault", "t")
	v2, _ := EnsureVault(ctx, q, "/vault", "renamed", "/vault", "t2")
	if v1 != v2 {
		t.Errorf("vault key produced two ids: %d vs %d", v1, v2)
	}
	l1, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, `{"a":1}`, "t")
	l2, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, `{"a":2}`, "t2")
	if l1 != l2 {
		t.Errorf("layout name/version produced two ids: %d vs %d", l1, l2)
	}
}

func TestSelectWatchCandidatesAreVaultScoped(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	vaultA, _ := EnsureVault(ctx, q, "/vault-a", "a", "/vault-a", "t")
	vaultB, _ := EnsureVault(ctx, q, "/vault-b", "b", "/vault-b", "t")
	layoutID, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, "{}", "t")

	recent, _ := UpsertSource(ctx, q, "/src/recent.md", "/src/recent.md", "t")
	r19, _ := FindOrCreateRevision(ctx, q, recent, "sha-19", 1, 1, "t")
	r20, _ := FindOrCreateRevision(ctx, q, recent, "sha-20", 1, 2, "t")
	s19, _ := CreateSnapshot(ctx, q, recent, r19, "2026-07-19", "t")
	s20, _ := CreateSnapshot(ctx, q, recent, r20, "2026-07-20", "t")
	CreateMaterialization(ctx, q, s19, vaultA, layoutID, "_External/19.md", r19, "t")
	CreateMaterialization(ctx, q, s20, vaultA, layoutID, "_External/20.md", r20, "t")

	foreign, _ := UpsertSource(ctx, q, "/src/foreign.md", "/src/foreign.md", "t")
	rForeign, _ := FindOrCreateRevision(ctx, q, foreign, "sha-b", 1, 1, "t")
	sForeign, _ := CreateSnapshot(ctx, q, foreign, rForeign, "2026-07-20", "t")
	CreateMaterialization(ctx, q, sForeign, vaultB, layoutID, "_External/b.md", rForeign, "t")

	global, _ := UpsertSource(ctx, q, "/src/global.md", "/src/global.md", "t")
	rGlobal, _ := FindOrCreateRevision(ctx, q, global, "sha-global", 1, 1, "t")
	CreateSnapshot(ctx, q, global, rGlobal, "2026-07-20", "t")

	got, err := SelectWatchCandidates(ctx, q, "/vault-a", "2026-07-19", "2026-07-20")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("selected = %+v, want one vault-a candidate", got)
	}
	if got[0].CanonicalPath != "/src/recent.md" || got[0].SnapshotDate != "2026-07-20" || got[0].ContentSHA != "sha-20" {
		t.Errorf("candidate = %+v", got[0])
	}

	got, err = SelectWatchCandidates(ctx, q, "/vault-b", "2026-07-19", "2026-07-20")
	if err != nil || len(got) != 1 || got[0].CanonicalPath != "/src/foreign.md" {
		t.Errorf("vault-b candidates = %+v, err %v", got, err)
	}

	got, err = SelectWatchCandidates(ctx, q, "/never-registered", "2026-07-19", "2026-07-20")
	if err != nil || len(got) != 0 {
		t.Errorf("missing-vault candidates = %+v, err %v", got, err)
	}
	if id, err := GetVaultIDByKey(ctx, q, "/never-registered"); err != nil || id != 0 {
		t.Errorf("watch candidate query registered vault: id %d, err %v", id, err)
	}
	if _, err := ForgetSourceInVault(ctx, q, recent, "/src/recent.md", vaultA); err != nil {
		t.Fatal(err)
	}
	if got, err := SelectWatchCandidates(ctx, q, "/vault-a", "2026-07-19", "2026-07-20"); err != nil || len(got) != 0 {
		t.Fatalf("forgotten candidate = %+v, err %v", got, err)
	}
}

func TestSelectAllWatchCandidatesUsesLatestMaterializationPerSource(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	vaultA, _ := EnsureVault(ctx, q, "/vault-a", "a", "/vault-a", "t")
	vaultB, _ := EnsureVault(ctx, q, "/vault-b", "b", "/vault-b", "t")
	layoutID, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, "{}", "t")

	recent, _ := UpsertSource(ctx, q, "/src/recent.md", "/src/recent.md", "t")
	r19, _ := FindOrCreateRevision(ctx, q, recent, "sha-19", 1, 1, "t")
	r20, _ := FindOrCreateRevision(ctx, q, recent, "sha-20", 1, 2, "t")
	s19, _ := CreateSnapshot(ctx, q, recent, r19, "2026-07-19", "t")
	s20, _ := CreateSnapshot(ctx, q, recent, r20, "2026-07-20", "t")
	CreateMaterialization(ctx, q, s19, vaultA, layoutID, "_External/19.md", r19, "t")
	CreateMaterialization(ctx, q, s20, vaultA, layoutID, "_External/20.md", r20, "t")

	older, _ := UpsertSource(ctx, q, "/src/older.md", "/src/older.md", "t")
	rOld, _ := FindOrCreateRevision(ctx, q, older, "sha-old", 1, 1, "t")
	sOld, _ := CreateSnapshot(ctx, q, older, rOld, "2025-01-01", "t")
	CreateMaterialization(ctx, q, sOld, vaultA, layoutID, "_External/old.md", rOld, "t")

	foreign, _ := UpsertSource(ctx, q, "/src/foreign.md", "/src/foreign.md", "t")
	rForeign, _ := FindOrCreateRevision(ctx, q, foreign, "sha-foreign", 1, 1, "t")
	sForeign, _ := CreateSnapshot(ctx, q, foreign, rForeign, "2026-07-20", "t")
	CreateMaterialization(ctx, q, sForeign, vaultB, layoutID, "_External/foreign.md", rForeign, "t")

	unmaterialized, _ := UpsertSource(ctx, q, "/src/unmaterialized.md", "/src/unmaterialized.md", "t")
	rUnmaterialized, _ := FindOrCreateRevision(ctx, q, unmaterialized, "sha-none", 1, 1, "t")
	CreateSnapshot(ctx, q, unmaterialized, rUnmaterialized, "2026-07-20", "t")

	got, err := SelectAllWatchCandidates(ctx, q, "/vault-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("selected = %+v, want recent and older vault-a sources", got)
	}
	if got[0].CanonicalPath != "/src/older.md" || got[0].ContentSHA != "sha-old" {
		t.Errorf("older candidate = %+v", got[0])
	}
	if got[1].CanonicalPath != "/src/recent.md" || got[1].SnapshotDate != "2026-07-20" || got[1].ContentSHA != "sha-20" {
		t.Errorf("recent candidate = %+v", got[1])
	}
	if _, err := ForgetSourceInVault(ctx, q, recent, "/src/recent.md", vaultA); err != nil {
		t.Fatal(err)
	}
	if got, err := SelectAllWatchCandidates(ctx, q, "/vault-a"); err != nil || len(got) != 1 || got[0].CanonicalPath != "/src/older.md" {
		t.Fatalf("forgotten all-candidate = %+v, err %v", got, err)
	}
}

func TestTrackingEntriesAndListAreVaultScoped(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	vaultA, _ := EnsureVault(ctx, q, "/vault-a", "a", "/vault-a", "t")
	vaultB, _ := EnsureVault(ctx, q, "/vault-b", "b", "/vault-b", "t")
	layoutID, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, "{}", "t")
	sourceID, _ := UpsertSource(ctx, q, "/src/note.md", "/display/note.md", "t")
	revisionID, _ := FindOrCreateRevision(ctx, q, sourceID, "sha", 1, 1, "t")
	snapshotID, _ := CreateSnapshot(ctx, q, sourceID, revisionID, "2026-07-20", "t")
	CreateMaterialization(ctx, q, snapshotID, vaultA, layoutID, "_External/a.md", revisionID, "t")
	CreateMaterialization(ctx, q, snapshotID, vaultB, layoutID, "_External/b.md", revisionID, "t")

	entries, err := ListTrackingEntries(ctx, q, "/vault-a")
	if err != nil || len(entries) != 1 || entries[0].SnapshotDate != "2026-07-20" {
		t.Fatalf("vault-A tracking entries = %+v, err %v", entries, err)
	}
	entries, err = FindTrackingEntriesByPath(ctx, q, "/vault-a", "/unmatched", "/display/note.md")
	if err != nil || len(entries) != 1 {
		t.Fatalf("display-path lookup = %+v, err %v", entries, err)
	}

	preview, err := PreviewForgetSourceInVault(ctx, q, sourceID, "/src/note.md", vaultA)
	if err != nil {
		t.Fatal(err)
	}
	if preview != (ForgetResult{MaterializationsDeleted: 1}) {
		t.Fatalf("preview = %+v", preview)
	}
	forgotten, err := ForgetSourceInVault(ctx, q, sourceID, "/src/note.md", vaultA)
	if err != nil || forgotten != preview {
		t.Fatalf("forgotten = %+v, preview %+v, err %v", forgotten, preview, err)
	}
	if tracked, err := IsSourceTrackedInVault(ctx, q, sourceID, vaultA); err != nil || tracked {
		t.Fatalf("vault-A tracked = %v, err %v", tracked, err)
	}
	if tracked, err := IsSourceTrackedInVault(ctx, q, sourceID, vaultB); err != nil || !tracked {
		t.Fatalf("vault-B tracked = %v, err %v", tracked, err)
	}
	entries, err = ListTrackingEntries(ctx, q, "/vault-a")
	if err != nil || len(entries) != 0 {
		t.Fatalf("forgotten vault-A entries = %+v, err %v", entries, err)
	}
	entries, err = ListTrackingEntries(ctx, q, "/vault-b")
	if err != nil || len(entries) != 1 {
		t.Fatalf("vault-B tracking changed with vault-A forget = %+v, err %v", entries, err)
	}

	listed, err := ListSources(ctx, q, vaultA)
	if err != nil || len(listed) != 0 {
		t.Fatalf("listed forgotten vault-A source = %+v, err %v", listed, err)
	}
	listed, err = ListSources(ctx, q, vaultB)
	if err != nil || len(listed) != 1 || listed[0].RelativePath != "_External/b.md" {
		t.Fatalf("listed vault-B source = %+v, err %v", listed, err)
	}
	assertDatabaseIntegrity(t, q)
}

func TestForgetSourceInVaultUsesPredicatesAndReportsExactGC(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()
	vaultA, _ := EnsureVault(ctx, q, "/vault-a", "a", "/vault-a", "t")
	vaultB, _ := EnsureVault(ctx, q, "/vault-b", "b", "/vault-b", "t")
	layoutID, _ := EnsureLayout(ctx, q, "dated-flat-v1", 1, "{}", "t")
	sourceID, _ := UpsertSource(ctx, q, "/src/note.md", "/src/note.md", "t")
	r1, _ := FindOrCreateRevision(ctx, q, sourceID, "r1", 1, 1, "t")
	r2, _ := FindOrCreateRevision(ctx, q, sourceID, "r2", 1, 2, "t")
	r3, _ := FindOrCreateRevision(ctx, q, sourceID, "r3", 1, 3, "t")
	s1, _ := CreateSnapshot(ctx, q, sourceID, r1, "2026-07-19", "t")
	s2, _ := CreateSnapshot(ctx, q, sourceID, r2, "2026-07-20", "t")
	s3, _ := CreateSnapshot(ctx, q, sourceID, r3, "2026-07-21", "t")
	CreateMaterialization(ctx, q, s1, vaultA, layoutID, "_External/a/19.md", r1, "t")
	CreateMaterialization(ctx, q, s1, vaultB, layoutID, "_External/b/19.md", r1, "t")
	CreateMaterialization(ctx, q, s2, vaultA, layoutID, "_External/a/20.md", r2, "t")
	CreateMaterialization(ctx, q, s3, vaultB, layoutID, "_External/b/21.md", r3, "t")

	preview, err := PreviewForgetSourceInVault(ctx, q, sourceID, "/src/note.md", vaultA)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ForgetResult{MaterializationsDeleted: 2, SnapshotsDeleted: 1, RevisionsDeleted: 1}); preview != want {
		t.Fatalf("vault-A preview = %+v, want %+v", preview, want)
	}

	// Simulate an import committed after untrack selection/dry-run but before
	// its write transaction. Predicate deletion must include this new row.
	r4, _ := FindOrCreateRevision(ctx, q, sourceID, "r4", 1, 4, "t")
	s4, _ := CreateSnapshot(ctx, q, sourceID, r4, "2026-07-22", "t")
	CreateMaterialization(ctx, q, s4, vaultA, layoutID, "_External/a/22.md", r4, "t")

	forgotten, err := ForgetSourceInVault(ctx, q, sourceID, "/src/note.md", vaultA)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ForgetResult{MaterializationsDeleted: 3, SnapshotsDeleted: 2, RevisionsDeleted: 2}); forgotten != want {
		t.Fatalf("vault-A forgotten = %+v, want %+v", forgotten, want)
	}
	if tracked, err := IsSourceTrackedInVault(ctx, q, sourceID, vaultA); err != nil || tracked {
		t.Fatalf("vault-A tracked = %v, err %v", tracked, err)
	}
	if tracked, err := IsSourceTrackedInVault(ctx, q, sourceID, vaultB); err != nil || !tracked {
		t.Fatalf("vault-B tracked = %v, err %v", tracked, err)
	}
	assertDatabaseIntegrity(t, q)

	preview, err = PreviewForgetSourceInVault(ctx, q, sourceID, "/src/note.md", vaultB)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ForgetResult{MaterializationsDeleted: 2, SnapshotsDeleted: 2, RevisionsDeleted: 2, SourceDeleted: true}); preview != want {
		t.Fatalf("vault-B preview = %+v, want %+v", preview, want)
	}
	forgotten, err = ForgetSourceInVault(ctx, q, sourceID, "/src/note.md", vaultB)
	if err != nil || forgotten != preview {
		t.Fatalf("vault-B forgotten = %+v, preview %+v, err %v", forgotten, preview, err)
	}
	assertDatabaseIntegrity(t, q)
	if sources, snapshots, materializations, err := db.Counts(ctx); err != nil || sources != 0 || snapshots != 0 || materializations != 0 {
		t.Fatalf("counts = %d sources/%d snapshots/%d materializations, err %v", sources, snapshots, materializations, err)
	}
}

func assertDatabaseIntegrity(t *testing.T, q Querier) {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID int64
		var parent string
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("foreign-key violation: table %s row %d parent %s fk %d", table, rowID, parent, fkID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	var orphanSnapshots, orphanRevisions int
	if err := q.QueryRowContext(context.Background(), `
		SELECT
		    (SELECT COUNT(*) FROM snapshots AS sn
		     WHERE NOT EXISTS (SELECT 1 FROM materializations AS m WHERE m.snapshot_id = sn.snapshot_id)),
		    (SELECT COUNT(*) FROM revisions AS r
		     WHERE NOT EXISTS (SELECT 1 FROM snapshots AS sn WHERE sn.revision_id = r.revision_id)
		       AND NOT EXISTS (SELECT 1 FROM materializations AS m WHERE m.written_revision_id = r.revision_id))`,
	).Scan(&orphanSnapshots, &orphanRevisions); err != nil {
		t.Fatal(err)
	}
	if orphanSnapshots != 0 || orphanRevisions != 0 {
		t.Fatalf("post-GC orphans = %d snapshots/%d revisions", orphanSnapshots, orphanRevisions)
	}
}
