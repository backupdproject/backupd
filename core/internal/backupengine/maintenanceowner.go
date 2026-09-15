package backupengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/retnd/retnd/core/internal/model"
)

// This file is the record maintenance scheduling will be decided from
// (#786), and nothing that decides anything.
//
// # Why it lands now, empty of policy
//
// A repository has exactly one maintenance owner. That is not a design
// choice this project gets to make: quick and full maintenance rewrite
// index blobs and reclaim content, two processes doing it concurrently
// against one repository is how a repository loses content it still
// references, and the embedded engine's own answer -- an owner string in
// the repository, checked by default -- is one the adapter deliberately
// overrides (see the adapter's Maintain), because deferring to whichever
// machine happened to create the repository means a maintenance window
// that silently never runs.
//
// Having overridden it, this product owes the same guarantee from its own
// side, and that guarantee needs a durable record: who owns maintenance
// for this repository, when quick and full last ran, when the next one is
// allowed, and how the last one ended. Writing that record down is
// separable from deciding anything with it, and separating them is what
// lets the repository adapter land complete: #786 adds the scheduler, the
// capacity checks and the ownership handover, and it adds them against a
// record that already exists, is already persisted beside the repository's
// other local state, and is already proven to survive a restart.
//
// What is deliberately absent: any method that decides whether
// maintenance may run now. NextEligible is a recorded fact, not an
// enforced one, and nothing here reads the clock to compare against it. A
// half-made scheduling decision in this file would be a second scheduler
// for #786 to contend with rather than a foundation to build on.

// ErrNoMaintenanceOwnership is returned by a store that holds no record
// for a repository, which is the ordinary state of one that has never been
// maintained.
//
// It is distinct from a read failure because the two lead somewhere
// different: no record means "nobody has owned this yet, claim it", and an
// unreadable record means "something is wrong with this manager's state
// directory", which must never be silently treated as the first.
var ErrNoMaintenanceOwnership = errors.New("backupengine: no maintenance ownership record for this repository")

// ErrMaintenanceOwnershipExists is returned by Create when the
// repository already has a record, which is how the loser of a race to
// claim an unclaimed repository finds out it lost.
//
// Create exists as a separate operation from Save for exactly this: a
// caller that reads "no record" and then writes one is two operations,
// and two instances running them at once both see no record and both
// write themselves in as owner. A create that fails when the record
// already exists turns that into one winner and one refusal.
var ErrMaintenanceOwnershipExists = errors.New("backupengine: this repository already has a maintenance ownership record")

// ErrMaintenanceOwnershipStale is returned by CompareAndSwap when the
// stored record has moved on from the revision the caller read.
//
// It is the other half of the same problem, and the half that matters
// after a handover: an instance that loaded a record, spent ten minutes
// maintaining a repository and then wrote its outcome back would
// otherwise overwrite -- and silently revert -- a Transfer that happened
// while it worked, or clobber another window's history.
var ErrMaintenanceOwnershipStale = errors.New("backupengine: this repository's maintenance ownership record changed since it was read")

// MaintenanceOwner identifies the backupd instance that owns maintenance
// for one repository.
//
// It is a free-form string rather than a validated type because what
// usefully identifies an instance differs per deployment -- a hostname, a
// container id, an operator-assigned name -- and this record's job is to
// let two instances notice they disagree, which any stable distinct string
// achieves. The one rule is that it must be stable across restarts of the
// same instance, or every restart looks like a new owner.
type MaintenanceOwner string

func (o MaintenanceOwner) String() string { return string(o) }

// IsZero reports whether no owner is recorded.
func (o MaintenanceOwner) IsZero() bool { return o == "" }

// MaintenanceOutcome is how one maintenance run ended.
//
// Ran and Err are separate because "declined to do anything" is a normal,
// successful outcome (nothing was due, or another process held the lock)
// and recording it as a failure would have an operator investigating a
// healthy repository. Err is a string rather than an error because this
// record is persisted: an error's identity does not survive being written
// to a file, and pretending otherwise invites a reader to route on
// something that is only ever text by the time they see it.
type MaintenanceOutcome struct {
	// At is when the run finished.
	At time.Time `json:"at"`

	// Mode is which maintenance was attempted.
	Mode MaintenanceMode `json:"mode"`

	// Ran is whether it actually did work.
	Ran bool `json:"ran"`

	// Err is the failure, rendered, or empty when it succeeded.
	Err string `json:"error,omitempty"`

	// Reclaimed is the change in the repository's physical size across
	// this run, in bytes: positive when the run freed storage.
	//
	// It is a MEASUREMENT taken either side of the run and not a claim
	// about causation, which is why it can be negative. A full
	// maintenance that consolidated short packs writes new blobs before
	// it deletes the old ones, and a backup that ran during the window
	// added content of its own; reporting only the positive half would
	// tell an operator maintenance never costs anything.
	//
	// Zero also means "not measured": quick maintenance does not reclaim
	// blobs and does not pay for the storage listing that would measure
	// it. See repomaintenance.Runner.
	Reclaimed int64 `json:"reclaimed_bytes,omitempty"`
}

