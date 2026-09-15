#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# retnd/retnd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# A before hook that fails, which is the case where the BACKUP must not
# happen: a quiesce that did not work means the source is not in the state
# the transfer was going to assume.
echo "e2e-hook before-fail is about to fail"
exit 7
