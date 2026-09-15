package miniointegration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/backupengine/kopia"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/transport"
	"github.com/retnd/retnd/core/internal/transport/rclone"
	"github.com/retnd/retnd/core/tests/machines"
)

// This file is EPIC K's verification ladder run against a real S3 API, and
// the measurement of what a second snapshot physically costs in a bucket.
//
// # Why the ladder is proved twice
//
// The unit ladder in internal/backupengine/kopia proves the rungs are four
// different claims; it does it against a repository in a directory, where
// "list the blobs" is a readdir and "read every byte back" never leaves the
// page cache. Both of those are the parts of a verification an S3
// repository actually pays for, and both are the parts an S3-compatible
// endpoint can get wrong while answering every small request correctly: a
// truncated or paginated listing makes the blob-existence half of
// content_sample report on a subset of the bucket, and a read path that
// mishandles ranged GETs fails only content_full and only over the
// network. A ladder whose two lower rungs are a LIST and its two upper
// rungs are thousands of GETs is therefore a different test here, and this
// is the row that says so.
//
// # Why the container is per test and nothing here is shared
//
// Every test in this file starts its own MinIO through machines, which
// removes it in t.Cleanup -- on the failure path too, which is what makes
// the container ephemeral rather than usually ephemeral. Nothing in this
// suite may point at a real bucket or at a server somebody else started:
// the corruption row deletes an object out from under a repository, and a
// test that could do that to storage it does not own would be a test that
// can destroy somebody's backup.

const (
	// s3VerifyFiles is how many files the verified tree holds.
	//
	// Twelve, for the reason the unit ladder picks it: a quarter of twelve
	// is exactly three, so a sampled report of three files means the
	// stride ran and cannot be confused with "some files were read". A
	// count whose quarter is not a whole number would make the strongest
	// assertion in this file an inequality.
	s3VerifyFiles = 12

	// s3VerifySmallFile is the size of eleven of those files.
	//
	// 64 KiB of incompressible bytes is far above the size at which the
	// engine inlines a file's content into its object id. Inlined files
	// live in no pack blob at all, so a tree of them would leave the
	// corruption row below with nothing to delete and the reuse
	// measurement with nothing to reuse.
	s3VerifySmallFile = 64 << 10

	// s3VerifyLargeFile is the one file big enough to dominate the
	// bucket.
	//
	// Four MiB rather than multipartPayload's 48: the large-body upload
	// path is already proved by TestS3RepositoryMatrix, and what this file
	// needs from a big file is different -- one file whose content is most
	// of the repository, so that a pack object holding it is unambiguous
	// to pick out and so that the reuse ratio below is a statement about
	// content rather than about index overhead. Every byte here is read
	// back over the network twice by the top two rungs, so the size is
	// also what keeps this suite's wall time in seconds.
	s3VerifyLargeFile = 4 << 20

	// s3VerifySamplePercent and s3VerifySampledFiles are the sampled
	// rung's request and the only answer it may give: a stride of
	// ceil(100/25) reads every fourth file of twelve.
	s3VerifySamplePercent = 25
	s3VerifySampledFiles  = 3

	// s3VerifyTreeBytes is the tree's logical size, restated as a
	// constant because the full rung's report is asserted to equal it
	// exactly.
	s3VerifyTreeBytes = int64(s3VerifyLargeFile + (s3VerifyFiles-1)*s3VerifySmallFile)

	// s3VerifyChangedFile is the one file the reuse measurement rewrites.
	s3VerifyChangedFile = "nested/small-00.bin"
)

