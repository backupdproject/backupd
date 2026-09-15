// The snapshot-run catalog: that a replayed begin finds the row it already
// made, that a run only ever moves the way the machine allows, and above
// all that a failure can never take last-known-good away from a success
// that already happened.
//
// The last one is the reason this file is long. "A failed newer snapshot
// cannot replace last-known-good" is a sentence about policy, and the
// durable half of it is the one assertion nothing above this layer can
// make on its own: if the flag can be moved by anything other than
// reaching SUCCESS, then every caller that asks "what may I restore" is
// reading a row that a crashed run wrote. So the failure case is driven
// from every pre-commit phase in turn rather than from one convenient
// one, because the phases are exactly where a crash lands.
//
// The other tests that earn their place here are the refusals. Advancing
// backwards, departing a terminal phase, requesting a delete of a run that
// has no snapshot to delete, and listing without a bound are all writes
// this package must refuse in a sentence rather than perform quietly, and
// a refusal nobody drives is a refusal that stops working.

package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/model"
)

// testSnapshotSet is the identity most tests here use, built through
// model.NewBackupSetID for testArtifact's reason: a set id this product
// could not address would otherwise produce rows nothing can read back.
func testSnapshotSet(t *testing.T, source, set string) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID(%q, %q): %v", source, set, err)
	}
	return id
}

// testSetUUID is the DURABLE identifier the catalog keys a set's snapshot
// lineage on (see 0010_snapshot_run_lineage.sql). It is derived from the
// set's names here only so that a test which never mentions it gets a
// distinct one per set; the whole point of the column is that the names
// can change afterwards and this cannot, which is what the rename test
// below drives.
func testSetUUID(set model.BackupSetID) string {
	return "uuid-" + set.Source + "-" + set.Set
}

// snapshotRunAt is the base time every run in this file starts from, so an
// ordering assertion is about the timestamps the test chose rather than
// about how long the test took to run.
var snapshotRunAt = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

func testSnapshotRunRequest(t *testing.T, runID, key string, set model.BackupSetID, startedAt time.Time) SnapshotRunRequest {
	t.Helper()
	return SnapshotRunRequest{
		RunID:             runID,
		IdempotencyKey:    key,
		Set:               set,
		OperationID:       "op-1",
		Engine:            "kopia",
		Domain:            "nas-primary",
		SourceIdentity:    "sha256:5f0e1a",
		ConsistencyMode:   "snapshot",
		VerificationLevel: "content_full",
		StartedAt:         startedAt,
		SetUUID:           testSetUUID(set),
	}
}

// beginRun is the two lines every test below would otherwise repeat.
func beginRun(t *testing.T, j *Journal, req SnapshotRunRequest) SnapshotRun {
	t.Helper()
	out, err := j.BeginSnapshotRun(context.Background(), req)
	if err != nil {
		t.Fatalf("BeginSnapshotRun(%s): %v", req.RunID, err)
	}
	if !out.Created {
		t.Fatalf("BeginSnapshotRun(%s): Created = false, want true for a fresh key", req.RunID)
	}
	return out.Run
}

// advanceThrough walks a run along the nominal path, one phase per minute
// so the transition log has a real order in it.
func advanceThrough(t *testing.T, j *Journal, runID string, from time.Time, phases ...SnapshotPhase) {
	t.Helper()
	for i, p := range phases {
		at := from.Add(time.Duration(i+1) * time.Minute)
		if err := j.AdvanceSnapshotRun(context.Background(), runID, p, SnapshotRunUpdate{At: at}); err != nil {
			t.Fatalf("AdvanceSnapshotRun(%s -> %s): %v", runID, p, err)
		}
	}
}

// ParseSnapshotPhase is the door every phase string coming from outside Go
// (a config file, an API request, a row written by a build that knew a
// phase this one does not) comes through, so what it refuses is the whole
// value of having it. Defaulting an unrecognised phase to PENDING would
// hand a crash reconciler a finished run to re-drive; defaulting it to
// FAILED would write off a snapshot that exists.
func TestParseSnapshotPhase_RefusesEmptyAndUnknown(t *testing.T) {
	for _, p := range SnapshotPhases() {
		got, err := ParseSnapshotPhase(string(p))
		if err != nil {
			t.Errorf("ParseSnapshotPhase(%q): %v", p, err)
		}
		if got != p {
			t.Errorf("ParseSnapshotPhase(%q) = %q, want %q", p, got, p)
		}
	}

	for _, junk := range []string{"", "pending", "SUCCEEDED", "RUNNING", " SUCCESS"} {
		if _, err := ParseSnapshotPhase(junk); err == nil {
			t.Errorf("ParseSnapshotPhase(%q) = nil error, want a refusal", junk)
		}
	}
}

// Terminal and Advertised are two different questions and a reader who
// conflates them offers a restore point that was never written. Terminal
// says the run is over (so the crash reconciler leaves it alone);
// Advertised says its snapshot is a restore point a caller may offer, and
// only SUCCESS is that.
func TestSnapshotPhase_TerminalAndAdvertisedAreNotTheSameQuestion(t *testing.T) {
	wantTerminal := map[SnapshotPhase]bool{
		PhaseSuccess: true, PhaseFailed: true, PhaseLost: true,
		PhaseDeleted: true, PhaseQuarantined: true,
	}
	for _, p := range SnapshotPhases() {
		if got := p.Terminal(); got != wantTerminal[p] {
			t.Errorf("%s.Terminal() = %v, want %v", p, got, wantTerminal[p])
		}
		if got := p.Advertised(); got != (p == PhaseSuccess) {
			t.Errorf("%s.Advertised() = %v, want %v", p, got, p == PhaseSuccess)
		}
	}
}

// The idempotency contract CreateOperation already holds, held here for the
// same reason: a run driver that crashed between committing this row and
// observing it must resolve to the row it already made, not start a second
// snapshot of the same source.
func TestBeginSnapshotRun_ReplayResolvesToTheExistingRow(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	first := beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))
	if first.Phase != PhasePending {
		t.Fatalf("a new run starts at %q, want %q", first.Phase, PhasePending)
	}
	if first.LastKnownGood {
		t.Fatal("a new run is last-known-good, want false: nothing has succeeded yet")
	}

	// A retry mints a fresh run id (as a driver generating a UUID per
	// attempt would) and replays the same key.
	replay, err := j.BeginSnapshotRun(ctx, testSnapshotRunRequest(t, "run-2", "idem-1", set, snapshotRunAt.Add(time.Hour)))
	if err != nil {
		t.Fatalf("replayed BeginSnapshotRun: %v", err)
	}
	if replay.Created {
		t.Error("Created = true on a replayed idempotency key, want false")
	}
	if replay.Run.RunID != "run-1" {
		t.Errorf("replay resolved to run %q, want run-1", replay.Run.RunID)
	}
	if !replay.Run.StartedAt.Equal(snapshotRunAt) {
		t.Errorf("replay StartedAt = %v, want the original %v", replay.Run.StartedAt, snapshotRunAt)
	}

	if _, err := j.GetSnapshotRun(ctx, "run-2"); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Errorf("GetSnapshotRun(run-2) error = %v, want ErrSnapshotRunNotFound: the replay must not have inserted a second row", err)
	}
}

