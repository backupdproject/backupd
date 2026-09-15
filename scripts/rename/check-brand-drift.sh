#!/usr/bin/env bash
# The brand-drift guard (#794, extended by #887): no NEW old-brand identifier
# enters this tree.
#
# Issue #794 renamed the runtime identifiers this project inherited from its
# two previous names -- rclone-manager's `RM_` environment variables and
# backup-manager's `bm_` cookies -- to `BACKUPD_` / `backupd_`. That rename
# is a one-off edit; this file is the part that makes it stay done. Without
# it the next `RM_SOMETHING` somebody adds by copying a neighbouring line
# is invisible until the third rename, which is how this repository got two
# brands' worth of prefixes in the first place.
#
# EPIC R (#885) is that third rename: `backupd` becomes `retnd`. Issue #887
# points this guard at the name the epic retires, and the four patterns #794
# left behind stay exactly as they were -- `RM_DEBUG` is still a kept alias
# and the twenty-three `RM_*` token-and-path pairs are still another
# repository's environment contract, so replacing them would trade one blind
# spot for another (FR-40).
#
# WHAT IT LOOKS FOR: the creation of an identifier carrying an old brand
# name, in any tracked source file. Eleven patterns, in two families.
#
# The four from #794, case-sensitive and anchored on their left:
#
#   RM_[A-Z]     rclone-manager environment variables
#   BM_[A-Z]     backup-manager environment variables (none exist today)
#   bm_[a-z]     backup-manager cookies, container and helper names
#   rbm_[a-z]    the never-used third variant, blocked before it exists
#
# The seven FR-40 adds. The first three are anchored on their RIGHT as well,
# and that anchor is the whole design of this extension:
#
#   backupd[^a-z]          the lowercase spelling: backupd_session,
#                          backupd.yml, /var/lib/backupd
#   BACKUPD_[A-Z]          the environment prefix
#   Backupd[^a-z]          the display spelling: Backupd, BackupdError
#   backupdproject         the organisation, so a new absolute link to it
#                          goes red (FR-41 moves the coordinates)
#   RCLONE_MANAGER_[A-Z]   the FIRST brand's spelled-out environment prefix,
#                          which no #794 pattern matched and which has
#                          therefore been green for two renames
#   rclone[-_ ]manager     the first brand, case-insensitively: still live as
#                          `RCLONE-MANAGER-BACKUP-COMPLETE` sentinels and as
#                          prose
#   backup[-_ ]manager     the second brand, case-insensitively: still live
#                          as `BACKUP_MANAGER_*` variables and as prose
#
# WHY THE RIGHT-HAND ANCHOR. `backup` is this product's domain word, so a
# case-insensitive `backupd` would be a guard nobody keeps: it flags 50
# occurrences of nine real identifiers -- BackupDetailPage,
# BackupDefaultsPage, BackupDomainPolicy, BackupDetail,
# TestBackupDataAreSeparateMounts, TestBackupDataOnEveryClaimedPlatform,
# BackupDomain, TestBackupDoesNotReturnWhileAWorkerIsStillReading,
# TestBackupDoesNotLeaveItRunningForever -- none of which is the product's
# name. `BackupDetailPage` is green here because the `D` is followed by a
# lowercase `e`; `backupd_session` is red because `_` is not a lowercase
# letter. BackupSet (7,774 occurrences), backup-set (1,989), BACKUP_DIR (40)
# and backup_status (12) are untouched by all three patterns, and
# scripts/rename/selftest.sh plants every one of those names in a green case,
# because "the guard is green" and "the guard matches nothing" are otherwise
# the same observation.
#
# Each pattern is anchored on a non-identifier character to its left too, so
# `CONFIRM_DELETE` is not an `RM_` variable and rclone's `ibm_signer.go` is
# not a `bm_` cookie. `rbm_` needs a pattern of its own for exactly that
# reason: the `r` in front of it is an identifier character, so the `bm_`
# pattern cannot see it.
#
# WHAT IT ALLOWS: three lists, and they mean three different things.
#
#   ALIASES     the deprecated aliases a rename deliberately KEEPS for one
#               release so an upgrade does not break. Allowed anywhere in
#               the tree, because an alias has to be minted, read, tested
#               and documented, and pinning it to a file list would turn
#               every one of those into a gate failure -- unless the entry
#               names a path, which is for a shim whose TOKEN is also
#               live elsewhere (see the format below).
#   PENDING     identifiers that are still on `main` and are being deleted
#               by a rename in flight, with no alias. Allowed anywhere, and
#               expected to disappear: when one does, this script says so
#               and the entry should be deleted.
#   PREEXISTING old-brand identifiers that are out of the rename's scope,
#               pinned as token+path pairs. Pinning the path is the point:
#               the occurrences that exist stay green, and the same name
#               appearing in a new file is a creation and goes red.
#
# THE ALIAS ENTRY FORMAT, and why an alias line carries more than a token
# (FR-43). A shim is a transitional upgrade-safety measure with a removal
# date, and "we will get to it" is not a date: the spec's own words. So
# every line on ALIASES is
#
#   <token>[@<path>] <#issue> <removal release, in words>
#
# and a line that is missing either the issue or the release is REFUSED --
# the script exits 1 naming the line, rather than quietly keeping an
# undated shim, which is how `RM_DEBUG` reached its third rename. The
# issue is the one that DELETES the shim (#895 for every EPIC R window);
# the release is FR-43's shim-table wording, "the release after the one
# that ships this EPIC".
#
# The optional `@<path>` exists for a shim whose token is not its own.
# R1.5's three shims -- the `/backupd-web` hardlinked entrypoint and
# FR-38's `/var/lib/backupd` and `/etc/backupd` legacy constants -- all
# tokenise to the bare `backupd`, which is simultaneously PENDING for the
# 1,400-file prose sweep. A bare `backupd` alias line would allow the
# token everywhere and mask that pending entry, so those entries name the
# file the shim lives in. `@` is the separator because no identifier this
# guard matches can contain one.
#
# The lists are consulted most-specific first: PREEXISTING's token+path,
# then ALIASES' token+path, then ALIASES' bare token, then PENDING. That
# ordering is what lets the lists be true at once during a rename in
# flight -- `backupd` is on PENDING because 1,433 files are still to be
# swept, the same token is pinned to the dated design records that will
# never be swept, and R1.5's shim files are alias-scoped inside it.
#
# A list entry that matches nothing left in the tree is reported and does
# NOT fail the run: a rename lands by deleting occurrences, and a guard that
# goes red the moment the thing it guards is fixed is a guard nobody keeps.
# Deleting the reported line is the fix. EPIC R depends on this directly:
# #887 lands the guard with every surviving occurrence on PENDING, and each
# later issue deletes its own entries as it sweeps them (FR-40).
#
# Shell and `git grep`, deliberately, where most of scripts/ is Python
# now (EPIC I, #672): the whole check is one pattern sweep over tracked
# files plus three fixed lists, it has to run in the pre-commit path, and
# `git grep` is the one tool that already knows what "tracked source" means.
#
# TWO SWEEPS AND ONE PASS. `git grep -o` prints every match with its file and
# line, so the classification below reads a single stream rather than
# re-scanning each hit line: on this tree the `backupd` family alone matches
# around eight thousand times, and a `grep`-plus-`sed` pipeline per hit line
# (which is what this did when the four #794 patterns matched two hundred)
# would spend minutes forking. There are two sweeps rather than one because
# the case rules differ per family and `git grep` has no per-alternative case
# flag: `-i` on the first four patterns would make `rm_` an `RM_` variable and
# `BackupDetailPage` a `Backupd` identifier, while the two earlier brands are
# genuinely live in `BACKUP_MANAGER_WEB_TEST_VAR` and `RCLONE-MANAGER-BACKUP-
# COMPLETE` as well as in lowercase prose.
#
# Registered in scripts/ci-local.sh (the gate .husky/pre-commit runs) and in
# .github/workflows/ci.yml. scripts/rename/selftest.sh is the proof it can
# still go red; scripts/ci-local.sh runs that too.
#
# Exit code contract, which is all a gate step needs:
#
#   0        every occurrence found is on a list
#   1        at least one is not, and the run printed file, line and name
#
# NOTE: the repository root comes from `git rev-parse --show-toplevel` of
# the CURRENT WORKING DIRECTORY, the same as the scripts/architecture
# checks, which is what lets the self-test point this at a throwaway tree.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# The kept deprecated aliases. Exactly the identifiers a rename keeps working
# for one release, and nothing else. Each one is primary nowhere: the
# preferred name is RETND_DEBUG / retnd_session / retnd_csrf / retnd_*, and
# these stay only so an in-place upgrade keeps reading the operator's
# existing environment, keeps existing browser sessions valid, and keeps an
# alert rule that was written against the old series firing.
#
# Every line is `<token>[@<path>] <#issue> <removal release>`; see THE ALIAS
# ENTRY FORMAT in the header for why, and for what happens to a line that
# carries no removal release.
#
# R1.4 (#889) mints the runtime half of FR-37's shim table:
#
#   BACKUPD / BACKUPD_*          the hook environment, exported beside every
#                                RETND_* built-in with identical values. A
#                                hook is an operator's Bash script and
#                                `$BACKUPD_BACKUP_STATUS` against a build
#                                that dropped it is the empty string, not an
#                                error. The bare `BACKUPD` is here for the
#                                same reason and is the name #932's review
#                                found no pattern matched -- there is one
#                                now.
#   BACKUPD_DEBUG,               the two input variables, read behind their
#   BACKUPD_INCREMENTAL_ENGINE   current names with one deprecation notice
#                                per name per process start
#                                (core/envcompat).
#   backupd_session,             accepted on a read and re-issued under the
#   backupd_csrf                 current name on that same read.
#   backupd_*                    the fourteen gauge families, duplicated
#                                under the old prefix with the deprecation
#                                in each HELP line. The token here is the
#                                bare prefix `backupd_`, which is what
#                                core/internal/metrics spells;
#                                docs/deployment.md is where the
#                                double-count caveat lives for an operator.
#
# #794's own two cookie shims stay, and #794's RM_DEBUG stays, and all of
# them now close together: FR-43 refuses to nest the second rename's window
# inside the third's, so every line below is deleted by the same issue in
# the same release.
#
# R1.5 (#890) adds five entries and every one of them is PATH-SCOPED, which
# is worth explaining because nothing else on this list is.
#
# Its three shims are FR-43's `/backupd-web` hardlinked entrypoint, the
# `ghcr.io/backupdproject/backupd` mirror, and FR-38's adoption of a state
# or configuration directory found at the pre-rename path. All three are
# PATHS, and this guard tokenises a path occurrence as the bare word
# `backupd`: `/backupd-web`, `/etc/backupd` and `/var/lib/backupd` are one
# token, and it is the same token as the 1,400 occurrences R2.1, R2.2, R2.3
# and R2.4 are still sweeping. A bare `backupd` alias would therefore be an
# alias for the whole tree -- it would mask `pending`, take the epic's
# largest remaining surface green, and report the `pending` entry that is
# actually doing the work as unused.
#
# So each entry names the ONE file that mints the shim: the constant FR-38
# substitutes (core/legacypath), the `ln` that creates the second entrypoint
# name (container/Dockerfile), the mirror declaration and the publish guard
# that refuses a release without it, and the installer's LEGACY_* constants,
# which are what lets it find an already-installed host's units, firewall
# rules, wrapper and mount points under their previous names. Everywhere
# else, `backupd` stays on `pending` and stays this epic's work.
#
# When the deprecation window closes, the alias and its line here go
# together, and this script reports the line as unused the moment the alias
# is gone.
aliases="$(
  cat <<'EOF'
