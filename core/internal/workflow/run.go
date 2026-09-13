package workflow

import (
	"fmt"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// The two durable records: one Run per backup cycle that had a workflow,
// and one Step per script that plan declared.
//
// These are the DOMAIN shapes, not the journal's. internal/state persists
// them as plain columns and interprets none of the vocabulary, on the same
// discipline its own types.go states for the FR-10 lifecycle: the journal
// stores what this package decided. So the refusals live here, in
// Validate, and the journal's write path calls them.
//
// Every field a step gains after planning is a pointer or a zero-valued
// scalar with an explicit meaning, for state/types.go's reason: zero is a
// real answer. A script that exits 0 and a script whose exit status nobody
// observed must not read the same, which is why ExitCode is a *int and
// StateInterrupted exists.

// Run is one workflow run: everything that happened around one backup
// cycle of one backup set.
//
// A backup set with no workflow configuration produces no Run at all. That
// is not an empty run with zero steps -- it is nothing, no row, no spool
// directory, no hash -- because #808's first acceptance criterion is that
// such a set behaves exactly as it did before this package existed.
type Run struct {
	// ID identifies this run. It is the caller's to choose and it is the
	// spool's directory name, so Validate holds it to the same rule a
	// script basename obeys: it becomes a path component under the state
	// directory, and a run id containing a separator would put a spool
	// somewhere nobody declared.
	ID string

	// BackupSetID is which set this run belongs to. It is the resolved
	// model identity rather than a string, so nothing here concatenates
	// a source and a set name (model/ids.go's argument).
	BackupSetID model.BackupSetID

	// State is the run's position in its lifecycle. Any of RunStates.
	State State

	// StartedAt is when the run began, and it is required: a durable
	// record with no time on it cannot be reasoned about, which is
	// state.SnapshotHoldRequest's rule for the same reason.
	StartedAt time.Time

	// FinishedAt is nil until the run reaches a terminal state. Nil is
	// "still open", never "finished at the zero time".
	FinishedAt *time.Time

	// BackupStatus is what the backup itself did, as an "after" hook is
	// told it (BACKUPD_BACKUP_STATUS). StatusUnknown until the backup has
	// an outcome, which is what a "before" hook legitimately sees.
	BackupStatus Status

	// CleanupStatus is what the cleanup stage did
	// (BACKUPD_CLEANUP_STATUS). #811 owns the semantics; this is where
	// they are recorded.
	CleanupStatus Status

	// WorkflowStatus is the aggregate verdict over the run's own steps
	// (BACKUPD_WORKFLOW_STATUS): what a later stage's hook is told about
	// the stages before it.
	WorkflowStatus Status

	// RecoveryState is the second axis: whether an interruption in this
	// run has been dealt with. See RecoveryState's own doc for why it is
	// not folded into State.
	RecoveryState RecoveryState

	// ResolvedPlanHash is the fingerprint of the plan this run executes:
	// deterministic over the same directory contents, and computed once,
	// at snapshot time. Two runs over an unchanged workflow tree produce
	// the same value; a script whose bytes changed produces a different
	// one. See Plan.ResolvedPlanHash for what goes into it -- and, more
	// importantly, for what deliberately does not.
	ResolvedPlanHash string

	// ScriptSpoolRef is the run-scoped directory holding this run's
	// captured script bytes. It is the only place execution reads a
	// script from.
	ScriptSpoolRef string

	// Bypassed records that this run's hook scripts were deliberately
	// skipped (#811's history field, set by L6's
	// --skip-workflow-scripts).
	//
	// It is decided when the run is created and never afterwards, which
	// is what keeps it a history field rather than a control: a flag
	// that could be turned on mid-run would be a way to declare a run's
	// hooks irrelevant after they had already quiesced something, and
	// #811's requirement is that skipping hooks is structurally unable
	// to clear an outstanding recovery.
	Bypassed bool
}

// Validate reports every way this record is not one the journal may store.
//
// It returns the first problem rather than collecting them, unlike
// config's validator: a caller here is this product's own code assembling
// a record it controls, so a refusal is a bug report and the first one is
// the whole diagnosis. config.Validate collects because its caller is an
// operator with a text editor.
func (r Run) Validate() error {
	if err := validPathComponent("run id", r.ID); err != nil {
		return err
	}

	if r.BackupSetID.IsZero() {
		return fmt.Errorf("workflow: run %q names no backup set; a run not attributable to a set cannot be retained, recovered or reported", r.ID)
	}

	if !r.State.ValidForRun() {
		return vocabularyError("run state", r.State, RunStates())
	}

	if r.StartedAt.IsZero() {
		return fmt.Errorf("workflow: run %q has no start time; how long a run has been open is what a recovery pass reads", r.ID)
	}

	if r.FinishedAt != nil && r.FinishedAt.Before(r.StartedAt) {
		return fmt.Errorf("workflow: run %q finished at %s, before it started at %s",
			r.ID, r.FinishedAt.UTC().Format(time.RFC3339), r.StartedAt.UTC().Format(time.RFC3339))
	}

	for _, s := range []struct {
		what  string
		value Status
	}{
		{"backup status", r.BackupStatus},
		{"cleanup status", r.CleanupStatus},
		{"workflow status", r.WorkflowStatus},
	} {
		if !s.value.Valid() {
			return vocabularyError(s.what, s.value, Statuses())
		}
	}

	if !r.RecoveryState.Valid() {
		return vocabularyError("recovery state", r.RecoveryState, RecoveryStates())
	}

	if r.ResolvedPlanHash == "" {
		return fmt.Errorf("workflow: run %q has no resolved plan hash; the hash is what makes a plan auditable and a recovery reproducible", r.ID)
	}

	if r.ScriptSpoolRef == "" {
		return fmt.Errorf("workflow: run %q has no script spool; execution reads scripts from the spool and nowhere else, so a run without one can run nothing", r.ID)
	}

	return nil
}

// SpoolRetainable reports whether this run's captured scripts must still
// be kept.
//
// The rule is the run is terminal AND its recovery is settled, and it is
// one method because it is asked from two places (the retention pass and
// the run's own completion) and a comparison written the wrong way round
// at either of them deletes the scripts a recovery was about to run.
//
// The named intermediate is not decoration. "Settled" is the condition
// worth reading at a glance, and its NEGATION is what the method returns,
// so writing the two out separately is what keeps the double negative
// from being the thing a reader has to hold in their head.
func (r Run) SpoolRetainable() bool {
	settled := r.State.Terminal() && r.RecoveryState.Settled()

	return !settled
}

// Step is one script in one run: what it is, where it runs, and what
// happened to it.
//
// Everything down to TerminationConfirmed is decided at snapshot time and
// never changes afterwards -- that immutability IS the plan. Everything
// from State down is written as the run proceeds.
type Step struct {
	// ID identifies this step within its run, and it is the spool file's
	// name. It is derived (StepID), never supplied: a caller-chosen step
	// id would be a second naming scheme for the one thing that has to
	// stay a safe path component.
	ID string

	// RunID is the run this step belongs to.
	RunID string

	// Scope and Phase say which configuration declared the step and
	// which side of the backup it runs on.
	Scope Scope
	Phase Phase

	// Order is this step's position in the whole plan, counted across
	// every stage from zero, so that "step 7" is unambiguous in a plan
	// that has four stages in it. It is assigned by Snapshot from the
	// documented bytewise ordering and is unique within a run.
	Order int

	// ScriptName is the script's basename as discovered, which is also
	// its identity to an operator. It has passed ParseScriptName, so it
	// is safe to render into a path, a log line and a shell-adjacent
	// context.
	ScriptName string

	// ScriptSHA256 is the hex sha256 of the bytes this step will run,
	// taken from the descriptor the bytes were read through. It is what
	// lets an operator prove the script in the spool is the script that
	// was in /workflows at snapshot time, and what makes
	// ResolvedPlanHash change when a script's content does.
	ScriptSHA256 string

	// ScriptSize is the size of those same bytes.
	ScriptSize int64

	// Target is where the script runs, taken from its basename. See
	// Target's own doc for why it is never configured.
	Target Target

	// ExecutionConnectionRef names the connection a TargetRemote step
	// runs over. It is REQUIRED for a remote step and must be empty for a
	// local one: a local step carrying a connection reference is a
	// configuration that reads as if it were going to run somewhere it
	// will not.
	ExecutionConnectionRef string

	// Timeout is how long this step may run before it is killed. It is
	// resolved at snapshot time from the per-set or deployment default,
	// so a config edit mid-run cannot change the bound a running step is
	// held to.
	Timeout time.Duration

	// SpoolRef is the captured copy of the script: the only path
	// execution ever opens for this step.
	SpoolRef string

	// State is the step's position in its lifecycle. Any of StepStates,
	// and never one of the four run-only values.
	State State

	// StartedAt and FinishedAt are nil until the step reaches those
	// points. Nil is "has not happened", never the zero time.
	StartedAt  *time.Time
	FinishedAt *time.Time

	// ExitCode is nil unless a process actually exited and this product
	// observed the status. A killed step, an interrupted step and a step
	// that never started all leave it nil; nil and 0 are emphatically
	// not the same answer.
	ExitCode *int

	// TerminationConfirmed records that this product PROVED the process
	// it killed is gone, rather than that it sent a signal and moved on.
	//
	// It is a separate fact from StateTimedOut and StateCanceled because
	// a script that is still running after this product stopped waiting
	// for it is the worst case a workflow has: the backup proceeds while
	// a hook is still touching the thing it was quiescing. #810 owns the
	// proving; this is where the proof is recorded.
	TerminationConfirmed bool

	// StdoutLogRef and StderrLogRef point at this step's captured output.
	// Empty until there is any. They are references rather than the text
	// itself: a hook's output can be large, it is not something the
	// journal should grow without bound, and #812 owns where it lands.
	StdoutLogRef string
	StderrLogRef string
}

// Validate reports the first way this step is not one the journal may
// store. See Run.Validate for why it does not collect.
func (s Step) Validate() error {
	if err := validPathComponent("step id", s.ID); err != nil {
		return err
	}

	if err := validPathComponent("run id", s.RunID); err != nil {
		return err
	}

	if !s.Scope.Valid() {
		return vocabularyError("step scope", s.Scope, Scopes())
	}

	if !s.Phase.Valid() {
		return vocabularyError("step phase", s.Phase, Phases())
	}

	if s.Order < 0 {
		return fmt.Errorf("workflow: step %q has order %d; a plan's order is its position counted from zero", s.ID, s.Order)
	}

	// The two derivations, and the reason this is the load-bearing check
	// in this file rather than a schema formality.
	//
	// A step's target is read off its script's basename and NEVER
	// configured (see Target's doc), and its id is derived from the order,
	// scope, phase and that same name (StepID). A record that says
	// quiesce.remote.sh runs LOCALLY is therefore not a record this
	// package can have produced -- but it is one a recovery pass can read
	// back off disk after a restart, and executing it would run a script
	// written for somebody else's database server as root on the backup
	// server. So the parsed target is RETAINED and compared, rather than
	// discarded after the name was found to be well-formed.
	named, err := ParseScriptName(s.ScriptName)
	if err != nil {
		return err
	}

	if !s.Target.Valid() {
		return vocabularyError("step target", s.Target, Targets())
	}

	if named != s.Target {
		return fmt.Errorf(
			"workflow: step %q runs script %s, whose name says it runs %s, and the step says %s. Where a hook runs is read off its basename and never configured, so the two disagreeing is a record this product did not build: executing it would run a script written for one machine on the other",
			s.ID, s.ScriptName, named, s.Target)
	}

	if want := StepID(s.Order, s.Scope, s.Phase, s.ScriptName); s.ID != want {
		return fmt.Errorf(
			"workflow: step %q is not the id derived from its own order, scope, phase and script name (%q). The id is derived rather than supplied because it is the spooled script's filename, and a record whose id does not follow from its fields names a file that belongs to some other step",
			s.ID, want)
	}

	switch {
	case s.Target == TargetRemote && s.ExecutionConnectionRef == "":
		return fmt.Errorf(
			"workflow: step %q runs %s and names no execution connection; a remote hook with nowhere to run is a step that would be silently skipped on the host it was written for",
			s.ID, TargetRemote)
	case s.Target == TargetLocal && s.ExecutionConnectionRef != "":
		return fmt.Errorf(
			"workflow: step %q runs %s and names execution connection %q; a local step carrying a connection reads as if it were going to run somewhere it will not",
			s.ID, TargetLocal, s.ExecutionConnectionRef)
	}

	if len(s.ScriptSHA256) != 64 {
		return fmt.Errorf(
			"workflow: step %q has script hash %q, which is not a hex sha256; the hash is what proves the spooled bytes are the ones that passed validation",
			s.ID, s.ScriptSHA256)
	}

	if s.ScriptSize < 0 {
		return fmt.Errorf("workflow: step %q has size %d", s.ID, s.ScriptSize)
	}

	if s.Timeout <= 0 {
		return fmt.Errorf(
			"workflow: step %q has timeout %s; a hook with no bound can hold a backup window open indefinitely, so there is no spelling of \"wait forever\"",
			s.ID, s.Timeout)
	}

	if s.SpoolRef == "" {
		return fmt.Errorf("workflow: step %q has no spooled script; execution reads from the spool and nowhere else", s.ID)
	}

	if !s.State.ValidForStep() {
		return vocabularyError("step state", s.State, StepStates())
	}

	if s.FinishedAt != nil && s.StartedAt == nil {
		return fmt.Errorf("workflow: step %q finished without ever starting", s.ID)
	}

	if s.StartedAt != nil && s.FinishedAt != nil && s.FinishedAt.Before(*s.StartedAt) {
		return fmt.Errorf("workflow: step %q finished at %s, before it started at %s",
			s.ID, s.FinishedAt.UTC().Format(time.RFC3339), s.StartedAt.UTC().Format(time.RFC3339))
	}

	return nil
}

// StepID is the derived identity of one step: its order, scope, phase and
// script name, joined by a character the script-name rule reserves.
//
// It is derived rather than supplied for two reasons. It has to be unique
// within a run, and order already is; and it has to be a safe single path
// component, because it is the spool file's name. Deriving it from parts
// that have each already been validated means there is no second place a
// path component is invented.
//
// The order goes first, zero-padded, so a directory listing of a spool
// reads in execution order -- which is what somebody debugging a run
// actually wants from it.
func StepID(order int, scope Scope, phase Phase, scriptName string) string {
	return fmt.Sprintf("%04d~%s~%s~%s", order, scope, phase, scriptName)
}

// validPathComponent is the rule for the two identifiers that become
// directory and file names under the state directory.
//
// It is deliberately narrow. These strings are joined onto a path this
// process then creates with 0700/0600 and later reads a script out of, so
// the only interesting question is whether the value can move that path:
// "..", a separator, a NUL or an empty string all can, and a control
// character cannot but turns one audit line into two.
func validPathComponent(what, v string) error {
	switch v {
	case "":
		return fmt.Errorf("workflow: %s must not be empty", what)
	case ".", "..":
		return fmt.Errorf("workflow: %s must not be %q: it names a directory rather than a run", what, v)
	}

	for _, r := range v {
		switch {
		case r == '/' || r == '\\':
			return fmt.Errorf("workflow: %s %q must not contain a path separator: it becomes a single directory name under the state directory", what, v)
		case r == 0:
			return fmt.Errorf("workflow: %s must not contain a NUL byte", what)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("workflow: %s %q must not contain control characters", what, v)
		}
	}

	return nil
}
