package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// Issue #852's refusal, asked of the service layer that owns it, and
// asked hermetically.
//
// refuseDeleteOnUnwritableSource has its own unit coverage as a pure
// function, and what that cannot show is the half that actually matters:
// that the write probe's answer reaches it on the CREATE path and on the
// EDIT path, off the same connection check each of those already runs. A
// set written with delete-from-source enabled against a source these
// credentials cannot write to is the exact shape the issue exists to
// prevent, and it is invisible until a disk fills up.
//
// Hermetic throughout. The write probe is a transport stub that refuses
// with transport.PermissionDenied, which is what an sftp account with
// read-only rights on the remote path really produces, and the connection
// check's own network steps are either skipped (a local source) or run
// against the in-process SSH server this package already starts for its
// candidate-check cases. No container, no real remote.

// readOnlySourceTransport is a transport whose listing works and whose
// write probe is refused, which is a read-only source exactly as an
// operator configures one: the account can read the backup directory and
// cannot create anything in it.
//
// probeErr is a field rather than a constant so one test can watch the
// same source go from writable to read-only, which is the edit case:
// nothing about the set changed, the permissions on the far side did.
type readOnlySourceTransport struct {
	probeErr error
	probed   int
}

func (t *readOnlySourceTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, nil
}

func (t *readOnlySourceTransport) Stat(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, errors.New("readOnlySourceTransport: nothing stats a source in a connection test")
}

func (t *readOnlySourceTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return transport.TransferResult{}, errors.New("readOnlySourceTransport: nothing transfers in a connection test")
}

func (t *readOnlySourceTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	return "", errors.New("readOnlySourceTransport: nothing hashes in a connection test")
}

func (t *readOnlySourceTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return errors.New("readOnlySourceTransport: a connection test never deletes a real object")
}

func (t *readOnlySourceTransport) ProbeSourceWrite(context.Context, transport.Source) error {
	t.probed++

	return t.probeErr
}

var (
	_ transport.Transport        = (*readOnlySourceTransport)(nil)
	_ transport.SourceWriteProbe = (*readOnlySourceTransport)(nil)
)

// refusedWriteProbe is what the rclone adapter produces for a source the
// account may read and not write: a classified PermissionDenied, which is
// the one category sourcecheck words as "read-only" rather than as
// "unproven".
func refusedWriteProbe() error {
	return transport.NewError(transport.PermissionDenied, "probe_source_write",
		errors.New("sftp: permission denied creating an object under the remote path"))
}

