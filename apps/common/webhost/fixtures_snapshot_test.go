package webhost

import (
	"context"
	"sync"

	"github.com/backupdproject/backupd/core/service"
)

// The BackupServiceClient doubles for EPIC K's surface (#788), in their
// own file so the two big fixtures next door stay about what they were
// already about.
//
// The state is package-level per fake instance rather than fields on the
// two structs, for one reason: those structs live in fixtures_test.go and
// growing them for a feature is how that file became the size it is. A
// snapshot fixture is looked up by the fake's own pointer, so two fakes
// in one test never see each other's rows.

// snapshotFixture is everything the snapshot and repository surface
// answers with, for one fake backend.
type snapshotFixture struct {
	snapshots   []service.Snapshot
	detail      service.SnapshotDetail
	holds       []service.SnapshotHold
	retention   service.SnapshotRetentionPreview
	repos       service.RepositoryHealthReport
	maintenance service.RepositoryMaintenance

	// err, when set, is what every read in this fixture returns. One
	// field rather than one per read: the tests that use it are about the
	// mapping from a service refusal to a status code, and which read
	// produced it is not part of that question.
	err error

	// The last request each submission was handed, so a test can prove
	// which fields crossed the handler boundary rather than only that a
	// 202 came back.
	lastVerify      service.SnapshotVerifyRequest
	lastHold        service.SnapshotHoldRequest
	lastHoldRelease service.SnapshotHoldReleaseRequest
	lastRestore     service.SnapshotRestoreRequest

	// errOnSubmit is what every submission returns, for the same reason
	// err above is one field.
	errOnSubmit error

	// lastReadSet and lastReadRun are the identity the last read was
	// driven at. They are what proves a path built from the contract's
	// own template reached the resource it names, rather than merely
	// answering 200 from somewhere.
	lastReadSet string
	lastReadRun string
}

var (
	snapshotFixturesMu sync.Mutex
	snapshotFixtures   = map[any]*snapshotFixture{}
)

// snapshotsOf is one fake's fixture, created on first use so no fixture
// constructor has to know this file exists.
func snapshotsOf(fake any) *snapshotFixture {
	snapshotFixturesMu.Lock()
	defer snapshotFixturesMu.Unlock()

	f, ok := snapshotFixtures[fake]
	if !ok {
		f = &snapshotFixture{}
		snapshotFixtures[fake] = f
	}

	return f
}

func (f *syncFakeBackend) ListSnapshots(_ context.Context, id string) ([]service.Snapshot, error) {
	fx := snapshotsOf(f)
	fx.lastReadSet = id

	return fx.snapshots, fx.err
}

func (f *syncFakeBackend) GetSnapshot(_ context.Context, id, runID string) (service.SnapshotDetail, error) {
	fx := snapshotsOf(f)
	fx.lastReadSet, fx.lastReadRun = id, runID

	return fx.detail, fx.err
}

func (f *syncFakeBackend) ListSnapshotHolds(context.Context, string) ([]service.SnapshotHold, error) {
	fx := snapshotsOf(f)

	return fx.holds, fx.err
}

func (f *syncFakeBackend) SnapshotRetention(context.Context, string) (service.SnapshotRetentionPreview, error) {
	fx := snapshotsOf(f)

	return fx.retention, fx.err
}

func (f *syncFakeBackend) ListRepositories(context.Context) (service.RepositoryHealthReport, error) {
	fx := snapshotsOf(f)

	return fx.repos, fx.err
}

func (f *syncFakeBackend) RepositoryMaintenanceState(context.Context, string) (service.RepositoryMaintenance, error) {
	fx := snapshotsOf(f)

	return fx.maintenance, fx.err
}

func (f *syncFakeBackend) SubmitSnapshotRestore(_ context.Context, req service.SnapshotRestoreRequest) (service.Operation, error) {
	fx := snapshotsOf(f)
	fx.lastRestore = req

	return fakeSubmission(service.ActionRestoreSnapshot, req.BackupSetID, req.IdempotencyKey), fx.errOnSubmit
}

