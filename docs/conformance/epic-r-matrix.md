# EPIC R conformance matrix

Every gate line declares, as a cell: what it certifies, where it is asserted, and
the planted violation that has to make it red. One row per line of the spec's two
exit gates, one per entry of its section 4 planted-violation table that no gate
line already carries, with nothing dropped for not being ready yet.

`docs/EPIC-R-rename-backupd-to-retnd.md` is the contract. This file is the account
of which parts of it are checked by something that has been watched to fail, which
parts are checked by nothing, and which issue owns each gap.

## Why this file exists in this shape

A rename is the change most likely to produce a matrix that is entirely green and
entirely worthless, because the obvious check — "does the old name still appear?" —
passes trivially the moment somebody runs search and replace, and says nothing
about whether the product still works. Three checks in this repository were found
in a single day that could not fail, and this epic adds a guard whose naive form
has **fifty** false positives on the tree it ships with (`BackupDetailPage`,
`BackupDefaultsPage`, `BackupDomainPolicy` and six more). A guard that noisy gets
deleted, and a matrix that certifies it is worse than no matrix.

So every row carries a **falsification**: the specific mutation that must turn
that cell red. A row whose falsification has been run and watched fail is `PASS`.
A row whose falsification cannot be run yet, because the code it would mutate does
not exist, is `BLOCKED`, with the issue that unblocks it. There is no third
reading, and in particular there is no row that is green because nobody looked.

Two rows carry a **positive control** as well as a falsification, and they are the
two that matter most. R1.1's control is a file containing every lookalike the guard
must not catch: without it, "the guard is green" is indistinguishable from "the
guard matches nothing". R1.9's control is that a genuinely fresh install still
first-runs: without it, "it adopted the legacy path" is indistinguishable from a
product that can no longer be installed.

## How to read an outcome

| Outcome | Means |
| --- | --- |
| `PASS` | The check exists, runs in `scripts/ci-local.sh`, and has been shown to go red against a real planted violation. The falsification column names the mutation and it is automated in `scripts/rename/selftest.sh` or `scripts/compat/selftest.sh`. |
| `PARTIAL` | Part of the claim is checked and shown to fire, and the rest is not: either it waits on an unlanded issue, or it cannot be run here at all. The row says which half is which and which of those two it is. |
| `BLOCKED` | The check is specified here and cannot conclude today, because the code it certifies is not merged. Not a pass, and not a fail. The row names the issue. |

`BLOCKED` is a declaration, and it is checked rather than trusted — but not yet.
The four structural tests below live in `core/tests/compat/matrix_test.go` and are
bound today to a single hard-coded pair of paths, `docs/EPIC-E-alternative-storage.md`
and `docs/conformance/epic-e-matrix.md` (`specPath`, `matrixPath`). Nothing checks
*this* file until they take a table of spec/matrix pairs, and #887 owns that change,
because the first code issue of the EPIC is where an instrument belongs. Until it
lands, every claim below is held by a reader. The tests, once they see this file:
`TestTheMatrixDoesNotCiteSuitesThatDoNotExist` fails if a `PASS` or `PARTIAL` row
cites a path the repository does not have, and
`TestEveryBlockedRowCitesAnIssueThatIsStillOpen` resolves every issue a `BLOCKED`
row names against GitHub. `TestTheMatrixHasARowPerGateLine` compares the row ids
against the spec's own exit-gate checkbox lists, and
`TestTheSpecsExitGateBoxesAgreeWithTheMatrix` holds those checkboxes to these
outcomes in both directions, so a box cannot be ticked by hand and a row cannot be
dropped to tidy the table.

Every row in this file is `BLOCKED` on the day it is written, which is correct: the
spec is the only thing that has landed.

## Where the checks live

| Thing | Path |
| --- | --- |
| The old-name guard and its mutation self-test | `scripts/rename/check-brand-drift.sh`, `scripts/rename/selftest.sh` |
| The CLI, config and upgrade surface corpus | `core/tests/compat` |
| The FR-38 adoption warning, the two-journal refusal and the fresh-install control | `core/tests/compat` (new cells `20-legacy-state-adoption`, `21-fresh-install-first-run`, `22-two-journals-refusal`) |
| Compose start from an unedited pre-rename file | `distribution/compose` |
| Hook environment, executed rather than inspected | `core/internal/workflow` |
| Metric names and the gauges-only rule | `core/internal/metrics` |
| Cookie read-compat and re-issue | `apps/common/auth/local`, `apps/common/csrf` |
| The installer's mount/unit/config migration | `scripts/install/test_install_docker_host.py` |
| The planted violations, automated | `scripts/rename/selftest.sh`, `scripts/compat/selftest.sh` |
| The gate step that runs them | `scripts/ci-local.sh`, `.github/workflows/ci.yml` |

