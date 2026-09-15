#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# retnd/retnd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# The hook the engine is killed underneath. It has to be long enough for a
# browser to see the run RUNNING and ask the rig to restart the engine, and
# short enough that a run nobody crashes still finishes well inside the
# five-minute step bound.
#
# It prints a tick rather than sleeping silently so the suite can wait on
# real output rather than on a guess about scheduling.
echo "e2e-hook crash hold started"
i=1
while [ "$i" -le 9 ]; do
  sleep 5
  echo "e2e-hook crash hold tick $i"
  i=$(( i + 1 ))
done
echo "e2e-hook crash hold finished"