// A key is a promise about one logical run. Serving back a run of a
// different set, engine, domain or source would tell a caller "your
// snapshot is already in flight" about a snapshot of something else, and
// the caller would then skip taking the one it actually asked for.
//
// "A different set" means a different LINEAGE. A set that was renamed
// between a submission and its retry is the same set, and refusing the
// replay over the rename would start a second pass over one source.
func TestBeginSnapshotRun_RefusesAKeyReusedForADifferentRun(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")
	other := testSnapshotSet(t, "production", "uploads")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))

	differing := map[string]func(*SnapshotRunRequest){
		"set lineage":     func(r *SnapshotRunRequest) { r.SetUUID = testSetUUID(other) },
		"engine":          func(r *SnapshotRunRequest) { r.Engine = "restic" },
		"domain":          func(r *SnapshotRunRequest) { r.Domain = "nas-secondary" },
		"source identity": func(r *SnapshotRunRequest) { r.SourceIdentity = "sha256:deadbe" },
	}
	for name, mutate := range differing {
		req := testSnapshotRunRequest(t, "run-x", "idem-1", set, snapshotRunAt)
		mutate(&req)
		if _, err := j.BeginSnapshotRun(ctx, req); !errors.Is(err, ErrSnapshotRunIdempotencyKeyReused) {
			t.Errorf("key reused for a different %s: error = %v, want ErrSnapshotRunIdempotencyKeyReused", name, err)
		}
	}

	// The rename: same lineage, new names, same key. That is a replay of
	// the run that already exists, not a different piece of work.
	renamed := testSnapshotRunRequest(t, "run-x", "idem-1", testSnapshotSet(t, "prod-eu", "postgres-main"), snapshotRunAt)
	renamed.SetUUID = testSetUUID(set)
	out, err := j.BeginSnapshotRun(ctx, renamed)
	if err != nil {
		t.Fatalf("BeginSnapshotRun for a renamed set under the same key: %v", err)
	}
	if out.Created || out.Run.RunID != "run-1" {
		t.Errorf("a renamed set replaying its key created = %v, run = %q; want a replay of run-1", out.Created, out.Run.RunID)
	}
}

// One advance is one transaction: the phase, the facts the caller learned
// on the way, the updated_at stamp and the log entry all land together or
// none of them do. Reading it all back through a fresh Get rather than
// trusting the call is what proves it was written.
func TestAdvanceSnapshotRun_WritesPhaseFactsAndTransitionTogether(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")
	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))

	at := snapshotRunAt.Add(2 * time.Minute)
	advanceThrough(t, j, "run-1", snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite)
	err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseManifestCommitted, SnapshotRunUpdate{
		SnapshotID:             new("k1a2b3c4"),
		Files:                  new(int64(1204)),
		Directories:            new(int64(58)),
		LogicalBytes:           new(int64(9_000_000)),
		SourceBytesRead:        new(int64(8_100_000)),
		RepositoryBytesWritten: new(int64(2_300_000)),
		ContentReusedBytes:     new(int64(5_800_000)),
		VerificationStatus:     new("pending"),
		At:                     at.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("AdvanceSnapshotRun to MANIFEST_COMMITTED: %v", err)
	}

	got, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if got.Phase != PhaseManifestCommitted {
		t.Errorf("Phase = %q, want %q", got.Phase, PhaseManifestCommitted)
	}
	if got.SnapshotID != "k1a2b3c4" {
		t.Errorf("SnapshotID = %q, want k1a2b3c4", got.SnapshotID)
	}
	if got.Files == nil || *got.Files != 1204 {
		t.Errorf("Files = %v, want 1204", got.Files)
	}
	if got.LogicalBytes == nil || *got.LogicalBytes != 9_000_000 {
		t.Errorf("LogicalBytes = %v, want 9000000", got.LogicalBytes)
	}
	if got.ContentReusedBytes == nil || *got.ContentReusedBytes != 5_800_000 {
		t.Errorf("ContentReusedBytes = %v, want 5800000", got.ContentReusedBytes)
	}
	if got.VerificationStatus != "pending" {
		t.Errorf("VerificationStatus = %q, want pending", got.VerificationStatus)
	}
	if !got.UpdatedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, at.Add(time.Minute))
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil: the run is still in flight", got.CompletedAt)
	}

	// A fact the caller said nothing about this time survives the next
	// advance: an update is what changed, not a whole row.
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseVerification, SnapshotRunUpdate{At: at.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("AdvanceSnapshotRun to VERIFICATION: %v", err)
	}
	after, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if after.SnapshotID != "k1a2b3c4" || after.Files == nil || *after.Files != 1204 {
		t.Errorf("an update that said nothing about them cleared SnapshotID/Files: %q, %v", after.SnapshotID, after.Files)
	}

	transitions, err := j.SnapshotRunTransitions(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRunTransitions: %v", err)
	}
	want := []struct{ from, to SnapshotPhase }{
		{PhasePending, PhaseSourceScan},
		{PhaseSourceScan, PhaseSnapshotWrite},
		{PhaseSnapshotWrite, PhaseManifestCommitted},
		{PhaseManifestCommitted, PhaseVerification},
	}
	if len(transitions) != len(want) {
		t.Fatalf("got %d transitions, want %d: %+v", len(transitions), len(want), transitions)
	}
	for i, w := range want {
		if transitions[i].From != w.from || transitions[i].To != w.to {
			t.Errorf("transition %d = %s -> %s, want %s -> %s",
				i, transitions[i].From, transitions[i].To, w.from, w.to)
		}
	}
}

