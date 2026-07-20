// Command md2obs imports explicitly selected Markdown files into a dated
// folder inside an Obsidian vault. See README.md for the workflow.
package main

import (
	"context"
	"flag"
	"fmt"
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
	}

	if err := dispatch(ctx, deps, command, args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	return 0
}

func dispatch(ctx context.Context, deps *app.Deps, command string, args []string) error {
	switch command {
	case "import":
		fs := flag.NewFlagSet("import", flag.ContinueOnError)
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: md2obs import FILE...")
		}
		return app.RunImport(ctx, deps, fs.Args())

	case "watch":
		fs := flag.NewFlagSet("watch", flag.ContinueOnError)
		days := fs.Int("days", 1, "inclusive calendar-day window (1 = today)")
		debounce := fs.Duration("debounce", app.DefaultDebounce, "per-source quiet period before re-import")
		policyFlag := fs.String("on-vault-change", string(app.PolicySkip), "policy when the vault copy was edited: overwrite, skip, or preserve")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]")
		}
		policy, err := app.ParsePolicy(*policyFlag)
		if err != nil {
			return err
		}
		return app.RunWatch(ctx, deps, app.WatchOptions{
			Days:          *days,
			Debounce:      *debounce,
			OnVaultChange: policy,
		})

	case "list":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		if err := fs.Parse(args); err != nil {
			return err
		}
		return app.RunList(ctx, deps)

	case "history":
		fs := flag.NewFlagSet("history", flag.ContinueOnError)
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: md2obs history FILE")
		}
		return app.RunHistory(ctx, deps, fs.Arg(0))

	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		if err := fs.Parse(args); err != nil {
			return err
		}
		return app.RunStatus(ctx, deps)
	}
	return fmt.Errorf("unhandled command %q", command)
}
