package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// The run-start snapshot: the single function in this package that decides
// what a run will execute, and the reason the rest of it exists.
//
// # The sequence, and why it is this sequence
//
// canonicalize -> discover -> validate ancestry -> open each file ONCE ->
// check custody on the descriptor -> read -> hash -> copy into a
// run-scoped spool -> return an immutable Plan.
//
// Each arrow is a place a naive implementation would leave a window open.
// Validating a path and then opening it separately is the classic
// check-then-use race; opening it, reading it, and then re-opening it to
// copy it is the same race with an extra step. So every file is opened
// exactly once, with O_NOFOLLOW so a symbolic link cannot be substituted
// at the moment of the open, and every subsequent question -- is this a
// regular file, who can write it, how big is it, what is its hash, what
// bytes go in the spool -- is answered from that ONE descriptor and the
// ONE buffer read through it.
//
// # The spool is the execution authority
//
// After Snapshot returns, nothing in this product ever opens a path under
// the workflow root again. Execution goes through Plan.OpenScript. That is
// what makes the guarantee in #808's acceptance criteria true rather than
// hopeful: editing or deleting a script in /workflows after the snapshot
// cannot change what the run executes, and cannot change what a recovery
// of that run executes tomorrow after a restart, because the run's
// authority is a private copy under the state directory with modes
// (0700/0600) that say so.
//
// The Plan is OPAQUE for the same reason the spool exists. Its steps, its
// environment and its paths are package-private, and OpenScript is the
// only way to reach a script's bytes -- it re-derives the path from the
// plan, re-walks the spool with O_NOFOLLOW, re-checks custody and
// re-verifies the sha256. A Plan that exported a mutable Steps slice and
// a SpoolRef string would be a plan its own consumer could edit between
// the journal write and the exec, which is not a boundary a comment can
// hold. spool.go carries the argument for the creation side.
//
// # The hash, and what is deliberately not in it
//
// ResolvedPlanHash is a fingerprint of the DECISION, not of the run. The
// run id, the timestamps and the spool paths are all excluded, because the
// property #808 asks for is that two runs over an unchanged workflow tree
// produce the same hash -- that is what makes the value useful for
// answering "did anything about what we execute change since last night".
//
// A resolved secret is not in it either, and that is a security property
// rather than a tidiness one: a hash over a credential is a credential
// oracle. What goes in is the same thing the config file holds, the
// LOCATION the secret comes from.

// DefaultMaxScriptSize is how large a hook script may be before this
// product refuses it.
//
// A shell script that quiesces a database is a few hundred bytes and a
// generous one is a few kilobytes. One mebibyte is not a realistic
// ceiling, it is a bound: something megabytes long in a hook directory is
// a payload that arrived where a script belongs, or a log file somebody
// redirected into the wrong place, and executing the first megabyte of
// either is worse than refusing both.
const DefaultMaxScriptSize int64 = 1 << 20

// MaxConfigurableScriptSize is the ceiling on the configurable bound.
//
// The bound is configurable because a legitimate hook can be a
// self-contained script with an embedded certificate or a here-document of
// SQL, and one megabyte is a judgement rather than a law. It is BOUNDED
// because "configurable" with no limit means a deployment can configure
// the protection away, usually while debugging something else, and then
// keep running that way forever. Sixteen mebibytes is far past any real
// script and still small enough that this process reads it into memory
// without thinking about it.
const MaxConfigurableScriptSize int64 = 16 << 20

// MaxPlanBytes bounds the TOTAL size of the scripts one run captures.
//
// MaxScriptSize bounds one file and MaxScriptsPerStage bounds one
// directory, and neither bounds the product: four stages of sixty-four
// one-mebibyte scripts is a quarter of a gigabyte this process reads,
// hashes and copies inside the backup window, with every individual file
// perfectly legal. Eight mebibytes is far past any workflow somebody
// wrote and small enough that capturing it is not something an operator
// notices.
const MaxPlanBytes int64 = 8 << 20

// DefaultStepTimeout is how long a hook may run when nothing says
// otherwise.
//
// There is deliberately no spelling of "wait forever". A hook that hangs
// with no bound holds the backup window open indefinitely, which turns one
// stuck script into a deployment that quietly stops backing anything up --
// the failure mode that is hardest to notice and most expensive to
// discover. Five minutes is long enough for a database checkpoint and
// short enough that a stuck hook is a failed backup somebody sees tonight.
const DefaultStepTimeout = 5 * time.Minute

