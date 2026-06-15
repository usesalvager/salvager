# `salvager` — Local History for agents

> Home: **[salvager.sh](https://salvager.sh)**

A filesystem-level code safety net. A passive watcher saves **per-file**
revisions automatically; when an agent (or a human) breaks something, you
recover it with one command. Designed to be consumed by the agent itself over
MCP, so it can self-repair.

## Build

```sh
go build -o salvager .
```

Single static binary, no runtime. macOS and Linux supported; Windows best-effort.

## Usage

```
salvager watch [--allow-partial]  start the watcher (runs until killed)
salvager history <file>           list recorded versions of a file
salvager show <file> <ts>         print the content of one version
salvager restore <file> <ts>      restore a file to a version (reversible)
salvager mcp                      start the MCP server (stdio)
salvager gc [--max-age 7d]        purge revisions older than the threshold
```

Run `salvager watch` in the root of any project — zero configuration. It records
an initial revision of every tracked file on startup, then captures every
change (debounced ~300 ms) thereafter.

## Coverage on large trees (always whole)

Every OS real-time watch has a ceiling: macOS/BSD `kqueue` spends one file
descriptor per watched entry (`kern.maxfilesperproc`), and Linux `inotify`
spends one watch per directory (`fs.inotify.max_user_watches`). A tree large
enough to exhaust that ceiling — ~200k files hits the macOS default — would
otherwise leave the overflow directories unwatched: their files get an initial
snapshot and then **silently freeze**, never recording another edit.

Salvager closes that gap automatically. Any directory the kernel refuses to watch
in real time is handed to a **polling sweep** instead: a periodic
stat-based (mtime+size) reconciliation that re-enumerates the overflow subtree,
captures new files and edits, and lets content-hash dedup absorb the rest.
Coverage is "part real-time, part polling" and always the whole tree. This is
**automatic and silent** — no banner, no flag, no action required. The sweep is
near-zero disk I/O when idle (pure metadata) and backs off with the work it
finds, so it stays light even on a 200k-file tree.

`--allow-partial` is the one escape hatch. If polling is ever unavailable to
cover a real-time shortfall, Salvager **refuses to start** rather than run with
silent gaps — unless you pass `--allow-partial`, which means "I knowingly run
without full coverage." Partial coverage is only ever reachable by that explicit
choice; it is never the default and never silent.

Timestamps printed by `history` are human-readable; the raw millisecond values
(needed for `show`/`restore`) are listed underneath.

## How it recovers

`restore` never destroys: before overwriting the file it saves the current
on-disk state as a `pre-restore` revision, so any restore is itself reversible.

```
salvager history config.json          # find the good version
salvager show config.json 1718312445  # inspect it
salvager restore config.json 1718312445
# → prints the pre-restore timestamp to undo if needed
```

## MCP

`salvager mcp` exposes exactly three tools over stdio:

- `salvager_list_versions` — list a file's versions (read-only)
- `salvager_get_version` — read one version's content (inspect before acting)
- `salvager_restore` — restore a version (returns the pre-restore timestamp)

No purge or delete is exposed over MCP — the safety net can't be erased by the
agent that might break things.

Register it with an MCP client (e.g. Claude Code):

```json
{
  "mcpServers": {
    "salvager": { "command": "salvager", "args": ["mcp"], "cwd": "/path/to/project" }
  }
}
```

## Data layout (`.salvager/`)

Readable without the tool — `ls` and `cat` recover anything by hand:

```
.salvager/
├── objects/<sha256>          full content, deduplicated by hash
└── index/<relpath>.log       one line per revision (tab-separated):
                              <unix_ms>\t<sha256>\t<label>\t<lines>\t<bytes>\t<delta>\t<start-signature>
```

Each line carries a content signal computed once at capture — total `<lines>`
and `<bytes>`, the signed line `<delta>` vs the previous revision (`?` when that
revision predates the signal), and a Go-quoted start signature (first non-empty
lines). It lets `history` and the MCP `list_versions` tool say which revision
holds a given block of work without re-reading any object. Legacy lines written
before the signal keep the older three-column form (`<unix_ms>\t<sha256>\t<label>`)
and are shown as "signal unavailable".

Labels: `first-seen` · `modify` · `delete` · `pre-restore` · `restore`.
(`first-seen` is the first captured revision — it already holds work, it is not
an empty baseline.)

## What's ignored

The project's `.gitignore` plus always-on defaults: `.git`, `.salvager`,
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
fully recoverable under the old one (`salvager history old.txt` / `restore`).

## Retention

`salvager gc` drops revisions older than N days (default 7) and garbage-collects
any object no longer referenced by any log. Run it manually or once a day.

## Performance (measured)

The claims above — featherweight to leave running, whole-tree coverage until the
OS watch ceiling — are backed by a reproducible, external harness, not asserted.
Nothing in `bench/` patches the watcher; every figure is observed the way an
operator would see it (process CPU time, kernel watch/fd counts, save→queryable
latency). Method and honesty conditions: `bench/PROTOCOL.md`.

```sh
(cd .. && go build -o salvager .)   # build the binary the harness exercises
bench/run.sh                      # one tree → bench/RESULTS.md
bench/sweep.sh                    # 20k / 100k / 200k → bench/SCALING.md
go test ./store -bench=. -benchmem -run=^$   # store per-revision cost, no fs events
```

Scaling sweep on an Apple M2 Pro (`go1.25.0`, `kern.maxfilesperproc=122880`),
unique content per file so the store never deduplicates (a floor on speed,
ceiling on work — real repos capture faster):

| Files / dirs | Cold capture | RSS idle | CPU idle | Save→queryable p50/p95 | Live watch coverage |
|---|---|---|---|---|---|
| 20k / 2k | 8.7 s (2310 files/s) | 28 MB | 0.10% of a core | 357 / 426 ms | 100% |
| 100k / 10k | 72 s (1381 files/s) | 73 MB | 0.07% of a core | 365 / 451 ms | 100% |
| 200k / 20k | 316 s | 49 MB | 0.07% of a core | — | **55.8%** |

Read three things off this:

- **Idle CPU is ~zero.** Quiescent, the watcher burns well under 0.1% of one
  core at every size — the only scheduled work is the 100 ms debounce ticker.
- **Latency is debounce-bound, not overhead.** p50 ≈ 350 ms is the intentional
  ~300 ms write-burst debounce plus one ≤100 ms tick; it does not grow with the
  tree.
- **macOS coverage is fd-bound, and the sweep shows exactly where.** At 200k
  files the per-process fd cap is exhausted (`kern.maxfilesperproc=122880`,
  129 watch-add failures), so live-watch coverage drops to 55.8% — the overflow
  subtrees fall to the polling sweep automatically (see "Coverage on large
  trees"), still whole, just not real-time. Raising the sysctl keeps more of the
  tree on the lower-latency path. Linux counts watches per *directory*, so file
  count barely moves the ceiling there.

Numbers are comparative on the host in the stamp, not a universal claim — publish
the host line with any table. See `bench/RESULTS.md` and `bench/SCALING.md` for
the full stamped output.

## Scope (v1)

In: watcher, per-file store, list/get/restore/record, pre-restore safeguard,
polling sweep for over-cap subtrees (automatic full coverage) + `--allow-partial`
degradation policy, `.gitignore` + default excludes, CLI, MCP (3 tools),
age-based retention, external lightness/scaling benchmark harness (`bench/`).

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
- **Coarse-timestamp / NFS polling residual**: the polling fallback detects
  changes by stat (mtime+ctime+size). On a filesystem with ≥1s timestamp
  resolution — some NFS mounts (e.g. NFSv3), FAT — two same-size in-place writes
  to one file within a single tick can be missed until the next stat-moving
  change. This only applies to overflow subtrees under polling (not the
  real-time path) and self-heals on the next change; nanosecond-resolution
  filesystems (ext4/overlay in typical Linux containers, APFS) are unaffected.
  Relevant mainly to NFS-backed working trees on the cloud path.
- **Symlinks** are skipped, not followed (see above).
- **Long-running**: no known leak of memory or file descriptors; not yet
  validated by a multi-hour soak test.
