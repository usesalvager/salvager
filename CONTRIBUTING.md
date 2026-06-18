# Contributing

Thanks for helping improve Salvager.

## Build, test, vet

Salvager is a single Go module. CI runs exactly these on every push and PR, so
run them locally before opening a PR:

```sh
go build ./...
go vet ./...
go test ./...
```

## Branches & PRs

- Branch off `main`; open the PR against `main`.
- Keep unrelated work in separate PRs (a docs change and a test change are two PRs).
- PRs are **squash-merged**: the PR title becomes the commit subject, and the PR
  body plus the branch's commit messages feed the squash commit message.

## Commit & PR message hygiene — avoid accidental CI-skip directives

GitHub silently skips a workflow run when a push's HEAD commit message contains a
CI-skip directive **anywhere** in it — subject or body. The recognized tokens are:

```
[skip ci]   [ci skip]   [no ci]   [skip actions]   [actions skip]   ***NO_CI***
```

Because we squash-merge, one of these in a PR body or any commit message rides
into the merge commit and disables CI/Release on `main` — and on any tag that
points at that commit. This already cost us one silently-blocked release
(see issue #21): the failure produces no run and no error, so it looks like an
outage.

Rules:

- **Never** put one of those directives in a commit message or PR body unless you
  genuinely intend to skip CI.
- When you need to *describe* the mechanism in prose, rephrase it — write "a
  CI-skip directive" or break the token — so it can't be matched verbatim and
  ride into a squash commit.
- The only sanctioned use is the `sync-readme` job's own pin-bump commit in
  `.github/workflows/ci.yml`.

A `pull_request` CI job (`guard-skip-token`) scans PR bodies and commit messages
for these directives and fails the check if it finds one.
