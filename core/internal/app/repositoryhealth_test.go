package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/state"
)

// EPIC K's health requirement (#788), checked as the three things it
// actually is: a repository that does not answer, one that answers and
// cannot be written to, and a clock that has gone backwards past this
// deployment's own durable history.
//
// All three are cases where every backup set reads perfectly healthy and
// the next run fails, which is precisely why they are their own verdict
// rather than something folded into a freshness report.

// probeEngine is a backupengine.Engine whose two failure modes are the
// two an operator actually meets.
type probeEngine struct {
	openErr error
	health  backupengine.HealthReport
	healthE error
}

func (e probeEngine) CreateRepository(context.Context, backupengine.RepositoryLocation) error {
	return errors.New("probeEngine: a health probe must never create a repository")
}

func (e probeEngine) OpenRepository(context.Context, backupengine.RepositoryLocation) (backupengine.Repository, error) {
	if e.openErr != nil {
		return nil, e.openErr
	}

	return probeRepository{report: e.health, err: e.healthE}, nil
}

// probeRepository answers Health and refuses everything else, which is
// the whole point: a health probe that could snapshot, delete or maintain
// would be a read that can write.
type probeRepository struct {
	report backupengine.HealthReport
	err    error
}

func (r probeRepository) Snapshot(context.Context, backupengine.SnapshotRequest) (backupengine.SnapshotInfo, error) {
	panic("probeRepository: a health probe must not take a snapshot")
}

func (r probeRepository) ListSnapshots(context.Context, backupengine.Source) ([]backupengine.SnapshotInfo, error) {
	panic("probeRepository: a health probe must not list snapshots")
}

func (r probeRepository) LookupSnapshot(context.Context, backupengine.SnapshotID) (backupengine.SnapshotInfo, error) {
	panic("probeRepository: a health probe must not look up a snapshot")
}

func (r probeRepository) Verify(context.Context, backupengine.SnapshotID, backupengine.VerifyRequest) (backupengine.VerifyReport, error) {
	panic("probeRepository: a health probe must not verify")
}

func (r probeRepository) Restore(context.Context, backupengine.SnapshotID, backupengine.RestoreRequest) (backupengine.RestoreReport, error) {
	panic("probeRepository: a health probe must not restore")
}

func (r probeRepository) DeleteSnapshot(context.Context, backupengine.SnapshotID) error {
	panic("probeRepository: a health probe must not delete a snapshot")
}

func (r probeRepository) Maintain(context.Context, backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error) {
	panic("probeRepository: a health probe must not run maintenance")
}

func (r probeRepository) Health(context.Context) (backupengine.HealthReport, error) {
	return r.report, r.err
}

func (r probeRepository) Stats(context.Context) (backupengine.RepositoryStats, error) {
	panic("probeRepository: a health probe must not walk the storage listing")
}

func (r probeRepository) Close(context.Context) error { return nil }

// probeService is a Service holding one declared domain and one
// incremental set inside it.
func probeService(t *testing.T, engine backupengine.Engine, now time.Time) *Service {
	t.Helper()

	domain, err := model.NewRepositoryDomainID("production-vault")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	cfg := &config.Config{
		Capacity: config.Capacity{BackupRoot: t.TempDir()},
		RepositoryDomains: []config.RepositoryDomainConfig{{
			ID:            domain.String(),
			Isolation:     string(model.RepositoryShared),
			Domain:        model.RepositoryDomain{ID: domain, Isolation: model.RepositoryShared},
			PassphraseRef: secretref.Ref{Env: "BACKUPD_TEST_PASSPHRASE"},
		}},
		Sources: []config.Source{{
			Name: "production",
			BackupSets: []config.BackupSet{{
				Name:       "uploads-tree",
				ID:         mustSetID(t, "production", "uploads-tree"),
				UUID:       "3d0d3d5a-6c1b-4a4e-9a1f-2b7c8d9e0f11",
				Engine:     model.EngineKopia,
				Repository: model.RepositoryRef{Domain: domain},
			}},
		}},
	}

	return &Service{Config: cfg, Journal: openJournal(t), Repositories: engine, Now: fixedNow(now)}
}