// Forward-only, with the one crash-recovery exception. A backward move is
// a caller re-driving work whose result is already recorded, and applying
// it would put a finished run back on the reconciler's worklist; departing
// a terminal phase is worse, because it rewrites what happened.
func TestAdvanceSnapshotRun_RefusesRegressionAndTerminalDeparture(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))
	advanceThrough(t, j, "run-1", snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted)

	back := j.AdvanceSnapshotRun(ctx, "run-1", PhaseSourceScan, SnapshotRunUpdate{At: snapshotRunAt.Add(time.Hour)})
	if !errors.Is(back, ErrSnapshotPhaseRegression) {
		t.Errorf("MANIFEST_COMMITTED -> SOURCE_SCAN error = %v, want ErrSnapshotPhaseRegression", back)
	}

	advanceThrough(t, j, "run-1", snapshotRunAt.Add(time.Hour), PhaseVerification, PhaseCatalogCommit, PhaseSuccess)
	for _, to := range []SnapshotPhase{PhaseSourceScan, PhaseFailed, PhaseCatalogCommit} {
		err := j.AdvanceSnapshotRun(ctx, "run-1", to, SnapshotRunUpdate{At: snapshotRunAt.Add(2 * time.Hour)})
		if !errors.Is(err, ErrSnapshotPhaseRegression) {
			t.Errorf("SUCCESS -> %s error = %v, want ErrSnapshotPhaseRegression", to, err)
		}
	}

	// A refused advance changes nothing, which is the half a caller that
	// retries depends on.
	got, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if got.Phase != PhaseSuccess {
		t.Errorf("after three refused advances Phase = %q, want %q", got.Phase, PhaseSuccess)
	}
}

// The one re-entry crash recovery needs: a verification interrupted by a
// crash is retried from the manifest, which is durable. Without it the
// reconciler's only options for a run that died mid-verification are to
// fail a snapshot that exists or to leave it unfinished for ever.
func TestAdvanceSnapshotRun_VerificationReEntersFromTheCommittedManifest(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))
	advanceThrough(t, j, "run-1", snapshotRunAt,
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification)

	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseManifestCommitted, SnapshotRunUpdate{
		Reason: new("verification did not finish before the process died; retrying from the committed manifest"),
		At:     snapshotRunAt.Add(time.Hour),
	}); err != nil {
		t.Fatalf("VERIFICATION -> MANIFEST_COMMITTED: %v", err)
	}

	got, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if got.Phase != PhaseManifestCommitted {
		t.Fatalf("Phase = %q, want %q", got.Phase, PhaseManifestCommitted)
	}

	// The re-entry is logged like any other edge: an operator asking why a
	// run verified twice has the row that says so.
	transitions, err := j.SnapshotRunTransitions(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRunTransitions: %v", err)
	}
	last := transitions[len(transitions)-1]
	if last.From != PhaseVerification || last.To != PhaseManifestCommitted {
		t.Errorf("last transition = %s -> %s, want VERIFICATION -> MANIFEST_COMMITTED", last.From, last.To)
	}

	// And the run can go forward again from there.
	advanceThrough(t, j, "run-1", snapshotRunAt.Add(2*time.Hour), PhaseVerification, PhaseCatalogCommit, PhaseSuccess)
}

// Re-advancing to the phase a run is already in is a retry of work whose
// result is already recorded, not a new edge: it applies whatever facts
// the caller learned and stamps updated_at, and it appends no second
// transition row. A log with a self-edge in it would make "how did this
// run get here" unreadable at exactly the moment somebody needs it.
func TestAdvanceSnapshotRun_SamePhaseAppliesFactsAndLogsNoSelfEdge(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))
	advanceThrough(t, j, "run-1", snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite)

	again := snapshotRunAt.Add(30 * time.Minute)
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseSnapshotWrite, SnapshotRunUpdate{
		SourceBytesRead: new(int64(4_096)),
		At:              again,
	}); err != nil {
		t.Fatalf("re-advance to the same phase: %v", err)
	}

	got, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if !got.UpdatedAt.Equal(again) {
		t.Errorf("UpdatedAt = %v, want %v: a same-phase advance is still progress", got.UpdatedAt, again)
	}
	if got.SourceBytesRead == nil || *got.SourceBytesRead != 4_096 {
		t.Errorf("SourceBytesRead = %v, want 4096", got.SourceBytesRead)
	}

	transitions, err := j.SnapshotRunTransitions(ctx, "run-1")
	if err != nil {
		t.Fatalf("SnapshotRunTransitions: %v", err)
	}
	if len(transitions) != 2 {
		t.Fatalf("got %d transitions, want 2 (PENDING->SOURCE_SCAN, SOURCE_SCAN->SNAPSHOT_WRITE): %+v", len(transitions), transitions)
	}

	// A same-phase re-advance to SUCCESS must not move last-known-good
	// either: only the edge INTO success does that, and by the time this
	// runs a newer run may hold the flag.
	advanceThrough(t, j, "run-1", again, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)
	beginRun(t, j, testSnapshotRunRequest(t, "run-2", "idem-2", set, snapshotRunAt.Add(6*time.Hour)))
	advanceThrough(t, j, "run-2", snapshotRunAt.Add(6*time.Hour),
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)

	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseSuccess, SnapshotRunUpdate{At: snapshotRunAt.Add(12 * time.Hour)}); err != nil {
		t.Fatalf("re-advance of the older run to SUCCESS: %v", err)
	}
	lkg, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set))
	if err != nil {
		t.Fatalf("LastKnownGoodSnapshot: %v", err)
	}
	if lkg.RunID != "run-2" {
		t.Errorf("last-known-good = %q, want run-2: a replayed success must not take the flag back", lkg.RunID)
	}
}

// The configured level and the achieved level are two different claims and
// a row that mixed them would advertise a verification that never ran:
// asking for content_full and proving structural must read as exactly
// that, not as content_full.
func TestAdvanceSnapshotRun_KeepsConfiguredAndAchievedVerificationLevelsApart(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	req := testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt)
	req.VerificationLevel = "content_full"
	beginRun(t, j, req)

	fresh, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if fresh.VerificationLevelAchieved != "" {
		t.Errorf("VerificationLevelAchieved = %q on a fresh run, want empty: nothing has been proven yet", fresh.VerificationLevelAchieved)
	}

	advanceThrough(t, j, "run-1", snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted)
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseVerification, SnapshotRunUpdate{
		VerificationLevelAchieved: new("structural"),
		VerificationStatus:        new("passed"),
		At:                        snapshotRunAt.Add(time.Hour),
	}); err != nil {
		t.Fatalf("AdvanceSnapshotRun to VERIFICATION: %v", err)
	}

	got, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if got.VerificationLevel != "content_full" {
		t.Errorf("VerificationLevel = %q, want content_full: what the operator asked for does not change", got.VerificationLevel)
	}
	if got.VerificationLevelAchieved != "structural" {
		t.Errorf("VerificationLevelAchieved = %q, want structural", got.VerificationLevelAchieved)
	}
}

