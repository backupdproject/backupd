// EPIC K's production feature gate (#789), tested where it is enforced.
//
// The gate's own semantics (what the file says, what the environment
// overrides, what the refusal reads like) are internal/config's subject.
// What this file asks is the question only this package can answer: with
// the gate shut, does an incremental set get REFUSED without anything
// being read, opened or written -- and does the artifact set sitting
// beside it in the same configuration still get backed up?
//
// The second half is the one that matters. A feature flag whose use takes
// the rest of the deployment down is worse than the feature it gates, and
// the way that happens is not a design decision, it is a refusal placed
// one layer too early.

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/alert"
	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/health"
	"github.com/retnd/retnd/core/internal/lifecycle"
	"github.com/retnd/retnd/core/internal/model"
)

// poisonEngine is a backupengine.Engine that fails the test if anything
// touches it. It is the gate's real assertion: "refused" has to mean the
// repository was never opened, not that opening it failed.
type poisonEngine struct{ t *testing.T }

func (e poisonEngine) CreateRepository(context.Context, backupengine.RepositoryLocation) error {
	e.t.Error("a gated deployment created a repository")

	return errors.New("poisonEngine: CreateRepository")
}

func (e poisonEngine) OpenRepository(context.Context, backupengine.RepositoryLocation) (backupengine.Repository, error) {
	e.t.Error("a gated deployment opened a repository; the production gate must refuse before any storage is touched")

	return nil, errors.New("poisonEngine: OpenRepository")
}

// gatedService is a deployment with one artifact set and one incremental
// set, the gate SHUT, and an engine that fails the test if it is used.
//
// Both sets in one source and one cycle, because the interesting claim is
// about their relationship: one refused, the other backed up, in the same
// pass.
func gatedService(t *testing.T) (*Service, *fakeTransport, config.BackupSet, config.BackupSet) {
	t.Helper()

	artifact := testBackupSet(t, t.TempDir())
	artifact.RemotePath = "" // fakeTransport ignores Source.Root.

	incremental := testBackupSet(t, t.TempDir())
	incremental.Name = "uploads-tree"
	incremental.ID = mustSetID(t, "production", "uploads-tree")
	incremental.Engine = model.EngineKopia
	incremental.Repository = model.RepositoryRef{Domain: "production", Set: mustSetID(t, "production", "uploads-tree")}
	incremental.SourceIdentity = "1e3f0c2b4a5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"
	incremental.LocalPath = ""
	incremental.Include = nil
	incremental.Completion = config.Completion{}

	// The transport serves the artifact set (which legitimately lists,
	// copies and then deletes its finished artifact) and fails the moment
	// anything touches the INCREMENTAL set's source, matching
	// engineguard_test.go's discipline: the refusal is proved by the
	// error being the gate's rather than by an assertion after the fact,
	// because a walk that already happened cannot be un-walked.
	tr := newFakeTransport()
	tr.put("backup.dump", "cycle payload", epoch.Unix())
	tr.failForSourceID = incremental.ID.String()
	tr.failErr = errors.New("the gated incremental set's source was read, which a refused pass must never do")

	cfg := testConfig(t, testSource("production", artifact, incremental))
	cfg.IncrementalEngine.Enabled = false

	svc := New(cfg, openJournal(t), tr, nil)
	svc.Now = fixedNow(epoch)
	svc.Repositories = poisonEngine{t: t}

	return svc, tr, artifact, incremental
}

