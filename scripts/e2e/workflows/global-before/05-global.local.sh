#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# The deployment-wide before stage. Nothing points at this directory in the
# seeded configuration on purpose: a configured global stage gives every
# backup set a workflow surface, and the quiet "no hooks are configured"
# empty state would then be unreachable for any set at all. The suite
# points the deployment at it for one case and restores what was there.
echo "e2e-hook global before ok"
