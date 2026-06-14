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
lochis watch [--allow-partial]  start the watcher (runs until killed)
lochis history <file>           list recorded versions of a file
lochis show <file> <ts>         print the content of one version
lochis restore <file> <ts>      restore a file to a version (reversible)
lochis mcp                      start the MCP server (stdio)
lochis gc [--max-age 7d]        purge revisions older than the threshold
```

Run `lochis watch` in the root of any project — zero configuration. It records
an initial revision of every tracked file on startup, then captures every
change (debounced ~300 ms) thereafter.

## Coverage on large trees (always whole)

Every OS real-time watch has a ceiling: macOS/BSD `kqueue` spends one file
descriptor per watched entry (`kern.maxfilesperproc`), and Linux `inotify`
spends one watch per directory (`fs.inotify.max_user_watches`). A tree large
enough to exhaust that ceiling — ~200k files hits the macOS default — would
otherwise leave the overflow directories unwatched: their files get an initial
snapshot and then **silently freeze**, never recording another edit.

Lochis closes that gap automatically. Any directory the kernel refuses to watch
in real time is handed to a **polling sweep** instead: a periodic
stat-based (mtime+size) reconciliation that re-enumerates the overflow subtree,
captures new files and edits, and lets content-hash dedup absorb the rest.
Coverage is "part real-time, part polling" and always the whole tree. This is
**automatic and silent** — no banner, no flag, no action required. The sweep is
near-zero disk I/O when idle (pure metadata) and backs off with the work it
finds, so it stays light even on a 200k-file tree.

`--allow-partial` is the one escape hatch. If polling is ever unavailable to
cover a real-time shortfall, Lochis **refuses to start** rather than run with
silent gaps — unless you pass `--allow-partial`, which means "I knowingly run
without full coverage." Partial coverage is only ever reachable by that explicit
choice; it is never the default and never silent.

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
polling sweep for over-cap subtrees (automatic full coverage) + `--allow-partial`
degradation policy, `.gitignore` + default excludes, CLI, MCP (3 tools),
age-based retention.

Out: branches, merge, sync, cloud, accounts, config files, web UI, RBAC,
rendered diffs, explicit checkpoints, size-based retention.

## Known limits (v1)

- **Large files** are read whole into memory on each capture — no streaming hash
  and no size cap. Multi-MB files are fine; files of hundreds of MB cause a
  memory spike per capture. (A documented size limit/skip is a candidate for v2.)
- **Real-time watch limit (macOS/Linux)**: each directory (kqueue: each entry)
  consumes a kernel resource. When the cap (`kern.maxfilesperproc` /
  `fs.inotify.max_user_watches`) is reached, the affected subtrees fall back to
  the polling sweep automatically — coverage stays whole (see "Coverage on large
  trees"). Raising the sysctl keeps more of the tree on the lower-latency
  real-time path, but is no longer required for correctness.
- **Polling latency / CPU at extreme scale**: overflow files are detected on the
  next sweep, not instantly, and a very large overflow region (tens of thousands
  of polled files) costs ~1.7 s of `lstat` per pass on an M2 (near-zero disk
  I/O). The sweep backs off to keep that under ~10% of a core. A directory-level
  recursive backend (macOS FSEvents) would remove the scan entirely; it is
  deferred — the cgo dependency and loss of the static binary are not justified
  by the measured disk cost, only by this CPU cost, which has not yet proven
  painful in practice.
- **Symlinks** are skipped, not followed (see above).
- **Long-running**: no known leak of memory or file descriptors; not yet
  validated by a multi-hour soak test.
