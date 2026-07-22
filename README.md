# md2obs

`md2obs` imports explicitly selected Markdown files from arbitrary filesystem
locations into a dated folder inside an Obsidian vault. The imported files are
normal vault files, so Obsidian Sync distributes them to phones and tablets.

The original file stays where it is and remains the content source of truth.
The vault copy is a dated, derived snapshot — an import journal, not a
bidirectional sync. Nothing is ever discovered or imported automatically; the
only way a file enters the vault is an explicit `md2obs FILE...` invocation
(`md2obs import FILE...` is equivalent), or the watcher re-importing a file
you already imported. `md2obs refresh` can perform the same re-import check
once for previously imported sources without starting a watcher.

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
md2obs refresh [--days N | --all] [--on-vault-change=POLICY]
md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
md2obs untrack FILE...
md2obs untrack [--missing] [--older-than AGE] [--dry-run]
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

### refresh

```console
md2obs refresh                         # sources materialized today
md2obs refresh --days 3                # today and the previous two days
md2obs refresh --all                   # every source ever materialized here
md2obs refresh --all --on-vault-change=preserve
```

`refresh` is a one-shot catch-up pass for sources already materialized in the
configured vault. It uses the same vault-scoped candidate selection and pinned
source identities as the watcher, hashes each selected source, and imports only
sources whose current content differs from their selected snapshot. It never
scans source directories or discovers unrelated Markdown files. Missing
registered sources are counted in the summary but are not fatal; identity
changes, read errors, and import failures are reported per source and make the
command return a non-zero status after the remaining candidates are checked.

`--days N` selects sources by the date of their materialization in this vault,
not by an unobserved source change time. For example, a source last imported ten
days ago is not selected by `--days 3` even if it changed yesterday. Use `--all`
for catch-up after an unknown amount of downtime or after a filesystem
notification overflow. `--days` and `--all` are mutually exclusive.

Like the watcher, refresh defaults to `--on-vault-change=skip`; `preserve` and
`overwrite` have the same meanings described below. The policy is consulted
only when the source changed and refresh would replace an existing
materialization for today's snapshot in this vault. When catch-up creates a new
dated snapshot, earlier vault copies — including edits — remain untouched and
are not treated as conflicts. If the source still matches its selected
snapshot, refresh does not inspect, restore, or overwrite an edited or deleted
vault copy. This is intentional: refresh catches up source changes; it does not
audit historical vault materializations or reconstruct intermediate versions
that existed while no watcher was running.

A completed pass notifies a running foreground watcher once so sources newly
materialized today can join its session.

### watch

```console
md2obs watch                         # sources imported today
md2obs watch --days 3                # today and the previous two days
md2obs watch --debounce 500ms        # per-source quiet period (default)
md2obs watch --on-vault-change=preserve
```

The watcher selects recent sources materialized in the configured vault,
watches only their immediate parent directories (non-recursively, via native
filesystem notifications), and re-imports a source after its events settle.
Membership is vault-scoped: imports made only into another vault sharing the
same state database are not watched or materialized here.

Successful explicit imports into this vault automatically join an already
running watcher, including imports from directories it was not previously
watching. A watcher started with no eligible sources stays running and waits
for imports. Membership grows with successful imports and is not reduced merely
because the date window advances; an explicit `untrack` removes a source from
the live session. A new invocation recalculates the window. The discovery range
expands across midnight so an import immediately before midnight is not missed
when its notification arrives just after midnight. If a selected source
directory disappears, the watcher retries briefly with backoff. If the
directory returns during that window, md2obs restores the native watch and
checks the affected sources for changes. After retries stop, import the source
again or restart the watcher once its directory is available.

Startup is passive and never rewrites existing vault copies. A source enrolled
after startup gets one silent content check after its directory watch is armed,
closing the small import-to-watch race; matching content causes no vault write
and does not evaluate `--on-vault-change`. The watcher never scans directories,
never imports unrelated files, and does no polling while its directory watches
are healthy or after a recovery window ends — idle, it consumes effectively no
CPU. `md2obs watch` stays in the foreground until interrupted; stop it with
Ctrl-C. Run `md2obs refresh` for an explicit one-shot source catch-up before or
after a watch session.

Only one watcher may run for each resolved `(state database, vault)` pair on
Linux and macOS. A second invocation exits with an error. The operating system
releases the foreground watcher's lock when it exits, including after an
abnormal termination; md2obs stores no process record or watcher settings.

Each source identity is pinned when it is enrolled. If its path is replaced by
a symlink to another file, the event is rejected and reported rather than
registering or importing the new target.

Deleting a source does not implicitly untrack it. The watcher keeps the exact
registered path so temporary removals, atomic replacements, branch changes,
and later recreation can recover without losing the user's explicit source
selection. Use `md2obs untrack` when tracking should stop.

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

### untrack