// TestRunCycle_TheGateRefusesTheIncrementalSetAndBacksUpTheArtifactOne is
// the acceptance criterion for the flag, in one pass.
func TestRunCycle_TheGateRefusesTheIncrementalSetAndBacksUpTheArtifactOne(t *testing.T) {
	t.Setenv(config.IncrementalEngineEnvVar, "")

	svc, tr, artifact, incremental := gatedService(t)

	report := svc.RunCycle(context.Background())

	if len(report.Sets) != 2 {
		t.Fatalf("len(report.Sets) = %d, want 2; a gated set is still a set this cycle visited, and dropping it from the report hides the refusal from every surface that reads it", len(report.Sets))
	}

	rows := map[model.BackupSetID]BackupSetCycleResult{}
	for _, set := range report.Sets {
		rows[set.Set] = set
	}

	gated, ok := rows[incremental.ID]
	if !ok {
		t.Fatalf("the incremental set has no row in the cycle report: %+v", report.Sets)
	}

	if !errors.Is(gated.Err, config.ErrIncrementalEngineDisabled) {
		t.Fatalf("the gated set's failure is %v, which no surface can recognise as the production gate", gated.Err)
	}
	for _, want := range []string{"incremental_engine.enabled: true", incremental.ID.String(), "nothing on the source was deleted"} {
		if !strings.Contains(gated.Err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", gated.Err, want)
		}
	}
	if gated.Snapshot != nil {
		t.Errorf("the gated set carries a snapshot result (%+v), so the incremental pipeline ran anyway", gated.Snapshot)
	}
	if len(gated.Discovery.Discovered) != 0 {
		t.Errorf("discovery recorded %d artifact(s) for a gated incremental set; its source tree must not be walked as though its files were artifacts", len(gated.Discovery.Discovered))
	}

	// The other half: the artifact set in the same configuration ran to
	// completion. This is what stops the gate from being a bigger outage
	// than the feature.
	ran, ok := rows[artifact.ID]
	if !ok {
		t.Fatalf("the artifact set has no row in the cycle report: %+v", report.Sets)
	}
	if ran.Err != nil {
		t.Fatalf("the artifact set failed while the incremental engine was gated off: %v", ran.Err)
	}
	if len(ran.Discovery.Discovered) != 1 {
		t.Fatalf("the artifact set discovered %+v, want exactly the one artifact in its source", ran.Discovery.Discovered)
	}

	final, err := svc.Journal.Get(context.Background(), ran.Discovery.Discovered[0].Artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.State != string(lifecycle.Complete) {
		t.Errorf("the artifact set's backup is in %q, want %q: gating one engine must not stop the other one from producing a restore point",
			final.State, lifecycle.Complete)
	}

	// The artifact set's own source WAS deleted from, which is the
	// artifact engine doing its job (its copy is durable). The gated
	// set's source was not touched at all: tr.failErr would have
	// surfaced as that set's error instead of the gate's refusal.
	if got := tr.copyToLocalCalls(); got != 1 {
		t.Errorf("CopyToLocal was called %d time(s), want exactly the artifact set's one transfer", got)
	}
}

// The per-set surfaces, all of which reach the engine through one door.
// Each one has to be recognisable as the gate rather than as "no such
// backup set", which is what an operator would otherwise be sent to
// investigate.
func TestSnapshotSurfaces_AreRefusedByTheGateWithoutOpeningAnything(t *testing.T) {
	t.Setenv(config.IncrementalEngineEnvVar, "")

	svc, _, _, incremental := gatedService(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"list snapshots", func() error {
			_, err := svc.ListSnapshots(ctx, "production", incremental.Name)

			return err
		}},
		{"get snapshot", func() error {
			_, err := svc.GetSnapshot(ctx, "production", incremental.Name, "run_1")

			return err
		}},
		{"list holds", func() error {
			_, err := svc.ListSnapshotHolds(ctx, "production", incremental.Name)

			return err
		}},
		{"retention preview", func() error {
			_, err := svc.SnapshotRetentionPreview(ctx, "production", incremental.Name)

			return err
		}},
		{"verify", func() error {
			_, err := svc.VerifySnapshot(ctx, VerifySnapshotRequest{SourceName: "production", SetName: incremental.Name})

			return err
		}},
		{"restore", func() error {
			_, err := svc.RestoreSnapshot(ctx, SnapshotRestoreRequest{
				SourceName: "production",
				SetName:    incremental.Name,
				TargetPath: t.TempDir(),
			})

			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, config.ErrIncrementalEngineDisabled) {
				t.Errorf("%s returned %v, which no surface can map to the production gate", tc.name, err)
			}
		})
	}
}

// The repository probe is refused as a whole rather than answered with
// six booleans nobody measured.
func TestRepositoryHealth_TheGateRefusesTheProbeItself(t *testing.T) {
	t.Setenv(config.IncrementalEngineEnvVar, "")

	svc := probeService(t, poisonEngine{t: t}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))
	svc.Config.IncrementalEngine.Enabled = false

	reports, err := svc.RepositoryHealth(context.Background())
	if !errors.Is(err, config.ErrIncrementalEngineDisabled) {
		t.Fatalf("RepositoryHealth on a gated deployment returned %v", err)
	}
	if len(reports) != 0 {
		t.Errorf("RepositoryHealth reported %d reading(s) for repositories nothing may open", len(reports))
	}
}

// A gated deployment raises no repository alert. An operator who turned
// the engine off must not be paged about the store it is not using, and
// the maintenance conditions this pass also reads are silent for the same
// reason: there is no maintenance to be overdue.
func TestRepositoryAlertConditions_AGatedDeploymentIsSilent(t *testing.T) {
	t.Setenv(config.IncrementalEngineEnvVar, "")

	svc := probeService(t, poisonEngine{t: t}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))
	svc.Config.IncrementalEngine.Enabled = false

	if got := svc.RepositoryAlertConditions(context.Background()); len(got) != 0 {
		t.Errorf("a gated deployment raised %d repository condition(s): %+v", len(got), got)
	}
}

