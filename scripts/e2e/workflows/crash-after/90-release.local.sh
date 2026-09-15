#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# retnd/retnd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# The cleanup obligation. Its existence is what makes a crashed run
# RECOVERABLE rather than merely failed: an interrupted run whose after
# stage never ran is a source left in whatever state the before stage put
# it in, which is why the product blocks the set until this has run or a
# person has accounted for it.
echo "e2e-hook crash cleanup ran"
