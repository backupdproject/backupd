package remoteexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

const (
	// signalGrace is how long termination waits for the session to
	// complete after the exec-channel signal request.
	//
	// Whether that request does anything at all is the server's decision:
	// the protocol has it, and an sshd is free to ignore it. Measured
	// against OpenSSH 10 it is honoured and the session ends in
	// milliseconds; older and other servers do not implement it. It runs
	// AFTER the reaper (see terminate) because on a server that honours
	// it, it destroys the reaper's only handle.
	signalGrace = 3 * time.Second

	// reapGrace is how long termination waits after the reaper has killed
	// the step's process group. The reaper has already waited for its own
	// TERM-then-KILL, so this only covers the remote side closing the
	// channel.
	reapGrace = 5 * time.Second

	// reaperTimeout bounds the reaper session itself.
	reaperTimeout = 30 * time.Second

	// pgidCaptureTimeout bounds one of the process-group scan's sessions.
	pgidCaptureTimeout = 20 * time.Second

	// writeSettle is how long the payload write is given to report itself
	// after the session has ended and the channel has been closed under
	// it. A closed channel fails a pending write immediately, so this is
	// a bound on a wedged transport rather than a wait anything reaches.
	writeSettle = 5 * time.Second
)

// pgidCaptureAttempts are how long a step has to have been running before
// each attempt to learn its remote process group.
//
// Nothing is captured for a step that finishes inside the first, which is
// most of them: the capture exists only for a step that has to be STOPPED,
// and one that has already ended has nothing to stop. The second is for
// the other end of the same race -- a busy host where the leader is not in
// the process table yet, or one where it has only just appeared -- and it
// is only reached when the first attempt found nothing.
var pgidCaptureAttempts = [2]time.Duration{250 * time.Millisecond, 1500 * time.Millisecond}

// Request is one script to run on one connection.
type Request struct {
	// Token is the non-secret identifier this step's remote process group
	// is recognised by, and the ONLY variable part of the remote command
	// line.
	//
	// It is on the command line on purpose, and it is the only thing that
	// ever is. Without a PTY there is no signal this product can rely on,
	// so termination has to be able to find the step's processes on the
	// far side, and the remote process list is the one place it can look.
	// A token is safe to publish there -- it identifies a run, not a
	// credential -- which is exactly why an environment value or a secret
	// never may be.
	//
	// It is BUILT with StepToken and not taken from an identifier a
	// caller already has: it has to satisfy tokenRule, and every
	// identifier this product mints -- a step id above all -- contains
	// characters that rule refuses. Handing one over unchanged was #919,
	// and the refusal arrives before a session is ever opened.
	Token string

	// Environ is the step's fully resolved environment as NAME=VALUE
	// entries, in precedence order. Secret values are ordinary strings
	// here because this is the moment they are used; they never reach the
	// command line, a file, or a log.
	Environ []string

	// Script is the captured bytes, opened through the plan and verified
	// against its hash (internal/workflow's Plan.OpenScript). This package
	// never opens a path itself.
	Script []byte

	// Capability is the proof, from Preflight, that this connection will
	// run THESE bytes. It is optional and it is not a way round the
	// proof: a nil one makes Run take the preflight itself, and a
	// non-nil one that does not match this connection and this script is
	// refused rather than ignored.
	//
	// It exists because a caller that validates a whole stage before
	// running it (#811) has already paid for the probe and the syntax
	// check, and paying twice for the same two facts would be the kind of
	// cost that gets a gate removed later.
	Capability *Capability

	// Sink receives the output as it arrives.
	Sink workflowexec.Sink

	// Timeout is how long the step may run. Zero means the context's own
	// deadline is the only bound.
	Timeout time.Duration

	// The audit inputs the caller knows and this package does not.
	BackupSet    string
	StepID       string
	ScriptName   string
	ScriptSHA256 string
}

