package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/snapshotlifecycle"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// This file is the incremental engine's share of a processing cycle: the
// pass a backup set configured `engine: kopia` gets INSTEAD of the
// artifact pipeline.
//
// Instead is the whole shape of it. #780 put a refusal here, in
// processBackupSet, because running the artifact pipeline over a source
// TREE would walk it as though every file in it were a finished artifact,
// copy each one, commit it, and then offer the source's copy for deletion.
// This replaces that refusal for the one engine that now has a pipeline,
// and leaves it in place for anything else: unrunnableEngine still owns
// the default, so an engine this build has never heard of is refused
// rather than routed to whichever pipeline happens to be first in the
// switch.
//
// What a pass does, in order, is reconcile and then run, which is the
// cycle order the artifact side already uses and for the same reason: a
// crash left the catalog and the repository disagreeing, and deciding
// that disagreement BEFORE taking a new snapshot is what stops a second
// unattributable manifest being added to the pile.
//
// Nothing here deletes anything, on the source or in the repository. A
// completed snapshot is not permission to remove what it was taken of,
// which is EPIC K's one unconditional rule, and the way this file keeps
// it is by having no code that could: the only writes it can reach are
// the snapshot the engine stores and the catalog rows the lifecycle
// package advances.

// ErrNoIncrementalEngine is what an incremental set's pass reports when
// this Service has no engine to open a repository with.
//
// It is distinct from ErrEngineNotImplemented next door because the two
// are different problems with different fixes: that one means this BUILD
// has no pipeline for the configured engine, and this one means this
// PROCESS was constructed without the adapter that pipeline needs. The
// second is a wiring mistake in whoever built the Service (see
// core/service.Open), and reporting it in the words of the first would
// send an operator looking for a missing feature.
var ErrNoIncrementalEngine = errors.New("app: this service has no backup engine wired, so a set configured for the incremental engine cannot be run")

// ErrNoSnapshotCatalog is what a pass reports when the journal it was
// given cannot record snapshot runs.
//
// The catalog is asked for by type assertion rather than being part of
// the Journal interface, and that is deliberate: Journal is implemented
// by a dozen test doubles across this repository, and widening it would
// make every one of them grow methods for a feature they have no
// business in. The production journal (*state.Journal) satisfies it, and
// a journal that does not refuses the pass rather than performing an
// unrecorded backup.
var ErrNoSnapshotCatalog = errors.New("app: this journal cannot record snapshot runs, and a snapshot nothing records is not a backup")

// ErrRepositoryStorageUnsupported is what a pass reports for a repository
// domain whose storage this build cannot place.
//
// Today a repository lives under the deployment's own backup root, in the
// reserved namespace backupengine.ReservedLocalDir computes. A bucket
// repository is a real case the adapter already supports (#781) and it
// needs per-domain storage CONFIGURATION that the schema does not carry
// yet; until it does, a domain cannot silently get a local repository
// somewhere the operator did not ask for.
var ErrRepositoryStorageUnsupported = errors.New("app: this build can only place a repository under the deployment's backup root")

// SnapshotSetResult is one incremental backup set's pass through a cycle:
// what reconciliation decided, what the run did, and what stopped it.
//
// Reconcile and Run are both reported even when Err is set, because they
// are separate halves and an operator needs to know which one failed: a
// reconciliation that could not reach the repository and a run that could
// not read the source are the same cycle outcome and completely different
// problems.
type SnapshotSetResult struct {
	Reconcile snapshotlifecycle.ReconcileReport
	Run       snapshotlifecycle.RunResult
	Err       error
}

// Succeeded reports whether this pass produced a restore point.
func (r *SnapshotSetResult) Succeeded() bool {
	return r != nil && r.Err == nil && r.Run.Succeeded()
}

// operationKey is the context key carrying the durable operation a cycle
// is running under.
type operationKey struct{}

