# `salvager` — Undo for AI agents

> Home: **[salvager.sh](https://salvager.sh)**

A filesystem-level code safety net. A passive watcher saves **per-file**
revisions automatically; when an agent (or a human) breaks something, you
recover it with one command. Designed to be consumed by the agent itself over
MCP, so it can self-repair.

The whole binary is free forever under Apache-2.0 — every feature, no crippled
tier, no paid unlock. What's sold (support, deployment, compliance) is work a
local binary physically can't do, not features withheld from it.

## Why

An AI agent rewrites a file, deletes work you hadn't committed, or clobbers an
uncommitted change — and `git` can't bring it back, because it was never staged.
Salvager can: it has been quietly saving a revision of that file every time it
changed, so you `restore` it in one command. No commits, no checkpoints, no
configuration — just a running watcher and a recoverable history.

## Recover a state git never held

A refactor lands in passes: a clean rename by hand first, then a later blanket
sed over-applies and breaks the build. You want the last-good pass back — but the
clean rename was never committed or stashed, so git holds only the old code and
the broken tree. Salvager saved every pass:

```text
# rename Session -> Run across watcher.go and cli.go
● Renamed the type and its call sites in both files by hand. Build is green.
$ go build ./...
⎿ (ok)
● Now a blanket sed to catch any stragglers across the package.
$ grep -rl Session . | xargs sed -i 's/Session/Run/g'
⎿ (rewrote watcher.go, cli.go)
$ go build ./...
⎿ # github.com/usesalvager/watcher
⎿ watcher.go:9:14: undefined: cfg.RunTimeout
✗ The sed also hit cfg.SessionTimeout, a config key that had to stay. The build is broken.
● I want the hand-rename back, without losing it to a full reset. It was never committed.
$ git stash list
⎿ (empty)
✗ Nothing was committed or stashed between the rename and the sed. git has only the old code and the broken tree.

● Salvager recorded every save. The clean pre-sed revision of each file is in its history.
$ salvager history watcher.go
⎿ TIMESTAMP            HASH      LABEL       LINES  ΔLINES  START
⎿ 2026-06-17 13:02:33  1c3a997b  modify      17     +1      package watcher
⎿ 2026-06-17 13:02:31  85fb497d  modify      16     +0      package watcher
⎿ 2026-06-17 13:02:30  af46411e  first-seen  16     +16     package watcher
⎿ restore with: salvager restore watcher.go <timestamp-ms>
⎿ (timestamps below are human-readable; raw ms:)
⎿   1781694153232  modify
⎿   1781694151932  modify
⎿   1781694150430  first-seen
● The 13:02:31 revision is the clean rename — before the sed. Restoring it.
$ salvager restore watcher.go 1781694151932
⎿ restored watcher.go to revision 1781694151932
⎿ previous state saved as pre-restore revision 1781694161069 (undo with: salvager restore watcher.go 1781694161069)
✓ Restore is non-destructive — the broken tree is kept as its own revision, so the recovery itself is reversible.

● Same for cli.go, the other file the sed touched.
$ salvager history cli.go
⎿ TIMESTAMP            HASH      LABEL       LINES  ΔLINES  START
⎿ 2026-06-17 13:02:34  57a69357  modify      9      +1      package watcher
⎿ 2026-06-17 13:02:32  c8fe5c77  modify      8      +0      package watcher
⎿ 2026-06-17 13:02:30  7a70eda4  first-seen  8      +8      package watcher
⎿ restore with: salvager restore cli.go <timestamp-ms>
⎿ (timestamps below are human-readable; raw ms:)
⎿   1781694154432  modify
⎿   1781694152033  modify
⎿   1781694150430  first-seen
$ salvager restore cli.go 1781694152033
⎿ restored cli.go to revision 1781694152033
⎿ previous state saved as pre-restore revision 1781694161076 (undo with: salvager restore cli.go 1781694161076)
$ go build ./...
⎿ (ok)
✓ Both files back to the clean rename — the sed undone, the refactor kept. A state git never held.
```

## Install

**Install script** (macOS / Linux) — downloads the right prebuilt binary, verifies
its SHA-256 checksum, and puts it on your PATH:

```sh
curl -fsSL https://raw.githubusercontent.com/usesalvager/salvager/main/install.sh | sh
```

Pin a version (omit to get the latest) or pick the install dir with environment
variables — `SALVAGER_VERSION` takes any tag from the [releases page](https://github.com/usesalvager/salvager/releases):

```sh
SALVAGER_VERSION=v1.4.0 \
SALVAGER_INSTALL_DIR="$HOME/.local/bin" \
  curl -fsSL https://raw.githubusercontent.com/usesalvager/salvager/main/install.sh | sh
```

It installs to `/usr/local/bin` if writable, otherwise `$HOME/.local/bin`. It never
uses `sudo`, sends no telemetry, and does not edit your shell config. The checksum
is verified before anything is installed — a mismatch aborts with nothing written.

**Homebrew** (macOS / Linux):

```sh
brew install usesalvager/tap/salvager
```

## Build

```sh
go build -o salvager .
```

Single static binary, no runtime. macOS and Linux supported; Windows is
build-from-source best-effort (no prebuilt binary).

A plain build reports its version as `dev` (`salvager --version` → `salvager dev`).
To stamp a real version, derive it from the current git tag via ldflags — the
same form the release workflow injects, so a local build's `--version` matches a
published binary and never goes stale:

```sh
CGO_ENABLED=0 go build -ldflags "-X 'github.com/usesalvager/salvager/version.Version=$(git describe --tags --always)'" -o salvager .
```

The same value backs both `salvager --version` and the version the MCP server
advertises to clients — one source of truth (`version.Version`).

## Quickstart

Onboarding is two commands, run in this order: turn on the watcher, then connect
your agent.

**Step 1 — start the watcher as a service.** This is the safety net, so turn it
on first.

```sh
salvager service install
```

Starts the per-project watcher now and on every login/reboot — install it and
forget it. See [Run it persistently](#run-it-persistently) below for the
launchd/systemd detail, the install-time preflight, and the Linux linger step
needed for it to survive reboot.

**Step 2 — connect your agent.**

```sh
salvager init
```

It connects your agent to Salvager with no JSON to copy by hand:

- registers the Salvager **MCP server** for this project in Claude Code (scope
  *local* — private to you, never committed), via the `claude` CLI;
- adds a short block to your **user** `~/.claude/CLAUDE.md` telling the agent the
  Salvager tools exist and when to use them; and
- registers a **PreToolUse hook** in this project's `.claude/settings.local.json`
  (scope local, never committed) so Salvager can intercept dangerous Bash commands
  before they run — see [Interception (Tier A/B)](#interception-tier-ab).

`init` is an idempotent reconciler: run it twice and nothing changes; run it after
something drifts and it repairs only what drifted. It only ever rewrites its own
delimited block in `CLAUDE.md`, merges its own entry into `settings.local.json`
(preserving your other hooks and keys), and never touches `~/.claude.json` by hand.
Flags: `--no-claude-md` (skip the CLAUDE.md block; the MCP server and hook still
register) and `--undo` (remove all three pieces).

> Requires the `claude` CLI on your PATH for the MCP step. If it's missing, `init`
> still updates `CLAUDE.md` and prints the exact command to run yourself. Only
> Claude Code is supported for now.

The two steps are independent — neither command fails if you run them in the other
order. Watcher-first is the sensible sequence, not a hard requirement: turn the
capture on, then tell the agent the net is there.

Just trying it, or prefer to run the watcher yourself? `salvager watch` runs it in
the foreground until you kill it (Ctrl-C):

```sh
salvager watch [--root <path>]
```

Zero configuration — run it in the root of any project. It records an initial
revision of every tracked file on startup, then captures every change (debounced
~300 ms) thereafter. `--root <path>` watches a tree other than the current
directory; without it, the working directory is used. `service install` is the
recommended default; this is the run-it-by-hand path.

```
salvager init [--no-claude-md] [--undo]  connect this project's agent
salvager watch [--root <path>] [--allow-partial]  start the watcher (until killed)
salvager service install | uninstall | status [--json]  run the watcher as a service
salvager history <file>           list recorded versions of a file
salvager show <file> <ts>         print the content of one version
salvager restore <file> <ts>      restore a file to a version (reversible)
salvager restore-at <ts> [path]   restore a set of files to a point in time
salvager restore-at --undo        revert the last restore-at batch
salvager timeline [path]          show activity and flag destructive bursts
salvager mcp                      start the MCP server (stdio)
salvager hook                     Claude Code PreToolUse guard (invoked by Claude Code, not by hand)
salvager gc [--max-age 7d] [--max-bytes 500M]  purge old revisions and cap store size
```

### Run it persistently

`salvager service install` from Step 1 is the recommended way to run the watcher —
installed once, it survives terminal close and reboot. Its companion subcommands:

```sh
salvager service install     # start now + on every login/reboot
salvager service status       # installed? running? persistent? (add --json for scripts)
salvager service uninstall    # stop and remove cleanly
```

It uses a **launchd** LaunchAgent on macOS and a **systemd user service** on
Linux — both per-user, no root for the unit itself. Install runs a preflight and
verifies the service is actually running before reporting success, so a tree the
watcher can't start on fails loud at install time instead of crash-looping later.

> **Linux:** a systemd user service does not survive logout/reboot until
> *lingering* is enabled. If it's off, `service install`/`status` tells you so
> (never falsely claiming persistence) and prints the one command to fix it:
> `loginctl enable-linger "$USER"` (re-run with `sudo` if denied).

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

When a whole set of files is wiped at once — an agent in another terminal runs
`git clean -fd` / `git checkout -f` / `git reset --hard` and takes your uncommitted,
untracked work with it — rewind them together instead of one `restore` per file:

```
salvager restore-at <timestamp-ms> [path]   # restore every tracked file under [path]
                                            # to its state at or before that instant
salvager restore-at --undo                  # revert the last restore-at batch
```

It is non-destructive: files created after that instant are left alone, a file whose
state then was a deletion is left in place (never removed), and the batch records a
per-file `pre-restore` so `--undo` puts everything back. `[path]` defaults to the whole
tree.

Not sure *which* instant to rewind to? `salvager timeline [path]` reads the recorded
history (creating nothing) and flags clusters of destructive revisions — many files
deleted or sharply shrunk within a couple of seconds, the fingerprint of a bulk
`git clean -fd` / `reset --hard` — printing the exact `restore-at` command to undo each:

```
salvager timeline            # whole tree
salvager timeline src        # just one subtree

⚠ 1 likely-destructive burst(s):
  2026-06-29 14:21:07  —  12 file(s) hit (12 deleted) within 30ms
    rewind to just before:  salvager restore-at 1782693666999
```

`salvager gc` drops revisions older than N days (default 7) and garbage-collects
any object no longer referenced by any log. With `--max-bytes`, it also caps
store size: when the store exceeds the budget it evicts the oldest revisions
first until it's back under the limit, always keeping each file's most recent
revision and never breaking a restore's reversibility. Run it manually or once a
day.

## MCP

`salvager mcp` exposes four tools over stdio:

- `salvager_list_versions` — list a file's versions (read-only)
- `salvager_get_version` — read one version's content (inspect before acting)
- `salvager_restore` — restore a version (returns the pre-restore timestamp)
- `salvager_restore_at` — point-in-time batch restore: rewind a whole set of
  files at once, for a bulk command that wiped many together (non-destructive;
  each rewritten file carries its own pre-restore timestamp to undo)

No purge or delete is exposed over MCP — the safety net can't be erased by the
agent that might break things, and every restore is non-destructive. Every tool is also contained to the project root:
a `file` argument that escapes the tree is refused before any read or write (see
[architecture](docs/architecture.md#mcp-path-containment)).

For Claude Code, `salvager init` registers this for you (scope local). To wire it
into another MCP client by hand, point it at the binary:

```json
{
  "mcpServers": {
    "salvager": { "command": "salvager", "args": ["mcp"], "cwd": "/path/to/project" }
  }
}
```

## Interception (Tier A/B)

Recovery is the net under the agent; interception is the rail in front of it. On
Claude Code, `salvager init` also registers a **PreToolUse hook** (`salvager hook`,
in `.claude/settings.local.json`) that inspects every Bash command — and every
Edit/Write target (see [Protected paths](#protected-paths)) — *before* it runs. It
is invoked by Claude Code, never by a human.

The line it draws is the honest one — **what Salvager can vs cannot recover:**

- **Tier A — denied.** Damage the file-history net cannot undo: deletion reaching
  *outside* the project tree (`rm -rf ~`, `rm -rf /`, a `..` escape), destroying the
  net itself (any write to `.salvager/`), or an irreversible-beyond-the-filesystem
  write (`git push --force`, `dd`, `mkfs`, `shred`). The deny carries a reason the
  agent reads and self-corrects on, in the same turn.
- **Tier B — checkpointed, then allowed.** Destructive but recoverable *inside* the
  tree (`git reset --hard`, `git clean -fd`, `git checkout -f`, bulk `sed -i` /
  `find -delete` / `find … -exec rm` / `xargs rm`). Salvager lets it proceed and hands
  the agent the `restore-at` instant to rewind to if it goes wrong.
- **Everything else passes**, fast and silent.

Salvager never blocks something it could have undone anyway — it only walls off
what the net can't save. Crucially, PreToolUse hooks **fire and can block even when
the agent runs with `--dangerously-skip-permissions`**, so a Tier A deny is the one
guardrail a YOLO-mode agent cannot bypass. The hook always **fails open**: if it
can't parse, classify, or errors, the command is allowed — a net must never become
the thing that jams the tool it guards. Every Tier A/B attempt is appended to a
local, append-only `.salvager/hook-log` (the command is hashed, never stored
verbatim); nothing is uploaded.

The classifier is an agent-agnostic core (`guard/`) behind a thin Claude Code
adapter, so a second agent is a small adapter over the same, already-tested brain.

**One honest limit:** the hook sees the *shell*, not what an interpreter does inside
it. An inline write through a language runtime — `python -c "open('.env','w')…"`,
`node -e "fs.writeFileSync('.env',…)"`, `perl -e …` — can't be parsed by a shell
classifier and passes. Defense-in-depth still covers most of it: the watcher recovers
anything the interpreter writes to a non-gitignored file. The one thing genuinely
unprotected here is a *gitignored secret* written by an interpreter — recovery never
captured it and the shell parser can't see the write. We don't pretend to close that.

## Protected paths

Recovery says "you can get it back"; protection says "it must never be touched."
They don't overlap: the watcher **cannot recover a gitignored file** — it never
captured it — so for gitignored secrets (`.env`, private keys) *prevention is the
only protection there is*. The hook **denies any agent write or delete to a
protected path** — `Write`/`Edit`, and `rm` / `sed -i` / `mv` / `cp` / `tee` /
`install` / `ln` / `truncate` / `find … -exec rm` / `> redirect` — as a **Tier A
deny**, even under `--dangerously-skip-permissions`.

It works like antivirus definitions: a **built-in default set** plus a **user
layer**, layered.

- **Defaults** (shipped, focused on what recovery can't save): `.env`, `.env.*`
  (but **not** the non-secret templates `.env.example` / `.sample` / `.template` /
  `.dist`), `*.pem`, `*.key`, `*.p12`, `*.pfx`, the SSH private keys `id_rsa` /
  `id_dsa` / `id_ecdsa` / `id_ed25519` (their `.pub` public halves are **not**
  protected), `credentials`, `.npmrc`, `.pypirc`, `.aws/**`, `.ssh/**`, `.git/**`.
  (`.salvager/` is already net-protected.)
- **User layer** — `.salvager/protected`, plain text, one glob per line, `#`
  comments. A pattern with no `/` matches a **basename** at any depth (`*.crt`); a
  trailing `/**` matches anything under a directory (`secrets/**`); any other
  pattern matches the full path (`config/prod.yaml`).
- **Exclusions** — a line starting with `!` **un-protects** (an antivirus
  exception), so a default that misfires never makes you disable the whole feature.
  `.gitignore`-style: the last matching rule wins.

```
# .salvager/protected
config/prod.yaml      # add: protect a non-secret too
!.env.example         # exclude: this one is safe to write
```

A deny tells the agent what to do — defer to the user, or add a `!` exclusion —
so it self-corrects in the same turn. Reads are never blocked; this is about
writes and deletes only.

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

In: one-command onboarding (`init`: MCP registration + user CLAUDE.md + PreToolUse
hook, idempotent, reversible), watcher, per-file store, list/get/restore/record,
point-in-time batch restore (`restore-at`) + `timeline`, pre-restore safeguard,
PreToolUse interception (Tier A deny / Tier B checkpoint, fail-open, agent-agnostic
`guard` core + Claude Code adapter), polling sweep for over-cap subtrees (automatic
full coverage) + `--allow-partial` degradation policy, `.gitignore` + default
excludes, CLI, MCP (4 tools), age- and size-based retention (always keeps each
file's latest revision, never breaks a restore), external lightness/scaling
benchmark harness (`bench/`).

Out: branches, merge, sync, cloud, accounts, config files, web UI, RBAC,
rendered diffs, user-created explicit checkpoints (Tier B references the watcher's
continuous recovery point, it does not store a named snapshot).

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
Copyright 2026 Somhi Lagunak SL.