// ErrPlan is a refusal about the snapshot request itself rather than about
// anything on disk: a missing run id, a stage that names a scope and phase
// twice, a bound outside MaxConfigurableScriptSize. These are programming
// or configuration mistakes, not operator file-system mistakes, and they
// are worth telling apart from ErrStageDir and ErrCustody for that reason.
var ErrPlan = errors.New("workflow: this workflow run plan cannot be built")

// StageSpec is one configured hook stage: which scope and phase it is, and
// the directory it was configured with.
//
// An empty Dir means the stage is DISABLED, and that is a different fact
// from a directory that exists and is empty. The first produces no steps
// because nothing was asked for; the second produces no steps because the
// operator asked for a directory they have not put anything in yet. Only
// the second is a directory this product will refuse when it goes missing,
// which is the distinction #808's technical requirements spell out and the
// reason Dir is a plain string rather than a pointer -- "" is already the
// unambiguous spelling of "not configured", since a stage directory can
// never legitimately be the empty path.
type StageSpec struct {
	Scope Scope
	Phase Phase
	Dir   string
}

// StageDirs is the pair of hook directories one scope declares.
type StageDirs struct {
	Before string
	After  string
}

// PlanStages returns the configured stages in EXECUTION order.
//
// The order is global-before, set-before, (the backup itself), set-after,
// global-after: the "after" stages unwind in the reverse of the order the
// "before" stages were entered. That is the same nesting a shell trap, a
// defer stack and a database transaction all use, and it is the only order
// in which a global "before" hook that mounted something can rely on the
// per-set hooks having finished with it before the global "after" hook
// unmounts it.
//
// Stages with no directory configured are omitted entirely rather than
// returned empty, so a caller cannot accidentally treat "disabled" as
// "configured with the empty path".
func PlanStages(global, set StageDirs) []StageSpec {
	candidates := []StageSpec{
		{Scope: ScopeGlobal, Phase: PhaseBefore, Dir: global.Before},
		{Scope: ScopeSet, Phase: PhaseBefore, Dir: set.Before},
		{Scope: ScopeSet, Phase: PhaseAfter, Dir: set.After},
		{Scope: ScopeGlobal, Phase: PhaseAfter, Dir: global.After},
	}

	out := make([]StageSpec, 0, len(candidates))
	for _, c := range candidates {
		if c.Dir != "" {
			out = append(out, c)
		}
	}

	return out
}

// SnapshotRequest is everything Snapshot needs. Nothing in it is optional
// except the two that have documented defaults (MaxScriptSize, Timeout)
// and RemoteExecConnectionRef, which is only required by a plan that
// actually discovers a remote script.
type SnapshotRequest struct {
	// RunID is this run's identity and the spool directory's name.
	RunID string

	// BackupSetID is the set the run belongs to.
	BackupSetID model.BackupSetID

	// Root is the approved, canonicalized workflow root. A zero Root is
	// refused: a request to snapshot with no root is a caller that should
	// not have got this far, since a deployment with no workflow root
	// configured has no workflow runs.
	Root Root

	// Stages are the configured stages, in execution order (see
	// PlanStages). A scope-and-phase pair appearing twice is refused:
	// two directories for one stage is a configuration with no defined
	// order between them.
	Stages []StageSpec

	// Env is the merged, validated configured environment. It is carried
	// into the plan and into the hash by LOCATION only; see the hash's
	// own note on secrets.
	Env Environment

	// Timeout is the resolved per-step bound. Zero takes
	// DefaultStepTimeout.
	Timeout time.Duration

	// RemoteExecConnectionRef names the connection remote steps run over.
	// It is required if and only if a remote script is discovered, which
	// is why the refusal is here rather than in config validation: a
	// deployment can perfectly reasonably configure hook directories that
	// contain only local scripts and no remote connection at all.
	RemoteExecConnectionRef string

	// MaxScriptSize bounds one script. Zero takes DefaultMaxScriptSize;
	// anything above MaxConfigurableScriptSize is refused.
	MaxScriptSize int64

	// SpoolRoot is the directory run-scoped spools are created under,
	// normally <state dir>/workflow-runs. It must be absolute: this
	// process creates directories under it and later executes what it
	// finds there, so a path that means different things depending on the
	// working directory is not acceptable.
	SpoolRoot string
}

// Plan is the immutable result of a snapshot: the complete, ordered list
// of what this run will execute, and nothing that could change underneath
// it.
//
// Every path in it points into the spool. Nothing in it points into the
// workflow root, deliberately: a Plan that carried the original paths
// would be a Plan somebody could be tempted to re-read.
type Plan struct {
	runID       string
	backupSetID model.BackupSetID

	// steps are in execution order, with Order matching the index.
	steps []Step

	// env is the configured environment this plan resolves with,
	// carrying secret LOCATIONS and no material.
	env Environment

	// resolvedPlanHash fingerprints the decision. See this file's
	// preamble for what is in it and what is deliberately not.
	resolvedPlanHash string

	// spoolRef is the run-scoped spool directory, and scriptsDir is the
	// directory inside it that holds the captured scripts. Both are
	// derived here rather than taken from a step, because they are what
	// OpenScript checks a step's own SpoolRef AGAINST.
	spoolRef   string
	scriptsDir string
}

