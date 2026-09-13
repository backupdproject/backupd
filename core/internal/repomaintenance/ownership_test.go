package repomaintenance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/repomaintenance"
)

// One repository, one maintenance owner, and the only way that changes is
// somebody deciding it should. These tests are the whole of issue #786's
// "full maintenance has exactly one owner" criterion, asked of two
// instances sharing one durable record rather than of one instance
// asking itself.

// fakeRepository is a repository that records what maintenance was asked
// of it and answers with whatever the test set up.
//
// It exists rather than a real repository because these tests are about
// ownership arithmetic, and a real repository would make each of them a
// several-second fixture that proves the same thing more slowly. The
// behaviour against a real repository is proved in repository_test.go.
type fakeRepository struct {
	mu    sync.Mutex
	calls []backupengine.MaintenanceMode

	// err is returned by every Maintain call.
	err error

	// ran is what a successful Maintain reports.
	ran bool

	// stats is answered in order, one per Stats call, repeating the last.
	stats []backupengine.RepositoryStats

	statsCalls int
}

func (f *fakeRepository) Maintain(_ context.Context, mode backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, mode)

	if f.err != nil {
		return backupengine.MaintenanceReport{Mode: mode}, f.err
	}

	return backupengine.MaintenanceReport{Mode: mode, Ran: f.ran}, nil
}

func (f *fakeRepository) Stats(context.Context) (backupengine.RepositoryStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.stats) == 0 {
		return backupengine.RepositoryStats{}, nil
	}

	i := min(f.statsCalls, len(f.stats)-1)
	f.statsCalls++

	return f.stats[i], nil
}

func (f *fakeRepository) maintained() []backupengine.MaintenanceMode {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]backupengine.MaintenanceMode(nil), f.calls...)
}

// testStore is a file-backed ownership store under a temporary directory,
// which is what a deployment really uses.
func testStore(t *testing.T) backupengine.MaintenanceOwnershipStore {
	t.Helper()

	store, err := backupengine.NewFileMaintenanceOwnershipStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}

	return store
}

func testRunner(owner string, store backupengine.MaintenanceOwnershipStore, fence *repomaintenance.Fence) repomaintenance.Runner {
	return repomaintenance.Runner{
		Owner: backupengine.MaintenanceOwner(owner),
		Store: store,
		Fence: fence,
	}
}

// TestASecondInstanceCannotMaintainARepositoryItDoesNotOwn is the
// single-owner criterion. The assertion that matters is not the error: it
// is that the repository was never asked to do anything.
func TestASecondInstanceCannotMaintainARepositoryItDoesNotOwn(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	fence := repomaintenance.NewFence()
	domain := testDomain(t, "shared-domain")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	first := testRunner("instance-a", store, fence)
	second := testRunner("instance-b", store, fence)

	repo := &fakeRepository{ran: true}

	if _, err := first.Run(ctx, domain, repo, backupengine.MaintenanceFull, now); err != nil {
		t.Fatalf("the first instance could not maintain an unowned repository: %v", err)
	}

	_, err := second.Run(ctx, domain, repo, backupengine.MaintenanceFull, now.Add(time.Hour))
	if !errors.Is(err, repomaintenance.ErrNotOwner) {
		t.Fatalf("the second instance's maintenance returned %v, want ErrNotOwner", err)
	}

	if got := repo.maintained(); len(got) != 1 {
		t.Errorf("the repository was maintained %d times (%v); a repository has exactly one maintenance owner", len(got), got)
	}

	record, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if record.Owner != "instance-a" {
		t.Errorf("the record names %q as the owner, want instance-a; a refused run must not rewrite the claim", record.Owner)
	}
}

