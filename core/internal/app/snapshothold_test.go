package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/state"
)

// Holding a snapshot that has nothing to hold.
//
// The journal decides this and always has, inside the transaction that
// would otherwise insert the row. What it could not do is say so in a
// shape the layers above can route on: its four refusals arrived here as
// unclassified errors, core/service reads an unclassified error as "an
// internal error occurred", and the API answers 500. So an operator who
// clicked Hold on a run that failed last night was told this product had
// broken, when the product was working exactly as designed and the only
// thing wrong was the screen being a few seconds stale.

// seedSnapshotRun puts one run in the journal, driven to the phase the
// caller names, and reports it.
//
// Driven through the journal's own transitions rather than inserted,
// because which refusal PlaceSnapshotHold makes depends on the durable
// phase and on whether a manifest was ever committed, and a hand-built
// row is a row that can be in a state no real run reaches.
func seedSnapshotRun(t *testing.T, svc *Service, runID, key, manifest string, phase state.SnapshotPhase) state.SnapshotRun {
	t.Helper()

	journal, ok := svc.Journal.(*state.Journal)
	if !ok {
		t.Fatal("this fixture's journal cannot record snapshot runs")
	}

	set := svc.Config.Sources[0].BackupSets[0]
	at := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)

	outcome, err := journal.BeginSnapshotRun(context.Background(), state.SnapshotRunRequest{
		RunID:             runID,
		IdempotencyKey:    key,
		Set:               set.ID,
		SetUUID:           set.UUID,
		Engine:            string(model.EngineKopia),
		Domain:            set.Repository.Domain.String(),
		SourceIdentity:    "digest",
		ConsistencyMode:   string(model.ModeLiveBestEffort),
		VerificationLevel: string(model.DefaultVerificationLevel),
		StartedAt:         at,
	})
	if err != nil {
		t.Fatalf("BeginSnapshotRun: %v", err)
	}

	// Through the phases a real run walks, because the journal refuses a
	// jump: a row that reached LOST or DELETED did so from a committed
	// manifest, and a hand-placed one would be in a state no run reaches.
	step := at
	advance := func(to state.SnapshotPhase, update state.SnapshotRunUpdate) {
		step = step.Add(time.Minute)
		update.At = step
		if err := journal.AdvanceSnapshotRun(context.Background(), outcome.Run.RunID, to, update); err != nil {
			t.Fatalf("AdvanceSnapshotRun(%s): %v", to, err)
		}
	}

	if manifest == "" {
		advance(phase, state.SnapshotRunUpdate{})
	} else {
		complete := true
		advance(state.PhaseSourceScan, state.SnapshotRunUpdate{})
		advance(state.PhaseSnapshotWrite, state.SnapshotRunUpdate{})
		advance(state.PhaseManifestCommitted, state.SnapshotRunUpdate{SnapshotID: &manifest, SourceComplete: &complete})
		advance(state.PhaseVerification, state.SnapshotRunUpdate{})
		advance(state.PhaseCatalogCommit, state.SnapshotRunUpdate{})
		advance(state.PhaseSuccess, state.SnapshotRunUpdate{})
		if phase != state.PhaseSuccess {
			if phase == state.PhaseDeleted {
				if err := journal.MarkSnapshotDeleteRequested(context.Background(), outcome.Run.RunID, step.Add(time.Minute)); err != nil {
					t.Fatalf("MarkSnapshotDeleteRequested: %v", err)
				}
			}
			advance(phase, state.SnapshotRunUpdate{})
		}
	}

	got, err := journal.GetSnapshotRun(context.Background(), outcome.Run.RunID)
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}

	return got
}

