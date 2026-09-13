package hostrunner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Running one captured script in an ephemeral container, and the five
// things that make it a workflow runner rather than a `docker run`
// wrapper.
//
// # 1. The bytes are proved before anything is created
//
// Size and sha256 are re-checked here against what the request claimed,
// before a directory exists, before a file is written, before a container
// is started. internal/workflow already verified the same bytes against
// the plan when it opened them (Plan.OpenScript), so this is the second
// check of the same fact -- deliberately, because the hop in between is a
// socket, and a check on the far side of a boundary is not a check on
// this side of it.
//
// # 2. The script is this process's private file, mounted read-only
//
// The verified bytes are written to <run>/<step>.script, mode 0500, a
// SIBLING of the working directory rather than a file in it, and the
// container gets it as a READ-ONLY bind mount at the same path. bash
// reads a script incrementally, so a script the running hook could
// rewrite is a script whose second half is not the half that was hashed.
// The hook gets its own directory to write in; the script is mounted in a
// way the kernel refuses to let it write, which is stronger than the file
// mode was (a hook running as the same account could have chmod'ed its
// own copy).
//
// This is the one documented divergence from core/internal/remoteexec
// (#810), which sends the bytes on stdin because it may leave no residue
// on a remote host. Locally, stdin is worth more than the temporary file
// costs: a hook that reads stdin (a `while read` over a list the operator
// pipes in through their own mechanism) works here, and would silently
// get the script's own text as its input if the script arrived that way.
//
// # 3. One container per script, and this runner owns its name
//
// The container's name is minted HERE, before it exists
// (containerName), and everything that happens to it afterwards happens
// by that name. That is the container-era replacement for the setsid
// process group this file used to create, and it is strictly stronger in
// the case that mattered: a hook that ran `pg_dump | gzip > x` left both
// halves running when only the child was killed, and a hook that
// re-parents or changes its process group escaped a group-wide signal
// entirely. A container's cgroup holds everything the hook started, with
// no walk and no window.
//
// A name rather than the id docker prints: the id only exists once the
// container does, and a launch that was killed between fork and the
// client's first output would leave a container this runner could not
// address. The name is a fact before the container is.
//
// # 4. Termination is proved, not assumed
//
// SIGTERM to the container, then a grace period, then SIGKILL, then a
// LOOK -- and the look asks the DAEMON whether the container still
// exists. internal/workflow's Step.TerminationConfirmed is the field this
// feeds, and the distinction is worth the round trip: a container that is
// still there after a kill is a daemon that is wedged or a task in
// uninterruptible I/O, which does happen to a hook talking to a NAS
// share, and reporting that honestly is the difference between "the hook
// was killed" and "we stopped waiting for the hook".
//
// # 5. There is no path that does not go through a container
//
// Execute refuses when no capability was proven. It does not resolve a
// host bash, and this package no longer has the code to: see
// container.go's preamble for why a fallback would make every property
// in this file conditional on a daemon nobody checked.

// DefaultStepTimeout mirrors internal/workflow's default for a step whose
// request carries no bound. The engine normally resolves the timeout at
// snapshot time and sends it; this is the floor for a request that does
// not, because "no timeout" is not a thing this package will do.
const DefaultStepTimeout = 5 * time.Minute

// DefaultGracePeriod is how long a signalled container has to exit before
// it is killed.
//
// Five seconds. Long enough for a trap handler to unmount something or
// release a database lock, which is the reason SIGTERM is sent first at
// all, and short enough that a cancelled backup does not sit waiting on a
// hook that is never going to handle the signal.
const DefaultGracePeriod = 5 * time.Second

// killConfirmWindow is how long this runner watches for a killed
// container to disappear before reporting the termination as unconfirmed.
//
// Longer than the process-group window it replaces: this one includes a
// round trip to the daemon, and the daemon is the same one that is at
// that moment tearing the container down.
const killConfirmWindow = 10 * time.Second

