# EPIC L: Scripted Backup Workflows, Five Stages Around Every Backup

## Status

**Type:** EPIC / Detailed implementation specification
**Repository:** `backupdproject/backupd`
**Parent / predecessor EPICs:** EPIC A (#1, backup engine), EPIC B (#81, provider-neutral core and multi-NAS apps), EPIC E (#232, storage mediums per retention tier)
**Primary implementation root:** `core/`
**Tracker issue:** #807 (sub-issues #808 through #817, plus #906 inserted as L7.5)
**FR numbering:** this specification continues the product's FR series at **FR-36**. FR-1 through FR-24 are defined in `docs/EPIC.md`; FR-26 is claimed by the `version` command in `core/cmd/retnd` and `core/internal/app`; FR-27 through FR-35 are EPIC E's, in `docs/EPIC-E-alternative-storage.md`. FR-25 is an unclaimed hole and stays one. Nothing here renumbers an existing FR.

This document was written at the end of the epic rather than at its start, which is a defect in the process and not a licence to write it as a success story. Issue #807 carried the specification while the work was done; `docs/epic-checklist.md` section 1 asks for a file, and #817's scope audit asked for this one. Every claim below is written against the code on this branch, and section 8 is the part that matters: it says what was proved, by what, and what was not.

---

# Adversarial Review, Five-Expert Panel

Same discipline as `docs/EPIC-E-alternative-storage.md` and `docs/EPIC-B-multi-nas.md`: each reviewer was instructed to reject this EPIC if the design could create data loss, a credential leak, misleading backup UX, an untestable guard, or a compatibility break for an existing deployment. The five lenses were chosen for what this feature actually is — a workflow engine, a remote-execution channel, a Bash host, a terminal, and a thing wrapped around a backup — and the panel produced eleven consensus findings that were treated as release requirements rather than as optional hardening.

The eleven findings are tracked, one row each, as `CR-01` through `CR-11` in `docs/conformance/epic-l-matrix.md`, every one with the mutation that was run and watched to turn it red. This section is the reasoning; that file is the evidence. Where the two could drift, the matrix is right, because a test reads it.

## Expert 1, Workflow Engine Semantics

### Initial verdict: REJECT

Critical findings:

1. "Global" is ambiguous in a way that decides whether a database gets unquiesced. Read as "once per batch" it means a `--all` pass runs one global-before for many sets; read as "inherited by every run" it means one per set. The two produce different cleanup obligations from the same directory.
2. A stage that is *configured and empty* and a stage that is *not configured* are different facts, and a design that collapses them cannot tell an operator "the global after stage did not run" apart from "this deployment has no global after stage" — which is the sentence that matters when a machine is still quiesced.
3. Nothing in the draft said what happens to the backup-set stages when a global-before step fails, which is the failure matrix the whole feature is.
4. Borrowing a CI engine's vocabulary (`allow_failure`, conditional expressions, per-step images, DAGs) would import a semantics nobody can reason about around a backup, and would be impossible to remove later.

Required corrections, all adopted:

- "Global" means *inherited by every individual backup-set run*, never "once per batch", and global hooks receive the same merged environment as the set they wrap (FR-36);
- entering a scope is its own durable write made before the first side-effecting command in that scope, so a configured-but-empty before directory still enters its scope and its after stage still runs (FR-36, FR-42);
- the failure matrix is written out and is the shape of the executor: a global-before failure leaves global-after eligible and backup-set-after never eligible; a backup-set-before failure leaves both after stages eligible (FR-36, FR-41);
- no DAGs, no parallel steps, no per-step image, no conditional expressions, no `allow_failure`, and this is recorded as a non-goal rather than as a deferral (section 6).

### Consensus position: APPROVE AFTER REVISION

## Expert 2, OpenSSH Remote-Execution Security

### Initial verdict: REJECT

Critical findings:

1. `.remote.sh` reusing the backup set's own SFTP credential assumes that credential can run a command. `docs/ssh-setup.md` spends four sections teaching operators to make exactly that impossible — a chroot and a forced `internal-sftp`. The draft would have run hooks over a credential the product's own documentation tells you to cripple.
2. A `command=` in `authorized_keys` or a `ForceCommand` in `sshd_config` does not fail: it runs *something else* and exits zero. Silent success on the wrong program is worse than a refusal.
3. Uploading a script to the remote host to run it leaves a file behind on a machine the operator did not agree to have written to, and leaves it behind precisely when the connection drops.
4. Passing an environment value on a remote command line publishes it to every process table on that host.

Required corrections, all adopted:

- exec capability is *proven*, not assumed: a hook connection is probed and an SFTP-only or forced-command account is refused with a reason naming the missing capability, while artifact backup over that same credential keeps working unchanged (FR-40);
- a separate execution connection is declarable (`workflows.exec_connections`, `remote_exec_connection_ref`), so the transfer account stays crippled on purpose (FR-40);
- the script is streamed over the channel's stdin: nothing is uploaded, no file is written, nothing is left behind if the connection drops (FR-40);
- no environment value or secret reaches a remote command line; values are injected as single-quoted shell literals (FR-38, FR-40).

### Consensus position: APPROVE AFTER REVISION

## Expert 3, Bash and Shell Correctness

### Initial verdict: REJECT

Critical findings:

1. Bash startup files make a hook's behaviour a property of the account's home directory. The same script runs differently for two operators, and the difference is invisible in the script.
2. A session that arrives carrying `BASH_ENV` or `ENV` runs a file of somebody else's choosing before the first line of the hook.
3. Injecting `set -euo pipefail` "to be helpful" silently changes the meaning of every script an operator already tested by hand. Not injecting it leaves real footguns — an unchecked `cd`, a masked pipeline failure — with nothing to catch them.
4. `bash -n` proves a script parses. It proves nothing about an `rm -rf /$EMPTY`.

Required corrections, all adopted:

- fixed `bash --noprofile --norc`, and the four startup variables are sanitized; a session arriving with `BASH_ENV` or `ENV` set is refused rather than run (FR-45);
- nothing is injected: no `set -e`, no `-u`, no `pipefail`, no `xtrace`. A hook runs exactly as written, and the documentation says why (FR-45);
- the footguns are answered by *verification* rather than by injection: a real Bash parser (`mvdan.cc/sh`, linked into the product) plus this product's own small rule set, reported with line, column and an excerpt (FR-46);
- one rule blocks a save and the rest advise, because a linter that refuses everything gets turned off (FR-46).

### Consensus position: APPROVE AFTER REVISION

## Expert 4, Terminal Security and Scalability

### Initial verdict: REJECT

Critical findings:

1. A hook's stdout is attacker-controlled input to a terminal emulator. OSC 8 hyperlinks, title and window controls, and clipboard sequences are all side effects available to a script an operator dropped into a directory.
2. "One terminal per script" read literally is a hundred mounted emulators on a hundred-step run, in a browser.
3. A live follower that is slow — a laptop on hotel wifi — must not be able to slow a database quiesce down. Backpressure from a viewer to a hook is a correctness bug, not a performance one.
4. stdout and stderr are two pipes. Any claim of a total order across them is a claim the kernel does not make.

Required corrections, all adopted:

- a read-only terminal profile: stdin disabled, no link activation, no title or window side effects, no output-driven actions, bounded scrollback, and nothing rendered through `innerHTML` (FR-44);
- each step owns a *logical* log stream; the UI lazily mounts only the selected viewer (FR-44);
- output is persisted through the engine first and then fanned out asynchronously with bounded queues; a follower that stops reading is dropped rather than waited for, and catches up from the journal by cursor (FR-44);
- stream identity is preserved, capture sequence numbers come from one run-monotonic counter, and the merged view is labelled best-effort capture order (FR-44).

### Consensus position: APPROVE AFTER REVISION

## Expert 5, Backup and Disaster-Recovery Correctness

### Initial verdict: REJECT

Critical findings:

1. A crash after a before step leaves a database quiesced with nothing scheduled to unquiesce it. That is the feature's worst outcome and the draft had no state for it.
2. Reopening `/workflows` after a crash cannot guarantee the same cleanup code. The operator may have edited the directory; the file may be gone.
3. Folding backup and cleanup into one Boolean makes "the backup is good and the machine is still quiesced" unrepresentable, which is the exact state an operator has to be told about.
4. Any bypass flag becomes the thing an operator reaches for at 3am, and a bypass that can clear a cleanup obligation is a data-integrity hole with a command-line switch.

Required corrections, all adopted:

- a durable cleanup-obligation journal, a `recovery_required` run state, the backup set blocked (scheduled runs suspended, manual runs refused, a health warning raised), and nothing replayed automatically (FR-42);
- immutable run-scoped script copies in the protected state spool, retained until the run is terminal and any recovery is resolved; a resume executes *those* bytes, re-verified against the digest recorded at plan time (FR-42);
- three statuses recorded separately and never derived from one another (FR-41);
- the bypass cannot clear a recovery hold, and that is structural rather than a check: the advance path refuses to settle the recovery axis at all (FR-42, FR-47).

### Consensus position: APPROVE AFTER REVISION

## Five-Expert Consensus

> **A hook is a small linear thing wrapped around a backup, and every ambiguity in it is answered by a durable fact rather than by a default. Local hooks buy their shell from a host runner outside the container and the canonical container contract does not move; remote hooks require a proven exec capability and leave nothing on the far host; Bash runs exactly as written and is verified rather than corrected; a script's output is untrusted input to a read-only terminal that no follower can backpressure; the backup, the workflow and the cleanup are three answers and never one; and a crash leaves a blocked backup set with a recorded obligation and exactly two ways out, neither of which is a flag.**

---

# 1. Purpose

An operator places Bash scripts in configured directories and this product runs them as ordered, individually observable steps around every backup-set run. That is the whole feature.

It exists because the useful backup of a live system is taken with the system in a state somebody has to put it in: a database quiesced, a snapshot taken, a cache flushed, an application stopped. Every deployment already does this, with cron and a wrapper script, and the wrapper script is where the failures live — it has no record of what it ran, no way to report which step failed, no ordering guarantee, no way to tell you that a backup succeeded while the unquiesce did not, and no answer at all to the case where the machine running it died between the two.

It is deliberately **not** a CI engine. There are no DAGs, no parallel steps, no per-step images, no plugins, no YAML pipelines, no interactive shell and no browser script editor. The whole of the workflow is five stages in one order:

```text
GLOBAL BEFORE
      |
BACKUP-SET BEFORE
      |
BACKUP OPERATION      (the existing run, unchanged)
      |
BACKUP-SET AFTER
      |
GLOBAL AFTER
```

"Global" means inherited by every individual backup-set run, not once per batch.

# 2. Where every piece lives

| Piece | Location | Layer |
|---|---|---|
| Domain vocabulary, states, transitions | `core/internal/workflow/states.go` | domain |
| Script naming, target suffix, ordering | `core/internal/workflow/script.go` | domain |
| Root canonicalisation, stage resolution, custody | `core/internal/workflow/discover.go` | domain |
| Environment merge, built-ins, reserved namespace | `core/internal/workflow/env.go` | domain |
| Plan, timeouts, size bounds, spool capture | `core/internal/workflow/{plan.go,spool.go}` | domain |
| Cleanup obligations | `core/internal/workflow/obligation.go` | domain |
| Per-step log records and truncation marker | `core/internal/workflow/steplog.go` | domain |
| Shell verification rules `BSH001`–`BSH006` | `core/internal/workflowlint/{lint.go,rules.go,excerpt.go}` | domain |
| Five-stage executor, reconcile, resume, locks | `core/internal/workflowrun/{engine.go,lock.go,adapter.go,doc.go}` | application |
| Host Workflow Runner | `core/internal/hostrunner/{exec.go,container.go,protocol.go}` | application |
| Remote exec adapter | `core/internal/remoteexec` | infrastructure |
| Durable rows and migration | `core/internal/state`, `migrations/0012_workflow_runs.sql` | infrastructure |
| Configuration schema | `core/internal/config/workflows.go` | infrastructure |
| Read and validation API | `core/service/{workflowinspect.go,workflowvalidate.go,workflowverify.go}` | application |
| CLI | `core/cmd/retnd/{workflow.go,workflowrunner.go,settingsworkflow.go,backupsetworkflow.go,validateworkflow.go}` | presentation |
| HTTP | `apps/common/webhost/{handlers_workflowconfig.go,handlers_workflowruns.go,router.go}` | presentation |
| Web UI | `ui/shared/src` (`WorkflowRunPage`, `WorkflowStatusSplit`, `WorkflowRecoveryBanner`, `BackupSetWorkflowCard`, `WorkflowSettingsCard`) | presentation |
| Operator prose | `docs/install.md`, `docs/runtime-contract.md`, `docs/ssh-setup.md`, `docs/recovery.md`, `docs/recovery-without-a-terminal.md`, `docs/site/workflows.html` | docs |
| Decisions | `docs/adr/0019`–`0022` | docs |
| Evidence | `docs/conformance/epic-l-matrix.md`, `docs/perf/epic-l-workflows.md` | docs |

# 3. Functional Requirements

## FR-36, Five Stages, One Order, Two Scopes

Every backup-set execution is one workflow run with five stages: global-before, backup-set-before, the backup, backup-set-after, global-after. Steps run serially in plan order; the after stages unwind in the reverse of the order the before stages were entered.

There are two scopes, `global` and `set`, and two phases, `before` and `after`; a stage is the pair, and the backup itself is not a hook step and has neither. Entering a scope is its own durable write, made before the first side-effecting command in that scope. Global is entered when the run begins; the backup-set scope is entered only once global-before has succeeded. Therefore: a global-before failure makes global-after eligible and backup-set-after never eligible, and a backup-set-before failure makes both after stages eligible. A configured but empty before directory still enters its scope, so its after stage still runs.

An unset stage directory means the stage is disabled. A configured directory that does not exist is a refusal, not a stage that silently runs nothing.

## FR-37, Discovery, Naming and Deterministic Ordering

Hook scripts live under one approved absolute directory, `workflows.root`. A stage directory is a name inside it. The root may itself be a symlink — a mount point an operator declared — and a stage directory may not, because the permissions protecting a link say nothing about the ones protecting its target.

A script is named `^[0-9A-Za-z][0-9A-Za-z._-]*\.(local|remote)\.sh$`. Regular files only; symlinks are refused; a name carrying whitespace, a control character, invalid UTF-8, a NUL or a non-ASCII character that cannot be told from an ASCII one by eye is refused with its own sentence. A plain `*.sh` with no target suffix is refused rather than guessed, because a hook that quiesces a database has to run on the machine holding the database and guessing wrong is silent.

Ordering within a stage is bytewise over the whole basename with the target suffix included — exactly `LC_ALL=C ls`, which is the one ordering an operator can reproduce without running this product. Not locale-aware, not "local first", not "by name with the target as a tiebreak".

A containing directory that any other account may write is a refusal (custody). Unlike a secret reference, a sticky directory earns no exemption: a `1777` hook directory is a root shell for any local account. A script is read once through a custody-checked descriptor and the discovered path is not exposed, so nothing can re-open it after the snapshot.

## FR-38, The Environment and Secret Model

The environment a hook receives is built, never inherited. A hook gets none of the daemon's environment, because that block can hold the repository passphrase, and handing it to a script an operator dropped into a directory would make every hook a credential dump and would do it silently.

Precedence is `sanitized baseline < workflows.environment < backup-set environment < RETND_* built-ins`. The first three are ordinary specificity. The fourth is different in kind: the built-ins are the run's own facts, so the whole `RETND_` prefix is refused *in configuration, at validation time* rather than overridden at merge time — a key an operator can write and this product silently discards is a key that looks like it works.

Nineteen built-ins are supplied: `RETND`, `RETND_RUN_ID`, `RETND_BACKUP_SET_ID`, `RETND_BACKUP_SET_NAME`, `RETND_PHASE`, `RETND_STEP_ID`, `RETND_STEP_NAME`, `RETND_STEP_TARGET`, `RETND_SOURCE_HOST`, `RETND_SOURCE_PATH`, `RETND_DESTINATION`, `RETND_WORK_DIR`, `RETND_BACKUP_STATUS`, `RETND_WORKFLOW_STATUS`, `RETND_CLEANUP_STATUS`, `RETND_BACKUP_ERROR_CODE`, `RETND_STARTED_AT`, `RETND_RECOVERY`, `RETND_CLEANUP_REASON`. A before hook is told `RETND_BACKUP_STATUS=unknown` — a real value, because an unset variable and one saying "nobody knows yet" read identically in `test -z`.

Every one of those nineteen is exported a second time under the `BACKUPD_` name it had before the rename to `retnd` — `RETND_BACKUP_STATUS` beside `RETND_BACKUP_STATUS`, and the bare `BACKUPD` beside `RETND` — with identical values, for one release (EPIC R, #885, FR-37). A hook is an operator's own Bash script, `$RETND_BACKUP_STATUS` against a build that stopped exporting it is the empty string rather than an error, and a hook that notifies on failure would simply stop notifying. Both prefixes are reserved in configuration for as long as both are exported. The deprecated block is removed in the release after the one that renames this product.

A configured value is either a literal or a *reference* — exactly one of a file, an environment variable name or a command — and never a pasted secret. No new secret store: this is the custody shape `core/internal/config` already had. A resolved value exists only while a step runs. Nothing resolved reaches a plan, a plan digest, a spool file or a journal row, and that is structural rather than careful: the plan has no field that could hold one. Output is redacted streaming at every chunk boundary, so a credential split across two reads of a pipe is still caught, with needles sorted longest-first so `user@host` is not left as `user@[REDACTED]`.

## FR-39, Local Hooks Execute Outside the Container

A `.local.sh` hook means "run this on the machine retnd is installed on". The engine cannot run one: its image is distroless and has no shell, the container is read-only and non-root, and every capability is dropped. That contract does not move to buy a shell.

Local hooks execute through a **Host Workflow Runner**: a separate host process, built from the same commit as the engine and extracted from the same image by the installer, supervised as `retnd-workflow-runner.service`. It listens on one Unix-domain socket and never a TCP port, requires an installation-scoped credential on every connection, refuses an engine whose version is not exactly its own, and refuses to run as root. It accepts captured script *bytes*, never a path. Each hook runs in an ephemeral container with `--cap-drop ALL`, `--security-opt no-new-privileges`, `--read-only`, a non-root `--user`, `--network none` and a pids limit, and no Docker socket is mounted into a hook container under any name. The runner never pulls: an image that is not present is a refusal, not a download.

The engine container gains exactly two mounts and one file for this, and the one holding scripts is read-only. `docs/runtime-contract.md` is the contract and its gates stay green.

## FR-40, Remote Hooks Require a Proven Exec Capability

A `.remote.sh` hook runs on the host the backup set pulls from. The transfer credential is frequently, and by this product's own instruction, incapable of running a command: `docs/ssh-setup.md` teaches a chroot and a forced `internal-sftp`. Exec capability is therefore probed and proven before anything is served, and an SFTP-only or forced-command account is refused with a reason naming the missing capability — while artifact backup over that same credential keeps working unchanged.

A deployment declares execution connections (`workflows.exec_connections`) and a backup set may name one (`remote_exec_connection_ref`); absent that, remote hooks run over the set's own source connection. The script is streamed over the channel's stdin: nothing is uploaded, no file is written, nothing is left behind if the connection drops. No environment value reaches a command line. No PTY is allocated, so stdout and stderr stay separate streams. A hook runs as that user and this product never escalates: there is no configuration field for `sudo`, `su` or a command prefix.

## FR-41, Three Statuses, Never One

`backup_status`, `workflow_status` and `cleanup_status` are recorded separately and none is derived from the others, because "the backup succeeded and the cleanup did not" means a machine may be sitting quiesced with a good backup beside it, and that is not a state one Boolean can hold.

A failing before hook gives `backup_status = skipped` — nothing was attempted — while the run's own `state` carries *why* (`failed`, `canceled`, `timed_out`) and `failed_step` and `failed_script` name what ended it. A failing after hook gives `cleanup_status = failed` and run state `cleanup_failed`, which is the worst terminal state the feature has: the backup may be fine and the machine is not back the way the workflow found it.

A bypassed run reports `workflow_status = skipped` and says the hooks were bypassed, rather than showing a green verdict for scripts nothing executed.

## FR-42, Durable Obligations, `recovery_required`, and Two Ways Out

A step that was running when the process died becomes `interrupted`, deliberately not `failed`: a recovery pass has to be able to tell "this script reported failure" from "this script's outcome is unknown and its side effects may be half-applied".

On startup, Reconcile marks every interrupted run `recovery_required`, blocks the backup sets those runs belong to — scheduled runs suspended, an ordinary manual run refused, a health warning raised, the script spool retained — and **replays nothing**. A run whose backup set is locked is left alone.

There are exactly two exits, and no dismiss. `resume-cleanup` runs only the eligible after stages, out of that run's own captured bytes re-verified against the digest recorded at plan time, with the environment the run was planned with and its secrets re-resolved from their references — so editing `/workflows` or the configuration while the daemon was down does not change what the recovery executes. A resumed after stage is told `RETND_RECOVERY=1` and `RETND_CLEANUP_REASON=interrupted_run`. A resume that cannot account for everything lands back at `recovery_required`, durably. `acknowledge` is the other exit and takes a reason, which is recorded, because the whole value of an acknowledgement is answering, six months later, why a backup set was unblocked without its cleanup ever running.

Two processes cannot unwind one run at once: a scope moves to `in_progress` durably before a hook runs, and a second caller that finds it there is refused, so a terminal resume and a browser resume serialise on a row. The backup-set lock spans all five stages, cleanup included, and refuses rather than queues.

The bypass cannot clear a hold, and that is structural: the advance path refuses to settle the recovery axis at all.

## FR-43, Termination Certainty

Cancel and timeout record whether this product *proved* the process was gone: `confirmed`, or `unconfirmed`. A kill that could not be confirmed deliberately keeps the per-step working directory as the forensic record instead of cleaning it up. The local runner owns a process group and an engine lease, so a runaway is killed when the lease expires.

A hook that deliberately detaches a child — `nohup`, `setsid`, a double fork — is outside the guarantee, and the documentation says so rather than implying a completeness the product does not have.

## FR-44, One Logical Terminal Per Step, Bounded, Read-Only

Each step owns one logical log stream. Records carry the stream identity (`stdout` or `stderr`), a sequence number from one run-monotonic counter, and a kind; a merged view is labelled best-effort capture order, because two pipes have no total order to claim.

Output is persisted through the engine first and then fanned out asynchronously with bounded queues. A follower that stops reading is dropped from the queue rather than waited for, and catches up from the journal, which is the authority — a slow viewer cannot slow a database quiesce. Output is bounded per step and per run, and the truncation marker sits *in* the sequence, in position, so a client does not render it as something the hook printed. When the bound is reached the hook keeps running with its output still drained, because a hook blocked on a full pipe is a backup that never finishes.

A script's output is untrusted terminal input. The viewer is read-only: stdin disabled, no link activation, no title or window side effects, no output-driven actions, bounded scrollback, nothing through `innerHTML`. The UI mounts only the selected viewer, so a hundred-step run is one terminal.

## FR-45, Deterministic Bash

`bash --noprofile --norc`, so `~/.bashrc` cannot change a hook's environment. The four startup variables are sanitized, and a session arriving with `BASH_ENV` or `ENV` set is refused rather than run.

Nothing is injected: no `set -e`, no `-u`, no `pipefail`, no `xtrace`. A hook runs exactly as written. Injecting shell options would silently change the meaning of every script an operator tested by hand, and the footguns injection would catch are answered by FR-46 instead.

Every hook gets a bound. Per-set `script_timeout` pins it, `workflows.script_timeout` is the deployment default, and the built-in default is five minutes. There is deliberately no spelling of "wait forever".

## FR-46, Shell Verification Gates the Save

Saving either workflow configuration verifies every discovered hook script and refuses the save if any of them is not a shell program this product would run. Over HTTP that is `409 WORKFLOW_SCRIPT_REJECTED` carrying a structured `blocking_scripts` list, so a client draws the position rather than parsing prose; the same refusal is raised by `settings workflow patch` and `backup-set workflow patch`. Nothing executes a script body: the verdict comes from a real Bash parser (`mvdan.cc/sh`) linked into this product, so it arrives even when no runner and no source host is reachable.

Six rules, this product's own, deliberately few, in the `BSH` namespace rather than ShellCheck's `SC`:

| Code | What it is about | Severity |
|---|---|---|
| `BSH001` | an expansion in a command argument that will word-split and glob | `info` |
| `BSH002` | a `cd` whose failure nothing notices | `warning` |
| `BSH003` | an `rm -rf` that becomes a root-level recursive delete the moment an expansion is empty | **`error`** |
| `BSH004` | `set -e` and a pipeline with no `set -o pipefail` | `warning` |
| `BSH005` | an unquoted expansion inside `[ ... ]`, a run-time syntax error when the value is empty | `warning` |
| `BSH006` | no `#!` interpreter line | `style` |

A parse error or an `error`-severity finding refuses the save. `warning`, `info` and `style` are reported and never block, because a verification that refuses everything gets turned off. A script too large or too deeply nested to parse is reported "not examined" — never drawn as a pass — and does not block a save: a prefix of a shell script is a different program.

Findings carry a 1-based line and column and an excerpt taken from the bytes that were read and hashed, so position and excerpt cannot disagree. Validation reports two verdicts and never folds them: `valid_for_backup` and `workflow_valid`.

## FR-47, CLI and Web Parity, and the Bypass Boundary

Everything the browser can do to a workflow, a terminal can do. `retnd workflow run list|show|steps|log`, `retnd workflow recovery show|resume-cleanup|acknowledge`, `retnd settings workflow [patch|env …]`, `retnd backup-set workflow [patch|env …]`, `retnd validate workflow <source/backup-set>`, `retnd workflow-runner serve|status`. Every mutating route has a `core/cliecho/routes.go` builder or an accepted parity exemption, enforced by the parity test in `apps/common/webhost/router_test.go`.

Beside a serving engine, the reads fall back to the durable journal but `resume-cleanup` and `acknowledge` are refused, because recovery state lives in that process's memory as well as in the journal, and a resume made beside it would unblock a backup set that process would go on refusing.

No workflow route is behind the destructive-operations gate, and the two recovery writes carry CSRF only: a resume executes only bytes from the run's own spool, deletes no artifact, snapshot or remote object, and gating it would put a quiesced machine out of reach of an operator who has not turned destructive operations on.

The bypass is the single exception and its boundary is the administrator boundary. `retnd fetch --skip-workflow-scripts` is declared **only to be refused**, because that command's pass runs no hooks at all and a flag that silently does nothing is worse than one that says so. The only bypass is `skip_workflow_scripts` on `POST /api/v1/operations` with `run_backup_set`, it is recorded durably on the run row and in a warn-level event naming who asked, and it cannot clear a recovery hold.

## FR-48, Compatibility

A deployment with no workflow configuration behaves exactly as it does today. An unset root turns the feature off. Neither new mount in the canonical definition uses `:?`, so a deployment whose `.env` predates this epic still starts. The no-hook path does no journal work at all and writes no run row.

# 4. TDD Contract

Every guard below has a planted violation that has been run and watched to fail. The full table, with the exact test names and the mutation used, is `docs/conformance/epic-l-matrix.md`; this is the summary of what is guarded.

| Guard | Planted violation that proves it fires |
|---|---|
| The canonical container contract survives the feature | add a Docker socket volume and make `/workflows` writable |
| The runner refuses root and private-socket permissions are not configurable | start it as root; widen the socket directory |
| Version mismatch and credential order | connect with a wrong version, and with no credential |
| The wire carries bytes, never a path | send a path and expect it to be opened |
| Discovery refusals: escape, symlink, custody, size | a `..` stage, a symlinked stage dir, a group-writable ancestor, an oversized script |
| Crash at every durable transition reconciles to `recovery_required` | crash at each transition in turn and reconcile from the rows alone |
| The bypass cannot clear a hold | run a bypassed run over a set in recovery |
| Environment injection is inert, locally and remotely | a value containing shell metacharacters and a newline |
| Streaming redaction across a chunk boundary | split a secret across two writes |
| Log flood bounded per step and per run | emit past the bound and check the marker's position |
| A stalled follower does not slow the hook | stall a follower and measure the hook |
| Only an administrator may bypass; authorisation rechecked on every log replay | replay with a downgraded session |
| No resolved secret in a metric label, an API response or CLI output | resolve a secret and scan all three |
| The matrix itself cannot lie | a row naming a test that does not exist, and a row green under another word |

# 5. Phases

Two phases. Numbering is `L<phase>.<n>`, with one insertion, `L7.5`, recorded honestly as an insertion rather than renumbered in.

## Phase 1, the engine, the host runner and the execution core

- L1 #808 Workflow domain, configuration schema, script discovery and environment model (FR-36, FR-37, FR-38)
- L2 #809 Host Workflow Runner and installer runtime for local hooks (FR-39)
- L3 #810 Exec-capable remote SSH execution for remote hooks (FR-40)
- L4 #811 Five-stage workflow lifecycle, crash-safe recovery and durable logs (FR-41, FR-42, FR-43, FR-44)
- L5 #812 Workflow security, adversarial and performance gate (FR-45, FR-48, and the whole TDD contract)

### Phase 1 entry gate

- [x] The adversarial review is done and its eleven findings are release requirements.
- [x] The EPIC issue and its sub-issues exist.
- [ ] This specification is merged. It is not: it was written at the end of the epic, in #817, which is the process defect recorded at the top of this file.

### Phase 1 exit gate

Checkable claims, not intentions. Every box below is held to the outcome `docs/conformance/epic-l-matrix.md` records for the matching row. Unlike EPIC E, there is no test comparing these boxes to that file — `core/tests/compat/matrix_test.go` is hardcoded to EPIC E's pair. That makes every tick here an ungated claim, which is why each one names its row.

- [x] Five stages execute in one order, with two durable scopes and the failure matrix the review demanded. See matrix rows CR-01 and CR-03.
- [x] Local hooks run through the Host Workflow Runner and the canonical container contract is unweakened. See matrix rows CR-01, GC-01, GC-12, GC-13, GC-14.
- [x] Remote hooks require a proven exec capability; an SFTP-only credential is refused for hooks and stays valid for backup. See matrix rows CR-02, GC-15.
- [x] A crash at every durable transition reconciles to `recovery_required`, blocks the set, and `--skip-workflow-scripts` cannot clear it. See matrix rows CR-03, GC-10.
- [x] Bash is deterministic and nothing is injected. See matrix row CR-04.
- [x] Termination certainty is recorded, and detached descendants are excluded in writing. See matrix row CR-05.
- [x] No follower can backpressure a hook; replay is by cursor. See matrix rows CR-06, GC-11.
- [x] Environment injection is inert on both paths and secrets are redacted streaming. See matrix rows GC-06, GC-07, GC-08, GC-20.
- [x] The seven scale scenarios are benchmarks in the tree and the no-hooks path does no journal work. See matrix row GC-16, numbers in `docs/perf/epic-l-workflows.md`.
- [ ] Every provider states its local-hook answer where its operator will read it. Not done, and deliberately not ticked: that surface is #877 and does not exist on this branch. See matrix row GC-22, the one non-`PASS` row in the file.

## Phase 2, the operator surface, the browser, and release readiness

- L6 #813 CLI, API and observability parity for workflows (FR-47)
- L7 #814 Web UI workflow run timeline and configuration UX (FR-41, FR-44)
- L7.5 #906 Shell-script static verification gating the save, with a findings UI (FR-46)
- L8 #815 Per-script read-only terminals and log streaming (FR-44)
- L9 #816 Web-UI end-to-end suite and container rig integration
- L10 #817 Docs site, animated GIFs and the end-to-end release gate

### Phase 2 entry gate

- [x] Every Phase 1 exit line holds, or is unticked with its reason.
- [x] The three statuses exist durably before any surface draws them, because a UI that derives one from another cannot be corrected later without a migration.
- [x] The shell-verification rule set is decided before the findings UI is built (L7.5 was inserted ahead of L9 and L10 for exactly this reason).

### Phase 2 exit gate

- [x] Every mutating workflow action has a CLI equivalent or an accepted exemption. Gated by the parity test in `apps/common/webhost/router_test.go`.
- [x] Every registered CLI verb has a row on the reference page. Gated by `distribution/packaging/site_reference_test.go`.
- [x] A hook script that does not pass verification cannot be saved, on both the HTTP and the CLI path. See matrix row, and `CHANGELOG.md` under `[Unreleased]`.
- [x] One logical terminal per step, only the selected viewer mounted, hostile output inert. See matrix rows CR-07, CR-08, CR-09.
- [x] The web UI draws the three verdicts without deriving one from another, and offers exactly the two exits from a hold. Held to L7 #814's and L8 #815's own component tests, which is the evidence this box is ticked on. The end-to-end run of *pressing* those two controls in a browser is not green yet — see section 8, carve-out 3 — so this box is a claim about what the UI renders and offers, not about a resume having been driven through a browser against a real deployment.
- [x] The docs site documents the feature with GIFs produced by committed tooling. `docs/site/workflows.html`, `docs/site/tools/capture-workflows.mjs`.
- [ ] The end-to-end suite is green on the container rig. **Not ticked, for one case.** 208 passed, 29 failed, 75 skipped; 28 of the 29 are the pre-existing #913 drift and the twenty-ninth is this epic's: the browser case that settles a recovery hold, red because of **#931** — a hook whose Host Workflow Runner vanishes mid-step is not bounded by `script_timeout`. Everything else EPIC L claims is green there, including both remote-hook cases against a real sshd. Section 8, carve-out 3, has the causal chain — and a box is not ticked because the remainder went well.

# 6. What I cut to fit two phases, and why

- **Conditional execution.** No `allow_failure`, no `only_on_success`, no expressions. Every one of them is a small language, and a small language around a backup is how an operator ends up with a cleanup that did not run for a reason nobody can read off the configuration.
- **Parallel steps and DAGs.** The feature is a wrapper around one serial backup. Concurrency here buys nothing and makes the cleanup obligation graph the hard part.
- **Per-step container images.** One pinned image, present or refused. A per-step image is a supply-chain surface per script.
- **A browser script editor or upload.** Scripts arrive by the same means the rest of the deployment's configuration does. A web form that writes executable code into a directory this product then runs is a different threat model than the one reviewed here.
- **Batch-level hooks around `--all`.** "Global" was deliberately defined as per-run inheritance; a second, batch-scoped meaning would make "did the global after stage run" ambiguous again.
- **Dynamic environment propagation between steps.** A step exporting a value to the next one turns the plan into a dataflow graph, and turns recovery into replaying one.
- **Provider-by-provider local-hook answers.** Deferred to #877 and recorded as the one non-`PASS` matrix row rather than quietly dropped.

# 7. Compatibility and migration summary

- A deployment that configures no workflow root is unchanged: no run row, no journal work, no new refusals.
- The two new mounts in the canonical definition are optional (`:-` defaults, no `:?`), so an `.env` predating this epic still starts.
- One forward migration, `0012_workflow_runs.sql`, adding rows nothing else reads. The state strings it stores are a compatibility surface: `timed_out` may not become `timedOut` to match a JSON convention somewhere, and a value may not be removed once anything has written it.
- Local hooks require Docker reachable by the runner's account and the pinned hook image already present on the host. A deployment with hook scripts whose runner cannot reach the daemon is refused by `install_docker_host.py preflight` with exit 12 and the `usermod -aG` line it needs. `WORKFLOW_RUNNER=off` opts out. An empty workflows directory is held to none of it.
- Remote hooks may need a *second* SSH account, because the first one was deliberately made unable to run a command. `docs/ssh-setup.md` covers it.

# 8. Definition of done, and what proved each line

This is the section #817 exists to write, and it is the one to read sceptically. The rules for it: a line is ticked only when something that can be run proves it; each line names that proof and whether it is **gated** (something goes red if it stops being true) or **ungated** (nothing checks it but this file); and a line that is not true is not ticked and says why.

`docs/epic-checklist.md` defines gated and ungated and is the source of that vocabulary.

## The functional Definition of Done from #807

- [x] **The five stages execute in the declared order, local steps through the Host Workflow Runner and remote steps over an exec-capable connection.** L1 #808, L2 #809, L3 #810, L4 #811. *(Gated:* `core/internal/workflowrun` executor tests and matrix rows CR-01, CR-02.*)* Proved against a real Docker daemon in `core/tests/containerhooks` and, in a browser against a real runner, by L9's rig — see the release-gate subsection below.
- [x] **The engine container is still distroless, non-root, read-only, `cap_drop: ALL`, `no-new-privileges`, with no Docker socket, host-root mount or privileged container.** L2 #809, L5 #812. *(Gated:* `distribution/compose:TestLocalHooksBoughtTheirShellWithoutWeakeningTheContainer` and the `docs/runtime-contract.md` prohibition scan; matrix rows CR-01, GC-12, GC-13.*)*
- [x] **A backup set with no workflow configuration shows no behavioural or performance regression.** L5 #812. *(Gated:* `core/internal/workflowrun:TestTheNoHookPathDoesNoJournalWorkAtAll` and `:TestTheZeroHookPathRecordsNothing` — zero durable-store calls and no run row. Numbers in `docs/perf/epic-l-workflows.md`: the no-root plan is 24,708 ns/op against 15,436,917 ns/op for a plan whose stage directories exist and are empty. Matrix row GC-16.*)* The perf document is explicit that it is a baseline to compare against, not a threshold this gate enforces.
- [x] **Before failure skips the backup and still runs eligible cleanup; backup failure still runs after steps; an after failure fails the workflow while the backup stays `success`.** L4 #811. *(Gated:* the executor's failure-matrix tests; matrix row CR-03. Also asserted in a browser by L9's rig: before-fail → backup `skipped`, after-fail → backup `success` with workflow `failed`.*)*
- [x] **A crash after preparation yields `recovery_required`, blocks scheduled and manual runs for that set, and `--skip-workflow-scripts` cannot bypass it; `resume-cleanup` executes the interrupted run's captured bytes with `RETND_RECOVERY=1`.** L4 #811. *(Gated:* `core/internal/workflowrun:TestACrashAtEveryDurableTransitionReconcilesToRecoveryRequired`, `:TestSkippingWorkflowScriptsCannotClearRecoveryRequired`, `:TestAnInterruptedRunBlocksTheSetAndResumesFromItsCapturedBytes`; matrix rows CR-03, GC-10.*)*
- [x] **Remote cancel and timeout record termination `confirmed` or `unconfirmed`, and `unconfirmed` is surfaced prominently.** L3 #810, L4 #811, L7 #814. *(Gated:* matrix row CR-05 for the recording; the surfacing is on the run page, called out at the top rather than in a step row.*)* The wire carries a boolean `termination_confirmed` per step; the CLI prints it only in the positive.
- [x] **An SFTP-only source credential stays valid for backup and is refused for hooks.** L3 #810. *(Gated:* matrix rows CR-02, GC-15. Proved against a real sshd by L9's rig: an `internal-sftp`-forced account is refused with the server's own "This service allows sftp connections only." while that backup set keeps backing up.*)*
- [x] **Bash runs deterministically with no injected `set -e`, `-u` or `pipefail`.** L2 #809, L3 #810. *(Gated:* matrix row CR-04.*)*
- [x] **Slow or reconnecting followers never backpressure a script, and a reconnect replays from the durable log by cursor.** L4 #811, L8 #815. *(Gated:* `core/internal/workflowrun:TestAStalledFollowerDoesNotSlowTheHookAndCatchesUpByCursor`; matrix rows CR-06, GC-11. Measured: `BenchmarkASlowFollowerWhileAScriptEmits` records 26.30 hook_ms with 398 records dropped.*)*
- [x] **One logical terminal per step, only the selected read-only viewer mounted, and hostile OSC, title or window output has no application side effect.** L8 #815, L7 #814. *(Gated:* matrix rows CR-07, CR-08, CR-09. Also drawn and asserted inert in a browser by L9's rig, and a six-step run observed using one mounted terminal.*)*
- [x] **Secrets are never printed, never persisted resolved, and never shown in the UI, metrics or audit.** L1 #808, L5 #812. *(Gated:* matrix rows GC-08, GC-20 — no resolved secret in a metric label, an API response or CLI output.*)*
- [x] **Every mutating browser action has a CLI or API equivalent, or an accepted parity exemption.** L6 #813. *(Gated:* the parity test in `apps/common/webhost/router_test.go`.*)*
- [x] **A hook script that does not pass verification cannot be saved.** L7.5 #906. *(Gated:* the save path refuses with `409 WORKFLOW_SCRIPT_REJECTED` on both HTTP and CLI; `core/internal/workflowlint` rule tests.*)* Added late, as an insertion ahead of L9 and L10, because a findings UI needs a decided rule set.
- [x] **Suite B covers the workflow UI states, and the docs site documents the feature with captured GIFs.** L9 #816 for the suite, L10 #817 for the site. *(Partly gated:* the suite is gated in CI; the docs site's GIF regeneration is ungated, which `docs/epic-checklist.md` section 10 states as a standing risk. The clips are produced by `docs/site/tools/capture-workflows.mjs` and re-running it reproduces them, which is the only reason to trust them.*)*

## The checklist obligations from `docs/epic-checklist.md`

- [x] **§1.3 The spec lives at `docs/EPIC-<letter>-<slug>.md` with the Status block.** This file. *(Ungated.)* Written at the end of the epic rather than at its start, which is stated at the top rather than hidden.
- [x] **§1.4 FR numbers continue the series and renumber nothing.** FR-36 through FR-48. *(Ungated.)* `docs/epic-checklist.md` §1.4 is updated to name FR-49 as the next free number.
- [x] **§1.5 A five-expert adversarial review section with verdicts and consensus.** Above, portable from #807's consensus table. *(Ungated.)*
- [x] **§1.6 Entry and exit gates per phase, as checkable claims.** Section 5. *(Ungated,* and stated there as ungated because the EPIC-E box-to-matrix test is hardcoded to EPIC E.*)*
- [x] **§1.7 A conformance matrix with a falsification per row.** `docs/conformance/epic-l-matrix.md`. *(Gated:* `core/tests/workflowgate/matrix_test.go` fails when a finding has no row, when a row names a test that does not exist, when a row carries no falsification, or when a row is green under any word other than `PASS` — and that guard has its own mutation self-test.*)* This is the item the checklist calls the one that matters most, and it is the one piece of EPIC L's evidence that was gated from the start.
- [x] **§3.3 Every new CLI verb reaches the dispatch table and gets a reference-page row.** `workflow` and `workflow-runner` are both in `core/cmd/retnd/main.go` and both have rows. *(Gated:* `distribution/packaging/site_reference_test.go`.*)*
- [x] **§9.4 The prose docs the change touches.** `docs/install.md` (the runner and the Docker prerequisite), `docs/runtime-contract.md` (the mounts and the per-hook launch), `docs/ssh-setup.md` (the exec credential), and, added by #817, `docs/recovery.md` and `docs/recovery-without-a-terminal.md` (the hold and the two exits). *(Ungated.)*
- [x] **§9.6 A CHANGELOG entry under `[Unreleased]` with the issue number, what changed, why, and what an existing deployment sees.** Six entries: #906, #814, #812, #815, #915 and #817. *(Ungated.)*
- [x] **§10 Screenshots and GIFs from the capture scripts, never by hand, against the mock API, with the clock pinned.** `docs/site/tools/capture-workflows.mjs`, added to the section 10 list. *(Ungated.)*
- [x] **§11 Compliance and supply chain.** One new vendored dependency, `mvdan.cc/sh`, linked in for FR-46. *(Gated* by the existing supply-chain checks.*)*

## What has not been proven, and the four carve-outs, one of which has since closed

The site's home page carries a section with this name and this document has one for the same reason: an epic's closing document that drops the uncomfortable half is worse than no document.

1. **A browser cannot start a backup run on any deployment this repository builds (#92).** `POST /api/v1/operations` with `run_backup_set` is refused `403 DESTRUCTIVE_OPERATIONS_DISABLED` everywhere, because the only implementation of the destructive gate this repository ships returns false unconditionally. There is no flag, environment variable or constructor argument that makes it true. Two consequences for this epic, both real: every workflow run the end-to-end suite observes is one the **scheduler** started, not one a test pressed a button to start — the rig sets `poll_interval` to the product's 60-second floor and waits; and the one bypass this feature has, `skip_workflow_scripts` on that same route, is therefore unreachable over HTTP too, so "an administrator may bypass" is a code path with unit-test coverage and no deployment in which it can be exercised. Nothing about the hook lifecycle depends on the gate, and nothing here is a workaround: it is the honest shape of what was tested.
2. **Remote hooks had never run, and this epic's own end-to-end suite is what caught it (#919, fixed in #920).** The engine passed the workflow step's ID as the remote-exec token. A step ID uses `~` as its reserved separator — `%04d~scope~phase~script` — and `remoteexec` refuses any token outside `[A-Za-z0-9._-]{1,120}`, for two good reasons: the token is interpolated *unquoted* into the fixed remote command, where a leading `~` would tilde-expand, and the reaper reads it back out of `ps`. So `Request.validate()` rejected every remote step in about 15µs, **before any SSH session was opened**, and the adapter's `err != nil` catch-all reported that refusal as `transport_lost` with a nil exit code. The capability probe carries no token, so `exec_capability` reported **ok** throughout: a deployment that looked correctly configured while no remote hook had ever executed, from L2/L3 until #920. The fix derives a conforming token by construction, `remoteexec.StepToken(runID, stepID)` — which also closes a latent cross-run reaper collision, since the reaper matches the token as a whole `-s` operand — and classifies `ErrConnection` and `ErrExecCapability` as `not_attempted` rather than `transport_lost`. Mutation-proven against the in-process fixture sshd: the raw step ID opens zero sessions.

   This is the strongest evidence in the epic for the thesis `docs/epic-checklist.md` is written around, and it argues against every one of this epic's own green lights. The capability probe was green. The unit tests were green. The CLI was green. The UI was green. The feature did not work. What surfaced it was driving the whole thing end to end, in a browser, against a real deployment — which is what L9 is, and it is why a release gate is not a formality even when every gate above it passes. A capability probe that does not exercise the same code path as the operation it is a probe for is a green light for an untested path, and that sentence should be read as a design rule rather than as an anecdote.
3. **The end-to-end suite is not all green, and exactly one red belongs to this epic.** The definitive run is **208 passed, 29 failed, 75 skipped** in 28.1 minutes, on retnd-tests pin `0b691c59`, executed by `scripts/e2e/three-machine-web-ui.sh` against a product image built from `2049ab67` on a clean daemon. **28 of the 29 failures are pre-existing product/suite drift tracked in #913** — proven pre-existing by running the same rig with `--no-workflows` against both this pin and an earlier one (`d0431449`) and getting byte-identical failure lists. L9 #816's PR (#914, merged) carries the per-case status and is authoritative; if it and this paragraph ever disagree, that PR is right and this paragraph is stale, which is the failure mode a number copied into a second document always has and the reason the pointer stays here beside the number rather than instead of it.

   **Remote hooks are proved end to end.** Both cases pass: a real remote hook executes on the source machine and prints its own hostname, and a remote run finishes the cleanup its after stage owes. The executor the product names agrees with the hostname the hook printed. Given carve-out 2 — that no remote hook had executed at all from L2/L3 until #920 — this is the single most load-bearing green in the epic, and it is green against a real sshd on a separate machine rather than against a fixture.

   Getting there is worth one paragraph, because it nearly read as #919 still being open. Before the fixture was corrected, that case failed on a cross-check comparing names: the hook printed that it had run on `exechost` while the product reported its executor as `Remote · exec-host` — two names for one machine, in the rig's own fixture and not in the product. What told a fixture defect apart from a product defect was that the assertion quoted *both* strings, so its own failure message read as "one of them is describing a different machine". A weaker assertion checking the line contained some hostname would have gone green while proving nothing; one comparing against a literal would have failed without saying why. That is the argument for assertions that print their evidence, and it is the same lesson the harness's own `AUTH_CARD` break taught from the other side: the dangerous failure is the one that produces a plausible answer.

   **The recovery path is proved at the API level and NOT through the browser, and that is the one red this epic owns. Its cause is a product defect, #931.** Killing the engine while a step reports `running` yields the two holds it should — one at `global` scope, one at `set` scope, five seconds after the engine is back — and the rig asserts that. The browser case that drives both ways out of a hold is the only non-#913 failure, and it fails *before it crashes anything*: the backup set it crashes into had stopped being scheduled, so the case never got a live hook to interrupt.

   It took three cycles to say what that means, and the first two answers were wrong in an instructive way. The symptom looks exactly like test starvation, so it was first read as a hold left unsettled by an earlier case. L9 fixed the sequencing properly — clearing mid-wait through a poll callback, settling the runner-outage deployment-wide at `afterAll` — and then gave the crash set an explicit `--script-timeout 90s` against its roughly 45-second hook. **The symptom did not move, byte for byte**, down to the line `the scheduler did not run this set, with no hold on it to explain why`. That negative result is what identifies the defect: **a hook whose Host Workflow Runner vanishes mid-step is not bounded by `script_timeout`.** The run sits in progress indefinitely, the engine correctly declines to schedule that set again, and because nothing finalised there is no hold either — no run, no hold, no diagnostic. The two holds appear only when a later engine restart reconciles the stuck run. Had the 90-second bound been applied, the run would have finalised and produced either a new run or a hold; neither appeared, which isolates it to the bound not being applied rather than to its value.

   This is a defect only this rig can see, because seeing it requires a real runner that can be taken away while a hook is genuinely executing — no mock and no unit test has a runner to remove. The browser case is written, unrelaxed, and stands as #931's regression guard.

   That distinction matters and is not a technicality: **nobody has yet pressed Resume cleanup or Acknowledge in a browser against a real deployment and watched the hold settle.** FR-42 rests on the unit and API evidence in §8 above; the claim that an operator can resume or acknowledge from the web UI rests on L7 #814's and L8 #815's component tests. That is a real gap in this epic's release gate, it is somebody's next task, and writing it down is not the same as closing it.
4. **Until #929, four profiles advertised local hooks and mounted none of the paths one needs (#921, fixed).** FR-39's answer only exists if the deployment actually mounts the workflow root read-only, the runtime directory holding the runner socket, and the runner credential file. The canonical definition always did; four adapter profiles declared the capability and carried none of the three, so the declaration was true about the host and false about the engine's half of it. What #929 establishes is worth stating precisely, because it is stronger than "mounts were added": every one of the four was a capability-made-real, not a correction of a wrong declaration. OpenMediaVault (Debian and systemd), Proxmox VE (an operator-administered guest), Portainer CE (an ordinary Docker host) and CasaOS (an app layer over a distribution the operator administers) can each host the runner, and each acceptance procedure was already right that the administrator installs it beside the stack. ZimaOS is not a correction either: it declared `localHooks: unavailable`, carried none of the three, and still does — the honest declared-unavailable case. So the true claim today is that **every provider advertising local workflow hooks can deliver them, and the five that cannot — ZimaOS, Synology, TrueNAS, Unraid and UGOS — say so out loud.**

   This is not the same as GC-22, and GC-22 stays unticked. That row is about the provider *capability contract* and the packaging metadata that would hold every profile's answer to a test, which is #877's surface and still does not exist. #929 made four profiles' answers true; it did not make them checkable. A reader should take "local hooks work here" from `docs/install.md` and `docs/runtime-contract.md`, and should know that nothing yet fails a build if one of those answers goes stale.

And four narrower limits, each of which the operator documentation also states:

- **A detached remote descendant is outside the termination guarantee.** A hook that runs `nohup`, `setsid` or a double fork can leave a process this product cannot prove is gone; termination records `unconfirmed` and the per-step working directory is kept as the forensic record rather than cleaned up.
- **Redaction cannot follow a secret a script has transformed.** The redactor matches the resolved values it was given. A script that base64s a credential and prints it has printed something no needle matches.
- **Power-loss cleanup is not guaranteed.** `recovery_required` is the product's answer to a crash, and it is an answer about the *next* start: between the crash and that start, whatever a before hook did to the machine stays done. There is no agent on the far host to undo it.
- **No acceptance procedure for this feature has been run on real NAS hardware.** Consistent with the rest of the repository: the platforms are build-supported and uncertified, and the provider-by-provider local-hook answer is #877's surface, the one non-`PASS` row in the conformance matrix.
