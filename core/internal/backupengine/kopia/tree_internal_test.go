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

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/secretref"
)

// This file holds the claims about a tree snapshot that the boundary
// cannot express and therefore cannot be tested from outside the package.
//
// backupengine.SnapshotInfo reports a snapshot's Tags, so the tags a run
// stored are checkable from out there and tree_test.go checks them. What
// is not is the vendor's own source namespace: it has no place in a port
// that has to survive the engine being replaced, and "one run writes ONE
// source" is nonetheless the entire difference between this port and the
// per-object one it replaces. The same goes for the manifest's tags seen
// as the vendor holds them, which is where "the adapter added the run id
// and invented nothing else" is true or not. A claim nothing checks is a
// claim that lasts one refactor, so both are checked here, where the
// vendor's names are allowed to be spoken.

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
// The tags are the other half, and they have two owners. The engine
// synthesises no set and no domain: the caller owns that attribution,
// because the caller is the only thing that knows which backup set is
// running, and this asserts that what the caller passed is what the
// manifest holds, unaltered. The run id is the one tag the ADAPTER
// writes, from the RunID the request had to carry, and it is written
// here rather than trusted to the caller because a caller that forgot it
// would produce a manifest crash reconciliation could only match by
// timestamp -- and a timestamp does not tell this set's snapshot from a
// co-tenant's. So the assertion is that the manifest holds exactly the
// caller's tags plus that one, and nothing else invented along the way.
func TestSnapshotTreeStoresOneKopiaSourceCarryingTheRunsAttribution(t *testing.T) {
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
		RunID:       "run-4b7d2e10",
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

	if got := man.Tags[backupengine.TagKeyRun]; got != "run-4b7d2e10" {
		t.Errorf("the manifest carries %s=%q, want the run id the request carried; without it an orphaned manifest can only be matched to a run by its timestamp",
			backupengine.TagKeyRun, got)
	}

	if len(man.Tags) != len(want)+1 {
		t.Errorf("the manifest carries %d tags (%v), want the %d the caller passed plus the run id; the adapter writes attribution, it does not invent it",
			len(man.Tags), man.Tags, len(want))
	}

	// The caller's own map is untouched. A request is the caller's value
	// and may be reused for the next run; an adapter that recorded its
	// run id in it would make the second run carry the first one's.
	if _, ok := want[backupengine.TagKeyRun]; ok {
		t.Errorf("the adapter wrote its run tag into the caller's own map (%v); a request is not the adapter's to edit", want)
	}

	if man.Description != "nightly run" {
		t.Errorf("the manifest carries description %q, want the caller's", man.Description)
	}
}
