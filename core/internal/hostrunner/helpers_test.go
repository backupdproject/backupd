package hostrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The harness every test here uses: a real socket, a real capability
// preflight, a real process per step -- and a stand-in docker client,
// because a unit package may not need a daemon.
//
// fakedocker_test.go states exactly what that stand-in implements and
// what it deliberately cannot prove, and core/tests/containerhooks is
// where the claims it cannot prove are asserted against a real daemon.
// Nothing here fakes the SHELL: the stand-in execs a real bash with the
// arguments the runner chose, so "no set -e is injected", "BASH_ENV never
// reaches a hook" and "values arrive byte for byte" are measured against
// a real interpreter, as they were before #865.

// fakeCapability proves a container capability against the stand-in
// client and returns it with the directory holding that client's call log
// and container records.
//
// It goes through ProveContainerCapability rather than building a
// Container literal, so every test in this package runs behind the same
// preflight a deployment does -- including the probe, which the stand-in
// answers by really running the probe script under a real bash.
func fakeCapability(t *testing.T, cfg fakeDocker) (Container, string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("this suite cannot run as root: the runner refuses uid 0, and so does the hook user check")
	}

	state := fakeDockerState(t)
	docker := writeFakeDockerIn(t, state, cfg)

	container, err := ProveContainerCapability(context.Background(), ContainerConfig{
		Docker: docker,
		Image:  "stand-in/hook:test",
		// The interpreter the stand-in is told to exec. In a deployment
		// this is a path inside the image; here it has to be a path
		// that exists, because the stand-in really runs it.
		Bash: hostBashForFake(t),
		User: fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
	})
	if err != nil {
		t.Fatalf("proving a container capability against the stand-in client: %v", err)
	}
	return container, state
}

// testLayout is a runtime layout under a SHORT temporary directory.
//
// Short, and therefore not t.TempDir(): a Unix socket address is a fixed
// 104-byte field on Darwin, and Go's per-test temporary directory is
// named after the test, so a suite whose test names are sentences binds
// nothing at all. /tmp keeps every path well inside the limit and is the
// same shape a real installation's <prefix>/run has.
func testLayout(t *testing.T) Layout {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "bdhr")
	if err != nil {
		t.Fatalf("preparing a short temporary root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	layout := Layout{
		RuntimeDir:   filepath.Join(root, "run"),
		WorkspaceDir: filepath.Join(root, "workspace"),
		SecretsDir:   filepath.Join(root, "secrets"),
	}
	for _, dir := range []string{layout.RuntimeDir, layout.WorkspaceDir, layout.SecretsDir} {
		if err := EnsureDir(dir); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}
	return layout
}

// testExecutor is an executor whose containers are the stand-in's, with
// the state directory so a test can read what docker was asked to do.
func testExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	container, state := fakeCapability(t, fakeDocker{})
	return &Executor{
		Layout:    testLayout(t),
		Container: container,
		Grace:     200 * time.Millisecond,
	}, state
}

// scriptRequest builds a well-formed execute request for body, with the
// size and hash a truthful engine would send.
func scriptRequest(runID, stepID string, body string) Request {
	sum := sha256.Sum256([]byte(body))
	return Request{
		Op:           OpExecute,
		RunID:        runID,
		StepID:       stepID,
		Script:       []byte(body),
		ScriptSize:   int64(len(body)),
		ScriptSHA256: hex.EncodeToString(sum[:]),
		TimeoutMS:    30_000,
	}
}

// collector is a Sink that keeps every chunk, for the tests that assert
// on stream identity and ordering.
type collector struct {
	chunks []Chunk
}

func (c *collector) Chunk(chunk Chunk) error {
	c.chunks = append(c.chunks, chunk)
	return nil
}

func (c *collector) text(stream StreamID) string {
	var b strings.Builder
	for _, chunk := range c.chunks {
		if chunk.Stream == stream {
			b.Write(chunk.Data)
		}
	}
	return b.String()
}

// testToken is a credential of the shape the installer writes.
const testToken = "0123456789abcdef0123456789abcdef"

// serveTestRunner starts a real server on a real socket and returns a
// client for it, the layout it is using, and the stand-in client's state
// directory.
func serveTestRunner(t *testing.T, mutate func(*Config)) (Client, Layout, string) {
	t.Helper()

	layout := testLayout(t)
	container, state := fakeCapability(t, fakeDocker{})
	cfg := Config{
		Layout:    layout,
		Version:   "test-1.2.3",
		Token:     []byte(testToken),
		Container: container,
		Grace:     200 * time.Millisecond,
		EUID:      os.Geteuid(),
		Username:  CurrentUsername(os.Geteuid()),
	}
	if mutate != nil {
		mutate(&cfg)
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("preparing the runner: %v", err)
	}
	if err := server.Listen(); err != nil {
		t.Fatalf("listening: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = server.Close()
	})

	return Client{SocketPath: layout.SocketPath(), Version: cfg.Version, Token: testToken}, layout, state
}

// rawConn is a connection speaking the protocol by hand, for the tests
// that have to misbehave: a wrong token, a wrong version, a frame with a
// field that does not exist, an engine that simply vanishes.
func rawConn(t *testing.T, socket string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the runner: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func hello(t *testing.T, conn net.Conn, h Hello) Message {
	t.Helper()
	if err := WriteMessage(conn, Message{Kind: KindHello, Hello: &h}); err != nil {
		t.Fatalf("writing hello: %v", err)
	}
	msg, err := ReadMessage(conn)
	if err != nil {
		t.Fatalf("reading the answer to hello: %v", err)
	}
	return msg
}

// waitForFile polls for a path a hook is expected to create.
func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the hook never created %s within %s, so the test cannot tell what it is asserting about", path, within)
}
