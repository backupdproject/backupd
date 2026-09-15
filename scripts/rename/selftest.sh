#!/usr/bin/env bash
# Mutation self-test for the brand-drift guard (#794, extended by #887).
#
# The guard (scripts/rename/check-brand-drift.sh) is green on this tree, and
# a check that is green on the only tree anybody runs it against has proven
# nothing: the pattern could be misanchored, the allowlist could be swallowing
# everything, or `git grep` could be looking at no files at all. So this
# plants the drift the guard exists to catch, in a throwaway git repository
# per case, and requires it to go red -- and plants the things it must NOT
# catch (CONFIRM_DELETE, rclone's ibm_signer.go, the kept deprecated aliases,
# and the nine `BackupD[a-zA-Z]` identifiers EPIC R must leave alone) and
# requires it to stay green.
#
# The green cases are not decoration. #887 extends the guard to a name whose
# first seven characters are this product's domain word, so the anchored
# pattern has to distinguish `backupd_session` from `BackupDetailPage` 50
# times over; a guard that flagged both would be deleted, and one that flagged
# neither would read exactly as green as this one.
#
# Throwaway repositories rather than mutant copies of this tree, which is
# where this differs from scripts/architecture/selftest.sh and the five other
# anchored selftests: those plant a violation INTO a verbatim copy of product
# source, so their plants have to be anchored and
# scripts/selftest/check-anchors.sh watches them for drift. Nothing here
# copies product source. Every case writes the three or four lines it is
# about, so there is no anchor to drift and nothing for that aggregator to
# check -- the guard's own allowlist is the only thing coupled to the real
# tree, and the guard reports its own dead entries.
#
# Every case runs even after one has failed; the tally at the end is the
# result, and one run names every broken control rather than the first.
#
# Run directly (`bash scripts/rename/selftest.sh`) or let the gate run it:
# scripts/ci-local.sh invokes it next to the guard itself.
set -uo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
guard="$repo_root/scripts/rename/check-brand-drift.sh"

checks=0
failures=0

pass() {
  checks=$((checks + 1))
  echo "  ok   $1"
}

fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  echo "  FAIL $1"
  if [ -n "${2:-}" ]; then
    printf '%s\n' "$2" | sed 's/^/         /'
  fi
}

# A throwaway repository with one commit, so `git grep` has tracked files to
# look at. Nothing here shares state with any other case.
new_repo() {
  tree="$(mktemp -d)"
  (
    cd "$tree"
    git init -q
    mkdir -p core
    printf 'package core\n\nconst Level = "RETND_DEBUG"\n' >core/level.go
    git add -A
    git -c user.email=selftest@example.invalid -c user.name=selftest \
      commit -q -m "base"
  )
  echo "$tree"
}

# run_guard <tree> [guard]: exit status in $status, combined output in $out.
#
# The optional second argument is a MUTATED COPY of the guard, which is how
# the two list-bookkeeping cases at the end of this file work: what they are
# about is the `pending` list, and the only way to plant "this entry was
# deleted" is to delete it. The copy lives outside the throwaway tree, so the
# guard's exclusion of its own path is not what makes those cases pass.
run_guard() {
  out="$(cd "$1" && bash "${2:-$guard}" 2>&1)"
  status=$?
}

# green <label> <tree> [guard]: the guard must accept this tree.
green() {
  run_guard "$2" "${3:-}"
  if [ "$status" -eq 0 ]; then
    pass "$1"
  else
    fail "$1 (expected exit 0, got $status)" "$out"
  fi
  rm -rf "$2"
}

# red <label> <tree> <substring the report must name>...
#
# Every substring is required, which is how a case demands that the report
# name a file AND a line AND the identifier: a report somebody cannot open is
# a report somebody disables.
red() {
  local label="$1" tree="$2" needle
  shift 2
  run_guard "$tree"
  if [ "$status" -eq 0 ]; then
    fail "$label (the guard accepted it)" "$out"
    rm -rf "$tree"
    return
  fi
  for needle in "$@"; do
    if ! printf '%s' "$out" | grep -Fq -- "$needle"; then
      fail "$label (went red, but never named '$needle')" "$out"
      rm -rf "$tree"
      return
    fi
  done
  pass "$label"
  rm -rf "$tree"
}