// MaintenanceOwnership is the durable record of one repository's
// maintenance state.
type MaintenanceOwnership struct {
	// Domain is which repository this is about. It is the repository's
	// stable id rather than a storage path for RepositoryLocation's
	// reason: a record keyed by a path stops matching the first time a
	// mount point moves.
	Domain model.RepositoryDomainID `json:"domain"`

	// Owner is the instance that owns maintenance for it.
	Owner MaintenanceOwner `json:"owner"`

	// LastQuick and LastFull are when each mode last ran. Zero means
	// never, which is a fact and not a missing value: a repository that
	// has never had a full maintenance is exactly what an operator needs
	// to be told about.
	LastQuick time.Time `json:"last_quick,omitempty"`
	LastFull  time.Time `json:"last_full,omitempty"`

	// NextEligible is the earliest time the owner intends to run
	// maintenance again. It is recorded, never enforced here; see this
	// file's header.
	NextEligible time.Time `json:"next_eligible,omitempty"`

	// LastResult is how the most recent attempt ended, whichever mode it
	// was.
	LastResult MaintenanceOutcome `json:"last_result,omitzero"`

	// History is the recent attempts, oldest first, INCLUDING the one
	// LastResult repeats.
	//
	// It is bounded by whoever writes it (repomaintenance's
	// DefaultHistoryLimit) because this file is rewritten after every
	// maintenance window for the life of the deployment, and an
	// unbounded list would grow until the atomic rewrite that keeps the
	// record readable is the most expensive part of a quick maintenance.
	History []MaintenanceOutcome `json:"history,omitempty"`

	// Runs and Failures count every attempt this record has ever
	// described, not the ones History still holds.
	//
	// They are durable counters rather than something derived from
	// History for exactly that reason: a bounded list cannot answer "how
	// often has this repository's maintenance failed", and a surface that
	// counted the retained entries would report a number that silently
	// shrinks as the history rolls over.
	Runs     int `json:"runs,omitempty"`
	Failures int `json:"failures,omitempty"`

	// ReclaimedBytes is the sum of every run's measured change, over the
	// life of the record. See MaintenanceOutcome.Reclaimed for what one
	// measurement means and why it can be negative.
	ReclaimedBytes int64 `json:"reclaimed_bytes,omitempty"`

	// Revision is which version of this record the store handed out, and
	// it is the store's value rather than the record's: it is assigned by
	// whatever persists the record and it is the token a
	// CompareAndSwap is checked against. Zero means "not loaded from a
	// store", which is why CompareAndSwap refuses it.
	//
	// It is deliberately not serialised. A revision written inside the
	// record it describes is a fact that can disagree with where the
	// record actually is, and the store is the only thing that can know
	// the answer; FileMaintenanceOwnershipStore keeps it in the
	// filename, a sqlite-backed one would keep it in a column.
	Revision uint64 `json:"-"`
}

// Validate refuses a record that could not be matched back to a
// repository or an owner, which are the two halves that make it a claim
// rather than a note.
func (m MaintenanceOwnership) Validate() error {
	if m.Domain.IsZero() {
		return errors.New("backupengine: a maintenance ownership record must name its repository domain")
	}

	if m.Owner.IsZero() {
		return errors.New("backupengine: a maintenance ownership record must name its owner; an unowned claim is not a claim")
	}

	return nil
}

