#!/usr/bin/env bash
#
# salvager lightness benchmark — reproducible "proof of lightness" harness.
#
# Measures four operator-facing properties of `salvager watch` on a synthetic
# large repo, then writes a publishable Markdown table to bench/RESULTS.md:
#
#   1. Initial capture   wall time to record the first revision of every file
#                        (cold .salvager), plus files/s and MB/s.
#   2. Watches consumed  real kernel resources held while idle: inotify watch
#                        descriptors (Linux) or open kqueue dir fds (macOS),
#                        plus resident memory (RSS).
#   3. CPU at rest       CPU-seconds the watcher burns over a quiet window,
#                        expressed as % of one core. The only idle work is the
#                        100 ms ticker, so this is the floor of the design.
#   4. Save -> queryable end-to-end latency from writing a file to the new
#                        revision being listable. The floor is intentional:
#                        the 300 ms debounce + up to one 100 ms tick.
#
# Production code is never modified: every metric is observed from the outside.
# Content is unique per file, so the content-addressed store performs NO dedup
# — capture numbers are worst-case, not flattered by repeated bytes.
#
# Usage:
#   bench/run.sh                 # default profile (20k files / 2k dirs)
#   PROFILE=small bench/run.sh   # quick smoke (2k / 200)
#   PROFILE=large bench/run.sh   # stress (100k / 10k)
#   WINDOW=120 bench/run.sh      # longer CPU-at-rest window (seconds)
#
# Env knobs: PROFILE WINDOW LAT_SAMPLES SEED SALVAGER_BIN TREE OUT
set -euo pipefail

# --------------------------------------------------------------------------
# Config
# --------------------------------------------------------------------------
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"

PROFILE="${PROFILE:-default}"
WINDOW="${WINDOW:-60}"                 # CPU-at-rest sampling window, seconds
LAT_SAMPLES="${LAT_SAMPLES:-30}"       # save->queryable latency samples
SEED="${SEED:-1337}"
SALVAGER_BIN="${SALVAGER_BIN:-$REPO/salvager}"
TREE="${TREE:-${TMPDIR:-/tmp}/salvager-bench-tree}"
TREE="$(python3 -c 'import os,sys;print(os.path.normpath(sys.argv[1]))' "$TREE")"
OUT="${OUT:-$HERE/RESULTS.md}"
# Watcher stderr goes OUTSIDE the watched tree, else the watcher records it.
WERR="${TMPDIR:-/tmp}/salvager-bench-watch.err"

case "$PROFILE" in
  small)   DEF_FILES=2000;   DEF_DIRS=200;;
  default) DEF_FILES=20000;  DEF_DIRS=2000;;
  large)   DEF_FILES=100000; DEF_DIRS=10000;;
  huge)    DEF_FILES=200000; DEF_DIRS=20000;;
  custom)  DEF_FILES=0;      DEF_DIRS=0;;
  *) echo "unknown PROFILE=$PROFILE (small|default|large|huge|custom)" >&2; exit 2;;
esac
# FILES/DIRS env override the profile defaults (PROFILE=custom FILES=.. DIRS=..).
FILES="${FILES:-$DEF_FILES}"
DIRS="${DIRS:-$DEF_DIRS}"
[ "$FILES" -gt 0 ] 2>/dev/null || { echo "FILES must be > 0" >&2; exit 2; }
[ "$DIRS" -gt 0 ]  2>/dev/null || { echo "DIRS must be > 0"  >&2; exit 2; }

OS="$(uname -s)"
WPID=""

# Each watched path costs a file descriptor under the kqueue backend (macOS);
# raise the soft limit defensively so a large tree does not exhaust it.
ulimit -n $((FILES + DIRS + 1024)) 2>/dev/null || true

cleanup() { [ -n "$WPID" ] && kill "$WPID" 2>/dev/null || true; }
trap cleanup EXIT

# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------
now_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }

# CPU seconds consumed so far by a pid, parsed from `ps` H:M:S.ss time.
cpu_seconds() { ps -o time= -p "$1" 2>/dev/null | awk -F: '{s=0;for(i=1;i<=NF;i++)s=s*60+$i;print s}' || true; }

