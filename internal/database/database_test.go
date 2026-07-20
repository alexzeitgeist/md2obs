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
	if err != nil || v != 1 {
		t.Fatalf("schema version = %d, err %v", v, err)
	}
	db.Close()

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if v, err = db.SchemaVersion(ctx); err != nil || v != 1 {
		t.Fatalf("schema version after reopen = %d, err %v", v, err)
	}
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

func TestSelectSourcesWithSnapshotsBetween(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	q := db.Query()

	old, _ := UpsertSource(ctx, q, "/a/old.md", "/a/old.md", "t")
	recent, _ := UpsertSource(ctx, q, "/a/recent.md", "/a/recent.md", "t")
	rOld, _ := FindOrCreateRevision(ctx, q, old, "aaa", 1, 1, "t")
	rNew, _ := FindOrCreateRevision(ctx, q, recent, "bbb", 1, 1, "t")
	CreateSnapshot(ctx, q, old, rOld, "2026-07-10", "t")
	CreateSnapshot(ctx, q, recent, rNew, "2026-07-19", "t")
	CreateSnapshot(ctx, q, recent, rNew, "2026-07-20", "t")

	got, err := SelectSourcesWithSnapshotsBetween(ctx, q, "2026-07-19", "2026-07-20")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CanonicalPath != "/a/recent.md" {
		t.Errorf("selected = %+v", got)
	}

	got, err = SelectSourcesWithSnapshotsBetween(ctx, q, "2026-07-01", "2026-07-20")
	if err != nil || len(got) != 2 {
		t.Errorf("wide range selected %d sources, err %v", len(got), err)
	}
}
