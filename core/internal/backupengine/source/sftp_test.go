package source_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/backend"
	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/backupengine/source"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/transport/rclone"
	"github.com/retnd/retnd/core/tests/machines"
)

// An SFTP source, end to end, against a real SSH server in a container
// this test stands up for itself: real keys, a real host-key check, real
// pkg/sftp reads, this adapter, and a real Kopia repository.
//
// # It depends on no service that was already running
//
// The server is SIMULATED infrastructure, not ambient infrastructure.
// machines.Start builds a dedicated docker network and an atmoz/sftp
// container for this run, with freshly generated host and client keys
// and a run directory under core/tests/.run, and registers its single
// teardown (Source.finish) BEFORE anything can fail - so the container,
// the network and the directory go away on a failing test, a panicking
// test and a killed one alike, the last of those through
// tests/dockerlease's sweep. Nothing here reads a host, a port, a key or
// a credential from the environment, so there is no configuration under
// which this test silently starts talking to somebody's real server.
//
// # A skip is a developer convenience and never the CI path
//
// machines.Start FAILS, loudly and marked INFRA:, when docker is missing
// inside the gate (CI_LOCAL=1), and skips only on a laptop that has no
// daemon. That asymmetry is the contract: a skip in CI would delete the
// machine tier from a run that went on printing ok, which is #456.
//
// # Why BackupPaths and not Backup
//
// It is the finding rather than a shortcut. bundled/sftp.json declares
// bounded_listing false - rclone's sftp backend reads a directory
// through pkg/sftp's ReadDir, which returns the whole directory as one
// slice with no resumable cursor - so a walk of an SFTP source is
// refused by the capability gate before anything is dialed, and the
// first half of this test proves that the refusal fires against the real
// backend rather than only against a synthetic profile. What SFTP DOES
// declare is streaming_open, so its objects stream, and the second half
// proves that too.
func TestAnSFTPSourceStreamsThroughTheAdapter(t *testing.T) {
	fixture := machines.Start(t).Source(t)

	payload := patternBytes(4<<20, 0x5F7B)
	small := []byte("a second object, so the run is not one file wide\n")

	seed := map[string][]byte{
		"runs/2026/db.dump":   payload,
		"runs/2026/notes.txt": small,
	}

	for name, body := range seed {
		full := filepath.Join(fixture.UploadDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	adapter := rclone.New()
	src := fixture.TransportSource("sftp-source", "")

	repo, repoRoot, repoDir := realRepository(t)

	sink := source.RepositorySink{
		Repo:        repo,
		Source:      backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/sftp-source"},
		Ref:         testRef(t, "sftp-source"),
		Description: "sftp integration",
	}

	var (
		storedMu = make(chan struct{}, 1)
		stored   = map[string]string{}
	)

	storedMu <- struct{}{}

	a, err := source.New(source.Deps{
		Streamer:   adapter,
		Stater:     adapter,
		Enumerator: adapter,
	}, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 2,
		OnResult: func(r source.Result) {
			if !r.Verified() {
				return
			}
			<-storedMu
			stored[r.Path] = r.StoredID
			storedMu <- struct{}{}
		},
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	ctx := fixture.Context()

	// The walk is refused by the matrix, before anything is dialed.
	if _, err := a.Backup(ctx, source.Request{Source: src, Sink: sink}); !errors.Is(err, backend.ErrUnboundedListing) {
		t.Fatalf("walking an SFTP source returned %v; bundled/sftp.json declares bounded_listing false, so it must be refused", err)
	}

	// The objects stream.
	rep, err := a.BackupPaths(ctx, source.Request{Source: src, Sink: sink},
		[]string{"runs/2026/db.dump", "runs/2026/notes.txt"})
	if err != nil {
		t.Fatalf("BackupPaths over SFTP: %v", err)
	}

	if !rep.Complete() || rep.Stored != 2 {
		t.Fatalf("the SFTP run stored %d of 2 objects: %+v", rep.Stored, rep)
	}

	if rep.Backend != "sftp" {
		t.Errorf("the run was judged against the %q matrix, want sftp", rep.Backend)
	}

	if rep.Bytes != int64(len(payload)+len(small)) {
		t.Errorf("streamed %d bytes; the source holds %d", rep.Bytes, len(payload)+len(small))
	}

	for name, body := range seed {
		id := stored[name]
		if id == "" {
			t.Errorf("%s was not stored", name)

			continue
		}

		rc, err := repo.OpenSnapshotStream(context.Background(), backupengine.SnapshotID(id))
		if err != nil {
			t.Errorf("OpenSnapshotStream(%s): %v", name, err)

			continue
		}

		got, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			t.Errorf("reading %s back: %v", name, err)

			continue
		}

		if sha256Of(got) != sha256Of(body) {
			t.Errorf("%s came back as %d bytes; the SFTP source holds %d", name, len(got), len(body))
		}
	}

	// Nothing staged the object anywhere outside the repository, which
	// over SFTP is the claim that matters most: the obvious
	// implementation is an sftp GET to a temp file followed by a local
	// read of it.
	assertNothingStaged(t, repoRoot, repoDir)

	// And the SFTP source still holds everything it did.
	for name, body := range seed {
		got, err := os.ReadFile(filepath.Join(fixture.UploadDir, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("the backup removed %s from the SFTP source: %v", name, err)

			continue
		}
		if sha256Of(got) != sha256Of(body) {
			t.Errorf("the backup modified %s on the SFTP source", name)
		}
	}
}

// What a run over an SFTP source costs the server in SSH logins does not
// grow with the number of objects it backs up, and nothing is still
// connected when it returns.
//
// The measurement is the server's own: sshd records an "Accepted
// publickey" line per successful login, so what is counted here is what
// the source actually paid, not what this process believes it asked for.
//
// # Why "constant" rather than a number
//
// The property that matters is the slope. Every operation used to build
// its own rclone Fs, which on sftp is a TCP connect, a key exchange, a
// publickey authentication and a subsystem start; a run opens each object
// once and stats it again afterwards to check the read window, so the
// cost was two logins PER OBJECT and a production set of ten thousand
// small files cost twenty thousand logins - which a host with the
// connection cap #264 exists for sees as a fan-out, not as a backup. One
// session per run makes that cost a constant, and this test measures the
// same run at two sizes to say so in the only way a single count cannot.
//
// The absolute number is bounded as well as flat, because "constant" on
// its own would also be satisfied by a constant that is far too large.
// The bound is two: the session's own login, plus the one rclone's sftp
// backend takes when it builds the Fs and probes the root.
func TestAnSFTPRunsLoginCostDoesNotGrowWithTheObjectCount(t *testing.T) {
	fixture := machines.Start(t).Source(t)

	const (
		objects     = 24
		small       = 4
		loginsBound = 2
	)

	paths := make([]string, 0, objects)

	for i := range objects {
		name := fmt.Sprintf("set/object-%02d.bin", i)
		full := filepath.Join(fixture.UploadDir, filepath.FromSlash(name))

		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}

		if err := os.WriteFile(full, patternBytes(64<<10, uint32(i+1)), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}

		paths = append(paths, name)
	}

	adapter := rclone.New()
	src := fixture.TransportSource("sftp-session", "")
	repo, _, _ := realRepository(t)

	a, err := source.New(source.Deps{Streamer: adapter, Stater: adapter}, source.Options{
		Mode:   model.ModeLiveBestEffort,
		Preset: model.PresetConservative,
		// One worker, so a count is the run's shape rather than a race
		// between workers for connections out of the backend's pool.
		Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	// The seeding above wrote through the host mount rather than over
	// SSH, so the only logins counted are the ones a run makes.
	established := fixture.EstablishedConnections(t)

	backup := func(set string, want []string) int {
		t.Helper()

		before := fixture.AcceptedLogins(t)

		rep, err := a.BackupPaths(fixture.Context(), source.Request{
			Source: src,
			Sink: source.RepositorySink{
				Repo:   repo,
				Source: backupengine.Source{Host: "nas", User: "backupd", Path: "/sets/" + set},
				Ref:    testRef(t, set),
			},
		}, want)
		if err != nil {
			t.Fatalf("BackupPaths over SFTP: %v", err)
		}

		if !rep.Complete() || int(rep.Stored) != len(want) {
			t.Fatalf("the run stored %d of %d objects: %+v", rep.Stored, len(want), rep)
		}

		return fixture.AcceptedLogins(t) - before
	}

	few := backup("sftp-session-few", paths[:small])
	many := backup("sftp-session-many", paths)

	if few != many {
		t.Errorf(
			"%d objects cost %d SSH logins and %d objects cost %d: the cost still grows with the source, so something is still dialing per object",
			small, few, objects, many)
	}

	if many > loginsBound {
		t.Errorf(
			"a run over %d objects cost %d SSH logins, want at most %d (one session, plus rclone's own probe of the root)",
			objects, many, loginsBound)
	}

	// And the sessions are closed: nothing either run opened is still
	// established. A session that leaks is worse than the per-object
	// shape it replaced, because it leaks for the life of the daemon
	// rather than for the life of one operation.
	deadline := time.Now().Add(10 * time.Second)
	for {
		now := fixture.EstablishedConnections(t)
		if now <= established {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf(
				"the source still holds %d established connections against %d before the runs, so a run's session was never closed:\n%s",
				now, established, fixture.ConnectionTable(t))
		}

		time.Sleep(200 * time.Millisecond)
	}
}
