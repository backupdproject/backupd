package kopia

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/snapshot"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// This file owns a repository's LIFECYCLE: which storage it lives in, how
// it is created and opened, how the two secrets that reach it are
// resolved, and what it can report about itself. adapter.go owns what
// happens inside an open one.
//
// # Two storage backends, registered rather than imported
//
// filesystem and native s3, and each is a decision with a dependency, an
// authentication surface and a failure mode attached. What is NOT
// registered is the vendor's rclone-backed provider, which would have made
// every backend rclone speaks available by adding one import. See
// backupengine.LocationS3 for why that import is not there: a repository
// needs read-after-write listings, atomic whole-blob writes and stable
// timestamps from the storage underneath it, an rclone remote provides
// whatever its own backend happens to provide, and a repository built on
// storage that does not provide them does not fail, it forgets content.
//
// # "S3-compatible" is a claim, so it is tested rather than believed
//
// The same reasoning applies inside the s3 backend. The vendor's own
// blob.Storage doc states the required semantics, and half the products
// answering S3 on a port meet some of them. probeStorage writes one blob
// and checks each property against the actual endpoint before a repository
// is created on it, so an implementation that cannot list what it just
// wrote is a refusal an operator reads at setup time instead of a restore
// that comes back short.

// probePrefix is the blob id prefix the capability probe uses.
//
// The underscore is the point: every blob id the vendor writes begins with
// a lowercase letter or is the literal repository format blob, so a probe
// blob cannot be mistaken for content by a listing that filters on the
// vendor's own prefixes, and cannot collide with one either. Probe blobs
// are deleted by the probe that wrote them; one left behind by a killed
// process is inert.
const probePrefix = "_backupd_probe_"

// probeSize is how many bytes the cheap probe writes. It is larger than
// one range read so that a partial read can be checked against an offset
// that is neither the start nor the end of the blob, which is the case a
// storage that ignores Range headers gets wrong while looking fine.
const probeSize = 256

// probeRangeOffset and probeRangeLength are the range the cheap probe
// reads back: an offset that is neither the start nor the end, because a
// storage that ignores the range and returns the whole object passes any
// check anchored at zero.
const (
	probeRangeOffset = 64
	probeRangeLength = 32
)

// largeProbeSize is how many bytes the create-time probe writes, and the
// number is the vendor's own largest write plus a margin.
//
// # Why a second, large probe exists
//
// A 256-byte PUT proves almost nothing about a bucket that will be asked
// to hold 20 MiB pack blobs. The vendor's default MaxPackSize is 20 MiB
// (repo/initialize.go and repo/format/format_provider.go both default it
// to 20<<20), so this is the size of the largest single object this
// product will ever ask a bucket to store, written through exactly the
// code path production writes it through, and compared back BYTE FOR
// BYTE. What that catches is the whole class of endpoint that answers 200
// to everything and mangles large bodies: a chunked-signature stream
// reassembled wrongly, a proxy with a body limit, a gateway that silently
// truncates.
//
// # On "multipart"
//
// This size is also above minio-go's 16 MiB part size, which is what the
// review that asked for this probe was aiming at. It is worth recording
// that the vendor does NOT use multipart uploads for blobs at this pin:
// repo/blob/s3/s3_storage.go:176 sets DisableMultipart on every PutObject,
// with a comment saying Kopia's own packing makes further splitting
// pointless. So a backend that corrupts ONLY multipart uploads cannot
// corrupt this product's repository writes today -- and if a future
// version bump drops that flag, this probe is already over the threshold
// and covers it without needing to be revisited.
const largeProbeSize = 21 << 20

// multipartPartSize is minio-go's part size, restated here because it is
// the offset the large probe's range read straddles. See largeProbeSize.
const multipartPartSize = 16 << 20

// probeOptions selects how much a storage probe is prepared to spend.
//
// The distinction is create versus health, and it is a cost decision made
// once: CreateRepository decides whether a target may hold a repository at
// all, and 21 MiB of proof is cheap exactly once. Health runs before every
// backup, and a 21 MiB upload per preflight would make the health check
// the most expensive thing in a cycle for a NAS on a domestic uplink.
// Everything a develop-over-time fault can break -- read-after-write, the
// listing, the range, the delete -- is in both.
type probeOptions struct {
	LargeBody bool
}

// maxClockSkew is how far this machine's clock may differ from the
// storage's own before health says so.
//
// The vendor's storage contract allows "small clock skew up to minutes".
// Five minutes is inside that and outside anything NTP leaves behind on a
// working host, which makes a warning at this threshold actionable: it
// means time synchronisation is not running, not that it is running
// normally.
const maxClockSkew = 5 * time.Minute

// Option configures an Adapter.
//
// There is one, and it is a clock. The pattern is variadic so that
// New() keeps meaning "the production adapter", which is what every
// existing call site says and what production wiring should keep saying.
type Option func(*Adapter)

// WithClock replaces the clock this adapter compares storage timestamps
// against.
//
// It exists for exactly one reason: clock skew cannot be tested without a
// disagreeing clock, and the alternatives are to change the machine's time
// or to leave the check unproven. Production never calls this.
func WithClock(now func() time.Time) Option {
	return func(a *Adapter) {
		if now != nil {
			a.now = now
		}
	}
}

