#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# retnd/retnd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# One of six steps on the many-step run, which exists so the viewer can be
# asked to mount exactly ONE terminal while keeping five other logs a click
# away.
echo "e2e-hook many step one"
