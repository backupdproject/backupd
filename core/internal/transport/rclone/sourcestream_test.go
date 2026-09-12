package rclone_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/backupdproject/backupd/core/internal/transport"
	"github.com/backupdproject/backupd/core/internal/transport/rclone"
)

// StatSource answers about an object's metadata without reading the
// object, and Stat does not, and the difference is the whole reason
// StatSource exists.
//
// The assertion is on the HASH field rather than on a timing, because the
// hash is what the read is for: rclone's local backend advertises SHA-256
// and produces one by reading every byte of the file. A streaming backup
// stats every object again after it has read it, to see whether it moved,
// so a hashing stat there reads a 100 GB source twice - which is the
// staging anti-pattern arriving through a method nobody looked at.
//
// Stat's behaviour is asserted too, as the positive control. Without it
// this test would keep passing if the local backend silently stopped
// advertising a hash, and would then be proving nothing about StatSource
// at all.
func TestStatSourceReportsMetadataWithoutReadingTheObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i)
	}

	if err := os.WriteFile(filepath.Join(dir, "object.bin"), body, 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	adapter := rclone.New()
	src := transport.Source{ID: "stat-source", Type: "local", Root: dir}
	ctx := context.Background()

	hashing, err := adapter.Stat(ctx, src, "object.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if hashing.Hash == "" {
		t.Fatal("the local backend no longer produces a hash from Stat, so this test's control is gone and it proves nothing about StatSource")
	}

	metadata, err := adapter.StatSource(ctx, src, "object.bin")
	if err != nil {
		t.Fatalf("StatSource: %v", err)
	}

	if metadata.Hash != "" || metadata.HashAlg != "" {
		t.Errorf("StatSource produced the hash %q (%s); producing one means it read the whole object", metadata.Hash, metadata.HashAlg)
	}

	if metadata.Size != int64(len(body)) {
		t.Errorf("StatSource reports %d bytes, want %d", metadata.Size, len(body))
	}

	if metadata.ModTime != hashing.ModTime {
		t.Errorf("StatSource reports the modification time %d and Stat reports %d; they describe the same object", metadata.ModTime, hashing.ModTime)
	}

	// Kind is not answered, on purpose: rclone's object model cannot
	// classify a path (NewObject returns an object for a fifo and
	// follows a symlink), and a consumer that needs a kind must get it
	// from the enumerator rather than from a resolution.
	if metadata.Kind != transport.EntryKindUnknown {
		t.Errorf("StatSource claims the kind %q; rclone's object model does not answer that question", metadata.Kind)
	}
}

// A path that names nothing is a not-found rather than an empty artifact,
// so a caller can tell "the file was deleted between the listing and the
// read" from "the file is here and is empty".
func TestStatSourceRefusesAPathThatNamesNothing(t *testing.T) {
	t.Parallel()

	adapter := rclone.New()
	src := transport.Source{ID: "stat-missing", Type: "local", Root: t.TempDir()}

	if _, err := adapter.StatSource(context.Background(), src, "gone.bin"); err == nil {
		t.Fatal("StatSource invented an artifact for a path that names nothing")
	}
}

// One session serves every object of a run, and a stream taken from it
// stays readable while other objects are opened and closed around it.
//
// The second half is the bug this shape invites. OpenSourceStream binds
// the Fs it built to the reader it returns, so closing the reader hangs
// up; a session that reused that wrapper would have the first object a
// worker finished reading tear down the connection every OTHER worker
// was still reading through.
func TestASourceSessionServesManyObjectsAtOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	bodies := map[string][]byte{
		"a.bin": []byte("the first object"),
		"b.bin": []byte("the second object, which is a different length"),
		"c.bin": []byte("the third"),
	}

	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	adapter := rclone.New()
	src := transport.Source{ID: "session", Type: "local", Root: dir}
	ctx := context.Background()

	session, err := adapter.OpenSourceSession(ctx, src)
	if err != nil {
		t.Fatalf("OpenSourceSession: %v", err)
	}

	defer func() {
		if err := session.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}

		// Closing twice is what a run that both defers a close and
		// closes explicitly does, and it must not be an error.
		if err := session.Close(); err != nil {
			t.Errorf("closing the session a second time: %v", err)
		}
	}()

	// Hold one object open across the whole exercise.
	held, err := session.OpenStream(ctx, "a.bin")
	if err != nil {
		t.Fatalf("OpenStream(a.bin): %v", err)
	}

	for _, name := range []string{"b.bin", "c.bin"} {
		art, err := session.StatSource(ctx, name)
		if err != nil {
			t.Fatalf("StatSource(%s): %v", name, err)
		}

		if art.Size != int64(len(bodies[name])) {
			t.Errorf("StatSource(%s) reports %d bytes, want %d", name, art.Size, len(bodies[name]))
		}

		rc, err := session.OpenStream(ctx, name)
		if err != nil {
			t.Fatalf("OpenStream(%s): %v", name, err)
		}

		got, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}

		if !bytes.Equal(got, bodies[name]) {
			t.Errorf("%s came back as %q", name, got)
		}
	}

	// The reader opened first is unaffected by the readers closed after
	// it.
	got, err := io.ReadAll(held)
	_ = held.Close()

	if err != nil {
		t.Fatalf("reading the object held open across the session: %v", err)
	}

	if !bytes.Equal(got, bodies["a.bin"]) {
		t.Errorf("the held object came back as %q", got)
	}
}

// A session over a source that cannot be reached is a refusal at OPEN,
// not a handle that fails once per object.
func TestOpenSourceSessionRefusesABackendItCannotBuild(t *testing.T) {
	t.Parallel()

	adapter := rclone.New()
	src := transport.Source{ID: "nonsense", Type: "not-a-registered-backend", Root: t.TempDir()}

	if _, err := adapter.OpenSourceSession(context.Background(), src); err == nil {
		t.Fatal("a session was opened against a backend this binary does not have")
	}
}
