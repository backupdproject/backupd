#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# A secret-backed environment variable, proved resolved WITHOUT being
# disclosed. Echoing the material would put it in this step's captured log
# legitimately, and the claim under test is that nothing in the product's
# own surfaces reveals it -- so the hook prints the one fact that
# distinguishes "resolved" from "empty" and nothing more.
echo "e2e-hook secret resolved, length ${#WF_E2E_SECRET}"
