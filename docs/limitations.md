# Known limits (v1)

> Reference for what can fail or surprise you — distinct from
> [Scope](../README.md#scope-v1) (what we deliberately did not build, in the
> README). Back to the [README](../README.md).

- **Large files**: capture streams the file through a fixed buffer, so resident
  memory is O(buffer) regardless of file size — hundreds of MB no longer spike
  memory. There is **no size cap that excludes content** (deliberate: a silent
  hole would break the guarantee); disk is bounded by retention instead. One
  edge: the start signature is computed from the first 64 KiB, so a long
  single-line file (minified JSON, a one-row CSV) gets a less-distinctive
  signature — this never affects the hash or recoverability, only the
  at-a-glance signal.
- **Real-time watch limit (macOS/Linux)**: each directory (kqueue: each entry)
  consumes a kernel resource. When the cap (`kern.maxfilesperproc` /
  `fs.inotify.max_user_watches`) is reached, the affected subtrees fall back to
  the polling sweep automatically — coverage stays whole (see
  [coverage](coverage.md)). Raising the sysctl keeps more of the tree on the
  lower-latency real-time path, but is no longer required for correctness.
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
  Relevant mainly to NFS-backed working trees on the cloud path. The real-time
  path has its own stat shortcut (skip re-streaming an unchanged file) gated by a
  conservative racy window — widened automatically on coarse-timestamp
  filesystems — that **fails toward capture**, so it never drops a change. On
  cross-host NFS, comparing the gate's wall clock against the server's mtime can
  distort that window and trigger redundant re-captures, never a miss.
- **Symlinks** are skipped, not followed (see
  [architecture](architecture.md#whats-ignored)).
- **Long-running**: no known leak of memory or file descriptors. Exercised by an
  opt-in soak test (`SALVAGER_SOAK=<duration> go test ./watch -run TestSoakNoLeak`):
  under sustained rewrite load — including large-file re-streams and same-size
  in-place rewrites — goroutine and file-descriptor counts return to baseline,
  with no per-capture leak in the streaming open/temp/rename path.
