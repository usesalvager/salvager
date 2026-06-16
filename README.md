# `salvager` — Local History for agents

> Home: **[salvager.sh](https://salvager.sh)**

A filesystem-level code safety net. A passive watcher saves **per-file**
revisions automatically; when an agent (or a human) breaks something, you
recover it with one command. Designed to be consumed by the agent itself over
MCP, so it can self-repair.

## Why

An AI agent rewrites a file, deletes work you hadn't committed, or clobbers an
uncommitted change — and `git` can't bring it back, because it was never staged.
Salvager can: it has been quietly saving a revision of that file every time it
changed, so you `restore` it in one command. No commits, no checkpoints, no
configuration — just a running watcher and a recoverable history.

## Build

```sh
go build -o salvager .
```

Single static binary, no runtime. macOS and Linux supported; Windows best-effort.

A plain build reports its version as `dev` (`salvager --version` → `salvager dev`).
For a release, inject the version via ldflags:

```sh
CGO_ENABLED=0 go build -ldflags "-X 'github.com/usesalvager/salvager/version.Version=1.0.0'" -o salvager .
```

The same value backs both `salvager --version` and the version the MCP server
advertises to clients — one source of truth (`version.Version`).

## Quickstart

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

When something goes wrong, recover it. `restore` never destroys: before
overwriting the file it saves the current on-disk state as a `pre-restore`
revision, so any restore is itself reversible.

```
salvager history config.json          # find the good version
salvager show config.json 1718312445  # inspect it
salvager restore config.json 1718312445
# → prints the pre-restore timestamp to undo if needed
```

Timestamps printed by `history` are human-readable; the raw millisecond values
(needed for `show`/`restore`) are listed underneath.

`salvager gc` drops revisions older than N days (default 7) and garbage-collects
any object no longer referenced by any log. Run it manually or once a day.

## MCP

`salvager mcp` exposes exactly three tools over stdio:

- `salvager_list_versions` — list a file's versions (read-only)
- `salvager_get_version` — read one version's content (inspect before acting)
- `salvager_restore` — restore a version (returns the pre-restore timestamp)

No purge or delete is exposed over MCP — the safety net can't be erased by the
agent that might break things. Every tool is also contained to the project root:
a `file` argument that escapes the tree is refused before any read or write (see
[architecture](docs/architecture.md#mcp-path-containment)).

Register it with an MCP client (e.g. Claude Code):

```json
{
  "mcpServers": {
    "salvager": { "command": "salvager", "args": ["mcp"], "cwd": "/path/to/project" }
  }
}
```

## How it works

The watcher captures each file by **streaming** it through a fixed buffer, so
resident memory stays flat regardless of file size and there is no size cap that
excludes content. Revisions are content-addressed and deduplicated by SHA-256 in
a plain `.salvager/` tree you can read with `ls` and `cat`.

→ Full detail: [docs/architecture.md](docs/architecture.md) (capture model,
data layout, dedup, what's ignored, rename/symlink handling, retention).

## Coverage on large trees

Every OS real-time watch has a ceiling (`kqueue` fds, `inotify` watches). Any
directory the kernel won't watch in real time is handed to an automatic
**polling sweep** instead — coverage is "part real-time, part polling" and
always the whole tree, with no flag or action required. `--allow-partial` is the
one explicit opt-out; without it, Salvager refuses to start rather than run with
silent gaps.

→ Full detail: [docs/coverage.md](docs/coverage.md).

## Limitations

A few things to know: large single-line files (minified JSON/CSV) get a
less-distinctive at-a-glance signal but are always fully recoverable; overflow
subtrees under polling detect changes on the next sweep, not instantly; and on
coarse-timestamp filesystems (some NFS, FAT) the polling path can miss two
same-size writes within one tick until the next change. None affect the hash or
recoverability on the real-time path.

→ Full known-limits (including the two V2 residuals — racy-gate clock-mixing on
cross-host NFS and the single-line signature edge): [docs/limitations.md](docs/limitations.md).

## Performance

Measured, not asserted: idle CPU under 0.1% of a core, save→queryable p50 ≈
350 ms, whole-tree coverage until the OS watch ceiling — all backed by a
reproducible external harness in `bench/`, not patched into the watcher.

→ Tables, scaling sweep, and how to run it: [docs/performance.md](docs/performance.md).

## Scope (v1)

In: watcher, per-file store, list/get/restore/record, pre-restore safeguard,
polling sweep for over-cap subtrees (automatic full coverage) + `--allow-partial`
degradation policy, `.gitignore` + default excludes, CLI, MCP (3 tools),
age-based retention, external lightness/scaling benchmark harness (`bench/`).

Out: branches, merge, sync, cloud, accounts, config files, web UI, RBAC,
rendered diffs, explicit checkpoints, size-based retention.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
Copyright 2026 Somhi Lagunak SL.
