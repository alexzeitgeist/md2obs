package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		VaultPath:     t.TempDir(),
		Layout:        DefaultLayout,
		RootDirectory: DefaultRootDirectory,
		StateDBPath:   filepath.Join(t.TempDir(), "state.db"),
	}
}

func TestValidateHappyPath(t *testing.T) {
	cfg := validConfig(t)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.VaultAbs == "" || !filepath.IsAbs(cfg.VaultAbs) {
		t.Errorf("VaultAbs = %q", cfg.VaultAbs)
	}
	want := filepath.Join(cfg.VaultAbs, "_External")
	if got := cfg.DestRootAbs(); got != want {
		t.Errorf("DestRootAbs = %q, want %q", got, want)
	}
}

func TestValidateRejects(t *testing.T) {
	vault := t.TempDir()
	notDir := filepath.Join(vault, "file.txt")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"missing vault", func(c *Config) { c.VaultPath = "" }, "no vault configured"},
		{"nonexistent vault", func(c *Config) { c.VaultPath = filepath.Join(vault, "nope") }, "does not exist"},
		{"vault not directory", func(c *Config) { c.VaultPath = notDir }, "not a directory"},
		{"unknown layout", func(c *Config) { c.Layout = "other" }, "unknown layout"},
		{"root is vault root", func(c *Config) { c.RootDirectory = "." }, "vault root"},
		{"root absolute", func(c *Config) { c.RootDirectory = "/abs" }, "must be relative"},
		{"root escapes", func(c *Config) { c.RootDirectory = "../out" }, "escapes"},
		{"root hidden", func(c *Config) { c.RootDirectory = ".hidden" }, "hidden"},
		{"root hidden first component", func(c *Config) { c.RootDirectory = ".hidden/ext" }, "hidden"},
		{"root hidden intermediate component", func(c *Config) { c.RootDirectory = "visible/.hidden/ext" }, "hidden"},
		{"db inside vault", func(c *Config) { c.StateDBPath = filepath.Join(c.VaultPath, "state.db") }, "outside the vault"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate accepted invalid config")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateRejectsDestinationSymlinkOutsideVault(t *testing.T) {
	vault := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(vault, DefaultRootDirectory)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := &Config{
		VaultPath:     vault,
		Layout:        DefaultLayout,
		RootDirectory: DefaultRootDirectory,
		StateDBPath:   filepath.Join(t.TempDir(), "state.db"),
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "destination root") {
		t.Fatalf("Validate error = %v, want destination-root rejection", err)
	}
}

func TestValidateRejectsDatabaseThroughSymlinkIntoVault(t *testing.T) {
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	dbDir := filepath.Join(vault, "database")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbLink := filepath.Join(base, "database-link")
	if err := os.Symlink(dbDir, dbLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := &Config{
		VaultPath:     vault,
		Layout:        DefaultLayout,
		RootDirectory: DefaultRootDirectory,
		StateDBPath:   filepath.Join(dbLink, "state.db"),
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside the vault") {
		t.Fatalf("Validate error = %v, want database containment rejection", err)
	}
}

func TestValidateRejectsDanglingDatabaseSymlinkIntoVault(t *testing.T) {
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	dbLink := filepath.Join(base, "state-link.db")
	if err := os.Symlink(filepath.Join(vault, "not-created.db"), dbLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := &Config{
		VaultPath:     vault,
		Layout:        DefaultLayout,
		RootDirectory: DefaultRootDirectory,
		StateDBPath:   dbLink,
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside the vault") {
		t.Fatalf("Validate error = %v, want dangling database-symlink rejection", err)
	}
}

func TestValidateCanonicalizesDatabasePathAliases(t *testing.T) {
	base := t.TempDir()
	vault := filepath.Join(base, "vault")
	stateDir := filepath.Join(base, "state")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingDB := filepath.Join(stateDir, "existing.db")
	if err := os.WriteFile(existingDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fileAlias := filepath.Join(base, "database-link.db")
	if err := os.Symlink(existingDB, fileAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	directoryAlias := filepath.Join(base, "state-link")
	if err := os.Symlink(stateDir, directoryAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolvedExisting, err := filepath.EvalSymlinks(existingDB)
	if err != nil {
		t.Fatal(err)
	}
	resolvedStateDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "existing file symlink", path: fileAlias, want: resolvedExisting},
		{name: "missing file below directory symlink", path: filepath.Join(directoryAlias, "future.db"), want: filepath.Join(resolvedStateDir, "future.db")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				VaultPath:     vault,
				Layout:        DefaultLayout,
				RootDirectory: DefaultRootDirectory,
				StateDBPath:   tc.path,
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.StateDBPath != tc.want {
				t.Fatalf("StateDBPath = %q, want canonical identity %q", cfg.StateDBPath, tc.want)
			}
		})
	}
}

func TestLoadFromFileAndEnv(t *testing.T) {
	confHome := t.TempDir()
	vault := t.TempDir()
	dbDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", confHome)
	t.Setenv("HOME", confHome)
	t.Setenv("MD2OBS_STATE_DB", filepath.Join(dbDir, "state.db"))
	t.Setenv("MD2OBS_VAULT", "")

	configPath, err := DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(configPath); got != "config.json" {
		t.Fatalf("config filename = %q, want config.json", got)
	}
	if got := filepath.Base(filepath.Dir(configPath)); got != "md2obs" {
		t.Fatalf("config directory = %q, want md2obs", got)
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{
		"vault_path":     vault,
		"layout":         DefaultLayout,
		"root_directory": "Imports",
	})
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RootDirectory != "Imports" {
		t.Errorf("RootDirectory = %q", cfg.RootDirectory)
	}
	resolvedDBDir, err := filepath.EvalSymlinks(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedDBDir, "state.db"); cfg.StateDBPath != want {
		t.Errorf("StateDBPath = %q, want %q", cfg.StateDBPath, want)
	}

	// MD2OBS_VAULT overrides the file.
	vault2 := t.TempDir()
	t.Setenv("MD2OBS_VAULT", vault2)
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with MD2OBS_VAULT: %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(vault2)
	if cfg.VaultAbs != resolved {
		t.Errorf("VaultAbs = %q, want %q", cfg.VaultAbs, resolved)
	}
}

func TestLoadWithoutConfigFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MD2OBS_VAULT", t.TempDir())
	t.Setenv("MD2OBS_STATE_DB", filepath.Join(t.TempDir(), "state.db"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RootDirectory != DefaultRootDirectory || cfg.Layout != DefaultLayout {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.ConfigPath != "" {
		t.Errorf("ConfigPath = %q for missing file", cfg.ConfigPath)
	}
}