# Resident set size in MB.
rss_mb() { ps -o rss= -p "$1" 2>/dev/null | awk '{printf "%.1f", $1/1024}' || true; }

# Number of recorded logs. Tolerant of the startup race where .salvager/index
# does not exist yet (find then exits non-zero under pipefail).
log_count() {
  local n
  n="$(find "$TREE/.salvager/index" -type f -name '*.log' 2>/dev/null | wc -l | tr -d ' ')" || true
  echo "${n:-0}"
}

start_watcher() {
  ( cd "$TREE" && exec "$SALVAGER_BIN" watch ) >/dev/null 2>"$WERR" &
  WPID=$!
}

say() { printf '%s\n' "$*" >&2; }

# --------------------------------------------------------------------------
# Tree generation (deterministic, unique content per file -> no dedup)
# --------------------------------------------------------------------------
gen_tree() {
  say "[gen] $PROFILE: $FILES files across ~$DIRS dirs -> $TREE"
  rm -rf "$TREE"
  # gen prints three space-separated integers: total_bytes file_count dir_count.
  # dir_count is the actual number of directories on disk (leaves + parents +
  # root) — that is exactly what the watcher must register, so it drives the
  # expected-watch denominator. We count it instead of trusting the parameter.
  local g
  g="$(python3 - "$TREE" "$FILES" "$DIRS" "$SEED" <<'PY'
import math, os, random, sys
tree, files, dirs, seed = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
rnd = random.Random(seed)
# Build a 2-level tree: ~sqrt(dirs) parent groups, each holding leaf dirs, for
# exactly `dirs` leaf directories with unique paths (realistic nesting, no
# accidental path collisions).
ngroups = max(1, int(math.isqrt(dirs)))
dpaths = []
for i in range(dirs):
    g = i // (((dirs - 1) // ngroups) + 1)        # which parent group
    p = os.path.join(tree, "g%04d" % g, "d%06d" % i)
    os.makedirs(p, exist_ok=True)
    dpaths.append(p)
total = 0
# Size distribution: mostly small source files, a long tail of larger ones.
for i in range(files):
    d = dpaths[i % len(dpaths)]
    size = rnd.randint(300, 4096) if rnd.random() < 0.9 else rnd.randint(4096, 40960)
    # Unique, incompressible-ish content: index prefix + seeded random bytes.
    # randbytes() is C-fast and seeded, so 200k files generate in seconds.
    head = ("// file %d\n" % i).encode()
    data = head + rnd.randbytes(max(0, size - len(head)))
    with open(os.path.join(d, "f%06d.txt" % i), "wb") as fh:
        fh.write(data)
    total += len(data)
actual_dirs = sum(1 for _ in os.walk(tree))       # leaves + parents + root
print(total, files, actual_dirs)
PY
)"
  GEN_BYTES="${g%% *}"
  DIRS="$(echo "$g" | awk '{print $3}')"          # actual dir count drives expectations
  GEN_MB="$(awk -v b="$GEN_BYTES" 'BEGIN{printf "%.1f", b/1048576}')"
  say "[gen] done: ${GEN_MB} MB, $FILES files, $DIRS dirs on disk"
}