// The durable half of "a failed newer snapshot cannot replace
// last-known-good" (EPIC K). A success sets the flag and takes it off the
// previous holder; nothing else in this package touches it, so a newer run
// that dies at any phase before the flag would have moved leaves the older
// restore point exactly where it was.
//
// The failure is driven from every pre-commit phase in turn rather than
// from one, because those phases are where a crash actually lands, and a
// rule that held for five of them and not the sixth would be a rule that
// loses a restore point on one particular kind of bad night.
func TestLastKnownGood_OnlySuccessMovesItAndNoFailureEverDoes(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	beginRun(t, j, testSnapshotRunRequest(t, "good-1", "idem-good-1", set, snapshotRunAt))
	advanceThrough(t, j, "good-1", snapshotRunAt,
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)

	good, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set))
	if err != nil {
		t.Fatalf("LastKnownGoodSnapshot after the first success: %v", err)
	}
	if good.RunID != "good-1" || !good.LastKnownGood {
		t.Fatalf("last-known-good = %q (flag %v), want good-1 (true)", good.RunID, good.LastKnownGood)
	}
	if good.CompletedAt == nil {
		t.Error("CompletedAt = nil on a successful run, want the time it reached SUCCESS")
	}

	path := []SnapshotPhase{
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit,
	}
	// Every pre-commit phase a newer run can die in, including PENDING
	// (index 0 walks nothing).
	for i := range len(path) + 1 {
		runID := "bad-" + string(rune('a'+i))
		started := snapshotRunAt.Add(time.Duration(i+1) * 24 * time.Hour)
		beginRun(t, j, testSnapshotRunRequest(t, runID, "idem-"+runID, set, started))
		advanceThrough(t, j, runID, started, path[:i]...)

		if err := j.AdvanceSnapshotRun(ctx, runID, PhaseFailed, SnapshotRunUpdate{
			Reason: new("the repository refused the write"),
			At:     started.Add(time.Hour),
		}); err != nil {
			t.Fatalf("failing %s: %v", runID, err)
		}

		still, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set))
		if err != nil {
			t.Fatalf("LastKnownGoodSnapshot after %s failed: %v", runID, err)
		}
		if still.RunID != "good-1" {
			t.Fatalf("after %s failed at phase index %d, last-known-good = %q, want good-1", runID, i, still.RunID)
		}
	}

	// A quarantined snapshot was never one of our runs, so it cannot touch
	// the flag at all. LOST and DELETED are different: those rows DID hold
	// the flag, because the only way to reach either is through SUCCESS,
	// and the phase they end in says their snapshot is not in the
	// repository any more. A flag left set on one of those would let
	// LastKnownGoodSnapshot hand a caller a snapshot id that resolves to
	// nothing, which is the one answer worse than "there is none".
	for _, to := range []SnapshotPhase{PhaseQuarantined, PhaseLost, PhaseDeleted} {
		runID := "other-" + string(to[0])
		started := snapshotRunAt.Add(30 * 24 * time.Hour)
		beginRun(t, j, testSnapshotRunRequest(t, runID, "idem-"+runID, set, started))

		if to == PhaseQuarantined {
			if err := j.AdvanceSnapshotRun(ctx, runID, to, SnapshotRunUpdate{
				Reason: new("a repository snapshot no configured set can account for"),
				At:     started.Add(time.Hour),
			}); err != nil {
				t.Fatalf("quarantining %s: %v", runID, err)
			}
			lkg, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set))
			if err != nil {
				t.Fatalf("LastKnownGoodSnapshot after a quarantine: %v", err)
			}
			if lkg.RunID != "good-1" {
				t.Errorf("after a quarantine, last-known-good = %q, want good-1", lkg.RunID)
			}
			continue
		}

		advanceThrough(t, j, runID, started,
			PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)
		holding, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set))
		if err != nil || holding.RunID != runID {
			t.Fatalf("after %s succeeded, last-known-good = %q (err %v), want %s", runID, holding.RunID, err, runID)
		}

		if err := j.AdvanceSnapshotRun(ctx, runID, to, SnapshotRunUpdate{
			Reason: new("the snapshot is no longer in the repository"),
			At:     started.Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("advancing %s to %s: %v", runID, to, err)
		}
		if _, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set)); !errors.Is(err, ErrSnapshotRunNotFound) {
			t.Errorf("after the last-known-good run reached %s, LastKnownGoodSnapshot error = %v, want ErrSnapshotRunNotFound: a snapshot that is not in the repository is not a restore point", to, err)
		}
		gone, err := j.GetSnapshotRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetSnapshotRun(%s): %v", runID, err)
		}
		if gone.LastKnownGood {
			t.Errorf("%s is at %s and still flagged last-known-good", runID, to)
		}

		// Which is exactly the hole RepointLastKnownGood fills: the older
		// success is still there and still restorable.
		if err := j.RepointLastKnownGood(ctx, testSetUUID(set), "good-1"); err != nil {
			t.Fatalf("RepointLastKnownGood back to good-1: %v", err)
		}
	}
}

// A set with no successful run has no last-known-good, and saying so is
// not the same as returning a zero row: a caller handed a zero SnapshotRun
// would offer a restore point with an empty snapshot id.
func TestLastKnownGoodSnapshot_NotFoundForASetThatHasNeverSucceeded(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")
	other := testSnapshotSet(t, "production", "uploads")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))
	if _, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set)); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Errorf("LastKnownGoodSnapshot with only an in-flight run: error = %v, want ErrSnapshotRunNotFound", err)
	}

	advanceThrough(t, j, "run-1", snapshotRunAt,
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)

	// One set's success says nothing about another set's.
	if _, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(other)); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Errorf("LastKnownGoodSnapshot for a different set: error = %v, want ErrSnapshotRunNotFound", err)
	}
}

