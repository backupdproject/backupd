package kopia_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is EPIC K's verification ladder, proved rung by rung against a
// real repository.
//
// The claim under test is not "verification works". It is that the four
// rungs are four DIFFERENT claims: that a structural pass says nothing
// about content, that a sampled pass finds a missing pack blob it never
// read, that a full pass finds damage inside a blob that is still there,
// and that only a restore drill has put the restore path itself under
// test. A product that reported one of those as another would be making
// the promise EPIC K exists to stop being made, and every assertion here
// is about the difference rather than about the success.

// verifyFileCount and verifyFileSize shape the fixture tree.
//
// Twelve files is chosen so that a 25% sample is exactly three of them
// and cannot be confused with "some", and 64 KiB of incompressible bytes
// each is far above the size at which the engine inlines content into an
// object id: a tree of inlined files would have no pack blob to delete
// and the corruption rows below would silently test nothing.
const (
	verifyFileCount = 12
	verifyFileSize  = 64 << 10
)

// fullContentVerification is what every test written before the ladder
// existed was implicitly asking for: read every byte of the snapshot back.
//
// It is named rather than repeated so that those tests keep asserting the
// behaviour they were written about -- damage found by reading, a
// cancellation reported as a cancellation -- instead of quietly becoming
// tests of whichever rung a later edit happened to pass in.
var fullContentVerification = backupengine.VerifyRequest{Level: model.LevelContentFull}

// verifyFixture is a repository holding one snapshot of a tree of twelve
// files, plus everything a test needs to damage it and to check a restore
// against the original bytes.
type verifyFixture struct {
	repo     backupengine.Repository
	loc      backupengine.RepositoryLocation
	source   backupengine.Source
	snapshot backupengine.SnapshotInfo
	srcDir   string

	// files and bytes are what the SOURCE held, counted by the test, so
	// that a report claiming to have read everything is checked against
	// the tree rather than against the report's own idea of the total.
	files int64
	bytes int64
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()

	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "source")

	var total int64

	for i := range verifyFileCount {
		payload := make([]byte, verifyFileSize)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("generating payload %d: %v", i, err)
		}

		// Two levels, so the walk has directories to resolve as well as
		// files to read: a structural pass that never descends would
		// otherwise look identical to one that did.
		name := filepath.Join(srcDir, fmt.Sprintf("dir-%d", i%3), fmt.Sprintf("file-%02d.bin", i))
		mustWrite(t, name, payload)

		total += verifyFileSize
	}

	loc := localLocation(t, root, "production")
	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	src := backupengine.Source{Host: "nas-01", User: "backupd", Path: srcDir}

	snap, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{Source: src, Description: "the verification ladder"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	return &verifyFixture{
		repo:     rep,
		loc:      loc,
		source:   src,
		snapshot: snap,
		srcDir:   srcDir,
		files:    verifyFileCount,
		bytes:    total,
	}
}

// reopenDamaged closes the fixture's handle and opens a fresh one, which
// is the only way a test sees damage it has just done to the storage: an
// open repository holds index state in memory, and a verification that
// read it would be verifying this process's memory rather than the bytes
// on the disk.
func (f *verifyFixture) reopenDamaged(t *testing.T, damage func(repoDir string)) backupengine.Repository {
	t.Helper()

	ctx := context.Background()

	if err := f.repo.Close(ctx); err != nil {
		t.Fatalf("closing before damage: %v", err)
	}

	damage(repoDir(t, f.loc))

	rep, err := kopia.New().OpenRepository(ctx, f.loc)
	if err != nil {
		t.Fatalf("reopening the damaged repository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("closing the damaged repository: %v", err)
		}
	})

	return rep
}

