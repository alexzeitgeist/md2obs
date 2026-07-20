# md2obs

`md2obs` imports explicitly selected Markdown files from arbitrary filesystem
locations into a dated folder inside an Obsidian vault. The imported files are
normal vault files, so Obsidian Sync distributes them to phones and tablets.

The original file stays where it is and remains the content source of truth.
The vault copy is a dated, derived snapshot — an import journal, not a
bidirectional sync. Nothing is ever discovered or imported automatically; the
only way a file enters the vault is `md2obs import` (or the watcher
re-importing a file you already imported).

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
md2obs import FILE...
md2obs watch [--days N] [--debounce DURATION] [--on-vault-change=POLICY]
md2obs list
md2obs history FILE
md2obs status
```

### import

```console
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

### watch

```console
md2obs watch                       # sources with snapshots dated today
md2obs watch --days 3              # today and the previous two days
md2obs watch --debounce 500ms      # per-source quiet period (default)
md2obs watch --on-vault-change=preserve
```

The watcher selects sources from the database, watches only their immediate
parent directories (non-recursively, via native filesystem notifications),
and re-imports a source after its events settle. It never scans directories,
never imports unrelated files, never rewrites anything at startup, and does
no polling — idle, it consumes effectively no CPU. Stop it with Ctrl-C.

`--on-vault-change` decides what happens when the vault copy was edited
(for example on a phone, synced back) since md2obs last wrote it:

| Policy | Behavior |
|---|---|
| `skip` (default) | Leave the edited vault copy alone; report the conflict. |
| `preserve` | Save the edited copy to `_External-Conflicts/YYYY-MM-DD/<name>--vault-edit.md`, then update. |
| `overwrite` | Replace the edited copy with the source. |

The check is a hash comparison against the last-written revision, done just
before each overwrite — the vault itself is never watched, so an Obsidian
edit alone produces no output; it is detected when the source next changes.

### list / history / status

`list` shows each source with its latest snapshot (`content: stale` means
the database intends a newer revision than the vault file actually contains,
e.g. after a skipped conflict). `history FILE` shows all dated snapshots for
one source. `status` shows configuration, database location, schema version,
and counts. All three are database queries only.

## Shell aliases

Fish:

```fish
function oi --description "Import Markdown into Obsidian"
    command md2obs import $argv
end

function ow --description "Watch recently imported Markdown files"
    command md2obs watch $argv
end
```

Bash:

```bash
alias oi='md2obs import'
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
5. Modify the source while `md2obs watch` runs; confirm the same-day vault
   file updates (and syncs).
6. Create another Markdown file in the same source directory; confirm it is
   *not* imported.
7. Stop the watcher; confirm later source changes are not imported.

## Troubleshooting

- **`no vault configured`** — write the config file or set `MD2OBS_VAULT`.
- **`vault … does not exist`** — `vault_path` must point at an existing
  directory (the vault root, not a subfolder).
- **Watcher prints `parent directory missing`** — the source's directory is
  gone; the source is skipped for this session. Re-import after it returns.
- **`notification queue overflowed`** — events may have been lost; re-run
  `md2obs import` on the files you changed.
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
materialization details; the watcher reacts only to exact registered paths;
and the import transaction wraps the atomic vault write so a failed write
rolls everything back.