# --------------------------------------------------------------------------
# 1. Initial capture (cold)
# --------------------------------------------------------------------------
measure_capture() {
  rm -rf "$TREE/.salvager"
  say "[capture] cold start, waiting for $FILES logs ..."
  local t0 t1 c last stable=0 poll=0.1
  [ "$FILES" -gt 50000 ] && poll=0.5   # find over 100k+ logs is itself costly
  t0="$(now_ms)"
  start_watcher
  last=-1
  while :; do
    if ! kill -0 "$WPID" 2>/dev/null; then
      say "[capture] watcher exited early; stderr:"; cat "$WERR" >&2 || true; exit 1
    fi
    c="$(log_count)"
    if [ "$c" -ge "$FILES" ]; then break; fi
    if [ "$c" -eq "$last" ]; then
      stable=$((stable + 1))
      if [ "$stable" -ge 60 ]; then            # no progress for ~60 polls -> plateau
        say "[capture] plateau at $c/$FILES logs (partial)"; break
      fi
    else
      stable=0; last="$c"
    fi
    sleep "$poll"
  done
  t1="$(now_ms)"
  CAP_MS=$((t1 - t0))
  CAP_LOGS="$(log_count)"
  CAP_FPS="$(awk -v n="$CAP_LOGS" -v ms="$CAP_MS" 'BEGIN{printf "%.0f", (ms>0?n/(ms/1000):0)}')"
  CAP_MBPS="$(awk -v b="$GEN_BYTES" -v ms="$CAP_MS" 'BEGIN{printf "%.1f", (ms>0?(b/1048576)/(ms/1000):0)}')"
  say "[capture] ${CAP_MS} ms  (${CAP_LOGS} files, ${CAP_FPS} files/s, ${CAP_MBPS} MB/s)"
}

# --------------------------------------------------------------------------
# 2. Watches consumed (+ RSS) — tree is quiet here
# --------------------------------------------------------------------------
measure_watches() {
  sleep 1
  local rp
  rp="$(cd "$TREE" && pwd -P)"   # resolve /var -> /private/var so lsof names match
  if [ "$OS" = "Linux" ]; then
    # inotify: one watch descriptor per watched directory (files are not
    # watched individually), so this tracks the tree's directory count.
    WATCH_KIND="inotify watch descriptors"
    WATCH_NOTE="one inotify watch per directory; tree = $FILES files / $DIRS dirs"
    EXP_PATHS=$DIRS
    WATCHES="$( { grep -c '^inotify ' /proc/"$WPID"/fdinfo/* 2>/dev/null \
               | awk -F: '{s+=$NF} END{print s+0}'; } || echo 0)"
  else
    # kqueue: one open fd per watched PATH — every file AND directory — so the
    # footprint scales with file count and is bounded by `ulimit -n`. Count
    # REG+DIR fds under the tree, excluding the .salvager store's own files.
    WATCH_KIND="open kqueue fds"
    WATCH_NOTE="kqueue holds ~1 fd per watched path (file + dir); tree = $FILES files / $DIRS dirs"
    EXP_PATHS=$((FILES + DIRS))
    WATCHES="$( { lsof -p "$WPID" 2>/dev/null \
               | awk -v t="$rp/" '($5=="REG"||$5=="DIR") && index($NF,t)==1 && $NF !~ /\/\.salvager(\/|$)/' \
               | wc -l | tr -d ' '; } || echo 0)"
  fi
  WATCH_RSS="$(rss_mb "$WPID")"
  # Watch-add failures (e.g. "too many open files" once the kqueue fd ceiling
  # or inotify watch limit is reached) are logged by the watcher, one per dir
  # it could not register. A nonzero count means coverage is incomplete: those
  # paths got an initial revision but are NOT being watched for later edits.
  # grep -c prints the count AND exits 1 when zero matches; keep the printed
  # count, swallow the exit, default to 0 if the file is absent.
  WATCH_FAILS="$( { grep -c 'watch add' "$WERR" 2>/dev/null; } || true)"; WATCH_FAILS="${WATCH_FAILS:-0}"
  WATCH_EMFILE="$( { grep -ci 'too many open files' "$WERR" 2>/dev/null; } || true)"; WATCH_EMFILE="${WATCH_EMFILE:-0}"
  COVERAGE_PCT="$(awk -v w="$WATCHES" -v e="$EXP_PATHS" 'BEGIN{printf "%.1f", (e>0?100*w/e:0)}')"
  if [ "${WATCH_FAILS:-0}" -gt 0 ]; then
    WATCH_NOTE="$WATCH_NOTE; CEILING HIT: $WATCH_FAILS watch-add failures ($WATCH_EMFILE EMFILE) — live coverage incomplete"
  fi
  say "[watches] $WATCHES/$EXP_PATHS paths (${COVERAGE_PCT}%) $WATCH_KIND, RSS ${WATCH_RSS} MB, fails ${WATCH_FAILS} (EMFILE ${WATCH_EMFILE})"
}