// RunID, BackupSetID, ResolvedPlanHash and ScriptSpoolRef are the plan's
// scalar facts, for the journal and for the operator surfaces.
func (p Plan) RunID() string                  { return p.runID }
func (p Plan) BackupSetID() model.BackupSetID { return p.backupSetID }
func (p Plan) ResolvedPlanHash() string       { return p.resolvedPlanHash }
func (p Plan) ScriptSpoolRef() string         { return p.spoolRef }

// IsZero reports whether this is the zero Plan: no run, nothing to
// execute. It is what distinguishes "this set has no workflow" from "this
// set has a workflow with no steps in it".
func (p Plan) IsZero() bool { return p.runID == "" }

// Steps returns the planned steps in execution order.
//
// It COPIES, and the slice being unexported is the point rather than
// style. A Plan whose Steps slice a caller held could have a step's
// target, timeout or spool path rewritten after the hash was taken and
// after the plan was journaled -- by the very layer (#810, #811) whose job
// is to execute it. The copy means an execution layer can only ever change
// its own view, which then disagrees with the journal and with the hash
// instead of quietly becoming the plan.
func (p Plan) Steps() []Step { return append([]Step(nil), p.steps...) }

// Env is the configured environment this plan resolves with: secret
// LOCATIONS and no material. Environment is already a value type whose
// entries are copied out (see Environment.Vars).
func (p Plan) Env() Environment { return p.env }

// SpooledScript is one step's captured bytes, opened through the plan and
// verified against it.
type SpooledScript struct {
	// StepID is the step these bytes belong to.
	StepID string

	// Path is the file inside the run's own spool that was opened. It is
	// returned so an execution layer can hand it to an interpreter, and
	// it is derived from the plan rather than from anything a caller
	// supplied.
	Path string

	// Body is the script itself, verified byte-for-byte against the
	// sha256 the plan recorded at snapshot time.
	Body []byte
}

// OpenScript is the ONLY way to get at a spooled script, and it is a
// capability rather than a path accessor.
//
// The distinction matters because of what #810 and #811 are going to do
// with it. A plan that handed out paths would be a plan whose consumer
// executes a string -- and a string can come from a journal row that was
// edited, from a step whose SpoolRef was rewritten in memory, or from a
// recovery pass that reconstructed a plan out of a database somebody else
// can write. So this function trusts NOTHING it is given except the step
// id, and re-derives, re-checks and re-verifies everything else:
//
//   - the step must be one THIS plan has;
//   - its SpoolRef must be exactly <spool>/scripts/<step id>, which is
//     what makes a forged "../../etc/cron.d/x" or an absolute path
//     somewhere else a refusal rather than an open;
//   - the whole path to the spool is walked with O_NOFOLLOW and
//     custody-checked again, so a link or a group-writable directory that
//     appeared since the snapshot is caught here too;
//   - the bytes are hashed and compared against the plan's own sha256.
//
// The last one is the one that makes the rest worth having: it holds for a
// RECOVERED plan exactly as it does for a fresh one, which is the case
// where the spool has been sitting on disk across a restart.
func (p Plan) OpenScript(stepID string) (SpooledScript, error) {
	if p.IsZero() {
		return SpooledScript{}, fmt.Errorf("%w: there is no plan to open a script from", ErrSpool)
	}

	step, ok := p.step(stepID)
	if !ok {
		return SpooledScript{}, fmt.Errorf(
			"%w: run %q has no step %q, so there is nothing in its spool to open; execution may only run steps the plan declared",
			ErrSpool, p.runID, stepID)
	}

	want := filepath.Join(p.scriptsDir, step.ID)
	if step.SpoolRef != want {
		return SpooledScript{}, fmt.Errorf(
			"%w: step %q of run %q names the spooled script %s, and this run's spool holds it at %s. A step's spool path is derived from the run and the step id, so one that does not match is a record that has been edited since the plan was committed",
			ErrSpool, step.ID, p.runID, step.SpoolRef, want)
	}

	scripts, err := openTrustedDir(p.scriptsDir, false)
	if err != nil {
		return SpooledScript{}, err
	}
	defer scripts.Close() //nolint:errcheck // read-only

	body, err := readSpooledScript(scripts, step)
	if err != nil {
		return SpooledScript{}, err
	}

	return SpooledScript{StepID: step.ID, Path: want, Body: body}, nil
}