// CreateRepository implements backupengine.Engine.
//
// The order here is the contract. Storage is probed BEFORE the repository
// format is written, so a target that cannot support a repository never
// gets one: the refusal leaves an empty bucket or an empty directory,
// which is a state an operator can fix, rather than a half-usable
// repository they will find out about during a restore.
func (a *Adapter) CreateRepository(ctx context.Context, loc backupengine.RepositoryLocation) error {
	if err := validate(loc); err != nil {
		return err
	}

	st, release, err := a.openStorage(ctx, loc, true)
	if err != nil {
		return err
	}

	defer release()
	defer st.Close(ctx) //nolint:errcheck

	if _, err := a.probeStorage(ctx, st, probeOptions{LargeBody: loc.Kind == backupengine.LocationS3}); err != nil {
		return err
	}

	passphrase, err := secretref.Resolve(ctx, loc.Passphrase)
	if err != nil {
		return fmt.Errorf("kopia: resolving the repository passphrase: %w", err)
	}

	// NewRepositoryOptions is left at its defaults on purpose. Content
	// format, hash, encryption and splitter defaults are the vendor's own
	// recommended set and they move forward with the pin; overriding them
	// here would freeze this repository's format at whatever looked good on
	// the day this was written, and "it was the default at creation time" is
	// a better answer to a future format question than a stale opinion.
	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, passphrase.Reveal()); err != nil {
		if errors.Is(err, format.ErrAlreadyInitialized) {
			return backupengine.ErrRepositoryExists
		}

		return fmt.Errorf("initializing repository: %w", err)
	}

	return nil
}

// OpenRepository implements backupengine.Engine.
func (a *Adapter) OpenRepository(ctx context.Context, loc backupengine.RepositoryLocation) (backupengine.Repository, error) {
	if err := validate(loc); err != nil {
		return nil, err
	}

	configPath, err := configPath(loc)
	if err != nil {
		return nil, err
	}

	passphrase, err := secretref.Resolve(ctx, loc.Passphrase)
	if err != nil {
		return nil, fmt.Errorf("kopia: resolving the repository passphrase: %w", err)
	}

	st, release, err := a.openStorage(ctx, loc, false)
	if err != nil {
		return nil, err
	}

	// Connect is unconditional, and that is the whole of it: it writes our
	// connection parameters to the config file from the storage the caller
	// asked for, and verifies them by opening and closing the repository, so
	// a bad passphrase fails here rather than on first use. Reusing an
	// existing config file because one happens to be there is the version of
	// this code that hands back a repository nobody asked for -- the config
	// records which storage it is connected to, and a caller whose
	// repository moved would keep writing snapshots into the old one,
	// successfully and silently, while the new location stayed empty.
	//
	// It is also what makes the credential registration behind a bucket
	// connection sound: the config this process is about to open is always
	// one this process just wrote, naming a registration this process
	// holds. See s3connection.go.
	//
	// The storage handle is only needed to read the format blob; the
	// repository opened below builds its own from the config file.
	connectErr := repo.Connect(ctx, configPath, st, passphrase.Reveal(), connectOptions(loc))

	if err := st.Close(ctx); err != nil {
		release()

		return nil, fmt.Errorf("closing repository storage: %w", err)
	}

	if connectErr != nil {
		release()

		return nil, translate(connectErr, "connecting to repository")
	}

	if err := hardenStateDir(loc); err != nil {
		release()

		return nil, err
	}

	rep, err := repo.Open(ctx, configPath, passphrase.Reveal(), &repo.Options{})
	if err != nil {
		release()

		return nil, translate(err, "opening repository")
	}

	// Maintenance is the one operation that needs blob-level access, so a
	// repository that cannot provide it is refused at open time rather than
	// at the first maintenance window, when nobody is watching. Every
	// storage this adapter registers satisfies this; a future API-server
	// location would not, and it should fail with this sentence rather than
	// a nil dereference.
	direct, ok := rep.(repo.DirectRepository)
	if !ok {
		release()

		if cerr := rep.Close(ctx); cerr != nil {
			return nil, fmt.Errorf("repository does not support maintenance, and closing it failed: %w", cerr)
		}

		return nil, errors.New("kopia: repository was opened without direct storage access, which maintenance requires")
	}

	// And the config file that was just written is read back through the
	// opened repository, because "we wrote the right thing" and "we are
	// talking to the right storage" are two different claims and only the
	// second one matters. One comparison turns the worst outcome available
	// to this adapter -- a snapshot successfully stored in the wrong
	// repository -- into a refusal at open time.
	if err := checkStorageIdentity(direct, loc); err != nil {
		release()

		if cerr := rep.Close(ctx); cerr != nil {
			return nil, fmt.Errorf("%w; closing it also failed: %w", err, cerr)
		}

		return nil, err
	}

	// The engine's own snapshot retention is turned off here, once per
	// repository, and enginepolicy.go is where the argument lives: the
	// vendor's uploader applies the repository's retention policy at every
	// mid-upload checkpoint, and its default policy would delete this
	// product's snapshots -- holds included -- during an unrelated backup.
	// It reads a manifest and writes nothing on a repository this product
	// has already neutralized.
	if err := disableEngineRetention(ctx, rep); err != nil {
		release()

		if cerr := rep.Close(ctx); cerr != nil {
			return nil, fmt.Errorf("%w; closing it also failed: %w", err, cerr)
		}

		return nil, err
	}

	return &repository{rep: rep, direct: direct, loc: loc, adapter: a, release: release}, nil
}

