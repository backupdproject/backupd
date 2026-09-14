#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# The happy path's local before hook: it runs on the Host Workflow Runner,
# in an ephemeral container, with no network, as a non-root user, and it
# proves all four by simply succeeding.
echo "e2e-hook happy before ok"
