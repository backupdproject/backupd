// Package workflowrun is the five-stage workflow lifecycle: the engine
// that wraps one backup-set run in its hooks, and everything that has to
// be true if this process dies in the middle of doing so (EPIC L, #807;
// this package is #811).
//
// # The five stages, and the one order they run in
//
// global-before, backup-set-before, THE BACKUP, backup-set-after,
// global-after. Serial, in plan order, with the "after" stages unwinding
// in the reverse of the order the "before" stages were entered. Steps go
// to one of two adapters depending on their target -- the host runner
// (#809) for a NAME.local.sh, the remote executor (#810) for a
// NAME.remote.sh -- and which adapter a step goes to changes nothing
// about the sequence, because the sequence is the plan's (#808).
//
// The backup itself is today's backup-set operation, called through a
// function this package is handed. It is not reimplemented, wrapped in a
// retry, or given a second timeout: what this package adds is what
// happens around it.
//
// # The obligation, which is the whole design
//
// A run that dies between its "before" stage and its "after" stage leaves
// a machine with something done to it and nothing to say so. Its step
// rows all read "pending", which is what a run that died BEFORE its first
// hook also leaves -- and one of those is a quiesced database and the
// other is not.
//
// So entering a SCOPE is its own durable write, made before the first
// side-effecting command in that scope, and it is a promise: the "after"
// stage of this scope will be accounted for. There are two scopes. The
// global one is entered when the run begins; the backup-set one is
// entered only once global-before has succeeded, which is why a
// global-before failure makes global-after eligible and backup-set-after
// never eligible, while a backup-set-before failure makes both eligible.
// A configured-but-empty "before" directory still enters its scope, so
// its "after" stage still runs: an operator who put a report script in
// the "after" directory and nothing in the "before" one meant that.
//
// workflow.ObligationState is the vocabulary, and the rule that makes a
// crash tractable is that every unsettled value can move to
// recovery_required and nothing moves out of a settled one. A process
// that dies between any two durable transitions therefore leaves each
// scope either settled (nothing to do) or unsettled (and the
// reconciliation moves it to recovery_required). There is no third case.
//
// # Recovery is never automatic
//
// Reconcile, on startup, marks every interrupted run recovery_required
// and blocks the backup sets those runs belong to: scheduled runs are
// suspended, an ordinary manual run is refused, a health warning is
// raised, and the run's script spool is retained. Nothing replays.
//
// ResumeCleanup is the operator-driven exit. It runs ONLY the eligible
// "after" stages, out of the interrupted run's own CAPTURED BYTES, with
// the environment that run was planned with and its secrets re-resolved
// from their references -- so an operator who edited /workflows or the
// configuration while the daemon was down has not changed what the
// recovery executes. Those hooks are told BACKUPD_RECOVERY=1 and
// BACKUPD_CLEANUP_REASON=interrupted_run, because unwinding after a crash
// is a different job from unwinding after a run.
//
// AcknowledgeRecovery is the only other exit, and it takes a reason,
// which is written down.
//
// What cannot happen, structurally rather than by convention: nothing
// clears recovery_required without one of those two. The journal's
// ordinary advance refuses to settle the recovery axis at all
// (state.AdvanceWorkflowRun), and the one call that settles it
// (ResolveWorkflowRecovery) reads the obligations first. That is what
// #811 means by "--skip-workflow-scripts must be structurally unable to
// clear recovery_required": the flag that skips hooks is recorded on the
// run and has no path to the axis.
//
// # Three statuses, never one
//
// backup_status, cleanup_status and workflow_status are separate fields
// end to end, and the reason is in the failure matrix: an "after" hook
// that fails after a backup that succeeded produces
// Backup=success / Cleanup=failed / Workflow=failed, and every one of
// those three is something an operator acts on differently. A single
// verdict would have to pick one and lose the other two.
//
// A hook is told all three, as they stand when it starts. A "before"
// hook therefore sees BACKUPD_BACKUP_STATUS=unknown, which is a real
// value rather than an empty string, because an unset variable and one
// saying "nobody knows yet" read identically in `test -z`.
//
// # Logs that cannot be backpressured
//
// Every chunk of a hook's output is redacted (streaming, so a credential
// split across two reads of a pipe is still caught), numbered from one
// run-monotonic counter, and written to the journal. A follower -- a
// browser, a CLI tailing a run -- gets it from a bounded queue, and a
// follower that stops reading is dropped from that queue rather than
// waited for. It catches up by asking the journal for everything after
// the last sequence it processed, which is gapless because the journal is
// the authority and the queue is only a fast path.
//
// Persisted output is bounded per step. When the bound is reached one
// truncation marker goes into the sequence, in position, and the hook
// keeps running with its output still being drained -- because a hook
// blocked on a full pipe is a backup that never finishes, and that is a
// worse outcome than a log that says where it stopped.
package workflowrun