// TestVerificationLevelsAreFourIndependentClaims is the acceptance
// criterion "all verification levels report independently", stated as
// what each rung may and may not claim.
//
// Every assertion here is a DIFFERENCE between rungs. A structural pass
// that reported bytes, or a sampled pass that reported all of them, would
// pass a test written as "the report is non-zero" and would be the exact
// lie the ladder exists to prevent.
func TestVerificationLevelsAreFourIndependentClaims(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newVerifyFixture(t)

	structural, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: model.LevelStructural})
	if err != nil {
		t.Fatalf("structural Verify: %v (%v)", err, structural.Errors)
	}

	if structural.Level != model.LevelStructural {
		t.Errorf("a structural verification reported level %q", structural.Level)
	}

	if structural.ObjectsVerified < f.files {
		t.Errorf("a structural verification resolved %d objects for a tree of %d files plus its directories",
			structural.ObjectsVerified, f.files)
	}

	if structural.FilesVerified != 0 || structural.BytesVerified != 0 {
		t.Errorf("a structural verification claims to have read %d file(s) and %d byte(s); it reads none, and reporting otherwise is how structural gets mistaken for a content check",
			structural.FilesVerified, structural.BytesVerified)
	}

	if structural.FilesRestored != 0 || structural.HashesMatched != 0 {
		t.Errorf("a structural verification claims to have restored %d file(s) and matched %d hash(es)",
			structural.FilesRestored, structural.HashesMatched)
	}

	sampled, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{
		Level:         model.LevelContentSample,
		SamplePercent: 25,
	})
	if err != nil {
		t.Fatalf("sampled Verify: %v (%v)", err, sampled.Errors)
	}

	if sampled.Level != model.LevelContentSample {
		t.Errorf("a sampled verification reported level %q", sampled.Level)
	}

	// A quarter of twelve files is three of them, exactly. This is the
	// assertion that keeps the sample a stride rather than a coin flip:
	// a probabilistic sampler passes "read at least one" and then, one
	// night in a hundred, reads none and records content_sample anyway.
	if want := int64(3); sampled.FilesVerified != want {
		t.Errorf("a 25%% sample of %d files read %d of them, want exactly %d", f.files, sampled.FilesVerified, want)
	}

	if sampled.BytesVerified == 0 || sampled.BytesVerified >= f.bytes {
		t.Errorf("a 25%% sample read %d bytes of a %d byte tree; a sample reads some and not all", sampled.BytesVerified, f.bytes)
	}

	if sampled.BlobsChecked == 0 {
		t.Error("a sampled verification checked no backing blobs; the repository-integrity half of this rung is what finds content that is referenced and not there")
	}

	full, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: model.LevelContentFull})
	if err != nil {
		t.Fatalf("full Verify: %v (%v)", err, full.Errors)
	}

	if full.Level != model.LevelContentFull {
		t.Errorf("a full verification reported level %q", full.Level)
	}

	if full.FilesVerified != f.files || full.BytesVerified != f.bytes {
		t.Errorf("a full verification read %d file(s) and %d byte(s); the tree holds %d and %d",
			full.FilesVerified, full.BytesVerified, f.files, f.bytes)
	}

	if full.FilesRestored != 0 {
		t.Errorf("a full verification restored %d file(s); reading content is not restoring it, and that distinction is the whole reason there is a rung above this one",
			full.FilesRestored)
	}

	target := filepath.Join(t.TempDir(), "drill")

	drill, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{
		Level:         model.LevelRestoreDrill,
		RestoreTarget: target,
	})
	if err != nil {
		t.Fatalf("restore drill Verify: %v (%v)", err, drill.Errors)
	}

	if drill.Level != model.LevelRestoreDrill {
		t.Errorf("a restore drill reported level %q", drill.Level)
	}

	if drill.FilesRestored != f.files || drill.HashesMatched != f.files {
		t.Errorf("a restore drill restored %d file(s) and matched %d hash(es) for a tree of %d files",
			drill.FilesRestored, drill.HashesMatched, f.files)
	}

	// The strongest evidence, checked the way an operator would: the
	// bytes on the disk after the drill are the bytes that were backed
	// up, compared against the ORIGINAL tree rather than against the
	// repository's own account of it.
	want := hashTree(t, f.srcDir)
	got := hashTree(t, target)

	if len(got) != len(want) {
		t.Fatalf("the drill left %d file(s) under %s, the source had %d", len(got), target, len(want))
	}

	for name, hash := range want {
		if got[name] != hash {
			t.Errorf("%s was restored with hash %s, want %s", name, got[name], hash)
		}
	}
}