// Renaming a backup set must not fork its snapshot lineage.
//
// This is the durable half of the finding: everything a run is looked up
// by used to be keyed on (source, backup_set), which are the NAMES in an
// operator's configuration. So an operator who moved a set to a different
// source stanza, or simply renamed it, silently got a set with no
// history, no unfinished work and no restore point -- while the old rows
// sat in the table still holding the last-known-good flag, ready to make
// a second one the next time this set succeeded. The set's uuid does not
// move, so nothing above this layer notices the rename at all.
func TestSnapshotLineage_SurvivesARenameOfTheSet(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()

	before := testSnapshotSet(t, "production", "postgres-primary")
	after := testSnapshotSet(t, "prod-eu", "postgres-main")
	lineage := testSetUUID(before)

	beginRun(t, j, testSnapshotRunRequest(t, "old-success", "idem-old", before, snapshotRunAt))
	advanceThrough(t, j, "old-success", snapshotRunAt,
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)

	// A second run of the same set that was still in flight when the
	// process died: the reconciler has to find this one after the rename
	// too, or a crashed run becomes invisible work.
	beginRun(t, j, testSnapshotRunRequest(t, "old-unfinished", "idem-unfinished", before, snapshotRunAt.Add(time.Hour)))
	advanceThrough(t, j, "old-unfinished", snapshotRunAt.Add(time.Hour), PhaseSourceScan, PhaseSnapshotWrite)

	// The rename: same lineage, new names.
	renamed := testSnapshotRunRequest(t, "new-success", "idem-new", after, snapshotRunAt.Add(2*time.Hour))
	renamed.SetUUID = lineage
	beginRun(t, j, renamed)

	history, err := j.ListSnapshotRuns(ctx, lineage, 10)
	if err != nil {
		t.Fatalf("ListSnapshotRuns: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("the lineage lists %d runs after the rename, want 3: %+v", len(history), history)
	}

	unfinished, err := j.UnfinishedSnapshotRuns(ctx)
	if err != nil {
		t.Fatalf("UnfinishedSnapshotRuns: %v", err)
	}
	var found bool
	for _, run := range unfinished {
		if run.RunID == "old-unfinished" {
			found = run.SetUUID == lineage
		}
	}
	if !found {
		t.Error("the run interrupted before the rename is not attributable to the renamed set's lineage")
	}

	advanceThrough(t, j, "new-success", snapshotRunAt.Add(2*time.Hour),
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)

	lkg, err := j.LastKnownGoodSnapshot(ctx, lineage)
	if err != nil {
		t.Fatalf("LastKnownGoodSnapshot after the rename: %v", err)
	}
	if lkg.RunID != "new-success" {
		t.Errorf("last-known-good = %q, want new-success", lkg.RunID)
	}

	// And exactly one row holds it. Two would mean "the" restore point
	// depends on which row a reader happens to get.
	var holders int
	for _, run := range history {
		got, err := j.GetSnapshotRun(ctx, run.RunID)
		if err != nil {
			t.Fatalf("GetSnapshotRun(%s): %v", run.RunID, err)
		}
		if got.LastKnownGood {
			holders++
		}
	}
	if holders != 1 {
		t.Errorf("%d rows in this lineage are flagged last-known-good, want exactly 1: the rename must not have started a second one", holders)
	}
}

// source_complete has three states and entries_scanned has two, and the
// third state of each is the one that matters: a row nobody recorded a
// verdict for is not a row whose verdict was "no", and a run nobody
// measured did not measure zero.
func TestSnapshotRun_RecordsTheSourceVerdictAndTheEntryCountItMeasured(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))

	fresh, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if fresh.SourceComplete != nil {
		t.Errorf("SourceComplete = %v on a fresh run, want nil: nobody has looked at the source yet", *fresh.SourceComplete)
	}
	if fresh.EntriesScanned != nil {
		t.Errorf("EntriesScanned = %v on a fresh run, want nil", *fresh.EntriesScanned)
	}

	advanceThrough(t, j, "run-1", snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite)

	incomplete := false
	entries := int64(9_412)
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseManifestCommitted, SnapshotRunUpdate{
		SnapshotID:     new("kopia-manifest-1"),
		SourceComplete: &incomplete,
		EntriesScanned: &entries,
		At:             snapshotRunAt.Add(time.Hour),
	}); err != nil {
		t.Fatalf("AdvanceSnapshotRun: %v", err)
	}

	got, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if got.SourceComplete == nil || *got.SourceComplete {
		t.Errorf("SourceComplete = %v, want a recorded false: the pass did not cover the source", got.SourceComplete)
	}
	if got.EntriesScanned == nil || *got.EntriesScanned != entries {
		t.Errorf("EntriesScanned = %v, want %d", got.EntriesScanned, entries)
	}

	// An update that says nothing about either leaves both as they are: a
	// later phase must not silently withdraw a verdict.
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseVerification, SnapshotRunUpdate{At: snapshotRunAt.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("AdvanceSnapshotRun to VERIFICATION: %v", err)
	}
	still, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if still.SourceComplete == nil || *still.SourceComplete {
		t.Errorf("SourceComplete = %v after an unrelated advance, want the recorded false", still.SourceComplete)
	}
	if still.EntriesScanned == nil || *still.EntriesScanned != entries {
		t.Errorf("EntriesScanned = %v after an unrelated advance, want %d", still.EntriesScanned, entries)
	}
}

// The crash reconciler's worklist. It is read on startup, by a process
// that did not write any of these rows, so the test closes the journal and
// opens it again: an answer that only held while the writing process was
// alive would be no answer at all.
func TestUnfinishedSnapshotRuns_SurvivesAReopenAndIsOldestFirst(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")

	j, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	alpha := testSnapshotSet(t, "production", "postgres-primary")
	beta := testSnapshotSet(t, "staging", "uploads")

	// Oldest first, created out of order so the ordering assertion is
	// about started_at rather than about insertion order.
	beginRun(t, j, testSnapshotRunRequest(t, "mid", "idem-mid", beta, snapshotRunAt.Add(2*time.Hour)))
	beginRun(t, j, testSnapshotRunRequest(t, "oldest", "idem-oldest", alpha, snapshotRunAt))
	beginRun(t, j, testSnapshotRunRequest(t, "newest", "idem-newest", alpha, snapshotRunAt.Add(5*time.Hour)))
	beginRun(t, j, testSnapshotRunRequest(t, "done", "idem-done", alpha, snapshotRunAt.Add(time.Hour)))

	advanceThrough(t, j, "mid", snapshotRunAt.Add(2*time.Hour), PhaseSourceScan, PhaseSnapshotWrite)
	advanceThrough(t, j, "done", snapshotRunAt.Add(time.Hour),
		PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)

	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	unfinished, err := reopened.UnfinishedSnapshotRuns(ctx)
	if err != nil {
		t.Fatalf("UnfinishedSnapshotRuns: %v", err)
	}
	var ids []string
	for _, r := range unfinished {
		ids = append(ids, r.RunID)
	}
	want := []string{"oldest", "mid", "newest"}
	if len(ids) != len(want) {
		t.Fatalf("unfinished = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("unfinished = %v, want %v (oldest first, across every set, terminal rows excluded)", ids, want)
		}
	}

	// The rows come back whole, not as bare ids: the reconciler decides
	// what to do from the engine, domain and snapshot id on the row.
	if unfinished[0].Domain != "nas-primary" || unfinished[0].Engine != "kopia" {
		t.Errorf("reconciler worklist row = engine %q domain %q, want kopia/nas-primary",
			unfinished[0].Engine, unfinished[0].Domain)
	}
}

