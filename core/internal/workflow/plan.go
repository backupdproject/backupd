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
	"syscall"
	"time"

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
// the workflow root again. Execution reads Step.SpoolRef. That is what
// makes the guarantee in #808's acceptance criteria true rather than
// hopeful: editing or deleting a script in /workflows after the snapshot
// cannot change what the run executes, and cannot change what a recovery
// of that run executes tomorrow after a restart, because the run's
// authority is a private copy under the state directory with modes
// (0700/0600) that say so.
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
	RunID       string
	BackupSetID model.BackupSetID

	// Steps are in execution order, with Order matching the index.
	Steps []Step

	// Env is the configured environment this plan resolves with,
	// carrying secret LOCATIONS and no material.
	Env Environment

	// ResolvedPlanHash fingerprints the decision. See this file's
	// preamble for what is in it and what is deliberately not.
	ResolvedPlanHash string

	// ScriptSpoolRef is the run-scoped spool directory.
	ScriptSpoolRef string
}

// Snapshot builds the immutable plan for one run, and durably captures
// every script it will execute.
//
// On any refusal it removes the spool it had begun to build, so a refused
// snapshot leaves nothing behind for a later recovery pass to find and
// misread as a plan. The removal is best-effort and its failure is not
// reported: the refusal the caller is about to see is the more important
// one, and a leftover directory under the state dir with no journal row
// pointing at it is inert.
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

	if !filepath.IsAbs(req.SpoolRoot) {
		return Plan{}, fmt.Errorf(
			"%w: the script spool root %q is not an absolute path. This process creates directories under it and later executes what it finds there",
			ErrPlan, req.SpoolRoot)
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

	spool := filepath.Join(req.SpoolRoot, req.RunID)
	scriptDir := filepath.Join(spool, "scripts")

	if err := makeProtectedDir(scriptDir); err != nil {
		return Plan{}, err
	}

	plan, err := snapshotInto(req, stages, scriptDir, maxSize, timeout)
	if err != nil {
		os.RemoveAll(spool) //nolint:errcheck // see Snapshot's doc: the refusal below is the report

		return Plan{}, err
	}

	plan.ScriptSpoolRef = spool

	return plan, nil
}

// snapshotInto is Snapshot's body once the request has been checked and
// the spool exists, split out so that every refusal inside it goes through
// the one caller that cleans the spool up.
func snapshotInto(req SnapshotRequest, stages []StageSpec, scriptDir string, maxSize int64, timeout time.Duration) (Plan, error) {
	plan := Plan{
		RunID:       req.RunID,
		BackupSetID: req.BackupSetID,
		Env:         req.Env,
	}

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
			order := len(plan.Steps)
			id := StepID(order, stage.Scope, stage.Phase, script.Name)

			connection := ""
			if script.Target == TargetRemote {
				if req.RemoteExecConnectionRef == "" {
					return Plan{}, fmt.Errorf(
						"%w: %s runs on the host this backup set pulls from, and no execution connection is configured for it. "+
							"Set the backup set's workflow remote_exec_connection_ref, or rename the script %s.%s.sh to run it on this backup server instead",
						ErrPlan, script.Path,
						strings.TrimSuffix(script.Name, "."+string(TargetRemote)+".sh"), TargetLocal)
				}

				connection = req.RemoteExecConnectionRef
			}

			captured, err := captureScript(script.Path, filepath.Join(scriptDir, id), maxSize)
			if err != nil {
				return Plan{}, err
			}

			plan.Steps = append(plan.Steps, Step{
				ID:                     id,
				RunID:                  req.RunID,
				Scope:                  stage.Scope,
				Phase:                  stage.Phase,
				Order:                  order,
				ScriptName:             script.Name,
				ScriptSHA256:           captured.sha256,
				ScriptSize:             captured.size,
				Target:                 script.Target,
				ExecutionConnectionRef: connection,
				Timeout:                timeout,
				SpoolRef:               captured.path,
				State:                  StatePending,
			})
		}
	}

	for i := range plan.Steps {
		if err := plan.Steps[i].Validate(); err != nil {
			return Plan{}, err
		}
	}

	plan.ResolvedPlanHash = plan.hash(req)

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
func captureScript(src, dst string, maxSize int64) (capturedScript, error) {
	if li, err := os.Lstat(src); err == nil && li.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(src)
		if rerr != nil {
			target = "a target that cannot be read"
		}

		return capturedScript{}, fmt.Errorf(
			"%w: %s is a symbolic link to %s. The directories protecting the link say nothing about the ones protecting the file it points at, so this product will not execute what it finds through one; put the script itself in the hook directory",
			ErrCustody, src, target)
	}

	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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

	if err := writeSpooledScript(dst, body); err != nil {
		return capturedScript{}, err
	}

	return capturedScript{sha256: hex.EncodeToString(sum[:]), size: int64(len(body)), path: dst}, nil
}

