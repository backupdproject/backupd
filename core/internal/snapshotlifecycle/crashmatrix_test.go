package snapshotlifecycle_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
	"github.com/backupdproject/backupd/core/internal/state"
)

// This file is the part of #784's verification matrix that a per-boundary
// test cannot be: the proof that the list of boundaries is COMPLETE, and
// that the two things a recovery pass must never do it never does at ANY
// of them.
//
// reconcile_test.go already has one test per crash boundary #783 names,
// and each of those describes its own boundary better than anything here
// could. What that file cannot say is "and there are no others". A phase
// added to the table in phases.go, or a verdict added to the closed set
// in reconcile.go, would leave every test in it passing while a crashed
// row sat undecided for ever -- and an undecided row is an invisible one,
// which is the failure mode reconcile.go's own header is written against.
//
// So the tests below take the phase table and VerdictKinds() as their
// INPUT rather than restating them: they enumerate the vocabulary, drive
// every state in it, and fail when the vocabulary grows without a rule to
// go with it. The route to each phase is derived from the table too, by
// crashPath, so a nominal path that gains a phase is walked rather than
// skipped.
//
// Rows are seeded through reconcile_test.go's seed(), which walks the same
// legal edges a live run walks, because a row placed at a phase by any
// other means would be a state no crash can actually produce.

// The identities the matrix seeds. Two rows are enough for every property
// here: the one a process died in, and an older successful one holding the
// restore point that must survive whatever happens to the newer.
const (
	crashSubjectRun    = "run-crashed"
	crashOlderRun      = "run-older"
	crashOlderSnapshot = "snap-older"
)

// crashRepository is the reconciler's entire view of a repository, and it
// is hostile in the one direction that matters: it RECORDS every call, and
// it refuses to store a snapshot at all.
//
// It is declared here rather than reused from run_test.go's fakeRepository
// because these tests assert on the calls themselves -- which methods a
// recovery pass may reach for, and with what verification request -- and a
// fake shared with the run tests would have to grow that bookkeeping for
// readers who do not need it.
type crashRepository struct {
	snapshots map[string]backupengine.SnapshotInfo

	// listErr makes the repository unreadable, which is the one input
	// that must stop a pass from deciding anything at all.
	listErr error

	// achieved, when overrideAchieved is set, is the level this
	// repository claims to have PROVEN regardless of the level it was
	// asked for. It is how a row for "a shallower check than the set
	// asked for" is built without a damaged fixture.
	overrideAchieved bool
	achieved         model.VerificationLevel

	mu       sync.Mutex
	calls    []string
	requests []backupengine.VerifyRequest
}

// newCrashRepository holds the given manifests, all of them started just
// after the run that might have written them, which is what makes an
// unclaimed one adoptable (see Reconciler.orphanFor).
func newCrashRepository(start time.Time, ids ...string) *crashRepository {
	repo := &crashRepository{snapshots: make(map[string]backupengine.SnapshotInfo, len(ids))}
	for _, id := range ids {
		repo.snapshots[id] = backupengine.SnapshotInfo{
			ID:          backupengine.SnapshotID(id),
			Source:      testSource,
			Files:       3,
			Directories: 1,
			Bytes:       4096,
			Start:       start,
			End:         start.Add(time.Second),
		}
	}

	return repo
}

func (f *crashRepository) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call)
}

// SnapshotTree refuses, because a reconciliation pass that stored a
// snapshot would be a recovery path writing new data into a repository it
// was asked to explain. Nothing in the matrix may reach this.
func (f *crashRepository) SnapshotTree(_ context.Context, _ backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error) {
	f.record("SnapshotTree")

	return backupengine.TreeSnapshotInfo{}, errors.New("a reconciliation pass asked this repository to store a snapshot")
}

func (f *crashRepository) Verify(_ context.Context, id backupengine.SnapshotID, req backupengine.VerifyRequest) (backupengine.VerifyReport, error) {
	f.record("Verify")

	f.mu.Lock()
	f.requests = append(f.requests, req)
	info, ok := f.snapshots[string(id)]
	f.mu.Unlock()

	if !ok {
		return backupengine.VerifyReport{Errors: []string{"no manifest " + string(id) + " in this repository"}}, backupengine.ErrSnapshotNotFound
	}

	level := req.Level
	if f.overrideAchieved {
		level = f.achieved
	}

	return backupengine.VerifyReport{
		Level:           level,
		ObjectsVerified: info.Files + info.Directories,
		FilesVerified:   info.Files,
		BytesVerified:   info.Bytes,
		BlobsChecked:    info.Files,
	}, nil
}

