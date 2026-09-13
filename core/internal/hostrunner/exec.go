package hostrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Running one captured script, and the four things that make it a
// workflow runner rather than an exec wrapper.
//
// # 1. The bytes are proved before anything is created
//
// Size and sha256 are re-checked here against what the request claimed,
// before a directory exists, before a file is written, before bash is
// invoked. internal/workflow already verified the same bytes against the
// plan when it opened them (Plan.OpenScript), so this is the second check
// of the same fact -- deliberately, because the hop in between is a
// socket, and a check on the far side of a boundary is not a check on
// this side of it.
//
// # 2. The script is this process's private file, not the hook's
//
// The verified bytes are written to <run>/<step>.script, mode 0500, a
// SIBLING of the working directory rather than a file in it. bash reads a
// script incrementally, so a script the running hook could rewrite is a
// script whose second half is not the half that was hashed. The hook gets
// its own directory to write in, and does not get this file.
//
// This is the one documented divergence from core/internal/remoteexec
// (#810), which sends the bytes on stdin because it may leave no residue
// on a remote host. Locally, stdin is worth more than the temporary file
// costs: a hook that reads stdin (a `while read` over a list the operator
// pipes in through their own mechanism) works here, and would silently
// get the script's own text as its input if the script arrived that way.
//
// # 3. One session per script
//
// Setsid puts the child in a new session and a new process group of its
// own. Everything the hook starts inherits that group unless it goes out
// of its way not to, so "kill the hook" is one signal to the negated
// group id rather than a best-effort walk of a process tree that is
// changing while it is walked.
//
// Without it, killing the child kills the child: a hook that ran
// `pg_dump | gzip > x` leaves both halves running, still holding the
// database connection the timeout was supposed to release.
//
// # 4. Termination is proved, not assumed
//
// SIGTERM, then a grace period, then SIGKILL, then a LOOK. The look is
// the part that is easy to leave out and the part the journal actually
// records (internal/workflow's Step.TerminationConfirmed): a process
// group that is still there after SIGKILL is a process stuck in
// uninterruptible I/O, which does happen to a hook talking to a NAS
// share, and reporting that honestly is the difference between "the hook
// was killed" and "we stopped waiting for the hook".

// DefaultStepTimeout mirrors internal/workflow's default for a step whose
// request carries no bound. The engine normally resolves the timeout at
// snapshot time and sends it; this is the floor for a request that does
// not, because "no timeout" is not a thing this package will do.
const DefaultStepTimeout = 5 * time.Minute

// DefaultGracePeriod is how long a signalled process group has to exit
// before SIGKILL.
//
// Five seconds. Long enough for a trap handler to unmount something or
// release a database lock, which is the reason SIGTERM is sent first at
// all, and short enough that a cancelled backup does not sit waiting on a
// hook that is never going to handle the signal.
const DefaultGracePeriod = 5 * time.Second

// killConfirmWindow is how long this runner watches for a signalled
// process group to disappear after SIGKILL before reporting the
// termination as unconfirmed.
const killConfirmWindow = 2 * time.Second

// killPollInterval is how often the process group is probed while
// waiting for it to go.
const killPollInterval = 20 * time.Millisecond

// Executor runs verified bytes. One per server; it holds no per-step
// state, which is what lets several steps run at once without them
// sharing anything but the layout.
type Executor struct {
	// Layout is where working directories and scripts go.
	Layout Layout

	// Bash is the interpreter fixed at preflight.
	Bash Bash

	// Grace is how long a signalled group has before SIGKILL. Zero takes
	// DefaultGracePeriod.
	Grace time.Duration

	// MaxScriptSize bounds one script's bytes. Zero takes
	// MaxFrameSize's practical equivalent by way of the frame bound, so
	// a server that does not set it is still bounded; setting it holds
	// the runner to the same ceiling internal/workflow configures.
	MaxScriptSize int64
}