// killPollInterval is how often the daemon is asked while waiting for a
// container to go.
const killPollInterval = 100 * time.Millisecond

// Executor runs verified bytes in ephemeral containers. One per server;
// it holds no per-step state, which is what lets several steps run at
// once without them sharing anything but the layout.
type Executor struct {
	// Layout is where working directories and scripts go.
	Layout Layout

	// Container is the capability proved at startup: which client, which
	// daemon, which image, which interpreter inside it. A zero value is
	// a runner that cannot execute anything, and says so.
	Container Container

	// Grace is how long a signalled container has before it is killed.
	// Zero takes DefaultGracePeriod.
	Grace time.Duration

	// ConfirmWindow is how long the daemon is asked about a killed
	// container before its termination is reported as unconfirmed. Zero
	// takes killConfirmWindow.
	//
	// It is a field rather than a constant because it is the one
	// duration a test has to WAIT OUT: the behaviour under assertion is
	// the answer after the window, never its length, and a suite that
	// endured ten seconds per unconfirmed case would be ten seconds
	// somebody eventually deletes the test to get back.
	ConfirmWindow time.Duration

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

// Execute runs one step to completion in its own container and streams
// its output.
//
// ctx is the LEASE as well as the cancellation: the server cancels it
// when the engine's connection goes away, and this function treats that
// exactly as it treats an explicit cancel -- signal the container, prove
// it is gone, remove it, clean up. See Server.handleExecute.
//
// The returned error is a *Failure when the step could not be attempted.
// A hook that exits non-zero is a Result with StateExited and a non-nil
// ExitCode, not an error: "the hook failed" and "we never ran the hook"
// are different answers and the workflow engine branches on which.
func (e *Executor) Execute(ctx context.Context, req Request, sink Sink) (Result, error) {
	if err := e.Verify(req); err != nil {
		return Result{}, err
	}
	// Before anything is created, because it is the answer with no
	// remedy the engine can apply mid-run: a host that cannot start a
	// container cannot run this hook, and pretending otherwise is the
	// silent host-bash fallback #865 forbids.
	if !e.Container.Available() {
		return Result{}, noCapability()
	}
	if err := e.Container.SyntaxCheck(ctx, req.Script); err != nil {
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
	//
	// It is the same string inside the container as outside, because the
	// mount is an identity mount: see Mount.
	env := EnvSet{Vars: append(append([]EnvVar(nil), req.Env.Vars...), EnvVar{Name: "BACKUPD_WORK_DIR", Value: workDir})}
	block, err := env.ProcessEnv(nil)
	if err != nil {
		e.cleanupStep(req.RunID, req.StepID)
		return Result{}, &Failure{Code: CodeRefused, Message: err.Error()}
	}

	// The two paths about to become bind mounts are checked one last
	// time, as LINKS rather than as paths. prepareStep created them
	// through descriptors that cannot be walked out of the workspace
	// (paths.go), but what is handed to the daemon is a STRING, and the
	// daemon resolves it itself, as root, with none of that discipline.
	// A symbolic link that appeared in between would be a mount of
	// wherever it points.
	if err := refuseLink(workDir); err != nil {
		e.cleanupStep(req.RunID, req.StepID)
		return Result{}, err
	}
	if err := refuseLink(scriptPath); err != nil {
		e.cleanupStep(req.RunID, req.StepID)
		return Result{}, err
	}

	timeout := req.Timeout()
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}

	name, err := containerName(req.RunID, req.StepID)
	if err != nil {
		e.cleanupStep(req.RunID, req.StepID)
		return Result{}, err
	}

	result, err := e.run(ctx, runSpec{
		name:       name,
		runID:      req.RunID,
		stepID:     req.StepID,
		scriptPath: scriptPath,
		workDir:    workDir,
		env:        block,
		timeout:    timeout,
	}, sink)
	if err != nil {
		// Even a failed operation obeys the invariant below. run
		// returns a POPULATED result alongside its error when the
		// engine's connection died mid-stream, and that path reaches
		// here having already signalled a container: removing a working
		// directory whose termination could not be confirmed would be
		// the exact deletion the next paragraph refuses.
		if result.TerminationCertainty != CertaintyUnconfirmed {
			e.cleanupStep(req.RunID, req.StepID)
		}
		return Result{}, err
	}
	result.DroppedEnvNames = req.Env.DroppedEnvNames()

	// "Removed after the step WHEN SAFE" (#809). Unsafe means exactly
	// one thing: this runner killed a container and could not prove it
	// was gone, so something may still be writing in there. Removing it
	// anyway would turn a hook that survived its own kill into a
	// half-written dump in a directory nobody can find.
	if result.TerminationCertainty != CertaintyUnconfirmed {
		e.cleanupStep(req.RunID, req.StepID)
		result.WorkDirRemoved = true
	}
	return result, nil
}

// refuseLink is the last look at a path before it becomes a bind mount.
func refuseLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return &Failure{Code: CodeInternal, Message: fmt.Sprintf("this runner could not check %s before mounting it: %v", path, err)}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &Failure{Code: CodeRefused, Message: fmt.Sprintf("%s is a symbolic link, and this runner will not hand the daemon a path whose meaning something else chose", path)}
	}
	return nil
}