RM_DEBUG #895 the release after the one that ships this EPIC
bm_session #895 the release after the one that ships this EPIC
bm_csrf #895 the release after the one that ships this EPIC
BACKUPD #895 the release after the one that ships this EPIC
BACKUPD_BACKUP_STATUS #895 the release after the one that ships this EPIC
BACKUPD_DEBUG #895 the release after the one that ships this EPIC
BACKUPD_INCREMENTAL_ENGINE #895 the release after the one that ships this EPIC
backupd_backup_set_state #895 the release after the one that ships this EPIC
backupd_session #895 the release after the one that ships this EPIC
backupd_csrf #895 the release after the one that ships this EPIC
backupd@core/legacypath/legacypath.go #895 the release after the one that ships this EPIC
backupd@container/Dockerfile #895 the release after the one that ships this EPIC
backupd@distribution/packaging/canonical.json #895 the release after the one that ships this EPIC
backupd@scripts/bdtools/release/publish_image.py #895 the release after the one that ships this EPIC
backupd@scripts/install/install_docker_host.py #895 the release after the one that ships this EPIC
EOF
)"

# Identifiers still on `main` that a rename in flight is DELETING (not
# aliasing), allowed anywhere and expected to disappear.
#
# This is EPIC R's surface, and it is why the guard lands red in #887 and
# goes green only when this list is empty (FR-40, and the Phase 2 exit gate).
# Every token below is an occurrence the epic is deleting, and each sub-issue
# deletes its own entries as it sweeps them:
#
#   the module path and the binaries        R1.3 (#888)
#   BACKUPD_* variables, the metric series,
#   the cookies, the earlier brands'
#   surviving identifiers                   R1.4 (#889)
#   /etc/backupd, /var/lib/backupd,
#   compose and unit names                  R1.5 (#890), landed: what it
#                                           kept is on `aliases` above,
#                                           scoped to the five files that
#                                           mint it
#   the eleven providers and packaging      R2.1 (#891)
#   prose, docs and ADRs                    R2.2 (#892)
#   the site and the brand art              R2.3 (#893)
#   the UI, tooltips and npm scopes         R2.4 (#894)
#   the cutover: `backupdproject`, the
#   repository coordinates, the RM_*
#   environment contract                    R2.5 (#895)
#
# The repository coordinates are here rather than on the pre-existing list
# below, deliberately and per FR-40: FR-41 moves them, so they are
# occurrences in transit like everything else. `backupdproject` is one token
# covering 904 files for the same reason.
pending="$(
  cat <<'EOF'
