#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# A bounded burst, for the case that asks whether a throttled log follower
# changes how long the SCRIPT takes. It does not sleep: the duration this
# hook reports is the duration of 400 writes, and a follower that reads
# them slowly must not make that number larger.
i=1
while [ "$i" -le 400 ]; do
  echo "e2e-hook slow line $i"
  i=$(( i + 1 ))
done
echo "e2e-hook slow burst done"
