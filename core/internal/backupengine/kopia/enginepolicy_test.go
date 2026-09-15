package kopia

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/secretref"
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

	// The pins this adapter puts on its own manifests are removed first,
	// because this test is about the STORED POLICY and nothing else. A
	// pinned manifest survives every policy there is (see
	// TestAPolicySomebodyElseSetCannotExpireThisProductsSnapshots), so
	// leaving them on would make this assertion pass against a
	// neutralization that had been deleted.
	stripEnginePins(t, rep, src)

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
	intoOneHour(manifests)
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

// intoOneHour rewrites in-memory manifest start times so that every
// snapshot in the fixture falls inside ONE hour of one day, a minute
// apart, newest last.
//
// The control above needs the vendor's default policy to expire something,
// and what decides that is which calendar buckets the fixture's instants
// land in: eleven snapshots taken in the same second land in one hour and
// the default keeps ten of them, but the same eleven taken across an hour
// or midnight boundary are kept by KeepHourly or KeepDaily instead and
// nothing expires. That is a property of the wall clock the test ran at,
// not of this adapter, and a control that fails at :59 past the hour is a
// control nobody trusts.
func intoOneHour(manifests []*snapshot.Manifest) {
	base := time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC)
	for i, m := range manifests {
		m.StartTime = fs.UTCTimestampFromTime(base.Add(time.Duration(i) * time.Minute))
		m.EndTime = fs.UTCTimestampFromTime(base.Add(time.Duration(i)*time.Minute + time.Second))
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
// takes n snapshots of one small source, and hands back the open Kopia
// repository and the source they were taken of.
func repositoryWithSnapshots(t *testing.T, n int) (repo.Repository, snapshot.SourceInfo) {
	t.Helper()

	opened, source := adapterWithSnapshots(t, n)

	si, err := sourceInfo(source)
	if err != nil {
		t.Fatalf("sourceInfo: %v", err)
	}

	return opened.(*repository).rep, si
}

// adapterWithSnapshots is the same fixture seen through this product's own
// boundary, for the assertions that are about what backupd can do rather
// than about what the vendor would compute.
func adapterWithSnapshots(t *testing.T, n int) (backupengine.Repository, backupengine.Source) {
	t.Helper()

	ctx := context.Background()
	loc := localLocation(t)

	adapter := New()
	if err := adapter.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	opened, err := adapter.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })

	source := backupengine.Source{Host: "test-host", User: "test-user", Path: sourceTree(t)}
	for i := range n {
		if _, err := opened.Snapshot(ctx, backupengine.SnapshotRequest{Source: source}); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
	}

	return opened, source
}

// localLocation is one filesystem repository location, passphrase and all.
func localLocation(t *testing.T) backupengine.RepositoryLocation {
	t.Helper()

	domain, err := model.NewRepositoryDomainID("engine-policy")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	secret := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(secret, []byte("engine-policy-passphrase-not-a-secret\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase: %v", err)
	}

	return backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     domain,
		Root:       t.TempDir(),
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Passphrase: secretref.Ref{File: secret},
	}
}

// sourceTree is one small directory worth snapshotting.
func sourceTree(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("one small file\n"), 0o600); err != nil {
		t.Fatalf("writing the source: %v", err)
	}

	return dir
}