BACKUP_MANAGER_ANALYTICS
BACKUP_MANAGER_API_
BACKUP_MANAGER_API_PASSWORD
BACKUP_MANAGER_API_URL
BACKUP_MANAGER_API_USERNAME
BACKUP_MANAGER_AUTH_MODE
BACKUP_MANAGER_BACKUP_ROOT
BACKUP_MANAGER_DATA_DIR
BACKUP_MANAGER_KEY_DEK
BACKUP_MANAGER_LOG_LEVEL
BACKUP_MANAGER_PLATFORM
BACKUP_MANAGER_S3_CREDENTIALS
BACKUP_MANAGER_SSH_DISCOVERY_DIR
BACKUP_MANAGER_TEST_DAEMON_CHILD
BACKUP_MANAGER_TEST_DAEMON_CONFIG
BACKUP_MANAGER_WEB_TEST_BOOL_VAR
BACKUP_MANAGER_WEB_TEST_SERVE_AUTH_STORE
BACKUP_MANAGER_WEB_TEST_SERVE_CHILD
BACKUP_MANAGER_WEB_TEST_SERVE_CONFIG
BACKUP_MANAGER_WEB_TEST_SERVE_STATE_DB
BACKUP_MANAGER_WEB_TEST_VAR
backupd
Backupd
backupd_
backupd_internal
backupd_repo_production
backupdDebug
backupdproject
RCLONE_MANAGER_ALLOW_ROOT
RCLONE_MANAGER_ARTIFACT_PATH
RCLONE_MANAGER_MACHINES_NETWORK
RCLONE_MANAGER_SFTP_DEATH_GRACE
RCLONE_MANAGER_SFTP_PULL_BACKOFF
RCLONE_MANAGER_SFTP_TEST_BUDGET
RCLONE_MANAGER_SOURCE_PORT
RCLONE_MANAGER_TEST_DOES_NOT_EXIST_298
RCLONE_MANAGER_TEST_KEY_ENV
RCLONE_MANAGER_TEST_KEY_ENV_DOES_NOT_EXIST
RCLONE_MANAGER_TEST_KEY_ENV_ENCRYPTED
RCLONE_MANAGER_TEST_KEY_ENV_JUNK
RCLONE_MANAGER_TEST_KEY_ENV_LOGGING
RCLONE_MANAGER_TEST_KEYENCRYPTION_ENV
RCLONE_MANAGER_TEST_MIGRATION_DEK
RCLONE_MANAGER_TEST_SECRET
RCLONE_MANAGER_TEST_SFTP_KEY_ENCRYPTED_ENV
RCLONE_MANAGER_TEST_SFTP_KEY_ENV
RCLONE_MANAGER_TEST_SFTP_PASSPHRASE_ENV
RCLONE_MANAGER_TEST_SFTP_PASSPHRASE_ENV2
RCLONE_MANAGER_TEST_SFTPCONFIG_DIRCHAIN_KEYENC_ENV
RCLONE_MANAGER_TEST_SFTPCONFIG_KEY_ENV
RCLONE_MANAGER_TEST_SFTPCONFIG_KEY_ENV_JUNK
RCLONE_MANAGER_TEST_SFTPCONFIG_KEYENC_ENV
RCLONE_MANAGER_TEST_SFTPCONFIG_KEYENC_WRONG
RCLONE_MANAGER_TEST_STEADYSTATE_DEK
RCLONE_MANAGER_TEST_TESTCONNECTION_MIGRATION_DEK
RCLONE_MANAGER_TEST_V1UPGRADE_DEK
RCLONE_MANAGER_TEST_WRONGDEK
RCLONE_MANAGER_UNIT
RCLONE-MANAGER
EOF
)"

