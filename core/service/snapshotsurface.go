package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/repomaintenance"
	"github.com/backupdproject/backupd/core/internal/snapshotretention"
	"github.com/backupdproject/backupd/core/internal/state"
)

// EPIC K's read surface, in this package's own vocabulary (#788): what
// snapshots exist, what retention would do about them, what is holding
// them, and whether the repositories underneath are healthy.
//
// # Why there is no Kopia anything here
//
// EPIC K is explicit that the embedded engine gets no API of its own, and
// this file is where that rule is actually kept. Every type below is
// spelled in this product's words -- a run, a phase, five measurements
// and a verification claim -- and nothing from core/internal crosses out
// of it. A surface that wanted the vendor's vocabulary would have to
// invent it, which is the point.
//
// # Why the counters are pointers
//
// Because nil is a different answer from zero and both occur. A run that
// died before its manifest was recorded, and a snapshot crash
// reconciliation adopted from the repository, both have counters nobody
// ever took; a run that scanned an empty directory has counters that are
// genuinely zero. Flattening the first into the second is how a surface
// ends up reporting "0 bytes read" for a backup nobody measured, which
// reads as a backup that did nothing.

// Snapshot is one snapshot run as every surface reports it.
//
// It is ONE type for three readers -- the snapshot list, the snapshot
// detail, and the snapshots attached to a durable operation -- because
// they are three views of one row and a second shape would eventually
// disagree with this one about what a byte count means.
type Snapshot struct {
	// RunID is this deployment's identifier for the pass, which exists
	// from the moment it starts. SnapshotID is the engine's opaque
	// manifest id and exists only once a manifest was committed, so a
	// failed run has the first and not the second. Neither is a path and
	// neither is a secret.
	RunID      string
	SnapshotID string

	// BackupSetID is "source/set". OperationID is the durable operation
	// the run belonged to, empty for a run the daemon's own schedule
	// started -- which is a real answer, not a lost attribution.
	BackupSetID string
	OperationID string

	// Engine and RepositoryDomain are which machinery ran and which
	// declared boundary its output went into. They are read off the ROW
	// rather than off the current configuration, so a set whose engine
	// was changed still reports each run under the engine that performed
	// it.
	Engine           string
	RepositoryDomain string

	// Phase is the run's durable phase and Succeeded is whether it
	// advertises a restore point. Both, because the phase vocabulary has
	// interesting values in the middle -- a committed, unverified
	// manifest is neither a success nor a failure -- and a client should
	// not have to interpret it to answer the one question everybody asks.
	Phase     string
	Succeeded bool

	// LastKnownGood is whether this run's snapshot is currently the set's
	// advertised restore point.
	LastKnownGood bool

	// SourceConsistency is what the operator had arranged around the
	// source while this pass read it. It is on the record because it is
	// what a restore point's trustworthiness rests on, and because it is
	// a claim about that moment rather than about the configuration as it
	// stands now.
	SourceConsistency string

	// VerificationStatus is "", "pending", "passed" or "failed".
	// VerificationLevel is what the set asked for and
	// VerificationAchieved is what this run actually proved. Three
	// fields because they are three claims: a set configured for a
	// restore drill whose run only verified content must not read as
	// having drilled.
	VerificationStatus   string
	VerificationLevel    string
	VerificationAchieved string

	// EntriesScanned is every source entry the pass considered, of every
	// kind, including the ones it deliberately skipped. It is the source
	// side's own census and NOT Files plus Directories: a pass that
	// refused two hundred sockets considered them, and adding two counts
	// a client already has would be a derivation dressed up as a
	// measurement.
	EntriesScanned *int64

	Files       *int64
	Directories *int64

	// The four byte counts, which are four different facts. LogicalBytes
	// is what the source tree weighs, SourceBytesRead is what the pass
	// pulled off the source, RepositoryBytesWritten is what actually
	// landed in storage, and ContentReusedBytes is what deduplication
	// saved. Rendering the first as "uploaded" is the specific
	// misrepresentation EPIC K forbids.
	LogicalBytes           *int64
	SourceBytesRead        *int64
	RepositoryBytesWritten *int64
	ContentReusedBytes     *int64

	// SourceComplete is the source side's own verdict that the pass
	// covered everything it was asked to. Nil means nobody recorded a
	// verdict, which is a different claim from "the pass was
	// incomplete", and neither of them is a restore point.
	SourceComplete *bool

	// Reason is the run's own sentence about why it failed, was
	// quarantined or was lost. Empty for a run with nothing to explain.
	Reason string

	StartedAt time.Time

	// CompletedAt is when the run came to rest, nil while it is in
	// flight, and Duration is the gap between the two. Duration is nil
	// for exactly the same runs: a duration for something unfinished is a
	// measurement of now rather than of the run.
	CompletedAt *time.Time
	Duration    *time.Duration

	// DeleteRequestedAt is the durable intent to delete this snapshot,
	// recorded before the repository was asked for anything.
	DeleteRequestedAt *time.Time

	// Holds are the unreleased holds over this snapshot. A held snapshot
	// is never deleted by retention, whatever the tier chain says.
	Holds []SnapshotHold
}

