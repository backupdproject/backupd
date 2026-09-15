// Package backupengine is the manager-owned boundary around whatever produces
// incremental, deduplicated, content-addressed backups.
//
// It exists for the same reason internal/transport exists: the data-plane
// implementation is somebody else's code on somebody else's release cadence,
// and the only way embedding stays cheaper than a fork is if every import of
// it lives in exactly one adapter package. Every import of the embedded
// engine in this repository lives in this package's single adapter
// subpackage. Nothing else may import it, and no type it defines may appear
// in any signature in this file, which is checkable and therefore checked:
// boundary_test.go greps this file for the vendor's name and fails if it
// finds one, because a convention nothing checks is a convention that lasts
// one busy afternoon.
//
// Note what is absent.
//
// There is no IncrementalSnapshot. Incrementality is not a mode the caller
// selects, it is what Snapshot does when the repository already holds a
// previous snapshot of the same Source: the engine finds the predecessor
// itself and reuses whatever content still matches. A second method, or a
// bool on SnapshotRequest, would advertise a choice the caller does not
// actually have and cannot verify, and the honest report of what happened is
// SnapshotInfo.ReusedFiles after the fact, not a flag before it.
//
// There is no Prune, Forget or ApplyRetention. Deciding which snapshot stops
// being protected is retention policy, which this project owns
// (internal/retention), and DeleteSnapshot takes one identity at a time so
// that the decision is always made on our side of this line. Maintain
// reclaims space for content nothing references any more; it never chooses
// what to stop referencing.
//
// There is no engine-shaped streaming type either. A source whose bytes can
// only be read once, forward, is a real case this product has, and it is
// carried here as a capability -- StreamSource plus StreamingRepository --
// stated in this package's own types and io.ReadCloser. The implementation
// of it lives in the adapter with everything else that knows the vendor's
// name, which is why folding the streaming spike in cost this file four
// declarations and no imports.
package backupengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/secretref"
)

// ErrRepositoryExists is returned by CreateRepository when the location
// already holds a repository. Creating over one would orphan every snapshot
// in it, so this is a refusal, not a warning.
var ErrRepositoryExists = errors.New("backupengine: repository already exists")

// ErrRepositoryNotFound is returned by OpenRepository when the location holds
// no repository, as distinct from holding one we failed to unlock.
var ErrRepositoryNotFound = errors.New("backupengine: repository not found")

// ErrPassphrase is returned when a repository exists and the passphrase does
// not open it. Kept distinct from ErrRepositoryNotFound because the operator
// response differs: one is a configuration mistake, the other is a lost key.
var ErrPassphrase = errors.New("backupengine: incorrect repository passphrase")

// ErrSnapshotNotFound is returned when a SnapshotID names nothing, including
// the case where it named something that a previous DeleteSnapshot removed.
var ErrSnapshotNotFound = errors.New("backupengine: snapshot not found")

// ErrStorageUnsupported is returned when a storage target is reachable and
// authenticated but does not provide the semantics a content-addressed
// repository needs -- read-after-write on both reads and listings, atomic
// whole-blob writes, range reads, and timestamps that do not run
// backwards.
//
// It exists because "S3-compatible" is a marketing claim, not a
// specification, and the ways a partial implementation fails are exactly
// the ways that cannot be recovered from later: an index blob that is not
// visible in a listing right after it was written does not produce an
// error, it produces a repository that has forgotten some of its content.
// So the gap is found by probing at create time and reported as a
// refusal, rather than discovered as corruption during the first restore
// somebody needed.
var ErrStorageUnsupported = errors.New("backupengine: storage target does not provide the semantics a repository requires")

// RepositoryLocation says which repository is wanted, where its bytes
// live, and where the two secrets that reach it come from.
//
// Kind is a closed set rather than a free-form backend URL, because every
// additional backend is a decision with a dependency and a failure mode
// attached, the way internal/transport/rclone treats registered backends.
//
// # Why a Domain rather than a path
//
// A repository's identity is its Repository Domain
// (model.RepositoryDomain), not the directory or bucket it happens to sit
// in. That is what makes "reopen the same repository after a restart" a
// question with an answer: the process that comes back up has the config
// file, the cache and possibly the mount point in a different state, and
// the only stable thing is the id an operator declared. Deriving the
// storage location FROM the id, rather than carrying both and hoping they
// agree, also removes the failure this package's checkStorageIdentity
// exists to catch at its source.
//
// # Why the secrets are references
//
// Passphrase and S3.Credentials name where a secret comes from and never
// carry one, so a RepositoryLocation is safe to log, to render into an
// error and to keep in a struct for the life of a daemon. The material is
// resolved by the adapter at the moment it opens storage and dropped
// immediately afterwards, which is the shortest lifetime available to a
// library that has to hand a passphrase to somebody else's Open call.
type RepositoryLocation struct {
	Kind LocationKind

	// Domain is the repository's stable identity. It names the encryption,
	// credential, maintenance, deduplication, corruption and
	// administrative boundary every backup set stored here shares; see
	// model.RepositoryDomain, which is where that argument lives and where
	// co-tenancy is decided.
	Domain model.RepositoryDomainID

	// Root is the backup root for LocationLocal: the directory this
	// manager already owns on the machine it runs on. The repository is
	// NOT placed directly in it. It goes under the reserved namespace
	// ReservedLocalDir computes, because a backup root is the directory a
	// NAS deployment exports and an artifact catalog walks, and pack files
	// sitting next to artifacts are pack files something eventually
	// treats as artifacts.
	Root string

	// S3 is the bucket for LocationS3, and is ignored for every other
	// kind.
	S3 S3Storage

	// StateDir is where this process keeps the repository's connection
	// config, its index cache and its maintenance-ownership record: local
	// state about a repository, never repository content.
	//
	// It is ours to place, not the engine's to choose, because a
	// process-wide default config location is shared mutable state
	// between unrelated backupd invocations. It belongs under this
	// manager's private state directory (/var/lib/backupd), never under
	// Root: #298 was filed over exactly that exposure for the SSH key,
	// and a cache directory under an exported backup root is the same
	// mistake with more bytes in it.
	//
	// Empty is accepted only for LocationLocal, where it resolves to the
	// reserved namespace under Root. An S3 repository has nowhere to put
	// local state implicitly, so it must be told.
	StateDir string

	// Passphrase names where the repository passphrase comes from. It is
	// required for every kind: a repository this product creates is
	// always encrypted.
	Passphrase secretref.Ref
}

// LocationKind names a repository storage backend this boundary speaks.
type LocationKind string

const (
	// LocationLocal is a repository under the backup root on a locally
	// reachable filesystem, which includes an already-mounted network
	// share.
	LocationLocal LocationKind = "local"

	// LocationS3 is a repository in an S3 bucket, reached natively.
	//
	// Natively is the load-bearing word. The embedded engine also has a
	// provider that proxies through rclone, and using it would have made
	// every backend rclone speaks available for free; it is refused
	// because a repository is not a file copy. It needs read-after-write
	// listings, atomic writes and stable timestamps from the thing
	// underneath it, an rclone remote provides whatever its own backend
	// provides, and the failure mode of getting that wrong is a
	// repository that silently forgets content. rclone stays on the
	// source side, where a wrong answer is a failed read.
	LocationS3 LocationKind = "s3"
)