// The environment override reaches the enforcement points, not just
// config's own resolution: a deployment that turns the engine on through
// its compose file and nothing else has to get a working engine.
func TestRunCycle_TheEnvironmentOverrideOpensTheGate(t *testing.T) {
	svc, _, _, incremental := gatedService(t)
	svc.Repositories = nil // The pass may now reach the engine; nothing may poison it.

	t.Setenv(config.IncrementalEngineEnvVar, "1")

	report := svc.RunCycle(context.Background())

	for _, set := range report.Sets {
		if set.Set != incremental.ID {
			continue
		}

		if errors.Is(set.Err, config.ErrIncrementalEngineDisabled) {
			t.Fatalf("%s=1 did not open the gate at the cycle: %v", config.IncrementalEngineEnvVar, set.Err)
		}
		if set.Snapshot == nil {
			t.Fatal("the incremental set was neither gated nor dispatched to the incremental pipeline")
		}

		return
	}

	t.Fatalf("the incremental set has no row in the cycle report: %+v", report.Sets)
}

// TestRepositoryAlertConditions_AnUnusableRepositoryAlerts is #789's
// monitoring gap, at the pass that delivers notifications.
//
// Each case is a repository that cannot take a backup, and each one used
// to be silent here: #788's pass read the maintenance record and nothing
// else, on the argument that the sets inside a broken repository go stale
// anyway. They do -- a day or more later, from an alert that names
// neither the repository nor the credential.
func TestRepositoryAlertConditions_AnUnusableRepositoryAlerts(t *testing.T) {
	t.Setenv(config.IncrementalEngineEnvVar, "")

	for _, tc := range []struct {
		name   string
		engine backupengine.Engine
		says   string
	}{
		{
			name:   "the storage does not answer",
			engine: probeEngine{openErr: errors.New("dial tcp 10.0.0.9:22: connect: no route to host")},
			says:   "could not be reached",
		},
		{
			name:   "the declared passphrase does not open it",
			engine: probeEngine{openErr: backupengine.ErrPassphrase},
			says:   "passphrase",
		},
		{
			name:   "it opened and cannot be written to",
			engine: probeEngine{health: backupengine.HealthReport{Reachable: false}},
			says:   "could not be written to",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := probeService(t, tc.engine, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

			got := svc.RepositoryAlertConditions(context.Background())

			var found bool
			for _, c := range got {
				if c.Kind != alert.RepositoryUnavailable {
					continue
				}

				found = true

				if c.Scope != "production-vault" {
					t.Errorf("the condition is scoped to %q, want the repository domain: an alert about a shared repository must name the repository, not one of the sets in it", c.Scope)
				}
				if !strings.Contains(c.Detail, tc.says) {
					t.Errorf("the condition's detail %q does not say %q, so an operator is told a repository is unavailable and not which of the four causes it is", c.Detail, tc.says)
				}
				// The probe's sentences are composed to carry no path,
				// endpoint or credential, and an alert is delivered to a
				// phone and an inbox.
				for _, leak := range []string{"10.0.0.9", "dial tcp"} {
					if strings.Contains(c.Detail, leak) {
						t.Errorf("the condition's detail leaks %q into a notification: %q", leak, c.Detail)
					}
				}
			}

			if !found {
				t.Fatalf("a repository that cannot take a backup raised no %s condition: %+v", alert.RepositoryUnavailable, got)
			}
		})
	}
}

// A working repository raises nothing, which is the half that keeps the
// condition worth delivering: a pass that alerts on a healthy store is a
// pass people filter.
func TestRepositoryAlertConditions_AWorkingRepositoryIsQuiet(t *testing.T) {
	t.Setenv(config.IncrementalEngineEnvVar, "")

	svc := probeService(t, probeEngine{health: backupengine.HealthReport{Reachable: true}}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

	if got := onlyRepository(t, svc); got.State != health.Healthy {
		t.Fatalf("precondition: the probe reports %s, want %s (detail: %q)", got.State, health.Healthy, got.Detail)
	}

	for _, c := range svc.RepositoryAlertConditions(context.Background()) {
		if c.Kind == alert.RepositoryUnavailable {
			t.Errorf("a healthy repository raised %s: %q", c.Kind, c.Detail)
		}
	}
}