// openServiceWithTransport is openTestService over a transport the caller
// chose.
//
// New plus an assigned configPath rather than Open, because Open wires
// the real rclone adapter and there is no seam in it: this is the same
// construction Open performs, with the one dependency this test is about
// substituted. In-package, so nothing is exported for a test's benefit.
func openServiceWithTransport(t *testing.T, tr transport.Transport) (*BackupService, string) {
	t.Helper()

	configPath := writeTestConfigFile(t)

	cfg, journal, release, err := OpenConfigAndJournal(context.Background(), configPath)
	if err != nil {
		t.Fatalf("OpenConfigAndJournal: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	svc := New(cfg, journal, tr, nil)
	svc.configPath = config.ResolvePath(configPath)
	t.Cleanup(func() { _ = svc.Close() })

	return svc, configPath
}

// sshBackedCreateRequest is a create aimed at the in-process SSH server,
// so the connection check's connect and host-key steps are real and pass,
// authentication and listing come off the stub transport, and the write
// probe is the only step with anything to say.
func sshBackedCreateRequest(t *testing.T, configPath, name string) CreateBackupSetRequest {
	t.Helper()

	host, port, knownHostsLine := startTestSSHServer(t)

	return CreateBackupSetRequest{
		SourceName:         "production",
		Name:               name,
		Host:               host,
		Port:               port,
		User:               "backup-agent",
		SSHKeyID:           importedTestKey(t, configPath),
		KnownHostsLine:     knownHostsLine,
		RemotePath:         "/var/backups",
		LocalPath:          filepath.Join(t.TempDir(), name),
		Include:            []string{"*.dump"},
		CompletionStrategy: "marker",
	}
}

// TestCreateBackupSet_RefusesDeleteFromSourceWhenTheProbeProvesReadOnly
// is the create half of #852.
//
// Both directions are asked in one test on purpose. The refusal alone
// would pass just as well against a create path that refused everything,
// and the way PAST the refusal is the feature's whole design: read-only
// is not an override, it is the honest description of what that source
// can do, so the same request with read_only set must succeed.
func TestCreateBackupSet_RefusesDeleteFromSourceWhenTheProbeProvesReadOnly(t *testing.T) {
	tr := &readOnlySourceTransport{probeErr: refusedWriteProbe()}
	svc, configPath := openServiceWithTransport(t, tr)

	before := mustRead(t, configPath)

	req := sshBackedCreateRequest(t, configPath, "deletes-from-source")
	req.ReadOnly = false

	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrSourceNotWritable) {
		t.Fatalf("CreateBackupSet(read_only: false) against a source whose write probe was refused = %v, want ErrSourceNotWritable", err)
	}
	if tr.probed == 0 {
		t.Fatal("the write probe was never asked, so this refusal came from something other than the source's own permissions")
	}
	if after := mustRead(t, configPath); after != before {
		t.Errorf("a refused create still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The way past it, and the non-vacuity check for everything above: a
	// set that never deletes needs no delete permission.
	readOnlyReq := sshBackedCreateRequest(t, configPath, "reads-only")
	readOnlyReq.ReadOnly = true

	if _, err := svc.CreateBackupSet(context.Background(), readOnlyReq); err != nil {
		t.Fatalf("CreateBackupSet(read_only: true) against the same read-only source = %v, want it accepted: asking for read-only is the documented way past this refusal", err)
	}
}

// TestUpdateBackupSet_RefusesAnEditThatRepointsDeleteFromSourceAtAReadOnlySource
// is the edit half.
//
// It is the case the create half cannot cover: the set was written when
// the source WAS writable, nothing about the set's own posture changed,
// and the edit repoints it somewhere these credentials may only read. The
// permissions on somebody else's machine are not this deployment's to
// know about until it asks, and the moment it asks is this edit.
func TestUpdateBackupSet_RefusesAnEditThatRepointsDeleteFromSourceAtAReadOnlySource(t *testing.T) {
	tr := &readOnlySourceTransport{}
	svc, configPath := openServiceWithTransport(t, tr)

	// The fixture's own set reads a local path, so this edit's connection
	// check runs the local ladder: no host, no host key, and the write
	// probe still asked, which is exactly the shape of a NAS mounted on
	// this machine.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Writable first, so the edit path itself is proven to work before
	// the same edit is refused for the one reason under test.
	if _, err := svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		RemotePath: &elsewhere,
	}); err != nil {
		t.Fatalf("an edit repointing a writable source = %v, want it accepted", err)
	}
	if tr.probed == 0 {
		t.Fatal("the edit ran no write probe at all, so the refusal below would prove nothing about #852")
	}

	tr.probeErr = refusedWriteProbe()
	before := mustRead(t, configPath)

	readOnlyPath := filepath.Join(t.TempDir(), "read-only-source")
	if err := os.MkdirAll(readOnlyPath, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	_, err := svc.UpdateBackupSet(context.Background(), "production/postgres-primary", UpdateBackupSetRequest{
		RemotePath: &readOnlyPath,
	})
	if !errors.Is(err, ErrSourceNotWritable) {
		t.Fatalf("an edit repointing a delete-from-source set at a source whose write probe was refused = %v, want ErrSourceNotWritable", err)
	}
	if after := mustRead(t, configPath); after != before {
		t.Errorf("a refused edit still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