// S3Storage is the operator-configured description of an S3 bucket a
// repository lives in.
//
// It is deliberately the same set of facts config.StorageMedium already
// collects for an artifact destination -- endpoint, region, bucket,
// prefix, and a credential REFERENCE -- because an operator who has
// already told this product how to reach a bucket should not have to
// describe it a second time in a second vocabulary for the repository
// that lives in it.
//
// There is no field for "do not verify TLS", and there will not be. A
// knob that disables authentication of the endpoint, on the connection
// carrying every backup this product holds, is not a convenience; a
// private CA is a real situation and RootCA is the answer to it.
type S3Storage struct {
	// Endpoint is the service endpoint as a URL, e.g.
	// https://minio.example:9000, spelled exactly as
	// config.StorageMedium.Endpoint spells it. Empty means AWS's own
	// endpoint for Region.
	//
	// An http:// endpoint is accepted and means what it says: no TLS.
	// That is a real deployment (a MinIO on a trusted LAN segment, a test
	// fixture) and pretending otherwise would only push operators towards
	// disabling verification instead, which is worse.
	Endpoint string

	// Region is the provider region, passed through unexamined for
	// config.StorageMedium.Region's reason: the set of legal regions
	// belongs to the provider and changes without this product being
	// rebuilt.
	Region string

	// Bucket holds the repository. This product never creates a bucket:
	// bucket creation is an account-level act with billing and policy
	// consequences, and an operator who mistyped a name deserves a
	// refusal rather than a second empty bucket.
	Bucket string

	// Prefix is the key namespace inside Bucket, so one bucket can hold
	// more than one repository, or a repository beside something else
	// entirely. Empty puts the repository at the root of the bucket.
	Prefix string

	// Credentials names where this bucket's credentials come from. The
	// material behind it is AWS shared-credentials text, which is what
	// config.MediumCredentials already points at.
	Credentials secretref.Ref

	// RootCA is a PEM certificate bundle to trust in addition to the
	// system roots, for an endpoint behind a private CA. Empty means the
	// system roots, which is the ordinary case.
	RootCA []byte
}

// Source identifies what gets backed up, in the engine's own namespace.
//
// Host and User are part of the identity, not decoration: they are how the
// repository distinguishes two different machines backing up the same
// pathname into one shared repository, and getting them wrong makes the two
// look like one source whose contents keep changing completely.
type Source struct {
	Host string
	User string
	Path string
}

// SnapshotID is an opaque handle to one stored snapshot.
//
// Opaque means opaque: it is produced by the engine, compared for equality,
// and handed back. Nothing outside the adapter may parse it, and the catalog
// stores it as a string without interpreting it.
type SnapshotID string

// The tag keys every snapshot this product writes carries.
//
// # Why the repository has to be told who owns a snapshot
//
// A repository serves a Repository Domain, and a domain may be shared by
// several backup sets (model.RepositoryDomain.MayShare) or declared
// isolated, in which case it may not. Nothing about a stored snapshot says
// which set put it there: the engine's own source identity is a
// host/user/path triple, and a streaming set writes one of those PER
// OBJECT, so counting them answers a different question from the one the
// isolation rule asks. The first two tags below are how a snapshot
// carries that answer, and RepositoryStats.Sources is counted from
// TagKeyBackupSet.
//
// # Why a snapshot also has to say which RUN wrote it
//
// A daemon that dies mid-run leaves behind a manifest its own catalog
// never recorded, and the next start has to decide whether that orphan
// is the dead run's restore point or somebody else's. Without a run tag
// the only evidence available is TIME -- "it appeared after this run
// started" -- and time does not tell a co-tenant set's snapshot, a
// second daemon's, or an operator's own manual one taken with the
// vendor's CLI against the same bucket, apart from this run's. Adopting
// one of those records a restore point holding somebody else's data
// under this set's name, which is worse than adopting nothing at all.
// TagKeyRun turns that judgement into an equality: a manifest belongs to
// a run if, and only if, it says so.
//
// # Why the literal strings are here and not at the caller
//
// Because they are a WRITE/READ contract between two packages that never
// call each other: the sink that stores a snapshot sets them, and this
// package's repository adapter counts them. A literal at each end is two
// strings that agree until somebody edits one.
//
// The "backupd." prefix keeps them out of the way of the vendor's own
// manifest labels and of any tag an operator sets by hand with the
// vendor's CLI against their own bucket.
const (
	// TagKeyBackupSet is the backup set a snapshot belongs to, as
	// model.BackupSetID renders it ("source/set").
	TagKeyBackupSet = "backupd.set"

	// TagKeyDomain is the Repository Domain the snapshot was written for,
	// as model.RepositoryDomainID renders it.
	//
	// It is redundant with the repository a snapshot is in, and that is
	// the point: a snapshot whose domain tag disagrees with the repository
	// holding it is a snapshot written somewhere it does not belong, and
	// the tag is the only evidence that would survive to say so.
	TagKeyDomain = "backupd.domain"

	// TagKeyRun is the manager's own snapshot-run id, as the catalog
	// recorded it before the run touched storage.
	//
	// It is the one tag an adapter writes rather than the caller (see
	// TreeSnapshotRequest.RunID), because it is the one piece of
	// attribution a caller must not be able to forget: a manifest
	// without it can only be matched to a run by timestamp, and matching
	// by timestamp is how somebody else's snapshot becomes this set's
	// restore point.
	//
	// Reconciliation matches it EXACTLY, and together with TagKeyDomain
	// and TagKeyBackupSet rather than alone. A run id is unique inside
	// this product's own catalog and therefore proves nothing by itself
	// about which set or which domain the run was for; an orphan is
	// adopted only when all three agree.
	TagKeyRun = "backupd.run"
)

// SnapshotRequest asks for one snapshot of one source.
type SnapshotRequest struct {
	Source Source

	// Description is operator-facing text stored with the snapshot. It has
	// no semantics here and nothing branches on it.
	Description string

	// Tags are stored with the snapshot for later selection. Keys and
	// values are ours; the engine only records them.
	//
	// Every snapshot this product writes carries TagKeyBackupSet and
	// TagKeyDomain. See them: the co-tenancy number in RepositoryStats is
	// counted from the first, and a snapshot without it is one nothing can
	// attribute.
	Tags map[string]string
}