// TestAStructuralVerificationIsNotAContentTest is the doctrine this whole
// ladder exists for, as a fixture rather than as a sentence in a document.
//
// A repository missing a pack blob still has an index that resolves every
// content identifier the snapshot names, so a structural pass over it
// PASSES -- correctly, because everything it checks is intact. Anything
// that treated that pass as evidence the backup is restorable would be
// advertising a restore point whose bytes are gone, which is the failure
// mode EPIC K names explicitly.
func TestAStructuralVerificationIsNotAContentTest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newVerifyFixture(t)

	damaged := f.reopenDamaged(t, func(dir string) { removeLargestBlob(t, dir) })

	structural, err := damaged.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: model.LevelStructural})
	if err != nil {
		t.Fatalf("a structural verification failed on a repository whose INDEX is intact: %v (%v); "+
			"if the vendor has started reading blobs during object resolution this test's argument has changed and needs rewriting, not deleting",
			err, structural.Errors)
	}

	if structural.Level != model.LevelStructural {
		t.Errorf("the passing structural verification reported level %q", structural.Level)
	}

	// And the rung above it, on the same damage, must not pass.
	sampled, err := damaged.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{
		Level:         model.LevelContentSample,
		SamplePercent: 1,
	})
	if err == nil {
		t.Fatalf("a sampled verification passed a repository with a missing pack blob (report: %+v)", sampled)
	}

	if len(sampled.Errors) == 0 {
		t.Errorf("the sampled verification failed with %v and no findings; the error says it failed, the findings say what is wrong", err)
	}

	// The point of checking blobs rather than only reading sampled files:
	// a 1% sample of twelve files reads one of them, and the damage is
	// still found.
	if sampled.FilesVerified > 1 {
		t.Errorf("a 1%% sample read %d files; this finding is supposed to come from the blob check, not from reading the whole tree",
			sampled.FilesVerified)
	}

	if !strings.Contains(strings.Join(sampled.Errors, "; "), "blob") {
		t.Errorf("the sampled findings are %v; none of them names the missing blob, so an operator cannot tell what is gone", sampled.Errors)
	}
}

// TestAFullVerificationFindsDamageInsideABlobThatIsStillThere is the
// other corruption fixture, and the one that says why content_full exists
// above content_sample.
//
// Flipping bytes inside a pack blob leaves every index entry resolvable
// and every backing blob present, so both cheaper rungs are entitled to
// pass. Only reading the bytes back finds it.
func TestAFullVerificationFindsDamageInsideABlobThatIsStillThere(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newVerifyFixture(t)

	damaged := f.reopenDamaged(t, func(dir string) { corruptLargestBlob(t, dir) })

	if report, err := damaged.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: model.LevelStructural}); err != nil {
		t.Fatalf("a structural verification failed on a repository whose structures are all intact: %v (%v)", err, report.Errors)
	}

	report, err := damaged.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: model.LevelContentFull})
	if err == nil {
		t.Fatalf("a full verification passed a repository with a corrupted pack blob (report: %+v)", report)
	}

	if errors.Is(err, context.Canceled) {
		t.Errorf("damage was reported as a cancellation: %v", err)
	}

	if len(report.Errors) == 0 {
		t.Errorf("the full verification failed with %v and no findings", err)
	}

	if report.BytesVerified >= f.bytes {
		t.Errorf("the full verification claims %d of %d bytes read back out of a repository whose content does not decrypt",
			report.BytesVerified, f.bytes)
	}

	// The drill cannot pass either: it restores through the same bytes.
	// This is the row that would otherwise let a damaged repository be
	// advertised as restore-tested.
	if drill, err := damaged.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{
		Level:         model.LevelRestoreDrill,
		RestoreTarget: filepath.Join(t.TempDir(), "drill"),
	}); err == nil {
		t.Fatalf("a restore drill passed against a corrupted repository (report: %+v)", drill)
	}
}

// TestARestoreDrillWithNowhereToRestoreToIsRefused is the refusal that
// keeps the top rung honest.
//
// The wrong behaviour is not an obscure one: an engine that quietly
// restored into a temporary directory of its own choosing would fill
// somebody's /tmp with a copy of their backup, and an engine that quietly
// downgraded to a content read would write "restore_drill" on a row where
// no restore happened.
func TestARestoreDrillWithNowhereToRestoreToIsRefused(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newVerifyFixture(t)

	report, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: model.LevelRestoreDrill})
	if !errors.Is(err, backupengine.ErrRestoreTargetRequired) {
		t.Fatalf("Verify(restore_drill) with no target = %v, want ErrRestoreTargetRequired", err)
	}

	if report.Level != "" {
		t.Errorf("the refused drill still claimed level %q; a verification that did not happen claims nothing", report.Level)
	}
}

