package watcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNotifyImportCreatesAndOverwritesSidecar(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state.db")
	if err := NotifyImport(databasePath); err != nil {
		t.Fatalf("first NotifyImport: %v", err)
	}
	path := NotificationPath(databasePath)
	first, err := os.ReadFile(path)
	if err != nil || len(first) == 0 {
		t.Fatalf("read first notification: %q, %v", first, err)
	}
	if err := NotifyImport(databasePath); err != nil {
		t.Fatalf("second NotifyImport: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("notification sidecar was empty after overwrite")
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("notification permissions = %o, want no group/other access", info.Mode().Perm())
	}
}