// TestS3VerificationLadder walks the four rungs, in order, against one
// repository in one bucket.
//
// The order is the claim and the container is the reason it is one test:
// each rung is asserted to have done MORE than the one below it against
// the same snapshot, which is a statement about four reports of the same
// storage. Four independent tests would be four MinIO containers proving
// less.
func TestS3VerificationLadder(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	bucket := bucketName(t, fixture)
	rep, _ := s3Repository(t, fixture, bucket)

	srcDir := s3VerifySourceTree(t)
	snap := s3VerifySnapshot(t, rep, srcDir, "the s3 verification ladder")

	// --- structural: the tree resolves, and nothing is read -----------------

	structural, err := rep.Verify(ctx, snap.ID, backupengine.VerifyRequest{Level: model.LevelStructural})
	if err != nil {
		t.Fatalf("structural Verify over s3: %v (findings: %v)", err, structural.Errors)
	}

	if structural.Level != model.LevelStructural {
		t.Errorf("a structural verification over s3 reported level %q", structural.Level)
	}

	if structural.ObjectsVerified < s3VerifyFiles {
		t.Errorf("a structural verification resolved %d objects for a tree of %d files plus its directories",
			structural.ObjectsVerified, s3VerifyFiles)
	}

	if structural.FilesVerified != 0 || structural.BytesVerified != 0 {
		t.Errorf("a structural verification over s3 claims to have read %d file(s) and %d byte(s); it reads none, "+
			"and over a bucket that difference is the entire cost of the rung",
			structural.FilesVerified, structural.BytesVerified)
	}

	// The S3-specific half of the structural claim: no bucket listing
	// either. This is the rung that runs on every backup, and a listing
	// per backup is a request an operator is billed for and a latency they
	// wait for, so a structural run that quietly listed the bucket would
	// be a different product decision wearing this rung's name.
	if structural.BlobsChecked != 0 {
		t.Errorf("a structural verification checked %d blob(s); it lists no storage, which is what makes it the rung that can run every night",
			structural.BlobsChecked)
	}

	// --- content_sample: a listing over a real S3 prefix, plus a stride ----

	sampled, err := rep.Verify(ctx, snap.ID, backupengine.VerifyRequest{
		Level:         model.LevelContentSample,
		SamplePercent: s3VerifySamplePercent,
	})
	if err != nil {
		t.Fatalf("sampled Verify over s3: %v (findings: %v)", err, sampled.Errors)
	}

	if sampled.Level != model.LevelContentSample {
		t.Errorf("a sampled verification over s3 reported level %q", sampled.Level)
	}

	if sampled.FilesVerified != s3VerifySampledFiles {
		t.Errorf("a %d%% sample of %d files read %d of them, want exactly %d; over s3 a sample that is a coin flip "+
			"also means two nights' reports cannot be compared",
			s3VerifySamplePercent, s3VerifyFiles, sampled.FilesVerified, s3VerifySampledFiles)
	}

	if sampled.BytesVerified == 0 || sampled.BytesVerified >= s3VerifyTreeBytes {
		t.Errorf("a %d%% sample read %d bytes of a %d byte tree; a sample reads some and not all",
			s3VerifySamplePercent, sampled.BytesVerified, s3VerifyTreeBytes)
	}

	// The row this whole file exists for. Above structural, the integrity
	// half of a verification is "every pack blob the index names is in the
	// storage", and over s3 that is a LIST over a prefix -- paginated,
	// signed, and answered by somebody else's implementation. A report of
	// zero blobs here means the listing returned nothing and the
	// verification checked the sampled files alone, which is the shape in
	// which a missing object goes unnoticed.
	if sampled.BlobsChecked == 0 {
		t.Error("a sampled verification over a real S3 prefix checked no backing blobs; the blob listing is what finds " +
			"content the index references and the bucket does not hold, and a run that lists nothing finds nothing")
	}

	// --- content_full: every byte back over the wire ------------------------

	full, err := rep.Verify(ctx, snap.ID, backupengine.VerifyRequest{Level: model.LevelContentFull})
	if err != nil {
		t.Fatalf("full Verify over s3: %v (findings: %v)", err, full.Errors)
	}

	if full.Level != model.LevelContentFull {
		t.Errorf("a full verification over s3 reported level %q", full.Level)
	}

	if full.FilesVerified != s3VerifyFiles || full.BytesVerified != s3VerifyTreeBytes {
		t.Errorf("a full verification over s3 read %d file(s) and %d byte(s); the tree holds %d and %d",
			full.FilesVerified, full.BytesVerified, s3VerifyFiles, s3VerifyTreeBytes)
	}

	if full.FilesRestored != 0 {
		t.Errorf("a full verification restored %d file(s); reading content out of a bucket is not putting it back on a disk",
			full.FilesRestored)
	}

	// --- restore_drill: the bytes land on a disk and are the source's ------

	target := filepath.Join(t.TempDir(), "drill")

	drill, err := rep.Verify(ctx, snap.ID, backupengine.VerifyRequest{
		Level:         model.LevelRestoreDrill,
		RestoreTarget: target,
	})
	if err != nil {
		t.Fatalf("restore drill Verify over s3: %v (findings: %v)", err, drill.Errors)
	}

	if drill.Level != model.LevelRestoreDrill {
		t.Errorf("a restore drill over s3 reported level %q", drill.Level)
	}

	if drill.FilesRestored != s3VerifyFiles || drill.HashesMatched != s3VerifyFiles {
		t.Errorf("a restore drill over s3 restored %d file(s) and matched %d hash(es) for a tree of %d files",
			drill.FilesRestored, drill.HashesMatched, s3VerifyFiles)
	}

	// The drill compares the disk against the REPOSITORY. This compares
	// it against the ORIGINAL tree, which is the only comparison that
	// notices a repository faithfully holding the wrong bytes -- an
	// upload path that mangled content the same way on the way in and on
	// the way out.
	assertRestoredTree(t, srcDir, target)
}

