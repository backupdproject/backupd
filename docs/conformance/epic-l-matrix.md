# EPIC L conformance matrix — scripted backup workflows

What this file is for, and what it is not. It is not a summary of the
work: it is the list of claims EPIC L makes, each one tied to the test
that would go red if the claim stopped being true, plus the **mutation
that was actually run and watched to fail**. A cell says `PASS` only
after somebody broke the thing on purpose and saw the named test refuse
it. `BLOCKED` is for a claim whose code has not landed on this branch,
with the reason in the cell. Nothing here is green because nobody looked.

`docs/epic-checklist.md` section 1 calls this the ungated item that
matters most. It is gated here anyway:
`core/tests/workflowgate/matrix_test.go` reads this file and fails when a
consensus finding has no row, when a row names a test that does not exist
in the tree, when a row carries no falsification, or when a row is green
under any word other than `PASS`. That guard has its own mutation
self-test — `TestTheMatrixCheckerRefusesEveryWayAMatrixCanLie` — because a
guard whose only evidence is that it passes on the tree it shipped with
has proven nothing.

**How to read a proof reference.** `<package dir>:<TestName>` is a Go test
function that has to exist in that directory; a bare path is a file that
has to exist. The guard resolves every one of them, so a test renamed by
a later refactor turns this file red rather than leaving it citing
nothing.

**Where the numbers are.** `docs/perf/epic-l-workflows.md` records the
seven scale benchmarks, the host they were measured on and the exact
commands.

---

## Trust boundary

Stated here because a gate that did not say what it is defending would
invite the wrong conclusions from the rows below.

A person who can write into a hook directory has **code-execution
authority** on the machine that runs the hook. That is not a weakness of
the design, it is the feature: an operator asked for their script to run
around a backup. Everything in this matrix is therefore about containing
the *consequences* of that authority and about refusing anything that
grants it to somebody who did not already have it — a symlink into a tree
somebody else can write, a stage directory an unprivileged account can
replace, a transfer credential silently promoted to a shell.

Two limits follow, and both are load-bearing:

- **Redaction cannot protect a value an arbitrary script deliberately
  transforms.** The streaming filter removes a resolved secret from a
  hook's output, including across chunk boundaries. It does nothing about
  a script that base64s, hashes, reverses or prints one character per line
  of the same value, and it is not meant to: the script already has the
  value. Redaction defends against the ordinary accident — `set -x` around
  a `psql` invocation — not against the author of the script.
- **Termination certainty is a claim about what was observed**, never a
  promise about detached remote descendants. A hook that deliberately
  double-forks away from its process group on the far side of an SSH
  connection is outside the guarantee, and the product says so by
  recording `unconfirmed` rather than by claiming a clean stop.

---

## The adversarial-review consensus (#807)

Eleven findings, each a release requirement rather than optional
hardening. One row per finding, in the epic's own order.

