package service

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/remoteexec"
)

// FR-24's workflow section, filled in (#813): can this deployment still
// run its hooks, and is anybody's machine waiting for a person.
//
// # Why two of the three answers are cached and one is not
//
// The recovery holds are free: they are the engine's own in-memory
// refusal set, established by the startup reconciliation and moved only
// by writes that go through this process. Reading them is a slice copy.
//
// The two PROBES are not free, and one of them is expensive. Asking the
// host runner what it is costs a connect to a Unix socket, which is
// cheap but not nothing on a NAS under a full backup (hostrunner's own
// DialTimeout doc says so). Asking an execution connection whether it can
// run a command costs a TCP connect, an SSH handshake, a key exchange and
// a probe script -- against a machine that may be asleep, on the other
// side of a VPN, or down.
//
// Health is polled. A dashboard asks every ten seconds, `status` asks
// whenever somebody types it, and an exporter asks on every scrape. A
// health report that opened an SSH connection per declared execution
// connection per poll would be this product logging into the source host
// six times a minute forever, which is both a load an operator would
// notice in their auth log and a way to get rate-limited out of the host
// this deployment backs up.
//
// So the probes are cached with a short TTL and the cache is what makes
// the section affordable. The TTL is short enough that a runner somebody
// has just restarted shows up within a poll or two, and long enough that
// a dashboard left open overnight is not a login attempt every ten
// seconds.
//
// # Why a stale reading is reported rather than a refusal
//
// Because the alternative is worse. A health call whose probe budget has
// not expired reports the last answer, which is at most one TTL old and
// is exactly what an operator would have been shown a moment earlier. A
// call that instead blocked to re-probe would make the health endpoint's
// latency the source host's latency, and an unreachable host would make
// the whole health report time out -- turning "one execution connection
// is down" into "this deployment cannot report its own health", which is
// the failure this product spends most of internal/health avoiding.

// workflowProbeTTL is how long a runner or capability answer is reused.
//
// Thirty seconds: three dashboard polls, well under the interval anybody
// waits before re-running `status` after fixing something, and far above
// the rate at which a scrape or a poll would otherwise open connections.
const workflowProbeTTL = 30 * time.Second

// workflowProbeCache holds the last answer and when it was taken.
type workflowProbeCache struct {
	mu       sync.Mutex
	takenAt  time.Time
	runner   health.WorkflowRunnerHealth
	execConn []health.WorkflowExecConnectionHealth
}

// WorkflowHealth is this deployment's workflow posture, for the health
// report and for the exporter.
//
// It is a method rather than a field on HealthReport's builder because it
// takes a context: the probes below are network calls and a caller's
// cancellation has to reach them.
func (b *BackupService) WorkflowHealth(ctx context.Context) health.WorkflowHealth {
	cfg := b.state.Load().inner.Config

	out := health.WorkflowHealth{Configured: cfg.WorkflowsConfigured()}
	if !out.Configured {
		// A deployment that runs no workflows gets the zero value, which
		// health.WorkflowHealth's own doc says reads as exactly that.
		// Probing anyway would connect to a runner nothing would use.
		return out
	}

	out.RecoveryHolds = b.workflowRecoveryHoldsForHealth()

	runner, conns := b.probeWorkflowExecutors(ctx)
	out.Runner = runner
	out.ExecConnections = conns

	return out
}

// workflowRecoveryHoldsForHealth is the engine's refusal set in the
// health package's shape.
//
// A backup set id that this deployment can no longer parse is reported
// with a zero model.BackupSetID rather than dropped: the hold is real and
// blocking, and hiding it because the id is unparseable would be the one
// case where a machine is left quiesced and no surface says so.
func (b *BackupService) workflowRecoveryHoldsForHealth() []health.WorkflowRecoveryHold {
	rt := b.runtime()
	if rt == nil || rt.engine == nil {
		return nil
	}

	holds := rt.engine.RecoveryHolds()
	out := make([]health.WorkflowRecoveryHold, 0, len(holds))

	for _, h := range holds {
		out = append(out, health.WorkflowRecoveryHold{
			RunID:     h.RunID,
			BackupSet: h.BackupSetID,
			Scope:     string(h.Scope),
			EnteredAt: h.EnteredAt,
		})
	}

	return out
}

// probeWorkflowExecutors answers the two reachability questions, from
// cache when the last answer is still fresh.
func (b *BackupService) probeWorkflowExecutors(ctx context.Context) (health.WorkflowRunnerHealth, []health.WorkflowExecConnectionHealth) {
	rt := b.runtime()
	if rt == nil {
		return health.WorkflowRunnerHealth{}, nil
	}

	rt.probes.mu.Lock()
	defer rt.probes.mu.Unlock()

	if !rt.probes.takenAt.IsZero() && now().Sub(rt.probes.takenAt) < workflowProbeTTL {
		return rt.probes.runner, append([]health.WorkflowExecConnectionHealth(nil), rt.probes.execConn...)
	}

	rt.probes.runner = b.probeHostRunner(ctx)
	rt.probes.execConn = b.probeExecConnections(ctx)
	rt.probes.takenAt = now()

	return rt.probes.runner, append([]health.WorkflowExecConnectionHealth(nil), rt.probes.execConn...)
}

