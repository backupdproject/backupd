package kopia

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// The one claim this file makes: after this adapter opens a repository,
// the ENGINE will not delete a snapshot.
//
// It is an internal test because the thing under examination is the
// vendor's own retention calculation, and asking it directly -- "given
// these snapshots and this repository's stored policy, which manifests
// would you expire?" -- is the only assertion that is actually about the
// behaviour rather than about the shape of a policy struct. A test that
// read back six zeroes would pass just as well against a version of the
// vendor that had changed what six zeroes mean.
//
// The number of snapshots is chosen against the default policy it has to
// beat: the vendor's default keeps the latest 10, so eleven snapshots of
// one source in one hour is the smallest fixture in which an un-neutralized
// repository expires something and a neutralized one does not.
const engineRetentionSnapshots = 11

func TestTheEngineExpiresNothingInARepositoryThisAdapterOpened(t *testing.T) {
	ctx := context.Background()
	rep, src := repositoryWithSnapshots(t, engineRetentionSnapshots)

	// The direct question, asked of the vendor with reallyDelete false:
	// what WOULD a checkpoint's retention pass delete right now?
	expired, err := policy.ApplyRetentionPolicy(ctx, mustWriter(t, rep), src, false)
	if err != nil {
		t.Fatalf("ApplyRetentionPolicy: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("the engine would expire %d of this product's snapshots (%v) during the next long backup; "+
			"backupd decides which snapshots may be deleted, and a hold recorded in the catalog is invisible to this calculation",
			len(expired), expired)
	}

	// The control. This assertion is only evidence if the same fixture
	// under the vendor's DEFAULT policy really does expire something:
	// otherwise a neutralization that did nothing at all would pass.
	var def policy.RetentionPolicy
	def.Merge(policy.DefaultPolicy.RetentionPolicy, &policy.RetentionPolicyDefinition{}, policy.GlobalPolicySourceInfo)

	manifests, err := snapshot.ListSnapshots(ctx, rep, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	def.ComputeRetentionReasons(manifests)

	var wouldExpire int
	for _, m := range manifests {
		if len(m.RetentionReasons) == 0 {
			wouldExpire++
		}
	}
	if wouldExpire == 0 {
		t.Fatalf("the vendor's default retention policy keeps all %d snapshots in this fixture, so the assertion above proves nothing; "+
			"raise engineRetentionSnapshots past whatever the default now keeps", len(manifests))
	}
}

// TestASecondOpenDoesNotRewriteTheGlobalPolicy is the cheap-path claim.
// The correction is meant to be a manifest read on every open after the
// first; a version that wrote it every time would add a manifest per open
// to every repository this product touches, forever.
func TestASecondOpenDoesNotRewriteTheGlobalPolicy(t *testing.T) {
	ctx := context.Background()
	rep, _ := repositoryWithSnapshots(t, 1)

	first, err := policy.GetDefinedPolicy(ctx, rep, policy.GlobalPolicySourceInfo)
	if err != nil {
		t.Fatalf("GetDefinedPolicy: %v", err)
	}
	if !engineRetentionIsOff(first) {
		t.Fatalf("the global policy still expires snapshots after an open: %+v", first.RetentionPolicy)
	}

	// A second call against an already-neutral repository must not write.
	before := manifestCount(t, rep)
	if err := disableEngineRetention(ctx, rep); err != nil {
		t.Fatalf("disableEngineRetention (second call): %v", err)
	}
	if after := manifestCount(t, rep); after != before {
		t.Errorf("a repeat correction wrote %d new manifest(s); it is supposed to read and leave", after-before)
	}
}

// repositoryWithSnapshots creates a local repository through this adapter,
// takes n snapshots of one small source, and hands back the open
// repository and the source they were taken of.
func repositoryWithSnapshots(t *testing.T, n int) (repo.Repository, snapshot.SourceInfo) {
	t.Helper()

	ctx := context.Background()
	root := t.TempDir()

	domain, err := model.NewRepositoryDomainID("engine-policy")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	secret := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(secret, []byte("engine-policy-passphrase-not-a-secret\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase: %v", err)
	}

	loc := backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       root,
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Passphrase: secretref.Ref{File: secret},
	}

	adapter := New()
	if err := adapter.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	opened, err := adapter.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "data.txt"), []byte("one small file\n"), 0o600); err != nil {
		t.Fatalf("writing the source: %v", err)
	}

	source := backupengine.Source{Host: "test-host", User: "test-user", Path: srcDir}
	for i := range n {
		if _, err := opened.Snapshot(ctx, backupengine.SnapshotRequest{Source: source}); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
	}

	si, err := sourceInfo(source)
	if err != nil {
		t.Fatalf("sourceInfo: %v", err)
	}

	return opened.(*repository).rep, si
}

// mustWriter borrows a repository writer for a read-only retention
// computation. ApplyRetentionPolicy takes a writer because it can delete;
// this test never lets it, passing reallyDelete false.
func mustWriter(t *testing.T, rep repo.Repository) repo.RepositoryWriter {
	t.Helper()

	_, w, err := rep.NewWriter(context.Background(), repo.WriteSessionOptions{Purpose: "test:retention-preview"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { w.Close(context.Background()) }) //nolint:errcheck // test cleanup

	return w
}

// manifestCount is how many manifests of any kind the repository holds,
// which is what a policy write adds one to.
func manifestCount(t *testing.T, rep repo.Repository) int {
	t.Helper()

	all, err := rep.FindManifests(context.Background(), nil)
	if err != nil {
		t.Fatalf("FindManifests: %v", err)
	}

	return len(all)
}