// containerName mints the name this runner owns for one step's container.
//
// <prefix><run>-<step>-<8 random hex>, and every piece of it is load
// bearing. The prefix and the ids make an operator's `docker ps` legible.
// The random suffix is what makes the name UNIQUE rather than derived: a
// step that is retried after a termination this runner could not confirm
// would otherwise collide with the container it could not remove, and
// `docker run --name` fails on a collision -- turning "the last attempt
// left something behind" into "this step can never run again".
//
// The ids are already held to ValidID by Verify, whose alphabet (letters,
// digits, dot, dash, underscore) is a subset of what docker accepts in a
// name. The truncation is docker's 255-byte limit on a name against
// MaxIDLength twice over.
func containerName(runID, stepID string) (string, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", &Failure{Code: CodeInternal, Message: "this runner could not mint a container name: " + err.Error()}
	}
	name := containerNamePrefix + runID + "-" + stepID + "-" + hex.EncodeToString(random[:])
	if len(name) > 200 {
		name = name[:200]
	}
	return name, nil
}

// runSpec is one container's inputs, gathered so run's signature does not
// grow six parameters of the same type.
type runSpec struct {
	name       string
	runID      string
	stepID     string
	scriptPath string
	workDir    string
	env        []string
	timeout    time.Duration
}

