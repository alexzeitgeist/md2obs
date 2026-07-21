# md2obs

`md2obs` imports explicitly selected Markdown files from arbitrary filesystem
locations into a dated folder inside an Obsidian vault. The imported files are
normal vault files, so Obsidian Sync distributes them to phones and tablets.

The original file stays where it is and remains the content source of truth.
The vault copy is a dated, derived snapshot — an import journal, not a
bidirectional sync. Nothing is ever discovered or imported automatically; the
only way a file enters the vault is an explicit `md2obs FILE...` invocation
(`md2obs import FILE...` is equivalent), or the watcher re-importing a file
you already imported.

## Install

```console
go build -o md2obs ./cmd/md2obs
install -m 0755 md2obs ~/.local/bin/
```

## Configure

`~/.config/md2obs/config.json` (Linux):

```json
{
  "vault_path": "/home/alex/obsidian/vault",
  "layout": "dated-flat-v1",
  "root_directory": "_External"
}
```

- `vault_path` — your Obsidian vault root. Must exist.
- `layout` — destination layout; only `dated-flat-v1` exists today.
- `root_directory` — vault-relative destination root. Not the vault root
  itself, not hidden (no leading dot), must stay inside the vault.

Environment overrides for scripting:

| Variable | Overrides |
|---|---|
| `MD2OBS_VAULT` | `vault_path` |
| `MD2OBS_STATE_DB` | state database location |

State lives in SQLite at `~/.local/share/md2obs/state.db` (Linux),
`~/Library/Application Support/md2obs/state.db` (macOS). It is deliberately
kept outside the vault so Obsidian Sync never sees live WAL files. The
database must not be placed inside the vault.

## Commands

```console
md2obs FILE...                         # import is the default command
md2obs import FILE...
md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
md2obs watch start [--log] [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
md2obs watch status
md2obs watch stop
md2obs watch restart [--log] [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
md2obs list
md2obs history FILE
md2obs status
```

### import

```console
md2obs ~/projects/a/README.md /tmp/response.md
# Equivalent explicit form:
md2obs import ~/projects/a/README.md /tmp/response.md
```

Each file is hashed and written atomically to
`_External/YYYY-MM-DD/<name>.md` using today's local date:

- **imported** — a new dated snapshot was created.
- **updated** — today's existing snapshot was overwritten (same-day changes
  replace the same file; a later day gets a new dated file).
- **unchanged** — content already current; no vault write.

Same-name files from different places get progressively more path context:
`README.md`, `README--project-b.md`, `README--project-b--alex.md`, …, with a
deterministic 6-hex-digit hash suffix as the final fallback. Explicit
`import` always overwrites the vault copy, including edits made in Obsidian.
Generated filename components are capped at 255 bytes; overlong collision
names are truncated on a UTF-8 boundary and retain the source hash.

### watch

```console
md2obs watch                         # foreground; sources imported today
md2obs watch --days 3                # today and the previous two days
md2obs watch --debounce 500ms        # per-source quiet period (default)
md2obs watch --on-vault-change=preserve

md2obs watch start --days 3          # start the managed background watcher
md2obs watch start --log             # append output beside the state database
md2obs watch status                  # show its PID and start time
md2obs watch stop                    # SIGTERM, then wait for graceful exit
md2obs watch restart                 # preserve the running watcher's settings
```

The watcher selects recent sources materialized in the configured vault,
watches only their immediate parent directories (non-recursively, via native
filesystem notifications), and re-imports a source after its events settle.
Membership is vault-scoped: imports made only into another vault sharing the
same state database are not watched or materialized here.

Successful explicit imports into this vault automatically join an already
running watcher, including imports from directories it was not previously
watching. A watcher started with no eligible sources stays running and waits
for imports. Membership grows until restart; it is not reduced as the date
window advances. The discovery range expands across midnight so an import
immediately before midnight is not missed when its notification arrives just
after midnight.

Startup is passive and never rewrites existing vault copies. A source enrolled
after startup gets one silent content check after its directory watch is armed,
closing the small import-to-watch race; matching content causes no vault write
and does not evaluate `--on-vault-change`. The watcher never scans directories,
never imports unrelated files, and does no polling — idle, it consumes
effectively no CPU. Bare `md2obs watch` remains a foreground command; stop it
with Ctrl-C.

`watch start` runs the same watcher in a detached background session on Linux
or macOS. The starting command waits until the initial database selection and
filesystem watches are armed, then prints the daemon PID. A configuration,
database, lease, or watcher startup error therefore still returns a non-zero
status to the caller. Daemon output is discarded by default, so no log file is
created. Add `--log` to append standard output and errors to
`<state-database>.watch.log`, with permissions restricted to the current user:

```console
$ md2obs watch start --log --days 3
Started md2obs watch daemon (PID 12345)
Log: /home/alex/.local/share/md2obs/state.db.watch.log
$ md2obs watch status
Watch daemon:      running (PID 12345, started 2026-07-21T10:15:00+02:00)
$ md2obs watch stop
Stopped md2obs watch daemon (PID 12345)
```

Exactly one managed instance is allowed for each resolved `(state database,
vault)` pair. Concurrent or repeated `watch start` calls cannot create
duplicates: the daemon holds an exclusive lease for its lifetime. Its durable
record contains the PID, a random instance ID, kernel process-start identity,
start time, scope, and watch settings. An unlocked record left by a crash or
`SIGKILL` is stale and is removed by the next lifecycle command. The identity
check prevents `watch stop` from signaling an unrelated process if the stored
PID has been reused.

