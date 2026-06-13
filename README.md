# `lochis` — Local History for agents

A filesystem-level code safety net. A passive watcher saves **per-file**
revisions automatically; when an agent (or a human) breaks something, you
recover it with one command. Designed to be consumed by the agent itself over
MCP, so it can self-repair.

## Build

```sh
go build -o lochis .
```

Single static binary, no runtime. macOS and Linux supported; Windows best-effort.

## Usage

```
lochis watch                    start the watcher (runs until killed)
lochis history <file>           list recorded versions of a file
lochis show <file> <ts>         print the content of one version
lochis restore <file> <ts>      restore a file to a version (reversible)
lochis mcp                      start the MCP server (stdio)
lochis gc [--max-age 7d]        purge revisions older than the threshold
```

Run `lochis watch` in the root of any project — zero configuration. It records
an initial revision of every tracked file on startup, then captures every
change (debounced ~300 ms) thereafter.

Timestamps printed by `history` are human-readable; the raw millisecond values
(needed for `show`/`restore`) are listed underneath.

## How it recovers

`restore` never destroys: before overwriting the file it saves the current
on-disk state as a `pre-restore` revision, so any restore is itself reversible.

```
lochis history config.json          # find the good version
lochis show config.json 1718312445  # inspect it
lochis restore config.json 1718312445
# → prints the pre-restore timestamp to undo if needed
```

## MCP

`lochis mcp` exposes exactly three tools over stdio:

- `lochis_list_versions` — list a file's versions (read-only)
- `lochis_get_version` — read one version's content (inspect before acting)
- `lochis_restore` — restore a version (returns the pre-restore timestamp)

No purge or delete is exposed over MCP — the safety net can't be erased by the
agent that might break things.

Register it with an MCP client (e.g. Claude Code):

```json
{
  "mcpServers": {
    "lochis": { "command": "lochis", "args": ["mcp"], "cwd": "/path/to/project" }
  }
}
```

## Data layout (`.lochis/`)

Readable without the tool — `ls` and `cat` recover anything by hand:

```
.lochis/
├── objects/<sha256>          full content, deduplicated by hash
└── index/<relpath>.log       one line per revision: <unix_ms>\t<sha256>\t<label>
```

Labels: `initial` · `modify` · `delete` · `pre-restore` · `restore`.

## What's ignored

The project's `.gitignore` plus always-on defaults: `.git`, `.lochis`,
`node_modules`, `vendor`, `.venv`, `__pycache__`, `target`, `dist`, `build`.

Transient editor artifacts are ignored too — swap, autosave, lock and backup
files (`*.swp`, `*~`, `.#*`, `#*#`, `4913`, `.goutputstream-*`, `.~lock.*#`).
The common atomic save (write a temp file, rename it over the target) is
captured cleanly with no junk history; these patterns additionally suppress the
long-lived temps (e.g. vim's `.swp` open for a whole session).

Symlinks are never followed — a link could point outside the project or form a
loop, so its path is skipped. An in-project file that a link points to is still
versioned under its own real path, so nothing is lost.

Renaming a file is recorded as a delete of the old path plus a fresh history at
the new path — history is **not** transferred to the new path, but it stays
fully recoverable under the old one (`lochis history old.txt` / `restore`).

## Retention

`lochis gc` drops revisions older than N days (default 7) and garbage-collects
any object no longer referenced by any log. Run it manually or once a day.

## Scope (v1)

In: watcher, per-file store, list/get/restore/record, pre-restore safeguard,
`.gitignore` + default excludes, CLI, MCP (3 tools), age-based retention.

Out: branches, merge, sync, cloud, accounts, config files, web UI, RBAC,
rendered diffs, explicit checkpoints, size-based retention.

## Known limits (v1)

- **Large files** are read whole into memory on each capture — no streaming hash
  and no size cap. Multi-MB files are fine; files of hundreds of MB cause a
  memory spike per capture. (A documented size limit/skip is a candidate for v2.)
- **inotify watch limit (Linux)**: each directory consumes one watch. If
  `fs.inotify.max_user_watches` is reached, the watcher logs the failed
  `watch add` for the affected directories rather than failing silently — raise
  the sysctl for very large trees.
- **Symlinks** are skipped, not followed (see above).
- **Long-running**: no known leak of memory or file descriptors; not yet
  validated by a multi-hour soak test.
