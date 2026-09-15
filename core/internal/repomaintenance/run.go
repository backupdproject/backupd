package repomaintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
)

// ErrNotOwner is returned when this instance is asked to maintain a
// repository another instance owns.
//
// It is a refusal and never a takeover. Two instances rewriting one
// repository's indexes is the failure the ownership record exists to
// prevent, and "the other owner looks inactive" is not evidence available
// from here: a record is a local file, an instance that has not written
// to it for a week may be mid-restore against the same bucket right now,
// and the safe reading of a claim somebody else made is that they meant
// it. Moving it is Transfer, which is somebody deciding.
var ErrNotOwner = errors.New("repomaintenance: another instance owns maintenance for this repository")

// ErrOwnershipMoved is returned by Transfer when the record does not name
// the owner the transfer said it did.
//
// The compare-and-set is the whole safety of an administrative handover:
// it turns "make B the owner" into "make B the owner IF A still is",
// which is the difference between a handover and two owners.
var ErrOwnershipMoved = errors.New("repomaintenance: this repository's maintenance owner is not the one the transfer expected")

// DefaultHistoryLimit is how many past windows the record keeps when the
// caller names no bound. Enough to see a pattern -- a week of hourly
// quick maintenance is 168, and what an operator reads is the last few
// plus the counters -- without turning a small state file into a log.
const DefaultHistoryLimit = 32

// Repository is everything maintenance may ask of an open repository, and
// its narrowness is load-bearing rather than tidy.
//
// Two methods. Ask the engine to maintain itself, and read what the
// repository costs. There is no DeleteSnapshot, no snapshot listing and
// nothing that names a manifest, because reclamation must never be able
// to decide what stops being kept -- that is internal/snapshotretention,
// working from the catalog, with holds and last-known-good protection it
// can see and this package cannot. boundary_test.go pins the method set
// so that widening it fails a test rather than a review.
//
// It is satisfied by backupengine.Repository.
type Repository interface {
	// Maintain performs repository housekeeping, including reclaiming
	// the space behind manifests something else deleted.
	Maintain(ctx context.Context, mode backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error)

	// Stats reports what the repository physically occupies. It is read
	// either side of a full maintenance, and only a full one; see
	// Runner.Run.
	Stats(ctx context.Context) (backupengine.RepositoryStats, error)
}

// Result is what one maintenance window did.
type Result struct {
	Domain model.RepositoryDomainID
	Mode   backupengine.MaintenanceMode

	// Attempted is whether the repository was asked to do anything at
	// all, which RunDue answers false for on a repository whose next
	// window has not come round.
	//
	// It is distinct from Ran, which is the repository's own answer to
	// "did I do any work", and the two are different facts: an attempted
	// window that declined because the engine had nothing due is normal,
	// and reporting it as "maintenance ran" would make every cycle look
	// like a maintenance window.
	Attempted bool
	Ran       bool

	// Reason is why this window ran or did not, in the words Due chose.
	Reason string

	StartedAt time.Time
	Duration  time.Duration

	// Waited is how long the fence held this window back, which is the
	// number that says whether a maintenance schedule is contending with
	// this product's own retention passes.
	Waited time.Duration

	// PhysicalBytesBefore and PhysicalBytesAfter are the storage's own
	// accounting either side of a FULL maintenance, and are zero for
	// every other outcome, including a quick one -- see Run for why a
	// quick window does not pay for a blob listing.
	PhysicalBytesBefore int64
	PhysicalBytesAfter  int64

	// Reclaimed is Before minus After. Negative is a real answer; see
	// backupengine.MaintenanceOutcome.Reclaimed.
	Reclaimed int64

	// Record is the durable record as it stands after this window,
	// including this window's own history entry.
	Record backupengine.MaintenanceOwnership
}