// step finds one step by id. Linear, because a plan is a handful of steps
// and a map would be a second structure to keep in step with the slice.
func (p Plan) step(stepID string) (Step, bool) {
	for _, s := range p.steps {
		if s.ID == stepID {
			return s, true
		}
	}

	return Step{}, false
}

// readSpooledScript opens one captured script through the scripts
// directory's own descriptor and verifies it against the step.
//
// The mode test is 0o077 rather than discover.go's 0o022: a spooled script
// is this process's private copy under the state directory, so anything
// another account can even READ is a spool that is not what this package
// documents. That is stricter than the rule the original file was held to,
// deliberately -- the original is an operator's file in an operator's
// tree, and this one is ours.
func readSpooledScript(scripts *os.File, step Step) ([]byte, error) {
	path := filepath.Join(scripts.Name(), step.ID)

	fd, err := unix.Openat(int(scripts.Fd()), step.ID,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be opened: %w", ErrSpool, path, err)
	}

	f := os.NewFile(uintptr(fd), path)
	defer f.Close() //nolint:errcheck // read-only

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be inspected: %w", ErrSpool, path, err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is a %s, not a regular file", ErrCustody, path, fileKind(info.Mode()))
	}

	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf(
			"%w: the spooled script %s has permissions %04o. This file is this process's own copy, written 0600, and anything else means somebody has been in the spool",
			ErrCustody, path, mode)
	}

	if err := checkOwnership(path, info); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(io.LimitReader(f, step.ScriptSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be read: %w", ErrSpool, path, err)
	}

	if int64(len(body)) != step.ScriptSize {
		return nil, fmt.Errorf(
			"%w: the spooled script %s is %d bytes and the plan recorded %d",
			ErrSpool, path, len(body), step.ScriptSize)
	}

	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != step.ScriptSHA256 {
		return nil, fmt.Errorf(
			"%w: the spooled script %s hashes to %s and the plan recorded %s. The plan's hash is what proves the bytes about to run are the bytes that passed validation, so a mismatch is refused rather than reported: whatever is in the spool now, nobody vouched for it",
			ErrSpool, path, got, step.ScriptSHA256)
	}

	return body, nil
}

// RecoveredPlan is what the journal holds about a run, on its way back to
// being a Plan.
//
// It exists because recovery is the case the capability in OpenScript has
// to survive: after a restart the spool is a directory on disk and the
// plan is a set of rows, and the code that has to run the next step needs
// the same guarantees the original run had. So a recovered plan goes
// through the same validation a fresh one does, and gets the same opaque
// type out -- there is no second, weaker Plan for recovery.
type RecoveredPlan struct {
	RunID            string
	BackupSetID      model.BackupSetID
	Steps            []Step
	Env              Environment
	ResolvedPlanHash string
	ScriptSpoolRef   string
}

// RecoverPlan rebuilds a Plan from what was durably recorded, refusing
// anything that is not a plan this package could have produced.
//
// The environment comes back from the journal rather than from today's
// configuration file, which is the whole reason it is persisted: a run
// that was interrupted has to be recovered with the variables it was
// planned with, and an operator who edited (or deleted) the config in
// between must not silently change what the recovery executes. See
// EncodeEnvironment.
func RecoverPlan(rec RecoveredPlan) (Plan, error) {
	if err := validPathComponent("run id", rec.RunID); err != nil {
		return Plan{}, err
	}

	if rec.BackupSetID.IsZero() {
		return Plan{}, fmt.Errorf("%w: recovered run %q names no backup set", ErrPlan, rec.RunID)
	}

	if rec.ResolvedPlanHash == "" {
		return Plan{}, fmt.Errorf("%w: recovered run %q has no resolved plan hash; the hash is what makes a recovery reproducible", ErrPlan, rec.RunID)
	}

	if !filepath.IsAbs(rec.ScriptSpoolRef) {
		return Plan{}, fmt.Errorf(
			"%w: recovered run %q has script spool %q, which is not an absolute path",
			ErrPlan, rec.RunID, rec.ScriptSpoolRef)
	}

	plan := Plan{
		runID:            rec.RunID,
		backupSetID:      rec.BackupSetID,
		steps:            append([]Step(nil), rec.Steps...),
		env:              rec.Env,
		resolvedPlanHash: rec.ResolvedPlanHash,
		spoolRef:         filepath.Clean(rec.ScriptSpoolRef),
	}
	plan.scriptsDir = filepath.Join(plan.spoolRef, scriptsDirName)

	seenID := map[string]bool{}
	seenOrder := map[int]bool{}

	for i, s := range plan.steps {
		if err := s.Validate(); err != nil {
			return Plan{}, err
		}

		switch {
		case s.RunID != rec.RunID:
			return Plan{}, fmt.Errorf("%w: step %q belongs to run %q and was recovered as part of run %q", ErrPlan, s.ID, s.RunID, rec.RunID)
		case seenID[s.ID]:
			return Plan{}, fmt.Errorf("%w: run %q recovered two steps with id %q, which would share one spooled script", ErrPlan, rec.RunID, s.ID)
		case seenOrder[s.Order]:
			return Plan{}, fmt.Errorf("%w: run %q recovered two steps claiming order %d; the order IS the plan", ErrPlan, rec.RunID, s.Order)
		case i > 0 && s.Order < plan.steps[i-1].Order:
			return Plan{}, fmt.Errorf("%w: run %q recovered its steps out of order (%d after %d); the order is the execution sequence and a recovery that reads it wrongly runs an after hook before a before hook", ErrPlan, rec.RunID, s.Order, plan.steps[i-1].Order)
		case s.SpoolRef != filepath.Join(plan.scriptsDir, s.ID):
			return Plan{}, fmt.Errorf(
				"%w: recovered step %q names the spooled script %s, and run %q's spool holds it at %s",
				ErrPlan, s.ID, s.SpoolRef, rec.RunID, filepath.Join(plan.scriptsDir, s.ID))
		}

		seenID[s.ID] = true
		seenOrder[s.Order] = true
	}

	return plan, nil
}

