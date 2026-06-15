# salvager lightness benchmark — protocol

A reproducible way to measure what makes `salvager watch` cheap to leave running,
and to publish the numbers honestly. Nothing here patches the watcher: every
metric is observed from the outside, the way an operator would see it.

## What is measured

| # | Metric | Question it answers | Design floor |
|---|---|---|---|
| 1 | Initial capture (cold) | How long until *every* file has a recovery point on a fresh start? | bounded by disk read + sha256 + object write of the whole tree |
| 2 | Watches consumed | How much kernel watch/fd budget does it hold? | one per watched path (see OS note) |
| 3 | CPU at rest | What does it cost to leave running on a quiet tree? | the 100 ms ticker — the only idle work |
| 4 | Save → queryable latency | From saving a file to its new revision being listable | 300 ms debounce + up to one 100 ms tick |

## How each is measured

**1. Initial capture.** Delete `.salvager/`, start `salvager watch`, and poll the
number of `index/**/*.log` files until it reaches the file count (or plateaus).
The clock starts *before* the process spawns, so the figure includes process
start — it is time-to-first-protection, not just scan time. Reported with
files/s and MB/s derived from the generated tree size.

**2. Watches consumed.** With the tree quiescent, read the real kernel
resource the process holds, not an assumption:
- **Linux:** sum of `inotify wd:` entries across `/proc/<pid>/fdinfo/*` — one
  watch descriptor per watched directory.
- **macOS:** count of open directory fds under the tree from `lsof` — the
  kqueue backend opens one fd per watched path.
Resident memory (RSS) is captured in the same quiescent state.

**3. CPU at rest.** After the scan completes and the tree is quiet, read the
process's accumulated CPU time (`ps -o time`), wait `WINDOW` seconds (default
60), read it again. The delta is CPU-seconds burned while idle; reported as
% of one core and as ms-CPU-per-minute. A correct event-driven watcher should
sit near zero here — the only scheduled work is the 100 ms debounce ticker
iterating an empty pending-set.

**4. Save → queryable latency.** Create a probe file, wait for its initial
revision, then repeatedly append to it. For each edit: take a timestamp, write,
poll the file's `.log` (10 ms granularity) until a new revision line appears,
take a second timestamp. Report min / p50 / p95 / max / mean over `LAT_SAMPLES`
edits (default 30), spaced past the debounce so each is independent. This is
the full debounce + tick + read + hash + object-write + log-append path.

The store's per-revision cost in isolation (no fs events) is a separate, even
lower-noise artifact:

```sh
go test ./store -bench=. -benchmem -run=^$
```

`BenchmarkRecordUnique` is the worst case (real edit, unique bytes, no dedup);
`BenchmarkRecordUnchanged` is the common watcher path (event fires, content
identical, nothing written).

## Running it

```sh
(cd .. && go build -o salvager .)   # build the binary the harness exercises
PROFILE=small bench/run.sh        # 2k files / 200 dirs — quick smoke (~seconds)
bench/run.sh                      # default: 20k / 2k
PROFILE=large bench/run.sh        # 100k / 10k — stress
PROFILE=huge  bench/run.sh        # 200k / 20k — crosses the macOS watch ceiling
PROFILE=custom FILES=50000 DIRS=5000 bench/run.sh   # any size
WINDOW=120 LAT_SAMPLES=50 bench/run.sh
```

Output: `bench/RESULTS.md`, a self-contained table stamped with date, commit,
host, CPU, Go version, and profile.

Env knobs: `PROFILE` `WINDOW` `LAT_SAMPLES` `SEED` `SALVAGER_BIN` `TREE` `OUT`
plus `FILES`/`DIRS` overrides (with `PROFILE=custom`).

## Scaling sweep & the watch ceiling

`bench/sweep.sh` runs the same harness at several tree sizes and collects one
comparison row each into `bench/SCALING.md`, so you can show how capture time,
fds, CPU and latency grow with file count — and where the OS watch ceiling
bites.

```sh
bench/sweep.sh                                   # 20k, 100k, 200k
SCALES="20000:2000 200000:20000" bench/sweep.sh  # files:dirs pairs
```

Runs are **sequential by design** — a resource benchmark needs the machine
otherwise idle, so the sweep never parallelizes the runs.

The sweep adds two columns the single run also reports:

- **Watched** — paths actually under a live watch / total expected. `100%`
  means every file+dir is watched. Below 100% means the watch ceiling was hit.
- **Fails** — watch-add failures the watcher logged (e.g. `too many open
  files`), one per path it could not register.

**The ceiling, per OS.** The watcher registers one watch per *path* on macOS
(kqueue: a held fd for every file **and** directory) but one per *directory* on
Linux (inotify). So:

- **macOS** is bounded by the per-process fd cap, `kern.maxfilesperproc`
  (commonly 122,880). A tree with more files than that cannot be fully watched.
  Worse, once kqueue has consumed all the process's fds, the store's own
  `ReadFile` during capture is starved too — so beyond the ceiling files are
  *neither watched nor snapshotted*, and initial capture **plateaus** at the
  ceiling rather than finishing. The harness detects the plateau, counts the
  failures, and reports coverage < 100%. Raise it with
  `sudo sysctl -w kern.maxfilesperproc=<N>` (and a matching `ulimit -n`).
- **Linux** is bounded by `fs.inotify.max_user_watches`, counted per directory,
  so file count barely matters — a 200k-file tree in 20k dirs needs only ~20k
  watches. Raise it with `sudo sysctl -w fs.inotify.max_user_watches=<N>`.

This asymmetry is the honest scaling story: salvager is featherweight on CPU and
memory at any size, but live-watch *coverage* on macOS is fd-bound, and the
sweep shows exactly where.

## Honesty conditions (read before publishing)

- **Unique content, no dedup.** The generator writes distinct bytes to every
  file, so the content-addressed store never deduplicates. Capture numbers are
  therefore a floor on speed / ceiling on work — real repos with repeated or
  unchanged files capture *faster*. State the profile next to any number.
- **The latency floor is intentional, not overhead.** ~300 ms of the
  save→queryable figure is the debounce that collapses editor write-bursts into
  one revision. Report the total, but don't present the debounce as a cost to
  optimize away — quote p50/p95, not a cherry-picked min.
- **"Watches consumed" is OS-specific.** Linux inotify ≈ one per *directory*;
  macOS kqueue ≈ one fd per *path*. Don't quote a macOS fd count as if it were
  a Linux watch count. The harness labels which it measured.
- **Capture must be cold.** A warm re-run finds objects already present and
  skips the writes, which understates the work. The harness deletes `.salvager/`
  first; keep it that way.
- **Polling resolution is the error bar.** Capture is polled at 100 ms and
  latency at 10 ms, so treat those as the resolution of each figure. Run a few
  times and report the median; note the machine was otherwise idle.
- **Single machine, single tree shape.** Numbers are comparative on the host
  in the stamp, not a universal claim. Publish the host line with the table.

## What not to claim

Don't generalize one host's numbers to "salvager uses X% CPU" without the host
and profile. Don't compare against another tool unless it watched the *same*
tree on the *same* machine in the same run. The defensible claim is narrow and
strong: *on this tree, the watcher holds N watches, burns P% of a core idle,
captures the whole tree in T, and surfaces an edit in p50 L ms.*