// TestOwnershipMovesOnlyByAnExplicitTransfer is the transition half: an
// administrator moves the claim, and only then does the other instance
// maintain the repository.
func TestOwnershipMovesOnlyByAnExplicitTransfer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	fence := repomaintenance.NewFence()
	domain := testDomain(t, "handover-domain")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	first := testRunner("instance-a", store, fence)
	second := testRunner("instance-b", store, fence)

	repo := &fakeRepository{ran: true}

	if _, err := first.Run(ctx, domain, repo, backupengine.MaintenanceQuick, now); err != nil {
		t.Fatalf("the first instance's maintenance: %v", err)
	}

	// A transfer that names the wrong current owner is refused, because
	// the whole point of naming it is that the administrator is stating
	// what they believe and a stale belief is how two instances end up
	// both thinking they own the repository.
	if _, err := repomaintenance.Transfer(ctx, store, domain, "instance-c", "instance-b"); !errors.Is(err, repomaintenance.ErrOwnershipMoved) {
		t.Fatalf("a transfer from the wrong owner returned %v, want ErrOwnershipMoved", err)
	}

	moved, err := repomaintenance.Transfer(ctx, store, domain, "instance-a", "instance-b")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	if moved.Owner != "instance-b" {
		t.Fatalf("after the transfer the owner is %q, want instance-b", moved.Owner)
	}

	// The record is the same repository's record: a handover is not a
	// reset, and losing when maintenance last ran would make the new
	// owner run a full cycle it does not owe.
	if !moved.LastQuick.Equal(now) {
		t.Errorf("the transfer moved the claim and lost the history: LastQuick is %v, want %v", moved.LastQuick, now)
	}

	if _, err := second.Run(ctx, domain, repo, backupengine.MaintenanceFull, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("the new owner could not maintain the repository: %v", err)
	}

	if _, err := first.Run(ctx, domain, repo, backupengine.MaintenanceFull, now.Add(3*time.Hour)); !errors.Is(err, repomaintenance.ErrNotOwner) {
		t.Fatalf("the previous owner's maintenance returned %v, want ErrNotOwner", err)
	}
}

// TestTransferringARepositoryNobodyOwnsIsRefused: there is nothing to
// move, and inventing a claim here would let a typo in an administrative
// command create an owner for a repository this instance has never even
// opened.
func TestTransferringARepositoryNobodyOwnsIsRefused(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	domain := testDomain(t, "unclaimed-domain")

	_, err := repomaintenance.Transfer(ctx, store, domain, "instance-a", "instance-b")
	if !errors.Is(err, backupengine.ErrNoMaintenanceOwnership) {
		t.Fatalf("transferring an unowned repository returned %v, want ErrNoMaintenanceOwnership", err)
	}
}

// TestClaimingIsIdempotentForTheOwner: an owner that comes back after a
// restart reads its own claim rather than writing a new one, and the
// record it reads still says when maintenance last ran.
func TestClaimingIsIdempotentForTheOwner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := testStore(t)
	domain := testDomain(t, "restarted-owner")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	runner := testRunner("instance-a", store, repomaintenance.NewFence())

	if _, err := runner.Run(ctx, domain, &fakeRepository{ran: true}, backupengine.MaintenanceFull, now); err != nil {
		t.Fatalf("Run: %v", err)
	}

	claimed, err := runner.Claim(ctx, domain)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if !claimed.LastFull.Equal(now) {
		t.Errorf("a re-claim reports LastFull %v, want %v: claiming what you already own must not erase it", claimed.LastFull, now)
	}
}

// TestAnUnreadableRecordIsNeverTreatedAsUnowned: a corrupt record is the
// one case where "claim it" is the wrong answer, because the repository
// it describes may be being maintained by somebody else right now.
func TestAnUnreadableRecordIsNeverTreatedAsUnowned(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	domain := testDomain(t, "corrupt-record")

	dir := filepath.Join(t.TempDir(), "state")
	store, err := backupengine.NewFileMaintenanceOwnershipStore(dir)
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}

	// What a truncated write leaves behind. Valid JSON is not required:
	// this is the file a crash mid-write produces, at the revision a
	// reader would look for it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the state directory: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, domain.String()+".maintenance.1.json"), []byte(`{"domain":"`), 0o600); err != nil {
		t.Fatalf("writing the corrupt record: %v", err)
	}

	repo := &fakeRepository{ran: true}

	if _, err := testRunner("instance-a", store, repomaintenance.NewFence()).
		Run(ctx, domain, repo, backupengine.MaintenanceFull, time.Now()); err == nil {
		t.Fatal("maintenance ran against a repository whose ownership record could not be read")
	}

	if got := repo.maintained(); len(got) != 0 {
		t.Errorf("the repository was maintained %v despite an unreadable ownership record", got)
	}
}
