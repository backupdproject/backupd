package workflowrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/backupdproject/backupd/core/internal/hostrunner"
	"github.com/backupdproject/backupd/core/internal/remoteexec"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// The one seam between "what to run next" and "how a hook actually runs".
//
// # Why there is an interface here and not two branches
//
// Because the engine has to be unable to tell the difference. #811's
// ordering requirement is that the sequence is the plan's, and the way
// that goes wrong in practice is a stage loop that grows a special case
// for one target -- a retry the local path can afford, an extra probe the
// remote path needs -- until the two halves sequence differently. One
// interface with one mapping from outcome to state means there is exactly
// one sequencing.
//
// # Why the outcome is a DISPOSITION and not an exit code
//
// #810's technical requirement, and #811 repeats it: transport loss is
// never reported as a known exit code. So the shape of an answer is "what
// happened to this process" first, and an exit code only in the one case
// where a process exited and this product observed the status. The
// journal enforces the same rule from the other side
// (state.WorkflowStepOutcome), which is deliberate: a field that must be
// nil in five cases out of six is one that will be set by accident, so it
// is refused in two places.

// Disposition is what became of one step's process, from the engine's
// point of view.
type Disposition string

// The six dispositions. There is no "unknown": an adapter that cannot say
// what happened returns DispositionTransportLost, which is the honest
// answer and the one that keeps the exit code nil.
const (
	// DispositionExited is a process that ran and whose exit status this
	// product observed. It is the ONLY disposition that may carry an
	// exit code, and it may carry a non-zero one.
	DispositionExited Disposition = "exited"

	// DispositionTimedOut is a process stopped for outliving its bound.
	DispositionTimedOut Disposition = "timed_out"

	// DispositionCanceled is a process stopped because an operator, a
	// shutdown or a cancelled run asked.
	DispositionCanceled Disposition = "canceled"

	// DispositionTransportLost is a step whose outcome nobody observed:
	// the connection failed, the channel closed with no status, the
	// runner's socket went away mid-step. The side effects may be
	// half-applied and the exit code is nil, because there was none.
	DispositionTransportLost Disposition = "transport_lost"

	// DispositionNotAttempted is a step that never started: a runner
	// that refused the bytes, a capability that could not be proven, a
	// connection that would not open. Nothing of the hook ran, which is
	// a different fact from an outcome nobody saw.
	DispositionNotAttempted Disposition = "not_attempted"

	// DispositionSignaled is a process killed by a signal on the far
	// side -- an OOM killer, an operator on that host, this product's
	// own reaper. Not an exit code, and never recorded as one.
	DispositionSignaled Disposition = "signaled"
)

// StepRequest is one step, as the engine states it to an adapter.
//
// It carries the verified BYTES and no path, which is #808's contract
// made visible in a type: an adapter holding this has nothing it could
// open in the workflow tree.
type StepRequest struct {
	RunID string

	// Step is the planned step: its scope, phase, target, script name,
	// hash, timeout and (for a remote step) its execution connection.
	Step workflow.Step

	// Script is the captured copy, opened through the plan and verified
	// against the sha256 taken at snapshot time.
	Script workflow.SpooledScript

	// Environ is the fully resolved environment as NAME=VALUE entries.
	// The values are material -- this is the last hop before a process
	// -- and nothing in this package logs or persists one.
	Environ []string

	// Timeout is the step's own bound, resolved at snapshot time.
	Timeout time.Duration

	// Sink receives the output as it arrives.
	Sink workflowexec.Sink
}

// StepOutcome is what an adapter observed.
type StepOutcome struct {
	Disposition Disposition

	// ExitCode is set only for DispositionExited.
	ExitCode *int

	// Certainty is what was PROVED about a termination this product
	// requested. TerminationNotRequested for a step that ended by
	// itself.
	Certainty workflowexec.TerminationCertainty

	// Chunks is how many output chunks the step produced, so a consumer
	// can tell a truncated stream from a silent hook.
	Chunks uint64

	// Detail is a short operator-facing note -- what a reaper found, why
	// a step could not be attempted. It never contains an environment
	// value.
	Detail string
}

// validate refuses an outcome that would make the journal lie.
//
// The exit-code rule is the whole of it, and it is checked here as well
// as in the journal because this is where an adapter's mistake enters the
// engine: an outcome that says "transport lost, exit code 0" would
// otherwise be turned into a step recorded as having failed with a status
// somebody could read as meaningful.
func (o StepOutcome) validate() error {
	if o.Disposition == "" {
		return errors.New("workflowrun: an executor returned no disposition, so what became of the hook is unknown and must not be guessed at")
	}
	if o.ExitCode != nil && o.Disposition != DispositionExited {
		return fmt.Errorf("workflowrun: an executor reported %q with exit code %d; only a process that exited and whose status this product observed has one, and transport loss is never a known exit code",
			o.Disposition, *o.ExitCode)
	}
	if o.ExitCode == nil && o.Disposition == DispositionExited {
		return errors.New("workflowrun: an executor reported that a hook exited and gave no status, which is transport loss reported as a clean exit")
	}
	if !o.Certainty.Valid() {
		return fmt.Errorf("workflowrun: an executor reported termination certainty %q", o.Certainty)
	}

	return nil
}