// Snapshot builds the immutable plan for one run, and durably captures
// every script it will execute.
//
// On any refusal it removes the run directory IT CREATED, so a refused
// snapshot leaves nothing behind for a later recovery pass to find and
// misread as a plan. "It created" is the load-bearing half: the run
// directory is made with mkdirat, which fails rather than succeeding on a
// path that is already there, so this function never removes a directory
// it found -- another run's spool is what a recovery pass reads, and a
// refusal that deleted it would destroy the evidence of a run that is
// still open. The removal is best-effort and its failure is not
// reported: the refusal the caller is about to see is the more important
// one, and a leftover directory under the state dir with no journal row
// pointing at it is inert.
//
// On success the spool's directory entries are fsynced before this
// returns, because the caller's next act is to journal the plan and a
// crash between the two must not leave a run whose scripts have no names.
func Snapshot(req SnapshotRequest) (Plan, error) {
	if err := validPathComponent("run id", req.RunID); err != nil {
		return Plan{}, err
	}

	if req.BackupSetID.IsZero() {
		return Plan{}, fmt.Errorf("%w: run %q names no backup set", ErrPlan, req.RunID)
	}

	if req.Root.IsZero() {
		return Plan{}, fmt.Errorf("%w: run %q has no workflow root; a deployment with no root configured has no workflow runs", ErrPlan, req.RunID)
	}

	maxSize, err := boundedScriptSize(req.MaxScriptSize)
	if err != nil {
		return Plan{}, err
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = DefaultStepTimeout
	}
	if timeout < 0 {
		return Plan{}, fmt.Errorf("%w: a step timeout of %s is not a duration a hook can run for", ErrPlan, timeout)
	}

	stages, err := orderedStages(req.Stages)
	if err != nil {
		return Plan{}, err
	}

	dirs, err := createSpool(req.SpoolRoot, req.RunID)
	if err != nil {
		return Plan{}, err
	}
	defer dirs.close()

	plan, err := snapshotInto(req, stages, dirs, maxSize, timeout)
	if err != nil {
		dirs.removeAndClose()

		return Plan{}, err
	}

	// The directory entries are flushed before the plan is returned, so
	// that a caller journaling it is journaling something a crash cannot
	// take away. See spoolDirs.sync.
	if err := dirs.sync(); err != nil {
		dirs.removeAndClose()

		return Plan{}, err
	}

	return plan, nil
}

