package miniointegration_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/tests/machines"
)

// This file is EPIC K's §33 matrix for a repository in a bucket, run
// against a real S3 API rather than asserted about one.
//
// The unit tests under internal/backupengine/kopia prove the refusals: a
// storage target missing any one of the properties a repository needs is
// rejected explicitly, with faults injected because no test rig has a
// genuinely half-implemented endpoint in it. This is the other half, and
// the one that cannot be faked -- a conforming endpoint really does pass,
// over the wire, with signed requests, multipart uploads and a server
// clock that is not this process's.
//
// # Why the repository passphrase and the credentials are files
//
// Because that is the only way to give this adapter either one. There is
// no field on a repository location that a literal secret fits into, so a
// test that wanted to shortcut would have to add one, which is the whole
// custody argument: the shortcut does not exist for tests or for
// production wiring.

// repositoryPassphrase is this suite's own, written to a file per test.
const repositoryPassphrase = "s3-repository-passphrase-not-a-secret"

// multipartPayload is large enough to force the S3 client to push a body
// far bigger than one pack blob.
//
// The vendor writes content into pack blobs of up to about 20 MiB, so one
// incompressible file this size guarantees several full-size blobs go over
// the wire. That matters because a large body is a different code path on
// both sides -- a signature over megabytes, a content-length the server
// has to honour, a connection held open -- and "we wrote some small blobs
// successfully" says nothing about it.
const multipartPayload = 48 << 20

// multipartThreshold is minio-go's part size (its minPartSize), which is
// the size an object has to exceed for this run to have exercised the
// large-body path at all.
//
// It is restated here because it is the number the check below is about,
// and because a client upgrade that changed it should make this test say
// so rather than quietly stop covering that path. Note that nothing this
// product writes is actually split into parts: the vendor sets
// DisableMultipart on every PutObject (repo/blob/s3/s3_storage.go), so
// this threshold is a size, not a code-path switch. See the check itself.
const multipartThreshold = 16 << 20

