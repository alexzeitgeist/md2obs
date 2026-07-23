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

	"github.com/alexzeitgeist/md2obs/internal/app"
	"github.com/alexzeitgeist/md2obs/internal/config"
	"github.com/alexzeitgeist/md2obs/internal/database"
	"github.com/alexzeitgeist/md2obs/internal/layout"
)

const usage = `Copy selected Markdown files into dated folders in an Obsidian vault.

Usage:
  md2obs FILE...
  md2obs import FILE...
  md2obs refresh [--days N | --all] [--rerender] [--on-vault-change=POLICY]
  md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
  md2obs untrack FILE...
  md2obs untrack [--missing] [--older-than AGE] [--dry-run]
  md2obs version
  md2obs debug COMMAND

Commands:
  import   Copy files into today's folder (default command).
  refresh  Check tracked sources once and copy changes.
  watch    Watch tracked sources and copy changes until stopped.
  untrack  Stop tracking sources without deleting vault files.
  version  Print the installed version and source commit.
  debug    Inspect configuration and state.

Run 'md2obs COMMAND --help' for command options.

Configuration:
  Config file      ~/.config/md2obs/config.json (Linux)
  MD2OBS_VAULT     overrides vault_path
  MD2OBS_STATE_DB  overrides the state database path
`

var (
	version = "dev"
	commit  = "unknown"
)

var commandUsage = map[string]string{
	"import": `Usage:
  md2obs FILE...
  md2obs import FILE...

Copy each FILE into today's dated folder. Running the command again updates
today's copy. Explicit imports replace edits made to the managed vault copy.
`,
	"refresh": `Usage:
  md2obs refresh [--days N | --all] [--rerender] [--on-vault-change=POLICY]

Check tracked sources once and copy any changes.

Options:
  --days N                    Sources imported in the last N days (default 1)
  --all                       All sources tracked in this vault
  --rerender                  Apply the current rendering configuration
  --on-vault-change POLICY    skip (default), preserve, or overwrite
`,
	"watch": `Usage:
  md2obs watch [OPTIONS]

Watch recently imported sources. Runs in the foreground until interrupted.
New imports join the watch automatically.

Options:
  --days N                    Sources imported in the last N days (default 1)
  --debounce DURATION         How long a change must settle before copying
                              (default 500ms)
  --on-vault-change POLICY    skip (default), preserve, or overwrite
`,
	"untrack": `Usage:
  md2obs untrack [--dry-run] FILE...
  md2obs untrack --missing [--older-than AGE] [--dry-run]
  md2obs untrack --older-than AGE [--dry-run]

Stop tracking sources without deleting their vault copies. Importing an
untracked source starts tracking it again.

When selectors are combined, a source must match all of them.

Options:
  --missing           Sources that no longer exist
  --older-than AGE    Sources last imported more than AGE ago (such as 30d)
  --dry-run           Show what would change
`,
	"version": `Usage: md2obs version

Print the installed version and source commit.
`,
	"debug list": `Usage: md2obs debug list

List tracked sources and their latest vault paths.
`,
	"debug history": `Usage: md2obs debug history FILE

Show stored snapshot records for one source.
`,
	"debug status": `Usage: md2obs debug status

Show resolved paths, schema version, and state counts.
`,
}

const debugUsage = `Usage:
  md2obs debug list
  md2obs debug history FILE
  md2obs debug status

Debug commands:
  list     List tracked sources and their latest vault paths.
  history  Show stored snapshot records for one source.
  status   Show resolved paths, schema version, and state counts.
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
	case "version":
		if len(commandArgs) == 1 && isHelpArg(commandArgs[0]) {
			fmt.Fprint(os.Stdout, commandUsage["version"])
			return 0
		}
		if len(commandArgs) != 0 {
			fmt.Fprintln(os.Stderr, "md2obs: usage: md2obs version")
			return 2
		}
		displayCommit := commit
		if len(displayCommit) > 7 {
			displayCommit = displayCommit[:7]
		}
		fmt.Fprintf(os.Stdout, "md2obs %s (%s)\n", version, displayCommit)
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
		days := fs.Int("days", 1, "sources imported in the last N days (1 = today)")
		allSources := fs.Bool("all", false, "all sources tracked in this vault")
		rerender := fs.Bool("rerender", false, "apply the current rendering configuration")
		policyFlag := fs.String("on-vault-change", string(app.PolicySkip), "edited vault copy: skip, preserve, or overwrite")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs refresh [--days N | --all] [--rerender] [--on-vault-change=POLICY]")
		}
		daysSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "days" {
				daysSet = true
			}
		})
		policy, err := app.ParsePolicy(*policyFlag)
		if err != nil {
			return options, err
		}
		options.refresh = app.RefreshOptions{
			Days:          *days,
			DaysSet:       daysSet,
			All:           *allSources,
			Rerender:      *rerender,
			OnVaultChange: policy,
		}
		if err := options.refresh.Validate(); err != nil {
			return options, err
		}
		return options, nil

	case "watch":
		fs := commandFlagSet("watch")
		days := fs.Int("days", 1, "sources imported in the last N days (1 = today)")
		debounce := fs.Duration("debounce", app.DefaultDebounce, "how long a change must settle before copying")
		policyFlag := fs.String("on-vault-change", string(app.PolicySkip), "edited vault copy: skip, preserve, or overwrite")
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
		missing := fs.Bool("missing", false, "sources that no longer exist")
		olderThan := fs.String("older-than", "", "sources last imported more than AGE ago")
		dryRun := fs.Bool("dry-run", false, "show changes without applying them")
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

	case "debug list", "debug status":
		fs := commandFlagSet(command)
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs %s", command)
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