# red_with <guard> <label> <tree> <substring>...: `red`, against a mutated
# copy of the guard rather than the guard itself.
red_with() {
  local mutant="$1" label="$2" tree="$3" needle
  shift 3
  run_guard "$tree" "$mutant"
  if [ "$status" -eq 0 ]; then
    fail "$label (the guard accepted it)" "$out"
    rm -rf "$tree"
    return
  fi
  for needle in "$@"; do
    if ! printf '%s' "$out" | grep -Fq -- "$needle"; then
      fail "$label (went red, but never named '$needle')" "$out"
      rm -rf "$tree"
      return
    fi
  done
  pass "$label"
  rm -rf "$tree"
}

# guard_without_pending <token>: the guard with one `pending` entry deleted,
# printed to stdout as a path. Matched as a whole line, so it can only hit a
# list entry -- the token also appears in this guard's prose, and a prose line
# is never equal to a bare token.
guard_without_pending() {
  local copy
  copy="$(mktemp)"
  grep -vFx -- "$1" "$guard" >"$copy"
  if cmp -s "$copy" "$guard"; then
    fail "the self-test could not delete the pending entry '$1' from the guard" \
      "no line in $guard is exactly '$1'"
  fi
  echo "$copy"
}

# guard_without_alias <spec> [base]: the guard with one `aliases` entry
# deleted, printed as a path. Matched on the line's FIRST field, because an
# alias line carries its closing issue and its removal release after the
# token (R1.4, #889) and a whole-line match would have to restate the
# wording.
#
# `base` is an already-mutated copy, so two mutations can be composed: the
# path-scoped cases below need one entry added AND one pending entry gone.
guard_without_alias() {
  local copy base="${2:-$guard}"
  copy="$(mktemp)"
  awk -v spec="$1" '$1 == spec { next } { print }' "$base" >"$copy"
  if cmp -s "$copy" "$base"; then
    fail "the self-test could not delete the alias entry '$1' from the guard" \
      "no line in $base begins with the field '$1'"
  fi
  echo "$copy"
}

# guard_with_alias <line> [base]: the guard with one extra `aliases` entry,
# inserted at the top of that heredoc. This is how the path-scoped alias
# cases plant an entry that does not exist on this tree, and how the
# malformed-entry case plants a shim with no removal release.
guard_with_alias() {
  local copy base="${2:-$guard}"
  copy="$(mktemp)"
  awk -v line="$1" '
    { print }
    !added && $0 == "aliases=\"$(" { getline nextline; print nextline; print line; added = 1 }
  ' "$base" >"$copy"
  if cmp -s "$copy" "$base"; then
    fail "the self-test could not add an alias entry to the guard" \
      "the aliases heredoc was not found in $base"
  fi
  echo "$copy"
}

# commit <tree> <path> <content>: a tracked file, because the guard scans
# what git tracks and nothing else.
commit() {
  tree="$1"
  mkdir -p "$tree/$(dirname "$2")"
  printf '%s\n' "$3" >"$tree/$2"
  (
    cd "$tree"
    git add -A
    git -c user.email=selftest@example.invalid -c user.name=selftest \
      commit -q -m "case"
  )
}

echo "==> brand-drift guard: mutation self-test (#794, #887)"

# The control. A tree with nothing old-brand in it at all must be green, and
# every allowlist entry in the guard is dead here, which is the other half of
# the control: dead entries are REPORTED and do not fail the run, because a
# rename lands by deleting the occurrences it names -- which is exactly what
# EPIC R's 141-token `pending` list is for.
tree="$(new_repo)"
run_guard "$tree"
if [ "$status" -ne 0 ]; then
  fail "control: a tree with no old-brand identifier is green (exit $status)" "$out"
elif ! printf '%s' "$out" | grep -Fq "no longer match anything"; then
  fail "control: dead allowlist entries are reported" "$out"
else
  pass "control: clean tree green, dead allowlist entries reported not fatal"
fi
rm -rf "$tree"

# The four #794 prefixes, one case each. This is the whole point of the guard:
# a new identifier carrying a name this project no longer has.
tree="$(new_repo)"
commit "$tree" core/env.go 'package core

const Debug = "RM_NEW_THING"'
red "a new RM_ environment variable goes red" "$tree" "RM_NEW_THING"

tree="$(new_repo)"
commit "$tree" core/env.go 'package core