// TestEveryManifestThisAdapterSavesIsPinned walks all three of this
// adapter's write paths and asserts the pin on what each of them stored.
//
// One test rather than three, because the failure it guards is a FOURTH
// path: a snapshot kind added later that saves a manifest without the pin
// would produce snapshots the engine may expire behind the catalog, and
// nothing about that path would look wrong. Here, a new write path is
// either listed in this test or its manifests are visibly unprotected.
func TestEveryManifestThisAdapterSavesIsPinned(t *testing.T) {
	ctx := context.Background()
	rep := treeInternalRepository(t)

	if _, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source: backupengine.Source{Host: "pin-host", User: "pin-user", Path: sourceTree(t)},
	}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if _, err := rep.SnapshotTree(ctx, backupengine.TreeSnapshotRequest{
		Source: backupengine.Source{Host: "pin-host", User: "pin-user", Path: "/sets/nightly"},
		RunID:  "run-pin-tree",
		Root: &staticSourceDir{entries: []backupengine.SourceEntry{
			{Name: "a.txt", Stream: staticSourceFile{body: "a\n"}, Size: 2},
		}},
	}); err != nil {
		t.Fatalf("SnapshotTree: %v", err)
	}

	if _, err := rep.SnapshotStream(ctx, backupengine.StreamSnapshotRequest{
		Source: backupengine.Source{Host: "pin-host", User: "pin-user", Path: "/objects/db.dump"},
		Stream: staticSourceFile{body: "dump\n"},
	}); err != nil {
		t.Fatalf("SnapshotStream: %v", err)
	}

	sources, err := snapshot.ListSources(ctx, rep.rep)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 3 {
		t.Fatalf("the three write paths wrote %d engine sources (%v), want one each", len(sources), sources)
	}

	for _, si := range sources {
		manifests, err := snapshot.ListSnapshots(ctx, rep.rep, si)
		if err != nil {
			t.Fatalf("ListSnapshots(%v): %v", si, err)
		}
		if len(manifests) == 0 {
			t.Fatalf("source %v holds no manifest", si)
		}
		for _, m := range manifests {
			if !slices.Contains(m.Pins, enginePin) {
				t.Errorf("the manifest %s stored for %v carries pins %v, want %q: without it the engine's own retention may expire this snapshot",
					m.ID, si, m.Pins, enginePin)
			}
		}
	}
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

// TestAPolicySomebodyElseSetCannotExpireThisProductsSnapshots is the claim
// the global neutralization above cannot make on its own.
//
// What a checkpoint's retention pass reads is the EFFECTIVE policy for the
// source it is uploading, and the effective policy is not the global one:
// a host, user@host or path policy overrides it, and a NoParent policy
// discards it. One `kopia policy set --host nas-1 --keep-latest 1` against
// a bucket this product shares with somebody's own kopia install is enough
// to make the open-time check read "already neutral" while the next long
// upload deletes held snapshots.
//
// So every manifest this adapter saves is PINNED, and a pin is the one
// thing the vendor's expiry honours regardless of policy
// (snapshot/policy/expire.go keeps any manifest with a pin). This is the
// load-bearing guarantee; the global policy is defence in depth.
func TestAPolicySomebodyElseSetCannotExpireThisProductsSnapshots(t *testing.T) {
	ctx := context.Background()
	rep, src := repositoryWithSnapshots(t, 3)

	// Somebody else's policy, set at a level that beats the global one.
	setHostKeepLatest(t, rep, src, 1)

	expired, err := policy.ApplyRetentionPolicy(ctx, mustWriter(t, rep), src, false)
	if err != nil {
		t.Fatalf("ApplyRetentionPolicy: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("a host policy nobody in this product wrote would expire %d of its snapshots (%v); "+
			"the pin on every manifest this adapter saves is what has to stop that", len(expired), expired)
	}

	// The control: the same repository, the same policy, the same
	// snapshots -- with the pin taken off. If this expires nothing then
	// the fixture never posed the question.
	stripEnginePins(t, rep, src)

	unpinned, err := policy.ApplyRetentionPolicy(ctx, mustWriter(t, rep), src, false)
	if err != nil {
		t.Fatalf("ApplyRetentionPolicy (control): %v", err)
	}
	if len(unpinned) == 0 {
		t.Fatalf("without the pin the host policy expires nothing either, so the assertion above proves nothing")
	}
}

// TestBackupdCanStillDeleteAPinnedSnapshot is the other half of the pin.
//
// A pin that this product could not get past would have replaced an engine
// that deletes snapshots behind the catalog with a repository whose
// snapshots can never be removed at all, and retention would report
// deletions that never happened. The vendor's own DeleteManifest ignores
// pins, which is why the adapter's delete path uses it.
func TestBackupdCanStillDeleteAPinnedSnapshot(t *testing.T) {
	ctx := context.Background()
	opened, source := adapterWithSnapshots(t, 2)

	snaps, err := opened.ListSnapshots(ctx, source)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("the fixture holds %d snapshots, want 2", len(snaps))
	}

	if err := opened.DeleteSnapshot(ctx, snaps[0].ID); err != nil {
		t.Fatalf("DeleteSnapshot on a manifest this adapter pinned: %v", err)
	}

	after, err := opened.ListSnapshots(ctx, source)
	if err != nil {
		t.Fatalf("ListSnapshots after the delete: %v", err)
	}
	if len(after) != 1 || after[0].ID != snaps[1].ID {
		t.Errorf("after deleting %s the repository holds %+v, want only %s", snaps[0].ID, after, snaps[1].ID)
	}
}