// SnapshotInfo reports one stored snapshot.
//
// ReusedFiles and NewFiles are the incrementality report, and they are
// counts of files the engine did and did not have to re-read, which is the
// only measure of "was this incremental" available without trusting a flag
// nobody set. Bytes is the logical size of the source tree at snapshot time,
// not the physical cost of storing it: after deduplication the second
// snapshot of a mostly-unchanged tree has the same Bytes as the first and
// costs almost nothing new on disk.
type SnapshotInfo struct {
	ID          SnapshotID
	Source      Source
	Start       time.Time
	End         time.Time
	Description string

	Files       int64
	Directories int64
	Bytes       int64

	ReusedFiles int64
	NewFiles    int64

	// Incomplete is empty for a finished snapshot and otherwise carries the
	// engine's reason. A non-empty value means the snapshot exists but does
	// not represent the whole source, which is a thing retention and restore
	// must be able to see rather than infer.
	Incomplete string

	// Tags are the tags stored with the snapshot, read back as the engine
	// holds them. It is nil when the manifest carries none.
	//
	// This is the READ side of the attribution contract the TagKey
	// constants above define, and it is here rather than left to the
	// adapter because the question it answers -- "whose snapshot is
	// this" -- is asked by code that has no business knowing the
	// engine's name. Crash reconciliation matches run, domain and set
	// exactly against these; a snapshot that cannot be attributed is one
	// it leaves alone.
	Tags map[string]string
}

// DefaultVerifySamplePercent is how much of a snapshot's file content a
// sampled verification reads when the caller names no percentage.
//
// Five percent is a deliberate compromise and not a measurement: it is
// enough that a repository losing content at random is found within a
// handful of runs, and small enough that a set can afford it every night
// on a tree whose full read would not fit in the backup window. A
// deployment that wants a different trade makes it explicitly, which is
// what VerifyRequest.SamplePercent is for.
const DefaultVerifySamplePercent = 5

// ErrRestoreTargetRequired is returned when a restore drill is asked for
// without a directory to restore into.
//
// It is a refusal rather than a scratch directory the adapter invents,
// because a drill writes a whole snapshot to a disk somebody owns: which
// disk, with how much room on it, is the caller's decision, and an engine
// that picked one would fill an operator's /tmp with a restore nobody
// asked for. It is also, crucially, not a downgrade -- a drill that
// cannot run must never come back as a content verification wearing the
// drill's name.
var ErrRestoreTargetRequired = errors.New("backupengine: a restore drill needs a directory to restore into")

// VerifyRequest asks for verification at a stated depth.
//
// # Why the level is a parameter
//
// Because the four rungs of model.VerificationLevel cost four different
// amounts and prove four different things, and a port offering only the
// deepest one forces every caller to pay for a full content read to learn
// that a manifest resolves. Worse, it makes the CLAIM unavailable: a set
// configured for structural verification whose runs all performed a full
// read has a catalog full of rows that overstate nothing and understate
// everything, and the day somebody configures restore_drill the rows look
// exactly the same.
//
// So the level travels in, and what was actually performed travels back
// out on VerifyReport.Level. The two are never assumed equal by the
// caller: an adapter that could not do what was asked says so by
// returning an error, never by returning a shallower level quietly, and a
// caller comparing the two is comparing a request with a result rather
// than reading its own request back.
type VerifyRequest struct {
	// Level is the depth asked for. An empty level is refused rather
	// than defaulted: silence here would be a verification claim nobody
	// made, exactly as model.ParseVerificationLevel argues.
	Level model.VerificationLevel

	// SamplePercent is how much of the snapshot's file content
	// LevelContentSample reads, 1 to 100. Zero means
	// DefaultVerifySamplePercent, and values above 100 are clamped to it.
	//
	// It is a percentage of FILES rather than of bytes because the unit a
	// sample has to spread over is the unit damage arrives in: a
	// repository that has lost one pack blob has lost some files
	// entirely, and a byte-proportional sample would spend the whole
	// budget inside the largest file and never look at the others.
	//
	// The sample is a COUNT over the files the walk finds, not a coin
	// flip per file and not a stride: a run asked for 10% of 30 files
	// reads exactly 3 of them, and one asked for 51% reads 16 of 31
	// rather than "every second one". A verification whose achieved
	// level depends on a random number is a verification that
	// occasionally proves less than the row says it did, and a stride
	// answers 51% and 99% identically, which is a configured increase in
	// assurance that buys nothing and says nothing.
	SamplePercent int

	// RestoreTarget is the directory LevelRestoreDrill restores into. It
	// is created if it does not exist and must be empty; the caller owns
	// removing it afterwards, because a drill that tidied up after itself
	// would also destroy the evidence of a drill that failed.
	RestoreTarget string
}

// VerifyReport reports what a verification actually read.
//
// Errors is a slice of strings rather than errors because these are the
// engine's per-object findings, plural, and the caller's job is to record
// and surface them, not to branch on their identity.
//
// A nil error means one thing only: the verification completed and found
// nothing wrong. Every other outcome is a non-nil error plus whatever the
// report had managed to record, and the two carry different halves of the
// answer -- the error says the verification did not pass, Errors says what
// was found, and errors.Is against context.Canceled or
// context.DeadlineExceeded is how a caller tells a verification that was
// torn down from a repository that is damaged.
//
// Deciding "did it run" from len(Errors) is the mistake this doc exists to
// forbid. A walk torn down by a cancelled context records its findings and
// returns an error, so a caller counting findings alone reads a cancellation
// as "ran, found damage" when nothing was verified at all -- and then
// reports damage to an operator who has a healthy repository, or worse,
// retries later and calls the second cancellation the same thing.
type VerifyReport struct {
	// Level is what this verification ACTUALLY performed, which is the
	// only value a catalog may record as proven. It is never higher than
	// the depth the walk reached: a sampled run that found no file to
	// read reports what it did, and a drill that restored nothing is not
	// a drill.
	Level model.VerificationLevel

	// ObjectsVerified is how many entries -- files and directories -- had
	// their stored structure resolved: the manifest's tree walked and
	// every content identifier it names looked up.
	ObjectsVerified int64

	// FilesVerified and BytesVerified are the CONTENT half: files whose
	// stored bytes were actually read back, decrypted, decompressed and
	// hash-checked, and how many bytes that was. Both are zero for a
	// structural verification, which is the point of reporting them
	// separately from ObjectsVerified.
	FilesVerified int64
	BytesVerified int64

	// BlobsChecked is how many distinct backing blobs were proven to
	// exist in the repository's storage, which is the repository-integrity
	// half of a sampled verification: an index can happily reference a
	// pack file that is no longer in the bucket, and no amount of reading
	// the files that happen to be sampled will find the ones that are not.
	BlobsChecked int64

	// FilesRestored and HashesMatched are the restore drill's own
	// evidence: how many files were written to a real filesystem by the
	// real restore path, and how many of those matched the hash of what
	// the repository holds. They are zero at every level below
	// LevelRestoreDrill, because nothing below it restores anything.
	FilesRestored int64
	HashesMatched int64

	Errors []string
}