// snapshotInto is Snapshot's body once the request has been checked and
// the spool exists, split out so that every refusal inside it goes through
// the one caller that cleans the spool up.
func snapshotInto(req SnapshotRequest, stages []StageSpec, dirs *spoolDirs, maxSize int64, timeout time.Duration) (Plan, error) {
	plan := Plan{
		runID:       req.RunID,
		backupSetID: req.BackupSetID,
		env:         req.Env,
		spoolRef:    dirs.run.Name(),
		scriptsDir:  dirs.scripts.Name(),
	}

	captured := int64(0)

	for _, stage := range stages {
		dir, err := req.Root.ResolveStage(stage.Dir)
		if err != nil {
			return Plan{}, err
		}

		scripts, err := Discover(dir)
		if err != nil {
			return Plan{}, err
		}

		for _, script := range scripts {
			order := len(plan.steps)
			id := StepID(order, stage.Scope, stage.Phase, script.Name)

			connection := ""
			if script.Target == TargetRemote {
				if req.RemoteExecConnectionRef == "" {
					return Plan{}, fmt.Errorf(
						"%w: %s runs on the host this backup set pulls from, and no execution connection is configured for it. "+
							"Set the backup set's workflow remote_exec_connection_ref, or rename the script %s.%s.sh to run it on this backup server instead",
						ErrPlan, script.path,
						strings.TrimSuffix(script.Name, "."+string(TargetRemote)+".sh"), TargetLocal)
				}

				connection = req.RemoteExecConnectionRef
			}

			spooled, err := captureScript(script.path, dirs, id, maxSize)
			if err != nil {
				return Plan{}, err
			}

			// The aggregate bound, checked as the plan grows rather than
			// after it is built: the point of a bound is that the work
			// stops, and a plan refused after every byte of it has been
			// read and copied has already cost what the bound exists to
			// avoid. MaxScriptsPerStage bounds the COUNT; this bounds the
			// volume, which four stages of large-but-legal scripts can
			// reach without any single one being refused.
			captured += spooled.size
			if captured > MaxPlanBytes {
				return Plan{}, fmt.Errorf(
					"%w: the scripts in this plan come to more than the %d bytes one run may capture. A workflow is a handful of shell scripts; this much of it is something else, and copying it into the spool is work the backup window pays for",
					ErrScriptTooLarge, MaxPlanBytes)
			}

			plan.steps = append(plan.steps, Step{
				ID:                     id,
				RunID:                  req.RunID,
				Scope:                  stage.Scope,
				Phase:                  stage.Phase,
				Order:                  order,
				ScriptName:             script.Name,
				ScriptSHA256:           spooled.sha256,
				ScriptSize:             spooled.size,
				Target:                 script.Target,
				ExecutionConnectionRef: connection,
				Timeout:                timeout,
				SpoolRef:               spooled.path,
				State:                  StatePending,
			})
		}
	}

	for i := range plan.steps {
		if err := plan.steps[i].Validate(); err != nil {
			return Plan{}, err
		}
	}

	plan.resolvedPlanHash = plan.hash(req)

	return plan, nil
}

// boundedScriptSize resolves the configurable size bound, refusing one
// that has been configured past the ceiling. See
// MaxConfigurableScriptSize for why there is a ceiling at all.
func boundedScriptSize(configured int64) (int64, error) {
	switch {
	case configured == 0:
		return DefaultMaxScriptSize, nil
	case configured < 0:
		return 0, fmt.Errorf("%w: a maximum script size of %d bytes is not a size", ErrPlan, configured)
	case configured > MaxConfigurableScriptSize:
		return 0, fmt.Errorf(
			"%w: the maximum script size is configured at %d bytes, above the %d-byte ceiling. The bound is adjustable so a legitimately large hook is not refused; it is capped so a deployment cannot configure the protection away",
			ErrPlan, configured, MaxConfigurableScriptSize)
	default:
		return configured, nil
	}
}

// orderedStages checks the stage list and returns it unchanged.
//
// It does not sort: the ORDER IS THE CALLER'S, because it is a product
// decision (PlanStages) rather than a property of the data, and a function
// here that re-derived it would be a second opinion about the one thing
// every hook author's mental model depends on.
func orderedStages(stages []StageSpec) ([]StageSpec, error) {
	seen := map[StageSpec]bool{}

	for _, s := range stages {
		if !s.Scope.Valid() {
			return nil, vocabularyError("stage scope", s.Scope, Scopes())
		}

		if !s.Phase.Valid() {
			return nil, vocabularyError("stage phase", s.Phase, Phases())
		}

		if s.Dir == "" {
			return nil, fmt.Errorf(
				"%w: the %s %s stage was passed with no directory. An unconfigured stage is omitted from the plan entirely, never included as an empty one",
				ErrPlan, s.Scope, s.Phase)
		}

		key := StageSpec{Scope: s.Scope, Phase: s.Phase}
		if seen[key] {
			return nil, fmt.Errorf("%w: the %s %s stage is configured twice, and there is no defined order between two directories for one stage", ErrPlan, s.Scope, s.Phase)
		}
		seen[key] = true
	}

	return stages, nil
}

// capturedScript is what one file contributed to the plan.
type capturedScript struct {
	sha256 string
	size   int64
	path   string
}