func (f *crashRepository) LookupSnapshot(_ context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error) {
	f.record("LookupSnapshot")

	f.mu.Lock()
	defer f.mu.Unlock()

	info, ok := f.snapshots[string(id)]
	if !ok {
		return backupengine.SnapshotInfo{}, backupengine.ErrSnapshotNotFound
	}

	return info, nil
}

func (f *crashRepository) ListSnapshots(_ context.Context, src backupengine.Source) ([]backupengine.SnapshotInfo, error) {
	f.record("ListSnapshots")

	if f.listErr != nil {
		return nil, f.listErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var out []backupengine.SnapshotInfo
	for _, info := range f.snapshots {
		if info.Source == src {
			out = append(out, info)
		}
	}

	return out, nil
}

// manifests is what the repository holds, in a stable order, which is the
// before-and-after value TestCrashMatrixNeverDeletesARepositorySnapshot
// compares.
func (f *crashRepository) manifests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.snapshots))
	for id := range f.snapshots {
		out = append(out, id)
	}

	sort.Strings(out)

	return out
}

func (f *crashRepository) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

// crashCase is one row of the matrix: the phase a process died in, the
// repository state that makes that boundary concrete, and what the rule
// for it has to produce.
//
// The same phase appears in several rows on purpose: MANIFEST_COMMITTED
// with the manifest there and MANIFEST_COMMITTED with it gone are two
// different boundaries that happen to share a phase, and a matrix keyed by
// phase alone would test one of them and report the other as covered.
type crashCase struct {
	// what is the rest of the subtest name, after the phase.
	what string

	// crash is the phase the row was left in. It is what the coverage
	// assertion counts, and it is what crashPath walks to.
	crash state.SnapshotPhase

	// snapshotID is the manifest the row recorded before the process
	// died, empty for a row that never got one.
	snapshotID string

	// verification is the status durably on the row at the crash.
	verification string

	// repository is every manifest the repository holds, whether the
	// catalog knows about it or not.
	repository []string

	// deleteIntent records the durable "delete this snapshot" this
	// product asked for before the crash.
	deleteIntent bool

	// olderRestorePoint seeds the older successful run even in the tests
	// that do not force it, for the rows whose verdict needs one.
	olderRestorePoint bool

	// maintenance injects an interrupted maintenance record.
	maintenance bool

	// want is every verdict this boundary must produce.
	want []snapshotlifecycle.VerdictKind

	// rest is where the crashed row must come to rest. Empty means the
	// pass must leave it exactly where the crash did.
	rest state.SnapshotPhase
}

func (c crashCase) subtest() string { return string(c.crash) + "/" + c.what }

func (c crashCase) restingPhase() state.SnapshotPhase {
	if c.rest == "" {
		return c.crash
	}

	return c.rest
}

