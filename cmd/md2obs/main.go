// Command md2obs imports explicitly selected Markdown files into a dated
// folder inside an Obsidian vault. See README.md for the workflow.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"md2obs/internal/app"
	"md2obs/internal/config"
	"md2obs/internal/database"
	"md2obs/internal/layout"
)

const usage = `md2obs imports explicitly selected Markdown files into an Obsidian vault.

Usage:
  md2obs FILE...
  md2obs import FILE...
  md2obs refresh [--days N | --all] [--on-vault-change=POLICY]
  md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
  md2obs untrack FILE...
  md2obs untrack [--missing] [--older-than AGE] [--dry-run]
  md2obs debug COMMAND

Commands:
  import   Import (or refresh) the named Markdown files into today's
           dated vault folder. This is the default when the command is
           omitted. Explicit imports always overwrite.
  refresh  Check previously imported sources for changes and catch up the
           changed ones. --days selects recent materializations (default 1);
           --all selects every source currently tracked in this vault.
           Vault edits are skipped by default.
  watch    Watch sources materialized in this vault today (--days N widens
           the initial window) and enroll later imports while running.
           Re-import watched sources when they change.
           --debounce sets the per-source quiet period (default 500ms).
           --on-vault-change sets the policy when the vault copy was edited
           since the last import: skip (default), overwrite, or preserve.
  untrack  Forget named sources in this vault, or select a batch by definite
           absence and/or materialization age. Database bookkeeping no other
           vault needs is collected; physical vault files are untouched.
  debug    Inspect internal bookkeeping with list, history, and status.

Configuration:
  Config file  ~/.config/md2obs/config.json (Linux)
  MD2OBS_VAULT     overrides vault_path
  MD2OBS_STATE_DB  overrides the state database location
`

var commandUsage = map[string]string{
	"import": `Usage:
  md2obs FILE...
  md2obs import FILE...

Import or refresh explicitly named Markdown files. An explicit import restores
the source content if the vault copy was edited.
`,
	"refresh": `Usage:
  md2obs refresh [--days N | --all] [--on-vault-change=POLICY]

Check sources previously materialized in the configured vault and import the
ones whose current content differs from their selected snapshot.

Options:
  --days N                    Materialization date window (default 1)
  --all                       Every source currently tracked in this vault
  --on-vault-change POLICY    skip (default), overwrite, or preserve
`,
	"watch": `Usage:
  md2obs watch [OPTIONS]

Watch sources recently imported into the configured vault using native
filesystem notifications. The watcher stays in the foreground until
interrupted. Successful imports join a running watch session.

Options:
  --days N                    Inclusive calendar-day window (default 1)
  --debounce DURATION         Per-source quiet period (default 500ms)
  --on-vault-change POLICY    skip (default), overwrite, or preserve
`,
	"untrack": `Usage:
  md2obs untrack [--dry-run] FILE...
  md2obs untrack --missing [--older-than AGE] [--dry-run]
  md2obs untrack --older-than AGE [--dry-run]

Forget selected sources in the configured vault. Materialization records for
this vault and bookkeeping no other vault references are removed. Physical
vault files are untouched. A later explicit import registers the source again.

Batch selectors are combined: --missing --older-than 30d selects sources that
are both definitely absent and older than 30 local calendar days.

Options:
  --missing           Exact source absent while its parent is accessible
  --older-than AGE    Newest materialized snapshot is older than AGE (for
                      example 30d or 365d)
  --dry-run           Report bookkeeping that would be forgotten or collected
`,
	"debug list": `Usage: md2obs debug list

List sources currently tracked in the configured vault.
`,
	"debug history": `Usage: md2obs debug history FILE

Show retained snapshot diagnostics for one explicitly imported source. Entries
are complete while the source remains tracked but may be collected by untrack.
`,
	"debug status": `Usage: md2obs debug status

Show configuration, database location, schema version, and counts.
`,
}