func (r Request) validate() error {
	if !tokenRule.MatchString(r.Token) {
		return fmt.Errorf("%w: %q is not a usable step token; it becomes part of the fixed remote command, so it must be 1 to 120 characters of letters, digits, dot, dash and underscore and is refused rather than quoted",
			ErrConnection, r.Token)
	}
	if r.Sink == nil {
		return fmt.Errorf("%w: there is nowhere to put this step's output, and a hook whose output went nowhere must not be reported as captured", ErrConnection)
	}

	return nil
}

// Result is what happened to one step.
type Result struct {
	// ExitCode is the hook's own exit status, and it is nil unless this
	// product actually observed one. A step killed on timeout, a step
	// whose connection dropped and a step that was cancelled all leave it
	// nil: #810's technical requirement is that transport loss is never
	// reported as a known exit code, and nil is how "nobody saw a status"
	// is spelled.
	ExitCode *int

	// Signal is the remote signal name when the command died from one
	// ("TERM" after a successful termination), empty otherwise.
	Signal string

	// Certainty is what was PROVED about termination. See the package doc.
	Certainty workflowexec.TerminationCertainty

	// Chunks is how many capture chunks the step produced, across both
	// streams.
	Chunks uint64

	// StartedAt and FinishedAt bracket the session.
	StartedAt  time.Time
	FinishedAt time.Time

	// HostKeyFingerprint and User are the endpoint identity this ran
	// against, carried so an audit line does not have to ask twice.
	HostKeyFingerprint string
	User               string

	// Reaper is what the termination reaper found and did, for
	// diagnostics. Empty when termination was never requested.
	Reaper string
}