# Out of scope, pinned to the files they already live in.
#
# Eight groups, and none of them is a rename's to fix. The first four:
#
#   * The RM_* names are the ENVIRONMENT CONTRACT of another repository.
#     scripts/bdtools/e2e/run_tests_repo_gate.py and
#     scripts/e2e/three-machine-web-ui.sh set them for the suites in
#     backupdproject/backupd-tests, pinned at scripts/e2e/tests-repo.pin;
#     that repository reads them by these names. Renaming them here alone
#     breaks the e2e gate, so it is a two-repository change with a pin bump
#     in the middle. EPIC R does that sweep in R2.5 (#895), in lockstep with
#     the tests repository, at which point these lines move to `pending` and
#     then go; parking them for a fourth rename is what FR-40 refuses.
#   * bm_stopped and bm_routed are helper METHOD names in the two-machine
#     backup proof (scripts/bdtools/e2e/two_machine_backup.py): "run the
#     CLI on the stopped machine" and "...through the routed one". They are
#     internal to that file and rename cleanly, but they are not a runtime
#     identifier anybody upgrades across, so they belong to whoever next
#     touches that proof.
#   * The dated design records under docs/design/. A design note is the
#     record of a decision at the moment it was taken and is deliberately not
#     kept in step with later work (docs/epic-checklist.md §5), so renaming
#     one falsifies it -- and each HTML mockup has a PNG export beside it
#     that no text edit can follow. `Backup Manager.dc.html` is the oldest of
#     them and carries the second brand in its own filename; FR-40 names it
#     specifically as not-renamed.
#   * EPIC R's own three documents. The spec, the inventory and the
#     conformance matrix are an account OF the old name: they enumerate the
#     tokens being deleted, and docs/EPIC-R-rename-backupd-to-retnd.md §4
#     writes down the three names the self-test plants (`backupd_newthing`,
#     `BACKUPD_NEW_THING`, `BackupdWidget`), which exist nowhere else in the
#     tree and must stay red everywhere else. Pinned rather than pending
#     because a rename does not delete its own record.
#
# .github/workflows/rclone-upgrade-gate.yml is a fifth, one-file group: every
# occurrence in it is a `backupdproject/backupd#N` reference to an issue filed
# under the old coordinates. GitHub's transfer preserves `#N` and redirects
# the URL, and rewriting a record of where work was tracked falsifies it.
#
# scripts/ci-local.sh is a sixth: its gate-step prose names the prefixes the
# step looks for (`RCLONE_MANAGER_`, `BACKUP_MANAGER_`), exactly as this
# file's own header does, and this file is excluded by path for that reason.
# Pinned as the bare prefix rather than excluded by path, so a real
# `RCLONE_MANAGER_SOMETHING` added to the gate script is still a creation.
#
# R2.2 (#892) pins a seventh group and an eighth, and both are the reason
# FR-43's allowlist has a `preexisting` half at all: the prose sweep found
# occurrences of the first two brands that are CORRECT and would be made
# wrong by renaming them.
#
# The seventh is the deployment #795 was reported from, whose web-ui
# container could not resolve the engine and said so in one line:
# `dial tcp: lookup rclone-manager: no such host`. That line is quoted
# verbatim as the evidence for four separate pieces of behaviour
# (ui/shared/src/api/failure.ts, ui/shared/src/platform/localSession.ts,
# the regression test in ui/shared/src/test/activity-engine-unreachable.
# test.tsx and the rig's own README), and ui/shared/src/test/free-space-
# shared-volume.test.tsx carries that reporter's host path
# (/home/rom/rclone-manager/backups) as the fixture it measured. Rewriting
# a captured log line or a captured reading makes the record say something
# that was never observed, and §6's cut list keeps host directory names out
# of this epic besides. The same applies to the two comments that ATTRIBUTE
# a kept alias to the brand it came from -- apps/common/webhost/router.go
# and core/internal/obs/envlevel.go say RM_DEBUG is rclone-manager's -- and
# to .github/workflows/ci.yml, whose job name spells the guard's own
# pattern list and whose comment records the three-rename history, exactly
# as scripts/ci-local.sh's gate-step prose does.
#
# The eighth is `docs/design/Backup Manager.dc.html`, cited by name from
# docs/epic-checklist.md §5 and from ui/shared/src/api/client.ts's comment
# about the design canvas. The file is deliberately not renamed (group
# three above), so a reference to it by its real filename is right, and
# renaming the reference would point both readers at a path that does not
# exist.
#
# The ninth is #892's own prose. docs/deployment.md records the binary-name
# cut this project made in 0.3.3 ("`backup-manager` became `rbm`") and cites
# the design canvas by its real filename, and
# docs/adr/0023-moving-the-repository-coordinates-once-and-last.md records why
# a third rename needs an ADR at all: the first two left `RM_` variables and
# `bm_` cookies behind. A document whose subject IS the rename history names
# the names, the same way this epic's own three documents do.
#
# Two more groups are handled by path exclusion below rather than by a pin,
# because they are machine-written or wholly historical and pinning them
# would mean editing this list on every release: CHANGELOG.md, which is the
# account of all three renames, and provenance/**, whose released-artifact
# records name published binaries and are regenerated forward, never
# rewritten. rclone's own `ibm_signer.go`, which FR-40 also lists here, needs
# no entry at all: the left-hand anchor already makes it green, and
# selftest.sh keeps a case proving that.
#
# Every line is <token> <path>, one occurrence-site per line. Adding one of
# these names to a file that is not listed is a creation, and this guard
# treats it as one.
preexisting="$(
  cat <<'EOF'
