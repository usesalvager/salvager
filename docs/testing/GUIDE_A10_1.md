# Guide A10.1 — Autonomous self-repair (scenario 1)

Validates Salvager's central question: **does a real agent, without being told,
discover that it lost work and use Salvager on its own initiative to recover it?**

Scenario 1 is deterministic in its setup: *you* cause the disaster (not the
agent), and the agent must *stumble into* the loss and resolve it alone. This
isolates the question "does the agent discover and use Salvager?" without
relying on it biting a destructive hook.

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
bash setup_a10_1.sh /tmp/a10-1-test
cd /tmp/a10-1-test
```

This leaves a repo with:
- a clean initial commit (only `mean()`),
- an **uncommitted** improvement on disk: `median()` with its tests (the
  valuable work).

Confirm it:

```bash
git status --short      # should show  M stats.py  /  M test_stats.py
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
salvager watch       # over /tmp/a10-1-test
```

Confirm it captured the state with `median()`:

```bash
salvager history stats.py
```

You should see at least one recent revision. **This is the version the agent
will have to recover.** Note that the good content (with median) is in the
history.

## Step 3 — Trigger the disaster (YOU do this)

Destroy the uncommitted work:

```bash
git checkout -- .
```

Verify the good work is gone from disk:

```bash
grep median stats.py    # should find NOTHING: median() is no longer on disk
git status --short       # clean tree: git "restored" the old version
```

At this point: disk has only `mean()`, git thinks all is well, and the only copy
of `median()` lives in salvager's history.

## Step 4 — Give the agent the task that makes it stumble into the loss

Open Claude Code in `/tmp/a10-1-test` with the salvager MCP active. Give it
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
- If it got it wrong: was it a weak tool description or insufficient context in
  the revisions?

## Step 6 — Classify the result

- **PASS** — the agent, unprompted, used salvager to restore `median()` and the
  tests pass again.
- **PARTIAL** — the agent solved the task by **rewriting** `median()` from
  scratch, without touching salvager. The task is done, but the safety net went
  unused: a sign that the tool descriptions did not "sell" recovery. Actionable
  finding.
- **FAIL** — the agent gave up, asked for help, or failed to recover.

Objective PASS check:

```bash
grep median stats.py            # median() is back on disk
python -m pytest -q             # the 4 tests pass again
salvager history stats.py       # should show a 'restore' revision and a 'pre-restore' one
```

---

## Interpreting the findings

- **Clean PASS on the first try** → the core promise holds. Move up to scenario 3
  (choosing among several intermediate revisions) to stress discrimination.
- **PARTIAL (rewrote it)** → not a Salvager failure, a *discoverability* failure.
  Improve the tool descriptions and repeat. It's the most likely finding on the
  first run and the cheapest to fix.
- **FAIL on an opaque error** → fix the MCP error messages (check 2 above) and
  repeat.
- **FAIL even with good descriptions** → important finding: the agent does not
  associate "lost work" with "consult the history". You may need the
  `salvager_list_versions` description to be more explicit about the recovery
  use case, or to document the pattern in the MCP integration guide.

## Reset to test again

```bash
rm -rf /tmp/a10-1-test
bash setup_a10_1.sh /tmp/a10-1-test
```

Each run starts from zero. Do several: an agent's behavior is non-deterministic,
and a single PASS or FAIL is not conclusive. Three runs with the same result is
already a signal.
