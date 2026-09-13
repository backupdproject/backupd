package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Holds: the rows that say a person decided one snapshot must not be
// deleted, and the rows that say another person later decided it may be
// (EPIC K, issue #785).
//
// This package still decides no retention policy. It does not know what a
// GFS tier is, which snapshot the calendar would drop tonight, or whether
// a hold is a good idea; it knows how to persist the decision so that the
// process which took it and the process which runs retention next week are
// looking at the same account of what was decided. 0011_snapshot_holds.sql
// carries the argument for the shape.
//
// Two things about the write path are worth reading before changing it.
//
// The lineage on a hold row is copied off the RUN row inside the same
// transaction, and is never taken from the caller. A hold whose set_uuid
// disagreed with its run's would be invisible to the retention pass for
// that set, which reads holds by lineage: the hold would exist, be
// listed, be believed, and protect nothing.
//
// And a hold is refused unless the run actually has a snapshot to protect.
// A run with no committed manifest has nothing to hold, and a run whose
// snapshot this product already deleted cannot be held back into
// existence. Both are accepted-and-useless in the shape that costs the
// most: somebody reads the hold list, sees a hold, and stops worrying.

// ErrSnapshotHoldNotFound is returned by every write that names a hold id
// with no row behind it. It is a value, like this catalog's other
// refusals, because no caller needs structure out of it -- only the
// ability to tell it from a hold that is there.
var ErrSnapshotHoldNotFound = errors.New("state: snapshot hold not found")

// ErrSnapshotHoldReleased is what replaying a hold id whose hold has since
// been RELEASED gets instead of that row.
//
// It is its own value because the two ways a replay can fail demand
// different responses: a hold id already recorded against a different run
// is a caller bug, and a hold id that is spent is a decision somebody else
// took -- the protection was ended on purpose, and a caller that wants it
// back places a new hold, with its own id, reason and author.
var ErrSnapshotHoldReleased = errors.New("state: snapshot hold has been released")

// SnapshotHold is one row of the hold table, read back exactly as stored.
type SnapshotHold struct {
	HoldID string
	RunID  string

	// SetUUID is the lineage the held run belongs to, as the run row
	// carried it when the hold was placed. See this file's preamble for
	// why it is not the caller's to supply.
	SetUUID string

	// Reason and PlacedBy are what make a hold releasable by somebody who
	// was not there. Both are required.
	Reason   string
	PlacedBy string

	PlacedAt time.Time

	// ReleasedAt is nil while the hold is active. A released hold stays in
	// the table: see 0011's "why releasing is a column and not a delete".
	ReleasedAt *time.Time
	ReleasedBy string
}

// Active reports whether this hold still protects its snapshot, which is
// the only question the retention pass asks of one.
//
// It is a method rather than a nil comparison at each call site because
// the comparison is easy to write the wrong way round, and the wrong way
// round means deleting a held snapshot.
func (h SnapshotHold) Active() bool { return h.ReleasedAt == nil }

// SnapshotHoldRequest is everything PlaceSnapshotHold needs. Every field
// is required, and the refusals say what each one costs when it is
// missing.
type SnapshotHoldRequest struct {
	// HoldID is the caller's identifier, unique across this journal, and
	// the thing that makes a re-placed hold resolve to the row that
	// already exists rather than stacking a second one.
	HoldID string

	// RunID is the run whose snapshot is held.
	RunID string

	// Reason is one sentence for the operator who finds this hold later
	// and has to decide whether it may be released. It is redacted on the
	// way in, like every other durable prose column here (issue #295).
	Reason string

	// PlacedBy is whoever placed it: an account, a service, a ticket. It
	// is not authorization and this package checks nothing about it; it
	// is the audit trail's other half.
	PlacedBy string

	// At is when the hold was placed and is required. A durable decision
	// with no time on it cannot be reasoned about, and how long a hold has
	// stood is exactly what somebody reviewing it needs.
	At time.Time
}

func validateSnapshotHoldRequest(req SnapshotHoldRequest) error {
	switch {
	case req.HoldID == "":
		return fmt.Errorf("state: placing a snapshot hold requires a hold id; without one the same hold cannot be found again to release it")
	case req.RunID == "":
		return fmt.Errorf("state: placing a snapshot hold requires the run whose snapshot is held")
	case req.Reason == "":
		return fmt.Errorf("state: placing a snapshot hold on run %q requires a reason; a hold nobody explained is one nobody dares release, which makes it permanent by accident", req.RunID)
	case req.PlacedBy == "":
		return fmt.Errorf("state: placing a snapshot hold on run %q requires who placed it; a hold with no author cannot be reviewed", req.RunID)
	case req.At.IsZero():
		return fmt.Errorf("state: placing a snapshot hold on run %q requires a time", req.RunID)
	}
	return nil
}

