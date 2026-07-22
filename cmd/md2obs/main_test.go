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
		{"refresh", "refresh", nil, ""},
		{"refresh all", "refresh", []string{"--all", "--on-vault-change=preserve"}, ""},
		{"refresh positional", "refresh", []string{"extra"}, "usage: md2obs refresh"},
		{"refresh all and days", "refresh", []string{"--all", "--days=2"}, "--all cannot be combined"},
		{"refresh invalid days", "refresh", []string{"--days=0"}, "--days must be at least 1"},
		{"refresh invalid policy", "refresh", []string{"--on-vault-change=bogus"}, "invalid --on-vault-change"},
		{"watch", "watch", []string{"--days", "3", "--debounce", "750ms", "--on-vault-change=preserve"}, ""},
		{"watch positional", "watch", []string{"extra"}, "usage: md2obs watch"},
		{"watch removed start command", "watch", []string{"start"}, "usage: md2obs watch"},
		{"watch removed stop command", "watch", []string{"stop"}, "usage: md2obs watch"},
		{"watch removed restart command", "watch", []string{"restart"}, "usage: md2obs watch"},
		{"watch removed daemon flag", "watch", []string{"--daemon"}, "flag provided but not defined"},
		{"watch removed log flag", "watch", []string{"--log"}, "flag provided but not defined"},
		{"watch invalid days", "watch", []string{"--days", "0"}, "--days must be at least 1"},
		{"watch invalid debounce", "watch", []string{"--debounce", "0s"}, "--debounce must be positive"},
		{"watch invalid policy", "watch", []string{"--on-vault-change=bogus"}, "invalid --on-vault-change"},
		{"untrack files", "untrack", []string{"one.md", "two.md"}, ""},
		{"untrack missing", "untrack", []string{"--missing"}, ""},
		{"untrack old", "untrack", []string{"--older-than=90d"}, ""},
		{"untrack combined dry run", "untrack", []string{"--missing", "--older-than", "30d", "--dry-run"}, ""},
		{"untrack missing selector", "untrack", nil, "usage: md2obs untrack"},
		{"untrack ambiguous selection", "untrack", []string{"--missing", "one.md"}, "source paths cannot be combined"},
		{"untrack invalid age unit", "untrack", []string{"--older-than=24h"}, "invalid --older-than"},
		{"untrack invalid zero age", "untrack", []string{"--older-than=0d"}, "invalid --older-than"},
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

func TestParseRefreshOptions(t *testing.T) {
	got, err := parseCommand("refresh", []string{"--all", "--on-vault-change=preserve"})
	if err != nil {
		t.Fatal(err)
	}
	want := app.RefreshOptions{All: true, OnVaultChange: app.PolicyPreserve}
	if got.refresh != want {
		t.Fatalf("refresh options = %+v, want %+v", got.refresh, want)
	}

	defaults, err := parseCommand("refresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantDefaults := app.RefreshOptions{Days: 1, OnVaultChange: app.PolicySkip}
	if defaults.refresh != wantDefaults {
		t.Fatalf("refresh defaults = %+v, want %+v", defaults.refresh, wantDefaults)
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

func TestParseUntrackOptions(t *testing.T) {
	got, err := parseCommand("untrack", []string{"--missing", "--older-than=45d", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.untrack.Missing || got.untrack.OlderThanDays != 45 || !got.untrack.DryRun || len(got.untrack.Files) != 0 {
		t.Fatalf("untrack batch options = %+v", got.untrack)
	}

	named, err := parseCommand("untrack", []string{"one.md", "two.md"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(named.untrack.Files, ",") != "one.md,two.md" || named.untrack.Missing || named.untrack.OlderThanDays != 0 {
		t.Fatalf("untrack named options = %+v", named.untrack)
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

func TestRunRemovedWatchCommandsReportUsageBeforeLoadingConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MD2OBS_VAULT", "")
	t.Setenv("MD2OBS_STATE_DB", "")

	for _, command := range []string{"start", "stop", "restart"} {
		t.Run(command, func(t *testing.T) {
			code, stdout, stderr := captureRun(t, []string{"watch", command})
			if code != 2 || stdout != "" {
				t.Fatalf("watch %s = %d, stdout = %q, stderr = %q", command, code, stdout, stderr)
			}
			if !strings.Contains(stderr, "usage: md2obs watch [OPTIONS]") {
				t.Fatalf("watch %s stderr = %q", command, stderr)
			}
			if strings.Contains(stderr, "no vault configured") {
				t.Fatalf("configuration masked watch %s usage error: %q", command, stderr)
			}
		})
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
	for _, want := range []string{"md2obs watch [OPTIONS]", "stays in the foreground", "--debounce", "--on-vault-change"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
	for _, removed := range []string{"watch start", "watch stop", "watch restart", "--log", "daemon", "Managed watcher"} {
		if strings.Contains(stdout, removed) {
			t.Errorf("watch help still advertises removed daemon text %q:\n%s", removed, stdout)
		}
	}
}

func TestRunRefreshHelp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MD2OBS_VAULT", "")
	t.Setenv("MD2OBS_STATE_DB", "")

	code, stdout, stderr := captureRun(t, []string{"refresh", "-h"})
	if code != 0 || stderr != "" {
		t.Fatalf("refresh help = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"md2obs refresh", "--days", "--all", "--on-vault-change"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestRunUntrackHelp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MD2OBS_VAULT", "")
	t.Setenv("MD2OBS_STATE_DB", "")

	code, stdout, stderr := captureRun(t, []string{"untrack", "-h"})
	if code != 0 || stderr != "" {
		t.Fatalf("untrack help = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"md2obs untrack", "--missing", "--older-than", "--dry-run", "registers"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestRunStatusReportsDatabaseStateOnly(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("MD2OBS_VAULT", vault)
	t.Setenv("MD2OBS_STATE_DB", filepath.Join(root, "state", "state.db"))

	code, stdout, stderr := captureRun(t, []string{"status"})
	if code != 0 || stderr != "" {
		t.Fatalf("status = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"Schema version:", "Sources:", "Snapshots:", "Materializations:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "Watcher:") {
		t.Errorf("status output still contains removed watcher lifecycle state:\n%s", stdout)
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

func TestRunUntrackCommandUpdatesListState(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.Mkdir(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "tracked.md")
	if err := os.WriteFile(source, []byte("# tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("MD2OBS_VAULT", vault)
	t.Setenv("MD2OBS_STATE_DB", filepath.Join(root, "state", "state.db"))

	if code, _, stderr := captureRun(t, []string{source}); code != 0 {
		t.Fatalf("import = %d, stderr = %q", code, stderr)
	}
	code, stdout, stderr := captureRun(t, []string{"untrack", source})
	if code != 0 || stderr != "" || !strings.Contains(stdout, "untracked: "+source) {
		t.Fatalf("untrack = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	code, stdout, stderr = captureRun(t, []string{"list"})
	if code != 0 || stderr != "" || !strings.Contains(stdout, "No sources tracked in configured vault") {
		t.Fatalf("list after untrack = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	code, stdout, stderr = captureRun(t, []string{"untrack", source})
	if code != 0 || stderr != "" || !strings.Contains(stdout, "not tracked: "+source) {
		t.Fatalf("repeated untrack = %d, stdout = %q, stderr = %q", code, stdout, stderr)
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
