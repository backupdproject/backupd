# ADR 0022: The five-stage workflow lifecycle, its cleanup obligation, and its logs

- Status: accepted
- Date: 2026-09-13
- Scope: EPIC L (#807), issue #811. Builds on ADR 0019 (#808, the plan and
  the spool), ADR 0020 (#809, the host runner) and ADR 0021 (#810, remote
  exec) and does not re-litigate them. Blocks #812, #813 and #814.

## Context

#808 decided WHAT a run executes and captured it. #809 and #810 decided
HOW one script runs, locally and remotely. What was left is the part that
touches a machine: running four stages of hooks around a backup, in an
order, and being able to say afterwards what happened to the machine —
including when this process died halfway through.

The failure this exists to prevent is specific and is not a crashed
daemon. It is a quiesced database, a mounted share or a paused replica
that nobody unwinds, on a host where the only evidence is a load average
and a disk filling up. A daemon that comes back up and cannot tell "we
had not started yet" from "we had already stopped the database" has no
safe move available: replaying is a second side effect, ignoring it is an
outage nobody attributes.

## Decision 1: five stages, nested, serial, in plan order

`global_before -> backup_set_before -> BACKUP -> backup_set_after ->
global_after`. Serial, in the plan's order, with the "after" stages
unwinding in the reverse of the order the "before" stages were entered —
the nesting a shell trap, a `defer` stack and a transaction all use, and
the only order in which a global hook that mounted something can rely on
the per-set hooks being finished with it before it unmounts.

A step's target (`.local.sh` / `.remote.sh`) selects the adapter and
changes nothing else. The engine dispatches through one internal
interface with one mapping from outcome to state, precisely so that the
sequencing cannot grow a special case for one adapter.

The backup stage calls today's backup-set operation, unchanged, through a
function the engine is handed. It is not wrapped in a retry, not given a
second timeout, and not reimplemented.

Global hooks are **inherited configuration, not a deployment-wide
critical section**: they wrap each individual backup set's run, and two
sets back up concurrently, each running its own copy.

## Decision 2: the failure matrix, stated as behaviour rather than as a verdict

- a "before" failure (non-zero, timeout, cannot start, session lost)
  marks the step failed, stops the later steps of that stage, **skips the
  backup**, runs the eligible cleanup, and fails the workflow;
- a **backup** failure still runs the eligible "after" steps — whatever
  the "before" hooks did to the machine has to be undone whether or not
  the backup worked;
- an "after" failure is recorded, the **later cleanup continues** (an
  unmount that fails must not stop the notification that says so), and
  the workflow fails **even though the backup succeeded**.

That last row is why `backup_status`, `cleanup_status` and
`workflow_status` are three separate fields end to end, in the domain
type, in the journal, in the result and in the hook's environment. The
run it describes is `Backup: success / Cleanup: failed / Workflow:
failed`, and an operator acts on each of the three differently. A single
verdict has to pick one and lose the other two.

A hook is told all three as they stand when it starts, so a "before" hook
sees `RETND_BACKUP_STATUS=unknown` — a real value rather than an empty
string, because an unset variable and one saying "nobody knows yet" read
identically in `test -z`.

## Decision 3: eligibility is a property of SCOPE ENTRY, not of what ran

The global scope is entered when the **run begins**. The backup-set scope
is entered once **global-before has succeeded**. Everything else follows:

| situation | global-after | backup-set-after |
| --- | --- | --- |
| everything succeeds | eligible | eligible |
| global-before fails | eligible | never eligible |
| backup-set-before fails | eligible | eligible |
| backup fails | eligible | eligible |
| "before" directory configured and **empty** | eligible | eligible |
| no "before" stage configured at all | eligible | eligible |

The empty-directory row is the one that is easy to get wrong and is
deliberate: an operator who put a report script in the "after" directory
and nothing in the "before" one meant that, and post-run reporting is a
legitimate use of a stage. Deriving eligibility from "did a hook run"
would silently drop it.

A scope with nothing to undo still discharges its obligation
successfully: the promise was to be able to say what became of the scope,
and "there was nothing in it" is an answer.

## Decision 4: the cleanup obligation is a durable row written before the side effect

This is the load-bearing decision.

A run that dies between its "before" and "after" stages leaves step rows
that all read `pending` — byte for byte what a run that died *before* its
first hook leaves. No query over the steps can tell those apart, because
the difference is not in the steps: it is in whether this product had
already decided to enter the scope.

So entering a scope is **its own durable write**, made before the first
side-effecting command in that scope. For the global scope the first such
command is the run's first hook, so the obligation is committed *in the
same transaction as the plan* — the only write unambiguously before it.
For the backup-set scope the first such command may be the **backup**
rather than a hook (an empty "before" directory), so the write happens at
scope entry, not at first-hook start.

Seven states, one per thing an operator has to do next:

| state | what it means | what to do |
| --- | --- | --- |
| `never_eligible` | the scope was never entered | nothing |
| `eligible_not_started` | entered, cleanup not begun | run it |
| `running` | cleanup executing | wait |
| `success` | every applicable "after" step reached success | nothing |
| `failed` | cleanup ran and did not succeed | a person; the run is over |
| `recovery_required` | cannot be accounted for | a person; **the set is blocked** |
| `manually_acknowledged` | a person took responsibility, with a reason | nothing |

The transition graph is written out in `internal/workflow`, and the two
properties that make a crash tractable are the absence of edges rather
than the presence of them: **nothing leaves a settled state** (so no
later pass re-opens a scope somebody closed), and **every unsettled state
can reach `recovery_required`** (so erring toward recovery is possible
wherever a process happened to die).

`failed` is settled and `recovery_required` is not, and the asymmetry is
the whole design: a cleanup that ran and failed is a finished story with
a bad ending, while one nobody can account for is unfinished. The first
is reported; the second blocks the set.

## Decision 5: the reconciliation rule, in two branches

On startup, for every run in a non-terminal state, `reconcileRun` is a
pure function of the rows found:

1. **some scope is not settled** → the run becomes `recovery_required`.
   Every step that was `running` becomes `interrupted` (nobody observed
   its exit status and nobody ever will — that is not a failure, it is an
   outcome that does not exist). Every `pending` "before" step becomes
   `skipped`. Every `pending` "after" step of an outstanding scope
   **stays pending**, because it is still owed and marking it terminal
   would leave the recovery with nothing to run.
2. **every scope is settled** → the run's work was demonstrably finished
   and only the summary row is missing. That is not a recovery, and
   treating it as one would block a set whose machine was provably put
   back because the process died on its last write. It is safe precisely
   because a scope only settles when its cleanup reached a terminal
   recorded state, so such a run has no interrupted step anywhere in it.

Branch 1 is reachable from every crash point during a run's work, because
the global scope's obligation is `eligible` from the moment the plan is
committed. That is what makes "errs toward `recovery_required`" a
property of the graph rather than a list of cases somebody thought of.
`TestACrashAtEveryDurableTransitionReconcilesToRecoveryRequired` stops
the process after each of the run's durable writes in turn, restarts on
the same journal, and asserts the invariants at all of them.

Reconciliation is idempotent: a run it has moved is no longer in a
non-terminal state.

## Decision 6: recovery never replays, and has exactly two exits

A daemon coming back up runs **no hooks**. It cannot know what the
interrupted step got as far as doing, so unwinding would act on a state
nobody has looked at and re-running would apply the side effect twice.
What it does is record, block the set (scheduled runs suspended, an
ordinary manual run refused, a health warning raised, the run's spool
retained), and wait.

**`resume-cleanup`** runs only the eligible "after" stages, out of the
interrupted run's **captured bytes** — the plan is rebuilt through
`workflow.RecoverPlan`, which re-verifies every script against the sha256
taken at snapshot time — with the ordinary environment the run was
*planned* with and its secrets re-resolved from their references. An
operator who edited `/workflows` or `config.yaml` while the daemon was
down has changed nothing about what the recovery executes. Those hooks
get `RETND_RECOVERY=1` and
`RETND_CLEANUP_REASON=interrupted_run`, because unwinding after a crash
is a different job from unwinding after a run.

A resume does what it can and blocks on what it cannot: a scope whose
"after" step was itself **interrupted** goes back to `recovery_required`,
since re-running an undo whose first attempt may have half-applied is not
a decision this product will take.

**Acknowledgement** is the only other exit. It requires an actor and a
non-blank reason, both of which are written down, and the journal refuses
to settle the run's recovery axis until every scope has reached a settled
state.

Nothing else can clear `recovery_required`. `state.AdvanceWorkflowRun`
refuses to lower the recovery axis at all, so the only call that settles
it is `ResolveWorkflowRecovery`, which reads the obligations first. This
is what makes #811's requirement about L6's `--skip-workflow-scripts`
structural: the flag is recorded on the run as history (a column written
once, with the plan) and has no path to the axis.

## Decision 7: cancellation and timeout keep the unwinding

On cancellation or timeout the engine stops starting new "before" or
backup work, lets the executors' context cancellation terminate what is
running (which for the host runner is the lease, and for the remote
executor is reap-then-signal), records what was **proved** about each
termination, marks the backup skipped, and then runs the eligible cleanup
under a **separate bounded timeout on a context whose cancellation has
been removed**. The moment a run is cancelled is the moment its unwinding
matters most, so cleanup cannot inherit the deadline that just expired,
and abandoning an unmount because somebody pressed Ctrl-C twice is how a
machine gets left quiesced.

The run's state records the cancellation and `cleanup_status` still
records what the cleanup did: neither is flattened into the other.