// Verify re-checks a request's bytes against what it claims, and its ids
// against what may become a path component.
//
// It is separate from Execute so that syntax-check runs the same
// validation without creating anything, and so a test can ask the
// question directly.
func (e *Executor) Verify(req Request) error {
	if err := ValidID("run id", req.RunID); err != nil {
		return &Failure{Code: CodeRefused, Message: err.Error()}
	}
	if err := ValidID("step id", req.StepID); err != nil {
		return &Failure{Code: CodeRefused, Message: err.Error()}
	}
	if len(req.Script) == 0 {
		return &Failure{Code: CodeScriptMismatch, Message: "the request carried no script bytes, and this runner has no other way to obtain them"}
	}
	max := e.MaxScriptSize
	if max <= 0 {
		max = MaxFrameSize
	}
	if int64(len(req.Script)) > max {
		return &Failure{
			Code:    CodeRefused,
			Message: fmt.Sprintf("the script is %d bytes, past this runner's %d-byte bound", len(req.Script), max),
		}
	}
	if req.ScriptSize != int64(len(req.Script)) {
		return &Failure{
			Code:    CodeScriptMismatch,
			Message: fmt.Sprintf("the request declares a %d-byte script and carries %d bytes", req.ScriptSize, len(req.Script)),
		}
	}
	sum := sha256.Sum256(req.Script)
	got := hex.EncodeToString(sum[:])
	if got != req.ScriptSHA256 {
		return &Failure{
			Code:    CodeScriptMismatch,
			Message: fmt.Sprintf("the request declares sha256 %s and the bytes hash to %s, so these are not the bytes the plan captured", req.ScriptSHA256, got),
		}
	}
	return nil
}

// Execute runs one step to completion and streams its output.
//
// ctx is the LEASE as well as the cancellation: the server cancels it
// when the engine's connection goes away, and this function treats that
// exactly as it treats an explicit cancel -- signal the process group,
// prove it is gone, clean up. See Server.handleExecute.
//
// The returned error is a *Failure when the step could not be attempted.
// A hook that exits non-zero is a Result with StateExited and a non-nil
// ExitCode, not an error: "the hook failed" and "we never ran the hook"
// are different answers and the workflow engine branches on which.
func (e *Executor) Execute(ctx context.Context, req Request, sink Sink) (Result, error) {
	if err := e.Verify(req); err != nil {
		return Result{}, err
	}
	if err := e.Bash.SyntaxCheck(ctx, req.Script); err != nil {
		return Result{}, err
	}

	workDir, scriptPath, err := e.Layout.prepareStep(req.RunID, req.StepID, req.Script)
	if err != nil {
		if errors.Is(err, ErrLayout) {
			return Result{}, &Failure{Code: CodeRefused, Message: err.Error()}
		}
		return Result{}, &Failure{Code: CodeInternal, Message: err.Error()}
	}

	// BACKUPD_WORK_DIR is appended LAST and therefore wins, whatever the
	// engine sent. Only this process knows the directory it just
	// created, so only this process is in a position to state it; an
	// engine-supplied value would be a path to somewhere else, and a
	// hook writing its dump there would write it outside the directory
	// this runner cleans up.
	env := EnvSet{Vars: append(append([]EnvVar(nil), req.Env.Vars...), EnvVar{Name: "BACKUPD_WORK_DIR", Value: workDir})}
	block, err := env.ProcessEnv(nil)
	if err != nil {
		e.cleanupStep(req.RunID, req.StepID)
		return Result{}, &Failure{Code: CodeRefused, Message: err.Error()}
	}

	timeout := req.Timeout()
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}

	result, err := e.run(ctx, runSpec{
		scriptPath: scriptPath,
		workDir:    workDir,
		env:        block,
		timeout:    timeout,
	}, sink)
	if err != nil {
		// Even a failed operation obeys the invariant below. run
		// returns a POPULATED result alongside its error when the
		// engine's connection died mid-stream, and that path reaches
		// here having already signalled a process group: removing a
		// working directory whose termination could not be confirmed
		// would be the exact deletion the next paragraph refuses.
		if result.TerminationCertainty != CertaintyUnconfirmed {
			e.cleanupStep(req.RunID, req.StepID)
		}
		return Result{}, err
	}
	result.DroppedEnvNames = req.Env.DroppedEnvNames()

	// "Removed after the step WHEN SAFE" (#809). Unsafe means exactly
	// one thing: this runner signalled a process group and could not
	// prove it was gone, so something may still be writing in there.
	// Removing it anyway would turn a hook that survived its own kill
	// into a half-written dump in a directory nobody can find.
	if result.TerminationCertainty != CertaintyUnconfirmed {
		e.cleanupStep(req.RunID, req.StepID)
		result.WorkDirRemoved = true
	}
	return result, nil
}

// runSpec is one exec's inputs, gathered so run's signature does not grow
// five parameters of the same type.
type runSpec struct {
	scriptPath string
	workDir    string
	env        []string
	timeout    time.Duration
}