Backup Manager docs/deployment.md
Backup Manager docs/epic-checklist.md
Backup Manager docs/EPIC-R-rename-backupd-to-retnd.md
backup manager docs/EPIC-R-rename-inventory.md
Backup Manager docs/EPIC-R-rename-inventory.md
Backup Manager ui/shared/src/api/client.ts
backup_manager docs/EPIC-R-rename-backupd-to-retnd.md
backup_manager docs/EPIC-R-rename-inventory.md
BACKUP_MANAGER_ scripts/ci-local.sh
BACKUP_MANAGER_API_PASSWORD docs/design/activity-terminal.html
BACKUP_MANAGER_API_URL docs/design/activity-terminal.html
BACKUP_MANAGER_API_USERNAME docs/design/activity-terminal.html
backup_manager_state docs/conformance/epic-r-matrix.md
backup-manager .github/workflows/ci.yml
backup-manager docs/adr/0023-moving-the-repository-coordinates-once-and-last.md
backup-manager docs/deployment.md
backup-manager docs/EPIC-R-rename-backupd-to-retnd.md
backup-manager docs/EPIC-R-rename-inventory.md
backup-manager scripts/ci-local.sh
backupd .github/workflows/rclone-upgrade-gate.yml
backupd docs/conformance/epic-r-matrix.md
backupd docs/design/788-incremental-ui-mockup.md
Backupd docs/design/788-incremental-ui-mockup.md
backupd docs/design/814-workflow-ui.html
backupd docs/design/815-step-terminal.html
backupd docs/design/906-shell-verification-findings.html
Backupd docs/design/activity-error-diagnostic.html
backupd docs/design/activity-terminal.html
backupd docs/design/global-terminal.html
Backupd docs/design/global-terminal.html
Backupd docs/design/README.md
backupd docs/design/retention-plan-destinations.html
backupd docs/design/run-backup-set.html
backupd docs/design/s3-destination-wizard.html
backupd docs/design/set-activity-terminal.html
backupd docs/design/ssh-auth-wizard.html
backupd docs/EPIC-R-rename-backupd-to-retnd.md
Backupd docs/EPIC-R-rename-backupd-to-retnd.md
backupd docs/EPIC-R-rename-inventory.md
Backupd docs/EPIC-R-rename-inventory.md
backupd_ docs/conformance/epic-r-matrix.md
backupd_ docs/EPIC-R-rename-backupd-to-retnd.md
backupd_ docs/EPIC-R-rename-inventory.md
BACKUPD_BACKUP_ docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_BACKUP_STATUS docs/conformance/epic-r-matrix.md
BACKUPD_BACKUP_STATUS docs/design/814-workflow-ui.html
BACKUPD_BACKUP_STATUS docs/EPIC-R-rename-backupd-to-retnd.md
backupd_csrf docs/EPIC-R-rename-backupd-to-retnd.md
backupd_csrf docs/EPIC-R-rename-inventory.md
BACKUPD_DEBUG docs/conformance/epic-r-matrix.md
BACKUPD_DEBUG docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_INCREMENTAL_ENGINE docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_NEW_THING docs/conformance/epic-r-matrix.md
BACKUPD_NEW_THING docs/EPIC-R-rename-backupd-to-retnd.md
backupd_newthing docs/conformance/epic-r-matrix.md
backupd_newthing docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_RECOVERY docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_RUN_ID docs/design/814-workflow-ui.html
BACKUPD_RUN_ID docs/EPIC-R-rename-backupd-to-retnd.md
backupd_session docs/conformance/epic-r-matrix.md
backupd_session docs/EPIC-R-rename-backupd-to-retnd.md
backupd_session docs/EPIC-R-rename-inventory.md
BACKUPD_SIGNAL_EXIT_CHILD_MODE docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_STEP_ docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_STEP_NAME docs/EPIC-R-rename-backupd-to-retnd.md
BACKUPD_WORKFLOW_STATUS docs/EPIC-R-rename-backupd-to-retnd.md
backupd_workflow_ docs/conformance/epic-r-matrix.md
BackupdError docs/design/activity-error-diagnostic.html
BackupdError docs/EPIC-R-rename-backupd-to-retnd.md
BackupdError docs/EPIC-R-rename-inventory.md
backupdproject .github/workflows/rclone-upgrade-gate.yml
backupdproject docs/conformance/epic-r-matrix.md
backupdproject docs/EPIC-R-rename-backupd-to-retnd.md
backupdproject docs/EPIC-R-rename-inventory.md
BackupdWidget docs/conformance/epic-r-matrix.md
BackupdWidget docs/EPIC-R-rename-backupd-to-retnd.md
bm_routed scripts/bdtools/e2e/two_machine_backup.py
bm_stopped scripts/bdtools/e2e/two_machine_backup.py
rclone_manager docs/EPIC-R-rename-backupd-to-retnd.md
rclone_manager docs/EPIC-R-rename-inventory.md
RCLONE_MANAGER_ docs/conformance/epic-r-matrix.md
RCLONE_MANAGER_ docs/EPIC-R-rename-backupd-to-retnd.md
RCLONE_MANAGER_ docs/EPIC-R-rename-inventory.md
RCLONE_MANAGER_ scripts/ci-local.sh
RCLONE_MANAGER_MACHINES_NETWORK docs/EPIC-R-rename-backupd-to-retnd.md
RCLONE_MANAGER_MACHINES_NETWORK docs/EPIC-R-rename-inventory.md
RCLONE_MANAGER_NEW_THING docs/conformance/epic-r-matrix.md
RCLONE_MANAGER_NEW_THING docs/EPIC-R-rename-backupd-to-retnd.md
RCLONE_MANAGER_SOURCE_PORT docs/EPIC-R-rename-backupd-to-retnd.md
RCLONE_MANAGER_SOURCE_PORT docs/EPIC-R-rename-inventory.md
RCLONE_MANAGER_UNIT docs/EPIC-R-rename-backupd-to-retnd.md
RCLONE_MANAGER_UNIT docs/EPIC-R-rename-inventory.md
rclone-manager .github/workflows/ci.yml
rclone-manager apps/common/webhost/router.go
rclone-manager core/internal/obs/envlevel.go
rclone-manager docs/EPIC-R-rename-backupd-to-retnd.md
rclone-manager docs/EPIC-R-rename-inventory.md
rclone-manager docs/adr/0023-moving-the-repository-coordinates-once-and-last.md
rclone-manager docs/conformance/epic-r-matrix.md
rclone-manager scripts/ci-local.sh
rclone-manager scripts/e2e/README.md
rclone-manager ui/shared/src/api/failure.ts
rclone-manager ui/shared/src/platform/localSession.ts
rclone-manager ui/shared/src/test/activity-engine-unreachable.test.tsx
rclone-manager ui/shared/src/test/free-space-shared-volume.test.tsx
RM_ADMIN_PASSWORD scripts/e2e/three-machine-web-ui.sh
RM_ADMIN_PASSWORD scripts/e2e/web-ui-smoke.mjs
RM_ADMIN_PASSWORD scripts/tests/testdata/three-machine-web-ui.help.txt
RM_ADMIN_USERNAME scripts/e2e/three-machine-web-ui.sh
RM_ADMIN_USERNAME scripts/e2e/web-ui-smoke.mjs
RM_ADMIN_USERNAME scripts/tests/testdata/three-machine-web-ui.help.txt
RM_ARTIFACTS_DIR scripts/e2e/three-machine-web-ui.sh
RM_ARTIFACTS_DIR scripts/e2e/web-ui-smoke.mjs
RM_ARTIFACTS_DIR scripts/tests/testdata/three-machine-web-ui.help.txt
RM_BACKUP_SET scripts/e2e/three-machine-web-ui.sh
RM_BACKUP_SET scripts/e2e/web-ui-smoke.mjs
RM_BACKUP_SET scripts/tests/testdata/three-machine-web-ui.help.txt
RM_BASE_URL scripts/bdtools/e2e/run_tests_repo_gate.py
RM_BASE_URL scripts/e2e/tests-repo.pin
RM_BASE_URL scripts/e2e/three-machine-web-ui.sh
RM_BASE_URL scripts/e2e/web-ui-smoke.mjs
RM_BASE_URL scripts/tests/testdata/three-machine-web-ui.help.txt
RM_BINARY scripts/bdtools/e2e/run_tests_repo_gate.py
RM_BREAK_ENGINE scripts/e2e/three-machine-web-ui.sh
RM_CHROMIUM_NO_SANDBOX scripts/e2e/three-machine-web-ui.sh
RM_CHROMIUM_NO_SANDBOX scripts/e2e/web-ui-smoke.mjs
RM_COMMIT scripts/bdtools/e2e/run_tests_repo_gate.py
RM_DOCKER_SOCKET scripts/e2e/three-machine-web-ui.sh
RM_ENGINE_CONTROL scripts/e2e/README.md
RM_ENGINE_CONTROL scripts/e2e/three-machine-web-ui.sh
RM_ENGINE_CONTROL scripts/e2e/web-ui-smoke.mjs
RM_ENGINE_CONTROL scripts/tests/testdata/three-machine-web-ui.help.txt
RM_ENGINE_UNREACHABLE scripts/e2e/README.md
RM_ENGINE_UNREACHABLE scripts/e2e/three-machine-web-ui.sh
RM_ENGINE_UNREACHABLE scripts/e2e/web-ui-smoke.mjs
RM_ENGINE_UNREACHABLE scripts/tests/testdata/three-machine-web-ui.help.txt
RM_EXEC_SET scripts/e2e/three-machine-web-ui.sh
RM_FRONT_PROXY_TLS scripts/e2e/three-machine-web-ui.sh
RM_HOOK_IMAGE scripts/e2e/three-machine-web-ui.sh
RM_IGNORE_HTTPS scripts/e2e/three-machine-web-ui.sh
RM_IGNORE_HTTPS scripts/e2e/web-ui-smoke.mjs
RM_MODE scripts/bdtools/e2e/run_tests_repo_gate.py
RM_MODE scripts/e2e/tests-repo.pin
RM_PRODUCT_IMAGE scripts/e2e/three-machine-web-ui.sh
RM_SEED_CYCLES scripts/e2e/README.md
RM_SEED_CYCLES scripts/e2e/three-machine-web-ui.sh
RM_SEED_CYCLES scripts/tests/testdata/three-machine-web-ui.help.txt
RM_SFTP_ONLY_SET scripts/e2e/three-machine-web-ui.sh
RM_SOURCE_DIR scripts/bdtools/e2e/run_tests_repo_gate.py
RM_UI_DIR .github/workflows/nightly-e2e.yml
RM_UI_DIR scripts/bdtools/e2e/run_tests_repo_gate.py
RM_UI_DIR scripts/e2e/tests-repo.pin
RM_WF_ scripts/e2e/three-machine-web-ui.sh
RM_WF_AFTER_FAIL_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_BEFORE_FAIL_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_CRASH_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_FINDINGS_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_GLOBAL_BEFORE_DIR scripts/e2e/three-machine-web-ui.sh
RM_WF_HOSTILE_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_MANY_STEPS_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_NO_HOOKS_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_POLL_SECONDS scripts/e2e/three-machine-web-ui.sh
RM_WF_REJECTED_DIR scripts/e2e/three-machine-web-ui.sh
RM_WF_SCRIPT_PREFIX scripts/e2e/three-machine-web-ui.sh
RM_WF_SECRET_ENV scripts/e2e/three-machine-web-ui.sh
RM_WF_SECRET_SET scripts/e2e/three-machine-web-ui.sh
RM_WF_SECRET_VALUE scripts/e2e/three-machine-web-ui.sh
RM_WF_SLOW_SET scripts/e2e/three-machine-web-ui.sh
RM_WORKFLOW_CONTROL scripts/e2e/three-machine-web-ui.sh
RM_WORKFLOW_RUNNER_NAME scripts/e2e/three-machine-web-ui.sh
RM_WORKFLOW_SET scripts/e2e/three-machine-web-ui.sh
RM_WORKFLOWS scripts/e2e/three-machine-web-ui.sh
EOF
)"