// TestAVerificationLevelNobodyNamedIsRefused covers the empty and the
// misspelled level.
//
// Defaulting either way is wrong in a way that matters: reading silence as
// structural downgrades what a caller asked for, and reading it as a drill
// invents real I/O nobody budgeted for. The refusal is the same argument
// model.ParseVerificationLevel makes, at the port that acts on it.
func TestAVerificationLevelNobodyNamedIsRefused(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newVerifyFixture(t)

	for _, level := range []model.VerificationLevel{"", "thorough", "CONTENT_FULL"} {
		report, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: level})
		if err == nil {
			t.Errorf("Verify at level %q succeeded, reporting %q", level, report.Level)
		}

		if report.Level != "" {
			t.Errorf("Verify at level %q claimed to have achieved %q", level, report.Level)
		}
	}
}

// TestASampledVerificationReadsTheSameAmountEveryTime is what makes a
// sampled row comparable with the row before it.
//
// The vendor's own sampler is a coin flip per file, so two runs of the
// same configuration over the same tree read different amounts and,
// on a small tree, one of them reads nothing at all while still recording
// that content was sampled. A stride reads a stated fraction, every time,
// and a report that says three files really means three.
func TestASampledVerificationReadsTheSameAmountEveryTime(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newVerifyFixture(t)

	req := backupengine.VerifyRequest{Level: model.LevelContentSample, SamplePercent: 25}

	first, err := f.repo.Verify(ctx, f.snapshot.ID, req)
	if err != nil {
		t.Fatalf("first sampled Verify: %v", err)
	}

	second, err := f.repo.Verify(ctx, f.snapshot.ID, req)
	if err != nil {
		t.Fatalf("second sampled Verify: %v", err)
	}

	if first.FilesVerified != second.FilesVerified || first.BytesVerified != second.BytesVerified {
		t.Errorf("two identical sampled verifications read %d/%d and %d/%d files/bytes",
			first.FilesVerified, first.BytesVerified, second.FilesVerified, second.BytesVerified)
	}

	// The floor: a sample small enough to round to nothing still reads a
	// file, because a content_sample row that read no content is a claim
	// about content nobody checked.
	tiny, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{
		Level:         model.LevelContentSample,
		SamplePercent: 1,
	})
	if err != nil {
		t.Fatalf("1%% sampled Verify: %v", err)
	}

	if tiny.FilesVerified < 1 {
		t.Errorf("a 1%% sample of %d files read %d of them; the rung claims content was read", f.files, tiny.FilesVerified)
	}

	// An unstated percentage is the documented default rather than zero,
	// which would be a structural pass wearing the sampled name.
	def, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{Level: model.LevelContentSample})
	if err != nil {
		t.Fatalf("default sampled Verify: %v", err)
	}

	if def.FilesVerified < 1 || def.BytesVerified == 0 {
		t.Errorf("a sampled verification with no stated percentage read %d file(s) and %d byte(s)",
			def.FilesVerified, def.BytesVerified)
	}
}

// TestARestoreDrillLeavesItsEvidenceBehind is a small contract with a
// specific operator consequence: the drill does not tidy up.
//
// A drill that deleted its own output would destroy the only copy of what
// a FAILED drill produced, which is exactly the material somebody needs to
// find out why a restore does not reproduce a tree. The caller owns the
// directory and owns removing it.
func TestARestoreDrillLeavesItsEvidenceBehind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newVerifyFixture(t)
	target := filepath.Join(t.TempDir(), "drill")

	if _, err := f.repo.Verify(ctx, f.snapshot.ID, backupengine.VerifyRequest{
		Level:         model.LevelRestoreDrill,
		RestoreTarget: target,
	}); err != nil {
		t.Fatalf("restore drill: %v", err)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("reading the drill target: %v", err)
	}

	if len(entries) == 0 {
		t.Error("the drill target is empty after a drill that reported restoring files")
	}
}

// corruptLargestBlob flips the bytes in the middle of the biggest blob
// under dir without changing its length, which is the damage a bit-rotted
// disk or a partially-overwritten object produces: everything that lists
// or indexes the repository still agrees, and only a read finds it.
func corruptLargestBlob(t *testing.T, dir string) {
	t.Helper()

	size, path := largestFile(t, dir)

	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening %s to corrupt it: %v", path, err)
	}

	defer f.Close() //nolint:errcheck // the write below is checked, and this file is a test fixture.

	junk := make([]byte, 4096)
	for i := range junk {
		junk[i] = 0x5a
	}

	if _, err := f.WriteAt(junk, size/2); err != nil {
		t.Fatalf("corrupting %s: %v", path, err)
	}
}
