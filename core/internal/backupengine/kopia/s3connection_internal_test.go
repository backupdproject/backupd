package kopia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kopia/kopia/repo/blob"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/secretref"
)

// This file tests the in-memory credential registration a persisted
// bucket connection points at. What it is for, and why the persisted
// connection holds a nonce instead of anything resolvable, is in
// s3connection.go.
//
// None of this needs an endpoint: the vendor's s3.New builds a client and
// does no I/O (`_ = isCreate`), which is what makes the ownership rules
// testable without a container. The MinIO suite covers the same code over
// the wire, including the assertion that the state directory holds no
// credential.

// registeredCredentials counts live registrations. It reaches into the
// package's own state on purpose: the number is not something production
// has any business asking for, and a leak is invisible from outside.
func registeredCredentials() int {
	resolvedS3Credentials.mu.Lock()
	defer resolvedS3Credentials.mu.Unlock()

	return len(resolvedS3Credentials.by)
}

// s3TestLocation is a bucket location with a real credentials file and an
// endpoint that answers every request with a 404.
//
// The endpoint has to exist because the vendor's s3.New reads a
// `.storageconfig` object while building the client, and a 404 for it is
// the normal answer for a bucket that has none. Nothing here is a test of
// S3 behaviour -- that is the MinIO suite's job -- it is the cheapest
// honest way to get a built storage handle so the REGISTRATION rules can
// be asserted without a container.
func s3TestLocation(t *testing.T) backupengine.RepositoryLocation {
	t.Helper()

	domain, err := model.NewRepositoryDomainID("production")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")

	if err := os.WriteFile(creds,
		[]byte("[default]\naws_access_key_id = AKIATESTKEYID000000\naws_secret_access_key = test-secret-not-a-real-one\n"),
		0o600); err != nil {
		t.Fatalf("writing the credentials file: %v", err)
	}

	passphrase := filepath.Join(dir, "passphrase")
	if err := os.WriteFile(passphrase, []byte("test-passphrase"), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

	return backupengine.RepositoryLocation{
		Kind:     backupengine.LocationS3,
		Domain:   domain,
		StateDir: filepath.Join(dir, "state"),
		S3: backupengine.S3Storage{
			Endpoint:    "http://" + notABucket(t),
			Region:      "us-east-1",
			Bucket:      "backups",
			Prefix:      "repositories",
			Credentials: secretref.Ref{File: creds},
		},
		Passphrase: secretref.Ref{File: passphrase},
	}
}

// notABucket is an endpoint that 404s everything, and its address.
func notABucket(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	return srv.Listener.Addr().String()
}

// TestCredentialRegistrationsDoNotOutliveTheStorageTheyWereMadeFor is the
// leak assertion on the registry.
//
// A registration is a live credential reference held on a repository's
// behalf, and a nonce that outlives its storage is two problems: a
// daemon that accumulates them for as long as it runs, and a state file
// naming a registration that is still resolvable long after the handle
// that made it went away. The error path is the one worth testing -- it is
// the path that gets written once and never read again.
//
// It is deliberately not parallel: the registry is package state, and the
// number of entries in it is the assertion.
func TestCredentialRegistrationsDoNotOutliveTheStorageTheyWereMadeFor(t *testing.T) {
	ctx := context.Background()
	loc := s3TestLocation(t)

	if before := registeredCredentials(); before != 0 {
		t.Fatalf("%d credential registrations were already live before this test", before)
	}

	st, release, err := openS3Storage(ctx, loc, false)
	if err != nil {
		t.Fatalf("openS3Storage: %v", err)
	}

	if live := registeredCredentials(); live != 1 {
		t.Errorf("an open bucket storage holds %d registrations; want exactly 1", live)
	}

	if err := st.Close(ctx); err != nil {
		t.Errorf("closing the storage: %v", err)
	}

	// Closing the STORAGE must not release it: the connection this adapter
	// persists names the registration, and repo.Open builds a second
	// storage from that connection after this one is closed.
	if live := registeredCredentials(); live != 1 {
		t.Errorf("closing the storage handle dropped the registration the persisted connection names (%d live); "+
			"repo.Open would then fail to build storage from the config this adapter just wrote", live)
	}

	release()
	release() // idempotent, because the error paths that call it are the ones least worth auditing

	if live := registeredCredentials(); live != 0 {
		t.Errorf("%d registrations survived release", live)
	}

	// And a credential source that cannot be resolved leaves nothing
	// behind either.
	broken := loc
	broken.S3.Credentials = secretref.Ref{File: filepath.Join(t.TempDir(), "does-not-exist")}

	if _, _, err := openS3Storage(ctx, broken, false); err == nil {
		t.Fatalf("openS3Storage accepted a credential file that is not there")
	}

	if live := registeredCredentials(); live != 0 {
		t.Errorf("a failed open left %d registrations behind", live)
	}
}

// TestAPersistedConnectionCannotOpenStorageOnItsOwn is the other half of
// the nonce decision, stated as behaviour.
//
// A config file from a previous process names a registration nobody holds.
// The refusal matters more than it looks: the vendor's s3 provider builds
// a CHAIN of credential providers -- static, then the AWS environment,
// then the instance role -- so a storage built with empty credentials does
// not fail, it authenticates as whatever ambient identity the host
// happens to have. A repository opened as an identity nobody chose is
// worse than one that refuses to open.
func TestAPersistedConnectionCannotOpenStorageOnItsOwn(t *testing.T) {
	_, err := newS3Storage(context.Background(), &s3Connection{
		Bucket:      "backups",
		Prefix:      "repositories/production/",
		Credentials: "0123456789abcdef0123456789abcdef",
	}, false)
	if err == nil {
		t.Fatal("a connection naming a registration this process does not hold built a storage")
	}

	// ErrRepositoryNotFound, because to a caller that is what a state file
	// pointing at nothing means, and the caller's answer is to connect
	// again rather than to retry.
	if !isRepositoryNotFound(err) {
		t.Errorf("the refusal is %v; want one wrapping ErrRepositoryNotFound", err)
	}

	if !strings.Contains(err.Error(), "backups") {
		t.Errorf("the refusal does not name the bucket it was asked for: %v", err)
	}
}

func isRepositoryNotFound(err error) bool {
	for e := err; e != nil; {
		if e == backupengine.ErrRepositoryNotFound { //nolint:errorlint,err113 // identity is the assertion
			return true
		}

		unwrapped, ok := e.(interface{ Unwrap() error }) //nolint:errorlint // walking the chain by hand
		if !ok {
			return false
		}

		e = unwrapped.Unwrap()
	}

	return false
}

// TestThePersistedConnectionRoundTripsThroughTheVendorsRegistry is what
// makes the custom storage type usable at all: repo.Open rebuilds a
// storage by unmarshalling the config into whatever the registry says the
// type's config is, so a connection that does not survive that round trip
// is a repository that cannot be reopened.
func TestThePersistedConnectionRoundTripsThroughTheVendorsRegistry(t *testing.T) {
	t.Parallel()

	loc := s3TestLocation(t)

	conn, err := s3ConnectionFor(loc, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("s3ConnectionFor: %v", err)
	}

	encoded, err := blob.ConnectionInfo{Type: s3StorageType, Config: &conn}.MarshalJSON()
	if err != nil {
		t.Fatalf("marshalling the connection: %v", err)
	}

	var back blob.ConnectionInfo

	if err := back.UnmarshalJSON(encoded); err != nil {
		t.Fatalf("unmarshalling the connection the vendor's own way: %v", err)
	}

	got, ok := s3Identity(back)
	if !ok {
		t.Fatalf("a connection of type %q did not come back as this adapter's own: %+v", back.Type, back.Config)
	}

	if !reflect.DeepEqual(got, conn) {
		t.Errorf("the connection came back as %+v, want %+v", got, conn)
	}

	// The endpoint is a host and a TLS decision by the time it is
	// persisted, not the URL an operator wrote: the provider takes the two
	// separately, and an http endpoint means no TLS.
	if host := strings.TrimPrefix(loc.S3.Endpoint, "http://"); got.Endpoint != host || !got.DoNotUseTLS {
		t.Errorf("the persisted endpoint is %q with DoNotUseTLS=%v; want %q without TLS", got.Endpoint, got.DoNotUseTLS, host)
	}
}
