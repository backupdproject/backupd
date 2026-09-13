package miniointegration_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestS3SecretsNeverReachTheLifecycleReports is the report-sink half of
// FR-33 for a bucket repository, and it is the half this suite was
// missing.
//
// The two leak tests above cover what an open repository SAYS about
// itself (health, stats, a lookup that found nothing) and what it WRITES
// DOWN (the state directory). Neither runs the operations an operator
// actually asks for, and those are the ones that build a report out of a
// live, authenticated, credential-bearing connection: a snapshot, a
// verification, a restore and maintenance. A credential that reached one
// of those would travel much further than a log line, because these are
// the structs the API serialises, the journal records and the UI renders,
// so each one is checked here and so is the error each one can return
// while the credential is still in hand.
//
// Every surface is rendered three ways, because a leak one rendering
// hides another shows: %+v is what a log line or a wrapped error does,
// %#v reaches fields that a verb-specific rendering or a Stringer can
// cover for, and encoding/json is what the API surface does with the
// same value.
func TestS3SecretsNeverReachTheLifecycleReports(t *testing.T) {
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

	srcDir := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(srcDir, "runs"), 0o750); err != nil {
		t.Fatalf("creating the source tree: %v", err)
	}

	for name, body := range map[string]string{
		"index.txt":     "a file to snapshot, verify and restore\n",
		"runs/db.dump":  "a second file, in a subdirectory\n",
		"runs/notes.md": "a third, so a restore has a tree to walk\n",
	} {
		if err := os.WriteFile(filepath.Join(srcDir, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	src := backupengine.Source{Host: "nas-01", User: "backupd", Path: srcDir}

	snap, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source:      src,
		Description: "the report-sink row",
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: "nas-01/reports",
			backupengine.TagKeyDomain:    "production",
		},
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	verified, err := rep.Verify(ctx, snap.ID, backupengine.VerifyRequest{Level: model.LevelContentFull})
	if err != nil {
		t.Fatalf("Verify: %v (findings: %v)", err, verified.Errors)
	}

	restored, err := rep.Restore(ctx, snap.ID, backupengine.RestoreRequest{
		TargetPath:    filepath.Join(t.TempDir(), "restored"),
		SkipOwners:    true,
		VerifyContent: true,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if !restored.Complete {
		t.Fatalf("Restore did not complete, so the report below is not the one an operator would be shown: %+v", restored)
	}

	quick, err := rep.Maintain(ctx, backupengine.MaintenanceQuick)
	if err != nil {
		t.Fatalf("quick Maintain: %v", err)
	}

	full, err := rep.Maintain(ctx, backupengine.MaintenanceFull)
	if err != nil {
		t.Fatalf("full Maintain: %v", err)
	}

	// The error paths, every one of them built while the credential is
	// live: three refusals from the authenticated connection above, and
	// one from an endpoint that has just rejected a signature made with a
	// real-but-wrong secret, which is the moment the material is most
	// available to a message.
	_, verifyErr := rep.Verify(ctx, backupengine.SnapshotID("no-such-snapshot"), backupengine.VerifyRequest{Level: model.LevelContentFull})
	if verifyErr == nil {
		t.Fatal("Verify of a snapshot that was never written returned no error")
	}

	_, restoreErr := rep.Restore(ctx, snap.ID, backupengine.RestoreRequest{SkipOwners: true})
	if restoreErr == nil {
		t.Fatal("Restore with no destination returned no error")
	}

	_, snapshotErr := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source:      backupengine.Source{Host: "nas-01", User: "backupd", Path: filepath.Join(srcDir, "not-there")},
		Description: "a source that is not on this host",
	})
	if snapshotErr == nil {
		t.Fatal("Snapshot of a path that does not exist returned no error")
	}

	wrongSecret := loc
	wrongSecret.StateDir = filepath.Join(t.TempDir(), "rejected-state")
	wrongSecret.S3.Credentials = credentialsFile(t, fixture.AccessKeyID, "not-the-real-secret")

	createErr := eng.CreateRepository(ctx, wrongSecret)
	if createErr == nil {
		t.Fatal("CreateRepository with the wrong secret access key was accepted by the endpoint")
	}

	for _, surface := range []struct {
		label  string
		value  any
		report bool
	}{
		{label: "the snapshot report", value: snap, report: true},
		{label: "the verify report", value: verified, report: true},
		{label: "the restore report", value: restored, report: true},
		{label: "the quick maintenance report", value: quick, report: true},
		{label: "the full maintenance report", value: full, report: true},
		{label: "a verification of a snapshot that is not there", value: verifyErr},
		{label: "a restore with no destination", value: restoreErr},
		{label: "a snapshot of a source that is not there", value: snapshotErr},
		{label: "a create the endpoint rejected the signature of", value: createErr},
	} {
		for _, rendering := range renderings(t, surface.value) {
			label := surface.label + " rendered " + rendering.how

			assertNoSecretMaterial(t, fixture, label, rendering.text)

			// A report is held to the stronger rule: it must not even
			// have a FIELD a credential fits in. That is the only way to
			// cover the session token this fixture cannot have (MinIO
			// rejects a bogus one before anything is reported) and the
			// only check that survives a vendor upgrade adding a field
			// nobody here has read yet. An error string is exempt: it is
			// prose, and prose about a missing snapshot may legitimately
			// use any word.
			if surface.report {
				assertNoCredentialShapedField(t, label, rendering.text)
			}
		}
	}
}

// TestS3SecretsNeverReachACapturedLogAcrossAWholeLifecycle is the same
// question asked of the process's own output rather than of one value.
//
// A report is a struct a caller chose to render. A log is everything the
// program said while it worked, including whatever a vendor package
// decided to print on a path nobody here wrote, and it is the one sink
// that is written without any of this project's types in the way. So this
// row taps the three places a line can actually leave this process --
// os.Stdout and os.Stderr, the standard library's own logger, and
// log/slog's default handler -- runs one complete create, snapshot,
// verify, maintain, restore and delete cycle against a real endpoint with
// live credentials, and reads the bytes.
//
// The capture proves itself first. A tap that silently caught nothing
// would pass this test on an empty buffer forever, so a canary line is
// written through each of the three and each has to be FOUND before the
// absence of a secret in the same bytes means anything. That is what
// makes this row an assertion rather than a comment: the three taps are
// demonstrated live, in the same window, against the same buffer.
//
// What it cannot see, stated rather than implied: a library that captured
// os.Stderr into a private field before this test swapped it, and
// anything written after the window closes. The canary covers the first
// for the standard library's logger (which does exactly that, hence the
// explicit log.SetOutput), and the cycle below is entirely inside the
// window.
func TestS3SecretsNeverReachACapturedLogAcrossAWholeLifecycle(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	ctx := context.Background()

	loc := s3Location(t, fixture, bucketName(t, fixture), "production")
	eng := kopia.New()

	logs := captureProcessOutput(t)

	// The canary, written through all three taps before the work starts.
	fmt.Fprintln(os.Stderr, canaryMarker, "stderr")
	fmt.Fprintln(os.Stdout, canaryMarker, "stdout")
	log.Println(canaryMarker, "stdlib logger")
	slog.Default().Info(canaryMarker + " slog default")

	// --- the whole lifecycle, inside the window ---------------------------

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	// The two refusals that handle secret material directly, so the
	// window contains the paths most likely to print one.
	wrongPass := loc
	wrongPass.StateDir = filepath.Join(t.TempDir(), "wrong-pass-state")
	wrongPass.Passphrase = passphraseFile(t, "definitely-not-the-passphrase")

	if _, err := eng.OpenRepository(ctx, wrongPass); !errors.Is(err, backupengine.ErrPassphrase) {
		t.Fatalf("OpenRepository with the wrong passphrase: got %v, want ErrPassphrase", err)
	}

	wrongSecret := loc
	wrongSecret.StateDir = filepath.Join(t.TempDir(), "wrong-secret-state")
	wrongSecret.S3.Credentials = credentialsFile(t, fixture.AccessKeyID, "not-the-real-secret")

	if _, err := eng.OpenRepository(ctx, wrongSecret); err == nil {
		t.Fatal("OpenRepository with the wrong secret access key was accepted")
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	srcDir := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		t.Fatalf("creating the source tree: %v", err)
	}

	if err := os.WriteFile(filepath.Join(srcDir, "index.txt"), []byte("one file through a whole cycle\n"), 0o600); err != nil {
		t.Fatalf("writing the source file: %v", err)
	}

	src := backupengine.Source{Host: "nas-01", User: "backupd", Path: srcDir}

	snap, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source:      src,
		Description: "the log-capture row",
		Tags: map[string]string{
			backupengine.TagKeyBackupSet: "nas-01/logs",
			backupengine.TagKeyDomain:    "production",
		},
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if _, err := rep.Verify(ctx, snap.ID, backupengine.VerifyRequest{Level: model.LevelContentFull}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if _, err := rep.Maintain(ctx, backupengine.MaintenanceQuick); err != nil {
		t.Fatalf("quick Maintain: %v", err)
	}

	if _, err := rep.Restore(ctx, snap.ID, backupengine.RestoreRequest{
		TargetPath: filepath.Join(t.TempDir(), "restored"),
		SkipOwners: true,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, err := rep.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}

	if err := rep.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	if _, err := rep.Maintain(ctx, backupengine.MaintenanceFull); err != nil {
		t.Fatalf("full Maintain: %v", err)
	}

	if err := rep.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// --- read the window --------------------------------------------------

	captured := logs.text(t)

	for _, tap := range []string{"stderr", "stdout", "stdlib logger", "slog default"} {
		if !strings.Contains(captured, canaryMarker+" "+tap) {
			t.Fatalf("the %s tap caught nothing, so the absence of a secret in this capture is evidence of nothing; captured %d byte(s)",
				tap, len(captured))
		}
	}

	assertNoSecretMaterial(t, fixture, "the captured log of a whole snapshot lifecycle", captured)
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

// assertNoSecretMaterial is assertNoCredentials plus the repository's own
// key material: the passphrase this suite created the repository with.
//
// They are one helper because FR-33's rule is one rule over the whole set
// of secrets a bucket repository handles, and a row that checked the
// credential and forgot the passphrase would pass while disclosing the
// thing that actually decrypts the backups.
func assertNoSecretMaterial(t *testing.T, fixture *machines.Medium, label, text string) {
	t.Helper()

	assertNoCredentials(t, fixture, label, text)

	if strings.Contains(text, repositoryPassphrase) {
		t.Errorf("%s carries the repository passphrase: %s", label, text)
	}
}

// credentialShapedFields are the names of the fields a credential lands
// in, lowercased for a case-insensitive scan.
//
// The names are the vendor's own for the S3 provider's options struct,
// plus the two this project's own location type uses. They are checked as
// NAMES rather than values for the reason the state-directory test gives:
// this fixture cannot have a session token (MinIO rejects a bogus one
// before anything is reported) and a value check therefore covers it not
// at all, whereas a report with no field one fits in cannot carry one
// however the graph is upgraded.
var credentialShapedFields = []string{
	"accesskeyid",
	"secretaccesskey",
	"sessiontoken",
	"passphrase",
	"encryptionkey",
	"masterkey",
}

// assertNoCredentialShapedField fails when a rendered report has a field
// a secret would be reported in.
func assertNoCredentialShapedField(t *testing.T, label, text string) {
	t.Helper()

	lower := strings.ToLower(text)

	for _, field := range credentialShapedFields {
		if strings.Contains(lower, field) {
			t.Errorf("%s has a %q field, which is where a secret ends up in a report: %s", label, field, text)
		}
	}
}

// rendering is one way a value can be written down, with the name of the
// path that would do it.
type rendering struct {
	how  string
	text string
}

// renderings is the three ways a report or an error actually leaves this
// process: a log line or a wrapped error (%+v), a debug dump that reaches
// past a Stringer (%#v), and the API surface (encoding/json).
//
// A value encoding/json refuses is rendered the other two ways rather
// than failing the test, because "this type is not serialisable" is not
// this row's question. An error is the case that hits it: json.Marshal of
// one produces "{}" rather than its message, so the %+v rendering is the
// one that carries an error's prose and the JSON rendering is kept only
// because a future error type with exported fields would be serialised by
// an API that logs it.
func renderings(t *testing.T, value any) []rendering {
	t.Helper()

	out := []rendering{
		{how: "with %+v", text: fmt.Sprintf("%+v", value)},
		{how: "with %#v", text: fmt.Sprintf("%#v", value)},
	}

	if asJSON, err := json.Marshal(value); err == nil {
		out = append(out, rendering{how: "as JSON", text: string(asJSON)})
	}

	return out
}

// credentialsFile writes a shared-credentials file in the format the
// medium fixture writes, and returns the reference to it.
//
// It exists so a test can address the endpoint with a REAL key id and a
// wrong secret, which is the refusal where the material is most available
// to a message: the resolver has read the file, the provider has signed a
// request with it, and the endpoint has just said no.
func credentialsFile(t *testing.T, accessKeyID, secretAccessKey string) secretref.Ref {
	t.Helper()

	path := filepath.Join(t.TempDir(), "credentials")
	body := fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", accessKeyID, secretAccessKey)

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing a credentials file: %v", err)
	}

	return secretref.Ref{File: path}
}

// canaryMarker is the string the log-capture row writes through every tap
// it installs, so an empty capture is a failure rather than a pass.
const canaryMarker = "backupd-log-capture-canary"

// processOutput is a live capture of everywhere a line can leave this
// process, and the restoration of all four taps when the window closes.
type processOutput struct {
	mu  sync.Mutex
	buf []byte

	// The pipe standing in for os.Stdout and os.Stderr, and the signal
	// that everything written into it has reached buf.
	reader *os.File
	writer *os.File
	copied chan struct{}

	// What was in place before, put back by restore.
	realStdout   *os.File
	realStderr   *os.File
	realLogWrite io.Writer
	realLogFlags int
	realSlog     *slog.Logger

	once sync.Once
}

// Write implements io.Writer for the standard library's logger and for
// slog's handler, both of which write from whatever goroutine logged.
func (p *processOutput) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.buf = append(p.buf, b...)

	return len(b), nil
}

// restore closes the window: it puts all four taps back, then closes the
// pipe and WAITS for the copier.
//
// The wait is what makes a read of buf deterministic under -race. A pipe
// copier only finishes once its write end is closed, so a reader that
// skipped this would be racing the last lines of the very output it is
// about to make a claim about, and would do it by reading a buffer while
// another goroutine appends to it.
//
// It is idempotent because both the cleanup and text call it: the test
// reads the capture, and a test that fails before reading it still has to
// get os.Stdout back.
func (p *processOutput) restore() {
	p.once.Do(func() {
		os.Stdout, os.Stderr = p.realStdout, p.realStderr
		log.SetOutput(p.realLogWrite)
		log.SetFlags(p.realLogFlags)
		slog.SetDefault(p.realSlog)

		_ = p.writer.Close()
		<-p.copied
		_ = p.reader.Close()
	})
}

// text closes the window and returns everything caught in it.
func (p *processOutput) text(t *testing.T) string {
	t.Helper()

	p.restore()

	p.mu.Lock()
	defer p.mu.Unlock()

	return string(p.buf)
}

// captureProcessOutput redirects os.Stdout, os.Stderr, the standard
// library's logger and log/slog's default handler into one buffer for the
// rest of the test, and puts all four back afterwards.
//
// All four, because they are four different ways a line leaves a Go
// process and a capture of one says nothing about the others. The
// standard library's logger is the one that has to be redirected
// explicitly: it captured os.Stderr's value at package initialisation, so
// swapping os.Stderr does not reach it.
//
// Nothing here is safe beside a parallel test, and nothing in this suite
// is parallel: a test that later called t.Parallel() would have its own
// output swallowed by this window. That is why this is a helper in this
// file rather than something shared.
func captureProcessOutput(t *testing.T) *processOutput {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening the capture pipe: %v", err)
	}

	out := &processOutput{
		reader:       reader,
		writer:       writer,
		copied:       make(chan struct{}),
		realStdout:   os.Stdout,
		realStderr:   os.Stderr,
		realLogWrite: log.Writer(),
		realLogFlags: log.Flags(),
		realSlog:     slog.Default(),
	}

	go func() {
		defer close(out.copied)

		_, _ = io.Copy(out, reader)
	}()

	os.Stdout, os.Stderr = writer, writer
	log.SetOutput(out)
	slog.SetDefault(slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})))

	t.Cleanup(out.restore)

	return out
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