const Debug = "BM_NEW_THING"'
red "a new BM_ environment variable goes red" "$tree" "BM_NEW_THING"

tree="$(new_repo)"
commit "$tree" core/cookie.go 'package core

const Name = "bm_new_cookie"'
red "a new bm_ cookie goes red" "$tree" "bm_new_cookie"

# `rbm_` is behind an identifier character, so the bm_ pattern cannot see it
# and it needs one of its own. This is the case that proves it has one.
tree="$(new_repo)"
commit "$tree" core/cookie.go 'package core

const Name = "rbm_new_cookie"'
red "a new rbm_ identifier goes red" "$tree" "rbm_new_cookie"

# The report has to be openable. A guard that says "something somewhere" is
# a guard somebody disables.
tree="$(new_repo)"
commit "$tree" apps/thing/env.go 'package thing

const A = "RM_LOCATED"'
red "the report names file and line" "$tree" "apps/thing/env.go:3:"

# A commit is not the only state the gate runs in: .husky/pre-commit runs it
# with the change in the INDEX and nothing committed. `git add -N` is that
# state, and a guard that could not see it would pass every commit it was
# asked about.
tree="$(new_repo)"
mkdir -p "$tree/core"
printf 'package core\n\nconst A = "RM_STAGED_ONLY"\n' >"$tree/core/staged.go"
(cd "$tree" && git add -N core/staged.go)
red "a staged-but-uncommitted creation goes red" "$tree" "RM_STAGED_ONLY"

# The kept deprecated aliases (#794), which are allowed ANYWHERE rather than
# pinned to a file list: an alias has to be minted, read, tested and
# documented, and every one of those is a new file sooner or later.
tree="$(new_repo)"
commit "$tree" apps/common/auth/compat_test.go 'package auth

// RM_DEBUG is the deprecated alias of BACKUPD_DEBUG.
const legacyEnv = "RM_DEBUG"
const legacySession = "bm_session"
const legacyCSRF = "bm_csrf"'
green "the three kept aliases stay green in a file that did not exist" "$tree"

# Anchoring. Every one of these contains the guard pattern as a SUBSTRING and
# none of them is an old-brand identifier: the left anchor is the only thing
# standing between this guard and a wall of false positives that gets it
# deleted. ibm_signer.go is not hypothetical -- it is rclone's own S3 backend,
# named in distribution/packaging/compliance.json on this tree.
tree="$(new_repo)"
commit "$tree" core/anchor.go 'package core

const Confirm = "CONFIRM_DELETE"
const Form = "FORM_BASE_URL"
const Ids = "PLATFORM_IDS"
const Vendor = "backend/s3/ibm_signer.go"
const Alarm = "ALARM_STATE"
const Key = "rm-debug"'
green "substring lookalikes stay green (CONFIRM_, FORM_, ibm_signer, rm-debug)" "$tree"

# The out-of-scope names are pinned to their files on purpose, and this is
# the half of that decision that does work: RM_BASE_URL is the pinned
# environment contract of retnd/retnd-tests, and it is still a
# creation when it turns up somewhere new.
tree="$(new_repo)"
commit "$tree" core/service/copy.go 'package service

const Base = "RM_BASE_URL"'
red "a pre-existing name in a file it is not pinned to goes red" "$tree" "RM_BASE_URL"

# The excluded paths. A changelog that records the rename, a released-artifact
# record and a DO-NOT-EDIT binding are the places an old name is SUPPOSED to
# keep appearing, and the binding is checked at its source (api/v1/openapi.json)
# instead.
tree="$(new_repo)"
commit "$tree" CHANGELOG.md '- renamed RM_EXCLUDED_ONE to BACKUPD_EXCLUDED_ONE'
commit "$tree" provenance/sbom.spdx.json '{"note": "RM_EXCLUDED_TWO"}'
commit "$tree" core/apicontract/contract.gen.go 'package apicontract

// Code generated by gen-bindings.go. DO NOT EDIT.
const Cookie = "bm_excluded_three"'
commit "$tree" ui/shared/src/api/generated/contract.ts 'export const COOKIE = "bm_excluded_four";'
green "history records and generated bindings are out of scope" "$tree"