// crashMatrix is every crash state, twice over where the repository's
// answer changes the verdict.
//
// It is a literal table and that is not a contradiction of this file's
// purpose: the table is the EXPECTATION, and the enumeration the tests run
// against it comes from phases.go and VerdictKinds(). A boundary missing
// from here is what the coverage assertions fail on.
var crashMatrix = []crashCase{{
	what:  "nothing was ever asked of the engine",
	crash: state.PhasePending,
	want:  []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictAbandonedBeforeUpload},
	rest:  state.PhaseFailed,
}, {
	// The one edge in the table that opens a row rather than moving one:
	// PENDING -> QUARANTINED, on a row this pass creates for a manifest
	// nothing can attribute.
	what:       "a manifest in the repository that no row can account for",
	crash:      state.PhasePending,
	repository: []string{"snap-stray"},
	want: []snapshotlifecycle.VerdictKind{
		snapshotlifecycle.VerdictAbandonedBeforeUpload,
		snapshotlifecycle.VerdictQuarantined,
	},
	rest: state.PhaseFailed,
}, {
	what:  "the source scan never finished",
	crash: state.PhaseSourceScan,
	want:  []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictAbandonedBeforeUpload},
	rest:  state.PhaseFailed,
}, {
	what:  "the upload was in flight and left no manifest",
	crash: state.PhaseSnapshotWrite,
	want:  []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictAbandonedUpload},
	rest:  state.PhaseFailed,
}, {
	what:       "an unclaimed manifest this run must have written",
	crash:      state.PhaseSnapshotWrite,
	repository: []string{"snap-orphan"},
	want: []snapshotlifecycle.VerdictKind{
		snapshotlifecycle.VerdictManifestAdopted,
		snapshotlifecycle.VerdictVerified,
	},
	rest: state.PhaseSuccess,
}, {
	what:       "the manifest is durable and unproven",
	crash:      state.PhaseManifestCommitted,
	snapshotID: "snap-1",
	repository: []string{"snap-1"},
	want:       []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictVerified},
	rest:       state.PhaseSuccess,
}, {
	what:       "the manifest the row names is gone",
	crash:      state.PhaseManifestCommitted,
	snapshotID: "snap-1",
	want:       []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictManifestMissing},
	rest:       state.PhaseFailed,
}, {
	what:         "the verification never finished",
	crash:        state.PhaseVerification,
	snapshotID:   "snap-1",
	verification: "pending",
	repository:   []string{"snap-1"},
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictVerified},
	rest:         state.PhaseSuccess,
}, {
	what:         "the verification never finished and the manifest is gone",
	crash:        state.PhaseVerification,
	snapshotID:   "snap-1",
	verification: "pending",
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictManifestMissing},
	rest:         state.PhaseFailed,
}, {
	what:         "verified, and interrupted before the catalog said so",
	crash:        state.PhaseCatalogCommit,
	snapshotID:   "snap-1",
	verification: "passed",
	repository:   []string{"snap-1"},
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictCommitCompleted},
	rest:         state.PhaseSuccess,
}, {
	what:         "at the catalog commit without a verification that passed",
	crash:        state.PhaseCatalogCommit,
	snapshotID:   "snap-1",
	verification: "pending",
	repository:   []string{"snap-1"},
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictVerificationFailed},
	rest:         state.PhaseFailed,
}, {
	what:         "verified, and the manifest it verified is gone",
	crash:        state.PhaseCatalogCommit,
	snapshotID:   "snap-1",
	verification: "passed",
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictManifestMissing},
	rest:         state.PhaseFailed,
}, {
	what:         "the restore point it promised is gone",
	crash:        state.PhaseSuccess,
	snapshotID:   "snap-1",
	verification: "passed",
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictRestorePointLost},
	rest:         state.PhaseLost,
}, {
	what:              "the newest restore point is gone and an older one remains",
	crash:             state.PhaseSuccess,
	snapshotID:        "snap-1",
	verification:      "passed",
	olderRestorePoint: true,
	want: []snapshotlifecycle.VerdictKind{
		snapshotlifecycle.VerdictRestorePointLost,
		snapshotlifecycle.VerdictLastKnownGoodRepointed,
	},
	rest: state.PhaseLost,
}, {
	what:         "a delete this product asked for had already happened",
	crash:        state.PhaseSuccess,
	snapshotID:   "snap-1",
	verification: "passed",
	deleteIntent: true,
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictDeleteCompleted},
	rest:         state.PhaseDeleted,
}, {
	what:         "a delete this product asked for is still outstanding",
	crash:        state.PhaseSuccess,
	snapshotID:   "snap-1",
	verification: "passed",
	repository:   []string{"snap-1"},
	deleteIntent: true,
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictDeletePending},
}, {
	// The matrix form of reconcile_test.go's
	// TestReconcile_AnInterruptedMaintenanceIsReportedAndChangesNothing:
	// that test is the argument, this row is the one that keeps the
	// verdict inside the enumeration, so removing the rule would fail
	// the coverage assertion rather than only its own test.
	what:         "maintenance was interrupted under a healthy restore point",
	crash:        state.PhaseSuccess,
	snapshotID:   "snap-1",
	verification: "passed",
	repository:   []string{"snap-1"},
	maintenance:  true,
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictMaintenanceInterrupted},
}, {
	what:         "a delete of a snapshot the repository had already lost",
	crash:        state.PhaseLost,
	snapshotID:   "snap-1",
	verification: "passed",
	deleteIntent: true,
	want:         []snapshotlifecycle.VerdictKind{snapshotlifecycle.VerdictDeleteCompleted},
	rest:         state.PhaseDeleted,
}}