// Run executes one script on this connection and captures its output.
//
// It is the ONLY exported way to run anything on this package's channel,
// and it does not start a hook on a connection whose capability has not
// just been proven against the server. A forced-command account accepts
// the exec request, runs somebody else's program and exits 0, so an
// execution path that started with "the request was accepted" would report
// a hook as having succeeded when nothing of it ever ran -- the silent
// success ADR 0021 exists to refuse. Either the caller brings a Capability
// from Preflight for these exact bytes, or Run takes the preflight itself.
//
// The rest of the envelope is decided elsewhere and applied here: the fixed
// remote command (Client.remoteCommand), the stdin payload
// (workflowexec.StdinPayload), and the two separate capture streams sharing
// one sequence counter (workflowexec.Capture). What this function owns is
// the waiting and the stopping.
//
// The step's bound (Request.Timeout) covers everything from opening the
// channel onward, INCLUDING the payload write. A payload can be sixteen
// mebibytes and a half-dead TCP connection accepts none of it, so a bound
// that started once the write had finished would be a bound that a dead
// peer never reaches.
//
// It returns a Result for every outcome it observed, INCLUDING the failures:
// a refused capability, a lost connection and a killed step all have facts
// worth auditing, and a function that returned only an error would throw
// them away. The error says what went wrong; the Result says what was seen.
func (c *Client) Run(ctx context.Context, req Request) (Result, error) {
	result := Result{
		HostKeyFingerprint: c.HostKeyFingerprint(),
		User:               c.conn.Source.User,
	}

	if err := req.validate(); err != nil {
		return result, err
	}
	payload, err := workflowexec.StdinPayload(req.Environ, req.Script)
	if err != nil {
		return result, err
	}

	capability := req.Capability
	if capability == nil {
		if capability, err = c.Preflight(ctx, req.Script); err != nil {
			return result, err
		}
	}
	if err := c.honours(capability, req.Script); err != nil {
		return result, err
	}

	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	session, err := c.session(runCtx)
	if err != nil {
		return result, err
	}
	defer func() { _ = session.Close() }()

	capture := workflowexec.NewCapture(req.Sink)
	session.Stdout = capture.Writer(workflowexec.StreamStdout)
	session.Stderr = capture.Writer(workflowexec.StreamStderr)

	stdin, err := session.StdinPipe()
	if err != nil {
		return result, fmt.Errorf("%w: opening the exec channel's stdin: %v", ErrTransportLoss, err)
	}

	// No PTY is requested, here or anywhere in this package. That is what
	// keeps stdout and stderr two streams; it is also why termination
	// needs the reaper, because without a controlling terminal sshd
	// delivers no hangup to what was running.
	result.StartedAt = time.Now()
	if err := session.Start(c.remoteCommand(req.Token)); err != nil {
		return result, fmt.Errorf("%w: starting the exec channel: %v", ErrTransportLoss, err)
	}

	// The write runs beside the wait rather than before it. bash reads the
	// payload as it parses, so a remote side that has stopped reading
	// blocks this write on the channel window with no deadline of its own;
	// with the wait already running, the step's own bound reaches it.
	//
	// Whether that write succeeded does not decide the step. The hook's
	// own account of how it ended does; see the wait branch below.
	writeDone := make(chan error, 1)
	go func() { writeDone <- writePayload(stdin, payload) }()

	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- session.Wait()
		close(finished)
	}()

	group := c.capturePGID(ctx, req.Token, finished)

	select {
	case waitErr := <-done:
		result.FinishedAt = time.Now()
		result.Chunks = capture.Sequence()
		result.Certainty = workflowexec.TerminationNotRequested
		// The write is drained -- to unblock one still parked on the
		// channel window, and so it cannot outlive the step -- and
		// its verdict is DISCARDED. A hook stops reading its payload
		// the moment it exits: an early "exit 0", a script that ends
		// before its stdin does, a hook killed on the far side. The
		// remote closes the channel's stdin under a write that is
		// still going, and the write fails on the hook's own ending.
		// Reporting that as transport loss threw away the exit status
		// sitting right here and reported a hook that ran, and said
		// how it ended, as an outcome nobody observed (#915).
		//
		// Nothing is lost by discarding it: a transport that really
		// did drop the script leaves NO exit status, and finish()
		// classifies the *ssh.ExitMissingError -- or the connection
		// error -- that comes back instead as exactly that loss.
		_ = drainWrite(session, writeDone)

		return c.finish(result, waitErr)

	case <-runCtx.Done():
		reason := runCtx.Err()
		c.terminate(ctx, req.Token, group.get(), session, done, &result)
		result.FinishedAt = time.Now()
		result.Chunks = capture.Sequence()

		if errors.Is(reason, context.DeadlineExceeded) {
			return result, fmt.Errorf("%w: the step outran its %s bound on %s and termination is recorded as %s",
				ErrStepTimeout, req.Timeout, c.describeEndpoint(), result.Certainty)
		}

		return result, fmt.Errorf("%w: the step was cancelled on %s and termination is recorded as %s",
			ErrStepCanceled, c.describeEndpoint(), result.Certainty)
	}
}

// drainWrite unblocks the payload write and collects what it reported.
//
// The channel is closed first, deliberately: a write still blocked on a
// remote side that stopped reading fails the moment the channel goes, so
// this returns promptly with the truth rather than waiting out a window
// that will never open. The timer is the bound on a transport that will
// not even do that, and it reports transport loss rather than success,
// because a payload whose delivery nobody can account for is a script
// nobody can say arrived whole.
//
// What that verdict MEANS is the caller's, and the two callers answer
// differently. runOnce is worth nothing without the whole payload -- a
// capability proved over a PREFIX of the script is not a proof about the
// script -- so there a failed write fails the session. Run has the hook's
// own exit status, which is authoritative over a write that the hook's
// ending is what broke.
func drainWrite(session *ssh.Session, writeDone <-chan error) error {
	_ = session.Close()

	timer := time.NewTimer(writeSettle)
	defer timer.Stop()

	select {
	case err := <-writeDone:
		return err
	case <-timer.C:
		return fmt.Errorf("%w: the execution envelope's delivery never completed or failed, so there is no saying how much of the script the far side received", ErrTransportLoss)
	}
}