// state maps a disposition onto the step state the journal records.
//
// DispositionSignaled, TransportLost and NotAttempted all become
// StateFailed, and all three leave the exit code nil. They are separate
// dispositions because the DETAIL an operator is shown differs, and one
// state because the engine's own next decision is the same in all three:
// this step did not succeed.
func (o StepOutcome) state() workflow.State {
	switch o.Disposition {
	case DispositionExited:
		if o.ExitCode != nil && *o.ExitCode == 0 {
			return workflow.StateSuccess
		}

		return workflow.StateFailed
	case DispositionTimedOut:
		return workflow.StateTimedOut
	case DispositionCanceled:
		return workflow.StateCanceled
	default:
		return workflow.StateFailed
	}
}

// Executor runs one step's verified bytes somewhere.
//
// An error return means the step could not be ATTEMPTED in a way the
// adapter could describe; a hook that ran and failed is an outcome, not
// an error. The engine records both, differently: one names the step as
// the failure, the other names it as the failure and says the hook never
// ran.
type Executor interface {
	ExecuteStep(ctx context.Context, req StepRequest) (StepOutcome, error)
}

// LocalExecutor dispatches a NAME.local.sh to the host runner (#809).
//
// It is a few lines because the runner's client is the adapter: the
// engine's job here is only to translate one request shape into another
// and one answer shape back. What it must not do is interpret -- a
// runner's refusal code is the runner's to explain, and re-deriving a
// verdict from it here would be a second opinion about what happened.
type LocalExecutor struct {
	Client hostrunner.Client
}

// ExecuteStep runs one local step.
//
// The context is the lease as well as the bound: the client closes the
// connection when it is cancelled, the runner reads that as the lease
// expiring, and the hook's process GROUP is terminated. That is the same
// mechanism an engine crash uses, which is why cancellation takes it
// rather than a separate path nobody exercises.
func (e LocalExecutor) ExecuteStep(ctx context.Context, req StepRequest) (StepOutcome, error) {
	env := make([]hostrunner.EnvVar, 0, len(req.Environ))
	for _, kv := range req.Environ {
		name, value, found := cutEnv(kv)
		if !found {
			return StepOutcome{}, fmt.Errorf("workflowrun: the environment entry %q for step %s has no '='", name, req.Step.ID)
		}
		env = append(env, hostrunner.EnvVar{Name: name, Value: value})
	}

	result, err := e.Client.Execute(ctx, hostrunner.ExecuteRequest{
		RunID:   req.RunID,
		StepID:  req.Step.ID,
		Script:  req.Script.Body,
		Env:     hostrunner.EnvSet{Vars: env},
		Timeout: req.Timeout,
	}, hostrunner.SinkFunc(func(c hostrunner.Chunk) error {
		if req.Sink == nil {
			return nil
		}

		return req.Sink.Chunk(workflowexec.Chunk{
			Stream: workflowexec.StreamID(c.Stream),
			Seq:    c.Seq,
			Data:   c.Data,
		})
	}))
	if err != nil {
		return localFailure(err), nil
	}

	return localOutcome(result), nil
}

// localFailure turns a client-side error into an outcome.
//
// A *hostrunner.Failure is the runner having REFUSED -- bad bytes, an
// unknown operation, no bash -- so nothing ran: DispositionNotAttempted.
// Anything else is the socket, which is transport loss. The distinction
// is what an operator needs: one is something to fix in the
// configuration, the other is something to fix in the deployment.
func localFailure(err error) StepOutcome {
	var refusal *hostrunner.Failure
	if errors.As(err, &refusal) {
		return StepOutcome{
			Disposition: DispositionNotAttempted,
			Detail:      refusal.Error(),
		}
	}

	return StepOutcome{
		Disposition: DispositionTransportLost,
		Detail:      err.Error(),
	}
}

