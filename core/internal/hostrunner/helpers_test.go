package hostrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The harness every test here uses: a real socket, a real bash, a real
// process.
//
// Nothing in this file fakes the shell. A runner whose tests substituted
// a fake interpreter would be a runner whose central claims -- that
// BASH_ENV never reaches a hook, that no `set -e` is injected, that
// killing the process group kills what the hook started -- are asserted
// about a mock and true of nothing. bash is present on every platform
// this product supports and on every machine its tests run on, so its
// absence is a FAILURE here rather than a skip: a green suite that
// silently checked none of the above is worse than a red one.

func testBash(t *testing.T) Bash {
	t.Helper()
	bash, err := FindBash(context.Background(), "")
	if err != nil {
		t.Fatalf("this host has no bash, so none of this package's claims can be checked: %v", err)
	}
	return bash
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

func testExecutor(t *testing.T) *Executor {
	t.Helper()
	return &Executor{Layout: testLayout(t), Bash: testBash(t), Grace: 200 * time.Millisecond}
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
// client for it, plus the layout it is using.
func serveTestRunner(t *testing.T, mutate func(*Config)) (Client, Layout) {
	t.Helper()

	layout := testLayout(t)
	cfg := Config{
		Layout:   layout,
		Version:  "test-1.2.3",
		Token:    []byte(testToken),
		Bash:     testBash(t),
		Grace:    200 * time.Millisecond,
		EUID:     os.Geteuid(),
		Username: CurrentUsername(os.Geteuid()),
	}
	if cfg.EUID == 0 {
		t.Skip("this suite cannot run as root, because the runner refuses to: see RefuseRoot")
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

	return Client{SocketPath: layout.SocketPath(), Version: cfg.Version, Token: testToken}, layout
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
