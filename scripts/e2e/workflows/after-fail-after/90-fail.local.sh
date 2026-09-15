#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# retnd/retnd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# The other half of the split status: the backup itself succeeded and the
# cleanup did not, so "did this run work?" has two different answers and
# the product has to render both.
echo "e2e-hook after-fail is about to fail"
exit 9