// ErrStepTimeout and ErrStepCanceled are the two ways a step is stopped
// rather than finishing. They are separate sentinels because the journal
// records them as different states (workflow.StateTimedOut and
// StateCanceled) and because one is this product's own bound while the
// other is an operator or a shutdown.
var (
	ErrStepTimeout  = errors.New("remoteexec: the step was stopped because it outran its timeout")
	ErrStepCanceled = errors.New("remoteexec: the step was stopped because it was cancelled")
)

// finish turns x/crypto/ssh's account of how the session ended into an exit
// code, a signal, or a transport loss.
//
// The three are told apart deliberately. An *ssh.ExitError carries a status
// the remote command actually returned. An *ssh.ExitMissingError means the
// channel closed without one, which is transport loss however tidy it
// looked. Anything else is the connection failing under us. Only the first
// produces an ExitCode.
func (c *Client) finish(result Result, waitErr error) (Result, error) {
	if waitErr == nil {
		zero := 0
		result.ExitCode = &zero

		return result, nil
	}

	var exitErr *ssh.ExitError
	if errors.As(waitErr, &exitErr) {
		if signal := exitErr.Signal(); signal != "" {
			result.Signal = signal
			// A command killed by a signal has no exit status of its
			// own, and inventing 128+n here would be this product
			// making up a number the hook never returned.
			return result, fmt.Errorf("%w: the hook on %s was killed by SIG%s", ErrStepSignaled, c.describeEndpoint(), signal)
		}
		code := exitErr.ExitStatus()
		result.ExitCode = &code

		return result, nil
	}

	var missing *ssh.ExitMissingError
	if errors.As(waitErr, &missing) {
		return result, fmt.Errorf("%w: the exec channel on %s closed without reporting an exit status", ErrTransportLoss, c.describeEndpoint())
	}

	return result, fmt.Errorf("%w: the exec channel on %s failed: %v", ErrTransportLoss, c.describeEndpoint(), waitErr)
}

// ErrStepSignaled is a hook killed by a signal on the far side -- by the
// remote host's OOM killer, by an operator on that host, or by this
// product's own reaper. It is not an exit code and must never be recorded
// as one.
var ErrStepSignaled = errors.New("remoteexec: the hook was killed by a signal on the remote host")

// writePayload sends the bootstrap and the script, then closes stdin.
//
// Closing matters as much as writing: bash reads its script from this pipe
// and will not finish parsing until it sees the end. A write failure is
// returned rather than ignored, because a hook that received half its
// payload would run half a script.
func writePayload(stdin io.WriteCloser, payload []byte) error {
	_, writeErr := stdin.Write(payload)
	closeErr := stdin.Close()

	switch {
	case writeErr != nil:
		return fmt.Errorf("%w: writing the execution envelope to the exec channel: %v", ErrTransportLoss, writeErr)
	case closeErr != nil:
		return fmt.Errorf("%w: closing the exec channel's stdin: %v", ErrTransportLoss, closeErr)
	default:
		return nil
	}
}