// ErrNoRestoreDestination is returned when a restore is asked for with no
// local directory to write into.
//
// A refusal rather than a scratch directory the adapter invents, for the
// reason ErrRestoreTargetRequired gives about a drill: a restore writes a
// tree onto a disk somebody owns, and which disk is the caller's
// decision.
var ErrNoRestoreDestination = errors.New("backupengine: a restore needs a directory to restore into")

// ErrRestorePathNotFound is returned when RestoreRequest.SourcePath names
// nothing inside the snapshot.
//
// Distinct from ErrSnapshotNotFound because the two send an operator to
// different places: one means the restore point is gone, the other means
// the file they asked for was not in the restore point they named -- which
// for a single-file restore is the ordinary answer to "was this backed up
// on that date", and must never be reported as a missing snapshot.
var ErrRestorePathNotFound = errors.New("backupengine: that path is not in this snapshot")

// ErrRestoreConflict is returned when the destination already holds
// something where a restored entry would land and the request said to
// refuse rather than replace or skip it.
//
// It is the default outcome, and deliberately so: the one thing a restore
// must never do by accident is destroy the data somebody is restoring
// BESIDE.
var ErrRestoreConflict = errors.New("backupengine: the destination already holds something the restore would replace")

// ErrUnsafeSnapshotPath is returned when a snapshot names an entry whose
// name cannot be placed under the destination without leaving it.
//
// # Why a stored snapshot gets treated as hostile input
//
// Because a repository is not a trust boundary. A repository domain may
// be shared, an operator may have written to the same bucket with the
// vendor's own CLI, a source is somebody else's filesystem, and an
// attacker who can put a file on a source can choose its name. The
// snapshot WRITE path refuses a name that is not one path element
// (backupengine/source.SafeRelPath, and the tree adapter's own entry
// check), and the restore path refuses it again rather than trusting that
// it was written by the same build of the same program.
//
// A restore that met one of these stops. It does not skip the entry and
// carry on, because a restore that silently omits files is a restore that
// lies about being complete, and a snapshot containing a name shaped like
// an escape is evidence about the whole snapshot rather than about one
// file in it.
var ErrUnsafeSnapshotPath = errors.New("backupengine: this snapshot names an entry that would be written outside the restore destination")

// ErrUnknownRestoreConflict is returned for a conflict policy this
// boundary does not define. Silence means RestoreConflict's documented
// default; a value that is neither silence nor one of the three is a
// caller mistake, and guessing which one they meant is how "skip" becomes
// "overwrite".
var ErrUnknownRestoreConflict = errors.New("backupengine: unknown restore conflict policy")

// RestoreConflict says what a restore does when something already exists
// where an entry would land.
//
// The zero value is the safe one (ConflictRefuse), which is the whole
// reason this is a named string rather than a bool: the destructive
// choice cannot be reached by leaving a field unset, only by spelling it.
type RestoreConflict string

const (
	// ConflictRefuse stops the restore at the first collision and writes
	// nothing over it. It is what silence means.
	ConflictRefuse RestoreConflict = "refuse"

	// ConflictSkip leaves what is already there and carries on, counting
	// what it left alone in RestoreReport.Skipped. It is the policy for
	// resuming a restore that was interrupted, where everything already
	// written is what this restore would have written anyway.
	ConflictSkip RestoreConflict = "skip"

	// ConflictOverwrite replaces what is already there. It is the only
	// value that can destroy data the caller did not restore, so it is
	// never a default and never inferred.
	ConflictOverwrite RestoreConflict = "overwrite"
)

// ParseRestoreConflict resolves a conflict policy arriving as a string,
// which is how one arrives from outside this process.
//
// Empty is the documented default rather than an error, because a
// request that says nothing about collisions is asking for the safe
// behaviour and getting it. An unrecognised value is refused: a
// misspelling resolved by guessing is how "skip" becomes "overwrite" on
// somebody's data, and a restore is the operation where that matters
// most.
//
// It is here rather than in the adapter because both ends need it -- the
// adapter to normalise what it was handed, and whatever accepts an
// operator's request to refuse a bad one BEFORE a durable operation row
// exists for work that could never run.
func ParseRestoreConflict(s string) (RestoreConflict, error) {
	switch RestoreConflict(s) {
	case "":
		return ConflictRefuse, nil
	case ConflictRefuse, ConflictSkip, ConflictOverwrite:
		return RestoreConflict(s), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownRestoreConflict, s)
	}
}

// RestoreProgress is one entry's worth of a restore in flight, plus the
// running totals at that moment.
//
// It is delivered per completed entry rather than per byte because the
// unit an operator is waiting for is a file, and a byte-level callback on
// a tree of small files costs more than the restore. Path is the entry's
// slash-separated path relative to what was asked for, so a single-file
// restore reports the file's own name and a whole-snapshot restore
// reports the path inside the snapshot.
//
// The counters are cumulative and are the same numbers RestoreReport
// ends with, so a surface that renders progress and a surface that
// renders the outcome are reading one set of facts rather than two.
type RestoreProgress struct {
	Path        string
	Files       int64
	Directories int64
	Symlinks    int64
	Skipped     int64
	Bytes       int64
}

// RestoreRequest asks for a snapshot, or one directory or file inside it,
// to be written to a local directory.
//
// # Local, and that is a decision rather than a phase
//
// There is no destination other than a filesystem path this process can
// write to. Restoring straight back to the remote the source came from is
// a real thing operators want and is deliberately not here: it is a
// streaming write against somebody else's endpoint with its own failure,
// resume and partial-write story, and offering it as a field on this
// struct would make it look like a flag rather than the separate piece of
// work it is.
type RestoreRequest struct {
	// SourcePath selects what to restore, as a slash-separated path
	// inside the snapshot. Empty means the whole snapshot.
	//
	// One field rather than a mode plus a path, because the three
	// granularities an operator asks for -- everything, this directory,
	// this one file -- are one question ("what part of the restore point
	// do you want") and the answer is a path. What the path names decides
	// what happens: a directory is restored as a tree under TargetPath, a
	// file is restored as one file INSIDE TargetPath, keeping its own
	// name.
	//
	// It is validated as untrusted input like everything else here, so a
	// caller cannot use it to reach out of the snapshot either.
	SourcePath string

	// TargetPath is the local directory the restore writes into. It is
	// created if it does not exist, and it is the boundary nothing the
	// snapshot says can write outside of.
	TargetPath string

	// Conflict is what happens when something already exists where an
	// entry would land. The zero value refuses.
	Conflict RestoreConflict

	// SkipOwners skips restoring uid/gid, which is what an unprivileged
	// restore has to do. It covers files, directories and symbolic links
	// alike: a tree whose files carry their owners and whose directories
	// carry the restoring process's is not the tree that was backed up.
	//
	// Nothing else about metadata is optional. Every implementation
	// restores each entry's mode and its modification time, because a
	// restored tree stamped "now" is one no operator, build system or
	// incremental tool can reason about -- and a directory that was
	// ALREADY in the destination keeps its own mode, ownership and time,
	// since re-entering a directory to put a file in it is not
	// permission to restyle it. The one documented exception is a
	// symbolic link's own timestamp on a platform with no l-variant of
	// utimes, which is skipped rather than applied to the link's target.
	SkipOwners bool

	// VerifyContent re-reads every file after writing it and checks the
	// bytes on the disk against the bytes the repository handed over.
	// The check happens before the file is published under its real
	// name, so a file whose bytes did not survive the write never
	// appears at all.
	//
	// It is a request-level choice because it costs a second read of
	// everything restored, and because the callers want different
	// things. An operator restoring a terabyte to a disk they are about
	// to use has already paid for the write and may not want to pay for
	// the read. The durable restore operation always asks for it, since
	// it records a completion that is read later as evidence and a
	// restore's own statistics are exactly what a broken restore path
	// reports correctly.
	//
	// The verification ladder's restore drill does NOT set it, and that
	// is not an oversight: a drill compares the restored tree against
	// the repository's own bytes afterwards, which is the same property
	// established against a stronger reference, and asking for both
	// would read everything three times to learn one thing twice.
	//
	// A file whose disk bytes do not match is a failure of the whole
	// restore, not a finding on it: the file is there, it is wrong, and
	// nothing downstream would ever look again.
	VerifyContent bool

	// Progress, when set, is called once per completed entry, in the
	// restore's own goroutine. A slow callback slows the restore down,
	// which is the honest coupling: a surface that cannot keep up should
	// buffer on its own side rather than making this one guess.
	Progress func(RestoreProgress)
}