// MaintenanceOwnershipStore persists maintenance ownership records.
//
// It is an interface with one implementation because #786 will need a
// second one in its tests, and because the storage choice here (a file
// beside the repository's other local state) is the kind of decision a
// later requirement -- ownership visible to another instance, which a
// local file cannot provide -- may have to change without every caller
// noticing.
type MaintenanceOwnershipStore interface {
	// Load returns the newest record for one repository with its
	// Revision set, or ErrNoMaintenanceOwnership.
	Load(ctx context.Context, domain model.RepositoryDomainID) (MaintenanceOwnership, error)

	// Create writes the first record for a repository that has none,
	// atomically, and returns it with the revision it was stored at. It
	// returns ErrMaintenanceOwnershipExists if the repository already has
	// a record -- including one written between this caller's Load and
	// this call, which is the race it exists to decide.
	//
	// "Atomically" is a requirement on the implementation and not a hope:
	// exactly one of any number of concurrent Creates for one repository
	// may succeed.
	Create(ctx context.Context, record MaintenanceOwnership) (MaintenanceOwnership, error)

	// CompareAndSwap replaces the record if, and only if, the stored one
	// is still at record.Revision, and returns the record at its new
	// revision. It returns ErrMaintenanceOwnershipStale if it is not, and
	// ErrNoMaintenanceOwnership if the record has gone.
	//
	// There is no unconditional write in this interface on purpose. Every
	// update to this record is a read-modify-write by something that took
	// minutes over the modify, and an unconditional Save is the one
	// operation that can undo somebody else's completed work.
	CompareAndSwap(ctx context.Context, record MaintenanceOwnership) (MaintenanceOwnership, error)
}

// FileMaintenanceOwnershipStore keeps one JSON file per revision of one
// repository's record, under a directory this manager owns.
//
// A file rather than the state journal, and that is a decision worth
// stating: the journal is the artifact catalog, its schema is migrated,
// and a repository's maintenance state is not artifact state. A file under
// the same state directory that already holds the repository's connection
// config and index cache keeps everything about one repository's local
// state in one place, and makes "delete this repository's local state"
// something an operator can do with a directory.
//
// # How the revision is stored, and why it is the filename
//
// A record lives at <domain>.maintenance.<revision>.json, and the record
// IS the highest revision present. That is what makes Create and
// CompareAndSwap atomic without a lock of any kind: publishing revision N
// is os.Link(2) of a fully written temporary file onto the name for N,
// which the kernel either performs or fails with EEXIST, so exactly one
// of any number of concurrent writers can take a revision. A lock file
// would have needed a story about the holder dying; a revision nobody can
// take twice needs none.
//
// The content is complete before the link, so a crash can leave a
// half-written temporary file but never a half-written record: the name a
// reader looks for only comes into existence once the bytes behind it are
// all there. Superseded revisions are removed after a successful publish,
// which is best-effort tidying and never load-bearing -- a leftover lower
// revision is invisible to Load, because Load reads the highest.
//
// It requires a filesystem that supports hard links. That is the
// manager's own local state directory, which is also where the index
// cache and the connection config live.
type FileMaintenanceOwnershipStore struct {
	// dir holds one file per revision per repository.
	dir string
}

var _ MaintenanceOwnershipStore = FileMaintenanceOwnershipStore{}

// NewFileMaintenanceOwnershipStore returns a store writing under dir,
// which must be absolute: a relative state directory is a different
// directory per working directory, and the failure shows up as a
// repository that has forgotten it was ever maintained.
func NewFileMaintenanceOwnershipStore(dir string) (FileMaintenanceOwnershipStore, error) {
	if !filepath.IsAbs(dir) {
		return FileMaintenanceOwnershipStore{}, fmt.Errorf("backupengine: maintenance ownership directory %q is relative", dir)
	}

	return FileMaintenanceOwnershipStore{dir: filepath.Clean(dir)}, nil
}

// maintenanceRecordInfix separates a record's domain from its revision.
// It begins with a dot so that one domain's records can never be mistaken
// for another's by prefix (see the store's path).
const maintenanceRecordInfix = ".maintenance."

// maintenanceRecordSuffix is what a record file ends with.
const maintenanceRecordSuffix = ".json"

// firstMaintenanceRevision is what Create publishes. Revisions start at 1
// so that zero can mean "this record did not come from a store", which is
// what CompareAndSwap refuses.
const firstMaintenanceRevision uint64 = 1

// loadAttempts bounds Load's re-read of a record that was superseded
// between choosing the newest revision and reading it. More than one
// because a concurrent publish can remove the file this call had just
// decided to read; a bound because "the record keeps moving" must not be
// an infinite loop.
const loadAttempts = 4

// path is where one revision of one repository's record lives. The domain
// id is safe as a filename for ReservedLocalDir's reason:
// model.NewRepositoryDomainID already refuses everything that would make
// it unsafe.
func (s FileMaintenanceOwnershipStore) path(domain model.RepositoryDomainID, revision uint64) string {
	return filepath.Join(s.dir,
		domain.String()+maintenanceRecordInfix+strconv.FormatUint(revision, 10)+maintenanceRecordSuffix)
}

