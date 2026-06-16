# Coverage on large trees (always whole)

> Reference for the real-time watch ceiling, the polling sweep, and the
> `--allow-partial` escape hatch. Back to the [README](../README.md).

Every OS real-time watch has a ceiling: macOS/BSD `kqueue` spends one file
descriptor per watched entry (`kern.maxfilesperproc`), and Linux `inotify`
spends one watch per directory (`fs.inotify.max_user_watches`). A tree large
enough to exhaust that ceiling — ~200k files hits the macOS default — would
otherwise leave the overflow directories unwatched: their files get an initial
snapshot and then **silently freeze**, never recording another edit.

## The polling sweep

Salvager closes that gap automatically. Any directory the kernel refuses to watch
in real time is handed to a **polling sweep** instead: a periodic
stat-based (mtime+size) reconciliation that re-enumerates the overflow subtree,
captures new files and edits, and lets content-hash dedup absorb the rest.
Coverage is "part real-time, part polling" and always the whole tree. This is
**automatic and silent** — no banner, no flag, no action required. The sweep is
near-zero disk I/O when idle (pure metadata) and backs off with the work it
finds, so it stays light even on a 200k-file tree.

## `--allow-partial`

`--allow-partial` is the one escape hatch. If polling is ever unavailable to
cover a real-time shortfall, Salvager **refuses to start** rather than run with
silent gaps — unless you pass `--allow-partial`, which means "I knowingly run
without full coverage." Partial coverage is only ever reachable by that explicit
choice; it is never the default and never silent.

See [performance](performance.md) for the measured live-watch coverage at 20k /
100k / 200k files, and [limitations](limitations.md) for the polling-latency and
coarse-timestamp residuals.