// RestoreReport reports what a restore actually wrote.
//
// Complete is the field that must be read before any of the others are
// believed. A restore torn down by a cancelled context, or stopped by a
// conflict, still reports what it had written by then -- those files are
// really there -- and a caller that recorded such a report as a finished
// restore would be advertising a partial tree as a restore point. The
// counts are an observation; Complete is the claim.
type RestoreReport struct {
	Files       int64
	Directories int64
	Symlinks    int64
	Bytes       int64

	// Skipped is how many entries were left alone because something was
	// already there and the policy was ConflictSkip. It is always zero
	// under the other two policies, which is what makes a skip-policy
	// restore's report legible: Files is what this restore wrote, Skipped
	// is what it found already done.
	Skipped int64

	// Verified is how many restored files had their bytes read back off
	// the disk and compared with the repository's. It is zero unless
	// RestoreRequest.VerifyContent was set, and equals Files when it was.
	Verified int64

	// Complete is true only when the restore reached the end of what was
	// asked for. It is the only value that may be recorded as "this
	// restore succeeded".
	Complete bool
}

// MaintenanceMode selects how much work Maintain does.
type MaintenanceMode string

const (
	// MaintenanceQuick does the cheap, frequent housekeeping.
	MaintenanceQuick MaintenanceMode = "quick"

	// MaintenanceFull includes reclaiming space for unreferenced content,
	// which is the part that can actually delete bytes and therefore the
	// part that needs a deliberate caller.
	MaintenanceFull MaintenanceMode = "full"
)

// MaintenanceReport reports what maintenance did.
type MaintenanceReport struct {
	Mode MaintenanceMode

	// Ran is false when maintenance declined to do anything, which is a
	// normal outcome (nothing was due, or another process held the lock)
	// and not an error.
	Ran bool
}

// HealthWarningKind names a condition a repository can be in that is not
// a failure and is not fine either.
//
// The set is closed so that a caller can route on one -- raise an alert,
// refuse to start a maintenance window -- without matching prose, and so
// that adding a condition is a deliberate act with a name an operator will
// read.
type HealthWarningKind string

const (
	// HealthWarningClockSkew means this machine's clock and the storage's
	// own idea of the time disagree by more than a repository can safely
	// tolerate.
	//
	// It matters because almost everything a repository does about
	// concurrency and reclamation is expressed in timestamps: a
	// maintenance lock is held until a time, a blob is too recent to
	// garbage-collect until a time, and a snapshot's own start time is
	// what retention later reasons about. A clock an hour fast can make
	// one process believe another's lock has expired while it is still
	// held; a clock an hour slow can make freshly written content look old
	// enough to reclaim. Neither produces an error at the time.
	//
	// This is a WARNING and never a refusal, deliberately. The
	// alternative is a product that stops backing up because an NTP
	// server was unreachable, which trades a risk for a certainty. Fixing
	// it is time synchronisation, which is the operating system's job and
	// not this product's.
	HealthWarningClockSkew HealthWarningKind = "clock_skew"

	// HealthWarningUnreachable means the repository's storage did not
	// answer, or answered without providing what a repository needs.
	//
	// It accompanies HealthReport.Reachable == false, and it exists so
	// that "unreachable" carries WHICH part failed: a NAS that is asleep,
	// a bucket policy that started denying deletes and a credential that
	// expired are the same unreachable and three different jobs to do.
	//
	// Unlike the kind above this is not a "working but worth attention"
	// condition. Health returns it beside an error, and a caller that
	// must decide whether to start a backup reads the error; a caller
	// that must show an operator a status reads this.
	HealthWarningUnreachable HealthWarningKind = "unreachable"

	// HealthWarningEngineRetention means this repository's own stored
	// retention policy still expires snapshots, and the correction this
	// product attempts when it opens a repository could not be written.
	//
	// It matters because the engine applies that policy by itself, at
	// every mid-upload checkpoint, with no reference to this product's
	// catalog or its holds. Every manifest backupd writes carries a pin
	// the engine's expiry honours, so this product's own snapshots are
	// safe either way; a snapshot written into the same repository by
	// anything else is not.
	//
	// It is a WARNING and never a refusal, for the reason the correction
	// is best-effort in the first place: opening a repository is also how
	// a restore reads one, and storage that will not accept a write --
	// WORM, an object lock, a read-only mount, a legal hold -- is a
	// posture where restoring is the whole point.
	HealthWarningEngineRetention HealthWarningKind = "engine_retention"
)

// HealthWarning is one thing worth an operator's attention about a
// repository that is nonetheless working.
type HealthWarning struct {
	// Kind is what was found, from the closed set above.
	Kind HealthWarningKind

	// Detail is the operator-facing sentence: what was measured, what was
	// expected, and what to do. It never carries a credential, a
	// passphrase or any part of either.
	Detail string
}

// HealthReport is what a health check found.
//
// Reachable and Warnings answer two different questions and a caller needs
// both: a repository can be perfectly reachable and have a clock that will
// corrupt its next maintenance window, and it can be unreachable for a
// reason that has nothing wrong with it (a NAS that is asleep).
type HealthReport struct {
	// Reachable is whether the storage answered a read and a write.
	//
	// It is proved rather than assumed: a repository handle stays open
	// across a network partition and every method on it would fail, so
	// "we have a handle" is not evidence and this check does not treat it
	// as any.
	Reachable bool

	// Warnings is everything found that is not a failure, in the order it
	// was checked. Empty means nothing was found, which is the answer an
	// operator wants and the only one they should get when it is true.
	Warnings []HealthWarning
}

