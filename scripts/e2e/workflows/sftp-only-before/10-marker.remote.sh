#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# The same KIND of work remote-before/10-marker.remote.sh does -- a plain
# echo that would certainly have succeeded -- attached to the backup set
# whose only credential is an internal-sftp-forced account. (Not the same
# bytes: this one prints its own line and does not ask for a hostname,
# because it is never expected to get that far.) It is never expected to run: the point of the fixture is that
# the SERVER refuses the exec request, so the product reports an
# exec-capability finding while artifact backup over the same credential
# keeps working. A script that could not have succeeded anyway would prove
# nothing about the refusal.
echo "e2e-hook sftp-only remote ran, which it must not have"
