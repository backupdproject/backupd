package snapshotlifecycle_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
	"github.com/backupdproject/backupd/core/internal/state"
)

// These tests run against the REAL journal, on a temporary database,
// rather than against a fake catalog. That is deliberate: every property
// under test here -- a phase that survives a crash, an idempotency key
// that refuses a second pass, a last-known-good flag that a failure
// cannot move -- is a property of what internal/state persists, and a
// fake catalog would be a second implementation of exactly the semantics
// being asserted. The repository, by contrast, IS faked: what a run needs
// from it is four calls, and a real one would make these tests a
// repository-format test.

func journal(t *testing.T) *state.Journal {
	t.Helper()

	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("opening journal: %v", err)
	}

	t.Cleanup(func() { j.Close() }) //nolint:errcheck // a closed test database has nothing to report.

	return j
}

func setID(t *testing.T, name string) model.BackupSetID {
	t.Helper()

	id, err := model.NewBackupSetID("production", name)
	if err != nil {
		t.Fatalf("building backup set id: %v", err)
	}

	return id
}

// setUUID is the durable identifier the catalog keys a set's snapshot
// lineage on. Derived from the names here only so each set gets its own;
// the point of the column is that the names may change afterwards and
// this may not.
func setUUID(set model.BackupSetID) string {
	return "uuid-" + set.Source + "-" + set.Set
}

// fakeRepository is the four calls a run and a reconciliation make of a
// repository, and nothing else. It counts them, because "did this pass
// re-verify a snapshot it did not have to" is one of the properties under
// test and it is invisible in the row.
type fakeRepository struct {
	snapshots map[string]backupengine.SnapshotInfo

	tree      func(req backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error)
	verifyErr error
	listErr   error

	// achieved is the level this fake CLAIMS to have proved, whatever it
	// was asked for. Empty means "what was asked for", which is what a
	// healthy engine does; setting it is how a test reaches the one
	// outcome a real engine should never produce and the lifecycle must
	// still refuse -- a verification shallower than the set requires.
	achieved model.VerificationLevel

	treeCalls   int
	verifyCalls int

	// verifyRequests is every request this fake was handed, in order, so
	// a test can assert what depth was ASKED for rather than only what
	// was recorded afterwards.
	verifyRequests []backupengine.VerifyRequest

	tags   map[string]string
	source backupengine.Source
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{snapshots: map[string]backupengine.SnapshotInfo{}}
}

func (f *fakeRepository) SnapshotTree(_ context.Context, req backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error) {
	f.treeCalls++
	f.tags, f.source = req.Tags, req.Source

	if f.tree != nil {
		info, err := f.tree(req)
		if err == nil {
			f.snapshots[string(info.ID)] = info.SnapshotInfo
		}

		return info, err
	}

	info := backupengine.TreeSnapshotInfo{
		SnapshotInfo: backupengine.SnapshotInfo{
			ID:          backupengine.SnapshotID("snap-1"),
			Source:      req.Source,
			Files:       3,
			Directories: 1,
			Bytes:       3000,

			// The adapter's own contract: the tags the caller supplied
			// plus the run id it was given. Reconciliation adopts an
			// orphan manifest on exactly these, so a fake that dropped
			// them would make every adoption test pass for the wrong
			// reason.
			Tags: storedTags(req),
		},
		SourceBytesRead:        3000,
		RepositoryBytesWritten: 900,
		ContentReusedBytes:     2_048,
		ContentReuseMeasured:   true,
	}
	f.snapshots[string(info.ID)] = info.SnapshotInfo

	return info, nil
}

func (f *fakeRepository) Verify(_ context.Context, id backupengine.SnapshotID, req backupengine.VerifyRequest) (backupengine.VerifyReport, error) {
	f.verifyCalls++
	f.verifyRequests = append(f.verifyRequests, req)

	if f.verifyErr != nil {
		return backupengine.VerifyReport{Errors: []string{"object " + string(id) + " is damaged"}}, f.verifyErr
	}

	info := f.snapshots[string(id)]

	achieved := req.Level
	if f.achieved != "" {
		achieved = f.achieved
	}

	// A drill's output is what the caller told it to write, so the fake
	// writes something there: the rule that a passing drill's scratch
	// tree is removed and a failing one's is kept cannot be tested
	// against a directory nothing ever created.
	if req.Level == model.LevelRestoreDrill && req.RestoreTarget != "" {
		if err := os.MkdirAll(req.RestoreTarget, 0o750); err != nil {
			return backupengine.VerifyReport{}, err
		}

		if err := os.WriteFile(filepath.Join(req.RestoreTarget, "restored.bin"), []byte("restored"), 0o600); err != nil {
			return backupengine.VerifyReport{}, err
		}
	}

	return backupengine.VerifyReport{
		Level:         achieved,
		FilesVerified: info.Files,
		BytesVerified: info.Bytes,
	}, nil
}