# And the same names in a file that is NOT excluded, so the case above is
# measuring the exclusion rather than the pattern failing to match its own
# content.
tree="$(new_repo)"
commit "$tree" docs/notes.md '- renamed RM_EXCLUDED_ONE to BACKUPD_EXCLUDED_ONE'
red "the same text outside an excluded path goes red" "$tree" "RM_EXCLUDED_ONE"

# The guard excludes ITSELF and this file, because both write these names
# down for a living. Two cases, because the exclusion is named file by file
# rather than by directory, and the second one is what makes that decision
# worth anything.
tree="$(new_repo)"
commit "$tree" scripts/rename/check-brand-drift.sh '# RM_SELF_LISTED bm_self_listed'
commit "$tree" scripts/rename/selftest.sh '# RM_SELF_PLANTED bm_self_planted'
green "the guard and its self-test are not scanned for their own contents" "$tree"

tree="$(new_repo)"
commit "$tree" scripts/rename/other.sh '# RM_NEIGHBOUR'
red "a third file in scripts/rename is scanned like anything else" "$tree" "RM_NEIGHBOUR"

# ---------------------------------------------------------------------------
# The `backupd` family, and the two earlier brands the org-wide grep found
# still live (#887, FR-40). Eight patterns, a red case each, plus the alias
# list's own bookkeeping (R1.4, #889).
# ---------------------------------------------------------------------------

# The three anchored spellings of the name this epic retires. Each one has to
# name the file and the line as well as the identifier, because these are the
# reds an author sees on a pre-commit hook and acts on.
tree="$(new_repo)"
commit "$tree" core/cookie.go 'package core

const Name = "backupd_newthing"'
red "a new lowercase backupd_ identifier goes red" "$tree" \
  "backupd_newthing" "core/cookie.go:3:"

tree="$(new_repo)"
commit "$tree" core/env.go 'package core

const Key = "BACKUPD_NEW_THING"'
red "a new BACKUPD_ environment variable goes red" "$tree" \
  "BACKUPD_NEW_THING" "core/env.go:3:"

tree="$(new_repo)"
commit "$tree" ui/shared/src/BackupdWidget.tsx 'export function BackupdWidget() {
  return null;
}'
red "a new Backupd display identifier goes red" "$tree" \
  "BackupdWidget" "ui/shared/src/BackupdWidget.tsx:1:"

# The BARE uppercase name, which is the twelfth pattern and the gap #932's
# review found: `BACKUPD` with nothing after it matches no prefix rule, and
# it is a real runtime identifier -- the variable a hook reads to tell it is
# running under this product. It is on the ALIAS list today (R1.4 exports it
# beside RETND for one release), so the plant runs against a guard with that
# entry deleted, which is what the case is measuring: the pattern exists and
# fires the moment the shim goes.
mutant="$(guard_without_alias BACKUPD)"
tree="$(new_repo)"
commit "$tree" core/env.go 'package core

const Marker = "BACKUPD"'
red_with "$mutant" "a new bare BACKUPD environment variable goes red once the shim goes" "$tree" \
  "BACKUPD" "core/env.go:3:"
rm -f "$mutant"

# ...and the bare pattern must not swallow the prefixed one. A report that
# named `BACKUPD` for a line holding `BACKUPD_RUN_ID` would make every
# `BACKUPD_*` finding unactionable and would let one alias entry allow the
# whole family, which is the opposite of what the alias list is for.
mutant="$(guard_without_alias BACKUPD)"
tree="$(new_repo)"
commit "$tree" core/run.go 'package core

const Key = "BACKUPD_RUN_ID"'
red_with "$mutant" "a prefixed BACKUPD_ name is still reported in full, not as the bare name" "$tree" \
  "BACKUPD_RUN_ID"
rm -f "$mutant"

# FR-43's shim table, as a check rather than a paragraph: an alias entry
# with no closing issue and no removal release is refused outright. An
# undated shim is how RM_DEBUG reached its third rename.
mutant="$(guard_with_alias 'backupd_undated_shim')"
tree="$(new_repo)"
red_with "$mutant" "an alias entry with no removal release is refused" "$tree" \
  "backupd_undated_shim" "no closing issue and removal release"
rm -f "$mutant"

mutant="$(guard_with_alias 'backupd_dated_shim #895 the release after the one that ships this EPIC')"
tree="$(new_repo)"
commit "$tree" core/shim.go 'package core

