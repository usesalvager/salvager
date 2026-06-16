# Architecture — how the engine works

> Reference for the capture engine, storage layout, and what gets ignored.
> Back to the [README](../README.md).

Salvager is a passive watcher that saves **per-file** revisions automatically. It
records an initial revision of every tracked file on startup, then captures every
change (debounced ~300 ms) thereafter.

## Capture model (streaming, no size cap)

Each capture **streams** the file through a fixed buffer, so resident memory
stays flat regardless of file size — a multi-hundred-MB dataset is captured
without a memory spike. By design there is **no size cap that excludes content**:
the watcher captures everything (a silent content hole would contradict the
guarantee), and disk is bounded by retention instead.

Resident memory is therefore O(buffer), independent of file size. See
[limitations](limitations.md) for the one cosmetic edge (start signature on a
long single-line file).

## Data layout (`.salvager/`)

Readable without the tool — `ls` and `cat` recover anything by hand:

```
.salvager/
├── objects/<sha256>          full content, deduplicated by hash
└── index/<relpath>.log       one line per revision (tab-separated):
                              <unix_ms>\t<sha256>\t<label>\t<lines>\t<bytes>\t<delta>\t<start-signature>
```

Content is **content-addressed**: each object is stored once under its SHA-256,
so identical content across revisions or files is **deduplicated** automatically.

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

## Retention and garbage collection

`salvager gc` drops revisions older than N days (default 7) and garbage-collects
any object no longer referenced by any log. Because objects are deduplicated by
hash, an object is only removed once **no** log line anywhere references it. Run
it manually or once a day.

## MCP path containment

Every MCP tool is contained to the project root: a `file` argument that escapes
the tree (`../`, an absolute path, or empty) is refused with a structured error
before any read, write, or delete — the MCP can never reach a file outside the
watched project. `list_versions` on a file with no history is a success, not an
error: it returns `tracked: false` with an empty `versions` list.

See the [README MCP section](../README.md#mcp) for the three tools and client
registration.