| ID | Claim | Executable proof | Falsification (run, watched to fail) | Status |
|---|---|---|---|---|
| CR-01 | `.local.sh` runs through the out-of-container Host Workflow **runner**, and gaining a shell weakened no part of the canonical container contract | `core/internal/hostrunner:TestRefuseRoot_IsNotConfigurable` `core/internal/hostrunner:TestListen_PutsThePrivateSocketInAPrivateDirectory` `core/internal/hostrunner:TestExecute_RefusesWithoutAProvenCapabilityAndRunsNoHostBash` `distribution/compose:TestLocalHooksBoughtTheirShellWithoutWeakeningTheContainer` | Added `/var/run/docker.sock` as a volume to the canonical definition and made `/workflows` writable: the compose rules and the prohibition scan both refuse (`TestTheWorkflowMountRulesFireOnTheMistakesTheyExistFor`) | PASS |
| CR-02 | A hook runs only over an **exec-capable** connection; an SFTP-only or forced-command credential is refused for hooks and stays valid for backup | `core/internal/remoteexec:TestResolveRefusals` `core/internal/remoteexec:TestARunOnAForcedCommandAccountIsRefusedRatherThanReportedAsSuccess` `core/internal/remoteexec:TestAnExecCapableAccountIsNotRefusedForTheOptionsEveryBashHas` | The fixture server accepts every exec request and runs its own program: the step is refused as `ErrExecCapability` and the hook's own token never appears in a command the server saw | PASS |
| CR-03 | A crash after a before step leaves a durable cleanup obligation, a blocked set and `recovery_required`, and `--skip-workflow-scripts` cannot clear it | `core/internal/workflowrun:TestACrashAtEveryDurableTransitionReconcilesToRecoveryRequired` `core/internal/workflowrun:TestSkippingWorkflowScriptsCannotClearRecoveryRequired` `core/internal/workflowrun:TestAnInterruptedRunBlocksTheSetAndResumesFromItsCapturedBytes` | Crashed at every durable transition in turn and reconciled from the rows alone; a bypassed run over a set in recovery is refused rather than taken | PASS |
| CR-04 | Fixed `bash --noprofile --norc`, the four startup variables sanitized, and no `set -e`, `-u`, `-x` or **pipefail** this product injected | `core/internal/workflowexec:TestAHookUnderARealBashSeesNoShellOptionThisProductNeverAskedForAndAClosedStdin` `core/internal/hostrunner:TestExecute_InjectsNoShellOptions` `core/internal/hostrunner:TestProcessEnv_DeletesTheFourVariablesThatMakeBashRunSomethingElse` `core/internal/remoteexec:TestAnAccountWhoseShellStartupChangedWhatAHookMeansIsRefused` | Injected `set -euo pipefail` at the head of the envelope prologue: the real-bash test goes red on `$-`, on the `set -o` table and on all three consequences (a survived `false`, an unset reference, a failing left-hand pipe) | PASS |
| CR-05 | Timeout and cancel record termination certainty — `confirmed` or **unconfirmed** — rather than claiming a stop nobody saw | `core/internal/remoteexec:TestAStepWhoseSessionNeverCompletesRecordsTerminationAsUnconfirmed` `core/internal/remoteexec:TestAStepThatEndsWhileTerminationWaitsIsRecordedAsConfirmed` `core/internal/hostrunner:TestExecute_KeepsTheWorkingDirectoryWhenTerminationCannotBeProved` `core/internal/hostrunner:TestServer_LeaseExpiryKillsARunawayHook` | A fixture that holds the channel open past the bound and ignores the signal request: certainty stays `unconfirmed`, no exit code is invented, and the confirmed counterpart proves the field is not a constant | PASS |
| CR-06 | A live follower never applies **backpressure** to a script: persist through the engine first, fan out to bounded queues, replay by cursor | `core/internal/workflowrun:TestAStalledFollowerDoesNotSlowTheHookAndCatchesUpByCursor` `core/internal/workflowrun:TestADroppedFollowerReceivesNothingMoreSoItsCursorHasNoHole` `core/internal/workflowrun:BenchmarkASlowFollowerWhileAScriptEmits` | A follower with a queue of two that reads nothing: the hook's wall clock is unchanged against a no-follower control, the follower reports having lagged, and the catch-up read from the journal is gapless | PASS |
| CR-07 | Each step owns one **logical** log stream, bounded per step, addressed by one run-monotonic cursor (the lazy mounting of the viewer is L8 #815's row in its own matrix, not this gate's) | `core/internal/workflowrun:TestTheOutputBoundIsPerStep` `core/internal/workflowrun:TestStepOutputIsCapturedInOrderWithItsStreamAndTime` | One step flooding past the bound does not consume another step's budget; the bound is per step and the cursor stays run-monotonic | PASS |
| CR-08 | Script output is untrusted terminal input, and at the log/API data layer a hostile **OSC**, title or window-control sequence has no effect beyond being bytes | `core/internal/workflowrun:TestTheWireFormOfAHostileRecordCarriesNoRawControlByteAndStillDecodesExactly` `core/internal/workflowrun:TestNoStructuralFieldOfARecordPicksUpAByteOfAHostileTerminalSequence` `core/internal/workflowrun:TestAForgedTruncationMarkerIsDistinguishableFromTheGenuineOneOnlyByKind` | Marshalled the payload as a string rather than as bytes: the wire check goes red on both C1 introducer entries. Classified the record kind by payload text: the forged-marker test goes red | PASS |
| CR-09 | stdout and stderr keep their identity and share one capture **sequence** counter, and the merged view is labelled best-effort capture order | `core/internal/workflowexec:TestCaptureKeepsTheStreamsApartAndOrdersThemTogether` `core/internal/hostrunner:TestExecute_StdoutAndStderrStayApartAndShareOneCounter` `core/internal/workflowrun:TestRecordsReachTheJournalInSequenceOrder` | Concurrent writers on both streams issue every sequence exactly once; a record committed out of order breaks the follower's cursor claim and is refused | PASS |
| CR-10 | Conservative ASCII **basename**s only, regular files only, symlinks refused, bytewise `LC_ALL=C` order | `core/internal/workflow:TestParseScriptNameRefusesEveryUnsafeName` `core/internal/workflow:TestDiscoverRefusesASymlinkedScript` `core/internal/workflow:TestDiscoverRefusesAnEntryThatIsNotAScript` `core/internal/workflow:TestScriptOrderIsBytewiseOverTheWholeBasename` | A table of unsafe names — separators, `..`, a leading dot, a control character, a space, non-ASCII, a bare `.sh` — each refused by name, and a symlinked script refused even when its target is valid | PASS |
| CR-11 | Immutable run-scoped script copies in the protected **spool**, executed from the captured bytes until the run is terminal | `core/internal/workflow:TestSnapshotIsImmuneToAPostSnapshotReplacement` `core/internal/workflow:TestSnapshotProtectsTheSpool` `core/internal/workflowrun:TestATamperedSpoolIsNotExecutedByAResume` | Replaced the script on disk after the snapshot and tampered with the spooled copy before a resume: the run executes the captured bytes, and the tampered spool is refused instead of run | PASS |

---

## The gate classes (#812)

| ID | Claim | Executable proof | Falsification (run, watched to fail) | Status |
|---|---|---|---|---|
| GC-01 | The host runner refuses to run as root, listens on a Unix socket only, keeps it `0600` in a `0700` directory, and authenticates every connection | `core/internal/hostrunner:TestRefuseRoot_IsNotConfigurable` `core/internal/hostrunner:TestListen_PutsThePrivateSocketInAPrivateDirectory` `core/internal/hostrunner:TestServer_RefusesAClientWithoutTheInstallationCredential` `core/internal/hostrunner:TestLoadToken_RefusesACredentialOtherAccountsCanRead` | A credential file other accounts can read is refused; a client with no credential is refused before it can name an operation | PASS |
| GC-02 | An engine and a runner from different releases refuse each other, and the credential is checked before the version | `core/internal/hostrunner:TestServer_RefusesAnEngineFromADifferentRelease` `core/internal/hostrunner:TestServer_AuthenticatesBeforeItComparesVersions` | A handshake carrying another release's version is refused naming both versions; an unauthenticated client learns nothing about the runner's version | PASS |
| GC-03 | The wire carries bytes and never a path, and a request naming one is refused | `core/internal/hostrunner:TestReadMessage_RefusesAFrameThatNamesAPath` `core/internal/hostrunner:TestServer_RefusesARequestThatNamesAPath` `core/internal/hostrunner:TestExecute_RefusesAnIdThatWouldEscapeTheRuntimeDirectory` | A frame carrying `script_path` is refused by the decoder; an id that would escape the runtime directory is refused rather than joined | PASS |
| GC-04 | Path, symlink, permission and TOCTOU refusals on the whole discovery and capture path | `core/internal/workflow:TestResolveStageRefusesEveryEscapeAndUnsafeShape` `core/internal/workflow:TestResolveStageRefusesAWritableAncestor` `core/internal/workflow:TestDiscoveryRefusesAHookDirectoryOwnedBySomebodyElse` `core/internal/workflow:TestOpenScriptVerifiesWhatItOpens` `core/internal/hostrunner:TestExecute_FollowsNoSymbolicLinkUnderTheWorkspace` | Replaced a script with a symlink between the stat and the open, and made an ancestor group-writable: both refused, the second one walking to the filesystem root with no sticky-bit exception | PASS |
| GC-05 | A hook cannot rewrite the bytes bash is reading, and the runner re-verifies size and digest before it runs anything | `core/internal/hostrunner:TestExecute_RefusesBytesWhoseHashDoesNotMatchTheClaim` `core/internal/hostrunner:TestExecute_RefusesBytesWhoseSizeDoesNotMatchTheClaim` `core/internal/hostrunner:TestExecute_TheHookCannotRewriteTheScriptBashIsReading` | A request whose claimed digest and size disagree with its bytes is refused; the script file is mode `0500` and a hook's attempt to rewrite it fails | PASS |
| GC-06 | Local ENV injection is inert: values travel in the client's own environment block as `--env NAME`, never as argv, and arrive byte-identical | `core/internal/hostrunner:TestHookArgs_CarriesEnvironmentNamesAndNeverValues` `core/internal/hostrunner:TestExecute_DeliversValuesToTheHookByteForByte` `core/internal/hostrunner:TestProcessEnv_RefusesANulRatherThanTruncatingASecret` | Searched the whole launch vector for every hostile value and found none of them; a NUL is refused rather than silently truncating a credential | PASS |
| GC-07 | Remote ENV injection is inert: every value is a single-quoted shell literal that a real bash delivers byte-identically and never executes | `core/internal/workflowexec:TestStdinPayloadNeverLetsAValueBecomeSyntax` `core/internal/workflowexec:TestAHookUnderARealBashReceivesHostileValuesByteIdenticalAndNeverRunsThem` `core/internal/workflowexec:TestShellQuoteRoundTripsEveryByteButNUL` | Eleven hostile values — `$(...)`, backticks, embedded quotes, newlines, a whole shell command — each aimed at a canary file under the test's own directory: every value arrives byte for byte and no canary exists, while a deliberately unquoted control assignment does create its own canary | PASS |
| GC-08 | Streaming secret redaction holds at every chunk boundary, through both capture implementations, and across the truncation bound | `core/internal/workflowrun:TestASecretIsUnreachableAtEveryChunkBoundaryOnBothCapturePaths` `core/internal/workflowrun:TestASecretSplitAcrossChunksNeverReachesTheJournal` `core/internal/workflowrun:TestNoRedactedValueIsAnywhereInTheJournalFile` `core/internal/workflowrun:TestASecretStraddlingTheBoundIsStillNeverPersisted` | Pointed the secret reference at a different value so the filter's needle no longer matched: the boundary test reports the surviving fragment and goes red at the first window | PASS |
| GC-09 | A hook's process tree dies with its container or its remote process group, and a lost engine lease kills a runaway | `core/internal/hostrunner:TestExecute_TimeoutStopsTheContainerAndEverythingInIt` `core/internal/hostrunner:TestExecute_EscalatesToAKillWhenTheHookIgnoresTheStop` `core/internal/hostrunner:TestServer_LeaseExpiryKillsARunawayHook` `core/internal/hostrunner:TestServe_ACancelledContextStopsTheRunnerAndTheHooksItOwns` | A hook that ignores `SIGTERM` is escalated to a kill and the container's absence is then proved by label rather than assumed; dropping the engine's connection stops and removes the container | PASS |
| GC-10 | A crash leaves `recovery_required`, blocks the set, and a bypass cannot clear it; resume executes the captured bytes with `BACKUPD_RECOVERY=1` | `core/internal/workflowrun:TestSkippingWorkflowScriptsCannotClearRecoveryRequired` `core/internal/workflowrun:TestResumeLeavesAnUnaccountableScopeBlockedUntilAcknowledged` `core/internal/workflowrun:TestTheRunsFactsReachItsHooksOnBothTheRunAndTheResumePath` | A bypassed run against a set in recovery is refused; an unacknowledged scope stays blocked; the resumed cleanup runs the spooled bytes rather than whatever is in `/workflows` now | PASS |
| GC-11 | The log flood is bounded per step and across a whole run, with the truncation marker in sequence position | `core/internal/workflowrun:TestPersistedOutputIsBoundedWithATruncationMarker` `core/internal/workflowrun:TestTheBoundCoversBothStreamsAndTheFlushAfterIt` `core/internal/workflowrun:TestALogFloodIsBoundedAcrossAWholeRun` | Raised the per-step bound to four times its value: the whole-run test reports 16384 bytes persisted against a 4096-byte bound and goes red | PASS |
| GC-12 | The engine container contract still holds with local hooks enabled: two mounts, one read-only, and no capability, privilege, device or Docker socket gained | `distribution/compose:TestTheHookScriptTreeIsMountedReadOnly` `distribution/compose:TestTheRunnerDoorIsWritableAndIsNotTheDaemonSocketsDirectory` `distribution/compose:TestNoWorkflowMountReachesTheRunnersWorkspaceOrIsWritable` `distribution/compose:TestAReadOnlyCredentialFileMountIsNotRefused` `distribution/compose:TestLocalHooksBoughtTheirShellWithoutWeakeningTheContainer` `distribution/compose:TestTheWorkflowMountRulesFireOnTheMistakesTheyExistFor` | Five injected mutations, each caught by its own rule: a writable `/workflows`, a writable workflow directory beside it, a runtime directory pointed at `/run`, the runner's workspace mounted into the engine, and the Docker socket handed to the engine — while a read-only single-file credential mount (the SSH key's shape, and the runner token's) is deliberately NOT refused, so the rule is a privilege invariant rather than a frozen mount list | PASS |
| GC-13 | Every container this runner starts is distroless of privilege at the argv level — `--cap-drop ALL`, `--security-opt no-new-privileges`, `--read-only`, non-root `--user`, `--network none`, `--pids-limit`, no Docker socket — on the hook, probe and syntax vectors alike | `core/internal/hostrunner:TestEveryLaunchVectorCarriesTheHardeningTheHookVectorIsTestedFor` `core/internal/hostrunner:TestEveryLaunchVectorRefusesTheFlagsThatWouldUndoTheHardening` `core/internal/hostrunner:TestEveryLaunchVectorIsTheSubcommandItsTerminationStoryNeeds` `core/internal/hostrunner:TestTheStartVectorCarriesNoHardeningBecauseTheCreateAlreadyFixedIt` | Deleted `--read-only` from the syntax vector and injected `--privileged`, `--volume /var/run/docker.sock`, `--network host`, `--user 0:0`, `--rm` on the hook create and `--cap-drop` on the start vector: each reddened its own row | PASS |
| GC-14 | Those flags do what they claim against a real Docker daemon, measured from inside a hook container | `core/tests/containerhooks:TestTheContainmentIsWhatItClaims` | The machine-tier suite fails rather than skipping on a host with no usable daemon, so a deployment shape where the containment does not hold cannot report `ok` | PASS |
| GC-15 | The capability is proven before anything is served, and there is no host-bash fallback | `core/internal/hostrunner:TestExecute_RefusesWithoutAProvenCapabilityAndRunsNoHostBash` `core/internal/hostrunner:TestProveContainerCapability_RefusesAProbeThatDoesNotEmitOurMarker` `core/internal/hostrunner:TestProveContainerCapability_RefusesAHookThatWouldRunAsRootInside` `core/internal/hostrunner:TestProveContainerCapability_RefusesANetworkThatUndoesTheIsolation` | A probe that exits 0 without printing this repository's marker is refused; `--hook-network host` and `container:<id>` are refused; a root hook user is refused with the uid read as a number | PASS |
| GC-16 | The seven scale scenarios are benchmarks in the tree, and the no-hooks path does no journal work at all | `core/internal/workflowrun:BenchmarkAPlanWithNoWorkflowConfiguredAtAll` `core/internal/workflowrun:BenchmarkAPlanWhoseStageDirectoriesAreEmpty` `core/internal/workflowrun:BenchmarkAHundredTinyLocalHooks` `core/internal/workflowrun:BenchmarkAHundredRemoteHooks` `core/internal/workflowrun:BenchmarkAHighOutputScript` `core/internal/workflowrun:BenchmarkLogsAfterOverAHundredStepHistory` `core/internal/workflowrun:BenchmarkASlowFollowerWhileAScriptEmits` `core/internal/workflowrun:TestTheNoHookPathDoesNoJournalWorkAtAll` `core/internal/workflowrun:TestTheZeroHookPathRecordsNothing` `docs/perf/epic-l-workflows.md` | Added one `WorkflowRunsInStates` call to the no-hook path: the counting store reports one journal call where zero are allowed and the test goes red | PASS |
| GC-17 | The workflow config surface has compat fixtures for the global-plus-per-set pair and for what it refuses | `core/tests/compat:TestMediumFreeSurfacesAreUnchanged` `core/tests/compat/testdata/configs/05-workflow-global-and-per-set.yaml` `core/tests/compat/testdata/configs/06-workflow-configured-missing-dir.yaml` `core/tests/compat/testdata/configs/55-invalid-workflow-relative-root.yaml` `core/tests/compat/testdata/configs/56-invalid-workflow-stage-dir-escapes.yaml` `core/tests/compat/testdata/configs/57-invalid-workflow-exec-connection-without-root.yaml` | Each invalid fixture is recorded in the corpus as a refusal naming the config key at fault; the configured-but-missing directory validates on purpose, because refusing it would stop a daemon whose `/workflows` mount is merely late | PASS |
| GC-18 | This gate is a CI job and the release gate covers it | `.github/workflows/ci.yml` `scripts/tests/release-gate-covers-every-job.test.sh` | The release-gate guard fails when a job in the workflow is absent from the gate's `needs` list, which is how a new job silently stops being covered | PASS |
| GC-19 | This matrix cannot claim a test that does not exist, and cannot quietly drop a consensus finding | `core/tests/workflowgate:TestEveryConsensusFindingMapsToATestThatExists` `core/tests/workflowgate:TestTheMatrixCheckerRefusesEveryWayAMatrixCanLie` `core/tests/workflowgate:TestTheMatrixParserReadsTheRealTablesRatherThanTheirShape` | Seven mutations of an honest matrix — a missing finding, a row numbered as something it is not about, a row with no test, a row citing a test nobody wrote, a `PASS` with no falsification, a row green under another word, and the same finding answered twice — each refused for its own reason, with the honest version still accepted | PASS |
| GC-20 | No resolved secret reaches a metric label, an API response or CLI output | `core/internal/metrics:TestRender_NoLabelCarriesASecretOrAPath` `apps/common/webhost:TestNoWorkflowResponseCarriesAResolvedSecret` `core/cmd/retnd:TestWorkflowEnvPrintsTheLocationOfASecretAndNeverItsValue` `core/internal/workflow:TestNoResolvedSecretReachesThePlanTheHashOrTheSpool` | Injected a resolved secret value into a metric label and into a workflow API and CLI response field: each secret-absence test fails, and L6 keeps that mutation permanently as its own `_WouldCatchOne` control so the absence can never pass vacuously. The plan-and-spool half is the same claim one layer down, and no resolved value is persisted there either | PASS |
| GC-21 | Only an administrator with configuration authority may bypass workflow scripts, the authorization is rechecked on every replay of a step-log stream, and every such mutation is audited without recording a credential | `core/service:TestBypassIsRefusedWithoutAnAdministrator` `core/service:TestBypassIsRefusedOnAScheduledRunEvenForAnAdministrator` `core/service:TestAnAdministratorsBypassIsAllowed` `apps/common/webhost:TestWorkflowStepLogs_AuthorizationIsRecheckedOnEveryCursorResume` `core/internal/remoteexec:TestAuditCarriesEveryFactAndNoCredential` | Dropped the administrator gate on `skip_workflow_scripts`, dropped its audit record, and skipped the per-replay authorization recheck on the step-log stream: each one fails its own test. A scheduled run stays refused even for an administrator, which is the case an authorization check keyed only on the caller would let through | PASS |
| GC-22 | Every provider states its local-hook answer where its own operator will read it, and the engine gains no Docker access in any packaging metadata | — | The provider capability and the packaging manifests are #877's surface and do not exist on this branch. When they land the proofs are `distribution/packaging:TestEveryProviderStatesItsLocalHookAnswerWhereItsOperatorWillRead`, `:TestTheCapabilityContractAndTheMatrixAgreeOnLocalHooks` and `:TestTheEngineGainsNothingAndTwoRulesSaySo`, against conformance capability id `local-workflow-hooks` (PASS on generic, openmediavault, proxmox; UNSUP on synology, truenas, unraid, ugos; N/A on the four that ship no engine container). The argv and compose halves of the same claim are GC-12 and GC-13 above, which are this branch's and are PASS | BLOCKED |

---

## What this gate does not prove

Stated plainly, because a matrix of green cells invites the reader to
assume the rest.

- **Browser-level behaviour.** Whether the terminal viewer mounts lazily,
  disables stdin and refuses to activate a hyperlink is L8 (#815) and
  L9 (#816). CR-07 and CR-08 above are the data-layer halves only, and say
  so.
- **A real NAS.** The container-containment claims are measured against a
  real Docker daemon on a developer machine and in CI
  (`core/tests/containerhooks`), not on UGOS, Synology DSM or TrueNAS
  hardware. `docs/site/index.html#honest` is where that stays honest.
- **A real sshd for every claim.** `core/internal/remoteexec`'s fixture
  server is built to have the three behaviours a real server reaches only
  by failing. What a real OpenSSH does with a forced command, with
  `internal-sftp` and with a process group is
  `core/tests/sshexecintegration`'s question.
- **Performance on the benchmark host.** The seven benchmarks run
  anywhere; the recorded numbers are from one machine, named in
  `docs/perf/epic-l-workflows.md`. They are a baseline to compare against,
  not a threshold this gate enforces.