func (f *fakeRepository) LookupSnapshot(_ context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error) {
	info, ok := f.snapshots[string(id)]
	if !ok {
		return backupengine.SnapshotInfo{}, backupengine.ErrSnapshotNotFound
	}

	return info, nil
}

func (f *fakeRepository) ListSnapshots(_ context.Context, src backupengine.Source) ([]backupengine.SnapshotInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	var out []backupengine.SnapshotInfo
	for _, info := range f.snapshots {
		if info.Source == src || info.Source == (backupengine.Source{}) {
			out = append(out, info)
		}
	}

	return out, nil
}

// storedTags is what a repository adapter records on a manifest: the
// caller's tags, plus the run id the adapter writes itself.
func storedTags(req backupengine.TreeSnapshotRequest) map[string]string {
	tags := make(map[string]string, len(req.Tags)+1)
	for k, v := range req.Tags {
		tags[k] = v
	}

	tags[backupengine.TagKeyRun] = req.RunID

	return tags
}

// fakeTree is a source that reports what a test tells it to. Root is nil
// because the fake repository never walks it; the walk itself is
// backupengine/source's and backupengine/kopia's own subject.
type fakeTree struct {
	report snapshotlifecycle.ScanReport
	err    error
	closed int
}

func (f *fakeTree) Root() backupengine.SourceDir                               { return nil }
func (f *fakeTree) Report() snapshotlifecycle.ScanReport                       { return f.report }
func (f *fakeTree) Err() error                                                 { return f.err }
func (f *fakeTree) Close() error                                               { f.closed++; return nil }
func (f *fakeTree) open(context.Context) (snapshotlifecycle.SourceTree, error) { return f, nil }

func completeScan() snapshotlifecycle.ScanReport {
	return snapshotlifecycle.ScanReport{Entries: 4, Stored: 3, Complete: true}
}

// runner builds a Runner on a frozen clock that advances a second per
// reading, so a test can assert order without sleeping.
func runner(t *testing.T, j *state.Journal) *snapshotlifecycle.Runner {
	t.Helper()

	return &snapshotlifecycle.Runner{Catalog: j, Now: tick()}
}

func tick() func() time.Time {
	at := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	return func() time.Time {
		at = at.Add(time.Second)

		return at
	}
}

func request(set model.BackupSetID, repo snapshotlifecycle.Repository, tree *fakeTree) snapshotlifecycle.RunRequest {
	return snapshotlifecycle.RunRequest{
		RunID:             "run-1",
		IdempotencyKey:    "key-1",
		OperationID:       "op-1",
		Set:               set,
		SetUUID:           setUUID(set),
		Engine:            model.EngineKopia,
		Domain:            model.RepositoryDomainID("vault"),
		SourceIdentity:    model.SourceIdentity("ab12cd34"),
		Consistency:       model.ModeLiveBestEffort,
		VerificationLevel: model.LevelStructural,
		Source:            backupengine.Source{Host: "nas", User: "backupd", Path: "/srv/data"},
		Repository:        repo,
		OpenTree:          tree.open,
	}
}

