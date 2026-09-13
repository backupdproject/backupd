package remoteexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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
	// milliseconds; older and other servers do not implement it. So this
	// window is short, and the reaper behind it is what makes termination
	// work on a server that ignores signals rather than a fallback nobody
	// reaches.
	signalGrace = 3 * time.Second

	// reapGrace is how long termination waits after the reaper has killed
	// the step's process group. The reaper has already waited for its own
	// TERM-then-KILL, so this only covers the remote side closing the
	// channel.
	reapGrace = 5 * time.Second

	// reaperTimeout bounds the reaper session itself.
	reaperTimeout = 30 * time.Second
)

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
// The whole envelope is decided elsewhere and applied here: the fixed
// remote command (Client.remoteCommand), the stdin payload
// (workflowexec.StdinPayload), and the two separate capture streams sharing
// one sequence counter (workflowexec.Capture). What this function owns is
// the waiting and the stopping.
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

	session, err := c.session()
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

	writeErr := writePayload(stdin, payload)

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	select {
	case waitErr := <-done:
		result.FinishedAt = time.Now()
		result.Chunks = capture.Sequence()
		result.Certainty = workflowexec.TerminationNotRequested
		if writeErr != nil {
			return result, writeErr
		}

		return c.finish(result, waitErr)

	case <-runCtx.Done():
		reason := runCtx.Err()
		c.terminate(ctx, req.Token, session, done, &result)
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
//  1. an exec-channel signal request. The protocol has one; a server may
//     ignore it. On a server that honours it this is the whole story and it
//     takes milliseconds.
//  2. a reaper session, which finds the step's process GROUP by the token
//     on its command line and sends it TERM and then KILL. This is what
//     works on a server that ignores signal requests, and it is also the
//     only thing that reaches a hook's own children.
//  3. closing the channel, which is all that is left.
//
// Certainty is decided by ONE rule, and not by which of the three steps ran:
// confirmed if the session COMPLETED -- exit reported, both streams at end
// of file -- and unconfirmed otherwise. That is why a deliberately detached
// descendant reports unconfirmed: it keeps the channel's stdout open, so the
// session never completes, and this product will not claim a clean stop it
// cannot see.
func (c *Client) terminate(ctx context.Context, token string, session *ssh.Session, done <-chan error, result *Result) {
	result.Certainty = workflowexec.TerminationUnconfirmed

	// The context that brought us here is already cancelled, so every
	// step below gets its own budget from a context that is not.
	base := context.WithoutCancel(ctx)

	_ = session.Signal(ssh.SIGTERM)
	if waitFor(done, signalGrace) {
		result.Certainty = workflowexec.TerminationConfirmed

		return
	}

	result.Reaper = c.reap(base, token)
	if waitFor(done, reapGrace) {
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
func (c *Client) reap(ctx context.Context, token string) string {
	ctx, cancel := context.WithTimeout(ctx, reaperTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	// No token on the reaper's own command line: it is looking for
	// processes carrying that token, and a reaper that matched itself
	// would kill its own process group.
	if _, err := c.runOnce(ctx, reaperTimeout, "", []byte(reaperScript(token)), &stdout, &stderr); err != nil {
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

// runOnce runs one fixed-command session to completion with bytes on stdin,
// capturing both streams into buffers, and returns the exit status.
//
// It is the shape every internal session shares: the probe, the reaper and
// the syntax check all send a script this package wrote and want the whole
// answer. A hook is the one thing that does NOT go through it, because a
// hook's output has to be streamed to the log layer as it arrives rather
// than buffered.
func (c *Client) runOnce(ctx context.Context, timeout time.Duration, token string, payload []byte, stdout, stderr *bytes.Buffer) (int, error) {
	session, err := c.session()
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
	if err := session.Start(c.remoteCommand(token)); err != nil {
		return 0, fmt.Errorf("%w: starting an internal exec channel: %v", ErrTransportLoss, err)
	}
	if err := writePayload(stdin, payload); err != nil {
		return 0, err
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case waitErr := <-done:
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

// reaperScript finds every process whose command line carries this step's
// token, sends its process GROUP a TERM and then a KILL, and reports what
// is left.
//
// # Why the process group and not the process
//
// Because a hook has children, and the thing that must stop is the hook's
// work rather than its shell. Measured on the fixture: sshd puts the exec'd
// command in its own process group (the session leader's group, distinct
// from sshd's own), so signalling the group reaches the hook and everything
// it started while reaching nothing of the server's.
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
func reaperScript(token string) string {
	quoted := workflowexec.ShellQuote(token)

	return `tok=` + quoted + `
groups=$(ps -A -o pid=,pgid=,args= 2>/dev/null | while IFS= read -r line; do
  set -- $line
  [ $# -ge 3 ] || continue
  pgid=$2
  shift 2
  case "$pgid" in ''|*[!0-9]*) continue ;; 0|1) continue ;; esac
  case "$*" in *"$tok"*) printf '%s ' "$pgid" ;; esac
done)
if ! ps -A -o pid= >/dev/null 2>&1; then printf 'ps=missing\n'; fi
printf 'groups=%s\n' "$groups"
for g in $groups; do kill -TERM -"$g" 2>/dev/null; done
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