func (f *syncFakeBackend) SubmitSnapshotVerify(_ context.Context, req service.SnapshotVerifyRequest) (service.Operation, error) {
	fx := snapshotsOf(f)
	fx.lastVerify = req

	return fakeSubmission(service.ActionVerifySnapshot, req.BackupSetID, req.IdempotencyKey), fx.errOnSubmit
}

func (f *syncFakeBackend) SubmitSnapshotHold(_ context.Context, req service.SnapshotHoldRequest) (service.Operation, error) {
	fx := snapshotsOf(f)
	fx.lastHold = req

	return fakeSubmission(service.ActionHoldSnapshot, req.BackupSetID, req.IdempotencyKey), fx.errOnSubmit
}

func (f *syncFakeBackend) SubmitSnapshotHoldRelease(_ context.Context, req service.SnapshotHoldReleaseRequest) (service.Operation, error) {
	fx := snapshotsOf(f)
	fx.lastHoldRelease = req

	return fakeSubmission(service.ActionReleaseSnapshotHold, req.BackupSetID, req.IdempotencyKey), fx.errOnSubmit
}

// fakeSubmission is the operation row a submission answers with. It is
// deliberately minimal: the handler tests here are about the mapping, and
// what a real operation carries is core/service's own suite's question.
func fakeSubmission(action, backupSetID, key string) service.Operation {
	return service.Operation{
		ID:             "op_" + key,
		Status:         "queued",
		Action:         action,
		BackupSetID:    backupSetID,
		IdempotencyKey: key,
	}
}

func (f *asyncFakeBackend) ListSnapshots(context.Context, string) ([]service.Snapshot, error) {
	fx := snapshotsOf(f)

	return fx.snapshots, fx.err
}

func (f *asyncFakeBackend) GetSnapshot(_ context.Context, _, _ string) (service.SnapshotDetail, error) {
	fx := snapshotsOf(f)

	return fx.detail, fx.err
}

func (f *asyncFakeBackend) ListSnapshotHolds(context.Context, string) ([]service.SnapshotHold, error) {
	fx := snapshotsOf(f)

	return fx.holds, fx.err
}

func (f *asyncFakeBackend) SnapshotRetention(context.Context, string) (service.SnapshotRetentionPreview, error) {
	fx := snapshotsOf(f)

	return fx.retention, fx.err
}

func (f *asyncFakeBackend) ListRepositories(context.Context) (service.RepositoryHealthReport, error) {
	fx := snapshotsOf(f)

	return fx.repos, fx.err
}

func (f *asyncFakeBackend) RepositoryMaintenanceState(context.Context, string) (service.RepositoryMaintenance, error) {
	fx := snapshotsOf(f)

	return fx.maintenance, fx.err
}

func (f *asyncFakeBackend) SubmitSnapshotRestore(_ context.Context, req service.SnapshotRestoreRequest) (service.Operation, error) {
	return fakeSubmission(service.ActionRestoreSnapshot, req.BackupSetID, req.IdempotencyKey), snapshotsOf(f).errOnSubmit
}

func (f *asyncFakeBackend) SubmitSnapshotVerify(_ context.Context, req service.SnapshotVerifyRequest) (service.Operation, error) {
	return fakeSubmission(service.ActionVerifySnapshot, req.BackupSetID, req.IdempotencyKey), snapshotsOf(f).errOnSubmit
}

func (f *asyncFakeBackend) SubmitSnapshotHold(_ context.Context, req service.SnapshotHoldRequest) (service.Operation, error) {
	return fakeSubmission(service.ActionHoldSnapshot, req.BackupSetID, req.IdempotencyKey), snapshotsOf(f).errOnSubmit
}

func (f *asyncFakeBackend) SubmitSnapshotHoldRelease(_ context.Context, req service.SnapshotHoldReleaseRequest) (service.Operation, error) {
	return fakeSubmission(service.ActionReleaseSnapshotHold, req.BackupSetID, req.IdempotencyKey), snapshotsOf(f).errOnSubmit
}