// captureScript is the one place a script's bytes are read, and it is
// written to be raceable against nothing.
//
// The Lstat first is only for the MESSAGE: a symbolic link where a script
// belongs is an operator situation that deserves a sentence naming the
// target, and O_NOFOLLOW's ELOOP is not that sentence. The Lstat's answer
// is then never trusted for anything -- the open below carries O_NOFOLLOW,
// so if the path became a link between the two calls the open fails, and
// every property that matters (regular file, mode, size, content, hash) is
// read from the resulting descriptor rather than from the path.
//
// O_NONBLOCK is internal/secretref's precaution for the same reason it
// takes it: a fifo or a character device left where the file should be
// would otherwise make this open BLOCK until somebody wrote to the other
// end. The refusal below is what rejects it; the flag is what makes sure
// the refusal is reached at all.
func captureScript(src string, dirs *spoolDirs, stepID string, maxSize int64) (capturedScript, error) {
	if li, err := os.Lstat(src); err == nil && li.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(src)
		if rerr != nil {
			target = "a target that cannot be read"
		}

		return capturedScript{}, fmt.Errorf(
			"%w: %s is a symbolic link to %s. The directories protecting the link say nothing about the ones protecting the file it points at, so this product will not execute what it finds through one; put the script itself in the hook directory",
			ErrCustody, src, target)
	}

	f, err := os.OpenFile(src, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return capturedScript{}, fmt.Errorf("%w: %s cannot be opened: %w", ErrCustody, src, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	info, err := f.Stat()
	if err != nil {
		return capturedScript{}, fmt.Errorf("%w: %s cannot be inspected: %w", ErrCustody, src, err)
	}

	if !info.Mode().IsRegular() {
		return capturedScript{}, fmt.Errorf(
			"%w: %s is a %s, not a regular file, so its contents are supplied by whoever is on the other end of it",
			ErrCustody, src, fileKind(info.Mode()))
	}

	// 0o022, not secretref's 0o077. A world-readable script is fine; a
	// world-WRITABLE one is a program any local account can change
	// between now and the next backup. See discover.go's preamble.
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return capturedScript{}, fmt.Errorf(
			"%w: %s has permissions %04o, which lets an account other than its owner rewrite it. This file is executed by this daemon, so its content has to be something only its owner can change; correct it (chmod go-w %s)",
			ErrCustody, src, mode, src)
	}

	// And who that owner is, which the mode does not say. A 0755 script
	// owned by an unaudited service account is a program that account can
	// rewrite between tonight and tomorrow night; see checkOwnership.
	if err := checkOwnership(src, info); err != nil {
		return capturedScript{}, err
	}

	if info.Size() > maxSize {
		return capturedScript{}, fmt.Errorf(
			"%w: %s is %d bytes and the limit is %d. A hook is a shell script; this product refuses the whole file rather than executing a prefix of it, because a prefix of a program is a different program",
			ErrScriptTooLarge, src, info.Size(), maxSize)
	}

	// LimitReader at maxSize+1 so a file that GREW between the fstat and
	// the read is refused rather than silently truncated. The size check
	// above is what produces the good message in the ordinary case; this
	// is what makes the bound true regardless.
	body, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return capturedScript{}, fmt.Errorf("%w: %s cannot be read: %w", ErrCustody, src, err)
	}

	if int64(len(body)) > maxSize {
		return capturedScript{}, fmt.Errorf(
			"%w: %s grew past the %d-byte limit while it was being read",
			ErrScriptTooLarge, src, maxSize)
	}

	sum := sha256.Sum256(body)

	dst, err := dirs.writeScript(stepID, body)
	if err != nil {
		return capturedScript{}, err
	}

	return capturedScript{sha256: hex.EncodeToString(sum[:]), size: int64(len(body)), path: dst}, nil
}

// planHashVersion prefixes the canonical form, so that a future change to
// what the hash covers is a visibly different hash rather than a silent
// collision in somebody's records.
const planHashVersion = "backupd/workflow-plan/2"