A related consequence worth stating, because it was a real bug caught by
the cancellation suite: every **journal** write in a run uses a context
with the cancellation removed, while every **execution** uses the
cancellable one. A cancelled run still has to record what its steps did —
a step killed by a cancellation is `canceled`, which is an outcome this
product observed — and a bookkeeping write that failed with the
cancellation would leave the step reading `running`, which the next
startup reconciles as an interruption and blocks the set for nothing.

## Decision 8: per-step timeouts are resolved at snapshot time and never unlimited

`internal/workflow` already resolves each step's bound when the plan is
captured, from the deployment default or the per-set override, and there
is no spelling of "wait forever". This engine holds the step to the bound
the plan recorded, so a configuration edit mid-run cannot change what a
running step is held to. The cleanup stage's own bound is separate (see
above) and is not the sum of its steps'.

## Decision 9: the log is a sequence in the journal, and the fan-out cannot be backpressured

Per-step output is redacted, numbered from **one run-monotonic counter**,
and written to the journal as rows: stream identity, capture timestamp,
payload as an undecoded BLOB (a hook's output is not required to be
UTF-8, and a store that re-encoded it would be editing evidence).

- **Redaction is streaming and happens before persistence.** A credential
  can straddle two 64 KiB reads off a pipe, so a per-chunk `Filter`
  matches neither half and both land in the journal, where anybody
  reassembles them from two adjacent rows. `obs.StreamFilter` holds back
  the longest suffix of what it has seen that could still be the start of
  a needle — not a fixed "longest needle minus one", which leaks for a
  needle that begins inside the emitted region. The needle set is the
  deployment's sensitive endpoints plus the step's own resolved secret
  values, layered for the duration of that step and dropped.
- **Followers cannot slow a hook down.** Every record is journalled and
  then *offered* to each subscriber's bounded queue with a non-blocking
  send. A subscriber that is not reading is dropped from the live path
  and marked as having fallen behind; it catches up by asking the journal
  for everything after the last sequence it *processed*. The journal is
  the authority and the queue is only a fast path, which is what makes
  the catch-up gapless. The subscriber owns its cursor, because only it
  knows what it has processed as opposed to what was handed to its
  channel.
- **Persisted output is bounded per step, and the bound drops rather than
  throttles.** A hook whose stdout pipe fills up blocks, and a hook
  blocked on a pipe is a backup that never finishes — the worst outcome
  available. So the bytes keep being read; what stops is the recording,
  marked by one truncation record **in the sequence, in position**, so a
  follower sees where the output stops rather than reading the gap as the
  end.

## Decision 10: the backup-set lock spans all five stages

Taken before the first "before" hook, released after the last "after"
step. A new run of the same set is refused (`ErrRunInFlight`) rather than
queued — `SubmitRunCycle`'s existing argument, plus a new one: a queued
run would race the previous run's cleanup as soon as the queue drained,
which is what the lock exists to prevent. The lock is per set, so
different sets run concurrently.

Across a restart the lock is gone and the durable side takes over: an
interrupted run leaves an obligation requiring recovery, and that is what
blocks the set.

## Decision 11: the health warning reuses the halts MODEL, not the halts TABLE

`backup_set_halts` is a standing statement that the manager could not
**connect** to a set, and its reasons are a `CHECK` constraint in the
schema. Widening one costs a table rebuild — 0002 and 0006 are what that
cost the last time, and `migrate.go`'s `suspendForeignKeys` exists
because of the damage.

So the durable claim here is the run's own `recovery_required` row, which
has a partial index built for exactly this query, and the model is the
same one halts use: **presence is the claim**. There is no boolean
anywhere saying a set is fine. The visible warning is an
`obs.Logger.Alert` on the existing alert event, and the run and step ids
are carried into the existing correlation model (`obs.Logger.Begin` and
`Action.End`, which pair a start with its outcome by action id).

## Decision 12: a set with no workflow pays nothing

A backup set with no hooks configured produces no run row, no spool, no
obligation and no log record. `Engine.Run` takes the set lock, calls the
backup, and returns. Measured at 90ns and two allocations per run against
a backup that takes minutes, and pinned by a benchmark, because the way
this regresses is not a slow function — it is somebody adding a journal
read "for consistency" on the path every hook-less deployment takes for
every set on every cycle.

## Consequences

- Adding a state to any of these vocabularies is a Go function and a
  test, not a migration. That is the trade 0012 made deliberately and
  0013 keeps.
- A run's history is queryable per step: workflow status, backup status,
  the failed step, duration, script count and the bypass flag all come
  off the rows rather than out of a summary blob.
- An operator can be blocked by a set they cannot un-block without either
  running the cleanup or writing down why they are not going to. That is
  the intended cost, and it is the only alternative to a product that
  quietly backs up a database it may have left paused.