// terminate asks the remote side to stop, with the strongest semantics the
// protocol and the far side actually offer, and records what was PROVED.
//
// The sequence is three steps because each one covers what the previous
// cannot:
//
//  1. a reaper session, which finds the step's process GROUP -- by the
//     token on the leader's command line, and by the group id captured
//     while that leader was demonstrably alive -- and sends it TERM and
//     then KILL. This is what reaches a hook's own children, and it is
//     what works on a server that ignores signal requests.
//  2. an exec-channel signal request. The protocol has one; a server may
//     ignore it. On a server that honours it this ends the session in
//     milliseconds.
//  3. closing the channel, which is all that is left.
//
// # Why the reaper goes first
//
// Because on a server that honours the signal request, sending it first
// destroys the reaper's only handle. OpenSSH signals the process GROUP, so
// bash -- which does not ignore TERM -- dies immediately while a child that
// ignores TERM carries on holding the channel's stdout. The token lives on
// the LEADER's command line, so once the leader is gone the reaper matches
// nothing, reports no groups, and kills nothing: the step is left running
// on the far side with termination recorded as unconfirmed. Reaping first
// costs one short session in the case where the signal would have been
// enough, and it is the difference between a mechanism and an optimisation
// that eats it.
//
// Certainty is decided by ONE rule, and not by which of the three steps ran:
// confirmed if the session COMPLETED -- exit reported, both streams at end
// of file -- and unconfirmed otherwise. That is why a deliberately detached
// descendant reports unconfirmed: it keeps the channel's stdout open, so the
// session never completes, and this product will not claim a clean stop it
// cannot see.
func (c *Client) terminate(ctx context.Context, token, pgid string, session *ssh.Session, done <-chan error, result *Result) {
	result.Certainty = workflowexec.TerminationUnconfirmed

	// The context that brought us here is already cancelled, so every
	// step below gets its own budget from a context that is not.
	base := context.WithoutCancel(ctx)

	result.Reaper = c.reap(base, token, pgid)
	if waitFor(done, reapGrace) {
		result.Certainty = workflowexec.TerminationConfirmed

		return
	}

	_ = session.Signal(ssh.SIGTERM)
	if waitFor(done, signalGrace) {
		result.Certainty = workflowexec.TerminationConfirmed

		return
	}

	_ = session.Close()
}

// waitFor reports whether the session completed within d.
func waitFor(done <-chan error, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// reap runs the reaper on the far side and returns a one-line account of
// what it found, for the audit.
//
// It is best effort by nature -- the connection may be the thing that
// broke -- so it never fails the step: whatever it could not do is
// reflected in the certainty, which is the field that matters.
func (c *Client) reap(ctx context.Context, token, pgid string) string {
	ctx, cancel := context.WithTimeout(ctx, reaperTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	// No token on the reaper's own command line: it is looking for
	// processes carrying that token, and a reaper that matched itself
	// would kill its own process group.
	if _, err := c.runOnce(ctx, reaperTimeout, c.remoteCommand(""), []byte(reaperScript(token, pgid)), &stdout, &stderr); err != nil {
		return "the reaper could not run: " + err.Error()
	}

	fields := map[string]string{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if name, value, found := strings.Cut(strings.TrimSpace(line), "="); found {
			fields[name] = value
		}
	}

	groups := strings.TrimSpace(fields["groups"])
	survivors := strings.TrimSpace(fields["survivors"])
	switch {
	case fields["ps"] == "missing":
		return "the remote host has no usable ps, so the step's process group could not be found"
	case groups == "" && survivors == "":
		return "no process carrying this step's token was still running"
	case survivors == "":
		return "killed process group(s) " + groups + ", none left"
	default:
		return "killed process group(s) " + groups + ", still present: " + survivors
	}
}

// processGroup is the step's remote process group id, learned while the
// step was running and read when it has to be stopped.
type processGroup struct {
	mu sync.Mutex
	id string
}

func (g *processGroup) get() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.id
}

func (g *processGroup) set(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.id = id
}

// capturePGID learns the step's remote process GROUP while its leader is
// still there to be found, and hands the answer to termination later.
//
// It exists because the token is on the LEADER's command line and nowhere
// else. A hook whose bash has exited -- killed by a signal the server sent,
// or finished while a child it started holds the channel open -- leaves a
// process group with this product's work in it and nothing left carrying
// the token, so a reaper that could only match the command line would
// report "nothing was running" about a group that is still running.
//
// It costs one short session, and only for a step that lasts longer than
// the first attempt's delay: a step that ends first is a step with nothing
// to terminate. A failure is silent on purpose -- the reaper's
// command-line match is the primary mechanism and this is the second
// handle on the same thing, so a host with no usable ps loses the backstop
// and keeps the step.
func (c *Client) capturePGID(ctx context.Context, token string, finished <-chan struct{}) *processGroup {
	group := &processGroup{}
	base := context.WithoutCancel(ctx)

	go func() {
		elapsed := time.Duration(0)
		for _, at := range pgidCaptureAttempts {
			timer := time.NewTimer(at - elapsed)
			select {
			case <-finished:
				timer.Stop()

				return
			case <-timer.C:
			}
			elapsed = at

			if id := c.scanForGroup(base, token); id != "" {
				group.set(id)

				return
			}
		}
	}()

	return group
}

// scanForGroup asks the far side for the process group of whatever carries
// this step's token, and returns "" for every way that can fail to answer.
//
// It is groupScript plus the one line that reports the answer: the reaper
// prints the same "groups=" line before it kills anything, so the capture
// and the reaper read each other's output in one format.
func (c *Client) scanForGroup(ctx context.Context, token string) string {
	scanCtx, cancel := context.WithTimeout(ctx, pgidCaptureTimeout)
	defer cancel()

	scan := groupScript(token) + reportGroups

	var stdout, stderr bytes.Buffer
	if _, err := c.runOnce(scanCtx, pgidCaptureTimeout, c.remoteCommand(""), []byte(scan), &stdout, &stderr); err != nil {
		return ""
	}

	return firstGroup(stdout.String())
}

// reportGroups is the line that puts $groups on stdout.
const reportGroups = "printf 'groups=%s\\n' \"$groups\"\n"

// firstGroup reads the first usable group id out of the scan's own
// "groups=" line. The guard is pgidRule's, applied here as well as on the
// far side: a number is the only thing that may be handed back to a kill.
func firstGroup(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "groups=")
		if !found {
			continue
		}
		for _, field := range strings.Fields(value) {
			if pgidRule.MatchString(field) && field != "0" && field != "1" {
				return field
			}
		}
	}

	return ""
}

