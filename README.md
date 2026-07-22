# md2obs

Copy selected Markdown files into dated folders in an Obsidian vault.

The original file remains the source. md2obs writes a one-way copy to
`_External/YYYY-MM-DD/`, where Obsidian can index and sync it. It only imports
files you name; it never scans directories for Markdown files.

## Quick start

Build and install:

```console
go build -o md2obs ./cmd/md2obs
install -m 0755 md2obs ~/.local/bin/
```

Tell md2obs where your vault is, in `~/.config/md2obs/config.json` on Linux
(the macOS path is under Configuration below):

```json
{
  "vault_path": "/home/alice/obsidian/vault"
}
```

Import a file, then optionally watch it for changes:

```console
md2obs ~/notes/meeting.md
md2obs watch
```

The first command creates a copy such as `_External/2026-07-22/meeting.md`.
The watcher re-imports the file each time it changes; stop it with Ctrl-C.

## Commands

| Command | Purpose |
|---|---|
| `md2obs FILE...` | Import files into today's folder. `import FILE...` is the same. |
| `md2obs refresh` | Check tracked sources once and import changes. |
| `md2obs watch` | Keep re-importing tracked sources until stopped. |
| `md2obs untrack` | Stop tracking sources. Vault copies stay. |
| `md2obs version` | Report the installed version and source commit. |
| `md2obs debug` | Inspect configuration and state. |

Run `md2obs COMMAND --help` for options.

## Import

```console
md2obs ~/projects/a/README.md /tmp/response.md
```

An import reports one of three results:

- `imported`: created today's copy.
- `updated`: replaced today's copy with the current source.
- `unchanged`: the source content has not changed.

On a later day, the next import creates a new dated copy. An explicit import
always restores the source content, even if today's vault copy was edited.

If two sources share a name, the later one gets parent directories in its
name: `README--project-b.md`. A vault file md2obs does not own is never
overwritten; the import picks a numbered name such as `README-1.md` instead.

## Watch

```console
md2obs watch
md2obs watch --days 3
md2obs watch --on-vault-change=preserve
```

`watch` re-imports tracked sources as they change, using filesystem
notifications; it never scans directories. It starts with the sources
imported today (`--days 3` adds the two previous days), and files you import
while it runs join automatically. It stays in the foreground until you stop
it with Ctrl-C.

A deleted source stays tracked, so files that disappear briefly (a branch
switch, an unmounted drive, an editor replacing the file) resume watching
when they return. If a source's directory disappears, the watcher retries for
a short while and recovers on its own if the directory comes back; after
that, import the file again or restart the watcher.

On macOS, if a tracked source is replaced by a symlink and later restored as
a regular file, the running watcher may not observe the restoration. The
replacement symlink is still rejected; run `md2obs refresh` or restart
`md2obs watch` after restoring the original path.

Only one watcher can run per vault and state database. `--debounce DURATION`
sets how long a change must settle before the copy is made (default 500ms).

## Refresh

```console
md2obs refresh
md2obs refresh --days 3
md2obs refresh --all
```

`refresh` is the one-shot version of `watch`: it checks tracked sources once,
imports the changed ones, prints a summary, and exits. Run it to catch up
after the watcher was off, or when it reported lost filesystem events.

`--days` counts from the date a source was last imported into this vault, not
from the source file's modification time. If you do not know how long the
watcher was off, use `--all`.

Missing sources are counted in the summary but stay tracked.

## Edited vault copies

If you edit a dated copy in Obsidian (say on your phone, synced back to the
desktop), `watch` and `refresh` will not silently replace it. Choose what
happens with `--on-vault-change`:

| Policy | Result |
|---|---|
| `skip` (default) | Keep the vault edit and report a conflict. |
| `preserve` | Save the vault edit under `_External-Conflicts/`, then update the copy. |
| `overwrite` | Replace the vault edit with the source. |

An explicit `md2obs FILE...` always overwrites its managed copy, and vault
edits are never copied back to the source file. Treat `_External` as managed
output: move or copy a note to another vault folder before editing anything
you want to keep.

## Untrack

`untrack` makes `watch` and `refresh` forget a source. Vault copies are never
deleted; importing the source again starts tracking it again.

```console
md2obs untrack ~/projects/a/README.md
md2obs untrack --missing
md2obs untrack --older-than 90d
md2obs untrack --missing --older-than 30d --dry-run
```

Batch selectors (a source must match all of them):

- `--missing`: the source path is gone while its parent directory is
  readable. If the parent is missing or unreadable too, the source stays
  tracked. md2obs cannot tell an unmounted drive from a deletion when the
  empty mount point is still readable, so run `--dry-run` first.
- `--older-than 90d`: the newest vault copy is older than 90 calendar days.
- `--dry-run`: show what would be untracked without changing anything.

## Debug commands

| Command | Output |
|---|---|
| `md2obs debug list` | Tracked sources and their latest vault paths. |
| `md2obs debug history FILE` | Stored snapshot records for one source. |
| `md2obs debug status` | Resolved paths, schema version, and state counts. |

Debug commands read the state database; they do not import files.

## Configuration

md2obs reads `~/.config/md2obs/config.json` on Linux and
`~/Library/Application Support/md2obs/config.json` on macOS.

| Field | Meaning |
|---|---|
| `vault_path` | Existing Obsidian vault directory. Required. |
| `layout` | Destination layout. Only `dated-flat-v1` exists; it is the default. |
| `root_directory` | Destination folder inside the vault. Defaults to `_External`. |

`root_directory` is relative to the vault and cannot be the vault root or
point outside it. No part of it may start with a dot, because Obsidian hides
dot-folders and the imports would be invisible.

Two environment variables override the configuration for scripting:
`MD2OBS_VAULT` replaces `vault_path`, and `MD2OBS_STATE_DB` sets the state
database path, by default `~/.local/share/md2obs/state.db` on Linux and
`~/Library/Application Support/md2obs/state.db` on macOS. A database path
inside the vault is rejected: Obsidian Sync would pick up the live database
files.

## Troubleshooting

| Problem | What to do |
|---|---|
| `no vault configured` | Create the config file or set `MD2OBS_VAULT`. |
| `vault … does not exist` | Point `vault_path` at an existing vault root, not a subfolder. |
| `another watcher is already running` | Stop the other watcher with Ctrl-C. |
| `cannot watch source directory` | Restore the directory; the watcher retries on its own for a while. After retries stop, import the file again or restart the watcher. |
| `source identity changed` | The path now leads to a different file. Restore the original, or import the new one explicitly. |
| `notification queue overflowed` | Run `md2obs refresh --all`. |
| A source is missing but still tracked | Restore it, or run `md2obs untrack FILE`. |
| A vault copy was deleted | Import the source again. Historical copies cannot be rebuilt. |
| A name contains extra directory parts | Another source already uses the shorter name. Check `md2obs debug list`. |
| A command reports a database failure | Run it again. Imports are safe to retry. |

## Platform support

Linux is supported and tested. macOS is expected to work. Windows is not yet
supported.

## License

md2obs is available under the [MIT License](LICENSE). Binary distributions
must also include the notices in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