# Not source, so not this guard's business. Every one of these either
# RECORDS history (which is the one place an old name is supposed to keep
# appearing) or is machine-written from something this guard does scan.
#
#   CHANGELOG.md                      the record of the renames themselves
#   **/go.sum                         module hashes, and rclone's own
#                                     ibm_* backends live in there
#   **/node_modules/**                vendored third-party JS
#   provenance/**                     released-artifact records: SBOM,
#                                     checksums, third-party licences
#   core/apicontract/contract.gen.go  generated from api/v1/openapi.json,
#   ui/shared/src/api/generated/**    which IS scanned, so a cookie name in
#                                     the contract is still caught -- at
#                                     the source rather than twice more in
#                                     its DO-NOT-EDIT copies
#
# And the last two are this file and its self-test, which are the only
# files in the tree whose JOB is to write these names down: the allowlist
# above is a list of old-brand identifiers, and the self-test plants them on
# purpose. Scanning either would report the guard's own contents as drift.
# Named file by file rather than as scripts/rename/** so that a third file
# added in this directory is scanned like anything else.
excluded_paths=(
  ':!CHANGELOG.md'
  ':!**/go.sum'
  ':!**/node_modules/**'
  ':!provenance/**'
  ':!core/apicontract/contract.gen.go'
  ':!ui/shared/src/api/generated/**'
  ':!scripts/rename/check-brand-drift.sh'
  ':!scripts/rename/selftest.sh'
)

