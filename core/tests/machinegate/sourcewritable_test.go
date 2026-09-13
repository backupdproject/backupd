package machinegate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/core/service"
	"github.com/backupdproject/backupd/core/tests/machines"
)

// Issue #852 driven through core/service against a real SFTP server: the
// connection test proves whether a source can be written to, and every
// write that would enable FR-16's delete-from-source is refused when it
// cannot.
//
// One container, two directories, one permission bit between them. That
// pairing is the whole design: an implementation that always answered
// "writable" and one that always answered "read-only" would each pass half
// of this file and neither passes it whole. The container is ephemeral and
// removed by machines.Start's own cleanup, on success, on failure and on a
// panic.
//
// It lives in this tier rather than in package service because everything
// it asks is a question only a real server can answer. The SFTP server
// decides whether a create under a directory is permitted, and a double
// standing in for it would be this test asserting its own premise.

// writableAndReadOnly seeds, under the fixture's bind-mounted upload
// directory, one directory its SFTP account may write in and one it may
// not, and returns the two REMOTE paths (inside the chroot) to configure
// backup sets against.
//
// 0o555 is the read-only one: the account inside the container is a plain
// non-root user that does not own these host-created directories (the fact
// machines.Source.Deny already relies on), so clearing the write bits
// leaves it the read and traverse a backup needs while denying the create a
// delete-from-source would need. That is precisely the posture this feature
// exists for, and it is the posture docs/ssh-setup.md's hardened account is
// one chmod away from.
func writableAndReadOnly(t *testing.T, fx *machines.Source) (writableRemote, readOnlyRemote string) {
	t.Helper()
	const writableName, readOnlyName = "writable-source", "readonly-source"

	for _, name := range []string{writableName, readOnlyName} {
		if err := os.MkdirAll(filepath.Join(fx.UploadDir, name), 0o777); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		// Explicitly, because MkdirAll applies the process umask.
		if err := os.Chmod(filepath.Join(fx.UploadDir, name), 0o777); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}

	// Seeded while the directory is still writable: a read-only source
	// with nothing in it would let the listing step pass for the wrong
	// reason, and a real one has a producer's files in it.
	if err := os.WriteFile(filepath.Join(fx.UploadDir, readOnlyName, "backup.dump"), []byte("a producer's file on a read-only source"), 0o644); err != nil {
		t.Fatalf("seeding the read-only source: %v", err)
	}
	if err := os.Chmod(filepath.Join(fx.UploadDir, readOnlyName), 0o555); err != nil {
		t.Fatalf("making the read-only source read-only: %v", err)
	}
	// Put the write bits back so the test's own temp-dir cleanup can
	// remove the tree.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(fx.UploadDir, readOnlyName), 0o755) })
	return "/upload/" + writableName, "/upload/" + readOnlyName
}

