#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# A bounded burst, for the case that asks whether a throttled log follower
# changes how long the SCRIPT takes. It does not sleep: the duration this
# hook reports is the duration of its writes, and a follower that reads
# them slowly must not make that number larger.
#
# EIGHT THOUSAND lines, not four hundred, and the number is the whole
# point of the fixture. 400 lines is about 8.7 KiB, which fits inside a
# pipe buffer and is written in single-digit milliseconds -- so the
# assertion would have passed unconditionally, INCLUDING on a build where
# a slow reader really did reach back and stall the hook, which is the
# regression it exists to catch. 8,000 lines is about 190 KiB: past any
# pipe buffer, so the writes genuinely depend on somebody draining them,
# and still under DefaultStepOutputBytes (256 KiB) so the terminating
# line below is recorded rather than truncated away.
i=1
while [ "$i" -le 8000 ]; do
  echo "e2e-hook slow line $i"
  i=$(( i + 1 ))
done
echo "e2e-hook slow burst done"