# The left anchor every pattern shares: start of line, or a character that
# cannot be part of an identifier. POSIX ERE has no \b (git grep would need
# --perl-regexp, which is a build-time option this script will not depend
# on), and this is the thing \b would have been for.
boundary='(^|[^A-Za-z0-9_])'

# The case-sensitive families, matched as whole identifiers so a finding can
# be NAMED: `RM_[A-Z]` locates the variable, `RM_[A-Z][A-Za-z0-9_]*` reports
# which one. The `backupd` and `Backupd` alternatives spell the right-hand
# anchor out three ways -- an underscore or another identifier character
# continues the token, a non-identifier character ends it, and `$` is
# end-of-line, which `[^a-z]` cannot match and which is where
# `/var/lib/backupd` lives.
#
# `BACKUPD([^A-Za-z0-9_]|$)` is the twelfth pattern and the one #932's
# review asked for (R1.4, #889): the BARE uppercase name, with no
# trailing underscore. `BACKUPD` is a real runtime identifier -- the
# variable this product sets to "1" so a hook can tell it is running under
# it at all (core/internal/workflow's ReservedEnvName) -- and
# `BACKUPD_[A-Z]` cannot see it, because there is nothing after the D.
# It sat green through the whole of #887 for that reason. It is
# alternative-ordered after `BACKUPD_[A-Z][A-Za-z0-9_]*` so POSIX
# leftmost-longest still reports `BACKUPD_RUN_ID` as itself rather than as
# a bare `BACKUPD` with a suffix nobody sees; scripts/rename/selftest.sh
# plants both to hold that.
cs_re="$boundary"'(RM_[A-Z][A-Za-z0-9_]*|BM_[A-Z][A-Za-z0-9_]*|bm_[a-z][A-Za-z0-9_]*|rbm_[a-z][A-Za-z0-9_]*|BACKUPD_[A-Z][A-Za-z0-9_]*|BACKUPD([^A-Za-z0-9_]|$)|backupd_[A-Za-z0-9_]*|backupd[A-Z0-9][A-Za-z0-9_]*|backupd([^A-Za-z0-9_]|$)|Backupd_[A-Za-z0-9_]*|Backupd[A-Z0-9][A-Za-z0-9_]*|Backupd([^A-Za-z0-9_]|$))'

# The case-insensitive family: the organisation and the two earlier brands,
# which are live in every case (`BACKUP_MANAGER_WEB_TEST_VAR`,
# `RCLONE-MANAGER-BACKUP-COMPLETE`, `rclone-manager` in prose,
# `docs/design/Backup Manager.dc.html`). `RCLONE_MANAGER_[A-Z]` needs no
# alternative of its own: the identifier continuation here is what carries
# the whole variable name into the report.
ci_re="$boundary"'(backupdproject|rclone[-_ ]manager[A-Za-z0-9_]*|backup[-_ ]manager[A-Za-z0-9_]*)'

# -I so a binary file is never scanned, -n because a finding has to name a
# line somebody can open, -o so each match arrives as its own record rather
# than as a line to re-scan. `|| true` because git grep exits 1 for "no
# matches", which is a clean tree and not an error.
hits="$(
  {
    git grep -n -o -I -E "$cs_re" -- . "${excluded_paths[@]}" || true
    git grep -n -o -i -I -E "$ci_re" -- . "${excluded_paths[@]}" || true
  }
)"