// TestS3SecondSnapshotReusesPhysicalContent is the bucket-side half of the
// claim TestSnapshotTreeReusesContentOnASecondRun makes locally: a nightly
// snapshot of a tree that barely changed costs almost nothing NEW in the
// bucket.
//
// It is measured from the bucket's own listing rather than from the
// engine's byte counters, because what an operator is billed for and what
// a repository has to hold forever is what the storage physically holds.
// An engine that reported small writes while the bucket grew by the size
// of the tree every night would pass the local measurement and would still
// be the defect: storage cost that grows with the number of runs rather
// than with the amount of change.
func TestS3SecondSnapshotReusesPhysicalContent(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	bucket := bucketName(t, fixture)
	medium := fixture.MediumForBucket(bucket)
	rep, _ := s3Repository(t, fixture, bucket)

	srcDir := s3VerifySourceTree(t)

	first := s3VerifySnapshot(t, rep, srcDir, "the first night")

	firstObjects, firstBytes := bucketContents(t, medium)
	if firstBytes < s3VerifyTreeBytes/2 {
		t.Fatalf("the bucket holds %d bytes in %d object(s) after storing a %d byte incompressible tree; "+
			"the measurement below is a difference between two listings, so it means nothing if the first one is wrong",
			firstBytes, firstObjects, s3VerifyTreeBytes)
	}

	// One file of twelve, rewritten with new incompressible bytes of the
	// same length. Same length deliberately: a size change is something a
	// scanner can notice for free, and reuse that only worked when sizes
	// differed would be reuse of the wrong kind.
	s3VerifyRewriteFile(t, srcDir, s3VerifyChangedFile)

	second := s3VerifySnapshot(t, rep, srcDir, "the second night")
	if second.ID == first.ID {
		t.Fatalf("the second snapshot has the first one's id (%s); nothing was snapshotted twice", second.ID)
	}

	secondObjects, secondBytes := bucketContents(t, medium)
	growth := secondBytes - firstBytes

	// Two bounds, and both are needed.
	//
	// The lower one is not a formality: a growth of zero would mean the
	// changed bytes never reached the bucket, and a "reuse" test that
	// accepted zero would pass an engine that skipped the second snapshot
	// entirely. The changed file is 64 KiB of incompressible content, so
	// the bucket MUST have grown.
	if growth <= 0 {
		t.Errorf("the bucket grew by %d bytes (from %d to %d) after a snapshot of a tree with a rewritten file; "+
			"the new content was not stored", growth, firstBytes, secondBytes)
	}

	// The upper one is the reuse claim, stated as a ratio for
	// tree_test.go's reason: the numbers either side are a vendor's pack
	// and index sizes, and an absolute byte budget would be a test that
	// fails on a harmless upstream change while still passing an engine
	// that stopped reusing anything.
	//
	// A quarter, and the numbers behind it are measured rather than
	// guessed. What changed is one file of twelve, about 1.4% of the
	// tree; measured against MinIO, the second snapshot grows the bucket
	// by about 75 KiB -- the changed file plus a new index blob, a new
	// snapshot manifest and the directory manifests on the path to it --
	// against a 4.7 MiB first snapshot, and a run with every file
	// rewritten grows it by 4.9 MiB. A quarter therefore sits more than
	// an order of magnitude above what reuse actually costs and four
	// times below the failure it has to catch, which is what makes it a
	// measurement rather than a tripwire on fixed overhead.
	if wantBelow := firstBytes / 4; growth >= wantBelow {
		t.Errorf("a second snapshot of a tree with one file of %d changed grew the bucket by %d bytes "+
			"(%d object(s) to %d object(s)), want below a quarter of the first snapshot's %d (%d); content was not reused",
			s3VerifyFiles, growth, firstObjects, secondObjects, firstBytes, wantBelow)
	}

	// Reuse that cost the second snapshot its integrity is not a saving.
	// The second snapshot is the one that holds content it did not write,
	// so it is the one whose full verification and restore are the claim
	// worth making.
	full, err := rep.Verify(ctx, second.ID, backupengine.VerifyRequest{Level: model.LevelContentFull})
	if err != nil {
		t.Fatalf("full Verify of a snapshot that reused content: %v (findings: %v)", err, full.Errors)
	}

	if full.FilesVerified != s3VerifyFiles || full.BytesVerified != s3VerifyTreeBytes {
		t.Errorf("a full verification of the reusing snapshot read %d file(s) and %d byte(s); the tree holds %d and %d",
			full.FilesVerified, full.BytesVerified, s3VerifyFiles, s3VerifyTreeBytes)
	}

	target := filepath.Join(t.TempDir(), "drill")

	drill, err := rep.Verify(ctx, second.ID, backupengine.VerifyRequest{
		Level:         model.LevelRestoreDrill,
		RestoreTarget: target,
	})
	if err != nil {
		t.Fatalf("restore drill of a snapshot that reused content: %v (findings: %v)", err, drill.Errors)
	}

	if drill.FilesRestored != s3VerifyFiles || drill.HashesMatched != s3VerifyFiles {
		t.Errorf("the reusing snapshot restored %d file(s) and matched %d hash(es) for a tree of %d files",
			drill.FilesRestored, drill.HashesMatched, s3VerifyFiles)
	}

	// The reused bytes are the eleven files the second run did NOT write,
	// so this comparison against the tree as it is NOW is what proves the
	// second snapshot holds the new version of the changed file and the
	// old version of everything else.
	assertRestoredTree(t, srcDir, target)
}