// "Is this repository manifest one of ours" is the question that decides
// whether a snapshot found in the repository gets quarantined, and
// "does this domain hold any of ours at all" is the question that decides
// whether a repository may be created at a location. Both are answered
// per DOMAIN and over the whole table: two repositories can hand out the
// same opaque manifest id and they are not the same snapshot, and a
// bounded window over one set's newest rows answers the second question
// "no" for a new set in a populated domain -- which is a second, empty
// repository written over a live one.
func TestDomainSnapshotIDs_AreScopedToTheirDomainAndUnbounded(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))
	advanceThrough(t, j, "run-1", snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite)
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseManifestCommitted, SnapshotRunUpdate{
		SnapshotID: new("kopia-manifest-1"),
		At:         snapshotRunAt.Add(time.Hour),
	}); err != nil {
		t.Fatalf("AdvanceSnapshotRun: %v", err)
	}

	ids, err := j.DomainSnapshotIDs(ctx, "nas-primary")
	if err != nil {
		t.Fatalf("DomainSnapshotIDs: %v", err)
	}
	if ids["kopia-manifest-1"] != "run-1" {
		t.Errorf("manifest kopia-manifest-1 in nas-primary resolves to %q, want run-1", ids["kopia-manifest-1"])
	}
	if len(ids) != 1 {
		t.Errorf("DomainSnapshotIDs = %v, want exactly the one committed manifest: a run with no manifest yet carries '' and is not a manifest id", ids)
	}

	other, err := j.DomainSnapshotIDs(ctx, "nas-secondary")
	if err != nil {
		t.Fatalf("DomainSnapshotIDs(nas-secondary): %v", err)
	}
	if _, ok := other["kopia-manifest-1"]; ok {
		t.Error("the same manifest id is claimed in another domain: one repository's snapshot must not be attributed to a run against another")
	}

	// The creation guard's question, asked of a domain rather than of a
	// set: a domain holding a committed manifest already has a
	// repository, whoever put it there.
	has, err := j.DomainHasSnapshot(ctx, "nas-primary")
	if err != nil {
		t.Fatalf("DomainHasSnapshot: %v", err)
	}
	if !has {
		t.Error("DomainHasSnapshot(nas-primary) = false, want true: run-1 committed a manifest there")
	}

	empty, err := j.DomainHasSnapshot(ctx, "nas-secondary")
	if err != nil {
		t.Fatalf("DomainHasSnapshot(nas-secondary): %v", err)
	}
	if empty {
		t.Error("DomainHasSnapshot(nas-secondary) = true, want false: nothing has ever committed a manifest there")
	}

	// An unnamed domain is refused rather than answered for every row in
	// the table at once.
	if _, err := j.DomainSnapshotIDs(ctx, ""); err == nil {
		t.Error("DomainSnapshotIDs with no domain = nil error, want a refusal")
	}
	if _, err := j.DomainHasSnapshot(ctx, ""); err == nil {
		t.Error("DomainHasSnapshot with no domain = nil error, want a refusal")
	}

	// One manifest, one run. A second run claiming the same manifest in the
	// same domain would make "is this one of ours" depend on which row a
	// reader happened to get, and that answer decides whether the snapshot
	// is advertised, retained or deleted. The same id in another domain is
	// another repository's snapshot and is fine.
	beginRun(t, j, testSnapshotRunRequest(t, "run-2", "idem-2", set, snapshotRunAt.Add(time.Hour)))
	advanceThrough(t, j, "run-2", snapshotRunAt.Add(time.Hour), PhaseSourceScan, PhaseSnapshotWrite)
	err = j.AdvanceSnapshotRun(ctx, "run-2", PhaseManifestCommitted, SnapshotRunUpdate{
		SnapshotID: new("kopia-manifest-1"),
		At:         snapshotRunAt.Add(2 * time.Hour),
	})
	if err == nil {
		t.Error("a second run claiming a manifest another run already recorded = nil error, want a refusal")
	}
	after, err := j.DomainSnapshotIDs(ctx, "nas-primary")
	if err != nil || after["kopia-manifest-1"] != "run-1" {
		t.Errorf("after the refusal, manifest kopia-manifest-1 resolves to %q (err %v), want run-1", after["kopia-manifest-1"], err)
	}

	elsewhere := testSnapshotRunRequest(t, "run-3", "idem-3", set, snapshotRunAt.Add(3*time.Hour))
	elsewhere.Domain = "nas-secondary"
	beginRun(t, j, elsewhere)
	advanceThrough(t, j, "run-3", snapshotRunAt.Add(3*time.Hour), PhaseSourceScan, PhaseSnapshotWrite)
	if err := j.AdvanceSnapshotRun(ctx, "run-3", PhaseManifestCommitted, SnapshotRunUpdate{
		SnapshotID: new("kopia-manifest-1"),
		At:         snapshotRunAt.Add(4 * time.Hour),
	}); err != nil {
		t.Fatalf("the same manifest id in a different domain: %v", err)
	}
}

// The delete intent is what makes a crash during a snapshot delete a
// decidable state rather than a guess, so it is recorded before the delete
// and it does not move the phase. A run with no snapshot to delete, or one
// whose snapshot this product never intended to delete, is refused.
func TestMarkSnapshotDeleteRequested_RecordsIntentAndRefusesTheWrongPhase(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))
	advanceThrough(t, j, "run-1", snapshotRunAt, PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted)

	if err := j.MarkSnapshotDeleteRequested(ctx, "run-1", snapshotRunAt.Add(time.Hour)); err == nil {
		t.Error("MarkSnapshotDeleteRequested on a run at MANIFEST_COMMITTED = nil error, want a refusal")
	}

	advanceThrough(t, j, "run-1", snapshotRunAt.Add(time.Hour), PhaseVerification, PhaseCatalogCommit, PhaseSuccess)
	requested := snapshotRunAt.Add(5 * time.Hour)
	if err := j.MarkSnapshotDeleteRequested(ctx, "run-1", requested); err != nil {
		t.Fatalf("MarkSnapshotDeleteRequested on a successful run: %v", err)
	}

	got, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if got.DeleteRequestedAt == nil || !got.DeleteRequestedAt.Equal(requested) {
		t.Errorf("DeleteRequestedAt = %v, want %v", got.DeleteRequestedAt, requested)
	}
	if got.Phase != PhaseSuccess {
		t.Errorf("Phase = %q after recording a delete intent, want %q: the intent is not the delete", got.Phase, PhaseSuccess)
	}
	if !got.LastKnownGood {
		t.Error("recording a delete intent cleared last-known-good, want it left alone until the delete actually happens")
	}

	// Re-affirming the intent keeps the moment it first became durable:
	// that stamp is how long a delete has been outstanding, and moving it
	// forward on every retry destroys the evidence.
	if err := j.MarkSnapshotDeleteRequested(ctx, "run-1", requested.Add(time.Hour)); err != nil {
		t.Fatalf("second MarkSnapshotDeleteRequested: %v", err)
	}
	again, err := j.GetSnapshotRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSnapshotRun: %v", err)
	}
	if again.DeleteRequestedAt == nil || !again.DeleteRequestedAt.Equal(requested) {
		t.Errorf("DeleteRequestedAt = %v after a repeat call, want the original %v", again.DeleteRequestedAt, requested)
	}

	// A quarantined snapshot is one nothing can attribute, and this
	// product never deletes one.
	beginRun(t, j, testSnapshotRunRequest(t, "run-2", "idem-2", set, snapshotRunAt.Add(time.Hour)))
	if err := j.AdvanceSnapshotRun(ctx, "run-2", PhaseQuarantined, SnapshotRunUpdate{
		Reason: new("no configured backup set claims this snapshot"),
		At:     snapshotRunAt.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("quarantining run-2: %v", err)
	}
	if err := j.MarkSnapshotDeleteRequested(ctx, "run-2", snapshotRunAt.Add(3*time.Hour)); err == nil {
		t.Error("MarkSnapshotDeleteRequested on a QUARANTINED run = nil error, want a refusal")
	}

	if err := j.MarkSnapshotDeleteRequested(ctx, "nobody", snapshotRunAt); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Errorf("MarkSnapshotDeleteRequested on an unknown run: error = %v, want ErrSnapshotRunNotFound", err)
	}
}

