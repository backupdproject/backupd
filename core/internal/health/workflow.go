package health

import (
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is FR-24's workflow half (EPIC L, issue #813): not "is the
// process the build it claims to be", which ProcessHealth answers, and
// not "are this backup set's restore points fresh", which the rest of the
// package answers, but "can this deployment still RUN the hooks its
// backups depend on, and is there a run nobody has accounted for".
//
// # Why this is a third question and not a field on either existing half
//
// Because it is neither of the two the package doc separates, and folding
// it into either one would break the separation that doc exists to
// enforce. A workflow runner that has stopped answering is not a
// backup-set fact: it is one process-level fact that ruins every backup
// set at once, and putting it on BackupSetHealth would mean copying the
// same reading onto each set and inviting somebody to let it decide a
// per-set State. It is not a ProcessHealth fact either: ProcessHealth is
// deliberately a type with nowhere to put anything learned by talking to
// something else, and a reachability probe is exactly that.
//
// So it sits on Report beside the other two, as a value nothing in this
// package computes. Every fact here is established by a layer that can
// actually observe it -- internal/hostrunner's preflight, internal/
// remoteexec's capability check, internal/workflowrun's recovery holds --
// and this package holds the answers so one already-computed Report can
// be rendered by the CLI, the HTTP surface and the exporter without any
// of the three repeating those probes.
//
// # Why the zero value is the answer for most deployments
//
// The overwhelmingly common deployment configures no workflows at all,
// and it must cost nothing and change nothing. So WorkflowHealth is a
// plain value whose zero value reads "this deployment runs no workflows":
// nothing configured, no runner, no connections, no holds, and OK()
// true. That is what lets Report grow this section without a single
// existing NewReport call site or test changing, and it is why Configured
// is a field rather than something inferred from the emptiness of the
// three collections beside it -- a deployment can declare workflows.root
// and no exec connections, and "configured with nothing declared" and
// "not configured" are different answers to different questions.

// WorkflowHealth is the workflow half of one FR-24 report: whether this
// deployment can execute hooks, and whether any interrupted run is still
// waiting for a person.
//
// Its zero value is the honest reading for a deployment that runs no
// workflows. See the file doc for why that matters and why Configured is
// explicit.
type WorkflowHealth struct {
	// Configured says workflows.root is set, which is what makes every
	// other field here meaningful: an unreachable runner on a
	// deployment that declares no hooks is not a problem anybody needs
	// to be told about, and a surface that reported one would be
	// teaching operators to ignore this section.
	Configured bool

	// Runner is the Host Workflow Runner this engine reaches for
	// NAME.local.sh hooks (internal/hostrunner).
	//
	// One runner, not a list, because there is exactly one host this
	// engine is installed on and the runner is the door back out to it.
	Runner WorkflowRunnerHealth

	// ExecConnections is one entry per DECLARED execution connection
	// (workflows.exec_connections), whether or not a backup set
	// currently references it.
	//
	// Declared rather than referenced, because the failure this is for
	// is an operator declaring a connection, referencing it from a set
	// they enable next month, and finding out then that the far side
	// never had the capabilities. A connection nobody references yet is
	// still a connection whose capability answer is known now and free
	// to report.
	ExecConnections []WorkflowExecConnectionHealth

	// RecoveryHolds is every unresolved recovery_required scope, one
	// entry per (run, scope). Empty is the ordinary reading and the one
	// a working deployment sits at.
	//
	// A LIST rather than a count, and this is the field that most
	// wanted to be a number. The reason is what an operator does next:
	// the only two exits from recovery_required are resuming the run's
	// cleanup or acknowledging it with a reason (internal/workflowrun's
	// doc), and both of those commands take a RUN ID. A count tells an
	// operator that they must go and find the ids themselves, out of a
	// journal, at the moment they are least inclined to -- which is how
	// a hold gets acknowledged in bulk by somebody who never read which
	// run it was. The count is len(), and every surface that wants one
	// can take it.
	RecoveryHolds []WorkflowRecoveryHold
}

// OK reports whether the workflow half needs no attention.
//
// The three ways it is false are the three ways a hook stops being
// something this deployment can rely on: the door to the host is shut,
// a declared far side cannot be driven, or a previous run left a side
// effect nobody has accounted for. Note what is NOT here: a hook that
// ran and failed. That is a backup set's own verdict, it is already
// counted per set and on the run row, and letting it also turn this
// section red would mean one bad script made the whole workflow
// subsystem look broken.
//
// The runner is only judged when it is Configured, for the file doc's
// reason: a deployment with no hooks has no runner and is not thereby
// unhealthy.
func (w WorkflowHealth) OK() bool {
	if w.Runner.Configured && !w.Runner.Reachable {
		return false
	}
	for _, c := range w.ExecConnections {
		if !c.Capable {
			return false
		}
	}

	return len(w.RecoveryHolds) == 0
}

// WorkflowRunnerHealth is what this engine knows about the Host Workflow
// Runner after its preflight: whether it is there, what it is, and one
// sentence about it for a person.
type WorkflowRunnerHealth struct {
	// Configured says this deployment declares a runner
	// (workflows.runner). Without it, every NAME.local.sh hook is
	// unrunnable by construction rather than by failure, and the rest
	// of this struct is empty.
	Configured bool

	// Reachable says a handshake completed: the socket was there, the
	// credential was accepted, and the runner answered with its status.
	Reachable bool

	// Version is the runner's product version, from that status frame.
	//
	// It is carried SEPARATELY from Reachable rather than folded into
	// it, because a runner that answers with the wrong version is a
	// completely different problem from one that does not answer, and
	// the two remedies have nothing in common: a socket that is not
	// there means start the runner, or fix the directory permissions,
	// or fix the path the engine was pointed at, while a version that
	// does not match means upgrade the runner package to the release
	// this engine came from (hostrunner.Hello.Version requires
	// equality, exactly, and says why). Collapsing both into one
	// "healthy" bit would send an operator to strace a socket that is
	// working perfectly.
	//
	// The protocol refuses a mismatched engine rather than serving it,
	// so a version mismatch arrives here as Reachable false with this
	// field holding whatever the refusal named -- which is precisely
	// the case a single bit cannot express and this pair can.
	Version string

	// BashVersion is the first line of the interpreter's own
	// `bash --version`, as the runner reported it (hostrunner.Bash).
	//
	// It is carried because "which bash ran my hook" is a question an
	// operator writing a hook asks before they write it, and because a
	// NAS whose bash is from 2007 is a real deployment. It is reported
	// and never parsed: nothing in this product branches on a bash
	// version, and a parser would be one more thing that can be wrong
	// about a string whose only job is to be shown to a human.
	BashVersion string

	// Detail is one sentence for an operator about the state above: why
	// the handshake failed, or what the runner said when it refused.
	//
	// It is an operator sentence and never a filesystem path, a socket
	// path, a credential or any part of one. A socket path IS a path,
	// and it is the one a careless error message would carry here,
	// because the error a dialer returns has it in the text: this field
	// is the sentence somebody WROTE for a person, not the error string
	// somebody happened to have. The reason is where this ends up --
	// internal/metrics never renders it, but a CLI prints it, an HTTP
	// handler serializes it, and both of those get screenshotted into
	// support tickets. internal/hostrunner's package doc makes the same
	// argument about its wire: no field that can hold a path.
	Detail string
}

// WorkflowExecConnectionHealth is one declared execution connection's
// remote-exec capability answer (#810, internal/remoteexec).
type WorkflowExecConnectionHealth struct {
	// Ref is the connection's declared id -- the name a backup set's
	// remote_exec_connection_ref resolves against. It is an id from the
	// configuration and never a host, a user, a port or an endpoint:
	// those identify a machine somebody is trying not to publish, and
	// this identifies a line in their config file.
	Ref string

	// Capable says the far side was proven able to run this product's
	// remote hooks: the connection opened and the capabilities the
	// executor needs were established.
	//
	// Proven, not assumed. A connection this pass could not check is
	// not Capable, because the only alternative is reporting an
	// unproven far side as working, and the run that discovers
	// otherwise discovers it with a database already quiesced.
	Capable bool

	// Detail is one sentence for an operator about why a connection is
	// not capable. Like the runner's, it is written for a person and
	// carries no path, no endpoint and no credential.
	Detail string
}

// WorkflowRecoveryHold is one scope of one interrupted run that nobody
// has accounted for: the thing that blocks a backup set until a person
// resumes its cleanup or acknowledges it with a reason.
//
// It mirrors internal/workflowrun's RecoveryHold in this package's own
// vocabulary rather than importing that type, which is the same trade
// AwayFromHome makes in mediums.go and for the same reason: the engine is
// a large dependency to acquire for four fields, and this package must
// not gain a way to reach a recovery decision it could then make
// differently. The mapping at the call site is one loop.
type WorkflowRecoveryHold struct {
	// RunID is the run an operator's next command has to name. See
	// WorkflowHealth.RecoveryHolds for why this field is the whole
	// reason the holds are a list.
	RunID string

	// BackupSet is the set this hold blocks. It is carried beside the
	// run because the refusal it drives is per SET (workflow's
	// CleanupObligation makes the same point): an operator reading a
	// hold needs to know which backups have stopped happening, and a
	// surface that had to join the run back to its set to say so would
	// be a surface that sometimes does not bother.
	BackupSet model.BackupSetID

	// Scope is the cleanup scope left unaccounted for, from
	// workflow.Scope's closed vocabulary ("global", "set"), as a string
	// for the reason the type doc gives.
	Scope string

	// EnteredAt is when this hold started, which is what turns it from
	// a fact into a priority: a hold from four minutes ago is a machine
	// that just restarted, and one from nine days ago is a backup set
	// that has taken no backups for nine days and nobody noticed.
	EnteredAt time.Time
}