// TestS3VerificationFindsAPackObjectDeletedFromTheBucket is the corruption
// row, and the failure it describes is the one an operator is most likely
// to actually meet in a bucket: an object that is gone.
//
// A lifecycle rule that expired it, a prefix somebody tidied, a
// replication target that never received it -- none of those touch the
// repository's index, so the index still names the blob and every content
// identifier in the snapshot still resolves. A structural verification
// over that bucket therefore PASSES, correctly, and anything that read
// that pass as evidence the backup is restorable would be advertising a
// restore point whose bytes are not there.
//
// The object is deleted through this product's own S3 client, which is the
// honest version of this damage: a real DELETE against a real endpoint,
// out of band, exactly as the operator's lifecycle rule would do it.
func TestS3VerificationFindsAPackObjectDeletedFromTheBucket(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	bucket := bucketName(t, fixture)
	medium := fixture.MediumForBucket(bucket)
	rep, loc := s3Repository(t, fixture, bucket)

	srcDir := s3VerifySourceTree(t)
	snap := s3VerifySnapshot(t, rep, srcDir, "a snapshot somebody's lifecycle rule will damage")

	// Closed before the damage and reopened with no local state
	// afterwards, for the reason the unit ladder's reopenDamaged gives: an
	// open repository holds index state in memory and a cache on disk, so
	// a verification that read either would be verifying this process
	// rather than the bucket.
	if err := rep.Close(ctx); err != nil {
		t.Fatalf("closing before damaging the bucket: %v", err)
	}

	gone := s3VerifyDeleteLargestPackObject(t, medium)

	if err := os.RemoveAll(loc.StateDir); err != nil {
		t.Fatalf("clearing the state directory: %v", err)
	}

	damaged, err := kopia.New().OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("reopening the repository after deleting one of its pack objects: %v", err)
	}

	t.Cleanup(func() { _ = damaged.Close(context.Background()) })

	structural, err := damaged.Verify(ctx, snap.ID, backupengine.VerifyRequest{Level: model.LevelStructural})
	if err != nil {
		t.Fatalf("a structural verification failed on a repository whose INDEX is intact: %v (findings: %v); "+
			"if object resolution has started reading pack blobs then this test's argument has changed and needs "+
			"rewriting, not deleting", err, structural.Errors)
	}

	if structural.Level != model.LevelStructural {
		t.Errorf("the passing structural verification reported level %q", structural.Level)
	}

	// And the rung above it, on the same bucket, must not pass -- on the
	// strength of the listing alone. A 1% sample of twelve files reads
	// one of them, so this finding cannot have come from reading the file
	// whose content is missing.
	sampled, err := damaged.Verify(ctx, snap.ID, backupengine.VerifyRequest{
		Level:         model.LevelContentSample,
		SamplePercent: 1,
	})
	if err == nil {
		t.Fatalf("a sampled verification passed a bucket with pack object %q deleted (report: %+v)", gone, sampled)
	}

	if sampled.Level != "" {
		t.Errorf("the failed verification reported achieved level %q; a verification that did not pass proved nothing, "+
			"and a catalog row carrying a level here reads as still-good", sampled.Level)
	}

	if sampled.FilesVerified > 1 {
		t.Errorf("a 1%% sample read %d files; this finding is supposed to come from the bucket listing, not from "+
			"happening to read the damaged file", sampled.FilesVerified)
	}

	// The finding has to name the object that is gone, because that is
	// what an operator takes to their provider's console. "Verification
	// failed" sends them to read every byte of the repository to find out
	// what is missing.
	findings := strings.Join(sampled.Errors, "; ")

	if !strings.Contains(findings, gone) {
		t.Errorf("the sampled findings are %v; none of them names the deleted object %q, so an operator cannot tell "+
			"what is gone from the bucket", sampled.Errors, gone)
	}
}

