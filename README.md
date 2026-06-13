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

## Retention

`lochis gc` drops revisions older than N days (default 7) and garbage-collects
any object no longer referenced by any log. Run it manually or once a day.

## Scope (v1)

In: watcher, per-file store, list/get/restore/record, pre-restore safeguard,
`.gitignore` + default excludes, CLI, MCP (3 tools), age-based retention.

Out: branches, merge, sync, cloud, accounts, config files, web UI, RBAC,
rendered diffs, explicit checkpoints, size-based retention.
