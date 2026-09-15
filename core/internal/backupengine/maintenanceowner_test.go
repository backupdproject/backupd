package backupengine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
)

func domainID(t *testing.T, id string) model.RepositoryDomainID {
	t.Helper()

	domain, err := model.NewRepositoryDomainID(id)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID(%q): %v", id, err)
	}

	return domain
}

// ownershipStore is a store over a fresh directory, returned with that
// directory so a test can build a second store over the same one.
func ownershipStore(t *testing.T) (backupengine.FileMaintenanceOwnershipStore, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "state")

	store, err := backupengine.NewFileMaintenanceOwnershipStore(dir)
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}

	return store, dir
}

// TestMaintenanceOwnershipSurvivesARestart is the whole reason this record
// is durable rather than in memory: the question "who owns maintenance for
// this repository" has to have the same answer after a crash as before it,
// or two instances both answer "me".
func TestMaintenanceOwnershipSurvivesARestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, dir := ownershipStore(t)
	domain := domainID(t, "production")

	quick := time.Now().UTC().Truncate(time.Second)
	full := quick.Add(-48 * time.Hour)

	want := backupengine.MaintenanceOwnership{
		Domain:       domain,
		Owner:        "nas-01",
		LastQuick:    quick,
		LastFull:     full,
		NextEligible: quick.Add(time.Hour),
		LastResult: backupengine.MaintenanceOutcome{
			At:   quick,
			Mode: backupengine.MaintenanceQuick,
			Ran:  true,
		},
	}

	if _, err := store.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A second store over the same directory is the restart: nothing is
	// carried over in memory.
	reopened, err := backupengine.NewFileMaintenanceOwnershipStore(dir)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}

	got, err := reopened.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load after a restart: %v", err)
	}

	if got.Owner != want.Owner || !got.LastQuick.Equal(want.LastQuick) || !got.LastFull.Equal(want.LastFull) {
		t.Errorf("Load returned %+v, want %+v", got, want)
	}

	if !got.NextEligible.Equal(want.NextEligible) {
		t.Errorf("NextEligible came back as %s, want %s", got.NextEligible, want.NextEligible)
	}

	if got.LastResult.Mode != want.LastResult.Mode || got.LastResult.Ran != want.LastResult.Ran {
		t.Errorf("LastResult came back as %+v, want %+v", got.LastResult, want.LastResult)
	}
}

// TestLoadDistinguishesNeverOwnedFromUnreadable is the distinction that
// keeps this record safe to act on.
//
// "No record" means nobody has owned this repository yet, and the response
// to it is to claim it. "Unreadable record" must never produce that
// answer, because the repository it describes may well be being maintained
// by the instance whose record was truncated by a crash.
func TestLoadDistinguishesNeverOwnedFromUnreadable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, dir := ownershipStore(t)
	domain := domainID(t, "production")

	if _, err := store.Load(ctx, domain); !errors.Is(err, backupengine.ErrNoMaintenanceOwnership) {
		t.Errorf("Load with no record = %v, want ErrNoMaintenanceOwnership", err)
	}

	if _, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-01"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Truncate the record the way a crash mid-write would, if the write
	// were not atomic.
	record := filepath.Join(dir, domain.String()+".maintenance.1.json")
	if err := os.WriteFile(record, []byte(`{"domain":"produ`), 0o600); err != nil {
		t.Fatalf("truncating the record: %v", err)
	}

	_, err := store.Load(ctx, domain)
	if err == nil {
		t.Fatalf("Load accepted a truncated record")
	}

	if errors.Is(err, backupengine.ErrNoMaintenanceOwnership) {
		t.Errorf("Load read a truncated record as an unowned repository: %v", err)
	}
}