const debugUsage = `Usage:
  md2obs debug list
  md2obs debug history FILE
  md2obs debug status

Debug commands:
  list     List sources currently tracked in this vault.
  history  Show retained snapshot diagnostics for one source.
  status   Show configuration, database location, schema version, and counts.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	command := args[0]
	commandArgs := args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	case "import", "refresh", "watch", "untrack":
	case "debug":
		if len(commandArgs) > 0 && isHelpArg(commandArgs[0]) {
			fmt.Fprint(os.Stdout, debugUsage)
			return 0
		}
		if len(commandArgs) == 0 {
			fmt.Fprint(os.Stderr, debugUsage)
			return 2
		}
		subcommand := commandArgs[0]
		switch subcommand {
		case "list", "history", "status":
			command = "debug " + subcommand
			commandArgs = commandArgs[1:]
		default:
			fmt.Fprintf(os.Stderr, "md2obs: unknown debug command %q\n\n%s", subcommand, debugUsage)
			return 2
		}
	case "list", "history", "status":
		fmt.Fprintf(os.Stderr, "md2obs: %s moved to 'md2obs debug %s'\n", command, command)
		return 2
	default:
		command = "import"
		commandArgs = args
	}
	options, err := parseCommand(command, commandArgs)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stdout, commandUsage[command])
			return 0
		}
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	if command == "watch" {
		releaseWatchLock, err := acquireWatchLock(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
			return 1
		}
		defer releaseWatchLock()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, cfg.StateDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	defer db.Close()

	deps := &app.Deps{
		DB:     db,
		Config: cfg,
		Layout: layout.NewDatedFlatV1(),
		Now:    time.Now,
		Out:    os.Stdout,
		Err:    os.Stderr,
		Log:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := dispatch(ctx, deps, command, options); err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	return 0
}

type commandOptions struct {
	files       []string
	historyFile string
	refresh     app.RefreshOptions
	watch       app.WatchOptions
	untrack     app.UntrackOptions
}

func commandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func isHelpArg(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "--help"
}

// parseCommand validates all command syntax before configuration or SQLite is
// touched, so usage errors do not get masked by environmental failures.
func parseCommand(command string, args []string) (commandOptions, error) {
	var options commandOptions
	switch command {
	case "import":
		fs := commandFlagSet("import")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() == 0 {
			return options, fmt.Errorf("usage: md2obs import FILE...")
		}
		options.files = fs.Args()
		return options, nil

	case "refresh":
		fs := commandFlagSet("refresh")
		days := fs.Int("days", 1, "materialization date window (1 = today)")
		allSources := fs.Bool("all", false, "every source currently tracked in this vault")
		policyFlag := fs.String("on-vault-change", string(app.PolicySkip), "policy when the vault copy was edited: overwrite, skip, or preserve")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs refresh [--days N | --all] [--on-vault-change=POLICY]")
		}
		daysSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "days" {
				daysSet = true
			}
		})
		if *allSources && daysSet {
			return options, fmt.Errorf("--all cannot be combined with --days")
		}
		policy, err := app.ParsePolicy(*policyFlag)
		if err != nil {
			return options, err
		}
		refreshDays := *days
		if *allSources {
			refreshDays = 0
		}
		options.refresh = app.RefreshOptions{
			Days:          refreshDays,
			All:           *allSources,
			OnVaultChange: policy,
		}
		if err := options.refresh.Validate(); err != nil {
			return options, err
		}
		return options, nil

	case "watch":
		fs := commandFlagSet("watch")
		days := fs.Int("days", 1, "inclusive calendar-day window (1 = today)")
		debounce := fs.Duration("debounce", app.DefaultDebounce, "per-source quiet period before re-import")
		policyFlag := fs.String("on-vault-change", string(app.PolicySkip), "policy when the vault copy was edited: overwrite, skip, or preserve")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs watch [OPTIONS]")
		}
		policy, err := app.ParsePolicy(*policyFlag)
		if err != nil {
			return options, err
		}
		options.watch = app.WatchOptions{
			Days:          *days,
			Debounce:      *debounce,
			OnVaultChange: policy,
		}
		if err := options.watch.Validate(); err != nil {
			return options, err
		}
		return options, nil

	case "untrack":
		fs := commandFlagSet("untrack")
		missing := fs.Bool("missing", false, "sources whose exact path is absent while its parent is accessible")
		olderThan := fs.String("older-than", "", "sources whose newest materialized snapshot is older than AGE")
		dryRun := fs.Bool("dry-run", false, "report without changing bookkeeping")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		olderThanDays := 0
		if *olderThan != "" {
			var err error
			olderThanDays, err = parseAgeDays(*olderThan)
			if err != nil {
				return options, err
			}
		}
		options.untrack = app.UntrackOptions{
			Files:         fs.Args(),
			Missing:       *missing,
			OlderThanDays: olderThanDays,
			DryRun:        *dryRun,
		}
		if err := options.untrack.Validate(); err != nil {
			return options, err
		}
		return options, nil

	case "debug list":
		fs := commandFlagSet("debug list")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs debug list")
		}
		return options, nil

	case "debug history":
		fs := commandFlagSet("debug history")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 1 {
			return options, fmt.Errorf("usage: md2obs debug history FILE")
		}
		options.historyFile = fs.Arg(0)
		return options, nil

	case "debug status":
		fs := commandFlagSet("debug status")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs debug status")
		}
		return options, nil
	}
	return options, fmt.Errorf("unhandled command %q", command)
}

func parseAgeDays(value string) (int, error) {
	if !strings.HasSuffix(value, "d") || len(value) == 1 {
		return 0, fmt.Errorf("invalid --older-than value %q (want a positive whole-day age such as 30d)", value)
	}
	days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
	if err != nil || days <= 0 {
		return 0, fmt.Errorf("invalid --older-than value %q (want a positive whole-day age such as 30d)", value)
	}
	return days, nil
}

func dispatch(ctx context.Context, deps *app.Deps, command string, options commandOptions) error {
	switch command {
	case "import":
		return app.RunImport(ctx, deps, options.files)
	case "refresh":
		return app.RunRefresh(ctx, deps, options.refresh)
	case "watch":
		return app.RunWatch(ctx, deps, options.watch)
	case "untrack":
		return app.RunUntrack(ctx, deps, options.untrack)
	case "debug list":
		return app.RunList(ctx, deps)
	case "debug history":
		return app.RunHistory(ctx, deps, options.historyFile)
	case "debug status":
		return app.RunStatus(ctx, deps)
	default:
		return fmt.Errorf("unhandled command %q", command)
	}
}