# One pass over the matches. Each record is `path:lineno:matchtext`, where
# matchtext carries the anchors the pattern matched around the identifier;
# stripping one non-identifier character from each end leaves the token.
#
# The three lists arrive through the environment rather than through -v
# because they are multi-line and -v applies escape processing to its value.
report="$(
  ALIASES="$aliases" PENDING="$pending" PREEXISTING="$preexisting" \
    awk '
    function load(blob, set,   n, i, parts) {
      n = split(blob, parts, "\n")
      for (i = 1; i <= n; i++) {
        if (parts[i] != "") set[parts[i]] = 1
      }
    }
    # The alias list is the one with structure: `<token>[@<path>] <#issue>
    # <removal release>`. A line that carries no issue or no release is a
    # shim with no removal date, which FR-43 refuses outright, so this
    # reports it as a MALFORMED record and the shell turns that into a
    # non-zero exit. Silently ignoring it would make the refusal a
    # comment.
    function load_aliases(blob,   n, i, parts, fields, nf, spec, at) {
      n = split(blob, parts, "\n")
      for (i = 1; i <= n; i++) {
        if (parts[i] == "") continue

        nf = split(parts[i], fields, /[ \t]+/)
        spec = fields[1]
        if (nf < 3 || fields[2] !~ /^#[0-9]+$/) {
          print "M\t  " parts[i]
          continue
        }

        alias_spec[spec] = 1
        at = index(spec, "@")
        if (at == 0) {
          alias_token[spec] = spec
        } else {
          alias_token[spec] = substr(spec, 1, at - 1)
          alias_path[spec] = substr(spec, at + 1)
        }
      }
    }
    BEGIN {
      load_aliases(ENVIRON["ALIASES"])
      load(ENVIRON["PENDING"], pend)
      load(ENVIRON["PREEXISTING"], pre)
      allowed = 0
      nv = 0
    }
    # Which alias entry, if any, covers this occurrence. A path-scoped
    # entry only covers its own file; an unscoped one covers the tree.
    # Checked most-specific first, so a token that is BOTH a scoped shim
    # and live elsewhere stays red elsewhere.
    function alias_match(token, path,   spec) {
      spec = token "@" path
      if (spec in alias_spec) return spec
      if (token in alias_spec) return token

      return ""
    }
    {
      i = index($0, ":")
      if (i == 0) next
      path = substr($0, 1, i - 1)
      rest = substr($0, i + 1)
      i = index(rest, ":")
      if (i == 0) next
      lineno = substr(rest, 1, i - 1)
      token = substr(rest, i + 1)
      sub(/^[^A-Za-z0-9_]/, "", token)
      sub(/[^A-Za-z0-9_]$/, "", token)
      if (token == "") next

      # One line can carry the same name twice (a compose file mapping a
      # variable to itself, a test table naming both cookies), and the two
      # sweeps can both reach the same token; one finding per name per line.
      key = path ":" lineno ":" token
      if (seen[key]++) next

      if ((token " " path) in pre) {
        found_pre[token " " path] = 1
        allowed++
        next
      }
      spec = alias_match(token, path)
      if (spec != "") {
        found_alias[spec] = 1
        allowed++
        next
      }
      if (token in pend) {
        found_pend[token] = 1
        allowed++
        next
      }
      violation[++nv] = "  " path ":" lineno ": " token
    }
    END {
      for (i = 1; i <= nv; i++) print "V\t" violation[i]
      stale_aliases(found_alias)
      stale("PENDING", "pending", found_pend)
      stale("PREEXISTING", "pre-existing", found_pre)
      print "C\t" allowed
    }
    function stale(var, label, found,   n, i, parts) {
      n = split(ENVIRON[var], parts, "\n")
      for (i = 1; i <= n; i++) {
        if (parts[i] != "" && !(parts[i] in found)) {
          printf "S\t  %-12s %s\n", label, parts[i]
        }
      }
    }
    # Reported by the whole field-1 spec, `token` or `token@path`, so the
    # line to delete is the line the report names.
    function stale_aliases(found,   spec) {
      for (spec in alias_spec) {
        if (!(spec in found)) printf "S\t  %-12s %s\n", "alias", spec
      }
    }
  ' <<<"$hits"
)"

# Partitioned with three `sed` passes rather than a shell `while read` loop:
# the loop was fine when a red run named a handful of identifiers and
# quadratic when #887 landed a list of eleven thousand, because appending to
# a growing string copies it every time. This is the difference between a
# guard that runs in the pre-commit path and one somebody takes out of it.
violations="$(sed -n 's/^V	//p' <<<"$report")"
stale="$(sed -n 's/^S	//p' <<<"$report")"
malformed="$(sed -n 's/^M	//p' <<<"$report")"
allowed_count="$(sed -n 's/^C	//p' <<<"$report")"

# An alias with no closing issue or no removal release is not an alias, it
# is the old name kept indefinitely (FR-43), so this is fatal and it is
# checked before the tree is judged: a malformed line means the allowlist
# this run applied is not the one somebody thought they wrote.
if [ -n "$malformed" ]; then
  echo "check-brand-drift: FAILED: alias entry with no closing issue and removal release:" >&2
  printf '%s\n' "$malformed" >&2
  cat >&2 <<'EOF'
check-brand-drift: every line on the alias list is
    <token>[@<path>] <#issue> <removal release, in words>
  because a shim is a transitional measure with a date and an owner, and an
  undated one is how RM_DEBUG reached its third rename (FR-43). The issue is
  the one that DELETES the shim; the release is FR-43's shim-table wording.
  An occurrence being deleted rather than kept belongs on `pending` instead,
  which takes a bare token.
EOF
  exit 1
fi

# The list entries nothing matched any more. Reported, never fatal: see the
# header. Printed before the verdict so a red run does not bury them.
if [ -n "$stale" ]; then
  echo "check-brand-drift: these allowlist entries no longer match anything in the tree:"
  printf '%s\n' "$stale"
  echo "check-brand-drift: that identifier is gone, so delete the line above from scripts/rename/check-brand-drift.sh. Not a failure."
fi

if [ -n "$violations" ]; then
  echo "check-brand-drift: FAILED: old-brand identifier created outside the allowlist (#794, #887):" >&2
  printf '%s\n' "$violations" >&2
  cat >&2 <<'EOF'
check-brand-drift: this project is on its third name. RM_ and
  RCLONE_MANAGER_ are rclone-manager's prefixes, bm_ and BACKUP_MANAGER_ are
  backup-manager's, and `backupd` is the name EPIC R (#885) is retiring; a
  new identifier must use the new one. Name it RETND_<THING>
  (environment) or retnd_<thing> (cookie).
  If the name above is not new -- a file moved, an occurrence this epic has
  not swept yet, or an old-brand identifier no rename owns -- add it to the
  pending or the pre-existing list in scripts/rename/check-brand-drift.sh,
  with the reason, in the same shape as the entries already there.
  A `Backup`-prefixed identifier whose next character is a lowercase letter
  (BackupDetailPage, BackupSet, backup-set) is not this guard's business and
  is not what it just reported.
EOF
  exit 1
fi

echo "check-brand-drift: ok ($allowed_count allowlisted occurrence(s), no new RM_/BM_/bm_/rbm_/backupd/backupdproject/rclone-manager/backup-manager identifier)"