// pgidRule is what a process group id read back from the far side must
// look like before it may become part of a kill. See reaperScript's own
// argument about "kill -TERM -1".
var pgidRule = regexp.MustCompile(`^[0-9]+$`)

// runOnce runs one fixed-command session to completion with bytes on stdin,
// capturing both streams into buffers, and returns the exit status.
//
// It is the shape every internal session shares: the probe, the reaper, the
// process-group scan and the syntax check all send a script this package
// wrote and want the whole answer. A hook is the one thing that does NOT go
// through it, because a hook's output has to be streamed to the log layer
// as it arrives rather than buffered.
//
// Every step of it is bounded by ctx, including opening the channel and
// writing the payload. A session that could not be opened within the budget
// is transport loss, not a slow answer: the two are told apart by the
// caller's sentinel, and neither may hang.
func (c *Client) runOnce(ctx context.Context, timeout time.Duration, command string, payload []byte, stdout, stderr *bytes.Buffer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session, err := c.session(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = session.Close() }()

	session.Stdout = stdout
	session.Stderr = stderr

	stdin, err := session.StdinPipe()
	if err != nil {
		return 0, fmt.Errorf("%w: opening stdin on an internal exec channel: %v", ErrTransportLoss, err)
	}
	if err := session.Start(command); err != nil {
		return 0, fmt.Errorf("%w: starting an internal exec channel: %v", ErrTransportLoss, err)
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- writePayload(stdin, payload) }()

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case waitErr := <-done:
		// Unlike a hook (see Run), an internal session's answer is
		// only worth having if the whole payload reached the far
		// side, so here the write's verdict still fails the session.
		if writeErr := drainWrite(session, writeDone); writeErr != nil {
			return 0, writeErr
		}
		if waitErr == nil {
			return 0, nil
		}
		var exitErr *ssh.ExitError
		if errors.As(waitErr, &exitErr) {
			// A non-zero status is an ANSWER from a probe or a reaper,
			// not a failure: "this account cannot exec" is exactly what
			// exit 1 with an sftp banner means.
			return exitErr.ExitStatus(), nil
		}

		return 0, fmt.Errorf("%w: an internal exec channel on %s failed: %v", ErrTransportLoss, c.describeEndpoint(), waitErr)
	case <-ctx.Done():
		return 0, fmt.Errorf("%w: an internal exec channel on %s did not answer within %s", ErrTransportLoss, c.describeEndpoint(), timeout)
	}
}