// LookupSnapshot implements backupengine.Repository.
func (r *repository) LookupSnapshot(ctx context.Context, id backupengine.SnapshotID) (backupengine.SnapshotInfo, error) {
	man, err := r.load(ctx, id)
	if err != nil {
		return backupengine.SnapshotInfo{}, err
	}

	return snapshotInfo(man), nil
}

// Health implements backupengine.Repository.
//
// It proves reachability rather than assuming it. A repository handle
// survives a network partition and a NAS going to sleep, so "we hold an
// open repository" is not evidence about anything, and a health check that
// reported it as evidence would be green for exactly as long as it was
// useless.
//
// The probe is a write, a read back, a listing and a delete against the
// real storage, which is the same probe that decides at create time
// whether a target can hold a repository at all. Running the same code in
// both places is deliberate: a storage that DEVELOPS one of these faults
// -- a bucket policy that starts denying deletes, a filesystem that fills
// up -- is as interesting as one that never had the property. What it does
// NOT repeat is the create-time large-body write: see probeOptions.
//
// # Both return values are populated, always
//
// A failure returns a report AND an error, and neither is redundant. The
// error is for the caller that has to refuse -- a backup must not start
// against storage that cannot store -- and the report is for the caller
// that has to DISPLAY: "unreachable" and "healthy" are two states an
// operator's dashboard has to be able to show differently, and a zero
// report beside an error cannot say which one this is. Reachable is false
// for every probe failure, deliberately, including the ones where the
// endpoint answered: a storage that accepts a blob and then does not list
// it is not reachable FOR A REPOSITORY'S PURPOSES, and softening that into
// "reachable with a warning" is how a caller ends up proceeding.
func (r *repository) Health(ctx context.Context) (backupengine.HealthReport, error) {
	st, release, err := r.adapter.openStorage(ctx, r.loc, false)
	if err != nil {
		return unreachable(err), err
	}

	defer release()
	defer st.Close(ctx) //nolint:errcheck

	skew, err := r.adapter.probeStorage(ctx, st, probeOptions{})
	if err != nil {
		return unreachable(err), err
	}

	report := backupengine.HealthReport{Reachable: true}

	if skew < 0 {
		skew = -skew
	}

	if skew > maxClockSkew {
		report.Warnings = append(report.Warnings, backupengine.HealthWarning{
			Kind: backupengine.HealthWarningClockSkew,
			Detail: fmt.Sprintf(
				"this machine's clock and the repository storage disagree by %s, more than the %s a repository tolerates; "+
					"maintenance locks, content-retention windows and snapshot times are all expressed in timestamps, so a clock this far out "+
					"can let one process treat another's maintenance lock as expired or make freshly written content look old enough to reclaim. "+
					"Fix time synchronisation on this host",
				skew.Round(time.Second), maxClockSkew),
		})
	}

	return report, nil
}

// unreachable is the report that accompanies a failed health probe.
//
// The warning carries the failure's own sentence rather than a category,
// because what an operator needs from "unreachable" is WHICH part failed:
// a bucket policy that denies deletes, a NAS that is asleep and a
// credential that expired are the same Reachable=false and three different
// jobs to do. The error text is this adapter's own and carries no
// credential; the probe and the resolver are the two places that guarantee
// it, and both are tested for it.
func unreachable(err error) backupengine.HealthReport {
	return backupengine.HealthReport{
		Reachable: false,
		Warnings: []backupengine.HealthWarning{{
			Kind:   backupengine.HealthWarningUnreachable,
			Detail: err.Error(),
		}},
	}
}

