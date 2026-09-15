package source_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/backupengine/source"
	"github.com/retnd/retnd/core/internal/model"
)

// The two tag keys #781 counts distinct values of, written here as the
// literals both branches agreed rather than as a constant, so a rename on
// either side of the port is a test failure and not a repository that
// stops being able to say what it holds.
const (
	tagKeyBackupSet = "backupd.set"
	tagKeyDomain    = "backupd.domain"
)

// recordingRepository is a StreamingRepository that keeps the requests it
// was given. Everything else on the interface is unreachable from a sink.
type recordingRepository struct {
	backupengine.StreamingRepository

	mu       sync.Mutex
	requests []backupengine.StreamSnapshotRequest
}

func (r *recordingRepository) SnapshotStream(
	ctx context.Context,
	req backupengine.StreamSnapshotRequest,
) (backupengine.StreamSnapshotInfo, error) {
	rc, err := req.Stream.Open(ctx)
	if err != nil {
		return backupengine.StreamSnapshotInfo{}, err
	}

	n, err := io.Copy(io.Discard, rc)
	_ = rc.Close()

	if err != nil {
		return backupengine.StreamSnapshotInfo{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = append(r.requests, req)

	return backupengine.StreamSnapshotInfo{
		SnapshotInfo: backupengine.SnapshotInfo{ID: backupengine.SnapshotID("snap-1"), Bytes: n},
	}, nil
}

func (r *recordingRepository) taken() []backupengine.StreamSnapshotRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]backupengine.StreamSnapshotRequest(nil), r.requests...)
}

// Every snapshot the production sink writes says which backup set and
// which repository domain it belongs to.
//
// This is the co-tenancy signal #781's Stats counts distinct values of.
// Without it a repository holding one set of a hundred thousand files
// reports a hundred thousand Kopia sources - the per-object snapshots
// this interim sink writes - and an operator asking "is this domain
// shared" is answered with a number that has nothing to do with sharing.
func TestEverySnapshotCarriesTheBackupSetAndDomain(t *testing.T) {
	t.Parallel()

	repo := &recordingRepository{}
	sink := source.RepositorySink{
		Repo:   repo,
		Source: backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/photos"},
		Ref:    testRef(t, "photos"),
		Tags:   map[string]string{"schedule": "nightly"},
	}

	obj := source.Object{Path: "a/b.bin", Stream: newTestStream([]byte("bytes"))}

	if _, err := sink.Store(context.Background(), obj); err != nil {
		t.Fatalf("Store: %v", err)
	}

	taken := repo.taken()
	if len(taken) != 1 {
		t.Fatalf("the sink issued %d snapshot requests, want 1", len(taken))
	}

	tags := taken[0].Tags

	if got, want := tags[tagKeyBackupSet], "nas/photos"; got != want {
		t.Errorf("%s = %q, want %q; #781 counts distinct values of this tag as the repository's tenants", tagKeyBackupSet, got, want)
	}

	if got, want := tags[tagKeyDomain], "production"; got != want {
		t.Errorf("%s = %q, want %q", tagKeyDomain, got, want)
	}

	if got := tags["schedule"]; got != "nightly" {
		t.Errorf("the caller's own tag was dropped: schedule = %q", got)
	}
}

// A caller's tag map cannot rename the set a snapshot belongs to. The
// identity tags are the repository's answer about its own contents, and a
// configuration file that could overwrite them could make two sets look
// like one.
func TestTheIdentityTagsCannotBeOverwrittenByTheCaller(t *testing.T) {
	t.Parallel()

	repo := &recordingRepository{}
	sink := source.RepositorySink{
		Repo:   repo,
		Source: backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/photos"},
		Ref:    testRef(t, "photos"),
		Tags:   map[string]string{tagKeyBackupSet: "somebody/else", tagKeyDomain: "staging"},
	}

	if _, err := sink.Store(context.Background(), source.Object{Path: "x", Stream: newTestStream([]byte("x"))}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	tags := repo.taken()[0].Tags
	if tags[tagKeyBackupSet] != "nas/photos" || tags[tagKeyDomain] != "production" {
		t.Fatalf("a caller's tags overwrote the snapshot's identity: %v", tags)
	}
}

// A sink that cannot say which set a snapshot belongs to refuses to write
// one, rather than writing a snapshot no accounting can see.
//
// The failure being prevented is the silent one: an untagged snapshot is
// stored and restorable and invisible to every question asked by set, and
// it is invisible after the backup window, once the operator has already
// been told the run was fine.
func TestASinkThatCannotAttributeASnapshotRefusesToWriteIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		ref  model.RepositoryRef
	}{
		{name: "no reference at all"},
		{name: "no domain", ref: model.RepositoryRef{Set: testRef(t, "photos").Set}},
		{name: "no set", ref: model.RepositoryRef{Domain: testRef(t, "photos").Domain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := &recordingRepository{}
			sink := source.RepositorySink{
				Repo:   repo,
				Source: backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/photos"},
				Ref:    tc.ref,
			}

			_, err := sink.Store(context.Background(), source.Object{Path: "x", Stream: newTestStream([]byte("x"))})
			if err == nil {
				t.Fatal("the sink stored a snapshot it could not attribute to a backup set")
			}

			if !strings.Contains(err.Error(), "x") {
				t.Errorf("the refusal does not name the object: %v", err)
			}

			if n := len(repo.taken()); n != 0 {
				t.Fatalf("the repository was asked for %d snapshots after the refusal", n)
			}
		})
	}
}

// testStream is bytes in memory as a backupengine.StreamSource.
type testStream struct {
	data []byte
}

func newTestStream(data []byte) backupengine.StreamSource { return &testStream{data: data} }

func (s *testStream) ModTime() time.Time { return time.Time{} }

func (s *testStream) Open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