// WithOperation marks ctx as belonging to one durable operation, so the
// snapshot runs a cycle performs can be attributed to the request that
// asked for them.
//
// It is a context value rather than an argument threaded through
// RunCycle for the reason ProgressObserver is: every function between
// core/service's executor and this file would otherwise grow a parameter
// it does not read. What reads it is one catalog column, and what that
// column buys is the API's answer to "what did the run I submitted
// actually store" (see core/service's operation surface).
//
// A cycle nobody submitted -- the daemon's own schedule -- carries none,
// and its runs record an empty operation id. That is a real value and
// not a gap: it says the schedule did it.
func WithOperation(ctx context.Context, operationID string) context.Context {
	if operationID == "" {
		return ctx
	}

	return context.WithValue(ctx, operationKey{}, operationID)
}

func operationFrom(ctx context.Context) string {
	id, _ := ctx.Value(operationKey{}).(string)

	return id
}

// processIncrementalSet runs one incremental backup set's whole share of a
// cycle.
//
// The repository is opened once for the pass and closed on the way out:
// opening one loads format blobs and builds an index cache, so opening it
// per phase would pay that cost twice for reconciliation and the run, and
// leaving it open past the pass would hold a lock on storage the rest of
// the cycle has no use for.
func (s *Service) processIncrementalSet(ctx context.Context, src config.Source, bs config.BackupSet) *SnapshotSetResult {
	result := &SnapshotSetResult{}

	catalog, ok := s.Journal.(snapshotlifecycle.Catalog)
	if !ok {
		result.Err = ErrNoSnapshotCatalog

		return result
	}

	if s.Repositories == nil {
		result.Err = ErrNoIncrementalEngine

		return result
	}

	loc, err := s.repositoryLocation(bs)
	if err != nil {
		result.Err = err

		return result
	}

	repo, err := s.openRepository(ctx, catalog, loc)
	if err != nil {
		result.Err = err

		return result
	}

	defer func() {
		if cerr := repo.Close(ctx); cerr != nil && result.Err == nil {
			result.Err = fmt.Errorf("closing repository %s: %w", bs.Repository.Domain, cerr)
		}
	}()

	tree, ok := repo.(backupengine.TreeRepository)
	if !ok {
		// A repository that cannot store a whole source tree as one
		// snapshot cannot run an incremental set at all. Falling back to
		// the per-object streaming port would produce one snapshot per
		// file, which is the Phase-1 interim shape #783 exists to
		// replace, so it is refused rather than silently used.
		result.Err = errors.New("app: this repository cannot store a source tree as one snapshot")

		return result
	}

	identity := snapshotSource(bs)

	result.Reconcile, err = s.reconcileSnapshots(ctx, catalog, bs, tree, identity)
	if err != nil {
		result.Err = err

		return result
	}

	result.Run, result.Err = s.runSnapshot(ctx, catalog, src, bs, tree, identity)

	return result
}

// reconcileSnapshots decides what a crash left behind for one set, before
// this cycle adds anything of its own.
func (s *Service) reconcileSnapshots(
	ctx context.Context,
	catalog snapshotlifecycle.Catalog,
	bs config.BackupSet,
	repo snapshotlifecycle.Repository,
	identity backupengine.Source,
) (snapshotlifecycle.ReconcileReport, error) {
	rec := &snapshotlifecycle.Reconciler{
		Catalog:  catalog,
		Now:      s.now,
		Observer: s.snapshotObserver(ctx),
	}

	report, err := rec.Reconcile(ctx, snapshotlifecycle.ReconcileRequest{
		Set:            bs.ID,
		SetUUID:        snapshotLineage(bs),
		Domain:         bs.Repository.Domain,
		Source:         identity,
		SourceIdentity: bs.SourceIdentity,
		Repository:     repo,
		Maintenance:    s.maintenanceRecord(ctx, bs.Repository.Domain),
	})
	if err != nil {
		s.logger().Error(ctx, "snapshot-reconcile", err)

		return report, err
	}

	for _, v := range report.Changed() {
		s.logger().SnapshotPhase(ctx, bs.ID.String(), v.RunID, v.SnapshotID, string(v.From), string(v.To), v.Reason,
			v.To == state.PhaseFailed)
	}

	return report, nil
}

