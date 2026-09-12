package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// The durable catalog of snapshot RUNS for the incremental engine (EPIC K,
// issue #783): one row per run, an append-only log of the phase changes it
// made, and the reads a crash reconciler and a restore path need.
//
// This package still decides no backup policy. It does not know what a
// kopia snapshot is, when one should be taken, how long one should be
// kept, or what to do about a run that died halfway; it knows how to
// persist those facts so that the process which died and the process which
// starts next are looking at the same account of what happened. The
// division is the same one operations.go draws, and it is why the phase
// vocabulary below is spelled here rather than imported from the run
// driver: it is the vocabulary the SCHEMA enforces (0009_snapshot_runs.sql
// carries the matching CHECK), so it has to live where the writes are.
// What a phase means to a run driver is that driver's business.
//
// Three mechanisms carry the weight, and all three are the same mechanisms
// this file's neighbours already use.
//
// BeginSnapshotRun is idempotent on a caller-supplied key, in one
// transaction, exactly as CreateOperation is: a driver that crashed
// between committing the row and observing it must resolve to the run it
// already started rather than start a second snapshot of the same source.
//
// AdvanceSnapshotRun writes the phase, the facts the caller learned, the
// updated_at stamp and the log entry in one transaction, exactly as
// RecordTransition does, so there is no instant where the journal is half
// convinced. It moves a run forward only. The single exception is
// VERIFICATION -> MANIFEST_COMMITTED and it is argued at
// snapshotAdvanceAllowed, where somebody reading the rule will find it.
//
// And last-known-good is a FLAG ON A ROW, not a query. "The newest
// successful run" is what a derived answer would say, and deriving it is
// how this gets it wrong: the derivation lives above the durable record,
// so every caller re-implements the same set of exclusions and one of them
// eventually forgets that a newer run which failed, or whose snapshot has
// since disappeared, is not a restore point. Here the flag moves in
// exactly two places, both of them in this file: reaching SUCCESS takes it
// from whoever held it, and RepointLastKnownGood moves it deliberately
// when reconciliation has found that the holder's snapshot is gone. A run
// that reaches FAILED or QUARANTINED never touches it; a run that reaches
// LOST or DELETED clears it from ITSELF, because a restore point whose
// snapshot is no longer in the repository is not good and handing a caller
// a snapshot id that resolves to nothing is worse than saying there is
// none.

// SnapshotPhase is where a snapshot run is in its life.
//
// The nominal path is PENDING -> SOURCE_SCAN -> SNAPSHOT_WRITE ->
// MANIFEST_COMMITTED -> VERIFICATION -> CATALOG_COMMIT -> SUCCESS. The
// division that matters for recovery is at MANIFEST_COMMITTED: before it,
// a crash has left nothing durable in the repository and the run can
// simply be re-driven; after it, the snapshot exists whatever happens to
// this process, and a reconciler that treats the run as never having
// happened orphans a real snapshot.
//
// The remaining four are ends rather than steps, and they are four rather
// than one because they call for opposite responses. FAILED is a run that
// stopped. LOST is a run that succeeded and whose snapshot is no longer in
// the repository, which is a fact about the repository, not a failure of
// the run, and it is the one that costs a restore point. DELETED is a
// delete this product intended and completed. QUARANTINED is a repository
// snapshot nothing can attribute to any configured set: it is recorded so
// that it is accounted for and, deliberately, never deleted.
type SnapshotPhase string

const (
	PhasePending           SnapshotPhase = "PENDING"
	PhaseSourceScan        SnapshotPhase = "SOURCE_SCAN"
	PhaseSnapshotWrite     SnapshotPhase = "SNAPSHOT_WRITE"
	PhaseManifestCommitted SnapshotPhase = "MANIFEST_COMMITTED"
	PhaseVerification      SnapshotPhase = "VERIFICATION"
	PhaseCatalogCommit     SnapshotPhase = "CATALOG_COMMIT"
	PhaseSuccess           SnapshotPhase = "SUCCESS"
	PhaseFailed            SnapshotPhase = "FAILED"

	// PhaseLost is a run that succeeded once and whose snapshot is no
	// longer in the repository.
	PhaseLost SnapshotPhase = "LOST"

	// PhaseDeleted is a delete this product intended, completed.
	PhaseDeleted SnapshotPhase = "DELETED"

	// PhaseQuarantined is a repository snapshot nothing can attribute;
	// never deleted.
	PhaseQuarantined SnapshotPhase = "QUARANTINED"
)

// The verification statuses a run row can carry, which are not phases: a
// run can sit at PhaseVerification with nothing concluded yet, and a run
// at PhaseSuccess carries the conclusion of the verification it ran. Empty
// is the fourth value and the default, and it means nothing has been asked
// of this snapshot. 0009_snapshot_runs.sql constrains the column to
// exactly these.
const (
	SnapshotVerificationPending = "pending"
	SnapshotVerificationPassed  = "passed"
	SnapshotVerificationFailed  = "failed"
)

// snapshotNominalPath is the ordered spine of the machine, and its order is
// the definition of "forward": AdvanceSnapshotRun compares positions in
// this slice rather than consulting a table of legal pairs, so inserting a
// phase into the path is one edit here rather than an edit plus every pair
// somebody remembered to update.
var snapshotNominalPath = []SnapshotPhase{
	PhasePending,
	PhaseSourceScan,
	PhaseSnapshotWrite,
	PhaseManifestCommitted,
	PhaseVerification,
	PhaseCatalogCommit,
	PhaseSuccess,
}

// snapshotOffPathTerminals are the ends a run can reach without walking to
// the end of the nominal path.
var snapshotOffPathTerminals = []SnapshotPhase{
	PhaseFailed,
	PhaseLost,
	PhaseDeleted,
	PhaseQuarantined,
}