// Runner is one instance's maintenance of the repositories it owns.
//
// It holds no clock: every entry point takes the instant, for the reason
// schedule.go gives. It holds no repository either -- the caller opens
// one and passes it in -- because a repository handle is expensive, the
// caller already has one open for the backup it just took, and a package
// that opened its own would be a second connection to the same storage
// with its own cache.
type Runner struct {
	// Owner is who this instance is. It must be stable across restarts
	// (see backupengine.MaintenanceOwner); an instance whose name changes
	// every start looks like a new owner and will refuse to maintain its
	// own repositories.
	Owner backupengine.MaintenanceOwner

	// Store is where the ownership records live.
	Store backupengine.MaintenanceOwnershipStore

	// Fence is what keeps a full maintenance from interleaving with a
	// destructive snapshot operation on the same repository. A Runner
	// without one refuses to run rather than running unfenced.
	Fence *Fence

	// Intervals is this deployment's cadence; the zero value means
	// DefaultIntervals.
	Intervals Intervals

	// HistoryLimit bounds the record's history; zero means
	// DefaultHistoryLimit.
	HistoryLimit int
}

// Claim reads this repository's ownership record, creating one for this
// instance if the repository has never been claimed.
//
// It is the one place an owner is written without somebody asking for it,
// and it is safe for the reason the record's absence is unambiguous:
// nobody has ever maintained this repository, so there is no other owner
// to displace. Everything else is a refusal.
//
// The create is atomic, and that is not a detail. "Load said nobody owns
// it, so save myself as owner" is two operations, and two instances
// running them at the same time both read no record and both write
// themselves in -- which is precisely the two-owners state this package
// exists to prevent, reached through the code that exists to prevent it.
// backupengine.ErrMaintenanceOwnershipExists is how the loser finds out,
// and a loser is not a special case: it re-reads and treats the winner's
// claim exactly like any other instance's.
func (r Runner) Claim(ctx context.Context, domain model.RepositoryDomainID) (backupengine.MaintenanceOwnership, error) {
	if r.Owner.IsZero() {
		return backupengine.MaintenanceOwnership{}, errors.New("repomaintenance: this instance has no owner name, so it cannot claim maintenance for anything")
	}

	if r.Store == nil {
		return backupengine.MaintenanceOwnership{}, errors.New("repomaintenance: no ownership store, so a claim could not be recorded")
	}

	record, err := r.Store.Load(ctx, domain)

	switch {
	case err == nil:
		return r.mine(record, domain)

	case errors.Is(err, backupengine.ErrNoMaintenanceOwnership):
		created, err := r.Store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: r.Owner})

		switch {
		case err == nil:
			return created, nil

		case errors.Is(err, backupengine.ErrMaintenanceOwnershipExists):
			// Somebody claimed it between the read and the create. Whose
			// claim it is now is a question only the record can answer.
			record, err := r.Store.Load(ctx, domain)
			if err != nil {
				return backupengine.MaintenanceOwnership{}, err
			}

			return r.mine(record, domain)

		default:
			return backupengine.MaintenanceOwnership{}, fmt.Errorf("repomaintenance: claiming maintenance for %s: %w", domain, err)
		}

	default:
		// An unreadable record is never an unclaimed one. See the store's
		// own Load.
		return backupengine.MaintenanceOwnership{}, err
	}
}

// mine returns the record if this instance owns it, and refuses it
// otherwise.
func (r Runner) mine(record backupengine.MaintenanceOwnership, domain model.RepositoryDomainID) (backupengine.MaintenanceOwnership, error) {
	if record.Owner != r.Owner {
		return backupengine.MaintenanceOwnership{}, fmt.Errorf("%w: %s owns %s, this instance is %s",
			ErrNotOwner, record.Owner, domain, r.Owner)
	}

	return record, nil
}

