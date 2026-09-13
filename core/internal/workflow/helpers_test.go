package workflow

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The two platform-and-crypto shims the tests need, kept apart from the
// tests themselves so that the suites read as assertions rather than as
// plumbing.

// syscallMkfifo creates a named pipe. It is used to put something that is
// not a regular file exactly where a script belongs, which is the one
// custody case a file-mode check cannot express: the contents of a fifo
// are supplied by whoever is on the other end of it, at read time.
func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// sha256Sum is here so a test can hash the bytes it read out of the spool
// and compare against what the plan recorded, without reaching into the
// production code's own call.
func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// custodyTempDir is t.TempDir for a directory this package's custody
// rules have to accept, and it exists because t.TempDir is not usable for
// one.
//
// The walk from a hook directory (or a script spool) up to / refuses any
// ancestor another account can write, sticky bit or not -- see
// checkDirectoryCustody for why the sticky exception a secret file's walk
// makes is wrong for a directory this daemon reads programs out of. On
// Linux, t.TempDir lands under /tmp, which is mode 1777, so every fixture
// built there would be refused for a reason that is about the test harness
// rather than about the fixture. This builds the tree under the package
// directory instead, whose ancestry is the checkout's: owner-writable and
// nothing else, on every platform this suite runs on.
//
// The name is hidden (a leading dot) so that a crash that outruns the
// cleanup leaves something the Go tool and the format checks both skip.
func custodyTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp(".", ".wf-custody-")
	if err != nil {
		t.Fatalf("creating a fixture directory: %v", err)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolving the fixture directory %s: %v", dir, err)
	}

	t.Cleanup(func() {
		os.RemoveAll(abs) //nolint:errcheck // a leftover fixture is inert
	})

	return abs
}