// RepositoryStats reports what a repository holds.
//
// The numbers here are the PHYSICAL ones, which is the distinction that
// makes this type worth having beside SnapshotInfo. A snapshot's Bytes is
// the logical size of what was backed up and is the same for the tenth
// snapshot of an unchanged tree as for the first; what an operator needs
// to know is how much storage the repository is actually occupying, which
// only the storage can answer.
type RepositoryStats struct {
	// Sources is how many distinct BACKUP SETS have snapshots here.
	//
	// It is the co-tenancy number: a Repository Domain declared isolated
	// whose repository reports two sources is a boundary that has already
	// been crossed, and that claim is why the count is over backup sets
	// and not over the engine's own source identities. A streaming set
	// writes one engine source per OBJECT, so a count of those would
	// report forty co-tenants for one set of forty database dumps.
	//
	// The identity counted is the TagKeyBackupSet tag every snapshot this
	// product writes carries. Snapshots carrying no such tag -- which
	// this product does not produce, and an operator's own use of the
	// vendor's CLI against the same bucket would -- count as one
	// unattributed tenant between them, because content sharing a
	// domain's key that nothing can attribute is still content sharing
	// the domain's key.
	Sources int

	// Snapshots is how many snapshots the repository holds across every
	// source.
	Snapshots int

	// Blobs is how many storage objects the repository occupies, and
	// PhysicalBytes is their total size.
	//
	// Both are read from the storage's own listing rather than from the
	// repository's index, because the question they answer is "what is
	// this costing" and the answer to that is whatever is really there,
	// including blobs an interrupted maintenance left behind.
	Blobs         int
	PhysicalBytes int64
}

// Engine creates and opens repositories. It holds no repository state.
type Engine interface {
	// CreateRepository initializes a new repository at the location and
	// leaves it closed. It returns ErrRepositoryExists rather than
	// adopting or overwriting an existing one.
	CreateRepository(ctx context.Context, loc RepositoryLocation) error

	// OpenRepository connects to an existing repository. The caller owns
	// the returned Repository and must Close it.
	OpenRepository(ctx context.Context, loc RepositoryLocation) (Repository, error)
}

// Repository is an open repository, and the only surface lifecycle code is
// allowed to depend on.
//
// It is a handle rather than a set of stateless functions taking a
// RepositoryLocation, unlike transport.Transport, and the difference is not
// stylistic: opening a content-addressed repository loads format blobs and
// builds an index cache, so a stateless surface would pay that cost per call
// and a long-running daemon would spend most of a backup window reopening.
type Repository interface {
	// Snapshot stores the current state of a source. If the repository
	// already holds snapshots of the same Source, this reuses their
	// content; see the package doc on why that is not a parameter.
	Snapshot(ctx context.Context, req SnapshotRequest) (SnapshotInfo, error)

	// ListSnapshots returns snapshots of one source, oldest first.
	ListSnapshots(ctx context.Context, src Source) ([]SnapshotInfo, error)

	// LookupSnapshot returns one snapshot by its identity, or
	// ErrSnapshotNotFound.
	//
	// It exists beside ListSnapshots because the catalog stores a
	// SnapshotID and later has to ask what became of it, and answering
	// that by listing a source's snapshots and scanning for a match costs
	// a manifest load per snapshot and cannot answer at all for a source
	// whose identity has since changed.
	LookupSnapshot(ctx context.Context, id SnapshotID) (SnapshotInfo, error)

	// Verify checks a snapshot to the depth req names and reports what it
	// actually did. A nil error means the verification completed and
	// found nothing wrong; anything else is an error plus a report of
	// what it managed to check. See VerifyReport and VerifyRequest, which
	// is where that contract is argued.
	//
	// An implementation that cannot perform the level asked for returns
	// an error. It never returns a shallower level and a nil error,
	// because the whole value of the ladder is that a row saying
	// restore_drill means a restore happened.
	Verify(ctx context.Context, id SnapshotID, req VerifyRequest) (VerifyReport, error)

	// Restore writes a snapshot, or one directory or one file inside it,
	// to a local directory.
	Restore(ctx context.Context, id SnapshotID, req RestoreRequest) (RestoreReport, error)

	// DeleteSnapshot removes one snapshot's identity. It does not reclaim
	// space; Maintain does. Deleting an already-deleted snapshot returns
	// ErrSnapshotNotFound rather than succeeding quietly, because the
	// caller asked about a specific thing and deserves to know it was not
	// there.
	DeleteSnapshot(ctx context.Context, id SnapshotID) error

	// Maintain performs repository housekeeping, including reclaiming
	// space for content that DeleteSnapshot orphaned.
	Maintain(ctx context.Context, mode MaintenanceMode) (MaintenanceReport, error)

	// Health reports whether this repository is usable right now, and
	// what is worth an operator's attention even though it still works.
	//
	// A nil error means the check completed; it does NOT mean everything
	// is fine, because the interesting answers are warnings rather than
	// failures. Read HealthReport.
	Health(ctx context.Context) (HealthReport, error)

	// Stats reports what the repository holds and what it costs.
	//
	// It is separate from Health because the two have different costs and
	// different callers: health is a cheap preflight something runs before
	// every backup, and stats walks the storage's blob listing, which on a
	// bucket with a large repository in it is a real number of requests.
	Stats(ctx context.Context) (RepositoryStats, error)

	// Close releases the repository. Safe to call twice.
	Close(ctx context.Context) error
}

// StreamSource is one object whose bytes arrive once, in order, from
// somewhere this process cannot seek: the source data plane, stated in the
// least a streaming transport can honestly offer.
//
// There is no Size and no Seek, and both absences are a finding rather than
// an omission. A remote stream's length is not known until it ends, its
// bytes arrive once, and the spike behind ADR 0007 proved the engine needs
// neither: an io.ReadCloser is enough. An interface with a Seek on it would
// be an invitation to implement one by reopening the remote, which turns one
// sequential read into an unbounded number of connections and hides that
// behind a method name.
//
// The identity of what is being backed up is not here either. It is
// StreamSnapshotRequest.Source, because a source's name in the repository
// and the bytes of one of its objects are two different concerns, and the
// mismatch between them is exactly the bug that made nested object names
// unrestorable in the spike.
type StreamSource interface {
	// ModTime is the modification time recorded with the snapshot. The
	// zero time means the transport does not report one, and nothing here
	// treats it as content: see StreamSnapshotRequest on why a streamed
	// source is never reused on the strength of it.
	ModTime() time.Time

	// Open starts a new sequential read of the whole object, from byte
	// zero.
	//
	// It is called once per attempt: a retry re-opens rather than resumes,
	// because resuming would require the source to be seekable and this
	// boundary refuses to pretend that it is. The returned reader is
	// closed exactly once, by the engine or by the implementation behind
	// it, whichever gets there first.
	Open(ctx context.Context) (io.ReadCloser, error)
}