// reaperScript finds every process in this step's process GROUP, sends the
// group a TERM and then a KILL, and reports what is left.
//
// # Why the process group and not the process
//
// Because a hook has children, and the thing that must stop is the hook's
// work rather than its shell. Measured on the fixture: sshd puts the exec'd
// command in its own process group (the session leader's group, distinct
// from sshd's own), so signalling the group reaches the hook and everything
// it started while reaching nothing of the server's.
//
// # The two ways the group is found
//
// The token on the leader's command line is the first, and the group id
// captured while the step was running (capturePGID) is the second. They are
// both used because they fail in different circumstances: the command-line
// match needs the leader to still exist, and the captured id needs the step
// to have lasted long enough to be seen. Either one alone leaves a case
// where a hook's children keep running with nothing to kill them by.
//
// # The guard on the group id
//
// A pgid is only used if it is a number greater than 1. That is not
// defensive decoration: "kill -TERM -1" signals every process the account
// owns, and "kill -TERM -0" signals the reaper's own group. A malformed ps
// line -- an unusual ps, a locale, a command containing a newline -- is the
// only way either could be reached, and the cost of the guard is a string
// comparison.
//
// # Why POSIX sh constructs only
//
// This runs on somebody else's host. The reaper deliberately uses no
// here-document (bash implements one with a temporary file, and this
// package leaves nothing on the remote host), no arrays, and no bashism
// beyond what /bin/sh provides, so it behaves the same if a future
// connection ever runs it under a different shell.
func reaperScript(token, pgid string) string {
	return groupScript(token) + `captured=` + workflowexec.ShellQuote(pgid) + `
case "$captured" in
  ''|*[!0-9]*|0|1) ;;
  *) case " $groups " in *" $captured "*) ;; *) groups="$groups$captured " ;; esac ;;
esac
if ! ps -A -o pid= >/dev/null 2>&1; then printf 'ps=missing\n'; fi
` + reportGroups + `for g in $groups; do kill -TERM -"$g" 2>/dev/null; done
sleep 1
for g in $groups; do kill -KILL -"$g" 2>/dev/null; done
survivors=$(ps -A -o pgid= 2>/dev/null | while IFS= read -r p; do
  for g in $groups; do
    [ "$p" = "$g" ] && printf '%s ' "$p"
  done
done)
printf 'survivors=%s\n' "$survivors"
`
}

// groupScript sets $groups to the process groups whose leader carries this
// step's token, and is shared by the reaper and the capture so the two
// cannot come to recognise a step differently.
//
// # Why the token is matched as an OPERAND and not as a substring
//
// Because tokens of concurrent steps are related strings. The remote
// command ends in "-s <token>", so a substring match on the whole command
// line makes a step whose token is a PREFIX of another step's token match
// that other step -- and the reaper then kills a process group belonging to
// a hook that is running normally, on a different backup set, reporting it
// as a successful termination of this one. The match is therefore on the
// argument that follows "-s", compared whole.
func groupScript(token string) string {
	return `tok=` + workflowexec.ShellQuote(token) + `
groups=$(ps -A -o pid=,pgid=,args= 2>/dev/null | while IFS= read -r line; do
  set -- $line
  [ $# -ge 3 ] || continue
  pgid=$2
  shift 2
  case "$pgid" in ''|*[!0-9]*) continue ;; 0|1) continue ;; esac
  prev=''
  for arg in "$@"; do
    if [ "$prev" = "-s" ] && [ "$arg" = "$tok" ]; then printf '%s ' "$pgid"; break; fi
    prev=$arg
  done
done)
`
}
