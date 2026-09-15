package kopia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/secretref"
)

// This file exists so that an S3 repository's credentials are never
// written down.
//
// # What was wrong with the obvious code
//
// The vendor persists a connection: repo.Connect writes a LocalConfig to
// <StateDir>/<domain>.config, and the storage half of that config is
// whatever blob.Storage.ConnectionInfo() returns. For the vendor's own s3
// provider that is an s3.Options, and s3.Options carries AccessKeyID,
// SecretAccessKey and SessionToken with ordinary json tags -- the
// `kopia:"sensitive"` tag on them is a CLI display hint and nothing else.
// So connecting to a bucket repository with the vendor's provider writes
// the operator's resolved cloud credentials to a file, in cleartext, where
// they stay until somebody deletes the state directory.
//
// That defeats the whole point of secretref. An operator who took the
// trouble to keep the secret in a file with a mode on it, or behind a
// secrets-manager command, gets a plaintext copy of it on the backup host
// anyway, made by the program they were trusting not to do that.
//
// # What this does instead
//
// The connection this adapter persists is its OWN storage type, whose
// config holds the coordinates of the bucket and NOTHING resolvable: the
// credential half is a nonce, valid only inside this process, that names
// an in-memory registration of the operator's secretref.Ref. Every
// repo.Open therefore resolves the credential from the operator's declared
// source again, at the moment the storage is built, exactly as an open of
// a fresh location does. The state directory holds where the bucket is and
// what this process was calling it; it holds no credential, no argv and no
// path to either.
//
// # Why a nonce and not the Ref itself
//
// Because a Ref names an executable to run. Persisting one would make
// <StateDir>/<domain>.config a file that, if anything could write it,
// chooses a program this daemon then executes -- turning a state file into
// an execution primitive. It would also put the operator's resolver argv
// on disk, which is the thing secretref.Ref's own String/LogValue/
// MarshalJSON refuse to render into a log line. A nonce is neither.
//
// The cost is stated plainly: a config file alone cannot reopen a bucket
// repository in a new process. That costs nothing here, because
// OpenRepository always reconnects before it opens -- see the Connect
// comment in OpenRepository, which is unconditional for reasons that
// predate this file -- and a stale config from a previous process is
// therefore always overwritten before it is read.

// s3StorageType is the blob-storage type name this adapter persists for a
// bucket repository.
//
// It is deliberately not the vendor's "s3": a config written by this
// adapter must not be openable as a vendor s3 connection, because that is
// precisely the config shape that expects to find credentials in itself.
const s3StorageType = "backupd-s3"

// s3Connection is everything <StateDir>/<domain>.config says about a
// bucket repository.
//
// Every field here is an operator-visible coordinate except Credentials,
// which is a process-local nonce. There is deliberately no field that
// could hold credential material, so a future change that tried to
// persist one would have to add it here, in front of this comment.
type s3Connection struct {
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	DoNotUseTLS bool   `json:"doNotUseTLS,omitempty"`
	Region      string `json:"region,omitempty"`

	// RootCA is the operator's own CA bundle for a private endpoint. It is
	// public key material by definition, which is why it is the one
	// non-coordinate field that is allowed to be here.
	RootCA []byte `json:"rootCA,omitempty"`

	// Credentials is the nonce naming this process's in-memory
	// registration of the operator's credential reference. It resolves
	// through nothing but the map below, which is empty in every other
	// process and after this one exits.
	Credentials string `json:"credentialsHandle"`
}

func init() {
	// Registering rather than importing, for the reason at the top of
	// repository.go: this adapter owns which storage backends exist, and
	// this one is ours.
	blob.AddSupportedStorage(s3StorageType, s3Connection{}, newS3Storage)
}

// resolvedS3Credentials maps a live nonce to the credential reference it
// stands for.
//
// It holds references and never material: resolution happens per storage
// build, and what comes back is owned by the provider's options for as
// long as the storage handle lives, which is the shortest lifetime
// available (the provider signs every request with it).
//
//nolint:gochecknoglobals // the vendor's storage factory is a package-level registry and takes no context of ours
var resolvedS3Credentials = struct {
	mu sync.Mutex
	by map[string]secretref.Ref
}{by: map[string]secretref.Ref{}}

// registerS3Credentials records a credential reference for the life of one
// open repository and returns the nonce that names it.
//
// The caller owns the registration and must release it. Every caller that
// opens a storage does; see openStorage's release function.
func registerS3Credentials(ref secretref.Ref) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("kopia: generating a credential handle: %w", err)
	}

	handle := hex.EncodeToString(nonce)

	resolvedS3Credentials.mu.Lock()
	defer resolvedS3Credentials.mu.Unlock()

	resolvedS3Credentials.by[handle] = ref

	return handle, nil
}

// releaseS3Credentials forgets a registration. It is idempotent, because
// the error paths that call it are the ones least worth auditing for
// double release.
func releaseS3Credentials(handle string) {
	resolvedS3Credentials.mu.Lock()
	defer resolvedS3Credentials.mu.Unlock()

	delete(resolvedS3Credentials.by, handle)
}