// PlaceSnapshotHold durably records that runID's snapshot must not be
// deleted, or recognises that req.HoldID was already used and returns THAT
// hold unchanged.
//
// The lookup and the insert happen in one transaction, for
// BeginSnapshotRun's reason: a check-then-insert race would put two rows
// over one decision, and the second one's placed_at would report a hold as
// younger than it is.
//
// A hold id presented for a DIFFERENT run is refused rather than replayed.
// The convenient answer is the dangerous one here too: it would tell a
// caller its snapshot is protected while the protection sits on somebody
// else's.
//
// It refuses every run a hold could not actually protect: one with no
// committed manifest, one whose snapshot this product already deleted, one
// whose delete intent is already durable (the manifest is going and the
// row still says SUCCESS for a few seconds longer), and one at LOST, whose
// manifest is not in the repository at all. Each of those would be
// accepted-and-useless in the shape that costs the most: somebody reads
// the hold list, sees a hold, and stops worrying.
func (j *Journal) PlaceSnapshotHold(ctx context.Context, req SnapshotHoldRequest) (SnapshotHold, error) {
	if err := validateSnapshotHoldRequest(req); err != nil {
		return SnapshotHold{}, err
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return SnapshotHold{}, fmt.Errorf("state: begin place snapshot hold: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	existing, err := getSnapshotHoldBy(ctx, tx, "hold_id = ?", req.HoldID)
	switch {
	case err != nil && !errors.Is(err, ErrSnapshotHoldNotFound):
		return SnapshotHold{}, err
	case err == nil && existing.RunID != req.RunID:
		return SnapshotHold{}, fmt.Errorf(
			"state: hold %q is already recorded against run %s, so it cannot also answer for run %s",
			req.HoldID, existing.RunID, req.RunID)
	case err == nil && existing.ReleasedAt != nil:
		// Somebody ended this protection on purpose. Handing the row
		// back with a nil error would read as "your hold is in place"
		// to every caller that checks the error rather than Active().
		return SnapshotHold{}, fmt.Errorf(
			"%w: hold %q on run %s was released at %s by %s; a snapshot that needs protecting again needs a new hold, with its own reason and author",
			ErrSnapshotHoldReleased, req.HoldID, existing.RunID,
			existing.ReleasedAt.UTC().Format(time.RFC3339), existing.ReleasedBy)
	case err == nil:
		// A replay of the caller's own hold. Nothing is rewritten:
		// placed_at is how long this hold has stood, and a retry that
		// moved it would destroy the one fact a review reads.
		if err := tx.Commit(); err != nil {
			return SnapshotHold{}, fmt.Errorf("state: commit snapshot hold replay: %w", err)
		}
		return existing, nil
	}

	run, err := getSnapshotRunBy(ctx, tx, "run_id = ?", req.RunID)
	if err != nil {
		return SnapshotHold{}, err
	}
	if run.SnapshotID == "" {
		return SnapshotHold{}, fmt.Errorf(
			"state: run %s is at %s and has committed no manifest, so there is no snapshot for a hold to protect",
			run.RunID, run.Phase)
	}
	if run.Phase == PhaseDeleted {
		return SnapshotHold{}, fmt.Errorf(
			"state: run %s is at %s: its snapshot has already been removed from the repository and a hold cannot bring one back",
			run.RunID, run.Phase)
	}
	if run.Phase == PhaseLost {
		return SnapshotHold{}, fmt.Errorf(
			"state: run %s is at %s: its snapshot is not in the repository, so a hold on it would protect nothing; "+
				"reconciliation records this state when a manifest has gone without a delete ever being recorded",
			run.RunID, run.Phase)
	}
	if run.DeleteRequestedAt != nil {
		return SnapshotHold{}, fmt.Errorf(
			"state: run %s already carries a durable delete intent, recorded at %s: its manifest is being removed and this row will say %s shortly, "+
				"so a hold accepted now would protect nothing",
			run.RunID, run.DeleteRequestedAt.UTC().Format(time.RFC3339), PhaseDeleted)
	}

	reason := j.redact.Load().Filter(req.Reason)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO snapshot_holds (hold_id, run_id, set_uuid, reason, placed_by, placed_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		req.HoldID, req.RunID, run.SetUUID, reason, req.PlacedBy, formatTime(req.At),
	); err != nil {
		return SnapshotHold{}, fmt.Errorf("state: place snapshot hold %q: %w", req.HoldID, err)
	}

	placed, err := getSnapshotHoldBy(ctx, tx, "hold_id = ?", req.HoldID)
	if err != nil {
		return SnapshotHold{}, err
	}
	if err := tx.Commit(); err != nil {
		return SnapshotHold{}, fmt.Errorf("state: commit place snapshot hold: %w", err)
	}
	return placed, nil
}

// ReleaseSnapshotHold records that this hold no longer protects its
// snapshot, and who said so.
//
// A repeat release keeps the original instant and the original releaser,
// exactly as MarkSnapshotDeleteRequested keeps its first stamp: the moment
// the protection ended is the fact this column carries, and a retry that
// rewrote it would move the timestamp an investigation reads.
//
// Releasing does not delete anything, in either sense. The row stays, and
// the snapshot stays: what changes is that the next retention pass is free
// to decide about it on the policy's own terms.
func (j *Journal) ReleaseSnapshotHold(ctx context.Context, holdID string, at time.Time, by string) error {
	switch {
	case holdID == "":
		return fmt.Errorf("state: releasing a snapshot hold requires the hold id")
	case at.IsZero():
		return fmt.Errorf("state: releasing snapshot hold %q requires a time", holdID)
	case by == "":
		return fmt.Errorf("state: releasing snapshot hold %q requires who released it; that is the first thing asked after a deletion somebody disputes", holdID)
	}

	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin release snapshot hold: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	if _, err := getSnapshotHoldBy(ctx, tx, "hold_id = ?", holdID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshot_holds
		    SET released_at = COALESCE(released_at, ?),
		        released_by = CASE WHEN released_at IS NULL THEN ? ELSE released_by END
		  WHERE hold_id = ?`,
		formatTime(at), by, holdID,
	); err != nil {
		return fmt.Errorf("state: release snapshot hold %q: %w", holdID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: commit release snapshot hold: %w", err)
	}
	return nil
}

// snapshotHoldColumns is spelled once because scanSnapshotHold decodes it
// by position, for snapshotRunColumns' reason.
const snapshotHoldColumns = `
	hold_id, run_id, set_uuid, reason, placed_by, placed_at, released_at, released_by`

func getSnapshotHoldBy(ctx context.Context, q querier, where string, args ...any) (SnapshotHold, error) {
	row := q.QueryRowContext(ctx, `SELECT`+snapshotHoldColumns+` FROM snapshot_holds WHERE `+where, args...)
	hold, err := scanSnapshotHold(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotHold{}, ErrSnapshotHoldNotFound
	}
	return hold, err
}

func scanSnapshotHold(row scanRow) (SnapshotHold, error) {
	var (
		hold       SnapshotHold
		placedAt   string
		releasedAt sql.NullString
	)
	if err := row.Scan(
		&hold.HoldID, &hold.RunID, &hold.SetUUID, &hold.Reason, &hold.PlacedBy,
		&placedAt, &releasedAt, &hold.ReleasedBy,
	); err != nil {
		return SnapshotHold{}, err
	}

	// A stored timestamp that will not parse fails the read rather than
	// becoming a zero time, for scanSnapshotRun's reason: a zero here is
	// not a missing answer, it is a confident wrong one, and this one
	// decides whether a snapshot may be deleted.
	parsed, err := parseTime(placedAt)
	if err != nil {
		return SnapshotHold{}, fmt.Errorf("state: snapshot hold %q has an unreadable placed_at %q: %w", hold.HoldID, placedAt, err)
	}
	hold.PlacedAt = parsed

	released, err := nullableTime(releasedAt)
	if err != nil {
		return SnapshotHold{}, fmt.Errorf("state: snapshot hold %q has an unreadable released_at: %w", hold.HoldID, err)
	}
	hold.ReleasedAt = released

	return hold, nil
}

func querySnapshotHolds(ctx context.Context, q querier, tail string, args ...any) ([]SnapshotHold, error) {
	rows, err := q.QueryContext(ctx, `SELECT`+snapshotHoldColumns+` FROM snapshot_holds `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("state: query snapshot holds: %w", err)
	}
	defer rows.Close()

	var holds []SnapshotHold
	for rows.Next() {
		hold, err := scanSnapshotHold(rows)
		if err != nil {
			return nil, err
		}
		holds = append(holds, hold)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: read snapshot holds: %w", err)
	}
	return holds, nil
}

// ActiveSnapshotHolds is every unreleased hold in one backup set LINEAGE,
// oldest first. It is the read a retention pass performs, once, before it
// decides anything.
//
// Unbounded, deliberately, and it is the one read in this catalog where
// that is not a risk worth bounding: the population is holds a person
// placed and nobody has released, which is small in every deployment that
// is working and is precisely the list somebody needs in full in one that
// is not. A limit here would silently drop a hold, and the consequence of
// dropping a hold is deleting the snapshot it was protecting.
//
// Keyed on the durable uuid rather than the set's names, with every other
// per-set read in this catalog: a renamed set must not lose its holds.
func (j *Journal) ActiveSnapshotHolds(ctx context.Context, setUUID string) ([]SnapshotHold, error) {
	if setUUID == "" {
		return nil, fmt.Errorf("state: listing active snapshot holds requires the backup set's durable uuid")
	}
	return querySnapshotHolds(ctx, j.db,
		`WHERE set_uuid = ? AND released_at IS NULL ORDER BY placed_at, id`, setUUID)
}

// SnapshotHolds is one run's whole hold history, released holds included,
// in the order the holds were placed. It is the audit read: what was
// decided about this snapshot, by whom, and when it stopped applying.
func (j *Journal) SnapshotHolds(ctx context.Context, runID string) ([]SnapshotHold, error) {
	if runID == "" {
		return nil, fmt.Errorf("state: listing snapshot holds requires a run id")
	}
	return querySnapshotHolds(ctx, j.db, `WHERE run_id = ? ORDER BY id`, runID)
}
