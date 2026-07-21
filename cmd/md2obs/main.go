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
  md2obs FILE...
  md2obs import FILE...
  md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
  md2obs watch start [--log] [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
  md2obs watch stop
  md2obs watch restart [--log] [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
  md2obs list
  md2obs history FILE
  md2obs status

Commands:
  import   Import (or refresh) the named Markdown files into today's
           dated vault folder. This is the default when the command is
           omitted. Explicit imports always overwrite.
  watch    Watch sources materialized in this vault today (--days N widens
           the initial window) and enroll later imports while running.
           Re-import watched sources when they change.
           start runs one managed watcher for this database and vault in the
           background. stop and restart manage that instance.
           Managed watcher output is discarded unless --log is specified.
           --debounce sets the per-source quiet period (default 500ms).
           --on-vault-change sets the policy when the vault copy was edited
           since the last import: skip (default), overwrite, or preserve.
  list     List known sources and their most recent snapshot.
  history  Show dated snapshots for one source.
  status   Show configuration, database location, counts, and watcher state.

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
	"watch": `Usage:
  md2obs watch [OPTIONS]
  md2obs watch start [OPTIONS]
  md2obs watch stop
  md2obs watch restart [OPTIONS]

Watch sources recently imported into the configured vault using native
filesystem notifications. With no subcommand the watcher stays in the
foreground. Successful imports join a running watch session.

Options:
  --days N                    Inclusive calendar-day window (default 1)
  --debounce DURATION         Per-source quiet period (default 500ms)
  --on-vault-change POLICY    skip (default), overwrite, or preserve

Managed watcher commands:
  start                       Start one background watcher (Linux and macOS)
  stop                        Send SIGTERM and wait for a graceful exit
  restart                     Stop the managed watcher, then start it again
  --log                       On start/restart, log output beside the database
`,
	"list": `Usage: md2obs list

List known sources and their most recent snapshots.
`,
	"history": `Usage: md2obs history FILE

Show dated snapshots for one explicitly imported source.
`,
	"status": `Usage: md2obs status

Show configuration, database location, schema version, counts, and active
watcher state.
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
	commandArgs := args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	case "import", "watch", "list", "history", "status":
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

	var watchReady func()
	if command == "watch" && options.watchAction == watchStart && isDaemonChild() {
		var cleanup func()
		watchReady, cleanup, err = daemonReadySignal()
		if err != nil {
			fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
			return 1
		}
		defer cleanup()
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	var releaseWatchLease func()
	if command == "watch" && (options.watchAction == watchForeground || (options.watchAction == watchStart && isDaemonChild())) {
		mode := watchModeForeground
		if options.watchAction == watchStart {
			mode = watchModeManaged
		}
		_, releaseWatchLease, err = claimManagedWatch(cfg, mode, managedSettings(options))
		if err != nil {
			fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
			return 1
		}
		defer releaseWatchLease()
		if mode == watchModeManaged {
			fmt.Fprintf(os.Stdout, "Starting md2obs watch daemon (PID %d)\n", os.Getpid())
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if command == "watch" && !isDaemonChild() {
		switch options.watchAction {
		case watchStop:
			return runWatchStop(ctx, cfg)
		case watchRestart:
			state, err := inspectManagedWatch(cfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
				return 1
			}
			if state.Running && state.Record.Mode == watchModeManaged && !options.watchSettingsSet {
				applyManagedSettings(&options, state.Record.Settings)
			}
			if state.Running {
				if code := runWatchStop(ctx, cfg); code != 0 {
					return code
				}
			}
			options.watchAction = watchStart
		}
	}

	if command == "watch" && options.watchAction == watchStart && !isDaemonChild() {
		state, err := inspectManagedWatch(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
			return 1
		}
		if state.Running {
			fmt.Fprintf(os.Stderr, "md2obs: %v\n", watchInstanceConflict(state.Record))
			return 1
		}
		executable, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "md2obs: resolve executable for daemon: %v\n", err)
			return 1
		}
		process, err := launchWatchDaemon(ctx, executable, managedWatchArgs(options), cfg, options.log)
		if err != nil {
			fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "Started md2obs watch daemon (PID %d)\n", process.PID)
		if process.LogPath != "" {
			fmt.Fprintf(os.Stdout, "Log: %s\n", process.LogPath)
		}
		return 0
	}

	db, err := database.Open(ctx, cfg.StateDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	defer db.Close()

	deps := &app.Deps{
		DB:         db,
		Config:     cfg,
		Layout:     layout.NewDatedFlatV1(),
		Now:        time.Now,
		Out:        os.Stdout,
		Err:        os.Stderr,
		Log:        slog.New(slog.NewTextHandler(os.Stderr, nil)),
		WatchReady: watchReady,
	}

	if err := dispatch(ctx, deps, command, options); err != nil {
		fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
		return 1
	}
	if command == "status" {
		state, err := inspectManagedWatch(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "md2obs: %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, formatWatchStatus(state))
	}
	return 0
}

type watchAction uint8

const (
	watchForeground watchAction = iota
	watchStart
	watchStop
	watchRestart
)

type commandOptions struct {
	files       []string
	historyFile string
	watch       app.WatchOptions
	watchAction watchAction
	// watchSettingsSet distinguishes a bare restart, which preserves the
	// running instance's settings, from a restart with replacement settings.
	watchSettingsSet bool
	log              bool
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
		options.watchAction = watchForeground
		if len(args) > 0 {
			switch args[0] {
			case "start":
				options.watchAction = watchStart
				args = args[1:]
			case "stop":
				return parseWatchStopCommand(options, args[1:])
			case "restart":
				options.watchAction = watchRestart
				args = args[1:]
			}
		}
		fs := commandFlagSet("watch")
		var logOutput *bool
		if options.watchAction == watchStart || options.watchAction == watchRestart {
			logOutput = fs.Bool("log", false, "log daemon output beside the state database")
		}
		days := fs.Int("days", 1, "inclusive calendar-day window (1 = today)")
		debounce := fs.Duration("debounce", app.DefaultDebounce, "per-source quiet period before re-import")
		policyFlag := fs.String("on-vault-change", string(app.PolicySkip), "policy when the vault copy was edited: overwrite, skip, or preserve")
		if err := fs.Parse(args); err != nil {
			return options, err
		}
		if fs.NArg() != 0 {
			return options, fmt.Errorf("usage: md2obs watch [start|stop|restart] [OPTIONS]")
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
		if logOutput != nil {
			options.log = *logOutput
		}
		fs.Visit(func(*flag.Flag) { options.watchSettingsSet = true })
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

func parseWatchStopCommand(options commandOptions, args []string) (commandOptions, error) {
	options.watchAction = watchStop
	fs := commandFlagSet("watch")
	if err := fs.Parse(args); err != nil {
		return options, err
	}
	if fs.NArg() != 0 {
		return options, fmt.Errorf("usage: md2obs watch stop")
	}
	return options, nil
}

func dispatch(ctx context.Context, deps *app.Deps, command string, options commandOptions) error {
	switch command {
	case "import":
		return app.RunImport(ctx, deps, options.files)
	case "watch":
		if options.watchAction != watchForeground && !(options.watchAction == watchStart && isDaemonChild()) {
			return fmt.Errorf("internal error: unmanaged watch lifecycle action reached dispatcher")
		}
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