// SnapshotHold is one durable statement that a snapshot must not be
// deleted until somebody releases it.
type SnapshotHold struct {
	HoldID      string
	RunID       string
	BackupSetID string

	// Reason and PlacedBy are what make a hold releasable by somebody who
	// was not there when it was placed. Both are required at the point a
	// hold is created, which is why neither is ever empty here.
	Reason   string
	PlacedBy string

	PlacedAt time.Time

	// ReleasedAt and ReleasedBy are set once the hold has ended. A
	// released hold is kept rather than deleted, because "who ended this
	// protection, and when" is the first question asked after a deletion
	// somebody disputes.
	ReleasedAt *time.Time
	ReleasedBy string

	// Active is whether this hold still protects its snapshot.
	Active bool
}

// SnapshotTransition is one edge of the snapshot state machine as it
// actually happened. The run record is overwritten by every advance, so
// it says what a run IS and never how it got there; this log is what
// tells a run verified twice apart from one verified once.
type SnapshotTransition struct {
	From   string
	To     string
	At     time.Time
	Detail string
}

// SnapshotDetail is one run in full.
type SnapshotDetail struct {
	Snapshot    Snapshot
	Transitions []SnapshotTransition
}

// SnapshotRetentionVerdict is what snapshot retention would do about one
// snapshot, and why.
type SnapshotRetentionVerdict struct {
	RunID      string
	SnapshotID string

	// Action is "KEEP", "DELETE" or "REFUSE". Three values and never
	// two: "policy says delete but it is not safe to" and "policy says
	// keep" are different facts and only one of them needs somebody to
	// look at it.
	Action string

	StartedAt time.Time

	// Tiers is every reason this snapshot survived, populated only on a
	// KEEP.
	Tiers []SnapshotRetentionTier

	// Holds are the unreleased holds over it, whether or not they are
	// what kept it. Naming them is the point: "kept by a hold" is
	// unactionable without knowing which hold and who placed it.
	Holds []SnapshotHold

	// Reason is the sentence an operator reads. HoldReason is set only
	// when a REFUSE is about the whole backup SET rather than this
	// snapshot, so a client can raise a standing condition without
	// matching prose.
	Reason     string
	HoldReason string
}

// SnapshotRetentionTier is one selection that kept a snapshot: which
// tier, and what about the snapshot the tier selected it for.
//
// SelectedBy is the placement inside the tier's bucket -- the newest in
// the day, the newest in the month -- and it is empty for FR-19's
// last-known-good protection, which has no placement and whose tier name
// already says what kind of thing it is.
type SnapshotRetentionTier struct {
	Tier       string
	SelectedBy string
}

// SnapshotRetentionPreview is one backup set's whole retention picture at
// one instant.
type SnapshotRetentionPreview struct {
	GeneratedAt time.Time
	Verdicts    []SnapshotRetentionVerdict
}