// Stats implements backupengine.Repository.
//
// Blobs and PhysicalBytes come from the storage's own listing rather than
// from the repository index, because the question is what this repository
// is costing and the answer to that includes blobs an interrupted
// maintenance left behind, which the index does not mention.
//
// # Sources counts BACKUP SETS, not the engine's own sources
//
// RepositoryStats.Sources is documented as the co-tenancy number: an
// isolated Repository Domain whose repository reports more than one is a
// boundary that has already been crossed. The vendor's own source count
// cannot answer that question. A streaming backup set writes one Kopia
// source PER OBJECT (see stream.go and #782's sink), so one set of forty
// database dumps would report forty "sources" and an isolation check built
// on that number would fire on a domain nobody has crossed -- and, worse,
// two sets that happened to write one object each would report two and be
// indistinguishable from the breach this number exists to find.
//
// So the count is over the backup-set TAG every snapshot this product
// writes carries (backupengine.TagKeyBackupSet, set by the sink that owns
// the snapshot), which is the identity the isolation rule in
// model.RepositoryDomain.MayShare is actually expressed in.
//
// Snapshots that carry no set tag are counted as ONE unattributed tenant
// rather than as none. A snapshot in this repository that this product did
// not attribute is still content sharing the domain's key, and reporting
// zero for it would mean an operator who ran the vendor's CLI against
// their own bucket saw an isolated domain reporting no co-tenants while
// holding somebody else's snapshots.
func (r *repository) Stats(ctx context.Context) (backupengine.RepositoryStats, error) {
	ids, err := snapshot.ListSnapshotManifests(ctx, r.rep, nil, nil)
	if err != nil {
		return backupengine.RepositoryStats{}, fmt.Errorf("listing repository snapshots: %w", err)
	}

	stats := backupengine.RepositoryStats{Snapshots: len(ids)}

	manifests, err := snapshot.LoadSnapshots(ctx, r.rep, ids)
	if err != nil {
		return backupengine.RepositoryStats{}, fmt.Errorf("loading repository snapshots: %w", err)
	}

	sets := make(map[string]struct{}, len(manifests))

	for _, man := range manifests {
		// LoadSnapshots leaves a nil entry for a manifest it could not
		// parse rather than failing the batch. One of those is a snapshot
		// whose tags cannot be read, which is the unattributed case and is
		// counted as such.
		if man == nil {
			sets[""] = struct{}{}

			continue
		}

		sets[man.Tags[backupengine.TagKeyBackupSet]] = struct{}{}
	}

	stats.Sources = len(sets)

	// The empty prefix lists everything, which is what "physical size"
	// means. A prefix-filtered count would silently exclude whichever blob
	// kinds this code did not think of, and the numbers would look
	// plausible.
	if err := r.direct.BlobReader().ListBlobs(ctx, "", func(m blob.Metadata) error {
		stats.Blobs++
		stats.PhysicalBytes += m.Length

		return nil
	}); err != nil {
		return backupengine.RepositoryStats{}, fmt.Errorf("listing repository storage: %w", err)
	}

	return stats, nil
}

// probeStorage checks that a storage target actually provides what a
// repository needs, and reports the clock skew it measured on the way.
//
// Every property below is one the vendor's blob.Storage doc requires, and
// every one of them is something a partial S3 implementation gets wrong
// while answering every request with a 200:
//
//   - a written blob is immediately readable (read-after-write on GET);
//   - a written blob is immediately visible in a LISTING, which is the one
//     most commonly missing, because a repository finds its indexes by
//     listing and an index it cannot see is content it has forgotten;
//   - a range read returns that range, not the whole object, which is how
//     every content read after the first is served;
//   - a delete removes the blob, and the listing agrees.
//
// The skew comes from the metadata the storage itself reports for the blob
// that was just written, which for S3 is the server's Last-Modified and is
// therefore a real comparison against a different machine's clock.
//
// A failure is ErrStorageUnsupported, wrapped with which property failed,
// because "this endpoint is not usable" and "this endpoint is not usable
// BECAUSE listings are eventually consistent" are the same refusal to a
// caller and completely different information to the operator who has to
// choose another target.
func (a *Adapter) probeStorage(ctx context.Context, st blob.Storage, opts probeOptions) (time.Duration, error) {
	meta, err := a.probeBlob(ctx, st, probeSize, probeRangeOffset, probeRangeLength)
	if err != nil {
		return 0, err
	}

	if opts.LargeBody {
		// The range straddles the part boundary a client that did split
		// the upload would have used, so a backend that stitches parts
		// back together in the wrong order fails the range read as well as
		// the whole read.
		if _, err := a.probeBlob(ctx, st, largeProbeSize, multipartPartSize-32, 64); err != nil {
			return 0, err
		}
	}

	if meta.Timestamp.IsZero() {
		return 0, fmt.Errorf(
			"%w: %s reports no timestamp for a blob it just stored, and a repository's locks, retention windows and snapshot times are all timestamps",
			backupengine.ErrStorageUnsupported, st.DisplayName())
	}

	return a.now().Sub(meta.Timestamp), nil
}

// probeBlob runs the whole property sequence against one blob of a given
// size and reports what the storage said about it.
func (a *Adapter) probeBlob(ctx context.Context, st blob.Storage, size int, offset, length int64) (blob.Metadata, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return blob.Metadata{}, fmt.Errorf("kopia: generating a storage probe id: %w", err)
	}

	id := blob.ID(probePrefix + hex.EncodeToString(suffix))

	// Random bytes, so no layer between here and the storage can make the
	// comparison pass by compressing the payload away, and so a backend
	// that returns a plausible-looking buffer of its own is caught.
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		return blob.Metadata{}, fmt.Errorf("kopia: generating storage probe content: %w", err)
	}

	meta, err := blob.PutBlobAndGetMetadata(ctx, st, id, probeBytes(payload), blob.PutOptions{})
	if err != nil {
		return blob.Metadata{}, fmt.Errorf("%w: %s cannot store a blob of %d bytes: %w",
			backupengine.ErrStorageUnsupported, st.DisplayName(), size, err)
	}

	// The delete is deferred so that a failure anywhere below still cleans
	// up: a probe blob left in a bucket is harmless but it is litter, and
	// litter in a storage target somebody is about to be told is unusable
	// is litter nobody will come back for.
	defer st.DeleteBlob(ctx, id) //nolint:errcheck // the checked delete is below; this is the failure path

	if err := probeReadBack(ctx, st, id, payload, offset, length); err != nil {
		return blob.Metadata{}, err
	}

	if err := probeListing(ctx, st, id, true); err != nil {
		return blob.Metadata{}, err
	}

	if err := st.DeleteBlob(ctx, id); err != nil {
		return blob.Metadata{}, fmt.Errorf("%w: %s cannot delete a blob: %w",
			backupengine.ErrStorageUnsupported, st.DisplayName(), err)
	}

	if err := probeListing(ctx, st, id, false); err != nil {
		return blob.Metadata{}, err
	}

	return meta, nil
}

