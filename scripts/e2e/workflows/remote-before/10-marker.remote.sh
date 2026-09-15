#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# retnd/retnd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# A REMOTE hook: the engine opens an SSH exec channel on this set's
# execution connection and feeds these bytes to bash over it. It prints
# the hostname it ran on, because "the remote ran it" is the claim and the
# only evidence that distinguishes it from a local hook that happened to
# succeed is the machine's own name.
# Hoisted out of the echo deliberately: a command substitution
# INSIDE an argument fails open -- `hostname` erroring would
# print "... ok on " and pass errexit, and the suite reads this
# line as proof the hook ran on the far side. On its own it is
# a simple command, so `set -e` refuses the step instead.
host="$(hostname)"
echo "e2e-hook remote before ok on $host"
