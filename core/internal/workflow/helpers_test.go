package workflow

import (
	"crypto/sha256"
	"syscall"
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