// crashPath is the route a live run walks to reach one phase, derived from
// the phase table by breadth-first search.
//
// Derived rather than written down, because a matrix that hardcoded the
// nominal path would silently stop exercising a phase inserted into it:
// the seeded row would still reach the old phase names and the new one
// would never be crashed in. Searching the table means the route to every
// phase is whatever the table currently says it is, and a phase the table
// cannot reach from PENDING fails here rather than looking covered.
func crashPath(t *testing.T, target state.SnapshotPhase) []state.SnapshotPhase {
	t.Helper()

	if target == state.PhasePending {
		return nil
	}

	from := map[state.SnapshotPhase]state.SnapshotPhase{}
	seen := map[state.SnapshotPhase]bool{state.PhasePending: true}
	queue := []state.SnapshotPhase{state.PhasePending}

	for len(queue) > 0 && !seen[target] {
		current := queue[0]
		queue = queue[1:]

		for _, next := range snapshotlifecycle.Successors(current) {
			if seen[next] {
				continue
			}

			seen[next], from[next] = true, current
			queue = append(queue, next)
		}
	}

	if !seen[target] {
		t.Fatalf("the phase table has no route from %s to %s, so no crash can leave a row there", state.PhasePending, target)
	}

	var path []state.SnapshotPhase
	for p := target; p != state.PhasePending; p = from[p] {
		path = append(path, p)
	}

	slices.Reverse(path)

	return path
}

// crashStates is every phase a crashed row can be sitting in, taken from
// the table: a phase that is not terminal is by definition a run still in
// flight, and a terminal phase the table permits a move OUT of (SUCCESS,
// LOST) is a finished run whose snapshot the world can still change under.
// A terminal phase with no outgoing edge is where reconciliation PUTS
// things, not where it finds them.
func crashStates(t *testing.T) []state.SnapshotPhase {
	t.Helper()

	var out []state.SnapshotPhase
	for _, p := range snapshotlifecycle.Phases() {
		onward := len(snapshotlifecycle.Successors(p)) > 0

		if !p.Terminal() && !onward {
			t.Errorf("phase %s is not terminal and the table gives it nowhere to go, so a crashed row there can never be decided", p)
		}

		if !p.Terminal() || onward {
			out = append(out, p)
		}
	}

	return out
}

// crashRun is one pass over one seeded boundary, with everything the
// assertions need to compare before and after.
type crashRun struct {
	journal *state.Journal
	set     model.BackupSetID
	repo    *crashRepository
	report  snapshotlifecycle.ReconcileReport
	err     error

	// before is the repository's manifests as they were when the pass
	// started, and lkgBefore the run holding the restore point then.
	before    []string
	lkgBefore string
}

// crashFixture is what the three invariant tests vary about a row.
type crashFixture struct {
	// older seeds an older successful run holding the restore point,
	// whose manifest the repository still has.
	older bool

	// unreadable makes the repository's listing fail.
	unreadable bool
}

// driveCrashCase seeds one boundary and reconciles it exactly once.
func driveCrashCase(t *testing.T, c crashCase, f crashFixture) crashRun {
	t.Helper()

	ctx := context.Background()
	j := journal(t)
	set := setID(t, "postgres")

	older := f.older || c.olderRestorePoint
	if older {
		seed(t, j, set, crashOlderRun, crashPath(t, state.PhaseSuccess), map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new(crashOlderSnapshot)},
			state.PhaseCatalogCommit:     {VerificationStatus: new("passed")},
		})
	}

	// Seeded second, so it is the newer of the two: the journal orders a
	// set's runs by start time and then by row, and seed() starts every
	// run at the same instant.
	subject := seed(t, j, set, crashSubjectRun, crashPath(t, c.crash), c.seedUpdates(t))

	held := slices.Clone(c.repository)
	if older {
		held = append(held, crashOlderSnapshot)
	}

	repo := newCrashRepository(subject.StartedAt.Add(time.Second), held...)
	if f.unreadable {
		repo.listErr = errors.New("the repository is not answering")
	}

	if c.deleteIntent {
		if err := j.MarkSnapshotDeleteRequested(ctx, crashSubjectRun, subject.StartedAt.Add(time.Minute)); err != nil {
			t.Fatalf("recording the delete intent: %v", err)
		}
	}

	req := reconcileRequest(set, repo)
	if c.maintenance {
		req.Maintenance = &backupengine.MaintenanceOwnership{
			Domain: model.RepositoryDomainID(testDomain),
			Owner:  backupengine.MaintenanceOwner("nas-1"),
			LastResult: backupengine.MaintenanceOutcome{
				At:   subject.StartedAt.Add(-time.Hour),
				Mode: backupengine.MaintenanceQuick,
				Ran:  true,
				Err:  "interrupted",
			},
		}
	}

	run := crashRun{
		journal:   j,
		set:       set,
		repo:      repo,
		before:    repo.manifests(),
		lkgBefore: lastKnownGoodOf(t, j, set),
	}

	run.report, run.err = reconciler(t, j).Reconcile(ctx, req)

	return run
}

