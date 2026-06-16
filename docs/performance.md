# Performance (measured)

> Reference for the measured figures and how to reproduce them. Back to the
> [README](../README.md).

The claims elsewhere — featherweight to leave running, whole-tree coverage until
the OS watch ceiling — are backed by a reproducible, external harness, not
asserted. Nothing in `bench/` patches the watcher; every figure is observed the
way an operator would see it (process CPU time, kernel watch/fd counts,
save→queryable latency). Method and honesty conditions: `bench/PROTOCOL.md`.

## Running the harness

```sh
(cd .. && go build -o salvager .)   # build the binary the harness exercises
bench/run.sh                      # one tree → bench/RESULTS.md
bench/sweep.sh                    # 20k / 100k / 200k → bench/SCALING.md
go test ./store -bench=. -benchmem -run=^$   # store per-revision cost, no fs events
```

## Scaling sweep

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
  subtrees fall to the polling sweep automatically (see
  [coverage](coverage.md)), still whole, just not real-time. Raising the sysctl
  keeps more of the tree on the lower-latency path. Linux counts watches per
  *directory*, so file count barely moves the ceiling there.

Numbers are comparative on the host in the stamp, not a universal claim — publish
the host line with any table. See `bench/RESULTS.md` and `bench/SCALING.md` for
the full stamped output.