// runSnapshot takes this cycle's snapshot of one set.
func (s *Service) runSnapshot(
	ctx context.Context,
	catalog snapshotlifecycle.Catalog,
	src config.Source,
	bs config.BackupSet,
	repo snapshotlifecycle.Repository,
	identity backupengine.Source,
) (snapshotlifecycle.RunResult, error) {
	adapter, err := s.sourceAdapter(bs)
	if err != nil {
		return snapshotlifecycle.RunResult{}, err
	}

	transportSource := sourceFor(s.Config, src, bs)
	runID := uuid.NewString()

	runner := &snapshotlifecycle.Runner{
		Catalog:  catalog,
		Now:      s.now,
		Observer: s.snapshotObserver(ctx),
	}

	res, err := runner.Run(ctx, snapshotlifecycle.RunRequest{
		RunID: runID,

		// One key per PASS, not per set: a cycle that runs again
		// tomorrow is a new logical request over a source that has
		// changed, so it must not resolve to yesterday's row. What the
		// key does buy is the narrow case it is for -- the same
		// submission arriving twice, or a retry of one -- which is why
		// the operation id is in it when there is one.
		IdempotencyKey:    snapshotIdempotencyKey(bs.ID, operationFrom(ctx), runID),
		OperationID:       operationFrom(ctx),
		Set:               bs.ID,
		SetUUID:           snapshotLineage(bs),
		Engine:            bs.Engine,
		Domain:            bs.Repository.Domain,
		SourceIdentity:    bs.SourceIdentity,
		Consistency:       bs.Consistency,
		VerificationLevel: bs.VerificationLevel,
		Source:            identity,
		Description:       "backupd " + bs.ID.String(),
		Repository:        repo,
		OpenTree: func(ctx context.Context) (snapshotlifecycle.SourceTree, error) {
			t, err := adapter.OpenTree(ctx, transportSource)
			if err != nil {
				return nil, fmt.Errorf("opening the source tree: %w", err)
			}

			return sourceTree{tree: t}, nil
		},
	})

	s.logger().SnapshotStats(ctx, bs.ID.String(), res.RunID,
		res.Entries, res.LogicalBytes, res.SourceBytesRead, res.RepositoryBytesWritten, res.ContentReusedBytes)

	if err != nil {
		s.logger().Error(ctx, "snapshot-run", err)
	}

	return res, err
}

// sourceAdapter builds the reading side for one incremental set.
//
// Every capability comes off the transport this Service already holds, by
// type assertion, which is the arrangement source.SessionOpener's own doc
// argues for: the production transport satisfies all of them, and a
// second set of fields to wire would be a capability that is on in the
// test that proves it and off in production.
func (s *Service) sourceAdapter(bs config.BackupSet) (*source.Adapter, error) {
	if s.Transport == nil {
		return nil, errors.New("app: this service has no transport, so an incremental set's source cannot be read")
	}

	streamer, ok := s.Transport.(source.Streamer)
	if !ok {
		return nil, errors.New("app: this transport cannot open a source object for reading")
	}

	stater, ok := s.Transport.(source.Stater)
	if !ok {
		return nil, errors.New("app: this transport cannot stat a source object, so a read window cannot be checked")
	}

	enumerator, ok := s.Transport.(transport.Enumerator)
	if !ok {
		return nil, errors.New("app: this transport cannot enumerate a source within a memory bound")
	}

	deps := source.Deps{
		Streamer:   streamer,
		Stater:     stater,
		Enumerator: enumerator,
	}

	// A link reader is optional and its absence is what makes the
	// preserve policy a refusal rather than a guess (source.Deps.Links).
	if links, ok := s.Transport.(source.LinkReader); ok {
		deps.Links = links
	}

	return source.New(deps, source.Options{
		Mode: bs.Consistency,

		// The conservative preset, and not because it is the safe-looking
		// default: a tree run reads every byte the source offers
		// regardless (see backupengine.TreeRepository.SnapshotTree), so
		// this is the preset that DESCRIBES what actually happens.
		// Recording trust_metadata here would put a claim on the run
		// report that the run does not act on.
		Preset: model.PresetConservative,
	})
}