`watch stop` sends `SIGTERM` and waits up to 10 seconds for the daemon to close
its database and filesystem watches and release its lease. It reports a clear
error if graceful shutdown times out; it never escalates to `SIGKILL`.
`watch restart` stops and starts the instance. With no options it preserves the
running instance's `--days`, `--debounce`, `--on-vault-change`, and `--log`
settings. Supplying any option selects a new complete set, with defaults for
the options not supplied. If no managed instance is running, `restart` starts
one with the supplied settings or defaults.

Each source identity is pinned when it is enrolled. If its path is replaced by
a symlink to another file, the event is rejected and reported rather than
registering or importing the new target. Foreground watcher processes remain
independent of the managed lease, so users can still run one explicitly while
the managed watcher is active; both may perform the same idempotent refresh.

`--on-vault-change` decides what happens when the vault copy was edited
(for example on a phone, synced back) since md2obs last wrote it:

| Policy | Behavior |
|---|---|
| `skip` (default) | Leave the edited vault copy alone; report the conflict. |
| `preserve` | Save the edited copy to `_External-Conflicts/YYYY-MM-DD/<name>--vault-edit.md`, then update. |
| `overwrite` | Replace the edited copy with the source. |

The check is a hash comparison against the last-written revision, done just
before each overwrite — the vault itself is never watched, so an Obsidian
edit alone produces no output; it is detected when the source next produces a
relevant filesystem event.

### list / history / status

`list` shows each source with its latest snapshot (`content: stale` means
the database intends a newer revision than the vault file actually contains,
e.g. after a skipped conflict). `history FILE` shows all dated snapshots for
one source. `status` shows configuration, database location, schema version,
counts, and the same managed-watcher state as `watch status`. `list` and
`history` are database queries only; status also inspects and, when necessary,
cleans the managed watcher record.

## Path safety

Before configuration is accepted and before each vault write, md2obs resolves
symlinks through the nearest existing path ancestor. A destination root or
date directory that redirects outside the vault is rejected. The same check
keeps the SQLite database and its WAL files physically outside the vault even
when `MD2OBS_STATE_DB` contains a symlinked parent. This is practical v1
hardening, not a race-free sandbox: another process that swaps directory
components between validation and the write can still create a TOCTOU race.

## Shell aliases

Fish:

```fish
function oi --description "Import Markdown into Obsidian"
    command md2obs $argv
end

function ow --description "Watch recently imported Markdown files"
    command md2obs watch $argv
end
```

Bash:

```bash
alias oi='md2obs'
alias ow='md2obs watch'
```

## Phone edits — read this once

Mobile access is assumed to be mostly read-only. If you edit a dated
snapshot on your phone:

- Obsidian Sync brings the edit back to the desktop vault;
- the next time the *source* changes, the watcher's `--on-vault-change`
  policy decides the outcome (default: your edit is kept and the conflict
  reported) — but an explicit `md2obs import` **always overwrites it**;
- the edit is never copied back to the original source file.

For anything you want to keep, duplicate the note into a normal folder
(e.g. `Working/`) before editing. Treat `_External` as managed output.

## Obsidian Sync validation (manual, once per setup)

1. Configure a vault with Obsidian Sync and set `vault_path` to it.
2. `md2obs import` a Markdown file from outside the vault.
3. Confirm it appears under `_External/<today>/` and Obsidian indexes it.
4. Confirm Sync uploads it and it appears on the phone.
5. Modify the source while `md2obs watch` runs (in the foreground or after
   `md2obs watch start`); confirm the same-day vault
   file updates (and syncs).
6. Create another Markdown file in the same source directory; confirm it is
   *not* imported.
7. Stop the watcher; confirm later source changes are not imported.

## Troubleshooting

- **`no vault configured`** — write the config file or set `MD2OBS_VAULT`.
- **`vault … does not exist`** — `vault_path` must point at an existing
  directory (the vault root, not a subfolder).
- **Watcher logs `cannot watch source directory`** — the source's directory is
  gone or inaccessible. Restore it and re-import the source to retry dynamic
  enrollment, or restart the watcher.
- **Watcher logs `source identity changed`** — the registered path now resolves
  through a symlink to a different file. Restore the original path or import
  the new target explicitly.
- **`notification queue overflowed`** — source changes or new enrollments may
  have been lost; re-run `md2obs import` on the affected files.
- **Import warns that running watchers may need to be restarted** — the import
  itself committed, but its cross-process watcher notification failed. Run
  `md2obs watch restart`, or fix the reported state-directory error and
  re-import.
- **A file was imported under a `--project--…` name you didn't expect** —
  another source with the same basename already owns the plain name for that
  date; see `md2obs list`.
- **Database locked** — another md2obs process holds a write; retries wait
  up to 5 s (`busy_timeout`), so this normally resolves itself.

## Platform support

Developed and integration-tested on Linux; the code is portable Go and macOS
is expected to work (platform paths are implemented). Windows is *not* yet a
claimed platform: rename-over-existing semantics need dedicated integration
tests first (see `internal/materialize/replace.go`).

## Design

The full design rationale lives in `md2obs-implementation-plan.md`. Short
version: SQLite (source → revision → snapshot → materialization) is the
operational source of truth; vault paths are derived, replaceable
materialization details; and the watcher reacts only to exact registered
source identities. The physical replacement occurs inside the SQLite
transaction, so a failed physical write rolls back database changes. SQLite
and the filesystem cannot commit atomically, however: a database failure after
a successful rename can leave the vault ahead until a later import or a future
`repair` command reconciles it.