// seedUpdates places the facts the crashed row carries on the phases that
// record them, so a row is built the way a run builds it rather than by
// writing every field at the last phase.
func (c crashCase) seedUpdates(t *testing.T) map[state.SnapshotPhase]state.SnapshotRunUpdate {
	t.Helper()

	path := crashPath(t, c.crash)
	upd := map[state.SnapshotPhase]state.SnapshotRunUpdate{}

	if len(path) == 0 {
		// A row still at PENDING has walked no edge and so carries
		// nothing a later phase would have written.
		if c.snapshotID != "" || c.verification != "" {
			t.Fatalf("the matrix row for %s claims facts a row that has walked no edge cannot carry", c.crash)
		}

		return upd
	}

	at := func(phase state.SnapshotPhase) state.SnapshotPhase {
		if slices.Contains(path, phase) {
			return phase
		}

		return path[len(path)-1]
	}

	if c.snapshotID != "" {
		u := upd[at(state.PhaseManifestCommitted)]
		u.SnapshotID = &c.snapshotID
		upd[at(state.PhaseManifestCommitted)] = u
	}

	if c.verification != "" {
		u := upd[at(state.PhaseCatalogCommit)]
		u.VerificationStatus = &c.verification
		upd[at(state.PhaseCatalogCommit)] = u
	}

	return upd
}

// seedConfigured puts a row at the phase a crash left it in, exactly as
// reconcile_test.go's seed() does, with the verification level the SET
// asked for as a parameter.
//
// It exists for one row of this file, and only because seed() pins every
// run's configured level at structural, the weakest rung. The property
// below is about a set that asked for MORE than the repository turned out
// to prove, and a fixture that cannot express "configured deeper than
// achieved" cannot show that the difference is refused.
func seedConfigured(
	t *testing.T,
	j *state.Journal,
	set model.BackupSetID,
	runID string,
	level model.VerificationLevel,
	phases []state.SnapshotPhase,
	upd map[state.SnapshotPhase]state.SnapshotRunUpdate,
) state.SnapshotRun {
	t.Helper()

	ctx := context.Background()
	at := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)

	if _, err := j.BeginSnapshotRun(ctx, state.SnapshotRunRequest{
		RunID:             runID,
		IdempotencyKey:    runID,
		Set:               set,
		Engine:            model.EngineKopia.String(),
		Domain:            testDomain,
		SourceIdentity:    "ab12cd34",
		ConsistencyMode:   string(model.ModeLiveBestEffort),
		VerificationLevel: string(level),
		StartedAt:         at,
	}); err != nil {
		t.Fatalf("seeding run %s: %v", runID, err)
	}

	for i, phase := range phases {
		u := upd[phase]
		u.At = at.Add(time.Duration(i+1) * time.Second)

		if err := j.AdvanceSnapshotRun(ctx, runID, phase, u); err != nil {
			t.Fatalf("seeding %s at %s: %v", runID, phase, err)
		}
	}

	run, err := j.GetSnapshotRun(ctx, runID)
	if err != nil {
		t.Fatalf("reading seeded run %s: %v", runID, err)
	}

	return run
}

// lastKnownGoodOf is the run holding this set's restore point, or empty
// when it has none. A set with no restore point is an ordinary state here
// and not an error: several rows of the matrix are the first run a set
// ever attempted.
func lastKnownGoodOf(t *testing.T, j *state.Journal, set model.BackupSetID) string {
	t.Helper()

	run, err := j.LastKnownGoodSnapshot(context.Background(), set)
	switch {
	case err == nil:
		return run.RunID
	case errors.Is(err, state.ErrSnapshotRunNotFound):
		return ""
	default:
		t.Fatalf("reading last-known-good: %v", err)

		return ""
	}
}