// repositoryLocation is where one set's repository lives.
//
// Only the local kind is placed, and the refusal for anything else is
// ErrRepositoryStorageUnsupported's own argument. Root is the
// deployment's backup root rather than the repository directory: the
// adapter puts the repository under the reserved namespace inside it, and
// deriving that path in two places is how the two would eventually
// disagree.
func (s *Service) repositoryLocation(bs config.BackupSet) (backupengine.RepositoryLocation, error) {
	domain := bs.Repository.Domain
	if domain.IsZero() {
		return backupengine.RepositoryLocation{}, errors.New("app: this set names no repository domain")
	}

	passphrase, ok := s.repositoryPassphrase(domain)
	if !ok {
		return backupengine.RepositoryLocation{}, fmt.Errorf("app: repository domain %q is not declared in this configuration", domain)
	}

	root := s.Config.EffectiveBackupRoot()
	if root == "" {
		return backupengine.RepositoryLocation{}, ErrRepositoryStorageUnsupported
	}

	return backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       root,
		Passphrase: passphrase,
	}, nil
}

// repositoryPassphrase is the declared secret reference for one domain,
// and whether the domain is declared at all.
//
// The two answers are one lookup because they have one cause: a set
// naming an undeclared domain is a configuration this validator already
// refuses, so reaching here with one means the Service is holding a
// config that never went through Validate, and that is worth a refusal
// rather than a zero Ref the adapter would report as a missing secret.
func (s *Service) repositoryPassphrase(domain model.RepositoryDomainID) (secretref.Ref, bool) {
	for i := range s.Config.RepositoryDomains {
		d := s.Config.RepositoryDomains[i]
		if d.Domain.ID == domain {
			return d.PassphraseRef, true
		}
	}

	return secretref.Ref{}, false
}

// maintenanceRecord is the last recorded maintenance state for a
// repository, or nil when there is none or it cannot be read.
//
// Nil on failure is deliberate and is the one place in this file that
// swallows an error: a reconciliation pass must not fail a backup set
// over an unreadable maintenance note, because the note says nothing
// about any snapshot (see the reconciler's maintenanceVerdict).
func (s *Service) maintenanceRecord(ctx context.Context, domain model.RepositoryDomainID) *backupengine.MaintenanceOwnership {
	dir, err := backupengine.ReservedLocalStateDir(s.Config.EffectiveBackupRoot())
	if err != nil {
		return nil
	}

	store, err := backupengine.NewFileMaintenanceOwnershipStore(dir)
	if err != nil {
		return nil
	}

	record, err := store.Load(ctx, domain)
	if err != nil {
		return nil
	}

	return &record
}

// snapshotObserver is the cycle's sink for phase changes.
func (s *Service) snapshotObserver(ctx context.Context) snapshotlifecycle.Observer {
	return &snapshotLogObserver{service: s, ctx: ctx}
}

// snapshotLogObserver turns lifecycle phase changes into FR-23 events.
//
// It holds the cycle's context, which is unusual in this package and is
// what the Observer interface costs: a phase change is observed from
// inside a call this file made, and the alternative -- an observer method
// taking a context -- would be a context the lifecycle package has to
// thread through every durable write purely so a logger can have it.
type snapshotLogObserver struct {
	service *Service
	ctx     context.Context //nolint:containedctx // see the type's own doc.
}

func (o *snapshotLogObserver) ObservePhase(set model.BackupSetID, runID string, from, to state.SnapshotPhase, _ time.Time) {
	o.service.logger().SnapshotPhase(o.ctx, set.String(), runID, "", string(from), string(to), "", to == state.PhaseFailed)
}

func (o *snapshotLogObserver) ObserveRun(res snapshotlifecycle.RunResult) {
	o.service.logger().SnapshotPhase(o.ctx, res.Set.String(), res.RunID, res.SnapshotID,
		string(res.Phase), string(res.Phase), res.Reason, !res.Succeeded())
}

// snapshotIdempotencyKey identifies one logical snapshot request.
//
// A submitted run is keyed by its operation, so the same submission
// arriving twice resolves to one pass over the source. A scheduled cycle
// has no operation to key on and uses the run id, which makes every
// scheduled pass its own logical request -- which it is: the source has
// moved on since the last one.
func snapshotIdempotencyKey(set model.BackupSetID, operationID, runID string) string {
	if operationID != "" {
		return "operation:" + operationID + ":" + set.String()
	}

	return "cycle:" + set.String() + ":" + runID
}

