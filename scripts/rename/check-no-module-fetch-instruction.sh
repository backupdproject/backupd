#!/usr/bin/env bash
# Nothing in this tree tells anybody to `go install` or `go get` this
# module (#888, R1.3, FR-41).
#
# WHY THIS EXISTS. R1.3 swept the module path to its final value,
# `github.com/retnd/retnd`, in one commit and without staging it through an
# intermediate name. From that commit until FR-41's cutover (R2.5, #895)
# the module path deliberately does NOT match the location the module is
# fetched from, which is still `backupdproject/backupd`. A module path that
# does not resolve is harmless as long as nothing resolves it: every build
# in this repository is a workspace or `GOWORK=off` build over local
# `replace` directives and relative module directories, so the path is an
# identifier here and never a URL.
#
# It stops being harmless the moment one line of documentation says
#
#     go install github.com/retnd/retnd/core/cmd/retnd@latest
#
# because that line is an instruction to fetch, it cannot work, and the
# person it fails for is a newcomer who has no reason to suspect the
# repository of lying to them. The hazard is exactly one sentence wide, it
# is zero sentences today, and this is the check that keeps it there.
#
# WHAT IT LOOKS FOR. `go install`, `go get` or `go run` naming this
# module -- under either spelling, because instructing somebody to fetch
# the OLD coordinates is the same defect with a different address -- on one
# line, in any tracked file.
#
# `go build`, `go test` and `go vet` are not here and must not be: those
# take a package pattern that resolves inside the workspace, and
# `go build github.com/retnd/retnd/core/cmd/retnd` is how
# apps/common/webhost/settings_boundary_test.go builds the CLI from a
# sibling module. Nothing about that reaches the network.
#
# WHEN IT GOES. R2.5 (#895) moves the repository to `retnd/retnd`, at which
# point the instruction this file forbids becomes simply true, and the
# cutover PR deletes this script, its two registrations (scripts/ci-local.sh
# and .github/workflows/ci.yml) and this paragraph with it.
#
# Exit code contract:
#
#   0   no tracked file carries such an instruction
#   1   at least one does, and the run printed file, line and text
#
# The repository root comes from `git rev-parse --show-toplevel` of the
# CURRENT WORKING DIRECTORY, the same as check-brand-drift.sh, so a
# throwaway tree can be checked by running this from inside it.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# The files whose JOB is to write the forbidden instruction down, and
# there are four of them. This script quotes it in its own WHY paragraph.
# EPIC R's specification names it as the hazard FR-41 declares, its
# conformance matrix names it as the falsification for row R1.3, and the
# inventory enumerates the tokens. A record OF a defect is not the defect,
# which is the same argument check-brand-drift.sh makes at length for
# pinning these same three documents rather than sweeping them.
#
# By path, one line each, so a fifth file that quotes the instruction has
# to be added here on purpose rather than being caught by a directory
# glob.
excluded_paths=(
  ":(exclude)scripts/rename/check-no-module-fetch-instruction.sh"
  ":(exclude)docs/EPIC-R-rename-backupd-to-retnd.md"
  ":(exclude)docs/EPIC-R-rename-inventory.md"
  ":(exclude)docs/conformance/epic-r-matrix.md"
)

# Both coordinates, because the transient has two wrong answers rather than
# one: the module path nothing can fetch, and the location it is actually
# fetched from, which is not the module path.
module_re='go[[:space:]]+(install|get|run)[^"]*github\.com/(retnd/retnd|backupdproject/backupd)'

hits="$(git grep -I -n -E "$module_re" -- . "${excluded_paths[@]}" || true)"

if [ -n "$hits" ]; then
  echo
  echo "==> check-no-module-fetch-instruction: FAILED. these lines instruct somebody to fetch this module:"
  echo "$hits" | sed 's/^/  /'
  cat <<'EOF'

  This module's path is github.com/retnd/retnd and its fetch location is
  github.com/backupdproject/backupd until the cutover (#895, FR-41), so
  neither spelling works in a `go install`, `go get` or `go run`. Every
  build here is local: a workspace build, or GOWORK=off over the `replace`
  directives in the five go.mod files.

  Say "clone the repository and build from it" instead, or wait for #895,
  which deletes this check because the instruction becomes true.
EOF
  exit 1
fi

echo "check-no-module-fetch-instruction: ok (no tracked file tells anybody to go install/go get/go run this module)"