// undecidedRows is every row of this set still in flight after a pass,
// which is what "no boundary is left untouched" means concretely.
func undecidedRows(t *testing.T, j *state.Journal, set model.BackupSetID) []string {
	t.Helper()

	runs, err := j.UnfinishedSnapshotRuns(context.Background())
	if err != nil {
		t.Fatalf("reading the unfinished runs: %v", err)
	}

	var out []string
	for _, run := range runs {
		if run.Set == set {
			out = append(out, run.RunID+" at "+string(run.Phase))
		}
	}

	return out
}

// TestCrashMatrixCoversEveryStateMachineBoundary drives every phase a
// crash can leave a row in and fails if the vocabulary has grown past the
// rules.
//
// Two completeness claims, both made by enumeration rather than by a list:
//
// Every phase the table permits a move out of, and every phase that is not
// terminal, has at least one row here and is DECIDED by one pass -- the
// row comes to rest at a terminal phase and the set has nothing in flight
// afterwards. A phase reconciliation has not been taught makes
// Reconcile return "reconciliation has no rule for", which this fails on
// directly.
//
// Every VerdictKind in the closed set is produced by at least one row. A
// verdict nothing can produce is a boundary with a name and no rule, and a
// boundary with a rule and no verdict is a decision nothing reports; the
// first fails here, and the second fails as an undecided row.
func TestCrashMatrixCoversEveryStateMachineBoundary(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		phases   = map[state.SnapshotPhase]bool{}
		verdicts = map[snapshotlifecycle.VerdictKind]bool{}
	)

	// Registered on the parent, so it runs once every parallel subtest
	// below has finished and the two maps are complete.
	t.Cleanup(func() {
		for _, phase := range crashStates(t) {
			if !phases[phase] {
				t.Errorf("the crash matrix has no row for phase %s, so nothing proves a process dying there is ever decided", phase)
			}
		}

		known := snapshotlifecycle.VerdictKinds()
		for _, kind := range known {
			if !verdicts[kind] {
				t.Errorf("no row of the crash matrix produces the %s verdict, so that boundary has a name and nothing that reaches it", kind)
			}
		}

		for kind := range verdicts {
			if !slices.Contains(known, kind) {
				t.Errorf("reconciliation produced the %s verdict, which VerdictKinds() does not contain: the closed set is out of date and a surface walking it would not render this one", kind)
			}
		}
	})

	for _, c := range crashMatrix {
		t.Run(c.subtest(), func(t *testing.T) {
			t.Parallel()

			run := driveCrashCase(t, c, crashFixture{})
			if run.err != nil {
				t.Fatalf("reconciling a row left at %s: %v", c.crash, run.err)
			}

			for _, kind := range c.want {
				verdictOf(t, run.report, kind)
			}

			if got := phaseOf(t, run.journal, crashSubjectRun); got != c.restingPhase() {
				t.Errorf("the row came to rest at %s, want %s", got, c.restingPhase())
			}

			if left := undecidedRows(t, run.journal, run.set); len(left) != 0 {
				t.Errorf("one pass left %v in flight; a row a crash abandoned must be decided or it is invisible", left)
			}

			mu.Lock()
			defer mu.Unlock()

			phases[c.crash] = true
			for _, v := range run.report.Verdicts {
				verdicts[v.Kind] = true
			}
		})
	}
}

