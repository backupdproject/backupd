package kopia

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// This file holds the two claims about a tree snapshot that the boundary
// cannot express and therefore cannot be tested from outside the package.
//
// backupengine.SnapshotInfo has no Tags field and no notion of the
// vendor's own source namespace, deliberately: neither belongs in a port
// that has to survive the engine being replaced. But "one run writes ONE
// source, carrying the caller's attribution" is the entire difference
// between this port and the per-object one it replaces, and a claim
// nothing checks is a claim that lasts one refactor. So it is checked
// here, where the vendor's names are allowed to be spoken.

// treeInternalPassphrase is this file's repository passphrase. It is a
// literal rather than a shared constant because the constant belongs to
// the external test package, which this one cannot see.
const treeInternalPassphrase = "tree-internal-passphrase-not-a-secret"

// treeInternalRepository builds a local repository and returns the
// concrete handle, which is what makes reading a manifest back possible.
func treeInternalRepository(t *testing.T) *repository {
	t.Helper()

	ctx := context.Background()

	domain, err := model.NewRepositoryDomainID("production")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	secret := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(secret, []byte(treeInternalPassphrase+"\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       t.TempDir(),
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Passphrase: secretref.Ref{File: secret},
	}

	eng := New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	opened, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := opened.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	rep, ok := opened.(*repository)
	if !ok {
		t.Fatalf("OpenRepository returned %T, not this package's own handle", opened)
	}

	return rep
}

// staticSourceDir is the smallest source tree that has a file in it.
type staticSourceDir struct{ entries []backupengine.SourceEntry }

func (d *staticSourceDir) Open(context.Context) (backupengine.SourceDirIterator, error) {
	return &staticSourceIter{entries: d.entries}, nil
}

type staticSourceIter struct {
	entries []backupengine.SourceEntry
	next    int
}

func (i *staticSourceIter) Next(context.Context) (backupengine.SourceEntry, bool, error) {
	if i.next >= len(i.entries) {
		return backupengine.SourceEntry{}, false, nil
	}

	e := i.entries[i.next]
	i.next++

	return e, true, nil
}

func (i *staticSourceIter) Close() error { return nil }

// staticSourceFile is one object's bytes.
type staticSourceFile struct{ body string }

func (f staticSourceFile) ModTime() time.Time { return time.Unix(1700000000, 0).UTC() }

func (f staticSourceFile) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}

// TestSnapshotTreeStoresOneKopiaSourceCarryingTheCallersTags is the claim
// the set-scoped port exists to make, stated in the vendor's own terms.
//
// The per-object port writes one Kopia source PER OBJECT, which is why
// RepositoryStats.Sources cannot be counted from the vendor's source list
// and is counted from the backup-set tag instead. This port writes one
// source per RUN, so a three-object set that produced three sources would
// mean the replacement had not actually happened -- and every symptom of
// that is downstream and indirect, which is why it is asserted here at
// the point where it is true or not.
//
// The tags are the other half. The engine synthesises neither of them:
// the caller owns attribution because the caller is the only thing that
// knows which backup set is running, and this asserts that what the
// caller passed is what the manifest holds, unaltered.
func TestSnapshotTreeStoresOneKopiaSourceCarryingTheCallersTags(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep := treeInternalRepository(t)

	root := &staticSourceDir{entries: []backupengine.SourceEntry{
		{Name: "a.txt", Stream: staticSourceFile{body: "a\n"}, Size: 2},
		{Name: "b.txt", Stream: staticSourceFile{body: "bb\n"}, Size: 3},
		{Name: "nested", Dir: &staticSourceDir{entries: []backupengine.SourceEntry{
			{Name: "c.txt", Stream: staticSourceFile{body: "ccc\n"}, Size: 4},
		}}},
	}}

	want := map[string]string{
		backupengine.TagKeyBackupSet: "set-nightly",
		backupengine.TagKeyDomain:    "production",
	}

	info, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
		Source:      backupengine.Source{Host: "tree-host", User: "tree-user", Path: "/sets/nightly"},
		Root:        root,
		Description: "nightly run",
		Tags:        want,
	})
	if err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	sources, err := snapshot.ListSources(ctx, rep.rep)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}

	if len(sources) != 1 {
		t.Fatalf("the run wrote %d engine sources (%v), want exactly 1: a set is one source, not one per object", len(sources), sources)
	}

	if got := sources[0].Path; got != "/sets/nightly" {
		t.Errorf("the run wrote source path %q, want the backup set's own path", got)
	}

	man, err := snapshot.LoadSnapshot(ctx, rep.rep, manifest.ID(info.ID))
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	for key, value := range want {
		if got := man.Tags[key]; got != value {
			t.Errorf("the manifest carries %s=%q, want %q", key, got, value)
		}
	}

	if len(man.Tags) != len(want) {
		t.Errorf("the manifest carries %d tags (%v), want only the %d the caller passed; the engine must not synthesise attribution",
			len(man.Tags), man.Tags, len(want))
	}

	if man.Description != "nightly run" {
		t.Errorf("the manifest carries description %q, want the caller's", man.Description)
	}
}