// probeReadBack checks read-after-write for both a whole read and a range
// read.
//
// The range is the caller's, because where an interesting offset IS
// depends on how big the blob is: for the small probe any offset that is
// neither the start nor the end catches a storage that ignores ranges, and
// for the large one the offset that matters is the part boundary an upload
// of that size might have been split at.
func probeReadBack(ctx context.Context, st blob.Storage, id blob.ID, want []byte, offset, length int64) error {
	var buf probeBuffer

	if err := st.GetBlob(ctx, id, 0, -1, &buf); err != nil {
		return fmt.Errorf("%w: %s cannot read back a blob of %d bytes it just stored: %w",
			backupengine.ErrStorageUnsupported, st.DisplayName(), len(want), err)
	}

	if !bytes.Equal(buf.Bytes(), want) {
		// The byte comparison is the assertion, not the length: a backend
		// that corrupts a large upload -- by reassembling parts in the
		// wrong order, or by mishandling a chunked-signature stream --
		// returns exactly the right number of exactly the wrong bytes.
		return fmt.Errorf("%w: %s returned %d bytes that are not the %d that were just written to it",
			backupengine.ErrStorageUnsupported, st.DisplayName(), buf.Length(), len(want))
	}

	buf.Reset()

	if err := st.GetBlob(ctx, id, offset, length, &buf); err != nil {
		return fmt.Errorf("%w: %s cannot serve a partial read, which is how every content read after the first one is served: %w",
			backupengine.ErrStorageUnsupported, st.DisplayName(), err)
	}

	if !bytes.Equal(buf.Bytes(), want[offset:offset+length]) {
		return fmt.Errorf(
			"%w: %s returned %d bytes for a %d-byte range read at offset %d, so it is not honouring ranges; "+
				"a repository reading content at an offset would silently get the wrong bytes",
			backupengine.ErrStorageUnsupported, st.DisplayName(), buf.Length(), length, offset)
	}

	return nil
}

// probeListing checks that a listing agrees with what was just written or
// deleted, which is the property most often missing and the one whose
// absence is unrecoverable rather than merely wrong.
func probeListing(ctx context.Context, st blob.Storage, id blob.ID, want bool) error {
	var found bool

	if err := st.ListBlobs(ctx, blob.ID(probePrefix), func(m blob.Metadata) error {
		if m.BlobID == id {
			found = true
		}

		return nil
	}); err != nil {
		return fmt.Errorf("%w: %s cannot list blobs: %w", backupengine.ErrStorageUnsupported, st.DisplayName(), err)
	}

	switch {
	case want && !found:
		return fmt.Errorf(
			"%w: %s does not list a blob immediately after storing it, so its listings are eventually consistent; "+
				"a repository finds its indexes by listing, and an index it cannot see yet is content it has forgotten",
			backupengine.ErrStorageUnsupported, st.DisplayName())
	case !want && found:
		return fmt.Errorf(
			"%w: %s still lists a blob it reported deleting, so a repository could not tell reclaimed space from occupied space",
			backupengine.ErrStorageUnsupported, st.DisplayName())
	default:
		return nil
	}
}

// probeBytes adapts a byte slice to the vendor's blob.Bytes without
// reaching into its internal gather package. It is read-only and lives
// exactly as long as one probe.
type probeBytes []byte

func (b probeBytes) Length() int { return len(b) }

func (b probeBytes) Reader() io.ReadSeekCloser { return nopSeekCloser{bytes.NewReader(b)} }

func (b probeBytes) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)

	return int64(n), err
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// probeBuffer is the vendor's blob.OutputBuffer over a plain buffer, for
// probeBytes' reason.
type probeBuffer struct{ buf bytes.Buffer }

func (b *probeBuffer) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *probeBuffer) Reset()                      { b.buf.Reset() }
func (b *probeBuffer) Length() int                 { return b.buf.Len() }
func (b *probeBuffer) Bytes() []byte               { return b.buf.Bytes() }