// snapshotSourceHost is what every snapshot this product writes records
// as its host.
//
// It is this PRODUCT's name and not this machine's, and that is the
// decision the whole of snapshotSource exists to make explicit: a
// hostname is not stable -- a NAS gets renamed, a container gets a new
// id, a deployment moves -- and the engine treats host, user and path
// together as a source's identity, so a rename would present the same
// source as a new one, find no predecessor, and store a second full copy
// of a tree that had not changed.
const snapshotSourceHost = "backupd"

// snapshotSource is one backup set's identity in the repository's own
// namespace: ONE source per set, per #783, derived from the set's stable
// source identity rather than from where its data happens to live.
//
// # Why the path is a digest
//
// model.SourceIdentity already exists for exactly this question, and it
// is a digest rather than a readable composite for two reasons its own
// doc gives: a composite would carry a host and a path, both of which
// are treated as sensitive in real deployments (config's
// sensitive_endpoint, #295), and this value lands in a repository's
// source namespace, a catalog column and log lines. Putting the source's
// real path here instead would publish the shape of an operator's
// filesystem into all three, and would fork the lineage every time a
// mount prefix moved -- which is precisely what SourceMountPrefix exists
// to prevent.
//
// # Why the user is the domain
//
// The engine requires a user and it must be a stable, non-secret string.
// The repository domain is both, and it says something true and useful:
// which boundary this source was admitted to. A snapshot whose domain
// disagrees with the repository holding it is evidence of a snapshot
// written somewhere it does not belong, which is the same argument
// backupengine.TagKeyDomain makes for carrying it redundantly as a tag.
func snapshotSource(bs config.BackupSet) backupengine.Source {
	return backupengine.Source{
		Host: snapshotSourceHost,
		User: bs.Repository.Domain.String(),
		Path: "/" + bs.SourceIdentity.String(),
	}
}

// sourceTree adapts backupengine/source's Tree to the lifecycle's own
// port.
//
// The translation is one struct and two methods, and it is worth having
// rather than making the lifecycle package import the source adapter: the
// run driver needs four facts about a pass, and depending on the reading
// side for them would drag a transport, a capability matrix and a
// backend manifest set into a package whose tests are about crash
// boundaries.
type sourceTree struct {
	tree *source.Tree
}

func (t sourceTree) Root() backupengine.SourceDir { return t.tree.Root() }
func (t sourceTree) Err() error                   { return t.tree.Err() }
func (t sourceTree) Close() error                 { return t.tree.Close() }

// Report projects the reading side's census onto the four facts a run
// records.
//
// Complete is the source side's own verdict (source.Report.Complete) and
// is NOT re-derived here: whether a refused path or an unreadable object
// makes a backup incomplete is a decision that package argues, and a
// second opinion in this file would be a second answer.
func (t sourceTree) Report() snapshotlifecycle.ScanReport {
	rep := t.tree.Report()

	return snapshotlifecycle.ScanReport{
		Entries:  rep.Entries,
		Stored:   rep.Stored,
		Skipped:  rep.SkippedSymlink + rep.SkippedSpecial + rep.SkippedExcluded,
		Complete: rep.Complete(),
		Reason:   scanReason(rep),
	}
}

// scanReason is the first sentence the source side recorded about
// something it could not store, or empty for a clean pass.
//
// One sentence and not all of them: a Report carries a bounded sample of
// up to sixty-four, a catalog row is read by a person, and the whole
// sample is already in the cycle's own report for anyone who needs it.
func scanReason(rep source.Report) string {
	if rep.Complete() {
		return ""
	}

	if len(rep.Reasons) == 0 {
		return fmt.Sprintf("the source pass left %d entries unstored (%d refused, %d unreadable)",
			rep.Incomplete+rep.Refused+rep.Unreadable, rep.Refused, rep.Unreadable)
	}

	return fmt.Sprintf("%s (%d entries were not stored)",
		rep.Reasons[0], rep.Incomplete+rep.Refused+rep.Unreadable)
}

// snapshotProgress is an incremental pass expressed in the two counts a
// cycle report already speaks (CycleProgress): how much work was in front
// of this set, and how much of it landed.
//
// The unit is the RUN, not the entry, and that is the honest projection.
// A snapshot run is one indivisible piece of work whose output is one
// restore point: a pass that read nine thousand of ten thousand files and
// then failed did not deliver nine tenths of a backup, it delivered none,
// which is exactly what "walked one, none durable" says. Counting entries
// here would make a failed run look like a mostly-successful one on every
// screen that renders these two numbers.
func snapshotProgress(res *SnapshotSetResult) CycleProgress {
	if res == nil {
		return CycleProgress{}
	}

	progress := CycleProgress{Walked: 1}
	if res.Succeeded() {
		progress.Durable = 1
	}

	return progress
}