// StreamSnapshotRequest asks for one snapshot of one streamed object.
type StreamSnapshotRequest struct {
	// Source identifies the object in the repository. Path is a rooted,
	// slash-separated path in the source's own namespace
	// ("/runs/2026/db.dump"), not a local filesystem path: nothing opens
	// it, and requiring it rooted is what keeps a snapshot's identity from
	// depending on the process working directory.
	Source Source

	// Stream is where the bytes come from. It is required; a request
	// without one is refused rather than producing a snapshot of nothing.
	Stream StreamSource

	// Description is operator-facing text stored with the snapshot.
	Description string

	// Tags are stored with the snapshot for later selection.
	Tags map[string]string

	// MaxAttempts bounds how many times a broken stream is re-read from
	// byte zero. Zero means the engine's default; a negative value is
	// refused rather than quietly meaning "never try", because a caller
	// that computed a negative attempt count has a bug and deserves to
	// hear about it.
	MaxAttempts int
}

// StreamSnapshotInfo reports one streamed snapshot.
//
// It embeds SnapshotInfo and adds the two numbers only a streaming run can
// report: what it actually pushed into storage, and how many times it had to
// open the source to get there.
type StreamSnapshotInfo struct {
	SnapshotInfo

	// UploadedBytes is how many bytes this run pushed into the
	// repository's storage. On a re-snapshot of unchanged content it is
	// near zero while SnapshotInfo.Bytes is the whole object, and that
	// difference is the measurement that says content was reused rather
	// than stored again.
	UploadedBytes int64

	// Attempts is how many times the source had to be opened, so a caller
	// can tell a clean run from one that survived an interruption.
	Attempts int
}

// StreamingRepository is the capability a Repository advertises when it can
// store an object it cannot stat, seek or re-read.
//
// It is a separate interface rather than two more methods on Repository
// because it is a capability and not every engine has to have it: a caller
// type-asserts for it, and an engine that cannot stream says so by not
// satisfying it, which is a compile-time answer instead of a runtime
// ErrUnsupported. Everything it adds is expressed in this package's own
// types plus io.ReadCloser.
//
// # Its unit is ONE object, and that is a Phase-1 interim
//
// One call takes one stream and produces one snapshot, so a caller
// backing up a whole source through this port produces one snapshot per
// object: a set of 100k files becomes 100k snapshots, manifests and
// sources, with every per-snapshot cost paid per file. That is enough to
// prove a source can be read straight into a repository - which is what
// backupengine/source and ADR 0012 do with it - and it is not the shape a
// backup set is stored in.
//
// #783 introduces the session-scoped port that is: one snapshot per
// backup-set run, under the set's own identity, with its objects as a
// tree inside it. That port is not an implementation of this one. This
// interface is push-shaped, and the engine underneath is pull-shaped: its
// uploader walks a tree and asks it for children, so the control
// flow inverts, and with it the error model: there is nothing per-object
// to delete out of a set-wide snapshot, so what this port expresses as
// "store, then DeleteSnapshot what could not be proven" becomes a
// stream-level error the uploader observes while it is pulling.
type StreamingRepository interface {
	Repository

	// SnapshotStream reads the request's Stream once, straight into the
	// repository, and stores a snapshot only if the read completed. A
	// broken stream is retried by re-opening from byte zero; a cancelled
	// context is not retried, because it is an instruction rather than a
	// failure.
	//
	// A streamed object is never reused on metadata. It has no size to
	// compare, so "unchanged" would mean "same modification time", and a
	// source that rewrites a file while preserving its mtime would have
	// its new content skipped silently and unrecoverably. Every run reads
	// every byte; deduplication happens below, on content.
	SnapshotStream(ctx context.Context, req StreamSnapshotRequest) (StreamSnapshotInfo, error)

	// OpenSnapshotStream reads back what a streamed snapshot stored. The
	// reader is over the repository, not over the original source, and the
	// caller closes it.
	OpenSnapshotStream(ctx context.Context, id SnapshotID) (io.ReadCloser, error)
}

// --- the set-scoped tree snapshot (#783) ---------------------------------

// SourceDir is one directory of a backup source, seen the way a
// pull-based engine needs to see it: something that can be ASKED for its
// children, one at a time, when the engine gets to it.
//
// # Why the control flow is this way round
//
// StreamingRepository above is push-shaped -- a caller reads an object
// and hands the bytes over -- and that shape is what forces one snapshot
// per object, because the caller is the one deciding when a snapshot
// starts and ends. A snapshot that spans a whole backup set has to be
// the other way round: the engine walks, decides what it already holds,
// and only asks for the bytes it actually needs. So the source is
// expressed here as a tree that can be iterated rather than as a
// sequence of stores, and the run's errors travel back up through
// SourceDirIterator.Next and StreamSource.Open while the engine is
// pulling.
//
// Iteration is one-shot and stated as such: Open starts a pass, and a
// source whose directory listing is a forward cursor over a remote
// (transport.DirReader, and every backend that declares bounded_listing)
// cannot rewind. An engine that needs a second pass opens a second
// iterator, and an implementation that cannot serve one says so by
// failing Open rather than by silently replaying a buffer it would have
// had to keep.
type SourceDir interface {
	// Open begins one pass over this directory's children. The caller
	// closes the iterator.
	Open(ctx context.Context) (SourceDirIterator, error)
}

// SourceDirIterator is one pass over one directory's children.
//
// The three-value Next is deliberate: "no more entries" and "this walk
// failed" are different answers, and an iterator that reported the end
// of a directory as an error, or a failed listing as the end of one,
// would turn an unreadable source into a snapshot that claims the
// directory was empty. That is the one failure mode a backup must never
// have, so it is spelled out in the signature rather than left to a
// sentinel comparison.
type SourceDirIterator interface {
	// Next returns the next entry. ok is false, with a nil error, at the
	// end of the directory. A non-nil error ends the whole snapshot: no
	// manifest is written for a tree whose listing could not be
	// completed.
	Next(ctx context.Context) (entry SourceEntry, ok bool, err error)

	// Close releases the pass. It is called exactly once, including on
	// the paths where iteration stopped early.
	Close() error
}

// SourceEntry is one child of a source directory.
//
// Exactly one of Dir and Stream is set, and which one it is IS the
// entry's kind: there is no Kind field to disagree with them. An entry
// with neither is refused by the engine rather than stored as an empty
// file, because an entry nothing can be read from is a hole in a backup
// and a zero-byte file is how a hole gets hidden.
//
// A symbolic link that a source is configured to preserve arrives here
// as a Stream over its target string, which is what
// backupengine/source's reading path already produces; nothing in this
// boundary follows a link.
type SourceEntry struct {
	// Name is a single path element -- no slashes, never "." or ".." --
	// in the source's own namespace. The engine joins it onto the path
	// it is already walking, so a name carrying a separator would let a
	// source's own directory listing decide where in the snapshot its
	// content lands.
	Name string

	// ModTime is what the source said, at scan time. It is recorded with
	// the entry and is not evidence about content: see SnapshotTree on
	// why a tree snapshot does not reuse a file on the strength of it.
	ModTime time.Time

	// Size is what the listing reported, for progress and for the
	// scanned-bytes accounting. A negative value means the source does
	// not report one, which a streaming source legitimately does not,
	// and nothing derives "unchanged" from this number.
	Size int64

	// Dir is the child directory, for a directory entry.
	Dir SourceDir

	// Stream is the child's content, for a file entry. It is opened by
	// the engine, at most once, when the engine reaches it.
	Stream StreamSource
}