// TestSourceWritability_EndToEndAgainstARealSFTPFixture is issue #852's
// acceptance criteria, in the order an operator meets them.
func TestSourceWritability_EndToEndAgainstARealSFTPFixture(t *testing.T) {
	fx := machines.Start(t).Source(t)
	writableRemote, readOnlyRemote := writableAndReadOnly(t, fx)

	svc := openService(t)

	keyPEM, err := os.ReadFile(fx.KeyFile)
	if err != nil {
		t.Fatalf("reading the fixture key: %v", err)
	}
	keyRef, err := svc.ImportSSHKey(context.Background(), keyPEM, "")
	if err != nil {
		t.Fatalf("ImportSSHKey: %v", err)
	}
	probe, err := svc.ProbeHostKey(context.Background(), fx.Host, fx.Port)
	if err != nil {
		t.Fatalf("ProbeHostKey: %v", err)
	}

	testConnection := func(t *testing.T, remotePath string) service.ConnectionTestResult {
		t.Helper()
		result, err := svc.TestConnection(context.Background(), service.ConnectionTestRequest{
			Host:           fx.Host,
			Port:           fx.Port,
			User:           fx.User,
			SSHKeyID:       keyRef.ID,
			KnownHostsLine: probe.KnownHostsLine,
			RemotePath:     remotePath,
		})
		if err != nil {
			t.Fatalf("TestConnection(%s): %v", remotePath, err)
		}
		return result
	}

	createReq := func(name, remotePath string, readOnly bool) service.CreateBackupSetRequest {
		return service.CreateBackupSetRequest{
			Name:               name,
			Host:               fx.Host,
			Port:               fx.Port,
			User:               fx.User,
			SSHKeyID:           keyRef.ID,
			KnownHostsLine:     probe.KnownHostsLine,
			RemotePath:         remotePath,
			LocalPath:          t.TempDir(),
			Include:            []string{"*.dump"},
			CompletionStrategy: "rename",
			ReadOnly:           readOnly,
			Actor:              "integration-test",
		}
	}

	writeProbeStep := func(t *testing.T, result service.ConnectionTestResult) service.ConnectionCheck {
		t.Helper()
		for _, c := range result.Checks {
			if c.Step == "write_probe" {
				return c
			}
		}
		t.Fatalf("the connection test reported no write_probe step at all: %+v", result.Checks)
		return service.ConnectionCheck{}
	}

	t.Run("a writable source is reported writable", func(t *testing.T) {
		result := testConnection(t, writableRemote)
		if !result.OK {
			t.Fatalf("the connection test failed against a real, reachable source: %q", result.Message)
		}
		if !result.Writable {
			t.Errorf("writable is false for a directory the account can write: %+v", writeProbeStep(t, result))
		}
		if step := writeProbeStep(t, result); step.Outcome != "passed" {
			t.Errorf("write_probe outcome %q, want passed: %+v", step.Outcome, step)
		}
		// No leftovers, checked on the host side rather than through the
		// session that deleted the probe.
		entries, err := os.ReadDir(filepath.Join(fx.UploadDir, "writable-source"))
		if err != nil {
			t.Fatalf("reading the writable source back: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("the write probe left %d file(s) behind on the source", len(entries))
		}
	})

	t.Run("a read-only source is reported not writable and still connects", func(t *testing.T) {
		result := testConnection(t, readOnlyRemote)
		// The sharpest assertion in this file: read-only is a supported
		// posture, so the test as a whole still passes.
		if !result.OK {
			t.Fatalf("the connection test FAILED against a read-only source; read-only is a valid posture, not a broken connection: %q / %+v", result.Message, result.Checks)
		}
		if result.Writable {
			t.Fatal("writable is true for a directory with no write bits; the UI would have offered delete-from-source for a source backupd cannot delete from")
		}
		step := writeProbeStep(t, result)
		if step.Outcome != "passed" {
			t.Errorf("write_probe outcome %q, want passed (the step ran; only its answer was no): %+v", step.Outcome, step)
		}
		if step.Detail == "" {
			t.Error("write_probe carries no detail, so an operator is told nothing about why delete-from-source is unavailable")
		}
		entries, err := os.ReadDir(filepath.Join(fx.UploadDir, "readonly-source"))
		if err != nil {
			t.Fatalf("reading the read-only source back: %v", err)
		}
		if len(entries) != 1 {
			t.Errorf("the refused probe changed the source directory: %d entries, want the 1 seeded artifact", len(entries))
		}
	})

	t.Run("creating a delete-enabled set against a read-only source is refused", func(t *testing.T) {
		_, err := svc.CreateBackupSet(context.Background(), createReq("readonly-delete-enabled", readOnlyRemote, false))
		if !errors.Is(err, service.ErrSourceNotWritable) {
			t.Fatalf("CreateBackupSet(read_only:false) against a read-only source = %v, want ErrSourceNotWritable", err)
		}
		if _, err := svc.GetBackupSet(context.Background(), "api/readonly-delete-enabled"); err == nil {
			t.Error("the refused create persisted the backup set anyway")
		}
	})

	t.Run("the same set is created read-only", func(t *testing.T) {
		result, err := svc.CreateBackupSet(context.Background(), createReq("readonly-source-set", readOnlyRemote, true))
		if err != nil {
			t.Fatalf("CreateBackupSet(read_only:true) against a read-only source: %v", err)
		}
		set, err := svc.GetBackupSet(context.Background(), result.Set.ID)
		if err != nil {
			t.Fatalf("GetBackupSet: %v", err)
		}
		if !set.ReadOnly {
			t.Error("the created set is not read-only, so FR-15's delete step is still reachable for a source that cannot be deleted from")
		}

		// And the read-only posture cannot be turned off while the
		// source stays read-only: the CLI's `backup-set read-only <set>
		// off` verb and the API route behind it both land here.
		if _, err := svc.SetBackupSetReadOnly(context.Background(), result.Set.ID, false); !errors.Is(err, service.ErrSourceNotWritable) {
			t.Fatalf("SetBackupSetReadOnly(false) on a read-only source = %v, want ErrSourceNotWritable", err)
		}
		still, err := svc.GetBackupSet(context.Background(), result.Set.ID)
		if err != nil {
			t.Fatalf("GetBackupSet after the refusal: %v", err)
		}
		if !still.ReadOnly {
			t.Error("a refused SetBackupSetReadOnly(false) still cleared read-only")
		}
	})

	t.Run("a writable source may have delete-from-source enabled", func(t *testing.T) {
		// The positive control for all three refusals above: the same
		// request shape, against the writable directory, is accepted, and
		// read-only can be turned back off on it.
		result, err := svc.CreateBackupSet(context.Background(), createReq("writable-source-set", writableRemote, false))
		if err != nil {
			t.Fatalf("CreateBackupSet(read_only:false) against a WRITABLE source was refused: %v", err)
		}
		set, err := svc.GetBackupSet(context.Background(), result.Set.ID)
		if err != nil {
			t.Fatalf("GetBackupSet: %v", err)
		}
		if set.ReadOnly {
			t.Error("a set created with read_only:false against a writable source came back read-only")
		}
		if _, err := svc.SetBackupSetReadOnly(context.Background(), result.Set.ID, true); err != nil {
			t.Fatalf("SetBackupSetReadOnly(true): %v", err)
		}
		if _, err := svc.SetBackupSetReadOnly(context.Background(), result.Set.ID, false); err != nil {
			t.Fatalf("SetBackupSetReadOnly(false) against a writable source was refused: %v", err)
		}
	})
}