// openRepository opens this domain's repository, creating it on the first
// run that ever needs it.
//
// # Why creation happens here at all
//
// An operator declares a repository domain and where its passphrase comes
// from. Nothing else in Phase 1 turns that declaration into storage, and
// a set that refused every cycle until somebody ran a command that does
// not exist yet would be a feature with no way in. So the first run
// creates it, at the location derived from the declared domain, under the
// reserved namespace inside this deployment's own backup root.
//
// # Why creation is guarded by the catalog rather than by the storage
//
// "Open failed with not-found, so create one" is the version of this
// function that loses a deployment's backup history. A NAS share that is
// asleep, unmounted or mounted empty presents exactly as a location with
// no repository in it, and creating one there would give the set a fresh,
// empty repository on the wrong filesystem: every later snapshot would
// succeed, deduplicate against nothing, and land somewhere the real
// repository is not -- and the first anybody would hear of it is a
// restore.
//
// So creation requires the catalog to hold NO snapshot committed to this
// DOMAIN, by any backup set. A repository serves a domain, several sets
// may share one (model.RepositoryDomain.MayShare), and evidence from a
// co-tenant is evidence: asking only about the set that happens to be
// running answers "no history" for a brand new set in a populated domain,
// and the answer is used to create a second, empty repository on top of a
// live one. The question is also asked of the whole table rather than of
// a bounded window of recent rows, because a window says "no history" for
// a set whose newest few hundred runs all failed before uploading -- one
// bad fortnight and the guard opens.
//
// A domain with history and no repository is a refusal, loudly, because
// the two durable stores disagree in the one direction that cannot be
// repaired by writing anything: the snapshots are somewhere this process
// cannot see, and the correct action is an operator's.
func (s *Service) openRepository(
	ctx context.Context,
	catalog snapshotlifecycle.Catalog,
	loc backupengine.RepositoryLocation,
) (backupengine.Repository, error) {
	repo, err := s.Repositories.OpenRepository(ctx, loc)
	if err == nil {
		return repo, nil
	}

	if !errors.Is(err, backupengine.ErrRepositoryNotFound) {
		return nil, fmt.Errorf("opening repository %s: %w", loc.Domain, err)
	}

	held, histErr := catalog.DomainHasSnapshot(ctx, loc.Domain.String())
	if histErr != nil {
		return nil, fmt.Errorf("asking whether repository domain %s already holds snapshots: %w", loc.Domain, histErr)
	}

	if held {
		return nil, fmt.Errorf(
			"domain %s holds no repository at this deployment's backup root, but this journal has already recorded snapshots committed to it: "+
				"refusing to create a second, empty repository over a history this process cannot see. "+
				"Check that the storage is mounted and reachable before running again: %w",
			loc.Domain, err)
	}

	if err := s.Repositories.CreateRepository(ctx, loc); err != nil {
		return nil, fmt.Errorf("creating repository %s: %w", loc.Domain, err)
	}

	repo, err = s.Repositories.OpenRepository(ctx, loc)
	if err != nil {
		return nil, fmt.Errorf("opening the repository just created for %s: %w", loc.Domain, err)
	}

	return repo, nil
}

// snapshotLineage is the durable key a set's snapshot rows are grouped
// by: its configured uuid, lower-cased.
//
// Lower-cased because a uuid's letter case carries no information and an
// operator who retypes one in a different case has not created a second
// backup set -- but a lineage key compared byte for byte would say they
// had, and the set would lose its history and grow a second
// last-known-good row. internal/config already compares uuids for
// uniqueness this way, and model.NewSourceIdentity already digests them
// this way.
func snapshotLineage(bs config.BackupSet) string {
	return strings.ToLower(bs.UUID)
}