---

## Phase 1 — the product renames itself

| # | Outcome | Certifies | Where | Falsification |
| --- | --- | --- | --- | --- |
| R1.1 | BLOCKED (#887) | The old-name guard fires on a new old-brand identifier in any of the three anchored spellings, and does **not** fire on the domain word `backup` or on the nine `BackupD[a-zA-Z]` identifiers that already exist. | `scripts/rename/check-brand-drift.sh`; `scripts/rename/selftest.sh` in throwaway repositories | Three reds: a file containing `backupd_newthing`, one containing `BACKUPD_NEW_THING`, one containing `BackupdWidget`; each must name file and line. **Positive control, and the load-bearing half:** one green over a file containing `BackupDetailPage`, `BackupDefaultsPage`, `BackupDomainPolicy`, `BackupDetail`, `TestBackupDataAreSeparateMounts`, `TestBackupDataOnEveryClaimedPlatform`, `BackupDomain`, `TestBackupDoesNotReturnWhileAWorkerIsStillReading`, `TestBackupDoesNotLeaveItRunningForever`, `BackupSet`, `backup-set`, `BACKUP_DIR` and `backup_status`. Without that control a guard matching nothing is indistinguishable from a guard that works. |
| R1.2 | BLOCKED (#888, #889, #890) | No `pending` entry remains for anything Phase 1 owns: the module path, the binaries, the runtime identifiers, the deployment identity. | `scripts/rename/check-brand-drift.sh`'s own stale-entry report, quoted in each landing PR | The guard reports a list entry that matches nothing and does **not** fail on it, by design, so the falsification is the reverse: re-adding one deleted occurrence must produce a red, and deleting a `pending` entry whose occurrences still exist must also produce a red. Both run in `selftest.sh`. |
| R1.3 | BLOCKED (#888) | Every path-shaped tool resolves the new module path, and nothing in the tree instructs anybody to `go install` or `go get` it. | `go build ./...`, `go vet ./...`, `scripts/architecture/*.sh`, `verify-core-without-distribution.sh`, `scripts/docs/package-doc.baseline`, `.golangci.yml`; plus a new scan over docs, scripts and workflows | The failure mode here is a check that silently stops matching anything rather than one that goes red, so each path-shaped check gets a mutation that must be caught: a file placed in a layer it is not allowed in must still be refused **after** the sweep. For the `go install` half: a doc line reading `go install github.com/backupdproject/retnd/core/cmd/retnd@latest` must turn the scan red. |
| R1.4 | BLOCKED (#888) | `retnd --help`'s usage block, every verb in the dispatch table and every row of `docs/site/reference.html` agree; the compat corpus's CLI cells differ from their previous capture in the name and nothing else. | `distribution/packaging/site_reference_test.go`, `TestUsage_EveryRegisteredCommandIsPinned` and `TestUsage_NamesEveryBackupSetVerb` in `core/cmd/retnd`, cell `06b-cli-usage-block` in `core/tests/compat` | A verb renamed in the dispatch table only must turn `site_reference_test.go` red. For the corpus half: a variant that also reorders the usage block must turn the cell red on a line it already holds, which is the check that a regeneration cannot launder an unrelated change through this rename. |
| R1.5 | BLOCKED (#890) | A container built from this tree starts from an **unedited** pre-rename compose file, through `/backupd-web`, and from the new one through `/retnd-web`. | `distribution/compose` contract tests, run twice against the same image | An image built without the `/backupd-web` hardlink must fail the unedited-compose run with `exec /backupd-web: no such file or directory`, which is the exact message `container/compose.yaml`'s header warns about. The control is that the new path is not merely a copy: a build where `/retnd-web` is absent must fail the other run. |
| R1.6 | BLOCKED (#889) | A hook script reading `$BACKUPD_BACKUP_STATUS` and one reading `$RETND_BACKUP_STATUS` observe identical values. | `core/internal/workflow/env_test.go`, asserting through **executed** scripts rather than the exported map | A build exporting only `RETND_*` must fail. Asserting on the map would pass against a build that exports the legacy name and then unsets it, and the whole point of this row is what a Bash script sees, so the assertion is a script that prints the variable and a comparison of its stdout. |
| R1.7 | BLOCKED (#889) | `retnd_*` and `backupd_*` gauges are both scraped with identical values, the legacy `# HELP` names its replacement, and no counter is duplicated. | `core/internal/metrics/metrics_test.go`, `snapshot_test.go` | Two reds. A build emitting only `retnd_*` must fail the name pin. **And the control that keeps the rule honest:** adding a counter to the duplicated set must also fail, because a duplicated counter double-counts under any aggregation and the gauges-only rule is the reason this window is safe at all. |
| R1.8 | BLOCKED (#889) | A session cookie minted before the upgrade authenticates after it, and is re-issued under `retnd_session`. | `apps/common/auth/local`, `apps/common/csrf`, and the cookie declarations in `api/v1/openapi.json` | A build rejecting `backupd_session` must fail the survival test. The re-issue half is asserted separately, because a build that accepts the old name forever and never re-issues passes the survival test and never closes the window — which is how `bm_session` outlived its own deprecation. |
| R1.9 | BLOCKED (#890) | All four cells of FR-38's table: state at `/var/lib/backupd` with nothing at `/var/lib/retnd` is adopted, served and warned about on every start and creates no administrator account and no enrollment token; two different populated directories are refused with both named; one host directory mounted at both container paths is **not** refused; a genuinely empty deployment still first-runs. | New compat cells `20-legacy-state-adoption` (pinning the warning), `22-two-journals-refusal` (pinning the refusal) and `21-fresh-install-first-run` in `core/tests/compat` | **The epic's most important row.** The planted violation is a build that takes the first-run path when the legacy directory holds a database; cell 20 must go red. **Two controls, and both are load-bearing:** cell 21 asserts a genuinely empty deployment still first-runs, so "it adopted" cannot be satisfied by a product that can no longer be installed; cell 22's control drops the same-device-and-inode test and requires the installer's own rollback-window compose file — one host directory mounted at both paths — to stop starting, so "it refused the ambiguous case" cannot be satisfied by a product that refuses whenever both paths are populated. A fourth assertion checks the negative directly: after the adoption, no administrator row and no enrollment token were created. |
| R1.10 | BLOCKED (#887, #888, #889, #890) | `scripts/ci-local.sh` green; `check-contract-drift.sh` and `check-client-paths.sh` pass with the regenerated bindings; the release gate's `needs` list covers every job. | `scripts/ci-local.sh`, `scripts/api/check-contract-drift.sh`, `scripts/api/check-client-paths.sh`, `scripts/tests/release-gate-covers-every-job.test.sh` | A hand-edited `core/apicontract/contract.gen.go` that does not match `api/v1/openapi.json` must turn drift red; a new CI job left out of the release gate's `needs` must turn the release-gate test red. Both already fire today and are re-run after the sweep, because a path-pattern change is exactly how a check stops seeing its own subject. |

## Phase 2 — everything a user, a store reviewer or a contributor reads

| # | Outcome | Certifies | Where | Falsification |
| --- | --- | --- | --- | --- |
| R2.1 | BLOCKED (#895) | `check-brand-drift.sh` is green with an **empty `pending` list**, and every remaining occurrence of the old name is on `aliases` with its closing issue or on `preexisting` with its written reason. | `scripts/rename/check-brand-drift.sh`, `scripts/rename/selftest.sh` | Moving one surviving occurrence off its list must turn the guard red, and adding a `preexisting` token to a file the list does not name must turn it red too — the path half of that list is what makes "the occurrences that exist stay green, the same name in a new file is a creation" true rather than aspirational. |
| R2.2 | BLOCKED (#891) | All eleven providers' packaging manifests are regenerated through `distribution/packaging`'s derivation rather than hand-edited; the cross-provider conformance suite passes. | `distribution/packaging/matrix_test.go`, `matrix_guards_test.go`, `derive_test.go`, `conformance_test.go`; `apps/common/tests` | A hand-edited `canonical.json` that the derivation would not produce must turn the derivation test red. The control: a provider removed from the matrix must also turn it red, so "all eleven" is counted rather than assumed. |
| R2.3 | BLOCKED (#892, #893) | `reference.html`'s command table matches the dispatch table; all five site pages, their `<title>`s and the wordmark say `retnd`; 44 screens are re-recorded through the four capture scripts against `createMockApi` with the clock pinned. | `distribution/packaging/site_reference_test.go`; `docs/site/tools/capture-*.mjs`; the landing PR names the `tests-repo.pin` sha the Playwright was borrowed at | A verb added to the dispatch table and not to the page must turn `site_reference_test.go` red. The capture half cannot be falsified by a test — a picture is not an assertion — so its evidence is procedural and stated as such: the clock is pinned (`page.clock.setFixedTime`), so a re-record with no UI change produces a byte-identical file, and a diff that shows movement in a screen the epic did not touch is the signal. A hand-taken image is refused on sight. |
| R2.4 | PARTIAL, and it cannot be otherwise here | Every store-listing row whose screenshot shows the product name is **outstanding**, not passing, and `docs/submission/screenshots.md` says why it cannot be satisfied here. | `docs/conformance/submission-preflight.md`, `docs/submission/screenshots.md` | This row is `PARTIAL` by construction and will stay so: store screenshots require real hardware, a real installation and a real SFTP source, and substituting a mock there is forbidden by `docs/epic-checklist.md` §10. The checkable half is the **direction**: a row marked passing while its screenshot still shows the old name is the failure, so the falsification is flipping one such row to passing and requiring the preflight report to refuse it. |
| R2.5 | BLOCKED (#893) | The site's "What has not been proven" section states which surfaces were re-captured, which store screenshots still show the old name, and that no provider store listing has been re-reviewed. | `docs/site/index.html#honest` | Ungated by construction — it is prose about what was not done, and no test can know that. Its falsification is the honest one: if R2.4 stays `PARTIAL` and this section does not say so, the phase exit gate is not met, and the reviewer of the landing PR is the check. Stated here rather than dressed up as automation. |
| R2.6 | BLOCKED (#894) | `tooltips.json` has no entry nothing names and no explained control without one; the frontend gate is green; the nine `BackupD[a-zA-Z]` identifiers are unchanged. | `ui/shared`'s tooltip registry tests, typecheck, per-provider typecheck, eslint, vitest, build; plus a test naming the nine identifiers | Renaming `BackupDetailPage` to `RetndDetailPage` must turn the naming test red, which is the assertion that the rename stopped where it was supposed to. The registry half already fires today: an entry nothing names is red, and a control with no entry is not — that asymmetry is §5 of the checklist and this epic does not change it. |
| R2.7 | BLOCKED (#895) | `backupd-tests` suites pass against a stamped build of this tree; `tests-repo.pin` and their `build-under-test.json` name each other; nightly-e2e is green. | `scripts/e2e/tests-repo.pin`, `scripts/bdtools/e2e/run_tests_repo_gate.py`, `.github/workflows/nightly-e2e.yml`, and `build-under-test.json` in `backupdproject/backupd-tests` | A pin bumped on one side only must turn the gate red, and that is the falsification worth running because it is the way this pair has gone wrong before: the pin's own ledger records a bump whose other half "still has to move by hand". The suites' own pass is not evidence of this row; the lockstep is. |
| R2.8 | BLOCKED (#895) | `NOTICE`, the licence inventory and `provenance/**` are regenerated forward, and no already-published record is rewritten. | The licence-inventory gate; `provenance/{checksums.txt,release-provenance.json,sbom.spdx.json,third-party-licenses.json}` | A regeneration that rewrites an existing `release-provenance.json` entry must be refused: the falsification is a diff touching a published record, and the check is that `provenance/**` grows and never changes a line it already has. |
| R2.9 | BLOCKED (#890, #895) | FR-42 holds four ways: an unedited pinned compose file starts, works and warns; the new compose file finds its state at the new path; two different populated directories are refused while one mounted twice is not; an installer upgrade migrates mounts, units and persisted config and shows only new names. | `distribution/compose`, cells `20-legacy-state-adoption`, `21-fresh-install-first-run` and `22-two-journals-refusal` in `core/tests/compat`, `scripts/install/test_install_docker_host.py` | The build that first-runs when the legacy path holds a database fails the first. For the installer half: a migration that moves the mount without rewriting `config.yaml`'s absolute `state.database` path must fail, because that combination produces a deployment whose config names a path that no longer exists — the failure the installer exists to prevent. A half-migrated host with both systemd units enabled must be refused, not tolerated. |
| R2.10 | BLOCKED (#895) | No new config key exists: a config written by this build parses byte-identically under the previous build. | `core/tests/compat`, the older-build parse cell | A variant that adds a `legacy_paths:` key must be refused by the previous build under `KnownFields(true)`. That refusal **is** the proof the key was never safe, which is why the mutation is kept rather than described: it is the only way to show that the compiled-in-constant decision was not merely convenient. |
| R2.11 | BLOCKED (#892) | The CHANGELOG `[Unreleased]` entry names the issue, what changed, why, and what an existing deployment sees, including the one-release windows and the metric double-count caveat. | `CHANGELOG.md` | Ungated. Its falsification is a reader's: an entry that does not let an operator predict what their deployment does on upgrade has not met the line, and `docs/epic-checklist.md` §9 is the standard. Named here so it is reviewed rather than assumed. |
| R2.12 | BLOCKED (#895) | This file has no `BLOCKED` row, and every `PASS` row's falsification has been run and watched to fail. | `core/tests/compat`'s matrix tests: `TestTheMatrixHasARowPerGateLine`, `TestTheMatrixDoesNotCiteSuitesThatDoNotExist`, `TestEveryBlockedRowCitesAnIssueThatIsStillOpen`, `TestTheSpecsExitGateBoxesAgreeWithTheMatrix` — **after #887 generalises them from EPIC-E's hard-coded `specPath`/`matrixPath` to a table of pairs.** Until then this row is held by a reader, which is stated rather than papered over | Editing a row's outcome from `BLOCKED` to `PASS` without the check existing must turn the citation test red; ticking a spec checkbox whose row is not `PASS` must turn the agreement test red. Both directions, because this file's predecessor sat with seven rows claiming too little long after the work landed and nothing in the repository could tell. |

## Rows with no gate line of their own

These come from section 4's planted-violation table and are not restatements of a
gate line above. They are here rather than folded into a neighbour because each
one is a distinct mutation with a distinct owner.

| # | Outcome | Certifies | Where | Falsification |
| --- | --- | --- | --- | --- |
| V.1 | BLOCKED (#890) | The config-directory half of FR-38: a resolved configuration directory with no `config.yaml` beside a legacy directory holding one is an adoption, not a fresh install. | New compat cell `20-legacy-state-adoption`, second case | A build that treats the missing `config.yaml` as a fresh install must turn the cell red. Separate from R1.9 because the two directories are resolved by different code and an implementation can easily guard one and not the other — and the configuration directory is the one that also holds the SSH key store and `known_hosts.d/`. |
| V.2 | BLOCKED (#889) | The deprecation warning for a legacy input variable is emitted once per name per process start, not per read. | `core/internal/app`, the log-assertion test | A build warning on every read must fail. A per-read warning on `BACKUPD_DEBUG` in a hot path is a log flood, and a log flood is how a deprecation notice gets filtered out by the operator it was written for. |
| V.3 | BLOCKED (#888) | `legacyName` in `apps/generic/cmd/retnd-web/selfname_test.go` is `backupd`, and that test still fails on a binary that calls itself by the previous name. | `apps/generic/cmd/retnd-web/selfname_test.go` | A string literal `backupd-web usage:` planted in the web host's own output must turn the test red. The test exists because it caught this once already at `rbm`; moving the constant without re-proving it fires would leave the third rename with the guard the second one had to have restored by hand. |
| V.4 | BLOCKED (#890) | The `ghcr.io/backupdproject/backupd` mirror publishes for one release, so an unedited compose file keeps pulling. | `.github/workflows/release.yml`, the publish step's own assertions | A release run that pushes only the new package name must fail the publish gate. Paired with R1.5: the entrypoint alias and the image mirror are two independent halves of "an unedited compose file still works", and a build can satisfy either alone. |
| V.5 | BLOCKED (#890) | The adopted path is reported through the surfaces that report every other resolved path, so an operator can see which one is live without relying on a warning they scrolled past. | `retnd check`, the deployment-check route in `apps/common/webhost`, the startup log; cell `20-legacy-state-adoption` | A build that adopts the legacy path and reports the new one must turn the cell red. This is the row that stops adoption from being a silent success: a deployment can run for months on an adopted path, and "which directory is my journal in" is an incident question. |