// newest is the revision one repository's record is at, or
// ErrNoMaintenanceOwnership when it has none.
func (s FileMaintenanceOwnershipStore) newest(domain model.RepositoryDomainID) (uint64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNoMaintenanceOwnership
		}

		return 0, fmt.Errorf("backupengine: reading the maintenance ownership directory: %w", err)
	}

	prefix := domain.String() + maintenanceRecordInfix

	var newest uint64

	for _, entry := range entries {
		revision, ok := revisionOf(entry.Name(), prefix)
		if ok && revision > newest {
			newest = revision
		}
	}

	if newest == 0 {
		return 0, ErrNoMaintenanceOwnership
	}

	return newest, nil
}

// revisionOf reads the revision out of a record's filename. Anything that
// is not one of this domain's records -- another domain's record, a
// temporary file, an operator's backup copy -- is not one, rather than an
// error: this directory belongs to the manager, but a directory is
// something people put files in.
func revisionOf(name, prefix string) (uint64, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, maintenanceRecordSuffix) {
		return 0, false
	}

	digits := name[len(prefix) : len(name)-len(maintenanceRecordSuffix)]

	revision, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || revision == 0 {
		return 0, false
	}

	return revision, true
}

// Load implements MaintenanceOwnershipStore.
func (s FileMaintenanceOwnershipStore) Load(_ context.Context, domain model.RepositoryDomainID) (MaintenanceOwnership, error) {
	if domain.IsZero() {
		return MaintenanceOwnership{}, errors.New("backupengine: cannot load a maintenance ownership record without a repository domain")
	}

	for range loadAttempts {
		revision, err := s.newest(domain)
		if err != nil {
			return MaintenanceOwnership{}, err
		}

		record, err := s.read(domain, revision)
		if errors.Is(err, os.ErrNotExist) {
			// Superseded while this call was deciding what to read. The
			// record exists; it is simply further on than a moment ago.
			continue
		}

		return record, err
	}

	return MaintenanceOwnership{}, fmt.Errorf("backupengine: maintenance ownership for %s is being rewritten faster than it can be read", domain)
}

// read is one revision, with its revision filled in from where it was
// found rather than from what it says.
func (s FileMaintenanceOwnershipStore) read(domain model.RepositoryDomainID, revision uint64) (MaintenanceOwnership, error) {
	path := s.path(domain, revision)

	raw, err := os.ReadFile(path)
	if err != nil {
		// Wrapped, so that Load can still recognise the one case it
		// retries -- a revision tidied away under it -- by os.ErrNotExist.
		return MaintenanceOwnership{}, fmt.Errorf("backupengine: reading maintenance ownership for %s: %w", domain, err)
	}

	var record MaintenanceOwnership
	if err := json.Unmarshal(raw, &record); err != nil {
		// Not ErrNoMaintenanceOwnership: a corrupt record must never read
		// as an unowned repository, because the answer to unowned is
		// "claim it" and claiming a repository somebody else is
		// maintaining is the thing this record exists to prevent.
		return MaintenanceOwnership{}, fmt.Errorf("backupengine: maintenance ownership record for %s is unreadable: %w", domain, err)
	}

	if record.Domain != domain {
		return MaintenanceOwnership{}, fmt.Errorf(
			"backupengine: maintenance ownership record at %s is for repository %q, not %q",
			path, record.Domain, domain)
	}

	record.Revision = revision

	return record, nil
}

// Create implements MaintenanceOwnershipStore.
//
// The exclusive link on revision 1 is what decides a tie between two
// creates, but it cannot be the whole answer: a record that has moved on
// has had revision 1 tidied away, and a link onto a name nobody holds
// would succeed. So the newest revision is checked either side of it --
// before, so an existing record is refused outright, and after, because
// a revision below the newest one is invisible to Load and publishing it
// would be reporting a claim that nothing can read.
func (s FileMaintenanceOwnershipStore) Create(_ context.Context, record MaintenanceOwnership) (MaintenanceOwnership, error) {
	if err := record.Validate(); err != nil {
		return MaintenanceOwnership{}, err
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return MaintenanceOwnership{}, fmt.Errorf("backupengine: creating maintenance ownership directory: %w", err)
	}

	switch _, err := s.newest(record.Domain); {
	case err == nil:
		return MaintenanceOwnership{}, fmt.Errorf("%w: %s", ErrMaintenanceOwnershipExists, record.Domain)
	case !errors.Is(err, ErrNoMaintenanceOwnership):
		return MaintenanceOwnership{}, err
	}

	stored, err := s.publish(record, firstMaintenanceRevision)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return MaintenanceOwnership{}, fmt.Errorf("%w: %s", ErrMaintenanceOwnershipExists, record.Domain)
		}

		return MaintenanceOwnership{}, err
	}

	if newest, err := s.newest(record.Domain); err == nil && newest > firstMaintenanceRevision {
		// Somebody else's record got there first and has already moved
		// past the revision this one took. Withdraw it: leaving a
		// revision below the newest behind would be a claim no reader can
		// see, and reporting it as created would be this store agreeing
		// with two owners.
		os.Remove(s.path(record.Domain, firstMaintenanceRevision)) //nolint:errcheck // the record below the newest is unreadable either way

		return MaintenanceOwnership{}, fmt.Errorf("%w: %s", ErrMaintenanceOwnershipExists, record.Domain)
	}

	return stored, nil
}