// hash computes ResolvedPlanHash over a canonical, line-oriented rendering
// of the plan.
//
// A hand-written text encoding rather than JSON or a Go fmt of the struct,
// for one reason: this value is compared across releases, so what it
// covers has to be a decision somebody made and can read, not a
// consequence of which fields a marshaller happened to include. Adding a
// field to Step must not silently move every deployment's plan hash.
//
// Every field is LENGTH-PREFIXED (see writeRecord), and version 2 of this
// encoding exists because version 1 was not: it joined fields with tabs
// and terminated records with newlines, on the argument that every value
// reaching it had been validated free of control characters. That
// argument was wrong about exactly one class of value, and it is the class
// an operator controls -- an environment variable's literal VALUE, a
// secret file's PATH and a secret command's ARGV are all arbitrary bytes,
// tabs and newlines included. So
//
//	{"A": "x\nenv\tB\tliteral\ty"}
//
// and
//
//	{"A": "x", "B": "y"}
//
// rendered to the same bytes and therefore to the same plan hash: two
// materially different environments that this product would have reported
// as "nothing about what we execute has changed". A length prefix makes
// the encoding injective, which is the only property a fingerprint needs
// and the one it did not have.
func (p Plan) hash(req SnapshotRequest) string {
	h := sha256.New()
	h.Write([]byte(p.canonical(req))) //nolint:errcheck // hash.Hash never errors

	return hex.EncodeToString(h.Sum(nil))
}

// canonical renders the plan's hashable form.
//
// What is IN it: the set, the stage list as configured, and for each step
// its order, scope, phase, name, target, size, content hash, timeout and
// execution connection; then every environment variable by name, with
// literals by value and secrets by LOCATION.
//
// What is deliberately OUT of it, each for its own reason:
//
//   - the run id and the spool paths, which differ on every run by
//     construction, and whose inclusion would make the hash unable to
//     answer the one question it exists for ("has anything about what we
//     execute changed since last night");
//   - the timestamps, for the same reason;
//   - every step's mutable state, because the hash describes the plan and
//     not its progress;
//   - any resolved secret material. A hash over a credential is a
//     credential oracle, and the plan's own contract is that it carries
//     locations rather than values.
func (p Plan) canonical(req SnapshotRequest) string {
	var b strings.Builder

	b.WriteString(planHashVersion)
	b.WriteString("\n")

	writeRecord(&b, "set", p.backupSetID.String())

	for _, s := range req.Stages {
		writeRecord(&b, "stage", string(s.Scope), string(s.Phase), s.Dir)
	}

	for _, s := range p.steps {
		writeRecord(&b, "step",
			strconv.Itoa(s.Order),
			string(s.Scope),
			string(s.Phase),
			s.ScriptName,
			string(s.Target),
			strconv.FormatInt(s.ScriptSize, 10),
			s.ScriptSHA256,
			strconv.FormatInt(int64(s.Timeout), 10),
			s.ExecutionConnectionRef,
		)
	}

	for _, v := range p.env.Vars() {
		if v.IsSecret() {
			writeRecord(&b, append([]string{"env", v.Name, "secret"}, secretLocation(v.Secret)...)...)

			continue
		}

		writeRecord(&b, "env", v.Name, "literal", v.Value)
	}

	return b.String()
}

// writeRecord appends one length-prefixed record.
//
// The form is a field COUNT, then each field as its byte length and its
// bytes, each prefix terminated by a colon:
//
//	3:3:env1:A22:x\nenv\tB\tliteral\ty
//
// Nothing in a field can be mistaken for a delimiter, because the reader
// never looks for one: it is told how much to take. That is what makes
// this encoding injective, and the count is part of it -- without it, a
// record of two fields and a record of one field whose value happened to
// contain the second field's prefix would render alike, which is the same
// bug one level up. It also means a secret's argv can be written as its
// own fields rather than joined with a separator no argument may contain
// (there is no such byte).
//
// The newline at the end is for a human reading the canonical form during
// a debugging session. It carries no meaning for the parse.
func writeRecord(b *strings.Builder, fields ...string) {
	b.WriteString(strconv.Itoa(len(fields)))
	b.WriteString(":")

	for _, f := range fields {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteString(":")
		b.WriteString(f)
	}

	b.WriteString("\n")
}

// secretLocation renders WHERE a secret comes from as the fields of a
// hash record: stable across runs, and containing no material.
// secretref.Ref is documented as safe to log, and this is the same claim:
// a file path, a variable name, or an argv.
//
// A fourth source appearing on secretref.Ref has to appear here too, or a
// plan hash would stop distinguishing two plans that differ in where a
// secret comes from. TestPlanHashCoversEverySecretSource is what fails
// when it does not.
func secretLocation(ref secretref.Ref) []string {
	switch {
	case ref.File != "":
		return []string{"file", ref.File}
	case ref.Env != "":
		return []string{"env", ref.Env}
	case len(ref.Command) != 0:
		// Each argument is its own field rather than a join: an argv is
		// arbitrary bytes, so any separator chosen here would be one two
		// different argvs could render alike through. writeRecord's
		// length prefixes are what make the boundaries unambiguous.
		return append([]string{"command"}, ref.Command...)
	default:
		return []string{"none"}
	}
}