// run is the exec itself: start, pump, wait, and terminate on whichever
// of the three endings arrives first.
func (e *Executor) run(ctx context.Context, spec runSpec, sink Sink) (Result, error) {
	// NOT exec.CommandContext. Its cancellation kills the CHILD, and
	// this package's whole termination argument is about the process
	// GROUP: a context kill would leave the hook's own children running
	// and would do it silently, since the Result would still say
	// "canceled". The signalling below is done here instead.
	cmd := exec.Command(e.Bash.Path, "--noprofile", "--norc", spec.scriptPath)
	cmd.Dir = spec.workDir
	cmd.Env = spec.env

	// A new session, so the child leads its own process group and
	// nothing it starts shares this daemon's. Setctty is deliberately
	// absent: there is no controlling terminal, which is what makes a
	// hook that tries to prompt fail fast instead of blocking forever.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// nil Stdin is /dev/null. A hook that reads stdin gets EOF rather
	// than this daemon's own input, and cannot block the run waiting for
	// something nobody is going to type.
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, &Failure{Code: CodeInternal, Message: "this runner could not open a pipe for the hook's stdout: " + err.Error()}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, &Failure{Code: CodeInternal, Message: "this runner could not open a pipe for the hook's stderr: " + err.Error()}
	}

	capture := NewCapture(sink)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, &Failure{Code: CodeInternal, Message: fmt.Sprintf("this runner could not start %s: %v", e.Bash.Path, err)}
	}
	pgid := cmd.Process.Pid

	var pumps sync.WaitGroup
	pumps.Add(2)
	go pump(&pumps, stdout, capture.Writer(StreamStdout))
	go pump(&pumps, stderr, capture.Writer(StreamStderr))

	waited := make(chan error, 1)
	go func() {
		// The pipes are drained BEFORE Wait, which is what
		// StdoutPipe's own contract requires: Wait closes them, and a
		// read racing that close loses the tail of a hook's output.
		//
		// The consequence, stated because it looks like a bug the first
		// time it is met: a hook that leaves a background process
		// holding its stdout keeps this pipe open after the hook itself
		// exits, so the step runs until its timeout and is then killed
		// as a group. Every shell, every CI runner and every
		// command-substitution in bash behaves the same way for the same
		// reason, and the alternative -- returning while something is
		// still writing output nobody is reading -- is the runaway this
		// package exists to prevent.
		pumps.Wait()
		waited <- cmd.Wait()
	}()

	timer := time.NewTimer(spec.timeout)
	defer timer.Stop()

	var (
		state     = StateExited
		certainty = CertaintyNotApplicable
		waitErr   error
	)
	select {
	case waitErr = <-waited:
	case <-timer.C:
		state = StateTimedOut
		waitErr, certainty = e.killGroup(pgid, waited)
	case <-ctx.Done():
		state = StateCanceled
		if errors.Is(context.Cause(ctx), errLeaseExpired) {
			state = StateLeaseExpired
		}
		waitErr, certainty = e.killGroup(pgid, waited)
	}

	result := Result{
		State:                state,
		TerminationCertainty: certainty,
		DurationMS:           time.Since(started).Milliseconds(),
		Chunks:               capture.Seq(),
	}

	if state == StateExited {
		var exitErr *exec.ExitError
		switch {
		case waitErr == nil:
			code := 0
			result.ExitCode = &code
		case errors.As(waitErr, &exitErr):
			// A negative code is a process killed by a signal this
			// runner did not send -- the OOM killer, or an operator with
			// a terminal. There is no exit status to report and
			// inventing 128+n would make it indistinguishable from a
			// hook that returned that number itself, so ExitCode stays
			// nil, which internal/workflow's Step.ExitCode already
			// documents as "no process status was observed".
			if code := exitErr.ExitCode(); code >= 0 {
				result.ExitCode = &code
			}
		default:
			return Result{}, &Failure{Code: CodeInternal, Message: "this runner could not collect the hook's exit status: " + waitErr.Error()}
		}
	}

	// A sink that failed is reported over the process's own outcome: the
	// engine did not receive the output, so a Result claiming a clean
	// stream would be a lie about evidence rather than about the hook.
	if err := capture.Err(); err != nil {
		return result, &Failure{Code: CodeInternal, Message: "this runner lost the hook's output stream: " + err.Error()}
	}
	return result, nil
}

func (e *Executor) grace() time.Duration {
	if e.Grace > 0 {
		return e.Grace
	}
	return DefaultGracePeriod
}