// validate rejects a location this adapter cannot serve, before it does
// any storage work on the strength of it.
func validate(loc backupengine.RepositoryLocation) error {
	if loc.Domain.IsZero() {
		return errors.New("kopia: repository location has no repository domain; a repository with no stable id cannot be reopened after a restart")
	}

	if loc.Passphrase.IsZero() {
		return errors.New("kopia: repository location declares no passphrase source, and every repository this adapter creates is encrypted")
	}

	if err := loc.Passphrase.Validate(); err != nil {
		return fmt.Errorf("kopia: repository passphrase: %w", err)
	}

	switch loc.Kind {
	case backupengine.LocationLocal:
		if loc.Root == "" {
			return errors.New("kopia: a local repository location has no backup root")
		}

		if _, err := backupengine.ReservedLocalDir(loc.Root, loc.Domain); err != nil {
			return fmt.Errorf("kopia: %w", err)
		}

		return nil
	case backupengine.LocationS3:
		return validateS3(loc)
	default:
		return fmt.Errorf("kopia: unsupported repository kind %q", loc.Kind)
	}
}

// validateS3 refuses an S3 location that cannot be reached, saying which
// part is missing.
//
// The endpoint is parsed rather than passed through, and the two refusals
// below are the two mistakes that are otherwise diagnosed by a provider
// error hundreds of lines away: a scheme that is not http or https (an
// "s3://" endpoint, which is a bucket URL and not an endpoint at all), and
// an endpoint carrying a path, which is a bucket name typed into the wrong
// field.
func validateS3(loc backupengine.RepositoryLocation) error {
	if loc.S3.Bucket == "" {
		return errors.New("kopia: an s3 repository location has no bucket")
	}

	if strings.Contains(loc.S3.Bucket, "/") {
		return fmt.Errorf("kopia: bucket %q contains a path separator; a bucket and a key prefix written into one field is a destination that does not exist", loc.S3.Bucket)
	}

	if loc.StateDir == "" {
		return errors.New("kopia: an s3 repository location has no state directory, and its connection config and index cache have to live somewhere local")
	}

	if loc.S3.Credentials.IsZero() {
		return errors.New("kopia: an s3 repository location declares no credential source")
	}

	if err := loc.S3.Credentials.Validate(); err != nil {
		return fmt.Errorf("kopia: s3 credentials: %w", err)
	}

	if loc.S3.Endpoint == "" {
		if loc.S3.Region == "" {
			return errors.New("kopia: an s3 repository location with no endpoint needs a region, because the endpoint is derived from it")
		}

		return nil
	}

	u, err := url.Parse(loc.S3.Endpoint)
	if err != nil {
		return fmt.Errorf("kopia: endpoint %q is not a URL: %w", loc.S3.Endpoint, err)
	}

	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("kopia: endpoint %q has scheme %q; an endpoint is the service address, so it is http or https",
			loc.S3.Endpoint, u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("kopia: endpoint %q has no host", loc.S3.Endpoint)
	}

	if p := strings.Trim(u.Path, "/"); p != "" {
		return fmt.Errorf("kopia: endpoint %q carries the path %q; the bucket goes in the bucket field and the key namespace in the prefix field",
			loc.S3.Endpoint, p)
	}

	return nil
}

// openStorage builds one of the two backends this adapter registers, and
// returns the release function for whatever it had to hold open to do it.
//
// The release is separate from Storage.Close, and it has to be: for a
// bucket repository what is being held is the in-memory credential
// registration the persisted connection points at (see s3connection.go),
// and that registration has to outlive the storage handle this adapter
// hands to repo.Connect -- repo.Open builds its own storage from the
// config afterwards, and would find the registration already gone. It is
// always non-nil, so a caller can defer it without a check.
func (a *Adapter) openStorage(ctx context.Context, loc backupengine.RepositoryLocation, create bool) (blob.Storage, func(), error) {
	switch loc.Kind {
	case backupengine.LocationLocal:
		st, err := openFilesystemStorage(ctx, loc, create)

		return st, func() {}, err
	case backupengine.LocationS3:
		return openS3Storage(ctx, loc, create)
	default:
		return nil, func() {}, fmt.Errorf("kopia: unsupported repository kind %q", loc.Kind)
	}
}

// openFilesystemStorage opens the reserved directory a local repository's
// blobs live in.
//
// The path is derived from the backup root and the repository's id rather
// than taken from the caller, and the whole argument for that is in
// backupengine/reserved.go: a repository's pack files in a directory an
// artifact catalog walks are pack files something eventually prunes.
func openFilesystemStorage(ctx context.Context, loc backupengine.RepositoryLocation, create bool) (blob.Storage, error) {
	dir, err := localDir(loc)
	if err != nil {
		return nil, err
	}

	if create {
		// 0700, and the reserved parent too: the backup root is what a NAS
		// deployment exports, so the directory holding a repository's
		// encrypted blobs is created reachable by this manager and nobody
		// else. The vendor's own storage would create the leaf with its
		// default mode; creating it here is how the mode gets decided by
		// this project.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating the reserved repository directory %s: %w", dir, err)
		}
	}

	st, err := filesystem.New(ctx, &filesystem.Options{Path: dir}, create)
	if err != nil {
		// filesystem.New refuses a path it cannot stat, which for a
		// non-creating open is the same operator situation as a directory
		// that holds no repository.
		if !create && errors.Is(err, os.ErrNotExist) {
			return nil, backupengine.ErrRepositoryNotFound
		}

		return nil, fmt.Errorf("opening repository storage at %s: %w", dir, err)
	}

	return st, nil
}