// run is the launch itself: start the container, pump its two streams,
// wait, and terminate it on whichever of the three endings arrives first.
func (e *Executor) run(ctx context.Context, spec runSpec, sink Sink) (Result, error) {
	// NOT exec.CommandContext. Its cancellation kills the CLIENT, and
	// killing the docker client does not stop the container it started:
	// the container is the daemon's child, not this process's, so a
	// context kill would leave the hook running with nothing left that
	// knows its name, and the Result would still say "canceled". The
	// termination below is done through the daemon instead.
	cmd := exec.Command(e.Container.Docker, e.Container.hookArgs(launchSpec{
		name:       spec.name,
		runID:      spec.runID,
		stepID:     spec.stepID,
		workDir:    spec.workDir,
		scriptPath: spec.scriptPath,
		envNames:   envNames(spec.env),
	})...)

	// The client's environment IS the hook's environment, and that is
	// what makes `--env NAME` work: the client reads each named value
	// out of its own block and sends it to the daemon over the socket,
	// so no value ever appears in an argument vector, which on a NAS is
	// world-readable through `ps`.
	//
	// It is the whole block and nothing else -- not this process's
	// environment with the hook's merged in -- so a variable this
	// runner's own service manager set cannot leak into a hook. The
	// client's own settings travel as explicit flags for the same
	// reason, in the other direction: see Container.clientArgs.
	cmd.Env = spec.env

	// nil Stdin is /dev/null, and the launch does not ask for -i, so
	// the hook's stdin is closed inside the container too. A hook that
	// reads stdin gets EOF rather than this daemon's own input, and
	// cannot block the run waiting for something nobody is going to
	// type. No -t either, anywhere: see the probe's tty assertion.
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
		return Result{}, &Failure{Code: CodeInternal, Message: fmt.Sprintf("this runner could not start %s: %v", e.Container.Docker, err)}
	}

	var pumps sync.WaitGroup
	pumps.Add(2)
	// The two streams stay apart all the way from the container: docker
	// demultiplexes them for a launch with no PTY, which is the other
	// reason a PTY is refused -- it would merge them irrecoverably.
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
		// exits, so the step runs until its timeout and the container
		// is then killed with everything in it. Every shell, every CI
		// runner and every command-substitution in bash behaves the
		// same way for the same reason, and the alternative --
		// returning while something is still writing output nobody is
		// reading -- is the runaway this package exists to prevent.
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
		// The container removed itself (--rm) on the way out. The
		// removal is repeated anyway, because "--rm did not happen" is
		// exactly the case a leftover is: a daemon restart between the
		// exit and the removal leaves a dead container with this step's
		// name on it, and the next attempt at the same step would find
		// the name taken.
		e.sweep(spec.name)
	case <-timer.C:
		state = StateTimedOut
		waitErr, certainty = e.killContainer(spec.name, waited)
	case <-ctx.Done():
		state = StateCanceled
		if errors.Is(context.Cause(ctx), errLeaseExpired) {
			state = StateLeaseExpired
		}
		waitErr, certainty = e.killContainer(spec.name, waited)
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
			// The code is the CONTAINER's, as the client reports it:
			// the hook's own exit status, or 125 when the container
			// could not be created and 126/127 when the entrypoint
			// could not be run.
			//
			// One honest limit, recorded because it is a real
			// difference from the process this replaced: a hook killed
			// by something inside the container (the kernel's OOM
			// killer) arrives here as 137 rather than as "no status was
			// observed", because that is the only thing the client
			// reports. A hook that returned 137 itself is
			// indistinguishable from one that was killed. The states
			// this runner produces ITSELF -- timed out, canceled, lease
			// expired -- carry no exit code at all, so the distinction
			// that matters upstream is unaffected.
			if code := exitErr.ExitCode(); code >= 0 {
				result.ExitCode = &code
			}
			if code := exitErr.ExitCode(); code == 125 {
				// Not a hook that failed: a container that was never
				// created. Reporting it as an exit status would tell an
				// operator their script returned 125.
				return Result{}, &Failure{Code: CodeInternal, Message: fmt.Sprintf("the hook container could not be created in %s: %s", e.Container.Image, containerStartDetail(result.Chunks))}
			}
		default:
			return Result{}, &Failure{Code: CodeInternal, Message: "this runner could not collect the hook's exit status: " + waitErr.Error()}
		}
	}

	// A sink that failed is reported over the container's own outcome:
	// the engine did not receive the output, so a Result claiming a
	// clean stream would be a lie about evidence rather than about the
	// hook.
	if err := capture.Err(); err != nil {
		return result, &Failure{Code: CodeInternal, Message: "this runner lost the hook's output stream: " + err.Error()}
	}
	return result, nil
}

// containerStartDetail is what a failed creation has to say for itself.
//
// The client writes it on stderr, which the capture already streamed to
// the engine, so this is a POINTER at those bytes rather than a copy of
// them: they are in the step's log, and repeating them into a Failure
// message would put whatever the daemon said into every place a refusal
// is recorded -- including, for a bind-mount fault, a host path an
// operator's configuration named.
func containerStartDetail(chunks uint64) string {
	if chunks == 0 {
		return "the client said nothing"
	}
	return fmt.Sprintf("the client's own message is in this step's stderr (%d chunks)", chunks)
}