// pump copies one pipe into one capture writer in MaxChunkSize pieces.
func pump(wg *sync.WaitGroup, r io.Reader, w io.Writer) {
	defer wg.Done()
	buf := make([]byte, MaxChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// A sink error is not a reason to stop reading: the child
			// writes into a pipe with a fixed capacity, and a reader
			// that walks away leaves it blocked in write() forever,
			// which turns a lost connection into a wedged process the
			// timeout then has to kill. Keep draining; Capture.Err
			// remembers the failure.
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// cleanupStep removes one step's working directory and script, and the
// run directory once it is empty.
//
// Best effort, and silent. It runs on paths this process created, under a
// directory it owns, and its failure is never the most interesting thing
// that happened to a step: a caller that reported "the hook succeeded but
// the working directory could not be removed" as an error would fail runs
// for a full disk. What it must not do is remove something it did not
// create, which is why the whole removal happens inside Layout.removeStep
// -- validated ids, and descriptors that cannot be walked out of the
// workspace -- rather than through string concatenation and os.RemoveAll.
func (e *Executor) cleanupStep(runID, stepID string) {
	e.Layout.removeStep(runID, stepID)
}

// errLeaseExpired is the cancellation cause the server uses when the
// engine's connection went away, so that the Result says lease_expired
// rather than canceled. The two are the same kill and a different story:
// one is an operator or a timeout upstream, the other is an engine that
// is no longer there to be told.
var errLeaseExpired = errors.New("hostrunner: the engine's lease expired")

// killGroup terminates a step's process group and reports whether it is
// provably gone, in the ONE order that can answer that question.
//
// The order is the whole content of this function. Signal, then WAIT FOR
// THE LEADER TO BE REAPED, then probe -- because a process that has
// exited but not been waited on is a zombie, a zombie is still a member
// of its process group, and kill(-pgid, 0) cannot tell one from a
// running process. A probe taken before cmd.Wait therefore reports
// "still there" for every termination, including the ones that worked
// perfectly, and this runner would record TerminationConfirmed=false on
// every timeout in the product's history.
//
// The signal goes to -pgid, which is the point of the Setsid in run: it
// reaches the hook AND everything the hook started, in one call, with no
// window in which a new child is spawned between a walk and a kill.
//
// SIGTERM first, with a grace period, so a hook that traps it can release
// a database lock or unmount a snapshot -- which is the reason a hook
// exists at all. SIGKILL after, because a grace period nobody enforces is
// a hang.
//
// SIGKILL after BOTH ways of arriving at an unconfirmed group, which is
// the part that is easy to miss. The leader being reaped does not mean
// the group is empty: a hook that started a child which ignores SIGTERM
// -- a `while :; do :; done` under `trap ” TERM`, or any daemon that
// traps it to finish work first -- exits itself and leaves that child
// holding the group. Reporting "unconfirmed" there and stopping would
// leave a process running forever with nothing left that knows its pgid,
// on a host where the operator's only clue is a load average. So the
// probe failing is followed by the kill it exists to justify.
func (e *Executor) killGroup(pgid int, waited chan error) (error, TerminationCertainty) {
	if pgid <= 0 {
		return <-waited, CertaintyUnconfirmed
	}

	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case err := <-waited:
		if certainty := confirmGone(pgid, killConfirmWindow); certainty == CertaintyConfirmed {
			return err, certainty
		}
		// The leader is reaped and the group is not empty. Nothing is
		// left to wait for -- the survivors are not this process's
		// children -- so this is a signal and a second look, and the
		// answer after it is the honest one.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return err, confirmGone(pgid, killConfirmWindow)
	case <-time.After(e.grace()):
	}

	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	// No timeout on this receive, and that is not an oversight: SIGKILL
	// is not deliverable-or-not, and a leader that has been sent one and
	// is still not reaped is a process in uninterruptible I/O, which is
	// exactly the case where returning early would leave this runner
	// reporting a step as finished while its process is still writing to
	// the thing it was quiescing. It blocks, honestly, until the kernel
	// lets go.
	err := <-waited
	return err, confirmGone(pgid, killConfirmWindow)
}

// confirmGone watches a process group for a bounded time and reports
// whether it disappeared.
func confirmGone(pgid int, window time.Duration) TerminationCertainty {
	if groupGone(pgid, window) {
		return CertaintyConfirmed
	}
	return CertaintyUnconfirmed
}

// groupGone polls until the process group has no members left, or the
// window elapses.
//
// kill(-pgid, 0) is the probe: it delivers nothing and answers ESRCH when
// no process is in the group. The leader is reaped by cmd.Wait before
// this is called in the paths that matter, because a zombie is still a
// member of its group and would make every termination look unconfirmed.
//
// A pgid can in principle be recycled by the kernel between the last
// member exiting and this probe, which would report a group that is gone
// as still present. That is a false UNCONFIRMED -- the safe direction,
// and the reason this is a poll of a short window rather than a single
// answer nobody re-checks.
func groupGone(pgid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(killPollInterval)
	}
}