// s3Location builds a repository location pointing at the fixture's own
// bucket, with both secrets as references to files.
func s3Location(t *testing.T, fixture *machines.Medium, bucket, domain string) backupengine.RepositoryLocation {
	t.Helper()

	id, err := model.NewRepositoryDomainID(domain)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID(%q): %v", domain, err)
	}

	secrets := t.TempDir()
	passphrase := filepath.Join(secrets, "passphrase")

	if err := os.WriteFile(passphrase, []byte(repositoryPassphrase+"\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

	return backupengine.RepositoryLocation{
		Kind:     backupengine.LocationS3,
		Domain:   id,
		StateDir: filepath.Join(t.TempDir(), "state"),
		S3: backupengine.S3Storage{
			Endpoint: fixture.Endpoint,
			Region:   fixture.Region,
			Bucket:   bucket,
			Prefix:   "repositories",
			// The same shared-credentials file the medium plane uses,
			// which is the point: an operator who has told this product
			// how to reach a bucket has told it how to reach the
			// repository in that bucket.
			Credentials: secretref.Ref{File: fixture.CredentialsFile},
		},
		Passphrase: secretref.Ref{File: passphrase},
	}
}

// TestS3RepositoryMatrix is the sequence, in order, in one test.
//
// The order is the claim, exactly as the local lifecycle test argues: a
// snapshot that cannot be listed, read back, deleted and then garbage
// collected is not a backup, and each state only exists given the one
// before it. Splitting it into independent cases would mean starting a
// container per case for no additional coverage.
func TestS3RepositoryMatrix(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	bucket := bucketName(t, fixture)
	loc := s3Location(t, fixture, bucket, "production")
	eng := kopia.New()

	// --- create ------------------------------------------------------------

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository against MinIO: %v", err)
	}

	// Creating over an existing repository would orphan every snapshot in
	// it, and the refusal has to survive the round trip through a real
	// endpoint rather than only through a filesystem's ErrExist.
	if err := eng.CreateRepository(ctx, loc); !errors.Is(err, backupengine.ErrRepositoryExists) {
		t.Fatalf("CreateRepository over an existing s3 repository: got %v, want ErrRepositoryExists", err)
	}

	// --- open --------------------------------------------------------------

	wrongPass := loc
	wrongPass.Passphrase = passphraseFile(t, "definitely-not-the-passphrase")
	wrongPass.StateDir = filepath.Join(t.TempDir(), "wrong-state")

	if _, err := eng.OpenRepository(ctx, wrongPass); !errors.Is(err, backupengine.ErrPassphrase) {
		t.Fatalf("OpenRepository with the wrong passphrase: got %v, want ErrPassphrase", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	// --- health ------------------------------------------------------------

	// The clock comparison here is the real one: the timestamp comes from
	// MinIO's own Last-Modified, measured against this process's clock, so
	// a container whose clock has drifted is genuinely detected.
	health, err := rep.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if !health.Reachable {
		t.Errorf("Health says a bucket this test just wrote a repository into is unreachable")
	}

	if len(health.Warnings) != 0 {
		t.Errorf("Health warns about a fresh repository against a container on this machine's clock: %+v", health.Warnings)
	}

	// --- write, including a multipart upload --------------------------------

	srcDir := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatalf("creating the source tree: %v", err)
	}

	payload := make([]byte, multipartPayload)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generating the payload: %v", err)
	}

	if err := os.WriteFile(filepath.Join(srcDir, "large.bin"), payload, 0o600); err != nil {
		t.Fatalf("writing the payload: %v", err)
	}

	if err := os.WriteFile(filepath.Join(srcDir, "small.txt"), []byte("a small file beside a large one\n"), 0o600); err != nil {
		t.Fatalf("writing the small file: %v", err)
	}

	want := hashTree(t, srcDir)

	src := backupengine.Source{Host: "nas-01", User: "backupd", Path: srcDir}

	snap, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source:      src,
		Description: "the s3 matrix",
		// The tags the production sink sets on every snapshot, so what
		// Stats counts below is counted the way it is counted in
		// production rather than falling back to the unattributed case.
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: "nas-01/matrix",
			backupengine.TagKeyDomain:    "production",
		},
	})
	if err != nil {
		t.Fatalf("Snapshot into s3: %v", err)
	}

	if snap.Bytes < int64(multipartPayload) {
		t.Errorf("the snapshot recorded %d bytes for a source tree of at least %d", snap.Bytes, multipartPayload)
	}

	// The large-body claim, checked rather than asserted.
	//
	// The vendor fills pack blobs to about 20 MiB (MaxPackSize defaults to
	// 20<<20) before flushing them, so an object on the drive bigger than
	// minio-go's 16 MiB part size is one this run pushed in a SINGLE PUT:
	// repo/blob/s3/s3_storage.go sets DisableMultipart on every
	// PutObject, so nothing this product writes is ever split into parts.
	// That is the path worth pinning -- a 21 MiB body in one request, with
	// a signature over it -- because it is the one an S3 impostor, a proxy
	// with a body limit or a gateway that mishandles a chunked stream gets
	// wrong while answering 200 to everything small.
	//
	// Without this, "we wrote 48 MiB so the large-body path ran" is a
	// claim about somebody else's internal pack size with nothing
	// watching it, and a future pack-size default could quietly turn this
	// whole case back into a series of small PUTs.
	if largest := fixture.LargestObjectBytes(t, bucket); largest <= multipartThreshold {
		t.Errorf("the largest object in the bucket is %d bytes, at or below the %d-byte part size; "+
			"nothing in this run wrote a large body, so that path is untested",
			largest, multipartThreshold)
	}

	// --- list --------------------------------------------------------------

	snaps, err := rep.ListSnapshots(ctx, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if len(snaps) != 1 || snaps[0].ID != snap.ID {
		t.Fatalf("ListSnapshots returned %+v, want exactly the snapshot just written (%s)", snaps, snap.ID)
	}

	stats, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.Sources != 1 || stats.Snapshots != 1 {
		t.Errorf("Stats = %d source(s), %d snapshot(s); want 1 and 1", stats.Sources, stats.Snapshots)
	}

	// The physical size is read back out of the bucket's own listing, so
	// this is also the assertion that listing a real S3 prefix works.
	if stats.PhysicalBytes < int64(multipartPayload) {
		t.Errorf("Stats reports %d physical bytes in the bucket after storing %d incompressible bytes",
			stats.PhysicalBytes, multipartPayload)
	}

	// --- read: restore, and verify -----------------------------------------

	if report, err := rep.Verify(ctx, snap.ID, backupengine.VerifyRequest{Level: model.LevelContentFull}); err != nil {
		t.Fatalf("Verify: %v (findings: %v)", err, report.Errors)
	}

	restoreDir := filepath.Join(t.TempDir(), "restored")

	restored, err := rep.Restore(ctx, snap.ID, backupengine.RestoreRequest{TargetPath: restoreDir, SkipOwners: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if restored.Bytes < int64(multipartPayload) {
		t.Errorf("Restore reported %d bytes for a tree of at least %d", restored.Bytes, multipartPayload)
	}

	got := hashTree(t, restoreDir)
	if len(got) != len(want) {
		t.Fatalf("restored %d file(s), backed up %d", len(got), len(want))
	}

	for name, hash := range want {
		if got[name] != hash {
			t.Errorf("%s restored with hash %s, want %s", name, got[name], hash)
		}
	}

	// --- reopen by stable id after a restart --------------------------------

	if err := rep.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := os.RemoveAll(loc.StateDir); err != nil {
		t.Fatalf("clearing the state directory: %v", err)
	}

	reopened, err := kopia.New().OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("reopening the s3 repository by id with no local state: %v", err)
	}

	if found, err := reopened.LookupSnapshot(ctx, snap.ID); err != nil || found.ID != snap.ID {
		t.Fatalf("LookupSnapshot(%s) after a restart = %+v, %v", snap.ID, found, err)
	}

	// --- delete and maintain -----------------------------------------------

	if err := reopened.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	if _, err := reopened.LookupSnapshot(ctx, snap.ID); !errors.Is(err, backupengine.ErrSnapshotNotFound) {
		t.Errorf("LookupSnapshot of a deleted snapshot = %v, want ErrSnapshotNotFound", err)
	}

	quick, err := reopened.Maintain(ctx, backupengine.MaintenanceQuick)
	if err != nil {
		t.Fatalf("quick Maintain against s3: %v", err)
	}

	if !quick.Ran {
		t.Errorf("quick Maintain reported no work against a repository that has just had its only snapshot deleted")
	}

	full, err := reopened.Maintain(ctx, backupengine.MaintenanceFull)
	if err != nil {
		t.Fatalf("full Maintain against s3: %v", err)
	}

	if !full.Ran {
		t.Errorf("full Maintain reported no work")
	}

	// What is deliberately NOT asserted: that the bucket got smaller.
	//
	// Maintenance runs at full safety (see the adapter's Maintain), which
	// keeps recently written content out of garbage collection so that
	// maintenance is safe to run while a snapshot is in progress. Every
	// blob in this repository was written seconds ago, so reclamation is
	// correctly deferred, and a test demanding a smaller bucket here
	// would only pass if that safety were turned off.
	//
	// What this run does prove is that maintenance completes over a real
	// S3 API -- index rewrites, blob deletes, and a listing that agrees
	// afterwards -- and that it leaves the repository readable, which is
	// the failure an unusable endpoint would produce.
	if _, err := reopened.Stats(ctx); err != nil {
		t.Fatalf("Stats after maintenance: %v", err)
	}

	health, err = reopened.Health(ctx)
	if err != nil {
		t.Fatalf("Health after maintenance: %v", err)
	}

	if !health.Reachable {
		t.Errorf("the repository is unreachable after maintenance")
	}

	if snaps, err := reopened.ListSnapshots(ctx, src); err != nil || len(snaps) != 0 {
		t.Errorf("after deleting the only snapshot and maintaining, ListSnapshots = %+v, %v; want none", snaps, err)
	}

	if err := reopened.Close(ctx); err != nil {
		t.Errorf("closing after maintenance: %v", err)
	}
}

// TestS3RepositoryRefusesABucketThatIsNotThere is the refusal an operator
// meets most often, and the one whose failure mode is worst if it is not
// explicit: this product never creates a bucket, so a mistyped name must
// produce a named refusal rather than a repository nobody can find again.
func TestS3RepositoryRefusesABucketThatIsNotThere(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	loc := s3Location(t, fixture, "bucket-that-does-not-exist", "production")

	err := kopia.New().CreateRepository(ctx, loc)
	if err == nil {
		t.Fatalf("CreateRepository succeeded against a bucket that does not exist")
	}

	if !strings.Contains(err.Error(), "bucket-that-does-not-exist") {
		t.Errorf("the refusal does not name the bucket an operator has to fix: %v", err)
	}

	assertNoCredentials(t, fixture, "the missing-bucket refusal", err.Error())
}

// TestS3RepositoryRefusesWrongCredentials covers the other half of the
// authentication surface, and it is where the credential material is most
// available to leak: the resolver has just read the file, the provider has
// just signed a request with it, and the endpoint has just rejected it.
func TestS3RepositoryRefusesWrongCredentials(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	wrong := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(wrong, []byte(
		"[default]\naws_access_key_id = AKIANOTTHEREALKEY000\naws_secret_access_key = not-the-real-secret-either\n"), 0o600); err != nil {
		t.Fatalf("writing the wrong credentials: %v", err)
	}

	loc := s3Location(t, fixture, bucketName(t, fixture), "production")
	loc.S3.Credentials = secretref.Ref{File: wrong}

	err := kopia.New().CreateRepository(ctx, loc)
	if err == nil {
		t.Fatalf("CreateRepository succeeded with credentials the endpoint should reject")
	}

	assertNoCredentials(t, fixture, "the authentication refusal", err.Error())

	if strings.Contains(err.Error(), "not-the-real-secret-either") {
		t.Errorf("the refusal echoes the secret access key it was given: %v", err)
	}
}

// TestS3CredentialsNeverReachAnErrorOrAReport is the leak assertion for
// the successful path, where the credentials really are the live ones for
// a reachable endpoint.
func TestS3CredentialsNeverReachAnErrorOrAReport(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	loc := s3Location(t, fixture, bucketName(t, fixture), "production")
	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	health, err := rep.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	stats, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	// A lookup of a snapshot that is not there, so a refusal built against
	// a live, authenticated connection is in scope too.
	_, lookupErr := rep.LookupSnapshot(ctx, backupengine.SnapshotID("no-such-snapshot"))
	if lookupErr == nil {
		t.Fatalf("LookupSnapshot found a snapshot that was never written")
	}

	for _, surface := range []struct {
		label string
		text  string
	}{
		{"the health report", fmt.Sprintf("%+v", health)},
		{"the stats report", fmt.Sprintf("%+v", stats)},
		{"a snapshot lookup failure", lookupErr.Error()},
		{"the repository location itself", fmt.Sprintf("%+v", loc)},
	} {
		assertNoCredentials(t, fixture, surface.label, surface.text)

		if strings.Contains(surface.text, repositoryPassphrase) {
			t.Errorf("%s carries the repository passphrase: %s", surface.label, surface.text)
		}
	}
}

// TestS3CredentialsAreNeverWrittenToTheStateDirectory is the regression
// test for the worst defect this adapter has had.
//
// Connecting to a bucket repository persists a connection to
// <StateDir>/<domain>.config, and with the vendor's own s3 provider the
// persisted connection IS the provider's options struct -- AccessKeyID,
// SecretAccessKey and SessionToken included, with ordinary json tags. So
// every open used to write the operator's live cloud credentials to the
// backup host in cleartext and leave them there, which defeats the entire
// point of resolving them from a secretref at the last moment: an operator
// who kept the secret in a 0600 file or behind a Vault command got a
// plaintext copy anyway, made by the program they were trusting.
//
// The assertion is on the BYTES of everything under the state directory,
// not on a type or a field list, because that is the only assertion that
// stays true through a vendor upgrade that adds a field or renames one.
// The suite's other leak test (TestS3CredentialsNeverReachAnErrorOrAReport)
// covers what this adapter SAYS; this one covers what it WRITES DOWN.
func TestS3CredentialsAreNeverWrittenToTheStateDirectory(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	loc := s3Location(t, fixture, bucketName(t, fixture), "production")
	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	if _, err := rep.Stats(ctx); err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if err := rep.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	config := filepath.Join(loc.StateDir, "production.config")

	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("reading the connection config this adapter wrote: %v", err)
	}

	// Every file, not only the config: a cache file, a lock or a log
	// holding the same bytes would be the same disclosure.
	if err := filepath.WalkDir(loc.StateDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(loc.StateDir, path)
		if relErr != nil {
			rel = path
		}

		assertNoCredentials(t, fixture, "the state file "+rel, string(body))

		if strings.Contains(string(body), repositoryPassphrase) {
			t.Errorf("the state file %s carries the repository passphrase", rel)
		}

		return nil
	}); err != nil {
		t.Fatalf("walking the state directory: %v", err)
	}

	// A session token is the one credential this fixture cannot have --
	// MinIO would reject a bogus one before anything was written -- so it
	// is covered the only way it can be: the persisted connection must
	// have no FIELD that one fits in. These are the vendor's own names for
	// the three, and finding any of them means the config was written by
	// the provider's options struct again.
	for _, field := range []string{"accessKeyID", "secretAccessKey", "sessionToken"} {
		if strings.Contains(string(raw), field) {
			t.Errorf("the connection config has a %q field, which is where a credential ends up on disk: %s", field, raw)
		}
	}

	// The modes are this project's, not the vendor's default: the state
	// directory holds what a repository is connected to and, if caching is
	// ever enabled, repository content.
	dirInfo, err := os.Stat(loc.StateDir)
	if err != nil {
		t.Fatalf("stat of the state directory: %v", err)
	}

	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("the state directory is %04o; want 0700", mode)
	}

	fileInfo, err := os.Stat(config)
	if err != nil {
		t.Fatalf("stat of the connection config: %v", err)
	}

	if mode := fileInfo.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the connection config is %04o, which another local account can read", mode)
	}

	// And the repository still opens, which is the half that makes the
	// scrubbing a fix rather than a break: the credential comes from the
	// operator's declared source again on every open, and the config alone
	// is not expected to be enough.
	reopened, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("reopening a repository whose config holds no credentials: %v", err)
	}

	t.Cleanup(func() {
		if err := reopened.Close(context.Background()); err != nil {
			t.Errorf("closing the reopened repository: %v", err)
		}
	})

	if _, err := reopened.Stats(ctx); err != nil {
		t.Errorf("Stats on the reopened repository: %v", err)
	}
}