// probeHostRunner asks the runner what it is.
//
// Status and not a hook: the runner's own doc calls this the preflight
// answer, and it is the same question `backupd validate` asks and the
// same one the engine asks before it dispatches a local step. Three
// surfaces asking one question through one call is what stops them
// disagreeing about whether a runner is usable.
func (b *BackupService) probeHostRunner(ctx context.Context) health.WorkflowRunnerHealth {
	cfg := b.state.Load().inner.Config

	out := health.WorkflowRunnerHealth{Configured: cfg.Workflows.Runner.Configured()}
	if !out.Configured {
		out.Detail = "no host workflow runner is configured, so this deployment cannot run NAME.local.sh hooks"

		return out
	}

	client, err := b.hostRunnerClient()
	if err != nil {
		out.Detail = err.Error()

		return out
	}

	probeCtx, cancel := context.WithTimeout(ctx, workflowProbeTimeout)
	defer cancel()

	status, err := client.Status(probeCtx)
	if err != nil {
		// The socket path is deliberately not restated here. It is
		// already in the configuration an operator can read, it is a
		// PATH, and health.WorkflowRunnerHealth.Detail is rendered into
		// surfaces where a path has no business being. The runner's own
		// error sentence says what went wrong.
		out.Detail = err.Error()

		return out
	}

	out.Reachable = true
	out.Version = status.Version
	out.BashVersion = status.BashVersion
	out.Detail = "the host workflow runner answered"

	return out
}

// probeExecConnections asks each declared execution connection whether it
// can actually run a command.
//
// It uses Preflight with this package's own tiny script rather than an
// operator's hook, which is the whole reason a capability check is safe
// to run from a health poll: the bytes sent are a fixed `true`, and what
// is being established is that the far side RUNS the bytes it is sent
// rather than a program of its own. #810's motivating case is an account
// confined to internal-sftp, which authenticates perfectly, accepts an
// exec request, runs its own program and exits 0 -- and a check that
// stopped at "the connection opened" would report it as capable.
func (b *BackupService) probeExecConnections(ctx context.Context) []health.WorkflowExecConnectionHealth {
	cfg := b.state.Load().inner.Config

	out := make([]health.WorkflowExecConnectionHealth, 0, len(cfg.Workflows.ExecConnections))

	for _, declared := range cfg.Workflows.ExecConnections {
		out = append(out, b.probeExecConnection(ctx, declared.Name))
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })

	return out
}

// capabilityProbeScript is what a health probe asks a far side to parse
// and run.
//
// `true` and a newline: a shell builtin that exists on every POSIX shell,
// takes no arguments, touches nothing and cannot fail for any reason
// other than the one being measured. It is deliberately not `echo` (which
// writes), not `:` (which reads oddly in a log) and emphatically not
// anything the operator configured.
var capabilityProbeScript = []byte("true\n")

func (b *BackupService) probeExecConnection(ctx context.Context, ref string) health.WorkflowExecConnectionHealth {
	out := health.WorkflowExecConnectionHealth{Ref: ref}

	conn, err := remoteexec.Resolve(b.state.Load().inner.Config, ref)
	if err != nil {
		out.Detail = err.Error()

		return out
	}

	probeCtx, cancel := context.WithTimeout(ctx, workflowProbeTimeout)
	defer cancel()

	client, err := remoteexec.Dial(probeCtx, conn)
	if err != nil {
		out.Detail = err.Error()

		return out
	}
	defer client.Close() //nolint:errcheck // a probe connection

	if _, err := client.Preflight(probeCtx, capabilityProbeScript); err != nil {
		out.Detail = err.Error()

		return out
	}

	out.Capable = true
	out.Detail = "this connection accepted an exec channel and ran the bytes it was sent"

	return out
}

// suspendedBackupSetIDs is SuspendedBackupSets as the strings every
// surface above this one uses.
func (b *BackupService) suspendedBackupSetIDs() []string {
	sets := b.SuspendedBackupSets()

	out := make([]string, 0, len(sets))
	for _, s := range sets {
		out = append(out, s.String())
	}

	return out
}

// assert that the health package's hold carries a parsed set id, so a
// change to either side is a compile error here rather than a surface
// that silently starts printing an empty backup set.
var _ = func() model.BackupSetID { return health.WorkflowRecoveryHold{}.BackupSet }
