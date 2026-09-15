// EPIC K's production feature gate (#789) at this boundary: the two
// refusals only this layer can make.
//
// internal/app refuses everything that would RUN the incremental engine,
// and those refusals are tested there. What is tested here is what would
// otherwise happen ABOVE the engine, where the refusal is still free: a
// gated set being created into the configuration, and a durable operation
// row being written for work that will be refused the instant it starts.
//
// Both gated cases are driven through the ENVIRONMENT override rather
// than a second config fixture. That is deliberate, and it is a second
// assertion for free: RETND_INCREMENTAL_ENGINE=0 has to beat an
// incremental_engine.enabled: true sitting in the file, because the
// override's whole purpose is to be usable on a deployment whose config
// file is baked into an image.

package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/internal/config"
)

// countOperations is how many durable operation rows this deployment
// holds. A refused submission has to leave this unchanged.
func countOperations(t *testing.T, svc *BackupService) int {
	t.Helper()

	ops, err := svc.ListOperations(context.Background(), 100)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}

	return len(ops)
}

func listSets(t *testing.T, svc *BackupService) []BackupSet {
	t.Helper()

	sets, err := svc.ListBackupSets(context.Background())
	if err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}

	return sets
}

func TestSubmitSnapshotRestore_IsRefusedWhenTheIncrementalEngineIsGatedOff(t *testing.T) {
	svc := openRestoreTestService(t)

	t.Setenv(config.IncrementalEngineEnvVar, "0")

	before := countOperations(t, svc)

	_, err := svc.SubmitSnapshotRestore(context.Background(), SnapshotRestoreRequest{
		Actor:          "operator",
		IdempotencyKey: "restore-while-gated",
		ConfigRevision: svc.ConfigRevision(),
		BackupSetID:    snapshotSetID,
		TargetPath:     t.TempDir(),
	})

	if !errors.Is(err, ErrIncrementalEngineDisabled) {
		t.Fatalf("SubmitSnapshotRestore on a gated deployment = %v, want ErrIncrementalEngineDisabled", err)
	}
	for _, want := range []string{"incremental_engine.enabled: true", config.IncrementalEngineEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q, so the operator is not told how to enable the engine", err, want)
		}
	}

	// And nothing durable was written. A row here would be an operation
	// an operator has to poll before being told what this call already
	// knew, and it would sit in the activity feed as a restore that
	// failed rather than as one that was never accepted.
	if after := countOperations(t, svc); after != before {
		t.Errorf("a gated restore wrote %d durable operation row(s); a refused submission must record nothing", after-before)
	}
}

func TestSubmitSnapshotVerification_IsRefusedWhenTheIncrementalEngineIsGatedOff(t *testing.T) {
	svc := openRestoreTestService(t)

	t.Setenv(config.IncrementalEngineEnvVar, "0")

	before := countOperations(t, svc)

	_, err := svc.SubmitSnapshotVerify(context.Background(), SnapshotVerifyRequest{
		Actor:          "operator",
		IdempotencyKey: "verify-while-gated",
		ConfigRevision: svc.ConfigRevision(),
		BackupSetID:    snapshotSetID,
	})

	if !errors.Is(err, ErrIncrementalEngineDisabled) {
		t.Fatalf("SubmitSnapshotVerify on a gated deployment = %v, want ErrIncrementalEngineDisabled", err)
	}
	if after := countOperations(t, svc); after != before {
		t.Errorf("a gated verification wrote %d durable operation row(s)", after-before)
	}
}

// A gated deployment does not acquire an incremental backup set.
//
// The set would be inert (every cycle refuses it) and the operator would
// have a configured backup that never runs, which is the one state this
// product's own barren-set reporting exists to make impossible.
func TestCreateBackupSet_RefusesAnIncrementalSetWhenTheEngineIsGatedOff(t *testing.T) {
	svc := openRestoreTestService(t)

	t.Setenv(config.IncrementalEngineEnvVar, "0")

	req := validCreateReq(t, svc, "second-tree")
	req.SourceName = "production"
	req.Engine = "kopia"
	req.RepositoryDomain = "production"
	// The artifact-only keys config.Validate refuses on an incremental
	// set, cleared so this request is one the gate is the only thing
	// wrong with.
	req.LocalPath = ""
	req.Include = nil
	req.CompletionStrategy = ""

	_, err := svc.CreateBackupSet(context.Background(), req)

	if !errors.Is(err, ErrIncrementalEngineDisabled) {
		t.Fatalf("CreateBackupSet(engine: kopia) on a gated deployment = %v, want ErrIncrementalEngineDisabled", err)
	}

	// The configuration is untouched: a refusal that half-wrote a set
	// would leave a deployment holding a set it never agreed to.
	for _, set := range listSets(t, svc) {
		if set.Name == "second-tree" {
			t.Fatal("the refused incremental set was written into the configuration anyway")
		}
	}
}

// The other half, and the one that decides whether this gate is safe to
// ship on by default being off: an ARTIFACT set is created normally on a
// gated deployment. The gate is about one engine.
func TestCreateBackupSet_AnArtifactSetIsUnaffectedByTheGate(t *testing.T) {
	svc := openRestoreTestService(t)

	t.Setenv(config.IncrementalEngineEnvVar, "0")

	result, err := svc.CreateBackupSet(context.Background(), validCreateReq(t, svc, "nightly-dumps"))
	if err != nil {
		t.Fatalf("creating an artifact set on a deployment with the incremental engine gated off: %v", err)
	}
	if result.Set.Name != "nightly-dumps" {
		t.Fatalf("CreateBackupSet returned %+v", result.Set)
	}

	var found bool
	for _, set := range listSets(t, svc) {
		if set.Name == "nightly-dumps" {
			found = true
		}
	}
	if !found {
		t.Error("the created artifact set is not in this service's configuration, so the hot reload did not happen")
	}
}

// The read surfaces refuse with the gate's own error rather than with
// this package's generic internal-error sentence.
//
// That distinction is the whole reason these two are tested here and not
// only in internal/app: both of these paths deliberately flatten
// everything they do not recognise into "an internal error occurred", so
// a refusal that is not explicitly carried through arrives at the API as
// a 500 about a deployment where nothing is broken.
func TestReadSurfaces_CarryTheGateRefusalRatherThanAnInternalError(t *testing.T) {
	svc := openRestoreTestService(t)

	t.Setenv(config.IncrementalEngineEnvVar, "0")

	if _, err := svc.ListRepositories(context.Background()); !errors.Is(err, ErrIncrementalEngineDisabled) {
		t.Errorf("ListRepositories on a gated deployment = %v, want ErrIncrementalEngineDisabled", err)
	}

	if _, err := svc.ListSnapshots(context.Background(), snapshotSetID); !errors.Is(err, ErrIncrementalEngineDisabled) {
		t.Errorf("ListSnapshots on a gated deployment = %v, want ErrIncrementalEngineDisabled", err)
	}
}