func TestRun_ASuccessfulRunWalksEveryPhaseInOrderAndOnlyThenAdvertisesARestorePoint(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()
	tree := &fakeTree{report: completeScan()}

	res, err := runner(t, j).Run(context.Background(), request(set, repo, tree))
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	if !res.Succeeded() {
		t.Fatalf("run did not succeed: phase %s, reason %q", res.Phase, res.Reason)
	}

	want := []state.SnapshotPhase{
		state.PhaseSourceScan,
		state.PhaseSnapshotWrite,
		state.PhaseManifestCommitted,
		state.PhaseVerification,
		state.PhaseCatalogCommit,
		state.PhaseSuccess,
	}

	transitions, err := j.SnapshotRunTransitions(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("reading transitions: %v", err)
	}

	var got []state.SnapshotPhase
	for _, tr := range transitions {
		got = append(got, tr.To)
	}

	if len(got) != len(want) {
		t.Fatalf("recorded phases %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recorded phases %v, want %v", got, want)
		}
	}

	lkg, err := j.LastKnownGoodSnapshot(context.Background(), setUUID(set))
	if err != nil {
		t.Fatalf("reading last-known-good: %v", err)
	}

	if lkg.RunID != res.RunID {
		t.Errorf("last-known-good is run %q, want %q", lkg.RunID, res.RunID)
	}

	if tree.closed != 1 {
		t.Errorf("source tree closed %d times, want exactly once", tree.closed)
	}
}

func TestRun_TheSnapshotIsTaggedWithTheSetAndDomainUnderOneSourceIdentity(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()

	req := request(set, repo, &fakeTree{report: completeScan()})
	if _, err := runner(t, j).Run(context.Background(), req); err != nil {
		t.Fatalf("running: %v", err)
	}

	if got := repo.tags[backupengine.TagKeyBackupSet]; got != set.String() {
		t.Errorf("snapshot carries set tag %q, want %q", got, set)
	}

	if got := repo.tags[backupengine.TagKeyDomain]; got != "vault" {
		t.Errorf("snapshot carries domain tag %q, want %q", got, "vault")
	}

	if repo.source != req.Source {
		t.Errorf("snapshot stored under source %+v, want %+v", repo.source, req.Source)
	}

	if repo.treeCalls != 1 {
		t.Errorf("the engine was asked for %d snapshots, want exactly one per run", repo.treeCalls)
	}
}

func TestRun_TheFourByteNumbersAreRecordedAsFourDifferentFacts(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()
	repo.tree = func(req backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error) {
		return backupengine.TreeSnapshotInfo{
			SnapshotInfo: backupengine.SnapshotInfo{
				ID: "snap-1", Source: req.Source, Files: 10, Directories: 2, Bytes: 100_000,
			},
			SourceBytesRead:        90_000,
			RepositoryBytesWritten: 4_000,

			// The engine's own dedup accounting, which is nothing like
			// read minus written (86000): that difference is reuse plus
			// compression plus the pack and index overhead of storing
			// anything at all, and recording it as reuse credits a run
			// with content it actually re-uploaded.
			ContentReusedBytes:   12_000,
			ContentReuseMeasured: true,
		}, nil
	}

	res, err := runner(t, j).Run(context.Background(), request(set, repo, &fakeTree{report: completeScan()}))
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	if res.LogicalBytes != 100_000 {
		t.Errorf("logical bytes %d, want 100000", res.LogicalBytes)
	}

	if res.SourceBytesRead != 90_000 {
		t.Errorf("source bytes read %d, want 90000: a tree whose metadata let some entries be skipped read less than it scanned", res.SourceBytesRead)
	}

	if res.RepositoryBytesWritten != 4_000 {
		t.Errorf("repository bytes written %d, want 4000", res.RepositoryBytesWritten)
	}

	if res.ContentReusedBytes != 12_000 {
		t.Errorf("content reused %d, want 12000: the engine's dedup accounting, not read minus written", res.ContentReusedBytes)
	}

	if res.Entries != completeScan().Entries {
		t.Errorf("entries scanned %d, want %d: the source side's own census, not files plus directories",
			res.Entries, completeScan().Entries)
	}

	// The point of the four fields: nothing may present the logical size
	// as what was uploaded.
	if res.LogicalBytes == res.RepositoryBytesWritten {
		t.Error("logical bytes and repository bytes written are equal, so the report cannot distinguish a deduplicated run from a full upload")
	}
}

