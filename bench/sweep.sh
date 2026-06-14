#!/usr/bin/env bash
#
# Scaling sweep: run the lightness benchmark at several tree sizes and collect
# one comparison row per scale into bench/SCALING.md. Runs are SEQUENTIAL — a
# resource benchmark needs the machine otherwise idle, so never parallelize it.
#
# The point at scale is two-fold:
#   - show how capture time / fds / CPU / latency grow with file count, and
#   - reveal the OS watch ceiling: on macOS the kqueue backend needs one fd per
#     watched path and is capped by kern.maxfilesperproc (~122k), so a 200k-file
#     tree is fully snapshotted but only partially *watched* for later edits.
#     The Linux analog is fs.inotify.max_user_watches (per directory).
#
# Usage:
#   bench/sweep.sh                       # default scales: 20k 100k 200k
#   SCALES="20000:2000 200000:20000" bench/sweep.sh   # files:dirs pairs
#   WINDOW=60 LAT_SAMPLES=20 bench/sweep.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
OUT="${OUT:-$HERE/SCALING.md}"
WINDOW="${WINDOW:-60}"
LAT_SAMPLES="${LAT_SAMPLES:-20}"
# files:dirs pairs, smallest first.
SCALES="${SCALES:-20000:2000 100000:10000 200000:20000}"

[ -x "$REPO/lochis" ] || { echo "build first: (cd $REPO && go build -o lochis .)" >&2; exit 1; }

commit="$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo unknown)"
when="$(date -u '+%Y-%m-%d %H:%M:%SZ')"
host="$(uname -srm)"
if [ "$(uname -s)" = "Darwin" ]; then
  cpu_model="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || sysctl -n hw.model)"
  ncpu="$(sysctl -n hw.ncpu)"
  ceiling="kern.maxfilesperproc=$(sysctl -n kern.maxfilesperproc) (per-process fd cap)"
else
  cpu_model="$(awk -F: '/model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null | sed 's/^ //')"
  ncpu="$(nproc 2>/dev/null || echo '?')"
  ceiling="fs.inotify.max_user_watches=$(cat /proc/sys/fs/inotify/max_user_watches 2>/dev/null || echo '?')"
fi
go_ver="$(go version 2>/dev/null | awk '{print $3}')"

{
  echo "# lochis v1 — scaling sweep"
  echo
  echo "Reproduce: \`bench/sweep.sh\`. Method and caveats: \`bench/PROTOCOL.md\`."
  echo
  echo "| Field | Value |"
  echo "|---|---|"
  echo "| Date (UTC) | $when |"
  echo "| Commit | \`$commit\` |"
  echo "| Host | $host |"
  echo "| CPU | $cpu_model ($ncpu cores) |"
  echo "| Go | $go_ver |"
  echo "| Watch ceiling | $ceiling |"
  echo "| Window / latency samples | ${WINDOW}s / $LAT_SAMPLES |"
  echo
  echo "Unique content per file (no dedup). \`Watched\` = paths actually under a"
  echo "live watch / total expected; <100% means the watch ceiling was hit and"
  echo "those files were snapshotted but not watched for later edits."
  echo
  echo "| Profile | Files | Dirs | MB | Capture ms | files/s | MB/s | Watched | Fails | RSS MB | CPU %core | CPU ms/min | lat p50 | lat p95 |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
} > "$OUT"

for pair in $SCALES; do
  files="${pair%%:*}"; dirs="${pair##*:}"
  echo "=== sweep: $files files / $dirs dirs ===" >&2
  PROFILE=custom FILES="$files" DIRS="$dirs" \
    WINDOW="$WINDOW" LAT_SAMPLES="$LAT_SAMPLES" \
    ROW=1 OUT="$OUT" \
    "$HERE/run.sh"
done

echo >&2
cat "$OUT" >&2
echo >&2
echo "[sweep] wrote $OUT" >&2
