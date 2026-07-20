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
	"syscall"
	"time"

	"md2obs/internal/app"
	"md2obs/internal/config"
	"md2obs/internal/database"
	"md2obs/internal/layout"
)

const usage = `md2obs imports explicitly selected Markdown files into an Obsidian vault.

Usage:
  md2obs import FILE...
  md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
  md2obs list
  md2obs history FILE
  md2obs status

Commands:
  import   Import (or refresh) the named Markdown files into today's
           dated vault folder. Explicit imports always overwrite.
  watch    Watch sources with snapshots dated today (--days N widens the
           window to N calendar days) and re-import them when they change.
           --debounce sets the per-source quiet period (default 500ms).
           --on-vault-change sets the policy when the vault copy was edited
           since the last import: skip (default), overwrite, or preserve.
  list     List known sources and their most recent snapshot.
  history  Show dated snapshots for one source.
  status   Show configuration, database location, and counts.

Configuration:
  Config file  ~/.config/md2obs/config.json (Linux)
  MD2OBS_VAULT     overrides vault_path
  MD2OBS_STATE_DB  overrides the state database location
`

var commandUsage = map[string]string{
	"import": `Usage: md2obs import FILE...

Import or refresh explicitly named Markdown files. An explicit import restores
the source content if the vault copy was edited.
`,
	"watch": `Usage: md2obs watch [OPTIONS]

Watch recently imported sources using native filesystem notifications.

Options:
  --days N                    Inclusive calendar-day window (default 1)
  --debounce DURATION         Per-source quiet period (default 500ms)
  --on-vault-change POLICY    skip (default), overwrite, or preserve
`,
	"list": `Usage: md2obs list

List known sources and their most recent snapshots.
`,
	"history": `Usage: md2obs history FILE

Show dated snapshots for one explicitly imported source.
`,
	"status": `Usage: md2obs status

Show configuration, database location, schema version, and counts.
`,
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	command := args[0]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	case "import", "watch", "list", "history", "status":
	default:
		fmt.Fprintf(os.Stderr, "md2obs: unknown command %q\n\n%s", command, usage)
		return 2
	}
	options, err := parseCommand(command, args[1:])
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
	watch       app.WatchOptions
}

func commandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
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

	case "watch":
		fs := commandFlagSet("watch")
		days := fs.Int("days", 1, "inclusive calendar-day window (1 = today)")
		debounce := fs.Duration("debounce", app.DefaultDebounce, "per-source quiet period before re-import")
		policyFlag := fs.String("on-vault-change", string(app.PolicySkip), "policy when the vault copy was edited: overwrite, skip, or preserve")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]")
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

	case "list":
		fs := commandFlagSet("list")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs list")
		}
		return options, nil

	case "history":
		fs := commandFlagSet("history")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 1 {
			return options, fmt.Errorf("usage: md2obs history FILE")
		}
		options.historyFile = fs.Arg(0)
		return options, nil

	case "status":
		fs := commandFlagSet("status")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs status")
		}
		return options, nil
	}
	return options, fmt.Errorf("unhandled command %q", command)
}

func dispatch(ctx context.Context, deps *app.Deps, command string, options commandOptions) error {
	switch command {
	case "import":
		return app.RunImport(ctx, deps, options.files)
	case "watch":
		return app.RunWatch(ctx, deps, options.watch)
	case "list":
		return app.RunList(ctx, deps)
	case "history":
		return app.RunHistory(ctx, deps, options.historyFile)
	case "status":
		return app.RunStatus(ctx, deps)
	default:
		return fmt.Errorf("unhandled command %q", command)
	}
}