func TestRun_AFailedSnapshotWriteLeavesNoRestorePointAndDoesNotDisturbLastKnownGood(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()
	r := runner(t, j)

	// A first run that succeeds, so there is a last-known-good to protect.
	first, err := r.Run(context.Background(), request(set, repo, &fakeTree{report: completeScan()}))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	repo.tree = func(backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error) {
		return backupengine.TreeSnapshotInfo{}, errors.New("the repository stopped answering")
	}

	second := request(set, repo, &fakeTree{report: completeScan()})
	second.RunID, second.IdempotencyKey = "run-2", "key-2"

	res, err := r.Run(context.Background(), second)
	if err == nil {
		t.Fatal("a run whose snapshot was never stored reported success")
	}

	if res.Succeeded() {
		t.Errorf("failed run advertises a restore point: phase %s", res.Phase)
	}

	if res.Phase != state.PhaseFailed {
		t.Errorf("failed run is in phase %s, want %s", res.Phase, state.PhaseFailed)
	}

	if res.SnapshotID != "" {
		t.Errorf("a run that stored nothing recorded snapshot id %q", res.SnapshotID)
	}

	lkg, err := j.LastKnownGoodSnapshot(context.Background(), setUUID(set))
	if err != nil {
		t.Fatalf("reading last-known-good: %v", err)
	}

	if lkg.RunID != first.RunID {
		t.Errorf("last-known-good moved to %q after a newer run failed; want the older successful run %q", lkg.RunID, first.RunID)
	}
}

func TestRun_AnIncompleteSourcePassFailsTheRunAndKeepsTheManifestAttributed(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()

	tree := &fakeTree{report: snapshotlifecycle.ScanReport{
		Entries:  4,
		Stored:   2,
		Complete: false,
		Reason:   "one entry could not be read",
	}}

	res, err := runner(t, j).Run(context.Background(), request(set, repo, tree))
	if err == nil {
		t.Fatal("a run whose source pass had a hole in it reported success")
	}

	if res.Phase != state.PhaseFailed {
		t.Fatalf("phase %s, want %s", res.Phase, state.PhaseFailed)
	}

	if res.SnapshotID == "" {
		t.Error("the manifest this run wrote was not recorded, so nothing can attribute it later")
	}

	if res.Reason == "" {
		t.Error("a failed run recorded no reason")
	}

	if _, err := j.LastKnownGoodSnapshot(context.Background(), setUUID(set)); !errors.Is(err, state.ErrSnapshotRunNotFound) {
		t.Errorf("an incomplete pass produced a last-known-good snapshot (err %v)", err)
	}
}

func TestRun_AFailedVerificationIsNotARestorePoint(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()
	repo.verifyErr = errors.New("content is missing from the repository")

	res, err := runner(t, j).Run(context.Background(), request(set, repo, &fakeTree{report: completeScan()}))
	if err == nil {
		t.Fatal("a run whose snapshot failed verification reported success")
	}

	if res.Phase != state.PhaseFailed {
		t.Errorf("phase %s, want %s", res.Phase, state.PhaseFailed)
	}

	if res.VerificationStatus != "failed" {
		t.Errorf("verification status %q, want %q", res.VerificationStatus, "failed")
	}

	if res.VerificationAchieved != "" {
		t.Errorf("a failed verification recorded an achieved level %q", res.VerificationAchieved)
	}

	if _, err := j.LastKnownGoodSnapshot(context.Background(), setUUID(set)); !errors.Is(err, state.ErrSnapshotRunNotFound) {
		t.Errorf("a failed verification produced a last-known-good snapshot (err %v)", err)
	}
}

// TestRun_TheConfiguredVerificationLevelAndTheProvenOneAreRecordedSeparately
// is the two-column rule: what the set asked for and what this run
// proved are different facts, and a row carrying only the first would
// assert a verification nobody performed.
//
// The case that makes the distinction visible is a run that proved MORE
// than its set requires, because a periodic full read was due. The
// configured column must still say what the set is configured for --
// tomorrow's run, with no escalation due, is back to structural -- and
// the achieved column must say what actually happened.
func TestRun_TheConfiguredVerificationLevelAndTheProvenOneAreRecordedSeparately(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")

	req := request(set, newFakeRepository(), &fakeTree{report: completeScan()})
	req.VerificationLevel = model.LevelStructural
	req.Verification = snapshotlifecycle.VerificationOptions{FullEvery: 7 * 24 * time.Hour}

	res, err := runner(t, j).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	if res.VerificationLevel != model.LevelStructural {
		t.Errorf("configured level %q, want %q", res.VerificationLevel, model.LevelStructural)
	}

	if res.VerificationAchieved != model.LevelContentFull {
		t.Errorf("achieved level %q, want %q: this set had never had a full content read, so one was due",
			res.VerificationAchieved, model.LevelContentFull)
	}
}