func onlyRepository(t *testing.T, s *Service) health.RepositoryHealth {
	t.Helper()

	repos, err := s.RepositoryHealth(context.Background())
	if err != nil {
		t.Fatalf("RepositoryHealth: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("RepositoryHealth reported %d repositories, want the one this configuration declares", len(repos))
	}

	return repos[0]
}

func TestRepositoryHealth_AnUnreachableRepositoryIsFailingAndSaysSo(t *testing.T) {
	t.Parallel()

	svc := probeService(t, probeEngine{openErr: errors.New("dial tcp: connection refused")}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

	got := onlyRepository(t, svc)

	if got.Reachable || got.Readable || got.Writable {
		t.Errorf("an unreachable repository reported reachable=%v readable=%v writable=%v; none of the three has been proven",
			got.Reachable, got.Readable, got.Writable)
	}
	if got.State != health.Failing {
		t.Errorf("an unreachable repository is %s, want %s: the next backup into it cannot happen at all", got.State, health.Failing)
	}
	if got.Detail == "" {
		t.Error("an unreachable repository reported no detail, so an operator is told a state and not what to do about it")
	}
	// The transport's own sentence must not be echoed: it names an
	// address, and this string is rendered on dashboards and pasted into
	// support tickets.
	if contains(got.Detail, "dial tcp") {
		t.Errorf("the detail echoes the transport's own error: %q", got.Detail)
	}
}

func TestRepositoryHealth_AReadableRepositoryThatCannotBeWrittenToIsFailing(t *testing.T) {
	t.Parallel()

	svc := probeService(t, probeEngine{health: backupengine.HealthReport{Reachable: false}}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

	got := onlyRepository(t, svc)

	if !got.Readable || !got.CredentialsValid {
		t.Errorf("a repository that OPENED reported readable=%v credentials_valid=%v; opening one is exactly the proof of both",
			got.Readable, got.CredentialsValid)
	}
	if got.Writable {
		t.Error("a repository whose own health check proved no write reported writable=true, which is the claim that lets a deployment believe the next backup will land")
	}
	if got.State != health.Failing {
		t.Errorf("an unwritable repository is %s, want %s", got.State, health.Failing)
	}
}

func TestRepositoryHealth_AnEncryptionSecretThatDoesNotOpenTheRepositoryIsItsOwnAnswer(t *testing.T) {
	t.Parallel()

	svc := probeService(t, probeEngine{openErr: backupengine.ErrPassphrase}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

	got := onlyRepository(t, svc)

	if !got.Reachable {
		t.Error("a repository whose storage answered and whose key was refused reported unreachable, which sends an operator to check a mount that is fine")
	}
	if got.CredentialsValid {
		t.Error("a repository that refused the declared passphrase reported credentials_valid=true")
	}
}

// TestRepositoryHealth_AClockBehindItsOwnHistoryIsNotSane is the clock
// case, and it is driven through the journal rather than through the
// engine: the comparison this product can always make is against the
// newest durable timestamp it has itself written, and that answer is
// available even when the repository is the thing that is not answering.
func TestRepositoryHealth_AClockBehindItsOwnHistoryIsNotSane(t *testing.T) {
	t.Parallel()

	written := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// An hour earlier than a run this deployment has already recorded:
	// far enough past clockSkewTolerance that no NTP correction explains
	// it, which is the container-with-no-clock case.
	svc := probeService(t, probeEngine{health: backupengine.HealthReport{Reachable: true}}, written.Add(-time.Hour))

	set := svc.Config.Sources[0].BackupSets[0]
	journal, ok := svc.Journal.(*state.Journal)
	if !ok {
		t.Fatal("this fixture's journal cannot record snapshot runs")
	}
	if _, err := journal.BeginSnapshotRun(context.Background(), state.SnapshotRunRequest{
		RunID:             "run_1",
		IdempotencyKey:    "key_1",
		Set:               set.ID,
		SetUUID:           set.UUID,
		Engine:            string(model.EngineKopia),
		Domain:            "production-vault",
		SourceIdentity:    "digest",
		ConsistencyMode:   string(model.ModeLiveBestEffort),
		VerificationLevel: string(model.DefaultVerificationLevel),
		StartedAt:         written,
	}); err != nil {
		t.Fatalf("BeginSnapshotRun: %v", err)
	}

	got := onlyRepository(t, svc)

	if got.ClockSane {
		t.Error("a clock an hour behind a snapshot this deployment has already recorded reported sane; the next snapshot would be timestamped before one that already exists, and retention buckets on those timestamps")
	}
	if !got.ClockSkewKnown {
		t.Error("the skew was reported as unknown even though a durable timestamp exists to compare against")
	}
	if got.ClockSkew >= 0 {
		t.Errorf("the skew is %s, want a negative value: negative is the direction that reorders snapshots and the one worth warning about", got.ClockSkew)
	}
	if got.State != health.Degraded {
		t.Errorf("a repository whose only fault is this deployment's clock is %s, want %s: the storage works, and the verdict has to say which of the two is wrong", got.State, health.Degraded)
	}
}

func TestRepositoryHealth_AWorkingRepositoryIsHealthy(t *testing.T) {
	t.Parallel()

	svc := probeService(t, probeEngine{health: backupengine.HealthReport{Reachable: true}}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

	got := onlyRepository(t, svc)

	if got.State != health.Healthy {
		t.Fatalf("a reachable, readable, writable repository is %s, want %s (detail: %q). Without this the three failure cases above prove only that this function never says healthy.",
			got.State, health.Healthy, got.Detail)
	}
	if got.Domain != "production-vault" || len(got.BackupSets) != 1 {
		t.Errorf("the verdict names domain %q and %d backup set(s); a shared domain's fault is every co-tenant's, so the list is what makes it actionable", got.Domain, len(got.BackupSets))
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