// s3Repository creates and opens one repository in bucket, closed when the
// test ends.
func s3Repository(t *testing.T, fixture *machines.Medium, bucket string) (backupengine.Repository, backupengine.RepositoryLocation) {
	t.Helper()

	ctx := context.Background()
	loc := s3Location(t, fixture, bucket, "production")
	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository against MinIO: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	// Close is documented safe to call twice, so a test that closes the
	// handle itself (the corruption row does, to drop its index state)
	// does not have to coordinate with this.
	t.Cleanup(func() { _ = rep.Close(context.Background()) })

	return rep, loc
}

// s3VerifySourceTree writes the tree every test in this file snapshots:
// twelve files of unique incompressible bytes, one of them large, across
// two directories.
//
// Unique bytes per file, from crypto/rand, because identical files
// deduplicate to ONE stored object: a tree of repeated content would make
// every file count in this file's reports wrong for a reason that has
// nothing to do with what is being asserted. Two directories because a
// verification walk that never recursed would otherwise pass.
func s3VerifySourceTree(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "source")

	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o750); err != nil {
		t.Fatalf("creating the source tree: %v", err)
	}

	files := 0
	total := int64(0)

	write := func(rel string, size int) {
		t.Helper()

		s3VerifyWriteRandom(t, filepath.Join(dir, filepath.FromSlash(rel)), size)

		files++
		total += int64(size)
	}

	write("large.bin", s3VerifyLargeFile)

	for i := range 5 {
		write(fmt.Sprintf("small-%02d.bin", i), s3VerifySmallFile)
	}

	for i := range 6 {
		write(fmt.Sprintf("nested/small-%02d.bin", i), s3VerifySmallFile)
	}

	// The constants are what every report in this file is asserted
	// against, so a tree that stopped matching them has to say so here
	// rather than as an unexplained count three assertions later.
	if files != s3VerifyFiles || total != s3VerifyTreeBytes {
		t.Fatalf("the fixture tree is %d files and %d bytes; this file's assertions are written about %d and %d",
			files, total, s3VerifyFiles, s3VerifyTreeBytes)
	}

	return dir
}

// s3VerifyRewriteFile replaces one file's content with new random bytes of
// the same length.
func s3VerifyRewriteFile(t *testing.T, dir, rel string) {
	t.Helper()

	full := filepath.Join(dir, filepath.FromSlash(rel))

	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("stat %s before rewriting it: %v", rel, err)
	}

	s3VerifyWriteRandom(t, full, int(info.Size()))
}

func s3VerifyWriteRandom(t *testing.T, path string, size int) {
	t.Helper()

	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generating %d random bytes for %s: %v", size, path, err)
	}

	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// s3VerifySnapshot snapshots the tree with the tags the production sink