// IsDir reports whether this entry is a directory, which is the same
// question as "is Dir set".
func (e SourceEntry) IsDir() bool { return e.Dir != nil }

// TreeSnapshotRequest asks for ONE snapshot of one backup set's whole
// source tree.
//
// One request is one run of one backup set, and Source is the set's own
// identity (model.SourceIdentity), not an object's path. That is the
// difference from StreamSnapshotRequest that the whole type exists for: a
// run produces one manifest, under one source, so the repository's own
// accounting -- how many sources share this domain, which snapshots
// belong to this set, what the previous restore point was -- answers the
// question an operator actually asked.
type TreeSnapshotRequest struct {
	// Source is the backup set's source identity in the repository's
	// namespace: host, user and the set's root path.
	Source Source

	// Root is the tree to store. A nil Root is refused rather than
	// stored as an empty snapshot.
	Root SourceDir

	// RunID is the manager's own snapshot-run id for this run, as the
	// catalog recorded it before anything was stored.
	//
	// It is REQUIRED: SnapshotTree refuses an empty one before it
	// touches storage, because a snapshot that cannot say which run
	// wrote it is a snapshot crash reconciliation can only match by
	// timestamp. TagKeyRun is what that costs, argued in full.
	//
	// The adapter writes it as the TagKeyRun tag itself rather than
	// leaving it to Tags below. Attribution a caller can forget is
	// attribution that goes missing on the one path nobody exercises by
	// hand -- the crash path -- which is the only path this field exists
	// to survive.
	RunID string

	// Description is operator-facing text stored with the snapshot.
	Description string

	// Tags are stored with the snapshot, alongside the TagKeyRun the
	// adapter adds. Every snapshot this product writes carries
	// TagKeyBackupSet and TagKeyDomain, and the engine synthesises
	// neither: the caller owns set and domain attribution, because the
	// caller is the only thing that knows which backup set is running.
	//
	// A TagKeyRun entry here does not win. RunID is authoritative,
	// because two sources for one fact are one fact and one bug.
	Tags map[string]string
}

// TreeSnapshotInfo reports one stored tree snapshot.
//
// The four byte counts are four different numbers and a surface must
// never present one as another (#783's metrics requirement):
//
//   - SnapshotInfo.Bytes is what was SCANNED: the logical size of the
//     tree as the source described it.
//   - SourceBytesRead is what this run actually pulled off the source.
//   - RepositoryBytesWritten is what this run actually pushed into the
//     repository's storage, after deduplication and compression.
//   - ContentReusedBytes is how much of what the run offered the
//     repository was content it already held, and so did not store
//     again.
//
// On a second run over a mostly-unchanged tree the first two are the
// whole tree and the third is nearly nothing, and that gap is the only
// honest evidence that content was reused. Reporting Bytes as "uploaded"
// would report a deduplicated repository as growing by the size of the
// source every night, which is the specific lie this type is shaped to
// prevent.
//
// # Why reuse is MEASURED and not derived
//
// The arithmetic that suggests itself -- SourceBytesRead minus
// RepositoryBytesWritten -- is not deduplication. That difference also
// contains compression, and the pack and index overhead of storing
// anything at all, so a FIRST snapshot of highly compressible data,
// which by definition reused nothing, would report most of itself as
// reused and tell an operator their brand new backup was mostly free.
// ContentReusedBytes therefore comes from the engine's own
// content-level accounting or it does not come at all: when the engine
// cannot supply it, ContentReuseMeasured is false, because "not
// measured" and "measured zero" are different facts and a catalog that
// stored them as the same number would be reporting the first as the
// second.
//
// Compression savings are deliberately NOT reported here and are not
// folded into reuse. They are a different economy -- the same bytes
// stored more cheaply, rather than bytes not stored twice -- and one
// number covering both would answer neither "is incremental backup
// working" nor "is compression earning its CPU".
type TreeSnapshotInfo struct {
	SnapshotInfo

	// SourceBytesRead is how many bytes the run read from the source.
	SourceBytesRead int64

	// RepositoryBytesWritten is how many bytes the run wrote to the
	// repository's storage.
	RepositoryBytesWritten int64

	// ContentReusedBytes is how many bytes of content this run handed to
	// the repository that it already held. It is THIS run's reuse, not
	// the repository's lifetime total, and a first snapshot's is zero.
	//
	// It means nothing unless ContentReuseMeasured is true.
	ContentReusedBytes int64

	// ContentReuseMeasured says whether ContentReusedBytes is a
	// measurement at all.
	//
	// False means the engine could not account for reuse on this run. It
	// does not mean nothing was reused, and the two must not be reported
	// alike: a zero presented as a measurement is a claim that the run
	// deduplicated nothing, which sends an operator looking for a broken
	// incremental backup that is working perfectly well.
	ContentReuseMeasured bool
}

// TreeRepository is the capability a Repository advertises when it can
// store a whole source tree as one snapshot.
//
// It is a capability interface for the same reason StreamingRepository
// is: a caller type-asserts, and an engine that cannot do this says so by
// not satisfying it. It is not an implementation of StreamingRepository
// and does not replace it as an interface; what it replaces is the SHAPE
// a backup set is stored in, which is why the per-object port's own doc
// points here.
type TreeRepository interface {
	Repository

	// SnapshotTree walks req.Root and stores everything under it as one
	// snapshot of req.Source, or stores nothing at all.
	//
	// Nothing at all is the load-bearing half. A tree whose listing
	// failed, whose stream broke, or whose read was found to have been
	// torn is not a snapshot with a gap in it: it is a run that produced
	// no manifest, so no restore point is advertised and no later pass
	// has to work out which parts of it to trust.
	//
	// Content is reused, files are not. Deduplication happens below this
	// boundary, on content that has already been read, and every run
	// reads every byte the source offers: a source's size and
	// modification time are scan-time metadata from a live directory
	// somebody else is writing to, and skipping a file's content on the
	// strength of them is how a rewritten file silently stays at its old
	// content for ever. TreeSnapshotInfo is where the resulting
	// arithmetic is reported, and RepositoryBytesWritten is what shows
	// the reuse actually happened.
	SnapshotTree(ctx context.Context, req TreeSnapshotRequest) (TreeSnapshotInfo, error)
}