// snapshotHealth is the incremental engine's half of one backup set's
// health report (FR-24), or nil when there is nothing to say.
//
// Nil in three cases, and they are the same answer for different
// reasons: the set is an artifact set and has no snapshots; the journal
// cannot hold snapshot runs; or the set is incremental and has never run.
// A zeroed block in any of them would render as a run that measured
// nothing, which for the first two is a claim about a pipeline that never
// executes.
//
// A failure reading the catalog is also nil rather than an error, and
// that is the one judgement call here. BuildHealthReport is what
// `backupd status` and the dashboard are built on, and it already
// fails outright for the reads whose reassuring answer would be a lie
// (a connection refusal that could not be read must not look like "no
// set is refused"). This is not one of those: the newest run's byte
// counts are a measurement, not a safety claim, and an unavailable
// measurement is exactly what nil says.
//
// The unfinished-run count is passed IN rather than read here. It is one
// deployment-wide query whose answer is the same for every set in the
// report, and reading it per set made `backupd status` re-run it once per
// configured backup set. BuildHealthReport loads it once and hands each
// set its own count, exactly as it already does for the relocation
// journal (movesBySet).
func (s *Service) snapshotHealth(ctx context.Context, bs config.BackupSet, unfinished unfinishedBySet) *health.SnapshotHealth {
	if bs.Engine != model.EngineKopia {
		return nil
	}

	catalog, ok := s.Journal.(snapshotlifecycle.Catalog)
	if !ok {
		return nil
	}

	lineage := snapshotLineage(bs)

	runs, err := catalog.ListSnapshotRuns(ctx, lineage, 1)
	if err != nil || len(runs) == 0 {
		return nil
	}

	newest := runs[0]

	out := &health.SnapshotHealth{
		RunID:      newest.RunID,
		SnapshotID: newest.SnapshotID,
		Phase:      string(newest.Phase),

		// The source side's own census, as the run recorded it. It used
		// to be files + directories, which is a different number: it
		// leaves out every entry a pass deliberately skipped, and it
		// reads as a confident zero for a snapshot nobody measured.
		EntriesScanned:         counterOf(newest.EntriesScanned),
		LogicalBytes:           counterOf(newest.LogicalBytes),
		SourceBytesRead:        counterOf(newest.SourceBytesRead),
		RepositoryBytesWritten: counterOf(newest.RepositoryBytesWritten),
		ContentReusedBytes:     counterOf(newest.ContentReusedBytes),
		Measured:               newest.SourceBytesRead != nil && newest.RepositoryBytesWritten != nil,
		VerificationStatus:     newest.VerificationStatus,
		VerificationLevel:      newest.VerificationLevel,

		// What the run actually PROVED, beside what its set asked for.
		// Without it a surface has the configured level and the pass/fail
		// and no way to say that a set configured for a restore drill got
		// a content verification.
		VerificationAchieved: newest.VerificationLevelAchieved,
		UnfinishedRuns:       unfinished[lineage],
	}

	if lkg, err := catalog.LastKnownGoodSnapshot(ctx, lineage); err == nil {
		out.LastKnownGoodAt = lkg.CompletedAt
	}

	return out
}

// unfinishedBySet is how many non-terminal snapshot runs each lineage
// has, keyed by the set uuid the rows carry.
type unfinishedBySet map[string]int

// unfinishedSnapshotRuns counts the crash reconciler's worklist once for
// a whole health report.
//
// A failure is an empty map and not an error, which is the same
// judgement snapshotHealth makes about every count it reports: this is a
// measurement of work in flight, not a safety claim, and the report it
// feeds must not start failing because one deployment's journal was busy.
// A journal that cannot hold snapshot runs at all returns the same empty
// map, because it has none.
func (s *Service) unfinishedSnapshotRuns(ctx context.Context) unfinishedBySet {
	catalog, ok := s.Journal.(snapshotlifecycle.Catalog)
	if !ok {
		return nil
	}

	runs, err := catalog.UnfinishedSnapshotRuns(ctx)
	if err != nil {
		return nil
	}

	out := make(unfinishedBySet, len(runs))
	for _, run := range runs {
		out[run.SetUUID]++
	}

	return out
}

// counterOf flattens a nullable catalog counter for a report that carries
// Measured beside it: see health.SnapshotHealth.Measured for why the
// distinction is kept one level up rather than by rendering a zero.
func counterOf(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}
