# Manual A10 tests — real-agent self-repair

These are **manual** guides, run by a human with a real LLM agent. They prove
something a deterministic test cannot: that an agent, **given no hint that
Salvager exists**, discovers it on its own and uses it to recover lost work. The
only signal the agent is allowed is the MCP tool descriptions.

They complement — not replace — the automated end-to-end tests in
[`e2e_test.go`](../../e2e_test.go), which prove the recovery *mechanism* works
and run in CI on every push:

| Scenario | Manual guide (this dir) | Automated test (`e2e_test.go`) | Disaster | What it proves |
|----------|-------------------------|--------------------------------|----------|----------------|
| **A10.1** | `GUIDE_A10_1.md` + `setup_a10_1.sh` | `TestE2E_A10_1_RecoverAfterDestructiveGitCheckout` | `git checkout -- .` | Recovery after a tracked file is reverted to its committed state. |
| **A10.2** | `GUIDE_A10_2.md` + `setup_a10_2.sh` | `TestE2E_A10_2_RecoverUncommittedWorkGitCannot` | `git reset --hard HEAD` | Recovery of never-staged work git has **no record of** — git literally cannot bring it back. |

The setup scripts only build the throwaway project (clean commit + uncommitted
work). They do **not** start the watcher or trigger the disaster — you do those
by hand, per the guide, to control timing and observe what salvager captures.

```bash
bash setup_a10_1.sh /tmp/a10-1-test   # then follow GUIDE_A10_1.md
bash setup_a10_2.sh /tmp/a10-2-test   # then follow GUIDE_A10_2.md
```