// makeProtectedDir creates a spool directory tree owner-only.
//
// The explicit Chmod after the MkdirAll is not redundant. MkdirAll applies
// the process umask, which can only REMOVE bits, so the result is never
// more permissive than 0700 -- but on a deployment running with umask 077
// it would be 0600, and a spool directory this process cannot traverse is
// a run that cannot execute. Setting the mode explicitly makes the
// directory exactly what it is documented to be, whatever the umask is.
func makeProtectedDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: creating the script spool at %s: %w", ErrPlan, dir, err)
	}

	// Both the leaf and its parent: MkdirAll may have created either or
	// both, and the parent is the directory that holds one run's whole
	// spool.
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("%w: protecting the script spool at %s: %w", ErrPlan, d, err)
		}
	}

	return nil
}

// writeSpooledScript writes one captured script into the spool,
// owner-readable and owner-writable and nothing else.
//
// O_EXCL: a spool file that already exists means either a run id was
// reused or something else is writing into this run's spool, and both are
// situations where overwriting is the wrong answer.
//
// Not executable, deliberately. Execution runs these through an
// interpreter with the script as an argument rather than relying on the
// file's exec bit, so the spooled copy never needs to be executable, and a
// file under the state directory that is not executable is one fewer thing
// a mistake elsewhere can turn into a running program.
func writeSpooledScript(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: writing the captured script to %s: %w", ErrPlan, path, err)
	}

	if _, err := f.Write(body); err != nil {
		f.Close() //nolint:errcheck // the write already failed

		return fmt.Errorf("%w: writing the captured script to %s: %w", ErrPlan, path, err)
	}

	// Explicit, for makeProtectedDir's umask reason: 0600 & ~umask can
	// only be narrower, and a spool file this process cannot read back is
	// a run that cannot execute.
	if err := f.Chmod(0o600); err != nil {
		f.Close() //nolint:errcheck // the chmod already failed

		return fmt.Errorf("%w: protecting the captured script at %s: %w", ErrPlan, path, err)
	}

	// Sync before Close, and check Close: this copy is the run's only
	// authority over what it executes, so "durably committed before any
	// hook may execute" has to include the bytes actually reaching the
	// disk rather than a page cache that a crash discards.
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck // the sync already failed

		return fmt.Errorf("%w: flushing the captured script at %s: %w", ErrPlan, path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: closing the captured script at %s: %w", ErrPlan, path, err)
	}

	return nil
}

// planHashVersion prefixes the canonical form, so that a future change to
// what the hash covers is a visibly different hash rather than a silent
// collision in somebody's records.
const planHashVersion = "backupd/workflow-plan/1"

// hash computes ResolvedPlanHash over a canonical, line-oriented rendering
// of the plan.
//
// A hand-written text encoding rather than JSON or a Go fmt of the struct,
// for one reason: this value is compared across releases, so what it
// covers has to be a decision somebody made and can read, not a
// consequence of which fields a marshaller happened to include. Adding a
// field to Step must not silently move every deployment's plan hash.
//
// Field separator is a tab and records are newline-terminated, which is
// unambiguous because every value that reaches here has already been
// validated as free of control characters (script names, ids) or is a
// number, an enum or a path this process built.
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

	writeRecord(&b, "set", p.BackupSetID.String())

	for _, s := range req.Stages {
		writeRecord(&b, "stage", string(s.Scope), string(s.Phase), s.Dir)
	}

	for _, s := range p.Steps {
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

	for _, v := range p.Env.Vars() {
		if v.IsSecret() {
			writeRecord(&b, "env", v.Name, "secret", secretLocation(v.Secret))

			continue
		}

		writeRecord(&b, "env", v.Name, "literal", v.Value)
	}

	return b.String()
}

func writeRecord(b *strings.Builder, fields ...string) {
	b.WriteString(strings.Join(fields, "\t"))
	b.WriteString("\n")
}

// secretLocation renders WHERE a secret comes from, in a form that is
// stable across runs and contains no material. secretref.Ref is
// documented as safe to log, and this is the same claim: a file path, a
// variable name, or an argv.
//
// A fourth source appearing on secretref.Ref has to appear here too, or a
// plan hash would stop distinguishing two plans that differ in where a
// secret comes from. TestPlanHashCoversEverySecretSource is what fails
// when it does not.
func secretLocation(ref secretref.Ref) string {
	switch {
	case ref.File != "":
		return "file=" + ref.File
	case ref.Env != "":
		return "env=" + ref.Env
	case len(ref.Command) != 0:
		return "command=" + strings.Join(ref.Command, "\x1f")
	default:
		return "none"
	}
}