// assertNoCredentials fails when text carries any part of the fixture's
// credentials.
//
// The access key id is checked as well as the secret, because FR-33's rule
// is "never in a log line, in whole or in part": a key id identifies the
// principal and is half of what an attacker needs.
func assertNoCredentials(t *testing.T, fixture *machines.Medium, label, text string) {
	t.Helper()

	if strings.Contains(text, fixture.SecretAccessKey) {
		t.Errorf("%s carries the bucket's secret access key: %s", label, text)
	}

	if strings.Contains(text, fixture.AccessKeyID) {
		t.Errorf("%s carries the bucket's access key id: %s", label, text)
	}
}

// bucketName gives each test its own bucket on the one server, so a
// repository written by one case is not visible to another.
func bucketName(t *testing.T, fixture *machines.Medium) string {
	t.Helper()

	return fixture.NewBucket(t).Bucket
}

// passphraseFile writes a passphrase and returns the reference to it.
func passphraseFile(t *testing.T, passphrase string) secretref.Ref {
	t.Helper()

	path := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(path, []byte(passphrase), 0o600); err != nil {
		t.Fatalf("writing a passphrase file: %v", err)
	}

	return secretref.Ref{File: path}
}

// hashTree is the content of a directory tree as relative path to SHA-256,
// which is how a restore is compared against what was backed up.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])

		return nil
	}); err != nil {
		t.Fatalf("hashing %s: %v", dir, err)
	}

	return out
}