// RepositoryHealth is one repository domain's own verdict. See
// internal/health.RepositoryHealth for what each probe means and why they
// are reported separately.
type RepositoryHealth struct {
	Domain     string
	MayShare   bool
	BackupSets []string

	// State is "HEALTHY", "DEGRADED" or "FAILING", the same vocabulary a
	// backup set's verdict uses.
	State string

	Reachable        bool
	Readable         bool
	Writable         bool
	CredentialsValid bool

	// ClockSane is whether this process's clock can be trusted for the
	// timestamps a repository reasons about. ClockSkew is how far it is
	// ahead of (positive) or behind (negative) the newest durable
	// timestamp on record, and it is nil when there is no such timestamp
	// -- a brand-new deployment, which is neither a sane nor an insane
	// clock.
	ClockSane bool
	ClockSkew *time.Duration

	MaintenanceOverdue    bool
	LastMaintenanceAt     time.Time
	LastMaintenanceResult string

	LastSnapshotAt     time.Time
	LastSnapshotStatus string

	LastVerificationAt     time.Time
	LastVerificationStatus string

	// Detail is one sentence naming what is wrong. It never carries a
	// path, an endpoint or any part of a credential.
	Detail string
}

// RepositoryHealthReport is every declared domain's verdict, taken at one
// instant.
type RepositoryHealthReport struct {
	GeneratedAt  time.Time
	Repositories []RepositoryHealth
}

// RepositoryMaintenance is one repository's maintenance state.
type RepositoryMaintenance struct {
	Domain string

	// Owner is the deployment that owns maintenance for this repository,
	// empty when nobody does. Ownership is the load-bearing part: several
	// deployments may share one repository and exactly one of them may
	// maintain it, so "not being maintained" has two very different
	// causes and an operator needs to know which. There is deliberately
	// no companion "owned until": ownership does not lapse on a clock,
	// it moves when an owner hands it over or when another instance
	// claims a repository nobody owns.
	Owner       string
	LastQuickAt time.Time

	// LastFullAt is when a full maintenance last completed. Full
	// maintenance is what actually reclaims storage; a repository with
	// recent quick runs and no full run is growing.
	LastFullAt     time.Time
	NextEligibleAt time.Time

	Due       bool
	DueMode   string
	DueReason string

	// Overdue is the stronger claim than Due: long enough with no
	// successful maintenance to be worth an operator's attention.
	Overdue bool

	Runs           int64
	Failures       int64
	ReclaimedBytes int64

	// Failing is whether the most recent window failed, which is a
	// different situation from a failure count: ten failures in a row and
	// one failure a year ago send an operator to different places.
	Failing bool
}

// ErrSnapshotNotFound is what a read reports for a run id this backup
// set's lineage does not contain.
var ErrSnapshotNotFound = errors.New("service: no snapshot of that id belongs to this backup set")

// ErrSnapshotHoldNotFound is what a release reports for a hold id this
// backup set is not holding.
var ErrSnapshotHoldNotFound = errors.New("service: this backup set holds no active hold of that id")

// ErrSnapshotNotHoldable is what a hold reports for a snapshot that has
// nothing for a hold to protect: a run that committed no manifest, one
// already deleted or being deleted, and one at LOST.
//
// It is its own sentinel rather than ErrInvalidRequest for
// ErrConnectionNotProven's reason: the request is not malformed. It is a
// well-formed request whose one problem is the state of the snapshot it
// names, and a client reading INVALID_REQUEST would tell an operator to
// fix a body that is already correct. It is not ErrSnapshotNotFound
// either: the run is right there in the list they clicked it from, which
// is exactly why a not-found would read as this product losing track of
// it.
var ErrSnapshotNotHoldable = errors.New("service: this snapshot cannot be held")

// ErrRepositoryDomainNotFound is what a repository read reports for a
// domain this configuration does not declare.
var ErrRepositoryDomainNotFound = errors.New("service: this deployment declares no repository domain of that id")

