#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# backupdproject/backupd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# Hoisted out of the echo deliberately: a command substitution
# INSIDE an argument fails open -- `hostname` erroring would
# print "... ok on " and pass errexit, and the suite reads this
# line as proof the hook ran on the far side. On its own it is
# a simple command, so `set -e` refuses the step instead.
host="$(hostname)"
echo "e2e-hook remote after ok on $host"