// CompareAndSwap implements MaintenanceOwnershipStore.
func (s FileMaintenanceOwnershipStore) CompareAndSwap(_ context.Context, record MaintenanceOwnership) (MaintenanceOwnership, error) {
	if err := record.Validate(); err != nil {
		return MaintenanceOwnership{}, err
	}

	if record.Revision == 0 {
		return MaintenanceOwnership{}, fmt.Errorf(
			"backupengine: the maintenance ownership record for %s carries no revision, so it cannot be compared against the stored one; it was not loaded from a store",
			record.Domain)
	}

	// The check is what refuses a record whose revision is behind by more
	// than one, which the link below cannot see: the revision it would
	// take may have been tidied away. The link is what decides a race
	// between two writers at the SAME revision, and that ordering is the
	// whole safety -- the check only ever refuses, and the atomic
	// operation is the only thing that admits.
	newest, err := s.newest(record.Domain)
	if err != nil {
		return MaintenanceOwnership{}, err
	}

	if newest != record.Revision {
		return MaintenanceOwnership{}, fmt.Errorf("%w: %s is at revision %d, this record was read at revision %d",
			ErrMaintenanceOwnershipStale, record.Domain, newest, record.Revision)
	}

	stored, err := s.publish(record, record.Revision+1)
	if errors.Is(err, os.ErrExist) {
		return MaintenanceOwnership{}, fmt.Errorf("%w: another writer took revision %d of %s",
			ErrMaintenanceOwnershipStale, record.Revision+1, record.Domain)
	}

	return stored, err
}

// publish writes record as the given revision, and is where this store's
// atomicity is. It returns an error satisfying errors.Is(err,
// os.ErrExist) -- and writes nothing a reader can see -- when that
// revision has already been taken.
func (s FileMaintenanceOwnershipStore) publish(record MaintenanceOwnership, revision uint64) (MaintenanceOwnership, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return MaintenanceOwnership{}, fmt.Errorf("backupengine: encoding maintenance ownership for %s: %w", record.Domain, err)
	}

	tmp, err := os.CreateTemp(s.dir, "."+record.Domain.String()+".maintenance-*")
	if err != nil {
		return MaintenanceOwnership{}, fmt.Errorf("backupengine: creating a temporary maintenance ownership file: %w", err)
	}

	name := tmp.Name()

	defer os.Remove(name) //nolint:errcheck // best effort; the link below is what matters

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close() //nolint:errcheck // already failing

		return MaintenanceOwnership{}, fmt.Errorf("backupengine: writing maintenance ownership for %s: %w", record.Domain, err)
	}

	if err := tmp.Close(); err != nil {
		return MaintenanceOwnership{}, fmt.Errorf("backupengine: closing maintenance ownership for %s: %w", record.Domain, err)
	}

	if err := os.Link(name, s.path(record.Domain, revision)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return MaintenanceOwnership{}, err
		}

		return MaintenanceOwnership{}, fmt.Errorf("backupengine: installing revision %d of maintenance ownership for %s: %w",
			revision, record.Domain, err)
	}

	s.forget(record.Domain, revision)

	record.Revision = revision

	return record, nil
}

// forget removes the revisions a newly published one supersedes, so that
// a directory rewritten after every maintenance window for the life of a
// deployment does not accumulate one file per window.
//
// Every failure here is ignored on purpose: a revision that could not be
// removed is a file nothing reads, and refusing to report a completed
// maintenance window because its predecessor's file is still there would
// turn tidying into an outage.
func (s FileMaintenanceOwnershipStore) forget(domain model.RepositoryDomainID, published uint64) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}

	prefix := domain.String() + maintenanceRecordInfix

	for _, entry := range entries {
		revision, ok := revisionOf(entry.Name(), prefix)
		if ok && revision < published {
			os.Remove(s.path(domain, revision)) //nolint:errcheck // see the doc comment
		}
	}
}