// openS3Storage opens the bucket an S3 repository lives in.
//
// It registers the operator's credential REFERENCE in memory and builds
// the storage from that registration, which is the same path repo.Open
// takes when it rebuilds the storage from the persisted connection. See
// s3connection.go for why the credential is resolved per storage build and
// never written down, and why the registration is what the caller has to
// release.
func openS3Storage(ctx context.Context, loc backupengine.RepositoryLocation, create bool) (blob.Storage, func(), error) {
	handle, err := registerS3Credentials(loc.S3.Credentials)
	if err != nil {
		return nil, func() {}, err
	}

	release := func() { releaseS3Credentials(handle) }

	conn, err := s3ConnectionFor(loc, handle)
	if err != nil {
		release()

		return nil, func() {}, err
	}

	st, err := newS3Storage(ctx, &conn, create)
	if err != nil {
		release()

		return nil, func() {}, err
	}

	return st, release, nil
}

// s3Prefix is the key namespace one repository occupies inside a bucket:
// the operator's prefix, then the repository's own id.
//
// The id is part of the key layout rather than left to the operator's
// prefix for the same reason it names the local directory: two
// repositories sharing one bucket must not share one key namespace, and
// relying on an operator to remember a distinct prefix per domain is
// relying on them to avoid a mistake whose symptom is two repositories
// overwriting each other's format blob.
func s3Prefix(loc backupengine.RepositoryLocation) string {
	base := strings.Trim(loc.S3.Prefix, "/")
	if base == "" {
		return loc.Domain.String() + "/"
	}

	return base + "/" + loc.Domain.String() + "/"
}

// localDir is the one place the reserved local path is resolved for this
// adapter.
func localDir(loc backupengine.RepositoryLocation) (string, error) {
	dir, err := backupengine.ReservedLocalDir(loc.Root, loc.Domain)
	if err != nil {
		return "", fmt.Errorf("kopia: %w", err)
	}

	return dir, nil
}

// stateDir is where this process keeps local state about one repository.
//
// For a local repository an unset StateDir resolves under the reserved
// namespace, which is the only place under a backup root that artifact
// management may not touch. For S3 there is nothing to derive it from,
// which is why validate insists on it.
func stateDir(loc backupengine.RepositoryLocation) (string, error) {
	if loc.StateDir != "" {
		if !filepath.IsAbs(loc.StateDir) {
			return "", fmt.Errorf("kopia: state directory %q is relative; connection config and cache written relative to a working directory are state this process can lose track of", loc.StateDir)
		}

		return filepath.Clean(loc.StateDir), nil
	}

	if loc.Kind != backupengine.LocationLocal {
		return "", errors.New("kopia: this repository kind has no backup root to derive a state directory from, so one must be configured")
	}

	return backupengine.ReservedLocalStateDir(loc.Root)
}

// configPath is the connection-config file for one repository, created
// under the state directory.
//
// It is named after the repository's id, which is what makes a restart
// able to find it again, and it is one file per repository, which is what
// keeps two repositories from sharing connection state.
func configPath(loc backupengine.RepositoryLocation) (string, error) {
	dir, err := stateDir(loc)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("kopia: creating the repository state directory %s: %w", dir, err)
	}

	return filepath.Join(dir, loc.Domain.String()+".config"), nil
}

// connectOptions is what this adapter records about a connection, and
// LOCAL CACHING IS DELIBERATELY NOT PART OF IT.
//
// # Why there is no cache
//
// This used to set content.CachingOptions.CacheDirectory under the state
// directory and nothing else, which was dead code: the vendor's
// setupCachingOptionsWithDefaults (repo/caching.go) short-circuits on
// ContentCacheSizeBytes == 0 and writes an empty CachingOptions, so a
// directory without a size cached nothing. Reviewing that found the dead
// code and the obvious repair was to set the sizes.
//
// The sizes were set, and the repair is not available to this product. The
// vendor's caching switch is all-or-nothing -- a zero content size wipes
// the metadata and index caches too -- and with the content cache on, a
// repository missing a pack blob VERIFIES CLEAN: the content the verifier
// wants is in <StateDir>/<domain>.cache/contents, so the read succeeds
// against a bucket that no longer holds it. That was measured, not
// reasoned about, by deleting a pack blob and verifying twice; the
// verification failed only once the cache directory was removed.
//
// That is disqualifying here in a way it would not be in a file-sync tool.
// This product's claim is that a backup is restorable, the check behind
// that claim is Verify, and the case where the cache is warmest is
// verify-immediately-after-backup -- so caching would be most likely to
// lie in exactly the flow that matters most. The same applies to Restore:
// a restore satisfied from local cache proves a restore nobody can repeat.
//
// TestVerifyReportsDamageAsBothErrorAndFindings is the guard on this
// decision rather than a test of its own: it fails if caching is turned
// on, which is how a future attempt to enable it will find this comment.
//
// # What it costs, and what would have to change first
//
// Every content, metadata and index read goes to the storage, which for a
// bucket is a GET per index lookup on a link an operator pays for.
// Re-enabling caching needs a read path that BYPASSES the cache for
// verification and restore -- the vendor offers none at this pin -- and
// the cache would then belong under stateDir(loc), which is where the
// deleted CacheDirectory pointed and the only place under a backup root
// artifact management may not touch.
func connectOptions(loc backupengine.RepositoryLocation) *repo.ConnectOptions {
	return &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{
			Description: "backupd:" + loc.Domain.String(),
		},
	}
}