// TestCrashMatrixNeverLosesTheLastKnownGoodRestorePoint replays the whole
// matrix with an older successful run already holding the restore point.
//
// This is the property an operator actually depends on: whatever a crash
// left behind and whatever reconciliation decides about it, a set that had
// a restore point still has one afterwards. The newer row may TAKE the
// flag, but only by legitimately reaching SUCCESS in this pass -- every
// other outcome, failed, lost, deleted or quarantined, must leave it with
// the older run rather than clearing it on the way past.
//
// The report and the journal are checked against each other as well,
// because a caller acting on a restore point reads
// ReconcileReport.LastKnownGoodRunID and would never see a disagreement
// with the row; and the snapshot on the flagged row is checked against the
// repository, because a restore point that is advertised and not there is
// the one answer worse than none.
func TestCrashMatrixNeverLosesTheLastKnownGoodRestorePoint(t *testing.T) {
	t.Parallel()

	for _, c := range crashMatrix {
		t.Run(c.subtest(), func(t *testing.T) {
			t.Parallel()

			run := driveCrashCase(t, c, crashFixture{older: true})
			if run.err != nil {
				t.Fatalf("reconciling a row left at %s: %v", c.crash, run.err)
			}

			// The older run's manifest is always in the repository in
			// this fixture, so "this set has no restore point" is never
			// the right answer AFTER the pass. It is a legitimate answer
			// before it: seeding a row into LOST takes the flag off the
			// newer run and leaves the set advertising nothing until
			// this pass moves it back, which is the case
			// repointLastKnownGood exists for.
			want := crashOlderRun
			if phaseOf(t, run.journal, crashSubjectRun) == state.PhaseSuccess {
				want = crashSubjectRun
			}

			if got := lastKnownGoodOf(t, run.journal, run.set); got != want {
				t.Errorf("the restore point is on %q after the pass, want %q", got, want)
			}

			if run.report.LastKnownGoodRunID != want {
				t.Errorf("the report says the restore point is %q and the journal says %q", run.report.LastKnownGoodRunID, want)
			}

			flagged, err := run.journal.LastKnownGoodSnapshot(context.Background(), run.set)
			if err != nil {
				t.Fatalf("reading the last-known-good row: %v", err)
			}

			if _, ok := run.repo.snapshots[flagged.SnapshotID]; !ok {
				t.Errorf("the pass advertises run %s's snapshot %q as the restore point, and the repository does not hold it",
					flagged.RunID, flagged.SnapshotID)
			}
		})
	}
}

// TestCrashMatrixNeverDeletesARepositorySnapshot asserts across every
// boundary that the repository holds exactly the manifests it held before
// the pass, and that the pass only ever read.
//
// The port the reconciler is given cannot delete: snapshotlifecycle's
// Repository interface is four calls and none of them removes anything, so
// this test cannot fail by catching a delete. That is the point of it.
// What it proves is that no boundary NEEDS one -- every rule in the matrix
// reaches its verdict from what it can list, look up and verify -- and
// therefore that widening the port to let recovery delete would be adding
// a capability no boundary asked for, rather than supplying one the rules
// were missing. The recorded calls are checked against a read-only set for
// the same reason: a pass that stored a snapshot would be recovery writing
// new data into the repository it was asked to explain, and the fake
// refuses that rather than counting it.
func TestCrashMatrixNeverDeletesARepositorySnapshot(t *testing.T) {
	t.Parallel()

	readOnly := []string{"ListSnapshots", "LookupSnapshot", "Verify"}

	for _, c := range crashMatrix {
		t.Run(c.subtest(), func(t *testing.T) {
			t.Parallel()

			run := driveCrashCase(t, c, crashFixture{})
			if run.err != nil {
				t.Fatalf("reconciling a row left at %s: %v", c.crash, run.err)
			}

			if after := run.repo.manifests(); !slices.Equal(run.before, after) {
				t.Errorf("the repository held %v before the pass and %v after it", run.before, after)
			}

			for _, call := range run.repo.called() {
				if !slices.Contains(readOnly, call) {
					t.Errorf("the pass called %s on the repository; reconciliation may only %v", call, readOnly)
				}
			}
		})
	}
}