// lookupS3Credentials resolves a nonce back to the reference it names.
func lookupS3Credentials(handle string) (secretref.Ref, bool) {
	resolvedS3Credentials.mu.Lock()
	defer resolvedS3Credentials.mu.Unlock()

	ref, ok := resolvedS3Credentials.by[handle]

	return ref, ok
}

// newS3Storage is the factory the vendor's registry calls, both when this
// adapter opens a storage directly and when repo.Open builds one from the
// persisted config.
//
// This is the one place a bucket credential is resolved, and it resolves
// it from the operator's declared source every time.
func newS3Storage(ctx context.Context, conn *s3Connection, isCreate bool) (blob.Storage, error) {
	ref, ok := lookupS3Credentials(conn.Credentials)
	if !ok {
		// Reached only by a config file this process did not just write --
		// a leftover from a previous run, or one an operator copied. It is
		// a refusal rather than a fallback to an ambient credential chain,
		// because a repository opened with credentials nobody declared is
		// a repository nobody chose.
		return nil, fmt.Errorf(
			"%w: the connection state for bucket %s names credentials that were resolved by a process that is no longer running; "+
				"this state file cannot open a repository on its own, and nothing needs it to -- opening a repository always reconnects first",
			backupengine.ErrRepositoryNotFound, conn.Bucket)
	}

	creds, err := secretref.ResolveAWS(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("kopia: resolving s3 credentials for bucket %s: %w", conn.Bucket, err)
	}

	opts := &s3.Options{
		BucketName:  conn.Bucket,
		Prefix:      conn.Prefix,
		Endpoint:    conn.Endpoint,
		DoNotUseTLS: conn.DoNotUseTLS,
		Region:      conn.Region,
		RootCA:      conn.RootCA,

		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey.Reveal(),
	}

	if creds.HasSession {
		opts.SessionToken = creds.SessionToken.Reveal()
	}

	st, err := s3.New(ctx, opts, isCreate)
	if err != nil {
		return nil, fmt.Errorf("kopia: opening s3 bucket %s: %w", conn.Bucket, err)
	}

	// The wrapper is what keeps the credentials out of the config file:
	// the storage underneath returns the vendor's own s3.Options from
	// ConnectionInfo(), credentials included, and that is exactly what
	// repo.Connect would write down.
	return s3Storage{Storage: st, conn: *conn}, nil
}

// s3Storage is the vendor's s3 storage with one method replaced.
//
// Embedding the interface rather than the implementation is the vendor's
// own pattern for a storage whose connection info has to be substituted
// (internal/repotesting/reconnectable_storage.go does the same thing for
// the same reason), and blob.Storage has no optional sub-interfaces that a
// repository probes for, so nothing is lost by delegating through it.
type s3Storage struct {
	blob.Storage

	conn s3Connection
}

// ConnectionInfo returns what this adapter is willing to persist.
func (s s3Storage) ConnectionInfo() blob.ConnectionInfo {
	conn := s.conn

	return blob.ConnectionInfo{Type: s3StorageType, Config: &conn}
}

// s3ConnectionFor builds the persistable connection for a location,
// against a credential registration the caller has already made.
//
// The endpoint is split into the host and the TLS decision the provider
// actually takes, which is the same parse validateS3 does for its
// refusals; doing it here rather than passing the URL through is what
// keeps "http means no TLS" a decision this adapter made rather than a
// default somebody inherited.
func s3ConnectionFor(loc backupengine.RepositoryLocation, handle string) (s3Connection, error) {
	conn := s3Connection{
		Bucket:      loc.S3.Bucket,
		Prefix:      s3Prefix(loc),
		Region:      loc.S3.Region,
		RootCA:      loc.S3.RootCA,
		Credentials: handle,
	}

	if loc.S3.Endpoint == "" {
		return conn, nil
	}

	u, err := url.Parse(loc.S3.Endpoint)
	if err != nil {
		return s3Connection{}, fmt.Errorf("kopia: endpoint %q is not a URL: %w", loc.S3.Endpoint, err)
	}

	// The provider takes a host and a boolean, not a URL. An http endpoint
	// means no TLS and says so; see backupengine.S3Storage for why there
	// is no third option that keeps TLS and stops verifying it.
	conn.Endpoint = u.Host
	conn.DoNotUseTLS = u.Scheme == "http"

	return conn, nil
}

// s3Identity reports the bucket, prefix and endpoint behind a persisted
// connection, and whether it was one of ours at all.
//
// The vendor's own s3.Options is deliberately NOT accepted here. A config
// of that shape is one that carries credentials, which this adapter never
// writes, so meeting one means something else wrote the state file and the
// right answer is the refusal checkStorageIdentity produces.
func s3Identity(ci blob.ConnectionInfo) (s3Connection, bool) {
	switch cfg := ci.Config.(type) {
	case *s3Connection:
		return *cfg, true
	case s3Connection:
		return cfg, true
	default:
		return s3Connection{}, false
	}
}