// Transfer moves maintenance ownership of one repository from one
// instance to another, and is the ONLY way a claim changes hands.
//
// It is a package-level function taking a store rather than a method on
// Runner, and that is the design: an instance does not take ownership,
// an administrator moves it. A method on the thing that wants the
// repository would read as "claim this for me", which is the operation
// this package exists to make impossible.
//
// Everything except the owner survives the move, because the record
// describes the REPOSITORY and not its owner's opinion of it: losing when
// maintenance last ran would have the new owner immediately run a full
// cycle the repository does not need.
func Transfer(
	ctx context.Context,
	store backupengine.MaintenanceOwnershipStore,
	domain model.RepositoryDomainID,
	from, to backupengine.MaintenanceOwner,
) (backupengine.MaintenanceOwnership, error) {
	if store == nil {
		return backupengine.MaintenanceOwnership{}, errors.New("repomaintenance: no ownership store, so ownership cannot be moved")
	}

	if to.IsZero() {
		return backupengine.MaintenanceOwnership{}, errors.New("repomaintenance: a transfer must name the instance taking ownership")
	}

	record, err := store.Load(ctx, domain)
	if err != nil {
		// Including ErrNoMaintenanceOwnership: there is nothing to move,
		// and inventing a claim here would let a mistyped administrative
		// command create an owner for a repository nobody has opened.
		return backupengine.MaintenanceOwnership{}, err
	}

	if record.Owner != from {
		return backupengine.MaintenanceOwnership{}, fmt.Errorf("%w: %s owns %s, not %s", ErrOwnershipMoved, record.Owner, domain, from)
	}

	if from == to {
		return record, nil
	}

	record.Owner = to

	// The new owner starts eligible: a handover is usually somebody
	// moving maintenance to the instance that can actually reach the
	// storage, and making them wait out the previous owner's backoff
	// would be this package deciding the administrator was wrong.
	record.NextEligible = time.Time{}

	// CompareAndSwap and not a write, because the owner check above is
	// only worth anything if nothing can change between it and the
	// write. Two administrators handing the same repository to two
	// different instances at the same moment both pass the check; the
	// revision is what makes exactly one of them the handover and the
	// other a refusal.
	moved, err := store.CompareAndSwap(ctx, record)

	switch {
	case err == nil:
		return moved, nil

	case errors.Is(err, backupengine.ErrMaintenanceOwnershipStale):
		return backupengine.MaintenanceOwnership{}, fmt.Errorf(
			"%w: the record for %s changed while this transfer from %s to %s was being written: %w",
			ErrOwnershipMoved, domain, from, to, err)

	default:
		return backupengine.MaintenanceOwnership{}, fmt.Errorf("repomaintenance: recording the transfer of %s to %s: %w", domain, to, err)
	}
}

// RunDue runs whatever maintenance this repository is due for, or nothing.
//
// It is the entry point a scheduled pass calls: it decides from the
// record it already had to read to claim ownership, and returns a Result
// that says what it decided either way.
//
// Deciding and running are one critical section, held by
// Fence.SerialiseMaintenance for the whole of it. Two passes that both
// read "a full maintenance is due" would otherwise both queue for the
// exclusive fence, run full maintenance one after the other -- the second
// one against a repository that was fully maintained a second ago -- and
// write their outcomes over each other's history and counters. The
// recheck after the lease is granted is what makes the second one decide
// again rather than act on a decision that has since been carried out.
func (r Runner) RunDue(ctx context.Context, domain model.RepositoryDomainID, repo Repository, now time.Time) (Result, error) {
	result := Result{Domain: domain}

	if repo == nil {
		return result, errors.New("repomaintenance: no repository to maintain")
	}

	if r.Fence == nil {
		return result, errors.New("repomaintenance: no fence, and maintenance must never run unfenced")
	}

	done, err := r.Fence.SerialiseMaintenance(ctx, domain)
	if err != nil {
		return result, fmt.Errorf("repomaintenance: waiting to maintain %s: %w", domain, err)
	}
	defer done()

	record, err := r.Claim(ctx, domain)
	if err != nil {
		return result, err
	}

	decision := Due(record, r.Intervals, now)
	if !decision.Due {
		return Result{Domain: domain, Reason: decision.Reason, Record: record}, nil
	}

	held, err := r.window(ctx, domain, repo, decision.Mode, now, record)
	if held.Reason == "" {
		held.Reason = decision.Reason
	}

	return held, err
}