# --------------------------------------------------------------------------
# 3. CPU at rest
# --------------------------------------------------------------------------
measure_cpu_idle() {
  say "[cpu] sampling ${WINDOW}s idle window ..."
  local c0 c1
  c0="$(cpu_seconds "$WPID")"
  sleep "$WINDOW"
  c1="$(cpu_seconds "$WPID")"
  CPU_DELTA="$(awk -v a="$c0" -v b="$c1" 'BEGIN{printf "%.3f", b-a}')"
  CPU_PCT="$(awk -v d="$CPU_DELTA" -v w="$WINDOW" 'BEGIN{printf "%.3f", (w>0?100*d/w:0)}')"
  CPU_MS_PER_MIN="$(awk -v d="$CPU_DELTA" -v w="$WINDOW" 'BEGIN{printf "%.0f", (w>0?1000*d/(w/60):0)}')"
  say "[cpu] ${CPU_DELTA}s over ${WINDOW}s = ${CPU_PCT}% of one core (${CPU_MS_PER_MIN} ms CPU/min)"
}

# --------------------------------------------------------------------------
# 4. Save -> queryable latency (perturbs the tree; run last)
# --------------------------------------------------------------------------
measure_latency() {
  local probe="$TREE/d0000/lat_probe.txt"
  local plog="$TREE/.salvager/index/d0000/lat_probe.txt.log"
  mkdir -p "$(dirname "$probe")"
  printf 'seed\n' > "$probe"
  # Wait for the initial revision so the .log exists before we sample modifies.
  local waited=0
  while [ ! -f "$plog" ] && [ "$waited" -lt 200 ]; do sleep 0.05; waited=$((waited+1)); done

  say "[latency] $LAT_SAMPLES edits ..."
  local samples="" i before after start end waitn
  LAT_TIMEOUTS=0
  for i in $(seq 1 "$LAT_SAMPLES"); do
    before="$(wc -l < "$plog" 2>/dev/null | tr -d ' ' || echo 0)"
    start="$(now_ms)"
    printf 'edit %d %s\n' "$i" "$start" >> "$probe"
    waitn=0
    while :; do
      after="$(wc -l < "$plog" 2>/dev/null | tr -d ' ' || echo 0)"
      [ "${after:-0}" -gt "${before:-0}" ] && break
      waitn=$((waitn + 1))
      if [ "$waitn" -ge 300 ]; then             # ~3 s: edit never captured (unwatched)
        LAT_TIMEOUTS=$((LAT_TIMEOUTS + 1)); break
      fi
      sleep 0.01
    done
    if [ "${after:-0}" -gt "${before:-0}" ]; then
      end="$(now_ms)"; samples="$samples $((end - start))"
    fi
    # If the probe is clearly unwatched (ceiling starved even recording), stop
    # burning the timeout on every remaining sample.
    if [ "$LAT_TIMEOUTS" -ge 5 ] && [ -z "$samples" ]; then
      say "[latency] aborting: probe unwatched after $LAT_TIMEOUTS timeouts"; break
    fi
    sleep 0.6                                   # > debounce: keep samples independent
  done
  [ "$LAT_TIMEOUTS" -gt 0 ] && say "[latency] WARNING: $LAT_TIMEOUTS/$LAT_SAMPLES edits never captured (probe unwatched — ceiling?)"

  read -r LAT_MIN LAT_P50 LAT_P95 LAT_MAX LAT_MEAN < <(
    printf '%s\n' $samples | sort -n | awk '
      function pc(p,  i){i=int(p/100*n); if(i<1)i=1; if(i>n)i=n; return a[i]}
      {a[NR]=$1; s+=$1}
      END{n=NR; if(!n){print "NA NA NA NA NA"; exit}
          printf "%d %d %d %d %d\n", a[1], pc(50), pc(95), a[n], s/n}')
  say "[latency] min ${LAT_MIN}  p50 ${LAT_P50}  p95 ${LAT_P95}  max ${LAT_MAX}  mean ${LAT_MEAN} (ms)"
}

# --------------------------------------------------------------------------
# Report
# --------------------------------------------------------------------------
write_report() {
  # Sweep mode: append one pipe-row to $OUT (header written by the caller).
  if [ -n "${ROW:-}" ]; then
    printf '| %s | %s | %s | %s | %s | %s | %s | %s/%s (%s%%) | %s | %s | %s | %s | %s | %s |\n' \
      "$PROFILE" "$FILES" "$DIRS" "$GEN_MB" \
      "$CAP_MS" "$CAP_FPS" "$CAP_MBPS" \
      "$WATCHES" "$EXP_PATHS" "$COVERAGE_PCT" "$WATCH_FAILS" "$WATCH_RSS" \
      "$CPU_PCT" "$CPU_MS_PER_MIN" "$LAT_P50" "$LAT_P95" >> "$OUT"
    say "[report] appended row to $OUT"
    return
  fi

  local commit when host kern cpu_model ncpu go_ver
  commit="$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  when="$(date -u '+%Y-%m-%d %H:%M:%SZ')"
  host="$(uname -srm)"
  if [ "$OS" = "Darwin" ]; then
    cpu_model="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || sysctl -n hw.model)"
    ncpu="$(sysctl -n hw.ncpu)"
  else
    cpu_model="$(awk -F: '/model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null | sed 's/^ //')"
    ncpu="$(nproc 2>/dev/null || echo '?')"
  fi
  go_ver="$(go version 2>/dev/null | awk '{print $3}')"

  cat > "$OUT" <<EOF
# salvager v1 — lightness benchmark

Reproduce: \`PROFILE=$PROFILE bench/run.sh\`. Method and caveats: \`bench/PROTOCOL.md\`.

| Field | Value |
|---|---|
| Date (UTC) | $when |
| Commit | \`$commit\` |
| Host | $host |
| CPU | $cpu_model ($ncpu cores) |
| Go | $go_ver |
| Profile | \`$PROFILE\` — $FILES files / $DIRS dirs, ${GEN_MB} MB, unique content (no dedup) |

## Results

| Metric | Value | Notes |
|---|---|---|
| Initial capture (cold) | **${CAP_MS} ms** | ${CAP_LOGS} files → ${CAP_FPS} files/s, ${CAP_MBPS} MB/s |
| Watches consumed | **${WATCHES}** | ${WATCH_KIND} — ${WATCH_NOTE} |
| Live watch coverage | **${WATCHES} / ${EXP_PATHS} paths (${COVERAGE_PCT}%)** | ${WATCH_FAILS} watch-add failures (${WATCH_EMFILE} EMFILE); <100% = some files snapshotted but not watched for later edits |
| Resident memory (idle) | **${WATCH_RSS} MB** | full tree watched, quiescent |
| CPU at rest | **${CPU_PCT}% of one core** | ${CPU_DELTA}s over ${WINDOW}s = ${CPU_MS_PER_MIN} ms CPU/min; floor = 100 ms ticker |
| Save → queryable latency | **p50 ${LAT_P50} ms / p95 ${LAT_P95} ms** | min ${LAT_MIN} / max ${LAT_MAX} / mean ${LAT_MEAN}; ${LAT_TIMEOUTS} uncaptured; design floor = 300 ms debounce + ≤100 ms tick |

_Capture uses unique content per file, so the content-addressed store never
deduplicates — real repos with repeated/unchanged files capture faster._
EOF
  say "[report] wrote $OUT"
}

# --------------------------------------------------------------------------
main() {
  [ -x "$SALVAGER_BIN" ] || { say "build first: (cd $REPO && go build -o salvager .)"; exit 1; }
  gen_tree
  measure_capture
  measure_watches
  measure_cpu_idle
  measure_latency
  cleanup; WPID=""
  write_report
  if [ -z "${ROW:-}" ]; then say ""; cat "$OUT" >&2; fi
}
main "$@"