func TestRun_AReplayedIdempotencyKeyNeverReadsTheSourceASecondTime(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()
	r := runner(t, j)

	first, err := r.Run(context.Background(), request(set, repo, &fakeTree{report: completeScan()}))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	replay := request(set, repo, &fakeTree{report: completeScan()})
	replay.RunID = "run-2" // a client that retried without remembering its run id

	res, err := r.Run(context.Background(), replay)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if !res.Replayed {
		t.Error("the replay was not reported as one")
	}

	if res.RunID != first.RunID {
		t.Errorf("replay resolved to run %q, want the original %q", res.RunID, first.RunID)
	}

	if repo.treeCalls != 1 {
		t.Errorf("the source was read %d times for one logical request", repo.treeCalls)
	}
}

func TestRun_AReplayOfAnInFlightRunIsRefusedRatherThanStartingASecondPass(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()

	// A row left mid-flight, exactly as a killed process leaves one.
	begun, err := j.BeginSnapshotRun(context.Background(), state.SnapshotRunRequest{
		RunID:             "run-1",
		IdempotencyKey:    "key-1",
		Set:               set,
		SetUUID:           setUUID(set),
		Engine:            model.EngineKopia.String(),
		Domain:            "vault",
		SourceIdentity:    "ab12cd34",
		VerificationLevel: string(model.LevelStructural),
		StartedAt:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seeding a run: %v", err)
	}

	if err := j.AdvanceSnapshotRun(context.Background(), begun.Run.RunID, state.PhaseSourceScan, state.SnapshotRunUpdate{At: time.Now().UTC()}); err != nil {
		t.Fatalf("seeding a phase: %v", err)
	}

	res, err := runner(t, j).Run(context.Background(), request(set, repo, &fakeTree{report: completeScan()}))
	if !errors.Is(err, snapshotlifecycle.ErrRunInFlight) {
		t.Fatalf("replaying an in-flight run returned %v, want ErrRunInFlight", err)
	}

	if res.Phase != state.PhaseSourceScan {
		t.Errorf("the in-flight run was reported at phase %s, want %s", res.Phase, state.PhaseSourceScan)
	}

	if repo.treeCalls != 0 {
		t.Errorf("a refused replay still read the source (%d snapshot calls)", repo.treeCalls)
	}
}

func TestRun_ARunThatCannotBeIdentifiedOrRecordedIsRefusedBeforeAnyDurableWrite(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()

	cases := map[string]func(*snapshotlifecycle.RunRequest){
		"no run id":             func(r *snapshotlifecycle.RunRequest) { r.RunID = "" },
		"no idempotency key":    func(r *snapshotlifecycle.RunRequest) { r.IdempotencyKey = "" },
		"no backup set":         func(r *snapshotlifecycle.RunRequest) { r.Set = model.BackupSetID{} },
		"no repository domain":  func(r *snapshotlifecycle.RunRequest) { r.Domain = "" },
		"no source identity":    func(r *snapshotlifecycle.RunRequest) { r.SourceIdentity = "" },
		"no verification level": func(r *snapshotlifecycle.RunRequest) { r.VerificationLevel = "" },
		"the artifact engine":   func(r *snapshotlifecycle.RunRequest) { r.Engine = model.EngineArtifact },
		"no repository":         func(r *snapshotlifecycle.RunRequest) { r.Repository = nil },
		"no source":             func(r *snapshotlifecycle.RunRequest) { r.OpenTree = nil },
	}

	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := request(set, repo, &fakeTree{report: completeScan()})
			break_(&req)

			if _, err := runner(t, j).Run(context.Background(), req); err == nil {
				t.Fatalf("a request with %s was accepted", name)
			}
		})
	}
}

func TestRun_TheSourceIsClosedEvenWhenAPhaseFails(t *testing.T) {
	t.Parallel()

	j := journal(t)
	set := setID(t, "postgres")
	repo := newFakeRepository()
	repo.tree = func(backupengine.TreeSnapshotRequest) (backupengine.TreeSnapshotInfo, error) {
		return backupengine.TreeSnapshotInfo{}, errors.New("storage is full")
	}

	tree := &fakeTree{report: completeScan()}
	if _, err := runner(t, j).Run(context.Background(), request(set, repo, tree)); err == nil {
		t.Fatal("expected the run to fail")
	}

	if tree.closed != 1 {
		t.Errorf("source closed %d times after a failed run, want exactly once", tree.closed)
	}
}