// Run performs one maintenance window against an open repository.
//
// The order is not negotiable and is the whole of this function:
//
//  1. The mode, then the maintenance lease, then ownership, before
//     anything else. A mode this boundary does not define never reaches a
//     repository and never writes a claim; a repository this instance
//     does not own is not touched at all, not even to read its size; and
//     the ownership record is read inside the lease, so what this window
//     writes back is what it read.
//  2. The fence. Full maintenance takes the exclusive side, so no
//     snapshot delete can be in flight while content is being reclaimed;
//     quick maintenance takes the shared side, so it cannot run during a
//     full one but does not block a retention pass.
//  3. The measurement, for a full window only. Stats walks the storage's
//     blob listing, which on a bucket is a real number of requests, and
//     quick maintenance does not reclaim blobs -- paying for two listings
//     an hour to report a number that is always zero is a cost with no
//     reader.
//  4. The engine's own maintenance, at the engine's own safety margins.
//     Nothing here can weaken them; see the package doc.
//  5. The record, whatever happened, and written back only if nothing
//     else has written it since. A failure is recorded exactly like a
//     success, because the next pass has to be able to tell "it failed"
//     from "it never ran", and internal/snapshotlifecycle's reconciler
//     reads that distinction (maintenanceVerdict).
func (r Runner) Run(
	ctx context.Context,
	domain model.RepositoryDomainID,
	repo Repository,
	mode backupengine.MaintenanceMode,
	now time.Time,
) (Result, error) {
	result := Result{Domain: domain, Mode: mode, StartedAt: now}

	if repo == nil {
		return result, errors.New("repomaintenance: no repository to maintain")
	}

	if r.Fence == nil {
		return result, errors.New("repomaintenance: no fence, and maintenance must never run unfenced")
	}

	if _, err := exclusiveFor(mode); err != nil {
		return result, err
	}

	done, err := r.Fence.SerialiseMaintenance(ctx, domain)
	if err != nil {
		return result, fmt.Errorf("repomaintenance: waiting to maintain %s: %w", domain, err)
	}
	defer done()

	record, err := r.Claim(ctx, domain)
	if err != nil {
		return result, err
	}

	return r.window(ctx, domain, repo, mode, now, record)
}

// window is one maintenance window, with the domain's maintenance lease
// already held and its ownership record already read under that lease.
//
// It is split out so that RunDue's decision and the window it decided on
// are one critical section rather than two, and so that the record this
// window writes back is the one it was decided from.
func (r Runner) window(
	ctx context.Context,
	domain model.RepositoryDomainID,
	repo Repository,
	mode backupengine.MaintenanceMode,
	now time.Time,
	record backupengine.MaintenanceOwnership,
) (Result, error) {
	result := Result{Domain: domain, Mode: mode, StartedAt: now}

	exclusive, err := exclusiveFor(mode)
	if err != nil {
		return result, err
	}

	waitStart := time.Now()

	release, err := fenceFor(ctx, r.Fence, domain, exclusive)
	if err != nil {
		return result, fmt.Errorf("repomaintenance: waiting to maintain %s: %w", domain, err)
	}
	defer release()

	result.Waited = time.Since(waitStart)
	result.Attempted = true

	if exclusive {
		if before, err := repo.Stats(ctx); err == nil {
			result.PhysicalBytesBefore = before.PhysicalBytes
		}
		// A failed measurement is not a failed maintenance. The
		// alternative -- refusing to reclaim space because the listing
		// that would have told an operator how much was reclaimed did not
		// answer -- puts the report ahead of the work.
	}

	started := time.Now()
	report, maintainErr := repo.Maintain(ctx, mode)
	result.Duration = time.Since(started)
	result.Ran = maintainErr == nil && report.Ran

	if exclusive && maintainErr == nil {
		if after, err := repo.Stats(ctx); err == nil {
			result.PhysicalBytesAfter = after.PhysicalBytes
		}

		if result.PhysicalBytesBefore != 0 && result.PhysicalBytesAfter != 0 {
			result.Reclaimed = result.PhysicalBytesBefore - result.PhysicalBytesAfter
		}
	}

	outcome := backupengine.MaintenanceOutcome{
		At:        now.Add(result.Duration),
		Mode:      mode,
		Ran:       result.Ran,
		Reclaimed: result.Reclaimed,
	}

	if maintainErr != nil {
		outcome.Err = maintainErr.Error()
	}

	folded := r.record(record, outcome, mode, now, maintainErr == nil)

	// CompareAndSwap, because a maintenance window is long and the record
	// it started from can have been replaced while it ran -- by an
	// administrative Transfer, most consequentially. An unconditional
	// write here would put this window's owner and counters back over a
	// completed handover, which is a two-owner repository created by the
	// bookkeeping rather than by the work.
	saved, err := r.Store.CompareAndSwap(ctx, folded)
	if err != nil {
		// result.Record stays the record this window was decided from:
		// nothing was stored, and reporting the folded record would have
		// a caller believe otherwise.
		result.Record = record

		if errors.Is(err, backupengine.ErrMaintenanceOwnershipStale) {
			err = fmt.Errorf("repomaintenance: the maintenance record for %s changed while this window ran, so its outcome was not recorded and nothing it would have overwritten was lost: %w", domain, err)
		} else {
			err = fmt.Errorf("repomaintenance: recording the maintenance of %s: %w", domain, err)
		}

		if maintainErr != nil {
			return result, fmt.Errorf("%w; recording that failure also failed: %w", maintainErr, err)
		}

		return result, err
	}

	result.Record = saved

	return result, maintainErr
}