// sets, so what the engine counts is counted the way production counts it.
func s3VerifySnapshot(t *testing.T, rep backupengine.Repository, srcDir, description string) backupengine.SnapshotInfo {
	t.Helper()

	snap, err := rep.Snapshot(context.Background(), backupengine.SnapshotRequest{
		Source:      backupengine.Source{Host: "nas-01", User: "backupd", Path: srcDir},
		Description: description,
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: "nas-01/verification",
			backupengine.TagKeyDomain:    "production",
		},
	})
	if err != nil {
		t.Fatalf("Snapshot into s3 (%s): %v", description, err)
	}

	return snap
}

// bucketContents is what the bucket PHYSICALLY holds: how many objects and
// how many bytes, read out of a real S3 listing.
//
// It reads through this product's own S3 client rather than off the
// container's drive, unlike the fixture's LargestObjectBytes, and the
// difference is deliberate: LargestObjectBytes answers a question about a
// precondition (did anything here write a large body), where trusting the
// client under test would be circular. This answers a question about what
// the ENDPOINT reports it is storing, which is the number an operator is
// billed for and the only one a lifecycle rule acts on. It is read-only --
// a listing and nothing else.
func bucketContents(t *testing.T, medium transport.Medium) (int, int64) {
	t.Helper()

	objects, err := rclone.New().ListObjects(context.Background(), medium, "")
	if err != nil {
		t.Fatalf("listing the bucket: %v", err)
	}

	var total int64
	for _, o := range objects {
		total += o.Size
	}

	return len(objects), total
}

// s3VerifyDeleteLargestPackObject deletes the biggest pack object in the
// bucket and returns its blob id.
//
// The biggest object is the pack blob this tree's content went into: the
// engine fills a pack to about 20 MiB before flushing it, so a 4.7 MiB
// tree is one pack, and deleting it is the whole snapshot's content
// leaving the bucket while every index and manifest object stays.
//
// The assertions about WHICH object was deleted are what keep the
// corruption row from silently testing nothing: an index blob or a
// manifest would be different damage with a different expected outcome,
// so a repository whose layout stopped putting content in a large pack
// object has to fail here loudly rather than produce a passing test about
// a few kilobytes of metadata.
func s3VerifyDeleteLargestPackObject(t *testing.T, medium transport.Medium) string {
	t.Helper()

	ctx := context.Background()
	adapter := rclone.New()

	objects, err := adapter.ListObjects(ctx, medium, "")
	if err != nil {
		t.Fatalf("listing the bucket to find a pack object: %v", err)
	}

	var largest transport.ObjectInfo

	for _, o := range objects {
		if o.Size > largest.Size {
			largest = o
		}
	}

	// A content pack blob is named p<hex> by this engine; metadata packs
	// are q<hex> and indexes are x<hex>. The biggest object in a bucket
	// holding one snapshot of this tree must be a content pack, and it
	// must be most of the large file.
	id := path.Base(largest.Key)

	if !strings.HasPrefix(id, "p") || largest.Size < s3VerifyLargeFile/2 {
		t.Fatalf("the biggest object in the bucket is %q at %d bytes; this row deletes a CONTENT pack blob holding "+
			"most of a %d byte file, and %q is not one, so deleting it would prove something else",
			largest.Key, largest.Size, s3VerifyLargeFile, id)
	}

	if err := adapter.DeleteObject(ctx, medium, largest.Key); err != nil {
		t.Fatalf("deleting pack object %q: %v", largest.Key, err)
	}

	// Deleted for real, checked at the endpoint: an S3 DELETE that
	// answered 204 without removing anything would leave every assertion
	// below passing for the wrong reason.
	if _, err := adapter.StatObject(ctx, medium, largest.Key); err == nil {
		t.Fatalf("pack object %q is still in the bucket after a delete that reported success", largest.Key)
	}

	return id
}

// assertRestoredTree checks a restored directory against the source tree
// by content, which is the comparison an operator would make.
func assertRestoredTree(t *testing.T, srcDir, target string) {
	t.Helper()

	want := hashTree(t, srcDir)
	got := hashTree(t, target)

	if len(got) != len(want) {
		t.Fatalf("the restore left %d file(s) under %s, the source tree holds %d", len(got), target, len(want))
	}

	for name, hash := range want {
		if got[name] != hash {
			t.Errorf("%s was restored with hash %s, want %s", name, got[name], hash)
		}
	}
}
