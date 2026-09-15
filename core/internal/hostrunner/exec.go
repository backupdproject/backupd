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
// # 3. One container per script, and this runner owns its identity
//
// The container's NAME and a token unique to this one launch are minted
// HERE, before anything exists (mintContainerIdentity), and the token
// goes onto the container as a LABEL when the daemon creates it. That
// pair is the container-era replacement for the setsid process group
// this file used to create, and it is strictly stronger in the two cases
// that mattered: a hook that ran `pg_dump | gzip > x` left both halves
// running when only the child was killed, and a hook that re-parents or
// changes its process group escaped a group-wide signal entirely. A
// container's cgroup holds everything the hook started, with no walk and
// no window.
//
// A name rather than the id docker prints, because the id only exists
// once the container does. And a LABEL as well as a name, because a name
// is what this runner believes it created while the label is what the
// daemon has: a creation that was cancelled on the way back leaves a
// container this runner never saw registered, and the label is how it is
// still found and killed (Executor.reconcile).
//
// # 4. Termination is proved, not assumed
//
// SIGTERM to the container, then a grace period, then SIGKILL, then a
// LOOK -- and the look asks the DAEMON what it still has carrying this
// launch's label. internal/workflow's Step.TerminationConfirmed is the
// field this feeds, and the distinction is worth the round trip: a
// container that is still there after a kill is a daemon that is wedged
// or a task in uninterruptible I/O, which does happen to a hook talking
// to a NAS share, and reporting that honestly is the difference between
// "the hook was killed" and "we stopped waiting for the hook".
//
// Every docker call on that path is bounded, including the wait for the
// attached client, because a daemon that has stopped answering must cost
// this runner certainty and never its ability to return.
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

	// ControlTimeout bounds each individual docker control call: the
	// create, the signals, the inspections, the removals. Zero takes
	// dockerControlTimeout.
	//
	// A field for ConfirmWindow's reason plus one of its own. The
	// behaviour under assertion on this path is that a runner whose
	// daemon has stopped answering RETURNS -- with an unconfirmed
	// termination, which is the honest answer -- and a test that proved
	// it by waiting out twenty seconds per call is a test somebody
	// deletes to get the suite back.
	ControlTimeout time.Duration

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

	// RETND_WORK_DIR is appended LAST and therefore wins, whatever the
	// engine sent. Only this process knows the directory it just
	// created, so only this process is in a position to state it; an
	// engine-supplied value would be a path to somewhere else, and a
	// hook writing its dump there would write it outside the directory
	// this runner cleans up.
	//
	// It is the same string inside the container as outside, because the
	// mount is an identity mount: see Mount.
	env := EnvSet{Vars: append(append([]EnvVar(nil), req.Env.Vars...), EnvVar{Name: "RETND_WORK_DIR", Value: workDir})}
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

	id, err := mintContainerIdentity(req.RunID, req.StepID)
	if err != nil {
		e.cleanupStep(req.RunID, req.StepID)
		return Result{}, err
	}

	result, err := e.run(ctx, runSpec{
		id:         id,
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

// containerIdentity is everything this runner owns about one container
// before the container exists.
//
// Two facts rather than one, because they answer different questions.
// The NAME is what an operator sees in `docker ps` and what every signal
// is addressed to. The TOKEN is what makes a termination provable: it
// goes on the container as LabelInstance at creation, so `docker ps
// --filter label=` finds this launch's container even when nothing here
// ever observed its name being registered -- which is exactly the state
// a cancel landing inside a creation leaves behind.
type containerIdentity struct {
	name  string
	token string
}

// maxContainerNameLength is the bound this runner holds a name to.
// docker's own limit is 255 bytes; the margin is for the suffixes a
// daemon, a compose project or an operator's tooling appends to a name
// it is given.
const maxContainerNameLength = 200

// containerTokenBytes is how much randomness every container name and
// every instance label carries. Eight bytes as sixteen hex characters:
// far past any chance of two launches colliding, and short enough to
// leave the ids room to be legible in `docker ps`.
const containerTokenBytes = 8

// mintContainerIdentity mints the name and the label this runner owns for
// one step's container.
//
// <prefix><run>-<step>-<token>, and every piece of it is load bearing.
// The prefix and the ids make an operator's `docker ps` legible. The
// token is what makes the name UNIQUE rather than derived: a step
// retried after a termination this runner could not confirm would
// otherwise collide with the container it could not remove, and `docker
// create --name` fails on a collision -- turning "the last attempt left
// something behind" into "this step can never run again".
//
// The truncation is where that argument used to be undone, and it is a
// cross-kill rather than a cosmetic fault. Verify holds each id to
// MaxIDLength, which is 128, so a run id and a step id can exceed
// docker's limit between them -- and a name cut at the END loses the
// token, the only unique part of it. Two concurrent steps whose ids
// shared a long prefix therefore got the SAME name: the second `docker
// create` failed on the collision, and that failed attempt's own cleanup
// removed the container the first step was still running in. So the ID
// PORTION is what gets shortened, and the token is always present at
// full length.
func mintContainerIdentity(runID, stepID string) (containerIdentity, error) {
	token, err := mintToken()
	if err != nil {
		return containerIdentity{}, err
	}
	return containerIdentity{name: containerName(runID, stepID, token), token: token}, nil
}

// mintAuxIdentity mints an identity for one of this runner's OWN
// containers -- the capability probe, the `bash -n` syntax check -- which
// belong to no step and so carry no ids.
//
// They get one for the reason a hook does: an unnamed, unlabelled
// container is one nothing can address once the client that started it
// has gone, and both of those runs are started under a context that can
// be cancelled while the daemon is between creating the container and
// starting it -- a state `--rm` does not cover, because --rm is a
// promise about an exit.
func mintAuxIdentity(kind string) (containerIdentity, error) {
	token, err := mintToken()
	if err != nil {
		return containerIdentity{}, err
	}
	return containerIdentity{name: containerNamePrefix + kind + "-" + token, token: token}, nil
}

func mintToken() (string, error) {
	var random [containerTokenBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", &Failure{Code: CodeInternal, Message: "this runner could not mint a container name: " + err.Error()}
	}
	return hex.EncodeToString(random[:]), nil
}

// containerName composes one name, holding the ID PORTION to whatever
// space is left after the prefix and the token. See mintContainerIdentity
// for why it is that way round.
//
// The ids are already held to ValidID by Verify, and every id this
// product mints is within its alphabet -- but that alphabet is NOT a
// subset of docker's, which is what this function has to reconcile.
// internal/workflow.StepID joins an order, a scope, a phase and a script
// name with "~" (it is the one character the script-name rule reserves),
// so every real step id carries three of them, and docker accepts only
// [a-zA-Z0-9][a-zA-Z0-9_.-]* in a name: passing one through verbatim is
// a `docker create` refused for a name the operator never chose, which
// is every `.local.sh` hook failing. So the id portion is transliterated
// here, at the one place a name is composed, rather than by narrowing
// what a step may be called -- the step id is the journal's, the API's
// and the CLI's spelling of that step, and docker's naming rule has no
// business deciding it.
func containerName(runID, stepID, token string) string {
	ids := dockerNameSafe(runID + "-" + stepID)
	budget := maxContainerNameLength - len(containerNamePrefix) - len("-") - len(token)
	if len(ids) > budget {
		// A hash of the WHOLE pair rather than a cut, so that the
		// legible part of two long ids that agree for their first
		// hundred characters still differs -- and the token makes the
		// name unique regardless, which is the braces to this belt.
		sum := sha256.Sum256([]byte(ids))
		digest := hex.EncodeToString(sum[:4])
		keep := budget - len("-") - len(digest)
		if keep < 0 {
			keep = 0
		}
		ids = ids[:keep] + "-" + digest
	}
	return containerNamePrefix + ids + "-" + token
}

// dockerNameSafe transliterates an id into docker's name alphabet.
//
// Every byte docker will not accept becomes an underscore, which keeps
// the result the same LENGTH -- the budget arithmetic above is about
// bytes, and a substitution that changed the length would make that
// bound wrong -- and keeps the name legible: an operator reading
// `docker ps` sees backupd-hook-wfr_...-0000_global_before_10-quiesce...
// rather than a hash. Uniqueness does not rest on this at all: the token
// appended after it is eight bytes of randomness per launch, and the
// labels carry the exact run and step ids untransliterated, which is
// what a sweep and a termination select on.
func dockerNameSafe(ids string) string {
	safe := []byte(ids)
	for i := 0; i < len(safe); i++ {
		c := safe[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
		default:
			safe[i] = '_'
		}
	}

	return string(safe)
}

// runSpec is one container's inputs, gathered so run's signature does not
// grow six parameters of the same type.
type runSpec struct {
	id         containerIdentity
	runID      string
	stepID     string
	scriptPath string
	workDir    string
	env        []string
	timeout    time.Duration
}

// run is the launch itself, in the four steps a container's life has:
// create it, start it with this process attached, ask the DAEMON what its
// process did, remove it and prove the removal.
//
// `docker run` did all four behind one exit status, and every fault this
// shape exists to fix was a consequence of that. A cancel could land
// before the name was registered, so the one-shot kill that followed
// answered "no such container" twice and a hook created a moment later
// ran on with nothing watching it. The client's 125 meant either "the
// container could not be created" or "the hook exited 125", which are
// not the same answer. And a client that exited while its container
// survived -- a daemon restart, a dropped transport -- looked exactly
// like a clean exit, so the step was recorded as finished and its
// working directory deleted under whatever was still writing in it.
func (e *Executor) run(ctx context.Context, spec runSpec, sink Sink) (Result, error) {
	capture := NewCapture(sink)
	started := time.Now()
	elapsed := func() int64 { return time.Since(started).Milliseconds() }

	// # 1. Creation, as its own step, so the identity precedes the
	// container. See Container.create and hookArgs.
	detail, err := e.Container.create(ctx, e.controlTimeout(), spec.env, launchSpec{
		name:       spec.id.name,
		token:      spec.id.token,
		runID:      spec.runID,
		stepID:     spec.stepID,
		workDir:    spec.workDir,
		scriptPath: spec.scriptPath,
		envNames:   envNames(spec.env),
	})
	if err != nil {
		// A create that FAILED is not a create that did not happen. The
		// client may have been killed on the way back from a daemon
		// that had already made the container, which is precisely what
		// a cancel during creation looks like -- so absence is
		// RECONCILED rather than assumed, and with the settled
		// question: a single empty answer here could be one the daemon
		// gave before it finished creating anything.
		certainty := e.reconcile(spec.id, reconcileSettled)
		if detail != "" {
			// Where every other launch fault's message goes: this
			// step's own stderr, which the engine is already streaming.
			// Not into the refusal below -- a bind-mount fault names a
			// host path out of an operator's configuration, and a
			// refusal is recorded in more places than a log line.
			_, _ = capture.Writer(StreamStderr).Write([]byte(detail + "\n"))
		}
		if state, canceled := cancelState(ctx); canceled {
			// The cancel is the outcome, not the create's failure: an
			// engine that went away during creation gets the same
			// answer as one that went away during the hook, certainty
			// included -- and that certainty is what says whether
			// anything was left behind.
			return Result{
				State:                state,
				TerminationCertainty: certainty,
				DurationMS:           elapsed(),
				Chunks:               capture.Seq(),
			}, nil
		}
		return Result{
			State:                StateExited,
			TerminationCertainty: certainty,
			DurationMS:           elapsed(),
			Chunks:               capture.Seq(),
		}, &Failure{
			Code:    CodeInternal,
			Message: fmt.Sprintf("the hook container could not be created in %s: %s", e.Container.Image, containerStartDetail(capture.Seq())),
		}
	}

	// # 2. The attached start.
	//
	// NOT exec.CommandContext. Its cancellation kills the CLIENT, and
	// killing the docker client does not stop the container it started:
	// the container is the daemon's child, not this process's, so a
	// context kill would leave the hook running with nothing attached to
	// it, and the Result would still say "canceled". The termination
	// below is done through the daemon instead.
	cmd := exec.Command(e.Container.Docker, e.Container.startArgs(spec.id.name)...)

	// The hook's environment is not this client's. The values went to
	// the daemon at CREATE time, which is what `--env NAME` reads them
	// for, and a start carries none of them: what this process passes on
	// is its own environment, exactly as every other control call in
	// this package does, with the client's own settings already resolved
	// into explicit flags (Container.clientArgs) so that nothing an
	// operator can set moves this runner's daemon.
	cmd.Env = nil

	// nil Stdin is /dev/null, and the start does not ask for -i, so the
	// hook's stdin is closed inside the container too. A hook that reads
	// stdin gets EOF rather than this daemon's own input, and cannot
	// block the run waiting for something nobody is going to type. No -t
	// either, anywhere: see the probe's tty assertion.
	cmd.Stdin = nil

	// A failure before anything is attached still leaves a CREATED
	// container holding this step's working directory as a read-write
	// mount, so it is removed here rather than left to a timeout that
	// will never fire for it.
	launchFailure := func(sentence string) (Result, error) {
		certainty := e.reconcile(spec.id, reconcileKnown)
		return Result{
			State:                StateExited,
			TerminationCertainty: certainty,
			DurationMS:           elapsed(),
			Chunks:               capture.Seq(),
		}, &Failure{Code: CodeInternal, Message: sentence}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return launchFailure("this runner could not open a pipe for the hook's stdout: " + err.Error())
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return launchFailure("this runner could not open a pipe for the hook's stderr: " + err.Error())
	}
	if err := cmd.Start(); err != nil {
		return launchFailure(fmt.Sprintf("this runner could not start %s: %v", e.Container.Docker, err))
	}

	var pumps sync.WaitGroup
	pumps.Add(2)
	// The two streams stay apart all the way from the container: docker
	// demultiplexes them for a container with no PTY, which is the other
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
		exitCode  *int
		neverRan  bool
	)
	select {
	case <-waited:
		// # 3. What the container's process did, asked of the daemon.
		exitCode, certainty, neverRan = e.observeExit(spec.id)
	case <-timer.C:
		state = StateTimedOut
		certainty = e.terminate(spec.id, waited, cmd)
	case <-ctx.Done():
		state, _ = cancelState(ctx)
		certainty = e.terminate(spec.id, waited, cmd)
	}

	result := Result{
		State:                state,
		TerminationCertainty: certainty,
		DurationMS:           elapsed(),
		Chunks:               capture.Seq(),
		ExitCode:             exitCode,
	}

	if neverRan {
		// The daemon has the container in `created`: it was never
		// started, so nothing ran and there is no status to report.
		// Reporting it as an exit would tell an operator their script
		// returned something.
		return result, &Failure{
			Code:    CodeInternal,
			Message: fmt.Sprintf("the hook container was created in %s and never ran: %s", e.Container.Image, containerStartDetail(result.Chunks)),
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

// observeExit is the ordinary end of a step: the attached client has
// returned, and what the container's own process did is now asked of the
// daemon.
//
// Asked, because the client cannot answer it. `docker start --attach`
// reports the container's status as its OWN exit status, so a hook that
// ran and exited 125 and a client that could not reach the daemon are
// the same number -- and reading that number as "the container was never
// created" told an operator nothing had run when their script had run
// and returned 125. That is a lie about the one fact the workflow engine
// branches on, and it is a behaviour a hook running under the host's own
// bash never had.
//
// Three answers, and each is a different fact:
//
//   - the daemon says the container ran and stopped. That status is the
//     HOOK's, whatever it is, 125 included.
//   - the daemon says the container was created and never started. Then
//     nothing ran, and that is a launch failure rather than an exit.
//   - the container is still there, or the daemon cannot be asked at
//     all. Then the hook's status is NOT knowable from here, so no exit
//     code is invented; the container is killed and removed by label,
//     and the certainty is whatever that could prove. A removal that
//     cannot be proved is UNCONFIRMED, which is what keeps the working
//     directory (see Execute) -- a container that outlived the client
//     attached to it may still be writing in there.
//
// One honest limit, recorded because it is a real difference from the
// process this replaced: a hook killed by something inside the container
// (the kernel's OOM killer) arrives here as 137, because that is what
// the daemon reports, and a hook that returned 137 itself is
// indistinguishable from one that was killed. The states this runner
// produces ITSELF -- timed out, canceled, lease expired -- carry no exit
// code at all, so the distinction that matters upstream is unaffected.
func (e *Executor) observeExit(id containerIdentity) (*int, TerminationCertainty, bool) {
	observed, err := e.Container.inspectState(e.controlTimeout(), id.name)
	switch {
	case err == nil && observed.exited:
		code := observed.exitCode
		certainty := e.reconcile(id, reconcileKnown)
		if certainty == CertaintyConfirmed {
			// Nothing was terminated: the hook exited on its own and
			// its container is gone. CertaintyNotApplicable is what
			// internal/workflow records for that, and "confirmed" would
			// claim a kill that never happened.
			certainty = CertaintyNotApplicable
		}
		return &code, certainty, false
	case err == nil && observed.status == "created":
		return nil, e.reconcile(id, reconcileKnown), true
	default:
		return nil, e.reconcile(id, reconcileKnown), false
	}
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

// terminate stops a step's container and reports whether it is provably
// gone, in the ONE order that can answer that question.
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
// Then the LOOK, which is Executor.reconcile: it asks the DAEMON what it
// still has carrying this launch's label, kills and removes whatever
// that is, and reports only what it could prove. By label rather than by
// the name these signals are addressed to, because the two questions are
// not the same one -- the name is the object this runner believes in and
// the label is the object the daemon has.
//
// The wait for the attached client is BOUNDED, and that is a change from
// the version of this function that blocked on it. `docker start
// --attach` talking to a daemon that has stopped answering has no
// timeout of its own, so an unbounded receive here turned a lost lease
// or a shutdown into a hang with no message anywhere. When the bound
// elapses the CLIENT is killed -- safe for the same reason the launch
// does not use CommandContext: the client is not the container -- and
// the container's own fate is then settled against the daemon. What the
// bound costs is certainty, never a runaway.
func (e *Executor) terminate(id containerIdentity, waited chan error, client *exec.Cmd) TerminationCertainty {
	_ = e.Container.signal(e.controlTimeout(), id.name, "TERM")

	select {
	case <-waited:
		// The client returned inside the grace period, which is what a
		// container whose process handled the signal looks like. The
		// removal and its proof are still the reconciliation's job: a
		// client that exited proves only that a client exited.
		return e.reconcile(id, reconcileKnown)
	case <-time.After(e.grace()):
	}

	_ = e.Container.signal(e.controlTimeout(), id.name, "KILL")

	select {
	case <-waited:
		// The client let go after the kill, which means it was reaped
		// while talking to a daemon that answered: the container it
		// was attached to is a container that certainly exists, so the
		// ordinary question is the right one.
		return e.reconcile(id, reconcileKnown)
	case <-time.After(e.clientBound()):
	}

	if client.Process != nil {
		_ = client.Process.Kill()
	}
	// Killing the client closes the pipes the pumps are reading, so this
	// receive is the one that returns. It is bounded too, because a pump
	// blocked on a sink is not a reason to stop reconciling the
	// container.
	select {
	case <-waited:
	case <-time.After(e.controlTimeout()):
	}
	// SETTLED, because a client that had to be killed is a client whose
	// daemon was not answering, and a daemon that is not answering may
	// be a step behind this runner rather than done with it.
	return e.reconcile(id, reconcileSettled)
}

// reconcileMode is which question Executor.reconcile asks.
type reconcileMode bool

const (
	// reconcileKnown is for a container this runner watched being
	// created and whose client it has reaped: it is removed by name
	// first, and the first empty answer afterwards is the proof it is
	// gone.
	reconcileKnown reconcileMode = false

	// reconcileSettled is for a container whose very EXISTENCE is in
	// doubt -- a create client that was cancelled or killed, a daemon
	// that was asked and never answered.
	//
	// It watches for the whole window rather than believing the first
	// empty answer, and that is the fix for a specific runaway: a
	// daemon that accepted a creation and completed it after the client
	// asking for it had gone answers "nothing here" until the moment it
	// does not, so a single look proves only when it was taken. The
	// cost is up to ConfirmWindow on a path that is already an ending;
	// the alternative is a container nobody ever hears about again.
	reconcileSettled reconcileMode = true
)

// reconcile removes everything the daemon still has carrying this
// launch's unique label, and reports whether it could PROVE there is
// nothing left.
//
// By LABEL, which is the whole point. A removal by name is a removal of
// the object this runner thinks exists; the label is a question about
// what the daemon HAS -- including a container whose creation completed
// after the client that asked for it was already dead, which is the
// runaway a one-shot kill-by-name could not reach. Anything found is
// killed and removed by the id the daemon gave, so a container that
// appeared after the first signal is still ended.
//
// A daemon that cannot be asked answers UNCONFIRMED rather than
// "absent", which is the safe direction and the reason
// containersWithInstance returns its error separately from its answer:
// reporting "gone" because a question failed is how a runaway gets
// recorded as a clean kill. Every call inside the loop is bounded by
// ControlTimeout and the loop itself by ConfirmWindow, so a wedged
// daemon costs this function an answer and never its return.
func (e *Executor) reconcile(id containerIdentity, mode reconcileMode) TerminationCertainty {
	if mode == reconcileKnown {
		// The container is known to exist, so the removal comes first
		// and the question below is about its result rather than one
		// asked before it. That keeps the ordinary end of a step at one
		// round trip after the removal.
		_ = e.Container.remove(e.controlTimeout(), id.name)
	}

	deadline := time.Now().Add(e.confirmWindow())
	absent := false
	for {
		leftovers, err := e.Container.containersWithInstance(e.controlTimeout(), id.token)
		switch {
		case err != nil:
			// Not an answer. Keep asking until the window is out.
			absent = false
		case len(leftovers) == 0:
			absent = true
			if mode == reconcileKnown {
				return CertaintyConfirmed
			}
		default:
			absent = false
			for _, container := range leftovers {
				_ = e.Container.signal(e.controlTimeout(), container, "KILL")
				_ = e.Container.remove(e.controlTimeout(), container)
			}
		}
		if !time.Now().Before(deadline) {
			if absent {
				return CertaintyConfirmed
			}
			return CertaintyUnconfirmed
		}
		time.Sleep(killPollInterval)
	}
}

// cancelState maps a cancelled context onto the state that says WHY.
//
// An operator's cancel and an engine that went away are the same kill
// and two different stories, and internal/workflow's journal keeps them
// apart. It reports whether the context is done at all, so that the one
// caller which has to tell "the create failed" from "the create was
// cancelled" can.
func cancelState(ctx context.Context) (State, bool) {
	if ctx.Err() == nil {
		return StateExited, false
	}
	if errors.Is(context.Cause(ctx), errLeaseExpired) {
		return StateLeaseExpired, true
	}
	return StateCanceled, true
}

// controlTimeout bounds one docker control call. See
// Executor.ControlTimeout.
func (e *Executor) controlTimeout() time.Duration {
	if e.ControlTimeout > 0 {
		return e.ControlTimeout
	}
	return dockerControlTimeout
}

// confirmWindow is how long absence is waited for before a termination is
// reported as unconfirmed. See Executor.ConfirmWindow.
func (e *Executor) confirmWindow() time.Duration {
	if e.ConfirmWindow > 0 {
		return e.ConfirmWindow
	}
	return killConfirmWindow
}

// clientBound is how long this runner waits for the attached docker
// client to let go after the container has been killed, before it kills
// the client instead.
//
// Derived rather than configured, because it is not an independent
// policy: it is "long enough that a client talking to a working daemon
// always wins", and that is exactly the grace period plus the window in
// which absence is being proved anyway.
func (e *Executor) clientBound() time.Duration {
	return e.grace() + e.confirmWindow()
}
