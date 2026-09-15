#!/usr/bin/env bash
# The mutation self-test for the published-provenance guard (#895, R2.8).
#
# The suite itself is scripts/bdtools/tests/published_provenance_guards.py,
# beside the other ported guard suites (EPIC I, #672 / #697). This file
# exists so the path scripts/ci-local.sh and .github/workflows/ci.yml
# name is the same shape as every other `*.test.sh` gate step, rather
# than one step reaching into scripts/bdtools directly while its
# neighbours do not.
#
# `exec`, so the exit status is the suite's own.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
exec python3 "$repo_root/scripts/bdtools/tests/published_provenance_guards.py" "$@"
