// Package config loads and validates the small md2obs configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"md2obs/internal/safepath"
)

// Config is the resolved, validated configuration for one invocation.
type Config struct {
	// VaultPath is the configured Obsidian vault root as given.
	VaultPath string `json:"vault_path"`
	// Layout names the destination layout. Only dated-flat-v1 exists today.
	Layout string `json:"layout"`
	// RootDirectory is the vault-relative destination root, e.g. "_External".
	RootDirectory string `json:"root_directory"`

	// VaultAbs is VaultPath made absolute with symlinks resolved. It is the
	// stable vault identity (vault_key) and the base for all vault writes.
	VaultAbs string `json:"-"`
	// StateDBPath is the resolved SQLite database location.
	StateDBPath string `json:"-"`
	// ConfigPath is the file the configuration was read from, if any.
	ConfigPath string `json:"-"`
}

const (
	DefaultLayout        = "dated-flat-v1"
	DefaultRootDirectory = "_External"
)

// DefaultConfigPath returns the platform config file location, e.g.
// ~/.config/md2obs/config.json on Linux.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("determine user config directory: %w", err)
	}
	return filepath.Join(dir, "md2obs", "config.json"), nil
}

// DefaultStateDBPath returns the platform data location for the SQLite
// database, e.g. ~/.local/share/md2obs/state.db on Linux.
func DefaultStateDBPath() (string, error) {
	dir, err := userDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "md2obs", "state.db"), nil
}

func userDataDir() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support"), nil
	case "windows":
		dir := os.Getenv("LOCALAPPDATA")
		if dir == "" {
			return "", errors.New("%LOCALAPPDATA% is not set")
		}
		return dir, nil
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
			return dir, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share"), nil
	}
}

// Load reads the config file when present, applies MD2OBS_VAULT and
// MD2OBS_STATE_DB overrides, resolves defaults, and validates the result.
func Load() (*Config, error) {
	cfg := &Config{
		Layout:        DefaultLayout,
		RootDirectory: DefaultRootDirectory,
	}

	configPath, err := DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(configPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", configPath, err)
		}
		cfg.ConfigPath = configPath
	case errors.Is(err, os.ErrNotExist):
		// No config file; environment variables may still provide the vault.
	default:
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}

	if v := os.Getenv("MD2OBS_VAULT"); v != "" {
		cfg.VaultPath = v
	}
	if cfg.Layout == "" {
		cfg.Layout = DefaultLayout
	}
	if cfg.RootDirectory == "" {
		cfg.RootDirectory = DefaultRootDirectory
	}

	if v := os.Getenv("MD2OBS_STATE_DB"); v != "" {
		cfg.StateDBPath = v
	} else {
		cfg.StateDBPath, err = DefaultStateDBPath()
		if err != nil {
			return nil, err
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the vault, destination root, and database location, and
// fills in the resolved absolute paths.
func (c *Config) Validate() error {
	if c.VaultPath == "" {
		return errors.New("no vault configured: set vault_path in the config file or MD2OBS_VAULT")
	}
	abs, err := filepath.Abs(c.VaultPath)
	if err != nil {
		return fmt.Errorf("resolve vault path %s: %w", c.VaultPath, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("vault %s does not exist or cannot be resolved: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat vault %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("vault %s is not a directory", resolved)
	}
	c.VaultAbs = resolved

	if c.Layout != DefaultLayout {
		return fmt.Errorf("unknown layout %q (only %q is supported)", c.Layout, DefaultLayout)
	}

	root := path.Clean(filepath.ToSlash(c.RootDirectory))
	if root == "." || root == "" {
		return errors.New("root_directory must not be the vault root itself")
	}
	if path.IsAbs(root) || filepath.IsAbs(c.RootDirectory) {
		return fmt.Errorf("root_directory %q must be relative to the vault", c.RootDirectory)
	}
	if root == ".." || strings.HasPrefix(root, "../") {
		return fmt.Errorf("root_directory %q escapes the vault", c.RootDirectory)
	}
	if strings.HasPrefix(path.Base(root), ".") {
		return fmt.Errorf("root_directory %q must not be hidden (leading dot)", c.RootDirectory)
	}
	c.RootDirectory = root
	destResolved, err := safepath.ResolveExistingAncestor(c.DestRootAbs())
	if err != nil {
		return fmt.Errorf("resolve destination root %s: %w", c.DestRootAbs(), err)
	}
	destInside, err := safepath.Within(c.VaultAbs, destResolved, false)
	if err != nil {
		return err
	}
	if !destInside {
		return fmt.Errorf("destination root %s resolves outside the vault %s", c.DestRootAbs(), c.VaultAbs)
	}

	if c.StateDBPath == "" {
		return errors.New("no state database path resolved")
	}
	dbAbs, err := filepath.Abs(c.StateDBPath)
	if err != nil {
		return fmt.Errorf("resolve state database path %s: %w", c.StateDBPath, err)
	}
	c.StateDBPath = dbAbs
	dbResolved, err := safepath.ResolveExistingAncestor(dbAbs)
	if err != nil {
		return fmt.Errorf("resolve state database %s: %w", dbAbs, err)
	}
	dbInside, err := safepath.Within(c.VaultAbs, dbResolved, true)
	if err != nil {
		return err
	}
	if dbInside {
		return fmt.Errorf("state database %s must live outside the vault %s", dbAbs, c.VaultAbs)
	}
	return nil
}

// DestRootAbs returns the absolute path of the destination root inside the vault.
func (c *Config) DestRootAbs() string {
	return filepath.Join(c.VaultAbs, filepath.FromSlash(c.RootDirectory))
}