// holdService is a Service holding one incremental set and a real
// journal, and nothing else: no repository is opened by a hold, which is
// the whole reason a hold is answerable while the storage is down.
func holdService(t *testing.T) *Service {
	t.Helper()

	domain, err := model.NewRepositoryDomainID("production-vault")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	bs := config.BackupSet{
		Name:              "uploads-tree",
		ID:                mustSetID(t, "production", "uploads-tree"),
		UUID:              "9c2f1e0a-5b6d-4c3e-8a1f-7d9e0b2c4a66",
		Engine:            model.EngineKopia,
		Repository:        model.RepositoryRef{Domain: domain},
		Consistency:       model.ModeLiveBestEffort,
		VerificationLevel: model.LevelStructural,
	}

	cfg := &config.Config{
		Capacity: config.Capacity{BackupRoot: t.TempDir()},
		// EPIC K's production gate (#789) open: these tests are about
		// what a hold refuses on a deployment that runs the incremental
		// engine, and the gate refuses every per-set incremental surface
		// before it looks at the set at all.
		IncrementalEngine: config.IncrementalEngine{Enabled: true},
		Sources:           []config.Source{{Name: "production", BackupSets: []config.BackupSet{bs}}},
	}

	return &Service{Config: cfg, Journal: openJournal(t), Now: fixedNow(time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC))}
}

// TestPlaceSnapshotHold_ARunWithNothingToProtectIsRefusedAsNotHoldable
// drives each state the journal refuses and asserts the one thing every
// layer above depends on: the refusal is recognisable.
//
// A generic error here is not a cosmetic problem. core/service maps
// anything it cannot classify to "an internal error occurred" and
// apps/common/webhost answers 500, which tells an operator to file a bug
// about a product that is behaving correctly.
func TestPlaceSnapshotHold_ARunWithNothingToProtectIsRefusedAsNotHoldable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		phase    state.SnapshotPhase
		manifest string
	}{
		{"a run that failed before it committed a manifest", state.PhaseFailed, ""},
		{"a run whose manifest is not in the repository", state.PhaseLost, "manifest-lost"},
		{"a snapshot this product already deleted", state.PhaseDeleted, "manifest-deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := holdService(t)
			run := seedSnapshotRun(t, svc, "run-"+string(tc.phase), "key-"+string(tc.phase), tc.manifest, tc.phase)

			_, err := svc.PlaceSnapshotHold(context.Background(), PlaceSnapshotHoldRequest{
				SourceName: "production",
				SetName:    "uploads-tree",
				RunID:      run.RunID,
				HoldID:     "hold-1",
				Reason:     "kept for the incident review",
				PlacedBy:   "alice",
			})
			if !errors.Is(err, ErrSnapshotNotHoldable) {
				t.Fatalf("PlaceSnapshotHold on %s = %v, want ErrSnapshotNotHoldable", tc.name, err)
			}

			var typed *SnapshotNotHoldable
			if !errors.As(err, &typed) || typed.Reason == "" {
				t.Fatalf("the refusal carries no reason, so every surface above it can only say the hold failed: %v", err)
			}

			// The one rule every refusal crossing this boundary follows.
			// The journal's own sentence names the run id and the durable
			// phase and is written for a log; what a caller may echo is
			// composed here.
			if contains(typed.Reason, run.RunID) || contains(typed.Reason, string(tc.phase)) {
				t.Errorf("the reason echoes the journal's own vocabulary rather than a product sentence: %q", typed.Reason)
			}
		})
	}
}

// TestPlaceSnapshotHold_AHealthyRestorePointIsStillHeld is the
// non-vacuity half: everything above would pass just as well against a
// method that refused every hold.
func TestPlaceSnapshotHold_AHealthyRestorePointIsStillHeld(t *testing.T) {
	t.Parallel()

	svc := holdService(t)
	run := seedSnapshotRun(t, svc, "run-good", "key-good", "manifest-good", state.PhaseSuccess)

	hold, err := svc.PlaceSnapshotHold(context.Background(), PlaceSnapshotHoldRequest{
		SourceName: "production",
		SetName:    "uploads-tree",
		RunID:      run.RunID,
		HoldID:     "hold-1",
		Reason:     "kept for the incident review",
		PlacedBy:   "alice",
	})
	if err != nil {
		t.Fatalf("PlaceSnapshotHold on a restore point: %v", err)
	}
	if hold.RunID != run.RunID || !hold.Active() {
		t.Errorf("the hold came back for run %q, active=%v; want run %q, active", hold.RunID, hold.Active(), run.RunID)
	}
}