// hardenStateDir puts this project's modes on the state the vendor just
// wrote.
//
// The vendor creates the config directory 0700 and writes the config file
// through its own atomic-write helper, so this is belt and braces rather
// than a repair -- but the state directory is where a repository's content
// cache lives, and content in a cache is repository content: readable by
// another local account is exactly as bad there as it is in the bucket.
// Asserting the modes here means this project decided them, and the test
// that reads them back is checking a decision rather than an upstream
// default that a version bump may revise.
func hardenStateDir(loc backupengine.RepositoryLocation) error {
	dir, err := stateDir(loc)
	if err != nil {
		return err
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("kopia: restricting the repository state directory %s: %w", dir, err)
	}

	path, err := configPath(loc)
	if err != nil {
		return err
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("kopia: restricting the repository connection config %s: %w", path, err)
	}

	return nil
}

// checkStorageIdentity refuses an open whose repository is not the storage
// the caller named.
//
// It reads the identity back out of the opened repository rather than
// trusting the config file this process just wrote, because the failure it
// exists to catch is precisely the one where the config file says
// something other than what was asked for: one config file per repository
// id makes that rare, and "rare" is not the same claim as "checked".
func checkStorageIdentity(direct repo.DirectRepository, loc backupengine.RepositoryLocation) error {
	ci := direct.BlobReader().ConnectionInfo()

	switch loc.Kind {
	case backupengine.LocationLocal:
		want, err := localDir(loc)
		if err != nil {
			return err
		}

		got, ok := filesystemPath(ci)
		if !ok {
			return fmt.Errorf(
				"kopia: this repository's config is connected to %q storage, and the request was for a local repository at %s",
				ci.Type, want)
		}

		if samePath(got, want) {
			return nil
		}

		return fmt.Errorf("kopia: this repository's config is connected to the repository at %s, not the requested %s", got, want)
	case backupengine.LocationS3:
		got, ok := s3Identity(ci)
		if !ok {
			return fmt.Errorf(
				"kopia: this repository's config is connected to %q storage, and the request was for the s3 bucket %s",
				ci.Type, loc.S3.Bucket)
		}

		// The comparison is against the connection this adapter would
		// write for the location asked for, with the credential handle
		// left out of it: a handle differs on every open by construction,
		// and what is being checked is which bucket and which key
		// namespace, not which registration.
		want, err := s3ConnectionFor(loc, "")
		if err != nil {
			return err
		}

		if got.Bucket == want.Bucket && got.Prefix == want.Prefix && got.Endpoint == want.Endpoint {
			return nil
		}

		// Neither side of this message can carry a credential: the
		// persisted connection has no field one fits in.
		return fmt.Errorf(
			"kopia: this repository's config is connected to bucket %q prefix %q at endpoint %q, not the requested bucket %q prefix %q at endpoint %q",
			got.Bucket, got.Prefix, got.Endpoint, want.Bucket, want.Prefix, want.Endpoint)
	default:
		return fmt.Errorf("kopia: unsupported repository kind %q", loc.Kind)
	}
}

// filesystemPath reports the directory behind a filesystem storage's
// connection info, and whether it was a filesystem storage at all.
func filesystemPath(ci blob.ConnectionInfo) (string, bool) {
	switch cfg := ci.Config.(type) {
	case *filesystem.Options:
		return cfg.Path, true
	case filesystem.Options:
		return cfg.Path, true
	default:
		return "", false
	}
}

// samePath compares two directories as identities rather than as strings.
//
// The symlink resolution is not decoration: on darwin every temporary
// directory is reached through /var -> private/var, so two spellings of one
// directory are the normal case and not an exotic one.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}

	ra, aerr := filepath.EvalSymlinks(a)
	rb, berr := filepath.EvalSymlinks(b)

	if aerr != nil || berr != nil {
		return false
	}

	return ra == rb
}

// translate maps the vendor's errors this boundary has sentinels for onto
// them.
//
// Every case matches a typed value with errors.Is, never a message, for the
// reason internal/transport/rclone/errors.go spells out at length: upstream
// is allowed to reword any of these on any release, and this file is the only
// place in the repository that should need to notice.
func translate(err error, what string) error {
	switch {
	case errors.Is(err, repo.ErrRepositoryNotInitialized):
		return backupengine.ErrRepositoryNotFound
	case errors.Is(err, repo.ErrInvalidPassword):
		return backupengine.ErrPassphrase
	case errors.Is(err, os.ErrNotExist):
		return backupengine.ErrRepositoryNotFound
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}
