package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"md2obs/internal/app"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		wantErr string
	}{
		{"import", "import", []string{"one.md", "two.md"}, ""},
		{"import missing file", "import", nil, "usage: md2obs import FILE"},
		{"watch", "watch", []string{"--days", "3", "--debounce", "750ms", "--on-vault-change=preserve"}, ""},
		{"watch positional", "watch", []string{"extra"}, "usage: md2obs watch"},
		{"watch invalid days", "watch", []string{"--days", "0"}, "--days must be at least 1"},
		{"watch invalid debounce", "watch", []string{"--debounce", "0s"}, "--debounce must be positive"},
		{"watch invalid policy", "watch", []string{"--on-vault-change=bogus"}, "invalid --on-vault-change"},
		{"list", "list", nil, ""},
		{"list positional", "list", []string{"extra"}, "usage: md2obs list"},
		{"history", "history", []string{"note.md"}, ""},
		{"history missing", "history", nil, "usage: md2obs history"},
		{"status", "status", nil, ""},
		{"status positional", "status", []string{"extra"}, "usage: md2obs status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCommand(tc.command, tc.args)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parseCommand: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseWatchOptions(t *testing.T) {
	got, err := parseCommand("watch", []string{"--days=3", "--debounce=750ms", "--on-vault-change=preserve"})
	if err != nil {
		t.Fatal(err)
	}
	want := app.WatchOptions{Days: 3, Debounce: 750 * time.Millisecond, OnVaultChange: app.PolicyPreserve}
	if got.watch != want {
		t.Fatalf("watch options = %+v, want %+v", got.watch, want)
	}
}

func TestRunReportsUsageBeforeLoadingConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MD2OBS_VAULT", "")
	t.Setenv("MD2OBS_STATE_DB", "")

	code, stdout, stderr := captureRun(t, []string{"import"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "usage: md2obs import FILE") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Contains(stderr, "no vault configured") {
		t.Fatalf("configuration masked usage error: %q", stderr)
	}
}

func TestRunCommandHelp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MD2OBS_VAULT", "")
	t.Setenv("MD2OBS_STATE_DB", "")

	code, stdout, stderr := captureRun(t, []string{"watch", "-h"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	for _, want := range []string{"Usage: md2obs watch [OPTIONS]", "--debounce", "--on-vault-change"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestRunDefaultsToImport(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	first := filepath.Join(sourceDir, "first.md")
	second := filepath.Join(sourceDir, "second.md")
	if err := os.WriteFile(first, []byte("# first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("# second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("MD2OBS_VAULT", vault)
	t.Setenv("MD2OBS_STATE_DB", filepath.Join(root, "state", "state.db"))

	code, stdout, stderr := captureRun(t, []string{first, second})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if got := strings.Count(stdout, "imported: "); got != 2 {
		t.Fatalf("imported results = %d, want 2; stdout:\n%s", got, stdout)
	}

	vaultFiles, err := filepath.Glob(filepath.Join(vault, "_External", "*", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vaultFiles) != 2 {
		t.Fatalf("vault files = %v, want two imported Markdown files", vaultFiles)
	}
}

func captureRun(t *testing.T, args []string) (code int, stdout, stderr string) {
	t.Helper()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdoutR.Close()
		stderrR.Close()
	})

	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutW, stderrW
	func() {
		defer func() { os.Stdout, os.Stderr = oldStdout, oldStderr }()
		code = run(args)
	}()
	if err := stdoutW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrW.Close(); err != nil {
		t.Fatal(err)
	}

	stdoutBytes, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatal(err)
	}
	stderrBytes, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(stdoutBytes), string(stderrBytes)
}