```console
md2obs untrack ~/projects/a/README.md
md2obs untrack --missing
md2obs untrack --older-than 90d
md2obs untrack --missing --older-than 30d
md2obs untrack --missing --dry-run
```

`untrack` explicitly stops automatic watch and refresh for sources in the
configured vault. It changes only vault-scoped tracking state: source history,
dated snapshots, and existing vault files are preserved. Importing the source
again reactivates it in that vault.

Named paths may be untracked whether they still exist or not. A path shown by
`md2obs list` can be passed back to `untrack`, including a missing source that
was originally imported through a symlink. Named and batch selection cannot be
combined in one invocation.

`--missing` selects an exact source only when that path is absent and its
immediate parent can be read. If the parent is missing or inaccessible, the
source is reported as unavailable and remains tracked; this avoids interpreting
an unmounted volume or permissions problem as deletion. `--dry-run` reports the
same decisions without changing tracking state.

`--older-than AGE` selects sources whose newest materialized snapshot in this
vault is older than the given number of local calendar days. Ages use whole-day
syntax such as `30d` or `365d`; source and vault filesystem modification times
are not consulted. When `--missing` and `--older-than` are combined, both
conditions must match. Untracking by age is operational pruning, not history or
database cleanup.

An untrack operation notifies a running foreground watcher, which removes the
source from its live membership. If notification fails, the database change is
kept and a warning asks for a watcher restart.

When upgrading a schema-v2 database, md2obs reactivates inactive tracking rows
once because that version could create them only from automatic live-deletion
inference. In schema v3 and later, inactive rows represent explicit `untrack`
operations and persist across restarts.

### list / history / status

`list` shows each source with its vault-scoped tracking state and latest
snapshot. `tracking: inactive` means automatic watch and refresh were explicitly
disabled; it does not mean history was deleted. `content: stale` means that the
snapshot references a different revision from the last revision md2obs recorded
writing at that path, for example after a skipped conflict. It is a database-state
label, not the result of inspecting the current vault file. `history FILE` shows
all dated snapshots for one source. `status` shows configuration, database
location, schema version, and counts. All three are database queries only.

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
5. Modify the source while `md2obs watch` runs; confirm the same-day vault file
   updates (and syncs).
6. Create another Markdown file in the same source directory; confirm it is
   *not* imported.
7. Stop the watcher; confirm later source changes are not imported.

## Troubleshooting

- **`no vault configured`** — write the config file or set `MD2OBS_VAULT`.
- **`vault … does not exist`** — `vault_path` must point at an existing
  directory (the vault root, not a subfolder).
- **Watcher logs `cannot watch source directory`** — the source's directory is
  gone or inaccessible. Restore it and restart the watcher, or explicitly
  import the source to trigger a live membership retry.
- **A source was deleted but still appears as tracked** — absence does not
  revoke explicit source selection. Restore the source to resume automatic
  imports, or run `md2obs untrack FILE` to stop tracking it.
- **`untrack --missing` reports a source as unavailable** — its parent could
  not be read, so md2obs kept it tracked. Restore the mount or permissions and
  retry, or explicitly name the source with `md2obs untrack FILE`.
- **Watcher logs `source identity changed`** — the registered path now resolves
  through a symlink to a different file. Restore the original path or import
  the new target explicitly.
- **`another watcher is already running`** — stop the foreground watcher in its
  existing terminal with Ctrl-C before starting another for the same database
  and vault. After upgrading from a version with the background daemon, kill
  any leftover daemon process from the previous version.
- **`notification queue overflowed`** — the watcher refreshes its membership
  automatically, but source changes may have been lost; run
  `md2obs refresh --all` or re-run the affected `import`.
- **Import warns that running watchers may need to be restarted** — the import
  itself committed, but its cross-process watcher notification failed. Stop and
  run `md2obs watch` again, or fix the reported state-directory error and
  re-import.
- **Import reports a database or commit failure** — the vault and SQLite cannot
  commit atomically, so a successful vault write may outlive a later database
  failure. Re-run the same import to converge on the current source content.
- **A dated vault copy was deleted** — re-run an explicit import to materialize
  the current source for today. md2obs does not archive revision contents or
  reconstruct deleted historical snapshots.
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

SQLite records operational registry and materialization metadata
(source → revision → snapshot → materialization), while the original file
remains the content source of truth. Vault paths are derived, replaceable
materialization details, and the watcher reacts only to exact registered source
identities. Source enrollment is explicit: import activates automatic tracking,
untrack deactivates it, and filesystem absence alone changes neither history nor
tracking intent.

The physical replacement occurs inside the SQLite transaction, so a failed
physical write rolls back database changes. SQLite and the filesystem cannot
commit atomically, however: a database failure after a successful rename can
leave the vault ahead of the recorded state. Imports are safe to retry and
converge on the source's current content. md2obs does not retain revision bytes
or guarantee reconstruction of deleted historical snapshots.