// TestARepositoryWhosePolicyCannotBeWrittenStillOpens is the restore case.
//
// Opening is how this product reaches a repository to READ it: a restore,
// a verification drill and a lifecycle reconciliation all go through
// OpenRepository. Storage that will not accept a write is an ordinary
// disaster-recovery posture -- a WORM bucket, an object lock, a snapshot
// mounted read-only, a legal hold -- and refusing the open there would
// turn "this product cannot correct a policy" into "this product cannot
// restore", which is the one outcome worse than the one the correction
// exists to prevent.
//
// The pin on every manifest is what makes this safe to give up on: a
// repository whose policy this product could not neutralize still cannot
// expire a snapshot this product wrote.
func TestARepositoryWhosePolicyCannotBeWrittenStillOpens(t *testing.T) {
	ctx := context.Background()
	loc := localLocation(t)

	adapter := New()
	if err := adapter.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	opened, err := adapter.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	source := backupengine.Source{Host: "test-host", User: "test-user", Path: sourceTree(t)}
	if _, err := opened.Snapshot(ctx, backupengine.SnapshotRequest{Source: source}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Somebody's retention policy is back on this repository, so the next
	// open has a correction to attempt...
	setGlobalKeepLatest(t, opened.(*repository).rep, 10)
	if err := opened.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// ...and the storage will not accept it.
	makeReadOnly(t, loc.Root)

	reopened, err := adapter.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository against read-only storage: %v; a restore from a WORM bucket or a legal hold has to be possible", err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })

	snaps, err := reopened.ListSnapshots(ctx, source)
	if err != nil {
		t.Fatalf("ListSnapshots through the reopened repository: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("the reopened repository lists %d snapshots, want the 1 it holds", len(snaps))
	}

	// The control: the correction really did fail. If the write had
	// somehow succeeded, this test would be asserting nothing about a
	// best-effort path.
	defined, err := policy.GetDefinedPolicy(ctx, reopened.(*repository).rep, policy.GlobalPolicySourceInfo)
	if err != nil {
		t.Fatalf("GetDefinedPolicy: %v", err)
	}
	if engineRetentionIsOff(defined) {
		t.Fatalf("the read-only storage accepted the policy write, so this fixture never exercised the best-effort path")
	}
}

// TestNeutralizingRetentionKeepsTheRestOfADefinedPolicy is the
// less-than-obvious half of "this is a statement about retention alone".
//
// RetentionPolicy is not only the six counts. It also carries
// IgnoreIdenticalSnapshots, which decides whether a run that produced
// byte-identical content is recorded at all, and a correction that
// replaced the whole struct would silently switch an operator's setting
// off -- in the same write that claims to preserve everything else.
func TestNeutralizingRetentionKeepsTheRestOfADefinedPolicy(t *testing.T) {
	ctx := context.Background()
	rep, src := repositoryWithSnapshots(t, engineRetentionSnapshots)
	stripEnginePins(t, rep, src)

	// An operator's own global policy: a compression choice, an explicit
	// "do not record identical snapshots", and retention that expires
	// things.
	ignore := policy.OptionalBool(true)
	keep := policy.OptionalInt(2)
	defined := policy.Policy{
		CompressionPolicy: policy.CompressionPolicy{CompressorName: "zstd-fastest"},
		RetentionPolicy: policy.RetentionPolicy{
			KeepLatest:               &keep,
			IgnoreIdenticalSnapshots: &ignore,
		},
	}
	writePolicy(t, rep, policy.GlobalPolicySourceInfo, &defined)

	if err := disableEngineRetention(ctx, rep); err != nil {
		t.Fatalf("disableEngineRetention: %v", err)
	}

	got, err := policy.GetDefinedPolicy(ctx, rep, policy.GlobalPolicySourceInfo)
	if err != nil {
		t.Fatalf("GetDefinedPolicy: %v", err)
	}
	if got.CompressionPolicy.CompressorName != "zstd-fastest" {
		t.Errorf("the compression policy is now %q, want the operator's zstd-fastest", got.CompressionPolicy.CompressorName)
	}
	if got.RetentionPolicy.IgnoreIdenticalSnapshots == nil || !*got.RetentionPolicy.IgnoreIdenticalSnapshots {
		t.Errorf("ignore_identical_snapshots is %v, want the operator's explicit true preserved",
			got.RetentionPolicy.IgnoreIdenticalSnapshots)
	}
	if !engineRetentionIsOff(got) {
		t.Fatalf("the six counts were not neutralized: %+v", got.RetentionPolicy)
	}

	// And the behaviour the counts exist for: with keep-latest 2 defined
	// and eleven snapshots present, an un-neutralized repository expires
	// nine of them.
	expired, err := policy.ApplyRetentionPolicy(ctx, mustWriter(t, rep), src, false)
	if err != nil {
		t.Fatalf("ApplyRetentionPolicy: %v", err)
	}
	if len(expired) != 0 {
		t.Errorf("the engine would expire %d snapshots (%v) under the corrected policy", len(expired), expired)
	}
}

// setHostKeepLatest stores a HOST-level policy keeping only n snapshots,
// which is what somebody else's kopia install does to a shared repository.
func setHostKeepLatest(t *testing.T, rep repo.Repository, src snapshot.SourceInfo, n int) {
	t.Helper()

	keep := policy.OptionalInt(n)
	writePolicy(t, rep, snapshot.SourceInfo{Host: src.Host}, &policy.Policy{
		RetentionPolicy: policy.RetentionPolicy{KeepLatest: &keep},
	})
}

// setGlobalKeepLatest puts an ordinary expiring policy back on the global
// source, so that the next open has a correction to attempt.
func setGlobalKeepLatest(t *testing.T, rep repo.Repository, n int) {
	t.Helper()

	keep := policy.OptionalInt(n)
	writePolicy(t, rep, policy.GlobalPolicySourceInfo, &policy.Policy{
		RetentionPolicy: policy.RetentionPolicy{KeepLatest: &keep},
	})
}

func writePolicy(t *testing.T, rep repo.Repository, si snapshot.SourceInfo, pol *policy.Policy) {
	t.Helper()

	if err := repo.WriteSession(context.Background(), rep, repo.WriteSessionOptions{Purpose: "test:set-policy"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return policy.SetPolicy(ctx, w, si, pol)
		}); err != nil {
		t.Fatalf("SetPolicy(%v): %v", si, err)
	}
}

// stripEnginePins removes this adapter's pin from every manifest of one
// source, which is how a test asks what the STORED POLICY would do on its
// own.
func stripEnginePins(t *testing.T, rep repo.Repository, src snapshot.SourceInfo) {
	t.Helper()

	ctx := context.Background()

	manifests, err := snapshot.ListSnapshots(ctx, rep, src)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}

	if err := repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "test:unpin"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			for _, m := range manifests {
				m.UpdatePins(nil, []string{enginePin})
				if err := snapshot.UpdateSnapshot(ctx, w, m); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
		t.Fatalf("removing the adapter's pins: %v", err)
	}
}

// makeReadOnly takes write permission off a whole repository directory,
// which is the cheapest honest stand-in for WORM storage, an object lock
// or a read-only mount.
func makeReadOnly(t *testing.T, root string) {
	t.Helper()

	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		return os.Chmod(path, 0o400)
	}); err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// Directories last and innermost first: a directory made read-only
	// cannot have its children chmod'ed afterwards.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o500); err != nil {
			t.Fatalf("chmod %s: %v", dirs[i], err)
		}
	}

	// Given back on the way out so that the temporary directory can be
	// removed.
	t.Cleanup(func() {
		for _, dir := range dirs {
			_ = os.Chmod(dir, 0o700)
		}
	})
}