const Name = "backupd_dated_shim"'
green "a dated alias entry allows its own token" "$tree" "$mutant"
rm -f "$mutant"

# The path-scoped form, which exists for a shim whose TOKEN is live
# elsewhere (R1.5's `/backupd-web` entrypoint and FR-38's legacy state
# paths all tokenise to the bare `backupd`). The scope has to be load
# bearing in both directions, so this is two cases against one mutant: the
# named file is allowed, and the same token in another file is still a
# violation. Both run with the bare `backupd` pending entry deleted, since
# an entry on `pending` is allowed anywhere by design.
base="$(guard_without_pending backupd)"
mutant="$(guard_with_alias 'backupd@container/Dockerfile #895 the release after the one that ships this EPIC' "$base")"
tree="$(new_repo)"
commit "$tree" container/Dockerfile 'RUN ln /retnd-web /backupd-web'
green "a path-scoped alias allows the token in the file it names" "$tree" "$mutant"

tree="$(new_repo)"
commit "$tree" container/other.yaml 'command: ["/backupd-web", "serve-ui"]'
red_with "$mutant" "a path-scoped alias does not allow the token anywhere else" "$tree" \
  "backupd" "container/other.yaml:1:"
rm -f "$mutant" "$base"

# The organisation, which survived the last rename by not being in anybody's
# pattern at all. This case used to run against a guard with the
# `backupdproject` pending entry deleted, because the whole organisation was
# on `pending` while FR-41's cutover was still ahead of it, and an entry on
# `pending` is allowed anywhere by design -- so it could only prove the
# pattern would fire once the sweep landed.
#
# The sweep landed. R2.5 (#895)'s closing PR performed the transfer, swept
# every absolute coordinate and took the token OFF `pending`, so this now
# runs against the guard exactly as it ships: a new link to the old
# organisation is refused, with nothing relaxed to make the case work.
tree="$(new_repo)"
commit "$tree" docs/newpage.md 'See https://github.com/backupdproject/backupd/issues/1 for the details.'
red "a new link to the old organisation goes red" "$tree" \
  "backupdproject" "docs/newpage.md:1:"

# The first brand's spelled-out environment prefix. `RM_[A-Z]` has been in
# this guard since #794 and never saw `RCLONE_MANAGER_SOURCE_PORT`, which is
# a documented operator-facing installer variable.
tree="$(new_repo)"
commit "$tree" scripts/install/install_docker_host.py 'PORT_ENV = "RCLONE_MANAGER_NEW_THING"'
red "a new RCLONE_MANAGER_ environment variable goes red" "$tree" \
  "RCLONE_MANAGER_NEW_THING" "scripts/install/install_docker_host.py:1:"

# The two brand words themselves, case-insensitively, which is the half that
# catches prose and wire sentinels rather than identifiers. R2.2 (#892) swept
# `rclone-manager` off `pending`: what survives is pinned to the file that
# records it -- a quoted `dial tcp: lookup rclone-manager: no such host`, a
# comment attributing RM_DEBUG to the brand it came from, the guard's own
# pattern list in two gate scripts -- so a new one anywhere else is a
# creation and this case needs no mutated list any more. `backup_manager_state`
# below never needed one, because it is a name that exists nowhere in this
# tree.
tree="$(new_repo)"
commit "$tree" docs/history.md 'This product used to be called rclone-manager.'
red "the first brand name in new prose goes red" "$tree" \
  "rclone-manager" "docs/history.md:1:"

tree="$(new_repo)"
commit "$tree" core/env.go 'package core

const Legacy = "backup_manager_state"'
red "the second brand name in a new identifier goes red" "$tree" \
  "backup_manager" "core/env.go:3:"

# THE POSITIVE CONTROL, and the load-bearing half of this extension. Nine
# real identifiers carry `BackupD` followed by a lowercase letter, 50
# occurrences between them, and none of them is the product's name; the domain
# word `backup` is in another 9,800. A case-insensitive `backupd` pattern
# flags every one, which is a guard that gets deleted in a week. Without this
# case, "the guard is green" and "the guard matches nothing" are the same
# observation.
tree="$(new_repo)"
commit "$tree" ui/shared/src/pages/lookalikes.tsx 'export function BackupDetailPage() {}
export function BackupDefaultsPage() {}
export const BackupDomainPolicy = {};
export type BackupDetail = { id: string };
export type BackupDomain = { id: string };
export type BackupSet = { name: string };
export const slug = "backup-set";
export const dirEnv = "BACKUP_DIR";
export const statusKey = "backup_status";'
commit "$tree" core/mounts_test.go 'package core