// ListSnapshots is one backup set's snapshot history, newest first.
func (b *BackupService) ListSnapshots(ctx context.Context, id string) ([]Snapshot, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	records, err := b.state.Load().inner.ListSnapshots(ctx, sourceName, setName)
	if err != nil {
		return nil, snapshotSurfaceError(id, err)
	}

	out := make([]Snapshot, 0, len(records))
	for _, rec := range records {
		out = append(out, toSnapshot(rec.Run, rec.Holds))
	}

	return out, nil
}

// GetSnapshot is one run of one backup set, with its holds and the
// transition log that says how it reached the phase it is in.
func (b *BackupService) GetSnapshot(ctx context.Context, id, runID string) (SnapshotDetail, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return SnapshotDetail{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	detail, err := b.state.Load().inner.GetSnapshot(ctx, sourceName, setName, runID)
	if err != nil {
		return SnapshotDetail{}, snapshotSurfaceError(id, err)
	}

	out := SnapshotDetail{Snapshot: toSnapshot(detail.Run, detail.Holds)}
	for _, t := range detail.Transitions {
		out.Transitions = append(out.Transitions, SnapshotTransition{
			From:   string(t.From),
			To:     string(t.To),
			At:     t.At,
			Detail: t.Detail,
		})
	}

	return out, nil
}

// ListSnapshotHolds is every unreleased hold over one backup set's
// snapshots.
func (b *BackupService) ListSnapshotHolds(ctx context.Context, id string) ([]SnapshotHold, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	holds, err := b.state.Load().inner.ListSnapshotHolds(ctx, sourceName, setName)
	if err != nil {
		return nil, snapshotSurfaceError(id, err)
	}

	return toSnapshotHolds(id, holds), nil
}

// SnapshotRetention is what snapshot retention would decide about every
// one of this set's snapshots right now. It deletes nothing.
func (b *BackupService) SnapshotRetention(ctx context.Context, id string) (SnapshotRetentionPreview, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return SnapshotRetentionPreview{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	verdicts, err := b.state.Load().inner.SnapshotRetentionPreview(ctx, sourceName, setName)
	if err != nil {
		return SnapshotRetentionPreview{}, snapshotSurfaceError(id, err)
	}

	out := SnapshotRetentionPreview{GeneratedAt: now()}
	for _, v := range verdicts {
		out.Verdicts = append(out.Verdicts, toRetentionVerdict(id, v))
	}

	return out, nil
}

// ListRepositories probes every declared repository domain and reports
// each one's health.
func (b *BackupService) ListRepositories(ctx context.Context) (RepositoryHealthReport, error) {
	repos, err := b.state.Load().inner.RepositoryHealth(ctx)
	if err != nil {
		// EPIC K's production gate (#789) is carried through rather than
		// flattened into the internal-error sentence below. The sentence
		// is config's own -- a config key and an environment variable,
		// no path, endpoint or credential -- so the rule that generic
		// sentence enforces is not in play, and the gate is the one
		// refusal here an operator can actually act on. A 500 INTERNAL
		// would tell a dashboard something is broken about a deployment
		// where nothing is.
		if errors.Is(err, ErrIncrementalEngineDisabled) {
			return RepositoryHealthReport{}, err
		}

		return RepositoryHealthReport{}, fmt.Errorf("service: reading repository health: an internal error occurred")
	}

	out := RepositoryHealthReport{GeneratedAt: now()}
	for _, r := range repos {
		out.Repositories = append(out.Repositories, toRepositoryHealth(r))
	}

	return out, nil
}

// RepositoryMaintenanceState reports one declared domain's maintenance
// state.
func (b *BackupService) RepositoryMaintenanceState(ctx context.Context, domain string) (RepositoryMaintenance, error) {
	st, err := b.state.Load().inner.RepositoryMaintenance(ctx, domain)
	if err != nil {
		if errors.Is(err, app.ErrRepositoryDomainNotDeclared) {
			return RepositoryMaintenance{}, fmt.Errorf("%w: %s", ErrRepositoryDomainNotFound, domain)
		}

		return RepositoryMaintenance{}, fmt.Errorf("service: reading repository maintenance: an internal error occurred")
	}

	return toRepositoryMaintenance(domain, st), nil
}

// snapshotSurfaceError maps the app layer's refusals onto this package's
// own, so nothing carrying a filesystem path or a SQLite sentence crosses
// this boundary. See ErrInvalidRequest's own doc for the rule.
func snapshotSurfaceError(id string, err error) error {
	switch {
	case errors.Is(err, app.ErrBackupSetNotConfigured):
		return fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	case errors.Is(err, app.ErrNotAnIncrementalSet):
		return fmt.Errorf("%w: %s", ErrSnapshotRestoreUnsupported, id)
	case errors.Is(err, app.ErrSnapshotNotFound):
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, id)
	case errors.Is(err, state.ErrSnapshotHoldNotFound):
		return fmt.Errorf("%w: %s", ErrSnapshotHoldNotFound, id)
	case errors.Is(err, ErrIncrementalEngineDisabled):
		// EPIC K's production gate (#789), carried through for
		// ListRepositories' reason: config's own sentence names the key
		// to set and nothing about this deployment's storage, and it is
		// the one refusal on these surfaces that is neither about the
		// set nor about a snapshot.
		return err
	case errors.Is(err, app.ErrSnapshotNotHoldable):
		// The app layer's sentence is carried through, which every arm
		// above deliberately does not do. It is safe here and it is the
		// whole value of the refusal: app composes it itself, from the
		// run's own phase and nothing else (see app.notHoldableReason),
		// so it carries no path and no storage-layer text, and WHICH of
		// the four states this snapshot is in is the one thing an
		// operator can act on.
		return fmt.Errorf("%w: %s: %s", ErrSnapshotNotHoldable, id, notHoldableSentence(err))
	case errors.Is(err, app.ErrSetHasNoSnapshots):
		return fmt.Errorf("%w: %s has no restore point", ErrSnapshotNotFound, id)
	default:
		return fmt.Errorf("service: reading %s's snapshots: an internal error occurred", id)
	}
}

// notHoldableSentence is the app layer's own reason for a hold it
// refused, read out of the typed error rather than out of its message.
//
// The fallback is not dead code: errors.Is matches anything wrapping the
// sentinel, so a future caller wrapping it without the type still gets a
// true sentence rather than an empty one.
func notHoldableSentence(err error) string {
	var typed *app.SnapshotNotHoldable
	if errors.As(err, &typed) && typed.Reason != "" {
		return typed.Reason
	}

	return "this run has no snapshot for a hold to protect"
}

// toSnapshot projects one catalog row onto the wire's own vocabulary.
func toSnapshot(run state.SnapshotRun, holds []state.SnapshotHold) Snapshot {
	out := Snapshot{
		RunID:                  run.RunID,
		SnapshotID:             run.SnapshotID,
		BackupSetID:            run.Set.String(),
		OperationID:            run.OperationID,
		Engine:                 run.Engine,
		RepositoryDomain:       run.Domain,
		Phase:                  string(run.Phase),
		Succeeded:              run.Phase.Advertised(),
		LastKnownGood:          run.LastKnownGood,
		SourceConsistency:      run.ConsistencyMode,
		VerificationStatus:     run.VerificationStatus,
		VerificationLevel:      run.VerificationLevel,
		VerificationAchieved:   run.VerificationLevelAchieved,
		EntriesScanned:         run.EntriesScanned,
		Files:                  run.Files,
		Directories:            run.Directories,
		LogicalBytes:           run.LogicalBytes,
		SourceBytesRead:        run.SourceBytesRead,
		RepositoryBytesWritten: run.RepositoryBytesWritten,
		ContentReusedBytes:     run.ContentReusedBytes,
		SourceComplete:         run.SourceComplete,
		Reason:                 run.Reason,
		StartedAt:              run.StartedAt,
		CompletedAt:            run.CompletedAt,
		DeleteRequestedAt:      run.DeleteRequestedAt,
		Holds:                  toSnapshotHolds(run.Set.String(), holds),
	}

	if run.CompletedAt != nil && !run.StartedAt.IsZero() {
		d := run.CompletedAt.Sub(run.StartedAt)
		out.Duration = &d
	}

	return out
}

// toSnapshotHolds projects hold rows, attaching the backup set id the
// rows themselves do not carry (they key on the durable lineage uuid,
// which is not what a surface prints).
func toSnapshotHolds(backupSetID string, holds []state.SnapshotHold) []SnapshotHold {
	if len(holds) == 0 {
		return nil
	}

	out := make([]SnapshotHold, 0, len(holds))
	for _, h := range holds {
		out = append(out, SnapshotHold{
			HoldID:      h.HoldID,
			RunID:       h.RunID,
			BackupSetID: backupSetID,
			Reason:      h.Reason,
			PlacedBy:    h.PlacedBy,
			PlacedAt:    h.PlacedAt,
			ReleasedAt:  h.ReleasedAt,
			ReleasedBy:  h.ReleasedBy,
			Active:      h.Active(),
		})
	}

	return out
}

func toRetentionVerdict(backupSetID string, v snapshotretention.Verdict) SnapshotRetentionVerdict {
	out := SnapshotRetentionVerdict{
		RunID:      v.Run,
		SnapshotID: string(v.Snapshot),
		Action:     string(v.Action),
		StartedAt:  v.StartedAt,
		Holds:      toSnapshotHolds(backupSetID, v.Holds),
		Reason:     v.Reason,
		HoldReason: v.HoldReason,
	}

	for _, t := range v.Tiers {
		out.Tiers = append(out.Tiers, SnapshotRetentionTier{
			Tier:       string(t.Tier),
			SelectedBy: string(t.By),
		})
	}

	return out
}

func toRepositoryHealth(r health.RepositoryHealth) RepositoryHealth {
	out := RepositoryHealth{
		Domain:                 r.Domain,
		MayShare:               r.MayShare,
		BackupSets:             r.BackupSets,
		State:                  r.State.String(),
		Reachable:              r.Reachable,
		Readable:               r.Readable,
		Writable:               r.Writable,
		CredentialsValid:       r.CredentialsValid,
		ClockSane:              r.ClockSane,
		MaintenanceOverdue:     r.MaintenanceOverdue,
		LastMaintenanceAt:      r.LastMaintenanceAt,
		LastMaintenanceResult:  r.LastMaintenanceResult,
		LastSnapshotAt:         r.LastSnapshotAt,
		LastSnapshotStatus:     r.LastSnapshotStatus,
		LastVerificationAt:     r.LastVerificationAt,
		LastVerificationStatus: r.LastVerificationStatus,
		Detail:                 r.Detail,
	}

	if r.ClockSkewKnown {
		skew := r.ClockSkew
		out.ClockSkew = &skew
	}

	return out
}

func toRepositoryMaintenance(domain string, st app.RepositoryMaintenanceState) RepositoryMaintenance {
	m := st.Metrics

	return RepositoryMaintenance{
		Domain:         domain,
		Owner:          m.Owner.String(),
		LastQuickAt:    m.LastQuick,
		LastFullAt:     m.LastFull,
		NextEligibleAt: m.NextEligible,
		Due:            st.Decision.Due,
		DueMode:        dueModeOf(st.Decision),
		DueReason:      st.Decision.Reason,
		Overdue:        st.Overdue,
		Runs:           int64(m.Runs),
		Failures:       int64(m.Failures),
		ReclaimedBytes: m.ReclaimedBytes,
		Failing:        m.Failing,
	}
}

// dueModeOf reports which mode is due, and nothing when none is.
//
// A mode on a decision that is not due would be a plan nobody made: the
// scheduler names a mode as part of saying yes, and echoing it beside a
// no would read as "full maintenance is scheduled" to anybody scanning a
// column.
func dueModeOf(d repomaintenance.Decision) string {
	if !d.Due {
		return ""
	}

	return string(d.Mode)
}