// SnapshotPhases returns every phase this build knows, the nominal path in
// order first and then the off-path ends.
//
// It returns a fresh slice per call rather than the package's own, so a
// caller that sorts or truncates what it gets back cannot quietly redefine
// the machine for everybody else. It is the list the schema's CHECK
// constraint mirrors, and the integration owner pins the two together.
func SnapshotPhases() []SnapshotPhase {
	all := make([]SnapshotPhase, 0, len(snapshotNominalPath)+len(snapshotOffPathTerminals))
	all = append(all, snapshotNominalPath...)
	all = append(all, snapshotOffPathTerminals...)
	return all
}

// ParseSnapshotPhase turns a stored or transmitted string into a phase, and
// refuses everything else including the empty string.
//
// It never defaults, and that is the whole point of having it. Every
// available default is a lie with consequences: reading an unrecognised
// phase as PENDING hands a crash reconciler a finished run to re-drive,
// and reading it as FAILED writes off a snapshot that is sitting in the
// repository. A value this build does not recognise means the row was
// written by a build that knew something this one does not, and the honest
// response is to say so and let the caller stop.
func ParseSnapshotPhase(s string) (SnapshotPhase, error) {
	for _, p := range SnapshotPhases() {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("state: %q is not a snapshot phase this build knows", s)
}

// Terminal reports whether the run itself is over, which is the question
// the crash reconciler's worklist asks: a non-terminal row is work that
// was in flight when some process died.
//
// It is not the same as "this row can never change again". SUCCESS -> LOST,
// SUCCESS -> DELETED and LOST -> DELETED are all legal afterwards, and none
// of them is resumed work: they are facts about what later happened to the
// SNAPSHOT a finished run produced. See snapshotAdvanceAllowed.
func (p SnapshotPhase) Terminal() bool {
	switch p {
	case PhaseSuccess, PhaseFailed, PhaseLost, PhaseDeleted, PhaseQuarantined:
		return true
	default:
		return false
	}
}

// Advertised reports whether this phase means the run's snapshot is a
// restore point a caller may offer.
//
// Only SUCCESS is, and the reason this is a method rather than a
// comparison at each call site is that the tempting version of the
// comparison is "not failed". A run at MANIFEST_COMMITTED has a real
// snapshot in the repository that nothing has verified; a run at LOST had
// one. Neither is something to offer anybody.
func (p SnapshotPhase) Advertised() bool { return p == PhaseSuccess }

// The refusals this file returns that a caller branches on. They are here
// rather than in errors.go with the older sentinels because the catalog is
// self-contained: these three are meaningless without the phase machine
// above them, and a reader working out why an advance was refused should
// find the sentence and the rule in one file. They are values for the
// reason errors.go gives for the rest: no caller needs structured data out
// of a refusal, only the ability to tell one refusal from another with
// errors.Is rather than by matching on text an operator reads.
var (
	// ErrSnapshotRunNotFound is returned by every read and every write
	// that names a run id, a set or a snapshot id with no row behind it.
	ErrSnapshotRunNotFound = errors.New("state: snapshot run not found")

	// ErrSnapshotRunIdempotencyKeyReused is BeginSnapshotRun's equivalent
	// of ErrOperationIdempotencyKeyReused: the key was presented for a run
	// of a different set, engine, domain or source. Serving the existing
	// run back would tell a caller "your snapshot is already in flight"
	// about a snapshot of something else, and the caller would then skip
	// taking the one it actually asked for.
	ErrSnapshotRunIdempotencyKeyReused = errors.New("state: idempotency key already used for a different snapshot run")

	// ErrSnapshotPhaseRegression is returned when an advance would move a
	// run backward along the nominal path, or out of a phase that admits
	// no further moves. Both are the same mistake from the journal's side
	// (a caller re-driving work whose result is already recorded) and both
	// would put a finished run back on the reconciler's worklist.
	ErrSnapshotPhaseRegression = errors.New("state: snapshot run cannot move to that phase")
)

// SnapshotRunRequest is everything BeginSnapshotRun needs to persist a run
// row. Everything in it is the run's IDENTITY and its configuration, never
// a result: what the run went on to do is written by AdvanceSnapshotRun.
type SnapshotRunRequest struct {
	// RunID is the caller's identifier for this attempt, unique across
	// this journal. IdempotencyKey is what makes a resubmitted attempt
	// resolve to the row that already exists. Both are required and they
	// are separate for the reason OperationRequest's two identifiers are.
	RunID          string
	IdempotencyKey string

	Set model.BackupSetID

	// OperationID is the durable operation row this run belongs to, empty
	// for a scheduled cycle no client asked for.
	OperationID string

	// Engine, Domain and SourceIdentity are model.BackupEngine,
	// model.RepositoryDomainID and model.SourceIdentity rendered as
	// strings. SourceIdentity is a digest, never a path: this table holds
	// no source file names (see 0009_snapshot_runs.sql).
	Engine         string
	Domain         string
	SourceIdentity string

	ConsistencyMode string

	// VerificationLevel is the level this run is CONFIGURED for, which is
	// what the operator asked for. What a verification actually PROVED is
	// SnapshotRunUpdate.VerificationLevelAchieved, and the two are never
	// the same column.
	VerificationLevel string

	StartedAt time.Time
}

// SnapshotRunOutcome reports what BeginSnapshotRun did. Created is true
// only if this call inserted the row, and a caller must only start doing
// the actual snapshot when it is: false means an earlier call owns this
// run, which is what makes a crashed-and-retried driver take one snapshot
// rather than two.
type SnapshotRunOutcome struct {
	Run     SnapshotRun
	Created bool
}

// SnapshotRunUpdate carries whatever a caller learned on the way into a
// phase. Every field is optional and nil means "leave it as it is", which
// is the distinction that makes an advance an UPDATE of what changed
// rather than a rewrite of the row: a caller that says nothing about the
// snapshot id must not clear the one already recorded.
//
// The counters are pointers for the reason every optional number in this
// package is (see types.go): a run can genuinely read zero bytes, so a
// zero must not double as "nobody measured this".
type SnapshotRunUpdate struct {
	// SnapshotID is the engine's opaque manifest id, normally written
	// exactly once, on the way into MANIFEST_COMMITTED.
	SnapshotID *string

	Files        *int64
	Directories  *int64
	LogicalBytes *int64

	SourceBytesRead        *int64
	RepositoryBytesWritten *int64
	ContentReusedBytes     *int64

	// VerificationStatus is "", "pending", "passed" or "failed"; anything
	// else is refused rather than stored, because the schema's CHECK would
	// otherwise refuse it in the column's words instead of the caller's.
	VerificationStatus *string

	// VerificationLevelAchieved is the level a verification actually
	// PROVED, which is not SnapshotRunRequest.VerificationLevel and must
	// never be written into it: a run asking for a full content
	// verification that only managed a structural one has to read as
	// exactly that.
	VerificationLevelAchieved *string

	// Reason is one sentence: why it failed, why it was quarantined, why
	// it is lost. It is operator prose and it is redacted on the way in,
	// like state_transitions.detail (issue #295). It is also what the
	// transition row this advance appends records as its detail.
	Reason *string

	// At is when this transition happened and is required. A durable fact
	// with no time on it cannot be reasoned about at all, and this one is
	// what a reconciler uses to decide how long a run has been stuck.
	At time.Time
}

// SnapshotRun is one row of the catalog, read back exactly as stored.
type SnapshotRun struct {
	RunID          string
	IdempotencyKey string

	Set         model.BackupSetID
	OperationID string

	Engine         string
	Domain         string
	SourceIdentity string

	ConsistencyMode string

	// VerificationLevel is what this run was configured for;
	// VerificationLevelAchieved is what was actually proven, empty when
	// nothing was. They are two claims and never one.
	VerificationLevel         string
	VerificationLevelAchieved string

	Phase SnapshotPhase

	// SnapshotID is the engine's opaque manifest id, empty until the
	// manifest is committed.
	SnapshotID string

	Files        *int64
	Directories  *int64
	LogicalBytes *int64

	SourceBytesRead        *int64
	RepositoryBytesWritten *int64
	ContentReusedBytes     *int64

	VerificationStatus string
	Reason             string

	// LastKnownGood marks the one run per set whose snapshot may be
	// offered as a restore point.
	LastKnownGood bool

	StartedAt time.Time
	UpdatedAt time.Time

	// CompletedAt is when the run came to rest, nil while it is in flight.
	CompletedAt *time.Time

	// DeleteRequestedAt is the durable intent to delete this snapshot,
	// recorded before the repository is asked to delete anything.
	DeleteRequestedAt *time.Time
}

// SnapshotRunTransition is one edge of the machine, as it actually
// happened. The run row is overwritten by every advance, so it can say
// what a run IS and never how it got there; a run verified twice because a
// crash interrupted the first attempt reads identically on the row to one
// verified once, and this log is what tells them apart.
type SnapshotRunTransition struct {
	RunID    string
	From, To SnapshotPhase
	At       time.Time
	Detail   string
}

// snapshotAdvanceAllowed is the whole transition policy, in one function so
// that changing the machine is one edit and reading it is one screen.
//
// The rule for the nominal path is position: strictly forward, never back.
// A backward move is a caller re-driving work whose result is already
// recorded, and applying it would put a finished run back on the
// reconciler's worklist and re-open a phase whose side effects already
// happened.
//
// # The one exception, and why it is exactly one
//
// VERIFICATION -> MANIFEST_COMMITTED is allowed, and it is what crash
// recovery needs. A run killed during verification has a committed
// manifest, which is durable: the snapshot is in the repository whatever
// happened to this process. The only honest ways to resume it are to
// verify it again, or to give up on a snapshot that exists. Re-entering at
// MANIFEST_COMMITTED is the first, and it is safe precisely because
// everything between those two phases is a read of something already
// durable. No other backward edge has that property: re-entering
// SNAPSHOT_WRITE would write a second snapshot for one run, and
// re-entering SOURCE_SCAN would rescan a source that has since changed and
// call the result the same run.
//
// # Why SUCCESS admits anything at all
//
// SUCCESS, LOST, DELETED, FAILED and QUARANTINED are all Terminal(), which
// says the RUN is over. Three edges remain afterwards and none of them is
// resumed work: SUCCESS -> LOST and SUCCESS -> DELETED and LOST -> DELETED
// record what later happened to the snapshot that run produced. A
// quarantined snapshot admits nothing further, deliberately: this product
// does not delete a snapshot it cannot attribute, so there is no edge out
// of QUARANTINED for a delete to travel along.
func snapshotAdvanceAllowed(from, to SnapshotPhase) bool {
	if from == PhaseVerification && to == PhaseManifestCommitted {
		return true
	}

	fromRank, fromOnPath := snapshotPathRank(from)
	toRank, toOnPath := snapshotPathRank(to)
	if fromOnPath && toOnPath {
		return toRank > fromRank
	}

	switch to {
	case PhaseFailed, PhaseQuarantined:
		// Any run that has not finished can stop, or turn out to be
		// unattributable. A run that already succeeded did neither.
		return fromOnPath && from != PhaseSuccess
	case PhaseLost:
		// Only a snapshot that was successfully written can go missing.
		return from == PhaseSuccess
	case PhaseDeleted:
		// The two states in which deleting a snapshot is something this
		// product intended: it is ours and it is there, or it is ours and
		// the repository has already lost it.
		return from == PhaseSuccess || from == PhaseLost
	default:
		return false
	}
}

// snapshotPathRank reports a phase's position along the nominal path.
func snapshotPathRank(p SnapshotPhase) (int, bool) {
	for i, candidate := range snapshotNominalPath {
		if candidate == p {
			return i, true
		}
	}
	return 0, false
}

// snapshotIDFor is the manifest id an advance is about: the one it is
// writing, or the one the row already carries. It exists so the refusal
// above can name the snapshot two runs are fighting over rather than the
// run that happened to lose.
func snapshotIDFor(upd SnapshotRunUpdate, current SnapshotRun) string {
	if upd.SnapshotID != nil {
		return *upd.SnapshotID
	}
	return current.SnapshotID
}

// validateSnapshotRunRequest refuses a run that could not be found again,
// before a transaction is opened for it.
//
// Every field it insists on is one that makes the row addressable or
// attributable later: without the two identifiers a run cannot be polled
// or replayed, without the set it belongs to nothing, and without engine,
// domain and source identity a reconciler walking a repository cannot
// decide whether a snapshot it finds is this run's. The schema would
// refuse most of this too, in the column's words rather than the caller's,
// which is the argument validateOperationRequest already makes.
func validateSnapshotRunRequest(req SnapshotRunRequest) error {
	switch {
	case req.RunID == "":
		return fmt.Errorf("state: snapshot run requires a non-empty RunID")
	case req.IdempotencyKey == "":
		return fmt.Errorf("state: snapshot run requires a non-empty IdempotencyKey")
	case req.Set.IsZero():
		return fmt.Errorf("state: snapshot run requires a backup set id")
	case req.Engine == "":
		return fmt.Errorf("state: snapshot run requires an Engine")
	case req.Domain == "":
		return fmt.Errorf("state: snapshot run requires a Domain")
	case req.SourceIdentity == "":
		return fmt.Errorf("state: snapshot run requires a SourceIdentity")
	case req.StartedAt.IsZero():
		return fmt.Errorf("state: snapshot run requires StartedAt")
	}
	return nil
}

// BeginSnapshotRun durably persists req as a new run at PhasePending, or
// recognises that req.IdempotencyKey was already used and returns THAT run
// unchanged with Created == false.
//
// The idempotency check and the insert happen in one transaction, for
// CreateOperation's reason: that is what makes two callers racing the same
// key resolve to exactly one row instead of a check-then-insert race
// producing two runs, which here would mean two snapshots of one source
// and a repository the operator has to reconcile by hand.
//
// A key already used for a run of a different set, engine, domain or
// source is ErrSnapshotRunIdempotencyKeyReused rather than a convenient
// replay. The convenient answer is the dangerous one: it tells a caller
// its snapshot is already in flight about a snapshot of something else, so
// the caller skips the one it asked for and the operator is missing a
// backup nobody reported failing.
func (j *Journal) BeginSnapshotRun(ctx context.Context, req SnapshotRunRequest) (SnapshotRunOutcome, error) {
	if err := validateSnapshotRunRequest(req); err != nil {
		return SnapshotRunOutcome{}, err
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return SnapshotRunOutcome{}, fmt.Errorf("state: begin snapshot run: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	existing, err := getSnapshotRunBy(ctx, tx, "idempotency_key = ?", req.IdempotencyKey)
	if err != nil && !errors.Is(err, ErrSnapshotRunNotFound) {
		return SnapshotRunOutcome{}, err
	}
	if err == nil {
		return commitSnapshotRunReplay(tx, req, existing)
	}

	conflict, err := insertSnapshotRun(ctx, tx, req)
	if err != nil {
		return SnapshotRunOutcome{}, err
	}
	if conflict {
		// Either this transaction's own idempotency check lost a race with
		// a writer on another connection (CreateOperation's doc explains
		// why that is only reachable across two *sql.DB handles on one
		// file, and why it is still handled), or the caller reused a run
		// id under a new key. The two are told apart by looking the key up
		// again: found means the race, and it replays exactly like the
		// sequential path above.
		raced, err := getSnapshotRunBy(ctx, tx, "idempotency_key = ?", req.IdempotencyKey)
		if errors.Is(err, ErrSnapshotRunNotFound) {
			return SnapshotRunOutcome{}, fmt.Errorf(
				"state: snapshot run id %q is already recorded under a different idempotency key", req.RunID)
		}
		if err != nil {
			return SnapshotRunOutcome{}, fmt.Errorf("state: re-fetch after snapshot run key race: %w", err)
		}
		return commitSnapshotRunReplay(tx, req, raced)
	}

	created, err := getSnapshotRunBy(ctx, tx, "run_id = ?", req.RunID)
	if err != nil {
		return SnapshotRunOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return SnapshotRunOutcome{}, fmt.Errorf("state: commit begin snapshot run: %w", err)
	}
	return SnapshotRunOutcome{Run: created, Created: true}, nil
}

// commitSnapshotRunReplay is BeginSnapshotRun's shared "this key was
// already used" path. The four fields it compares are the run's identity:
// two requests agreeing on all of them are the same logical piece of work
// whoever asked for it, and two that differ on any of them are not.
func commitSnapshotRunReplay(tx *sql.Tx, req SnapshotRunRequest, existing SnapshotRun) (SnapshotRunOutcome, error) {
	if existing.Set != req.Set ||
		existing.Engine != req.Engine ||
		existing.Domain != req.Domain ||
		existing.SourceIdentity != req.SourceIdentity {
		return SnapshotRunOutcome{}, fmt.Errorf("%w: key %q", ErrSnapshotRunIdempotencyKeyReused, req.IdempotencyKey)
	}
	if err := tx.Commit(); err != nil {
		return SnapshotRunOutcome{}, fmt.Errorf("state: commit snapshot run replay: %w", err)
	}
	return SnapshotRunOutcome{Run: existing, Created: false}, nil
}

// insertSnapshotRun performs the INSERT, reporting a UNIQUE violation as
// conflict rather than as an error: both unique columns here (run_id,
// idempotency_key) mean something a caller can act on, and the driver's
// raw constraint message names neither in terms the caller used.
func insertSnapshotRun(ctx context.Context, tx *sql.Tx, req SnapshotRunRequest) (conflict bool, err error) {
	started := formatTime(req.StartedAt)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO snapshot_runs (
			run_id, idempotency_key, source, backup_set, operation_id,
			engine, domain, source_identity, consistency_mode, verification_level,
			phase, started_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.RunID, req.IdempotencyKey, req.Set.Source, req.Set.Set, req.OperationID,
		req.Engine, req.Domain, req.SourceIdentity, req.ConsistencyMode, req.VerificationLevel,
		string(PhasePending), started, started,
	); err != nil {
		if isUniqueViolation(err) {
			return true, nil
		}
		return false, fmt.Errorf("state: insert snapshot run: %w", err)
	}
	return false, nil
}

// AdvanceSnapshotRun moves runID to phase to, applies whatever upd says
// changed, stamps updated_at and appends the transition, all in one
// transaction. A crash cannot leave the row saying one thing and the log
// another.
//
// Re-advancing to the phase a run is already in is an idempotent no-op on
// the machine: it applies upd's facts and stamps updated_at, because the
// caller did learn something and did make progress, and it appends NO
// transition row. That is one coherent rule rather than a special case:
// the log records EDGES, and a self-edge would fill the history of a run
// that retried a long phase with rows that say nothing about how it got
// anywhere, at exactly the moment somebody is reading that history to find
// out. A same-phase advance also leaves last_known_good and completed_at
// alone, which matters for the one case that looks harmless: replaying an
// advance to SUCCESS on an old run must not take the restore point back
// from a newer run that has succeeded since.
//
// A move backward along the nominal path, or out of a phase that admits no
// further moves, is ErrSnapshotPhaseRegression and changes nothing. See
// snapshotAdvanceAllowed for the rule and for the one documented
// exception, VERIFICATION -> MANIFEST_COMMITTED, which is how a
// verification interrupted by a crash is retried from the durable
// manifest.
//
// Reaching SUCCESS additionally stamps completed_at and moves this set's
// last-known-good flag onto this row, taking it off whichever row of the
// SAME set held it, in this same transaction. Reaching LOST or DELETED
// clears the flag from this row: its snapshot is not in the repository any
// more, and a restore point that resolves to nothing is worse than none.
// Reaching FAILED or QUARANTINED never touches the flag at all, which is
// the durable half of "a failed newer snapshot cannot replace
// last-known-good".
func (j *Journal) AdvanceSnapshotRun(ctx context.Context, runID string, to SnapshotPhase, upd SnapshotRunUpdate) error {
	if _, err := ParseSnapshotPhase(string(to)); err != nil {
		return err
	}
	if upd.At.IsZero() {
		return fmt.Errorf("state: advancing snapshot run %q to %s requires SnapshotRunUpdate.At", runID, to)
	}
	if upd.VerificationStatus != nil {
		switch *upd.VerificationStatus {
		case "", SnapshotVerificationPending, SnapshotVerificationPassed, SnapshotVerificationFailed:
		default:
			return fmt.Errorf("state: %q is not a verification status (want one of %q, %q, %q or empty)",
				*upd.VerificationStatus, SnapshotVerificationPending, SnapshotVerificationPassed, SnapshotVerificationFailed)
		}
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin advance snapshot run: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	current, err := getSnapshotRunBy(ctx, tx, "run_id = ?", runID)
	if err != nil {
		return err
	}

	from := current.Phase
	changingPhase := from != to
	if changingPhase && !snapshotAdvanceAllowed(from, to) {
		return fmt.Errorf("%w: run %s is at %s and cannot move to %s", ErrSnapshotPhaseRegression, runID, from, to)
	}

	redact := j.redact.Load()
	stamp := formatTime(upd.At)

	set := []string{"updated_at = ?"}
	args := []any{stamp}
	if changingPhase {
		set = append(set, "phase = ?")
		args = append(args, string(to))
	}
	if upd.SnapshotID != nil {
		set = append(set, "snapshot_id = ?")
		args = append(args, *upd.SnapshotID)
	}
	for _, counter := range []struct {
		column string
		value  *int64
	}{
		{"files", upd.Files},
		{"directories", upd.Directories},
		{"logical_bytes", upd.LogicalBytes},
		{"source_bytes_read", upd.SourceBytesRead},
		{"repository_bytes_written", upd.RepositoryBytesWritten},
		{"content_reused_bytes", upd.ContentReusedBytes},
	} {
		if counter.value != nil {
			set = append(set, counter.column+" = ?")
			args = append(args, *counter.value)
		}
	}
	if upd.VerificationStatus != nil {
		set = append(set, "verification_status = ?")
		args = append(args, *upd.VerificationStatus)
	}
	if upd.VerificationLevelAchieved != nil {
		set = append(set, "verification_level_achieved = ?")
		args = append(args, *upd.VerificationLevelAchieved)
	}

	// The caller's sentence goes through the same filter
	// state_transitions.detail does (issue #295): it is written by the
	// same kind of call site, often from an error a transport produced,
	// and a redactor installed for one durable text column that did not
	// cover the other would not be a fix.
	detail := ""
	if upd.Reason != nil {
		detail = redact.Filter(*upd.Reason)
		set = append(set, "reason = ?")
		args = append(args, detail)
	}

	if changingPhase && to.Terminal() {
		// COALESCE rather than assignment: completed_at is when the RUN
		// came to rest, and SUCCESS -> LOST or SUCCESS -> DELETED are
		// facts about the snapshot recorded afterwards. Moving the time
		// the run finished forward to when its snapshot went missing would
		// lose the only record of how long the good snapshot existed.
		set = append(set, "completed_at = COALESCE(completed_at, ?)")
		args = append(args, stamp)
	}

	switch {
	case changingPhase && to == PhaseSuccess:
		// Take the flag off whoever in this set holds it, then put it on
		// this row. Both statements are in this transaction and in this
		// order because the partial unique index refuses two holders, so
		// the clear is not tidying up: it is what makes the set legal.
		if _, err := tx.ExecContext(ctx,
			`UPDATE snapshot_runs SET last_known_good = 0, updated_at = ?
			  WHERE source = ? AND backup_set = ? AND last_known_good = 1 AND run_id <> ?`,
			stamp, current.Set.Source, current.Set.Set, runID,
		); err != nil {
			return fmt.Errorf("state: clear previous last-known-good: %w", err)
		}
		set = append(set, "last_known_good = 1")
	case changingPhase && (to == PhaseLost || to == PhaseDeleted):
		set = append(set, "last_known_good = 0")
	}

	args = append(args, runID)
	if _, err := tx.ExecContext(ctx,
		"UPDATE snapshot_runs SET "+strings.Join(set, ", ")+" WHERE run_id = ?", args...,
	); err != nil {
		// The only UNIQUE constraint an advance can violate is the partial
		// index on (domain, snapshot_id): this caller is recording a
		// manifest that another run already claims. That is a genuine
		// refusal with a cause a caller can act on (it has just attributed
		// one repository snapshot to two runs, so one of those attributions
		// is wrong), and the driver's own message names neither run.
		if isUniqueViolation(err) {
			return fmt.Errorf("state: snapshot %q in domain %q is already recorded by another run, so run %q cannot claim it",
				snapshotIDFor(upd, current), current.Domain, runID)
		}
		return fmt.Errorf("state: advance snapshot run %q: %w", runID, err)
	}

	if changingPhase {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO snapshot_run_transitions (run_id, from_phase, to_phase, occurred_at, detail)
			 VALUES (?, ?, ?, ?, ?)`,
			runID, string(from), string(to), stamp, detail,
		); err != nil {
			return fmt.Errorf("state: record snapshot run transition: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit advance snapshot run: %w", err)
	}
	return nil
}

// RepointLastKnownGood moves this set's last-known-good flag onto runID,
// taking it off every other row of the same set, in one transaction.
//
// It is the ONE way the flag moves other than a run reaching SUCCESS, and
// it exists because of a hole nothing else can fill. When the newest
// successful run's snapshot turns out to be gone, that run goes to LOST
// and takes the flag off itself; the set then reports no restore point at
// all while an older success, whose snapshot is still in the repository,
// sits right there in the table. Reconciliation, which is the only thing
// that has actually looked in the repository, says so here explicitly.
//
// It refuses rather than quietly does nothing, in three cases, and the
// refusals are why it can be trusted with a flag everything else is
// forbidden to touch. A run that is not at SUCCESS has no snapshot worth
// offering: a run at PENDING or FAILED never wrote one, and a run at LOST
// or DELETED had one and does not now. A run belonging to a different set
// would offer one set's data as another's, which is the exact failure
// model.BackupSetID exists to make impossible. And a run id with no row is
// ErrSnapshotRunNotFound, not a silent success.
//
// It does not append a transition row: no phase changed. What changed is
// which restore point this product offers, and the run's own log is not
// where that belongs.
func (j *Journal) RepointLastKnownGood(ctx context.Context, set model.BackupSetID, runID string) error {
	if set.IsZero() {
		return fmt.Errorf("state: repointing last-known-good requires a backup set id")
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin repoint last-known-good: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	run, err := getSnapshotRunBy(ctx, tx, "run_id = ?", runID)
	if err != nil {
		return err
	}
	if run.Set != set {
		return fmt.Errorf("state: run %s belongs to backup set %s, not %s, and cannot be its last-known-good",
			runID, run.Set, set)
	}
	if run.Phase != PhaseSuccess {
		return fmt.Errorf("state: run %s is at %s, and only a run at %s has a snapshot that may be offered as a restore point",
			runID, run.Phase, PhaseSuccess)
	}

	stamp := formatTime(now())
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshot_runs SET last_known_good = 0, updated_at = ?
		  WHERE source = ? AND backup_set = ? AND last_known_good = 1 AND run_id <> ?`,
		stamp, set.Source, set.Set, runID,
	); err != nil {
		return fmt.Errorf("state: clear previous last-known-good: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshot_runs SET last_known_good = 1, updated_at = ? WHERE run_id = ?`, stamp, runID,
	); err != nil {
		return fmt.Errorf("state: set last-known-good on run %q: %w", runID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit repoint last-known-good: %w", err)
	}
	return nil
}

// MarkSnapshotDeleteRequested records the durable INTENT to delete this
// run's snapshot, without changing its phase.
//
// It is written BEFORE the repository is asked to delete anything, which
// is what makes a crash in the middle decidable rather than a guess: a run
// with an intent whose snapshot is still in the repository is a delete to
// resume, and a run with no intent whose snapshot has gone is a loss to
// investigate. Those two demand opposite responses and without this column
// they look identical afterwards.
//
// It is not a phase, because the phase describes the run and the run has
// not changed: the delete has not happened yet. When it does, the run
// advances to DELETED.
//
// Only a run at SUCCESS or LOST is a run this product may intend to delete
// a snapshot for. Anything earlier has no snapshot to delete, DELETED has
// already been deleted, and QUARANTINED is deliberately never deleted at
// all: a snapshot nothing can attribute is exactly the snapshot this
// product must not remove.
//
// A repeat call keeps the original stamp. It is the moment the intent
// became durable, which is how long a delete has been outstanding, and
// rewriting it on every retry would destroy the only evidence that a
// delete has been failing for a week.
func (j *Journal) MarkSnapshotDeleteRequested(ctx context.Context, runID string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("state: recording a delete intent for run %q requires a time", runID)
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin mark snapshot delete requested: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	run, err := getSnapshotRunBy(ctx, tx, "run_id = ?", runID)
	if err != nil {
		return err
	}
	if run.Phase != PhaseSuccess && run.Phase != PhaseLost {
		return fmt.Errorf("state: run %s is at %s, and a snapshot delete is only something this product intends for a run at %s or %s",
			runID, run.Phase, PhaseSuccess, PhaseLost)
	}

	stamp := formatTime(at)
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshot_runs
		    SET delete_requested_at = COALESCE(delete_requested_at, ?), updated_at = ?
		  WHERE run_id = ?`,
		stamp, stamp, runID,
	); err != nil {
		return fmt.Errorf("state: mark snapshot delete requested for run %q: %w", runID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit mark snapshot delete requested: %w", err)
	}
	return nil
}

// snapshotRunColumns is spelled once because scanSnapshotRun decodes it by
// position, for selectColumns' reason: a read that listed its own columns
// and got two of them the wrong way round would not fail, it would put a
// domain into an engine on that one code path.
const snapshotRunColumns = `
	run_id, idempotency_key, source, backup_set, operation_id,
	engine, domain, source_identity, consistency_mode,
	verification_level, verification_level_achieved,
	phase, snapshot_id,
	files, directories, logical_bytes,
	source_bytes_read, repository_bytes_written, content_reused_bytes,
	verification_status, reason, last_known_good,
	started_at, updated_at, completed_at, delete_requested_at`

// getSnapshotRunBy is every single-row read of this table, taking a
// querier so the writes above can use it inside the transaction they
// already hold, where the row either exists or does not without a second
// writer changing the answer partway.
func getSnapshotRunBy(ctx context.Context, q querier, where string, args ...any) (SnapshotRun, error) {
	row := q.QueryRowContext(ctx,
		`SELECT`+snapshotRunColumns+` FROM snapshot_runs WHERE `+where, args...)
	run, err := scanSnapshotRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotRun{}, ErrSnapshotRunNotFound
	}
	return run, err
}

// scanSnapshotRun decodes one row of snapshotRunColumns.
//
// A stored phase this build does not recognise fails the read rather than
// becoming a zero phase, and a stored timestamp that will not parse fails
// it rather than becoming a zero time, for buildPlacement's reason: a zero
// here is not a missing answer, it is a confident wrong one, and every
// caller of this table decides something real on it.
func scanSnapshotRun(row scanRow) (SnapshotRun, error) {
	var (
		run                                   SnapshotRun
		source, backupSet                     string
		phase                                 string
		files, directories, logicalBytes      sql.NullInt64
		sourceRead, repoWritten, contentReuse sql.NullInt64
		lastKnownGood                         int64
		startedAt, updatedAt                  string
		completedAt, deleteRequestedAt        sql.NullString
	)

	if err := row.Scan(
		&run.RunID, &run.IdempotencyKey, &source, &backupSet, &run.OperationID,
		&run.Engine, &run.Domain, &run.SourceIdentity, &run.ConsistencyMode,
		&run.VerificationLevel, &run.VerificationLevelAchieved,
		&phase, &run.SnapshotID,
		&files, &directories, &logicalBytes,
		&sourceRead, &repoWritten, &contentReuse,
		&run.VerificationStatus, &run.Reason, &lastKnownGood,
		&startedAt, &updatedAt, &completedAt, &deleteRequestedAt,
	); err != nil {
		return SnapshotRun{}, err
	}

	parsed, err := ParseSnapshotPhase(phase)
	if err != nil {
		return SnapshotRun{}, fmt.Errorf("state: snapshot run %q: %w", run.RunID, err)
	}
	run.Phase = parsed
	run.Set = model.BackupSetID{Source: source, Set: backupSet}
	run.Files = nullableInt64(files)
	run.Directories = nullableInt64(directories)
	run.LogicalBytes = nullableInt64(logicalBytes)
	run.SourceBytesRead = nullableInt64(sourceRead)
	run.RepositoryBytesWritten = nullableInt64(repoWritten)
	run.ContentReusedBytes = nullableInt64(contentReuse)
	run.LastKnownGood = lastKnownGood != 0

	if run.StartedAt, err = parseTime(startedAt); err != nil {
		return SnapshotRun{}, fmt.Errorf("state: snapshot run %q started_at: %w", run.RunID, err)
	}
	if run.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return SnapshotRun{}, fmt.Errorf("state: snapshot run %q updated_at: %w", run.RunID, err)
	}
	if run.CompletedAt, err = nullableTime(completedAt); err != nil {
		return SnapshotRun{}, fmt.Errorf("state: snapshot run %q completed_at: %w", run.RunID, err)
	}
	if run.DeleteRequestedAt, err = nullableTime(deleteRequestedAt); err != nil {
		return SnapshotRun{}, fmt.Errorf("state: snapshot run %q delete_requested_at: %w", run.RunID, err)
	}
	return run, nil
}

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullableTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetSnapshotRun returns the current row for runID.
func (j *Journal) GetSnapshotRun(ctx context.Context, runID string) (SnapshotRun, error) {
	return getSnapshotRunBy(ctx, j.db, "run_id = ?", runID)
}

// LastKnownGoodSnapshot returns the one run of set whose snapshot may be
// offered as a restore point, or ErrSnapshotRunNotFound when the set has
// none.
//
// Not-found is an answer, not a failure, and it is deliberately not a zero
// SnapshotRun: a caller handed one of those would offer a restore point
// with an empty snapshot id. A set has none before its first success, and
// again after the flag holder's snapshot turned out to be gone and nothing
// has re-pointed it yet.
func (j *Journal) LastKnownGoodSnapshot(ctx context.Context, set model.BackupSetID) (SnapshotRun, error) {
	return getSnapshotRunBy(ctx, j.db,
		"source = ? AND backup_set = ? AND last_known_good = 1", set.Source, set.Set)
}

// SnapshotRunBySnapshotID answers "is this repository manifest one of ours"
// within a domain, and returns ErrSnapshotRunNotFound when it is not.
//
// The domain is part of the question rather than a filter for tidiness:
// two repositories can hand out the same opaque manifest id and they are
// not the same snapshot, so an unscoped lookup would attribute one
// repository's snapshot to a run against another.
//
// An empty snapshot id is refused outright. Every run carries one until
// its manifest is committed, so answering it would attribute an unrelated,
// possibly still-running, run to a snapshot.
func (j *Journal) SnapshotRunBySnapshotID(ctx context.Context, domain, snapshotID string) (SnapshotRun, error) {
	if snapshotID == "" {
		return SnapshotRun{}, fmt.Errorf("state: looking up a snapshot run requires a non-empty snapshot id")
	}
	if domain == "" {
		return SnapshotRun{}, fmt.Errorf("state: looking up snapshot %q requires the domain it is in", snapshotID)
	}
	return getSnapshotRunBy(ctx, j.db, "domain = ? AND snapshot_id = ?", domain, snapshotID)
}

// querySnapshotRuns is every multi-row read of this table.
func querySnapshotRuns(ctx context.Context, q querier, tail string, args ...any) ([]SnapshotRun, error) {
	rows, err := q.QueryContext(ctx, `SELECT`+snapshotRunColumns+` FROM snapshot_runs `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("state: query snapshot runs: %w", err)
	}
	defer rows.Close()

	var runs []SnapshotRun
	for rows.Next() {
		run, err := scanSnapshotRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: read snapshot runs: %w", err)
	}
	return runs, nil
}

// ListSnapshotRuns returns the most recent limit runs of one set, newest
// first.
//
// Ordered by started_at then rowid, for ListOperations' reason: started_at
// is what a reader means by "most recent", and the rowid tiebreak totally
// orders two runs begun inside the same clock tick, which the timestamp
// alone does not.
//
// A limit of zero or less is refused rather than read as "everything",
// also for ListOperations' reason: this table is append-only and never
// pruned, so an unbounded read grows with the deployment's whole history,
// and an hourly snapshot schedule fills it faster than anything else here.
func (j *Journal) ListSnapshotRuns(ctx context.Context, set model.BackupSetID, limit int) ([]SnapshotRun, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("state: listing snapshot runs requires a positive limit, got %d", limit)
	}
	if set.IsZero() {
		return nil, fmt.Errorf("state: listing snapshot runs requires a backup set id")
	}
	return querySnapshotRuns(ctx, j.db,
		`WHERE source = ? AND backup_set = ? ORDER BY started_at DESC, id DESC LIMIT ?`,
		set.Source, set.Set, limit)
}

// SnapshotRunsByOperation returns the snapshot runs one durable operation
// performed, newest first, ordered and bounded exactly as ListSnapshotRuns
// is and for the same reasons.
//
// It is the read behind the engine facts the API surfaces on an operation
// a client is already polling, so it answers from this table alone: the
// operations row the caller holds is what it is asking ABOUT, and joining
// it here would make a list of runs fail because an operation row was
// swept.
//
// An operation that did no snapshot work returns an empty slice and a nil
// error, deliberately, not ErrSnapshotRunNotFound. The operation exists;
// "it started no snapshot runs" is an ordinary fact about it, and a
// sentinel there would make every caller branch on a failure in order to
// render an empty list.
//
// An empty operation id is REFUSED, and that refusal is the one worth
// reading. operation_id is legitimately empty for every scheduled cycle,
// because nobody submitted those, so a query for "" would match every
// unattributed run in the deployment and hand them back as though one
// operation had performed them all. That is not a wider answer to the
// caller's question, it is a different question.
func (j *Journal) SnapshotRunsByOperation(ctx context.Context, operationID string, limit int) ([]SnapshotRun, error) {
	if operationID == "" {
		return nil, fmt.Errorf("state: listing an operation's snapshot runs requires an operation id: a scheduled cycle's runs carry none, and an empty id would match all of them")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("state: listing an operation's snapshot runs requires a positive limit, got %d", limit)
	}
	return querySnapshotRuns(ctx, j.db,
		`WHERE operation_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`,
		operationID, limit)
}

// UnfinishedSnapshotRuns returns every run that is not in a terminal
// phase, oldest first, across every set. It is the crash reconciler's
// worklist.
//
// It is unbounded, unlike ListSnapshotRuns, and that is not an oversight:
// this result is bounded by how many runs were in flight when a process
// died, not by history, and a bound here would silently hide exactly the
// rows a reconciler exists to find.
//
// The terminal phases are excluded by asking the phase vocabulary above
// rather than by a literal list in the SQL, so a phase added to the
// machine is excluded, or not, according to its own Terminal() and cannot
// disagree with it.
func (j *Journal) UnfinishedSnapshotRuns(ctx context.Context) ([]SnapshotRun, error) {
	var (
		placeholders []string
		args         []any
	)
	for _, p := range SnapshotPhases() {
		if p.Terminal() {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, string(p))
	}
	return querySnapshotRuns(ctx, j.db,
		`WHERE phase IN (`+strings.Join(placeholders, ", ")+`) ORDER BY started_at ASC, id ASC`, args...)
}

// SnapshotRunTransitions returns the run's phase changes in the order they
// happened.
//
// Ordered by rowid rather than by occurred_at: two edges recorded inside
// one clock tick have to come back in the order they were written, and it
// is the same ordering LastTransition uses over state_transitions for the
// same reason.
//
// A run with no transitions yet returns nothing and no error. That is the
// ordinary state of a run still at PENDING, not a missing row, which is
// why this does not check that the run exists first.
func (j *Journal) SnapshotRunTransitions(ctx context.Context, runID string) ([]SnapshotRunTransition, error) {
	rows, err := j.db.QueryContext(ctx,
		`SELECT run_id, from_phase, to_phase, occurred_at, detail
		   FROM snapshot_run_transitions WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: query snapshot run transitions: %w", err)
	}
	defer rows.Close()

	var out []SnapshotRunTransition
	for rows.Next() {
		var (
			t                  SnapshotRunTransition
			fromPhase, toPhase string
			occurredAt         string
		)
		if err := rows.Scan(&t.RunID, &fromPhase, &toPhase, &occurredAt, &t.Detail); err != nil {
			return nil, fmt.Errorf("state: scan snapshot run transition: %w", err)
		}
		// The log is evidence and is read back as written: a phase a later
		// build retired is still what happened, so these are not put
		// through ParseSnapshotPhase.
		t.From = SnapshotPhase(fromPhase)
		t.To = SnapshotPhase(toPhase)
		if t.At, err = parseTime(occurredAt); err != nil {
			return nil, fmt.Errorf("state: snapshot run transition occurred_at: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: read snapshot run transitions: %w", err)
	}
	return out, nil
}
