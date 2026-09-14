#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# A hook with real lint findings and no runtime failure, because the
# findings panel is a surface of its own: a deployment whose every script
# is clean renders nothing there, and "nothing rendered" is
# indistinguishable from "the panel is broken".
#
# Two deliberate findings, both below the severity that gates a save:
#
#   BSH002  an unchecked `cd`. The next line runs wherever the process
#           already was if the directory is not there.
#   BSH001  an unquoted expansion, which is the info-level habit that
#           becomes a defect the first time a path holds a space.
#
# Nothing here is a mistake left in by accident, and nothing here fails:
# the step SUCCEEDS, so a suite can assert the findings beside a hook that
# ran.
cd "$BACKUPD_WORK_DIR"
cd /tmp
target=/tmp/e2e-findings
echo marker > $target
cat $target
echo "e2e-hook findings ok"
