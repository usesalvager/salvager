# Guide A10.2 — Recover work git cannot (scenario 2)

Same central question as A10.1: **does a real agent, without being told,
discover that it lost work and use Salvager on its own initiative to recover
it?** But A10.2 stages the product's single strongest argument and makes it
falsifiable:

> **git does not protect uncommitted work. Salvager does.**

In A10.1 the disaster (`git checkout -- .`) reverts a *tracked, modified* file to
its committed state — git "helpfully" restores the old version. In A10.2 the
valuable work is written to disk and **never staged**, so git never hashes it
into its object database. The disaster is `git reset --hard HEAD`, and afterward
**git has no way at all to bring the work back** — no object, no dangling blob,
no reflog entry. This mirrors the automated test
`TestE2E_A10_2_RecoverUncommittedWorkGitCannot` in `e2e_test.go`, which proves
the same git-cannot-recover property deterministically.

---

## Golden rule (do not break it)

At no point do you tell the agent that Salvager exists, that there is a history,
or that anything can be recovered. Its **only** allowed hint is the descriptions
of the MCP tools. If you give it a hint, you stop testing self-repair and start
testing that the tools work (which you already know they do).

---

## Before you start: two checks that make the test fair

1. **MCP tool descriptions.** Look at how `salvager_list_versions`,
   `salvager_get_version`, and `salvager_restore` are described. Each must say
   *when* to use it, not just what it does. An agent decides to use a tool by
   reading its description.
   - Weak: "Lists versions of a file."
   - Strong: "Lists the saved version history of a file. Useful for recovering
     lost work, reverting unwanted changes, or inspecting prior states after a
     destructive edit."
   If the descriptions are weak and the test fails, it will have failed for this
   reason, not for the concept. Tune them before the first run.

2. **Readable MCP errors.** Call `salvager_get_version` with a non-existent
   timestamp and confirm it returns a comprehensible error, not a crash or
   something empty. If the agent picks the wrong argument during the test, a
   clear error lets it correct course; an opaque one makes it give up for a
   cosmetic reason.

---

## Step 1 — Set up the project

```bash
bash setup_a10_2.sh /tmp/a10-2-test
cd /tmp/a10-2-test
```

This leaves a repo with:
- a clean initial commit (only `mean()`),
- an **uncommitted and never-staged** improvement on disk: `median()` with its
  tests (the valuable work git has no record of).

Confirm it:

```bash
git status --short      # should show  M stats.py  /  M test_stats.py  (NOT staged)
git diff --stat         # should show median() added
python -m pytest -q     # the 4 tests pass (or use the check below if no pytest)
```

Without pytest:

```bash
python -c "from stats import mean, median; assert median([1,2,3,4])==2.5; print('OK')"
```

## Step 2 — Start salvager and confirm capture

In a separate terminal, leave the watcher running:

```bash
salvager watch       # over /tmp/a10-2-test
```

Confirm it captured the state with `median()`:

```bash
salvager history stats.py
```

You should see at least one recent revision. **This is the version the agent
will have to recover.** Note that the good content (with median) is in the
history.

## Step 3 — Trigger the disaster (YOU do this)

Throw away the uncommitted work with a hard reset — the "discard all my local
changes" command an agent reaches for when it decides to "clean up" the repo:

```bash
git reset --hard HEAD
```

Verify the good work is gone from disk:

```bash
grep median stats.py    # should find NOTHING: median() is no longer on disk
git status --short       # clean tree: git thinks everything is fine
```

## Step 3b — Prove git genuinely cannot recover it (the heart of A10.2)

This is the whole point, so make it explicit. Because `median()` was never
`git add`-ed, git never wrote a blob for it. None of git's own recovery paths
can reach it:

```bash
# (1) No commit anywhere contains the lost work:
git log -p --all | grep median        # prints NOTHING

# (2) The canonical "rescue dangling objects" command finds nothing of it.
#     fsck --lost-found can only surface objects git actually hashed; this work
#     was never hashed, so there is nothing dangling to recover:
git fsck --unreachable --lost-found --no-reflogs | grep -i blob   # nothing for median

# (3) The reflog only records ref movements (commits, resets), never raw
#     working-tree edits, so there is no edit to roll back to:
git reflog                             # shows the commit and the reset, not the edit

# (4) Nothing was stashed:
git stash list                         # empty
```

At this point the only surviving copy of `median()` is in salvager's history.
git, by design, cannot help — this is exactly the failure git was never built to
catch, and exactly the one Salvager is.

## Step 4 — Give the agent the task that makes it stumble into the loss

Open Claude Code in `/tmp/a10-2-test` with the salvager MCP active. Give it
**exactly** a task like this (tweak the name if you want, but do not mention
salvager or "recover"):

> We were extending `stats.py`. It should have a `median()` function with its
> tests, which we added a while ago. Continue from there: verify it's present
> and that the tests pass.

The agent will open `stats.py`, see that `median()` is missing, and have a
conflict between what it's told exists and what it finds. That's where the real
test begins.

## Step 5 — Observe (without intervening)

Don't help. Watch what it does. Record:

- Did it look at its MCP tools and find salvager **on its own initiative**?
- How many steps did it take to connect "median is missing" with "salvager has it"?
- Which tool did it call first? `salvager_list_versions`?
- Did it inspect with `salvager_get_version` before restoring, or restore blind?
- Did it pick the right revision (the one with median)?
- Crucially: did it try (and fail) any git-based recovery first — `git checkout`,
  `git reflog`, `git stash`? Watching it exhaust git before reaching salvager is
  the most direct demonstration of the A10.2 claim.
- If it got it wrong: was it a weak tool description or insufficient context in
  the revisions?

## Step 6 — Classify the result

- **PASS** — the agent, unprompted, used salvager to restore `median()` and the
  tests pass again.
- **PARTIAL** — the agent solved the task by **rewriting** `median()` from
  scratch, without touching salvager. The task is done, but the safety net went
  unused: a sign that the tool descriptions did not "sell" recovery. Actionable
  finding.
- **FAIL** — the agent gave up, asked for help, or failed to recover. (Note: an
  agent that tries only git recovery and then gives up is a FAIL that *confirms*
  the A10.2 premise — git could not have helped it.)

Objective PASS check:

```bash
grep median stats.py            # median() is back on disk
python -m pytest -q             # the 4 tests pass again
salvager history stats.py       # should show a 'restore' revision and a 'pre-restore' one
```

---

## Interpreting the findings

- **Clean PASS on the first try** → the strongest promise holds: Salvager
  recovered work git could not. This is the demonstration to point a skeptic to.
- **PARTIAL (rewrote it)** → not a Salvager failure, a *discoverability* failure.
  Improve the tool descriptions and repeat. It's the most likely finding on the
  first run and the cheapest to fix.
- **FAIL after trying git** → expected if discoverability is weak, and it still
  proves the premise: the agent reached for git's recovery tools and they had
  nothing. Make the `salvager_list_versions` description more explicit about the
  recovery use case and repeat.
- **FAIL on an opaque error** → fix the MCP error messages (check 2 above) and
  repeat.

## Reset to test again

```bash
rm -rf /tmp/a10-2-test
bash setup_a10_2.sh /tmp/a10-2-test
```

Each run starts from zero. Do several: an agent's behavior is non-deterministic,
and a single PASS or FAIL is not conclusive. Three runs with the same result is
already a signal.