// The listing is per set and bounded, for ListOperations' reason: this
// table is append-only and never pruned, so an unbounded read grows with
// the deployment's whole history.
func TestListSnapshotRuns_NewestFirstPerSetAndRefusesAnUnboundedRead(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	alpha := testSnapshotSet(t, "production", "postgres-primary")
	beta := testSnapshotSet(t, "staging", "uploads")

	for i := range 3 {
		id := "alpha-" + string(rune('1'+i))
		beginRun(t, j, testSnapshotRunRequest(t, id, "idem-"+id, alpha, snapshotRunAt.Add(time.Duration(i)*time.Hour)))
	}
	beginRun(t, j, testSnapshotRunRequest(t, "beta-1", "idem-beta-1", beta, snapshotRunAt.Add(9*time.Hour)))

	runs, err := j.ListSnapshotRuns(ctx, testSetUUID(alpha), 10)
	if err != nil {
		t.Fatalf("ListSnapshotRuns: %v", err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	want := []string{"alpha-3", "alpha-2", "alpha-1"}
	if len(ids) != len(want) {
		t.Fatalf("ListSnapshotRuns = %v, want %v: another set's runs must not appear", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ListSnapshotRuns = %v, want %v (newest first)", ids, want)
		}
	}

	limited, err := j.ListSnapshotRuns(ctx, testSetUUID(alpha), 2)
	if err != nil {
		t.Fatalf("ListSnapshotRuns(limit 2): %v", err)
	}
	if len(limited) != 2 || limited[0].RunID != "alpha-3" {
		t.Fatalf("ListSnapshotRuns(limit 2) = %+v, want the two newest", limited)
	}

	for _, limit := range []int{0, -1} {
		if _, err := j.ListSnapshotRuns(ctx, testSetUUID(alpha), limit); err == nil {
			t.Errorf("ListSnapshotRuns(limit %d) = nil error, want a refusal", limit)
		}
	}
}

// A run that cannot be identified, or that carries no time, would insert
// perfectly well and then be unrecoverable, so it is refused with the
// caller's vocabulary rather than left to a NOT NULL error to describe in
// the schema's.
func TestBeginSnapshotRun_RefusesARunNobodyCouldFindAgain(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	broken := map[string]func(*SnapshotRunRequest){
		"no run id":          func(r *SnapshotRunRequest) { r.RunID = "" },
		"no idempotency key": func(r *SnapshotRunRequest) { r.IdempotencyKey = "" },
		"no set":             func(r *SnapshotRunRequest) { r.Set = model.BackupSetID{} },
		"no set uuid":        func(r *SnapshotRunRequest) { r.SetUUID = "" },
		"no engine":          func(r *SnapshotRunRequest) { r.Engine = "" },
		"no domain":          func(r *SnapshotRunRequest) { r.Domain = "" },
		"no source identity": func(r *SnapshotRunRequest) { r.SourceIdentity = "" },
		"no start time":      func(r *SnapshotRunRequest) { r.StartedAt = time.Time{} },
	}
	for name, mutate := range broken {
		req := testSnapshotRunRequest(t, "run-x", "idem-x", set, snapshotRunAt)
		mutate(&req)
		if _, err := j.BeginSnapshotRun(ctx, req); err == nil {
			t.Errorf("BeginSnapshotRun with %s = nil error, want a refusal", name)
		}
	}
}

// AdvanceSnapshotRun's own refusals: a run nobody has, a phase nothing
// defines, an update with no time on it, and a verification status outside
// the vocabulary the column allows.
func TestAdvanceSnapshotRun_RefusesWhatItCannotRecord(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")
	beginRun(t, j, testSnapshotRunRequest(t, "run-1", "idem-1", set, snapshotRunAt))

	if err := j.AdvanceSnapshotRun(ctx, "nobody", PhaseSourceScan, SnapshotRunUpdate{At: snapshotRunAt}); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Errorf("advancing an unknown run: error = %v, want ErrSnapshotRunNotFound", err)
	}
	if err := j.AdvanceSnapshotRun(ctx, "run-1", SnapshotPhase("MADE_UP"), SnapshotRunUpdate{At: snapshotRunAt}); err == nil {
		t.Error("advancing to a phase nothing defines = nil error, want a refusal")
	}
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseSourceScan, SnapshotRunUpdate{}); err == nil {
		t.Error("advancing with no At = nil error, want a refusal")
	}
	if err := j.AdvanceSnapshotRun(ctx, "run-1", PhaseSourceScan, SnapshotRunUpdate{
		VerificationStatus: new("probably fine"),
		At:                 snapshotRunAt,
	}); err == nil {
		t.Error("advancing with a verification status outside the vocabulary = nil error, want a refusal")
	}
}

