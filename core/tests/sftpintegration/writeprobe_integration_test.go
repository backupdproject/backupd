package sftpintegration_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/transport"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
	"github.com/backupdproject/backupd/core/tests/machines"
)

// Issue #852's evidence, against a real SFTP server: whether an account may
// DELETE from a source is a different question from whether it may read it,
// and the only honest way to answer it is to write something and take it
// away again.
//
// Both answers are proven against the same container, on two directories
// that differ in one permission bit. That pairing is the whole design of
// this file: a probe implementation that simply always failed would pass a
// read-only-only test, and one that always succeeded would pass a
// writable-only test. Neither passes both.
//
// The container is ephemeral and torn down by machines.Start's own
// t.Cleanup, including on failure and including when a test panics, so
// nothing here has a "if the test fails, docker rm by hand" step.

// probeDirs creates, under this fixture's bind-mounted upload directory, one
// directory the SFTP account can write in and one it cannot, and returns the
// roots (relative to the upload directory) to point a transport.Source at.
//
// The read-only one is 0o555: the SFTP account inside the container is a
// plain, non-root user that does not own these host-created directories (the
// same fact Source.Deny relies on), so clearing the write bits denies it a
// create while leaving the read and traverse it needs to still LIST the
// path, which is exactly the posture this feature exists for — a source that
// backs up perfectly and can never be deleted from.
func probeDirs(t *testing.T, src *machines.Source) (writable, readOnly string) {
	t.Helper()
	writable, readOnly = "probe-writable", "probe-readonly"

	for _, dir := range []string{writable, readOnly} {
		if err := os.MkdirAll(filepath.Join(src.UploadDir, dir), 0o777); err != nil {
			t.Fatalf("creating %s under the fixture's upload directory: %v", dir, err)
		}
	}
	// MkdirAll is subject to the process umask, so the mode is set
	// explicitly afterwards rather than trusted from the create.
	if err := os.Chmod(filepath.Join(src.UploadDir, writable), 0o777); err != nil {
		t.Fatalf("making the writable probe directory writable: %v", err)
	}
	if err := os.Chmod(filepath.Join(src.UploadDir, readOnly), 0o555); err != nil {
		t.Fatalf("making the read-only probe directory read-only: %v", err)
	}
	// Restored so the test's own cleanup can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src.UploadDir, readOnly), 0o755) })
	return writable, readOnly
}

func probeDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s back on the host side: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestSourceWriteProbe_AgainstARealServer is issue #852's acceptance
// criterion for the transport half: a writable source answers yes, a
// read-only source answers no rather than breaking, and neither leaves a
// probe file behind.
func TestSourceWriteProbe_AgainstARealServer(t *testing.T) {
	m := machines.Start(t)
	src := m.Source(t)
	writableDir, readOnlyDir := probeDirs(t, src)

	adapter := rclone.New()
	probe, ok := any(adapter).(transport.SourceWriteProbe)
	if !ok {
		t.Fatal("the rclone adapter does not implement transport.SourceWriteProbe, so no connection test can ever prove a source writable")
	}

	t.Run("a writable source is proven writable", func(t *testing.T) {
		if err := probe.ProbeSourceWrite(src.Context(), src.TransportSource("probe-writable-set", writableDir)); err != nil {
			t.Fatalf("the probe failed against a directory this account can write: %v", err)
		}
		// The probe object is gone, proven on the host side rather than
		// through the same SFTP session that deleted it: the whole claim
		// is that nothing is left on the operator's machine.
		if names := probeDirEntries(t, filepath.Join(src.UploadDir, writableDir)); len(names) != 0 {
			t.Errorf("the probe left %v behind in a directory it was supposed to clean up", names)
		}
	})

	t.Run("a read-only source is proven not writable, and nothing is left behind", func(t *testing.T) {
		err := probe.ProbeSourceWrite(src.Context(), src.TransportSource("probe-readonly-set", readOnlyDir))
		if err == nil {
			t.Fatal("the probe reported a directory with no write bits as writable; delete-from-source would have been offered for a source backupd cannot delete from")
		}
		// Read-only, not broken: the category is what the connection
		// check words its "this source is read-only" sentence from, and a
		// permission refusal has to arrive as one.
		category, classified := transport.CategoryOf(err)
		if !classified || category != transport.PermissionDenied {
			t.Errorf("the refusal classified as (%v, %v), want transport.PermissionDenied: a read-only account is a posture and the category is how a surface tells it apart from an unreachable host", category, classified)
		}
		// A create that was refused cannot have left anything, and this
		// is the assertion that would catch a probe that wrote first and
		// checked permissions afterwards.
		if names := probeDirEntries(t, filepath.Join(src.UploadDir, readOnlyDir)); len(names) != 0 {
			t.Errorf("a refused probe left %v behind", names)
		}
		if errors.Is(err, transport.ErrProbeNotRemoved) {
			t.Error("a write that never landed was reported as a probe that could not be removed, which would tell an operator to go and delete a file that does not exist")
		}
	})

	t.Run("the refusal names no path, no key and no probe file", func(t *testing.T) {
		err := probe.ProbeSourceWrite(src.Context(), src.TransportSource("probe-readonly-set", readOnlyDir))
		if err == nil {
			t.Fatal("expected a refusal to inspect")
		}
		// This error IS what the operator's log records: internal/
		// sourcecheck hands a step's cause straight to Deps.Observe,
		// which core/service logs. Issue #852's fourth requirement is
		// that nothing from this probe leaks there.
		text := err.Error()
		for _, secret := range []string{
			src.UploadDir,
			src.KeyFile,
			src.KnownHostsFile,
			readOnlyDir,
			"upload/",
			transport.ProbeObjectPrefix,
		} {
			if secret == "" {
				continue
			}
			if strings.Contains(text, secret) {
				t.Errorf("the probe's error carries %q, which reaches the operator's log through Deps.Observe: %q", secret, text)
			}
		}
	})
}