// TestLoadRefusesARecordForAnotherRepository covers the one way a
// correctly-formed record can still be the wrong answer: a state directory
// copied between deployments, or a file renamed by hand.
func TestLoadRefusesARecordForAnotherRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, dir := ownershipStore(t)
	wanted := domainID(t, "production")
	other := domainID(t, "customer-a")

	if _, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: other, Owner: "nas-01"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.Rename(
		filepath.Join(dir, other.String()+".maintenance.1.json"),
		filepath.Join(dir, wanted.String()+".maintenance.1.json"),
	); err != nil {
		t.Fatalf("renaming the record: %v", err)
	}

	if _, err := store.Load(ctx, wanted); err == nil {
		t.Errorf("Load accepted a record naming another repository")
	}
}

// TestCreateRefusesAnIncompleteClaim covers both halves of what makes the
// record a claim: without a domain it cannot be matched to a repository,
// and without an owner it claims nothing.
func TestCreateRefusesAnIncompleteClaim(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := ownershipStore(t)

	for _, tc := range []struct {
		name   string
		record backupengine.MaintenanceOwnership
	}{
		{"no domain", backupengine.MaintenanceOwnership{Owner: "nas-01"}},
		{"no owner", backupengine.MaintenanceOwnership{Domain: domainID(t, "production")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := store.Create(ctx, tc.record); err == nil {
				t.Errorf("Create accepted a record with %s", tc.name)
			}
		})
	}
}

// TestStoreRefusesARelativeDirectory is about a failure with no symptom:
// a relative state directory resolves against whatever the working
// directory happens to be, so a daemon that changed directory would stop
// finding the records it wrote and quietly decide every repository was
// unowned.
func TestStoreRefusesARelativeDirectory(t *testing.T) {
	t.Parallel()

	if _, err := backupengine.NewFileMaintenanceOwnershipStore("var/lib/backupd"); err == nil {
		t.Errorf("NewFileMaintenanceOwnershipStore accepted a relative directory")
	}
}

// TestExactlyOneOfManyConcurrentCreatesWins is the atomicity this store
// owes its callers, asked the only way it can be: many writers, one fresh
// repository, all released at once.
//
// Without it, "Load says unowned, so write myself in as owner" is a race
// every instance wins, and the ownership record -- whose entire purpose is
// that one instance maintains one repository -- reports whichever owner
// wrote last while several believe it is them.
func TestExactlyOneOfManyConcurrentCreatesWins(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := ownershipStore(t)
	domain := domainID(t, "contended")

	const writers = 8

	var (
		start   = make(chan struct{})
		ready   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []backupengine.MaintenanceOwner
		refused int
		other   []error
	)

	ready.Add(writers)
	done.Add(writers)

	for i := range writers {
		owner := backupengine.MaintenanceOwner("instance-" + strconv.Itoa(i))

		go func() {
			defer done.Done()

			ready.Done()
			<-start

			_, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: owner})

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				winners = append(winners, owner)
			case errors.Is(err, backupengine.ErrMaintenanceOwnershipExists):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}

	ready.Wait()
	close(start)
	done.Wait()

	if len(other) > 0 {
		t.Fatalf("concurrent Creates failed with unexpected errors: %v", other)
	}

	if len(winners) != 1 {
		t.Fatalf("%d of %d concurrent Creates succeeded, want exactly 1: %v", len(winners), writers, winners)
	}

	if refused != writers-1 {
		t.Errorf("%d Creates were refused as already-existing, want %d", refused, writers-1)
	}

	stored, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if stored.Owner != winners[0] {
		t.Errorf("the stored owner is %q; the only Create that succeeded was %q's", stored.Owner, winners[0])
	}
}

// TestCompareAndSwapRefusesARecordReadBeforeAnotherWrite is the stale
// write, which is the failure with real consequences: an instance that
// loaded a record, spent a maintenance window working, and then wrote
// back what it had read would undo whatever happened in between --
// including an administrative handover to another instance.
func TestCompareAndSwapRefusesARecordReadBeforeAnotherWrite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := ownershipStore(t)
	domain := domainID(t, "handed-over")

	created, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-01"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stale := created

	handover := created
	handover.Owner = "nas-02"

	if _, err := store.CompareAndSwap(ctx, handover); err != nil {
		t.Fatalf("the handover: %v", err)
	}

	stale.Runs = 9

	if _, err := store.CompareAndSwap(ctx, stale); !errors.Is(err, backupengine.ErrMaintenanceOwnershipStale) {
		t.Fatalf("the stale write returned %v, want ErrMaintenanceOwnershipStale", err)
	}

	got, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Owner != "nas-02" || got.Runs != 0 {
		t.Errorf("the record is %q with %d runs; the refused write got in anyway", got.Owner, got.Runs)
	}
}

