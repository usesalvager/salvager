#!/usr/bin/env bash
#
# setup_a10_1.sh — Builds the test project for A10.1 (autonomous self-repair).
#
# Creates a throwaway git repo with:
#   - a real function with a passing test
#   - a clean initial commit
#   - an UNCOMMITTED improvement (the "valuable work" the disaster will destroy)
#
# It does NOT start salvager and does NOT trigger the disaster: you do those by
# hand following the guide, so you control the timing and observe what gets
# captured.
#
# Usage:  bash setup_a10_1.sh /path/where/to/create/the/project
#
set -euo pipefail

TARGET="${1:?Pass the target path: bash setup_a10_1.sh /tmp/a10-1-test}"

if [ -e "$TARGET" ]; then
  echo "ERROR: $TARGET already exists. Delete it or choose another path." >&2
  exit 1
fi

mkdir -p "$TARGET"
cd "$TARGET"
git init -q

# --- Base state: a simple function + a passing test ------------------------
cat > stats.py <<'PY'
"""Statistics helpers for a list of numbers."""


def mean(values):
    """Arithmetic mean of a non-empty list."""
    if not values:
        raise ValueError("values must not be empty")
    return sum(values) / len(values)
PY

cat > test_stats.py <<'PY'
from stats import mean


def test_mean_basic():
    assert mean([2, 4, 6]) == 4


def test_mean_single():
    assert mean([10]) == 10
PY

cat > README.md <<'MD'
# stats — A10 test project

Statistics helpers. Run the tests with:

    python -m pytest -q
MD

git add -A
git commit -q -m "Initial: mean() with tests"

echo "==> Initial commit created."

# --- The valuable work, UNCOMMITTED ----------------------------------------
# We add median(): a real improvement, with its test, that passes. This is what
# the disaster (git checkout) will destroy, and what the agent must recover on
# its own using salvager.
cat > stats.py <<'PY'
"""Statistics helpers for a list of numbers."""


def mean(values):
    """Arithmetic mean of a non-empty list."""
    if not values:
        raise ValueError("values must not be empty")
    return sum(values) / len(values)


def median(values):
    """Median of a non-empty list. Handles even and odd lengths."""
    if not values:
        raise ValueError("values must not be empty")
    s = sorted(values)
    n = len(s)
    mid = n // 2
    if n % 2 == 1:
        return s[mid]
    return (s[mid - 1] + s[mid]) / 2
PY

cat > test_stats.py <<'PY'
from stats import mean, median


def test_mean_basic():
    assert mean([2, 4, 6]) == 4


def test_mean_single():
    assert mean([10]) == 10


def test_median_odd():
    assert median([3, 1, 2]) == 2


def test_median_even():
    assert median([1, 2, 3, 4]) == 2.5
PY

echo "==> Improvement 'median()' written to disk, UNCOMMITTED."
echo
echo "Repo state:"
git status --short
echo
echo "Project ready at: $TARGET"
echo "Now follow GUIDE_A10_1.md step by step."
