#!/usr/bin/env bash
# scripts/e2e/three-machine-web-ui.sh seeds this tree into the deployment's
# read-only /workflows mount. Every line of output here is asserted by
# retnd/retnd-tests' suites/web-ui, so the strings are a
# contract rather than narration (#816).
set -euo pipefail

# The gate's own fixture: a script the shell verification refuses at ERROR
# severity (BSH003, a recursive forced delete that targets a root-level
# directory whenever the expansion in it is empty).
#
# `stage` is DELIBERATELY empty rather than unset. The rule does not
# consult `set -u`, and says why in its own comment: nounset aborts on an
# unset parameter and not on an empty one, so a variable assigned "" gets
# past it and leaves the literal path behind. This is that shape, written
# on purpose.
#
# It is also harmless if it ever DOES run: `rm -rf /e2e-nothing-here` in
# the hook container removes a path that does not exist, on a read-only
# rootfs, as a non-root user, with no network. A fixture for a refusal
# must not depend on the refusal working.
stage=""
rm -rf "$stage/e2e-nothing-here"
echo "e2e-hook rejected ran, which the gate should have prevented"