// TestCompareAndSwapRefusesARecordWithNoRevision covers the mistake that
// would quietly turn every conditional write back into an unconditional
// one: a record built in memory rather than loaded, whose revision is
// zero and which therefore matches nothing.
func TestCompareAndSwapRefusesARecordWithNoRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := ownershipStore(t)
	domain := domainID(t, "production")

	if _, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-01"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.CompareAndSwap(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-02"}); err == nil {
		t.Error("CompareAndSwap accepted a record that was never loaded from a store")
	}
}

// TestExactlyOneOfManyConcurrentCompareAndSwapsWins is the other half of
// the primitive: several writers that all read the same revision, all
// released at once, and one revision for them to compete over.
func TestExactlyOneOfManyConcurrentCompareAndSwapsWins(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := ownershipStore(t)
	domain := domainID(t, "contended")

	created, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-01"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const writers = 8

	var (
		start   = make(chan struct{})
		ready   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []int
		stale   int
		other   []error
	)

	ready.Add(writers)
	done.Add(writers)

	for i := range writers {
		attempt := created
		attempt.Runs = i + 1

		go func() {
			defer done.Done()

			ready.Done()
			<-start

			_, err := store.CompareAndSwap(ctx, attempt)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				winners = append(winners, attempt.Runs)
			case errors.Is(err, backupengine.ErrMaintenanceOwnershipStale):
				stale++
			default:
				other = append(other, err)
			}
		}()
	}

	ready.Wait()
	close(start)
	done.Wait()

	if len(other) > 0 {
		t.Fatalf("concurrent compare-and-sets failed with unexpected errors: %v", other)
	}

	if len(winners) != 1 {
		t.Fatalf("%d of %d compare-and-sets at revision %d succeeded, want exactly 1", len(winners), writers, created.Revision)
	}

	if stale != writers-1 {
		t.Errorf("%d compare-and-sets were refused as stale, want %d", stale, writers-1)
	}

	got, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Runs != winners[0] {
		t.Errorf("the stored record holds %d runs, want the winner's %d: a losing write reached the record", got.Runs, winners[0])
	}

	if got.Revision == created.Revision {
		t.Errorf("the record is still at revision %d after a successful compare-and-set", got.Revision)
	}
}

// TestCreateRefusesARecordThatHasMovedPastItsFirstRevision is a
// regression, and the bug it pins was invisible from the outside.
//
// A record that has been updated no longer has its first revision on
// disk: the update tidies it away. A create that only asked "can I take
// the first revision" would then succeed against a repository that
// already has an owner, hand its caller a claim that no reader can see,
// and leave two instances each believing they own maintenance.
func TestCreateRefusesARecordThatHasMovedPastItsFirstRevision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := ownershipStore(t)
	domain := domainID(t, "already-owned")

	created, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-01"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// One ordinary update, which is what removes the first revision.
	updated := created
	updated.Runs = 1

	if _, err := store.CompareAndSwap(ctx, updated); err != nil {
		t.Fatalf("CompareAndSwap: %v", err)
	}

	if _, err := store.Create(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-02"}); !errors.Is(err, backupengine.ErrMaintenanceOwnershipExists) {
		t.Fatalf("Create against an owned repository returned %v, want ErrMaintenanceOwnershipExists", err)
	}

	got, err := store.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Owner != "nas-01" {
		t.Errorf("the record names %q as owner; a refused Create took the repository from nas-01", got.Owner)
	}
}
