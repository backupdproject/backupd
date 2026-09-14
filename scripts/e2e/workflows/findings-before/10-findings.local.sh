#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
#
# A hook with real lint findings and no runtime failure, because the
# findings panel is a surface of its own: a deployment whose every script
# is clean renders nothing there, and "nothing rendered" is
# indistinguishable from "the panel is broken".
#
# NO `set -euo pipefail` here, and that is the fixture rather than an
# oversight. BSH002 is about a `cd` whose failure nothing checks, and
# errexit checks it -- measured, not assumed: this script carried
# `set -euo pipefail` and the report came back with the BSH001 alone. So
# the two findings this file exists to produce are:
#
#   BSH002  warning  an unchecked `cd`, so the next line operates on
#                    whatever tree the process was already in.
#   BSH001  info     an unquoted expansion, the habit that becomes a
#                    defect the first time a path holds a space.
#
# Deliberately nothing at ERROR severity: that is the gate's business
# (rejected-before/10-reject.local.sh), and this set has to stay runnable
# so a suite can assert findings BESIDE a hook that ran. Nothing here
# fails; the step succeeds.
cd /tmp
target=/tmp/e2e-findings
echo marker > $target
cat $target
echo "e2e-hook findings ok"