// record folds one outcome into the durable record.
func (r Runner) record(
	record backupengine.MaintenanceOwnership,
	outcome backupengine.MaintenanceOutcome,
	mode backupengine.MaintenanceMode,
	now time.Time,
	succeeded bool,
) backupengine.MaintenanceOwnership {
	intervals := r.Intervals.withDefaults()

	record.LastResult = outcome
	record.Runs++
	record.ReclaimedBytes += outcome.Reclaimed

	limit := r.HistoryLimit
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}

	record.History = append(record.History, outcome)
	if len(record.History) > limit {
		record.History = append([]backupengine.MaintenanceOutcome(nil), record.History[len(record.History)-limit:]...)
	}

	if !succeeded {
		record.Failures++
		record.NextEligible = now.Add(intervals.Retry)

		// LastQuick and LastFull are deliberately NOT advanced. They mean
		// "when this repository last had that maintenance", and a failed
		// attempt is not one: advancing them would let a repository whose
		// maintenance has been failing for a month report that it was
		// maintained an hour ago.
		return record
	}

	switch mode {
	case backupengine.MaintenanceQuick:
		record.LastQuick = now
		record.NextEligible = now.Add(intervals.Quick)
	case backupengine.MaintenanceFull:
		// A full window does quick's work on the way past, so it satisfies
		// both cadences. Recording only LastFull would have a quick
		// maintenance fall due an hour after every full one for no reason.
		record.LastFull = now
		record.LastQuick = now
		record.NextEligible = now.Add(intervals.Quick)
	}

	return record
}

// exclusiveFor maps a mode onto which side of the fence it takes, and is
// where an unknown mode is refused -- before the repository is touched,
// and before an ownership claim is written for a window that cannot run.
func exclusiveFor(mode backupengine.MaintenanceMode) (bool, error) {
	switch mode {
	case backupengine.MaintenanceFull:
		return true, nil
	case backupengine.MaintenanceQuick:
		return false, nil
	default:
		return false, fmt.Errorf("repomaintenance: unknown maintenance mode %q", mode)
	}
}

func fenceFor(ctx context.Context, fence *Fence, domain model.RepositoryDomainID, exclusive bool) (func(), error) {
	if exclusive {
		return fence.Exclusive(ctx, domain)
	}

	return fence.Shared(ctx, domain)
}
