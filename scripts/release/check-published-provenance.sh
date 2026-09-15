#!/usr/bin/env bash
# "No already-published provenance record is rewritten" (#895, R2.8), at a
# path a person and a workflow can both name.
#
# The check itself is scripts/bdtools/release/check_published_provenance.py,
# beside the other release tools ported in EPIC I (#672). This wrapper
# exists for the same reason verify-manifest-parity.sh's does: the gate
# step, the CI workflow and the release documentation should all name one
# stable path.
#
# `exec`, so the exit status is the check's own: 1 is "a published fact
# moved", 2 is "the check could not do its job", and a wrapper that
# collapsed either to 0 would let the rewrite through.
#
# NOTE: no `cd`, deliberately. The target tree comes from `git rev-parse
# --show-toplevel` of the CURRENT WORKING DIRECTORY, which is what lets
# scripts/bdtools/tests/published_provenance_guards.py point it at a
# throwaway repository.
set -euo pipefail

exec python3 "$(cd "$(dirname "$0")/.." && pwd)/bdtools/release/check_published_provenance.py" "$@"