// TestCrashMatrixDecidesNothingWhenTheRepositoryCannotBeRead replays the
// matrix against a repository whose listing fails.
//
// reconcile_test.go's TestReconcile_AnUnreadableRepositoryDecidesNothing
// makes the argument for one boundary, and it is the better place to read
// WHY: an unreachable repository looks exactly like a repository that has
// lost every snapshot in it. What the sweep adds is that the refusal comes
// before the per-boundary rules rather than inside some of them, which is
// the shape that survives a rule being added later: every row here is left
// exactly as the crash left it, no row is opened for a manifest nobody
// could see, and nothing is verified.
func TestCrashMatrixDecidesNothingWhenTheRepositoryCannotBeRead(t *testing.T) {
	t.Parallel()

	for _, c := range crashMatrix {
		t.Run(c.subtest(), func(t *testing.T) {
			t.Parallel()

			run := driveCrashCase(t, c, crashFixture{older: true, unreadable: true})
			if run.err == nil {
				t.Fatalf("a pass that could not read the repository decided %d things: %+v", len(run.report.Verdicts), run.report.Verdicts)
			}

			if got := phaseOf(t, run.journal, crashSubjectRun); got != c.crash {
				t.Errorf("an unreadable repository moved the row from %s to %s", c.crash, got)
			}

			if got := lastKnownGoodOf(t, run.journal, run.set); got != run.lkgBefore {
				t.Errorf("an unreadable repository moved the restore point from %q to %q", run.lkgBefore, got)
			}

			rows, err := run.journal.ListSnapshotRuns(context.Background(), run.set, 50)
			if err != nil {
				t.Fatalf("listing the set's runs: %v", err)
			}

			want := 2
			if len(rows) != want {
				t.Errorf("the set has %d rows after an unreadable pass, want the %d it was seeded with", len(rows), want)
			}

			if calls := run.repo.called(); slices.Contains(calls, "Verify") {
				t.Errorf("the pass verified a snapshot it could not list: %v", calls)
			}
		})
	}
}

// TestCrashMatrixNeverEarnsARestorePointFromAShallowerCheckThanTheSetAsked
// is the matrix row for the level ladder #784 adds: a crashed run whose
// snapshot the repository will verify, but only to a shallower depth than
// the set configured.
//
// It belongs beside the crash matrix rather than with the level tests
// because recovery is where the temptation lives. A pass resolving a
// half-finished run holds a durable, well-formed manifest and one cheap
// way to make the row tidy, and writing down the check it could get
// instead of the check the set asked for would turn a crash into a
// silent downgrade of the promise -- and, worse, let that downgraded
// check earn the restore point an operator will later trust. So the run
// fails, the achieved level is not written as the configured one, and
// last-known-good stays with the older run that really did prove itself.
//
// The request is asserted too: a pass that quietly asked for the
// shallower level would satisfy everything else here while proving
// nothing about the rule.
func TestCrashMatrixNeverEarnsARestorePointFromAShallowerCheckThanTheSetAsked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	j := journal(t)
	set := setID(t, "postgres")

	seed(t, j, set, crashOlderRun, crashPath(t, state.PhaseSuccess), map[state.SnapshotPhase]state.SnapshotRunUpdate{
		state.PhaseManifestCommitted: {SnapshotID: new(crashOlderSnapshot)},
		state.PhaseCatalogCommit:     {VerificationStatus: new("passed")},
	})

	subject := seedConfigured(t, j, set, crashSubjectRun, model.LevelContentFull,
		crashPath(t, state.PhaseManifestCommitted),
		map[state.SnapshotPhase]state.SnapshotRunUpdate{
			state.PhaseManifestCommitted: {SnapshotID: new("snap-1")},
		})

	repo := newCrashRepository(subject.StartedAt.Add(time.Second), "snap-1", crashOlderSnapshot)
	repo.overrideAchieved, repo.achieved = true, model.LevelStructural

	rep, err := reconciler(t, j).Reconcile(ctx, reconcileRequest(set, repo))
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}

	verdictOf(t, rep, snapshotlifecycle.VerdictVerificationFailed)

	if len(repo.requests) != 1 {
		t.Fatalf("the pass made %d verification requests, want one", len(repo.requests))
	}

	if got := repo.requests[0].Level; got != model.LevelContentFull {
		t.Errorf("the pass asked for %q verification of a set configured for %q", got, model.LevelContentFull)
	}

	after, err := j.GetSnapshotRun(ctx, crashSubjectRun)
	if err != nil {
		t.Fatalf("reading the crashed run: %v", err)
	}

	if after.Phase != state.PhaseFailed {
		t.Errorf("the run is at %s; a snapshot proven only to %q may not complete a run configured for %q",
			after.Phase, model.LevelStructural, model.LevelContentFull)
	}

	if after.VerificationLevelAchieved == string(model.LevelContentFull) {
		t.Error("the row records a content_full verification that never happened")
	}

	if got := lastKnownGoodOf(t, j, set); got != crashOlderRun {
		t.Errorf("the restore point is on %q, want %q: a shallower check than the set asked for must not earn it", got, crashOlderRun)
	}

	if rep.LastKnownGoodRunID != crashOlderRun {
		t.Errorf("the report says the restore point is %q, want %q", rep.LastKnownGoodRunID, crashOlderRun)
	}
}