// The one deliberate exception to "only reaching SUCCESS moves the flag",
// and the reason it has to exist: when the newest successful run's
// snapshot turns out to be gone, that run goes to LOST and takes the flag
// off itself, and the set would then report NO restore point while a
// perfectly good older success sits in the same table. Reconciliation
// re-points at that older row, explicitly, in a call whose whole name says
// what it is doing.
//
// What it must refuse is the whole reason it can be trusted. A repoint at
// a run that never succeeded, or at a run whose snapshot is known to be
// gone, would advertise a restore point that does not exist, and a repoint
// across sets would offer one set's data as another's.
func TestRepointLastKnownGood_MovesTheFlagAndRefusesAnIneligibleRun(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")
	other := testSnapshotSet(t, "production", "uploads")

	succeed := func(runID string, started time.Time, id model.BackupSetID) {
		t.Helper()
		beginRun(t, j, testSnapshotRunRequest(t, runID, "idem-"+runID, id, started))
		advanceThrough(t, j, runID, started,
			PhaseSourceScan, PhaseSnapshotWrite, PhaseManifestCommitted, PhaseVerification, PhaseCatalogCommit, PhaseSuccess)
	}

	succeed("older", snapshotRunAt, set)
	succeed("newer", snapshotRunAt.Add(24*time.Hour), set)
	succeed("elsewhere", snapshotRunAt.Add(48*time.Hour), other)

	beginRun(t, j, testSnapshotRunRequest(t, "failed", "idem-failed", set, snapshotRunAt.Add(2*time.Hour)))
	if err := j.AdvanceSnapshotRun(ctx, "failed", PhaseFailed, SnapshotRunUpdate{
		Reason: new("the repository refused the write"),
		At:     snapshotRunAt.Add(3 * time.Hour),
	}); err != nil {
		t.Fatalf("failing a run: %v", err)
	}
	beginRun(t, j, testSnapshotRunRequest(t, "pending", "idem-pending", set, snapshotRunAt.Add(4*time.Hour)))

	succeed("lost", snapshotRunAt.Add(72*time.Hour), set)
	if err := j.AdvanceSnapshotRun(ctx, "lost", PhaseLost, SnapshotRunUpdate{
		Reason: new("the snapshot is no longer in the repository"),
		At:     snapshotRunAt.Add(73 * time.Hour),
	}); err != nil {
		t.Fatalf("losing a run: %v", err)
	}

	// The set is now exactly in the state this call exists for: its newest
	// success is lost and nothing holds the flag.
	if _, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set)); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Fatalf("LastKnownGoodSnapshot before the repoint: error = %v, want ErrSnapshotRunNotFound", err)
	}

	if err := j.RepointLastKnownGood(ctx, testSetUUID(set), "newer"); err != nil {
		t.Fatalf("RepointLastKnownGood: %v", err)
	}
	lkg, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set))
	if err != nil {
		t.Fatalf("LastKnownGoodSnapshot after the repoint: %v", err)
	}
	if lkg.RunID != "newer" {
		t.Fatalf("last-known-good = %q, want newer", lkg.RunID)
	}

	// And it moves rather than adds: repointing again at the older run
	// takes the flag off the newer one, which the partial unique index
	// would refuse outright if it did not.
	if err := j.RepointLastKnownGood(ctx, testSetUUID(set), "older"); err != nil {
		t.Fatalf("RepointLastKnownGood to the older run: %v", err)
	}
	moved, err := j.GetSnapshotRun(ctx, "newer")
	if err != nil {
		t.Fatalf("GetSnapshotRun(newer): %v", err)
	}
	if moved.LastKnownGood {
		t.Error("the previously flagged run is still flagged after a repoint at another run")
	}

	refusals := map[string]string{
		"a run that never succeeded":       "pending",
		"a run that failed":                "failed",
		"a run whose snapshot is gone":     "lost",
		"a run belonging to another set":   "elsewhere",
		"a run this journal has never had": "nobody",
	}
	for what, runID := range refusals {
		if err := j.RepointLastKnownGood(ctx, testSetUUID(set), runID); err == nil {
			t.Errorf("RepointLastKnownGood at %s = nil error, want a refusal", what)
		}
	}
	if err := j.RepointLastKnownGood(ctx, testSetUUID(set), "nobody"); !errors.Is(err, ErrSnapshotRunNotFound) {
		t.Errorf("RepointLastKnownGood at an unknown run: error = %v, want ErrSnapshotRunNotFound", err)
	}

	// Every one of those refusals left the flag exactly where it was, and
	// left the other set's own restore point alone.
	after, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(set))
	if err != nil || after.RunID != "older" {
		t.Errorf("after the refusals, last-known-good = %q (err %v), want older", after.RunID, err)
	}
	elsewhere, err := j.LastKnownGoodSnapshot(ctx, testSetUUID(other))
	if err != nil || elsewhere.RunID != "elsewhere" {
		t.Errorf("the other set's last-known-good = %q (err %v), want elsewhere", elsewhere.RunID, err)
	}
}

// The read behind "what did this operation actually do to the
// repository": a client polling an operation gets the snapshot runs that
// operation started.
//
// Two things about it are decisions rather than details. An operation with
// no snapshot runs is an empty slice and no error, because "that request
// did no snapshot work" is an ordinary answer about a request that exists,
// not a missing row, and an ErrSnapshotRunNotFound here would make every
// caller branch on a failure to render an empty list. And an empty
// operation id is refused, because operation_id is legitimately empty for
// every scheduled cycle: a query for "" would hand a caller every
// unattributed run in the deployment, which is the opposite of what
// asking about one operation means.
func TestSnapshotRunsByOperation_NewestFirstAndEmptyIsAnOrdinaryAnswer(t *testing.T) {
	j, _ := openJournal(t)
	ctx := context.Background()
	set := testSnapshotSet(t, "production", "postgres-primary")

	under := func(runID, operationID string, started time.Time) {
		t.Helper()
		req := testSnapshotRunRequest(t, runID, "idem-"+runID, set, started)
		req.OperationID = operationID
		beginRun(t, j, req)
	}

	under("a-first", "op-A", snapshotRunAt)
	under("a-second", "op-A", snapshotRunAt.Add(time.Hour))
	under("b-only", "op-B", snapshotRunAt.Add(2*time.Hour))
	under("scheduled", "", snapshotRunAt.Add(3*time.Hour))

	runs, err := j.SnapshotRunsByOperation(ctx, "op-A", 10)
	if err != nil {
		t.Fatalf("SnapshotRunsByOperation: %v", err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	want := []string{"a-second", "a-first"}
	if len(ids) != len(want) {
		t.Fatalf("SnapshotRunsByOperation(op-A) = %v, want %v: another operation's runs must not appear", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("SnapshotRunsByOperation(op-A) = %v, want %v (newest first)", ids, want)
		}
	}

	limited, err := j.SnapshotRunsByOperation(ctx, "op-A", 1)
	if err != nil {
		t.Fatalf("SnapshotRunsByOperation(limit 1): %v", err)
	}
	if len(limited) != 1 || limited[0].RunID != "a-second" {
		t.Fatalf("SnapshotRunsByOperation(limit 1) = %+v, want the newest run only", limited)
	}

	// An operation that started no snapshot work is answered, not refused.
	none, err := j.SnapshotRunsByOperation(ctx, "op-nothing", 10)
	if err != nil {
		t.Fatalf("SnapshotRunsByOperation for an operation with no runs: error = %v, want nil", err)
	}
	if len(none) != 0 {
		t.Errorf("SnapshotRunsByOperation for an operation with no runs = %+v, want an empty slice", none)
	}

	// And the scheduled run is unreachable this way rather than lumped in
	// with everything else that has no operation.
	if _, err := j.SnapshotRunsByOperation(ctx, "", 10); err == nil {
		t.Error("SnapshotRunsByOperation with an empty operation id = nil error, want a refusal")
	}
	for _, limit := range []int{0, -1} {
		if _, err := j.SnapshotRunsByOperation(ctx, "op-A", limit); err == nil {
			t.Errorf("SnapshotRunsByOperation(limit %d) = nil error, want a refusal", limit)
		}
	}
}