// envNames lists the names in a prepared environment block, in the order
// ProcessEnv put them, for the `--env NAME` flags.
func envNames(block []string) []string {
	names := make([]string, 0, len(block))
	for _, entry := range block {
		if name, _, ok := strings.Cut(entry, "="); ok {
			names = append(names, name)
		}
	}
	return names
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

// killContainer terminates a step's container and reports whether it is
// provably gone, in the ONE order that can answer that question.
//
// SIGTERM first, with a grace period, so a hook that traps it can release
// a database lock or unmount a snapshot -- which is the reason a hook
// exists at all. The signal goes to the CONTAINER, which means the
// container's own process: an operator's `trap ... TERM` runs exactly as
// it would if they had pressed Ctrl-C. Then SIGKILL, because a grace
// period nobody enforces is a hang -- and a SIGKILL to a container takes
// its whole cgroup, so the child that ignores SIGTERM, the child that
// changed its process group and the grandchild nobody knew about all go
// with it. That is the part the process-group signalling this replaced
// could not promise.
//
// Then the LOOK, and its position is the whole content of the rest of
// this function. It happens after the client process has been WAITED FOR,
// because until then `docker run --rm` has not had its chance to remove
// the container and every termination would look unconfirmed. And it asks
// the daemon, not the kernel: a container that is gone from `docker ps
// --all` is gone, while a client that exited proves only that a client
// exited.
//
// The removal at the end is why the lease is a guarantee rather than an
// intention. --rm covers the ordinary exit; a container that was killed
// while the daemon was busy, or whose client was itself killed, is
// removed here, by name. "An engine that went away leaves no runaway" is
// half of it; "and no leftover" is this call.
func (e *Executor) killContainer(name string, waited chan error) (error, TerminationCertainty) {
	_ = e.Container.signal(name, "TERM")

	var err error
	select {
	case err = <-waited:
		// The client returned, which for `--rm` normally means the
		// container is already gone. If it is not -- a daemon that has
		// not finished the teardown, or a container that outlived the
		// client that started it -- the kill it exists to justify
		// follows, exactly as the SIGTERM-then-look path did.
		if e.sweep(name) {
			return err, CertaintyConfirmed
		}
		_ = e.Container.signal(name, "KILL")
		return err, e.confirmGone(name)
	case <-time.After(e.grace()):
	}

	_ = e.Container.signal(name, "KILL")
	// No timeout on this receive, and that is not an oversight: a
	// container that has been sent SIGKILL and whose client is still
	// running is a task the daemon cannot reap, which is exactly the
	// case where returning early would leave this runner reporting a
	// step as finished while something is still writing to the thing it
	// was quiescing. It blocks, honestly, until the client lets go.
	err = <-waited
	return err, e.confirmGone(name)
}

// sweep removes a container by name and reports whether it is gone
// afterwards. It is the ordinary end of every step: --rm has usually done
// it already, and a removal that finds nothing is a success.
func (e *Executor) sweep(name string) bool {
	_ = e.Container.remove(name)
	return e.confirmGone(name) == CertaintyConfirmed
}

// confirmGone watches a container for a bounded time and reports whether
// the daemon stopped knowing about it.
//
// A daemon that cannot be asked answers UNCONFIRMED, which is the safe
// direction and the reason containerExists returns an error separately
// from its answer: reporting "gone" because a question failed is how a
// runaway gets recorded as a clean kill.
func (e *Executor) confirmGone(name string) TerminationCertainty {
	window := e.ConfirmWindow
	if window <= 0 {
		window = killConfirmWindow
	}
	deadline := time.Now().Add(window)
	for {
		exists, err := e.Container.containerExists(name)
		if err == nil && !exists {
			return CertaintyConfirmed
		}
		if !time.Now().Before(deadline) {
			return CertaintyUnconfirmed
		}
		time.Sleep(killPollInterval)
	}
}