func localOutcome(r hostrunner.Result) StepOutcome {
	out := StepOutcome{
		Chunks:    r.Chunks,
		Certainty: localCertainty(r.TerminationCertainty),
	}

	switch r.State {
	case hostrunner.StateExited:
		out.Disposition = DispositionExited
		out.ExitCode = r.ExitCode
	case hostrunner.StateTimedOut:
		out.Disposition = DispositionTimedOut
	case hostrunner.StateCanceled, hostrunner.StateLeaseExpired:
		out.Disposition = DispositionCanceled
		out.Detail = string(r.State)
	default:
		// A state this build does not know is transport loss rather than
		// a guess: the runner is the other half of this program, so an
		// answer it gives that this half cannot read means the two are
		// not the pair they are supposed to be.
		out.Disposition = DispositionTransportLost
		out.Detail = "the runner reported state " + string(r.State)
	}

	// A runner that exited cleanly still reports an exit code; one that
	// was killed must not, and the runner's own contract already says
	// so. Enforced rather than trusted, because this is the boundary.
	if out.Disposition != DispositionExited {
		out.ExitCode = nil
	}

	return out
}

func localCertainty(c hostrunner.TerminationCertainty) workflowexec.TerminationCertainty {
	switch c {
	case hostrunner.CertaintyConfirmed:
		return workflowexec.TerminationConfirmed
	case hostrunner.CertaintyUnconfirmed:
		return workflowexec.TerminationUnconfirmed
	default:
		return workflowexec.TerminationNotRequested
	}
}

// RemoteExecutor dispatches a NAME.remote.sh over an exec-capable SSH
// connection (#810).
//
// Connections are resolved per step through Connect rather than held,
// because a capability is bound to a connection and a script (see
// remoteexec.Capability): a client cached across a run would be a proof
// that outlived the thing it was a proof about.
type RemoteExecutor struct {
	// Connect opens (or returns) the client for one execution
	// connection reference. It is a function rather than a map so the
	// engine does not have to know how connections are configured.
	Connect func(ctx context.Context, ref string) (*remoteexec.Client, error)
}

// ExecuteStep runs one remote step.
func (e RemoteExecutor) ExecuteStep(ctx context.Context, req StepRequest) (StepOutcome, error) {
	if e.Connect == nil {
		return StepOutcome{}, errors.New("workflowrun: the remote executor has no way to reach a connection")
	}

	client, err := e.Connect(ctx, req.Step.ExecutionConnectionRef)
	if err != nil {
		return StepOutcome{
			Disposition: DispositionNotAttempted,
			Detail:      err.Error(),
		}, nil
	}

	result, runErr := client.Run(ctx, remoteexec.Request{
		Token:        req.Step.ID,
		Environ:      req.Environ,
		Script:       req.Script.Body,
		Sink:         req.Sink,
		Timeout:      req.Timeout,
		BackupSet:    req.Step.RunID,
		StepID:       req.Step.ID,
		ScriptName:   req.Step.ScriptName,
		ScriptSHA256: req.Step.ScriptSHA256,
	})

	return remoteOutcome(result, runErr), nil
}

// remoteOutcome reads the remote client's answer.
//
// The ORDER of these tests is the contract. A timeout and a cancellation
// are told apart by sentinel before anything looks at the exit code,
// because a step that was killed may still carry a signal name and must
// not be read as having reported anything; and an observed exit code is
// only believed when there is no error at all, which is what keeps
// transport loss out of the exit-code field.
func remoteOutcome(r remoteexec.Result, err error) StepOutcome {
	out := StepOutcome{
		Chunks:    r.Chunks,
		Certainty: r.Certainty,
		Detail:    r.Reaper,
	}

	switch {
	case errors.Is(err, remoteexec.ErrStepTimeout):
		out.Disposition = DispositionTimedOut
	case errors.Is(err, remoteexec.ErrStepCanceled):
		out.Disposition = DispositionCanceled
	case errors.Is(err, remoteexec.ErrStepSignaled):
		out.Disposition = DispositionSignaled
		out.Detail = "killed by " + r.Signal + " on the remote host"
	case err != nil:
		out.Disposition = DispositionTransportLost
		out.Detail = err.Error()
	case r.ExitCode == nil:
		// No error and no status is the case the remote package is
		// careful about (an *ssh.ExitMissingError): the channel closed
		// tidily and said nothing. It is transport loss however tidy it
		// looked, and reporting it as a clean exit is the silent success
		// ADR 0021 exists to refuse.
		out.Disposition = DispositionTransportLost
		out.Detail = "the remote session ended without reporting a status"
	default:
		out.Disposition = DispositionExited
		out.ExitCode = r.ExitCode
	}

	if out.Disposition != DispositionExited {
		out.ExitCode = nil
	}

	return out
}

// cutEnv splits a NAME=VALUE entry. strings.Cut, spelled out because the
// "not found" case is a refusal here rather than an empty value.
func cutEnv(kv string) (name, value string, found bool) {
	for i := range len(kv) {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}

	return kv, "", false
}