func TestBackupDataAreSeparateMounts(t *testing.T)                   {}
func TestBackupDataOnEveryClaimedPlatform(t *testing.T)              {}
func TestBackupDoesNotReturnWhileAWorkerIsStillReading(t *testing.T) {}
func TestBackupDoesNotLeaveItRunningForever(t *testing.T)            {}

// rclone ships backend/s3/ibm_signer.go, which is not a bm_ cookie.
const vendored = "backend/s3/ibm_signer.go"'
green "the nine BackupD identifiers, BackupSet, backup-set, BACKUP_DIR, backup_status and ibm_signer stay green" "$tree"

# The name this epic renames TO. A tree whose identifiers are all `retnd` is
# what green is supposed to mean at the end of EPIC R, and it has to be green
# through the patterns rather than through a list entry.
tree="$(new_repo)"
commit "$tree" core/new.go 'package core

const Debug = "RETND_DEBUG"
const Session = "retnd_session"
const Metric = "retnd_backup_set_state"'
green "the replacement RETND_/retnd_ names are green" "$tree"

# ---------------------------------------------------------------------------
# The `pending` list, which is the mechanism EPIC R lands on (FR-40): every
# surviving occurrence is listed, each issue deletes its own entries, and the
# list going empty is the completion signal. Three cases, because a list that
# cannot be got wrong in either direction is not doing any work.
# ---------------------------------------------------------------------------

# A listed token is allowed ANYWHERE, including in a file that did not exist:
# a rename in flight touches new files, and pinning in-transit names to a file
# list would turn every one of those into a gate failure.
#
# The token is `backupd`, and it is chosen rather than arbitrary: a case
# that needs a genuinely-pending token has to use one that IS on the list,
# and after R2.5 (#895)'s closing PR the list is `backupd` and `Backupd`.
# It was `backupd_internal` until #895 renamed it, and then `backupdproject`
# until the FR-41 cutover swept that one off too -- both times these cases
# were left asserting against a list entry that no longer existed, and both
# times the self-test said so rather than passing. That is the failure mode
# this comment exists to keep visible: whichever token is used here, it has
# to be one `check-brand-drift.sh` still lists.
#
# It is planted as a bare `"backupd"` rather than inside a path or a
# coordinate so that exactly one token is reported: `/var/lib/backupd`
# tokenises once, but `backupd_session` or `backupdproject/backupd` would be
# reported twice, and a case that asserts on one finding is clearer than one
# that has to tolerate a second.
tree="$(new_repo)"
commit "$tree" core/brandnewfile.go 'package core

const Name = "backupd"'
green "a pending token is allowed in a file that did not exist" "$tree"

# The same tree, with that entry deleted from the list. This is the
# bookkeeping error the matrix's R1.2 row names first: an issue deletes its
# `pending` entry, the occurrences it was covering are still there, and the
# guard has to say so rather than accept them because the list used to.
tree="$(new_repo)"
commit "$tree" core/session.go 'package core

const Name = "backupd"'
commit "$tree" apps/common/csrf/csrf.go 'package csrf

const Name = "backupd"'
mutant="$(guard_without_pending backupd)"
red_with "$mutant" "a pending entry deleted while its occurrences still exist goes red" "$tree" \
  "backupd" "core/session.go:3:" "apps/common/csrf/csrf.go:3:"

# And the other end of the same mutation, which is the one the R1.2 row names
# second: the occurrence was deleted, the entry went with it, and the name
# comes back later in a file nobody associated with the rename. Re-creation is
# a creation, and the guard is the only thing that would notice.
tree="$(new_repo)"
commit "$tree" core/service/newsurface.go 'package service

// Copied from a pre-rename branch.
const Name = "backupd"'
red_with "$mutant" "a deleted occurrence re-added after its pending entry went goes red" "$tree" \
  "backupd" "core/service/newsurface.go:4:"
rm -f "$mutant"

echo "==> brand-drift guard self-test: $checks checks, $failures failure(s)"
[ "$failures" -eq 0 ] || exit 1
