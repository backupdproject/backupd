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

// TestMaintenanceOverdue_NeverFullIsOverdueOnlyOnceTheRepositoryIsOld is
// the polarity of the one branch that reads backwards.
//
// A repository with recent quick runs and no full one is the case the
// state exists for: quick maintenance never reclaims, so the storage
// grows forever. The comparison has to say "the quick evidence is OLD
// enough that a full window should have happened by now", and the
// opposite reading -- overdue precisely because the last quick run was
// minutes ago -- reports every healthily-maintained repository as
// degraded and every genuinely neglected one as fine.
func TestMaintenanceOverdue_NeverFullIsOverdueOnlyOnceTheRepositoryIsOld(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		record backupengine.MaintenanceOwnership
		want   bool
	}{
		{
			name:   "a quick run an hour ago and no full run yet",
			record: backupengine.MaintenanceOwnership{LastQuick: now.Add(-time.Hour)},
			want:   false,
		},
		{
			name:   "quick runs for a month and no full run at all",
			record: backupengine.MaintenanceOwnership{LastQuick: now.Add(-30 * 24 * time.Hour)},
			want:   true,
		},
		{
			name:   "nothing has ever run",
			record: backupengine.MaintenanceOwnership{},
			want:   false,
		},
		{
			name:   "a full run an hour ago",
			record: backupengine.MaintenanceOwnership{LastFull: now.Add(-time.Hour), LastQuick: now.Add(-time.Hour)},
			want:   false,
		},
		{
			name:   "a full run a month ago",
			record: backupengine.MaintenanceOwnership{LastFull: now.Add(-30 * 24 * time.Hour), LastQuick: now.Add(-time.Hour)},
			want:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := maintenanceOverdue(tc.record, now); got != tc.want {
				t.Errorf("maintenanceOverdue = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecideRepositoryState_OverdueMaintenanceIsDegradedAndSaysWhy is the
// branch that turns the state above into something an operator reads.
//
// Degraded rather than Failing, because the snapshots are fine: what is
// wrong is that storage freed by deleted ones is still occupied, which is
// a bill rather than a lost restore point. A verdict with no sentence
// beside it would be a coloured row nobody can act on.
func TestDecideRepositoryState_OverdueMaintenanceIsDegradedAndSaysWhy(t *testing.T) {
	t.Parallel()

	out := health.RepositoryHealth{
		Reachable: true, Readable: true, Writable: true,
		CredentialsValid: true, ClockSane: true,
		MaintenanceOverdue: true,
	}
	decideRepositoryState(&out)

	if out.State != health.Degraded {
		t.Errorf("a repository whose only fault is overdue maintenance is %s, want %s: it still takes backups and still restores", out.State, health.Degraded)
	}
	if out.Detail == "" {
		t.Error("overdue maintenance produced no detail, so a surface colours a row and names nothing to do")
	}
}

// TestRepositoryHealth_AnEngineWarningNeverReachesTheDetailVerbatim is
// the guard the engine's own warning type asks for and this package had
// only claimed.
//
// backupengine.HealthWarning.Detail is composed by the engine adapter,
// and one of the three kinds carries the underlying error's own text
// (kopia's repository.go passes r.engineRetention.Error() straight
// through), which can name the repository's filesystem path. This
// detail string is rendered on a dashboard and pasted into support
// conversations, so every kind gets a product sentence here and none is
// echoed.
func TestRepositoryHealth_AnEngineWarningNeverReachesTheDetailVerbatim(t *testing.T) {
	t.Parallel()

	leak := "unable to set retention policy at /srv/vault/kopia/production-vault: read-only file system"
	svc := probeService(t, probeEngine{health: backupengine.HealthReport{
		Reachable: true,
		Warnings: []backupengine.HealthWarning{{
			Kind:   backupengine.HealthWarningEngineRetention,
			Detail: leak,
		}},
	}}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

	got := onlyRepository(t, svc)

	if contains(got.Detail, "/srv/vault") || contains(got.Detail, leak) {
		t.Errorf("the detail echoes the engine's own warning text, which carries this deployment's filesystem layout: %q", got.Detail)
	}
	if got.Detail == "" {
		t.Error("an engine warning produced no detail at all; dropping it hides a condition the engine went to the trouble of reporting")
	}
}

// TestRepositoryHealth_APassphraseRefusalIsReadableStorage pins what the
// three access probes mean when the secret is the only thing wrong.
//
// The storage answered and this deployment read the repository's format
// blob; what failed was the key. Reporting readable=false would send an
// operator to check a mount that is fine, which is the exact sentence the
// code's own comment and the contract's credentials_valid description
// both already give.
func TestRepositoryHealth_APassphraseRefusalIsReadableStorage(t *testing.T) {
	t.Parallel()

	svc := probeService(t, probeEngine{openErr: backupengine.ErrPassphrase}, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))

	got := onlyRepository(t, svc)

	if !got.Reachable || !got.Readable {
		t.Errorf("a repository whose format blob was read and whose key was refused reported reachable=%v readable=%v; both are true of it",
			got.Reachable, got.Readable)
	}
	if got.CredentialsValid {
		t.Error("a refused passphrase reported credentials_valid=true")
	}
	if got.State != health.Failing {
		t.Errorf("a repository nothing can open is %s, want %s", got.State, health.Failing)
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
