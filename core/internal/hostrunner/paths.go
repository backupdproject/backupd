package hostrunner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SocketName, WorkflowDirName and TokenName are the three names this
// package owns inside an installation. They are constants rather than
// configuration because the installer, the compose mount, the engine's
// client and this server all have to agree on them, and four places
// spelling the same string is how three of them end up spelling it
// differently.
const (
	// SocketName is the listening socket inside the runtime directory:
	// <prefix>/run/workflow-runner.sock.
	SocketName = "workflow-runner.sock"

	// WorkflowDirName holds one directory per run, and one working
	// directory per step inside that: <prefix>/run/workflow/<run>/<step>/.
	WorkflowDirName = "workflow"

	// TokenName is the installation-scoped credential, in backupd's
	// secrets area beside the SSH key and the known_hosts file.
	TokenName = "workflow-runner.token"
)

// RuntimeDirMode is the mode of every directory this package creates.
//
// 0700 rather than 0750. The group case is the one that looks harmless
// and is not: the deployments this product targets routinely put the
// service account in a shared group (`users` on a Synology, `docker` on a
// hobby host), so a group-readable working directory is a
// world-readable-in-practice directory holding whatever a hook wrote into
// BACKUPD_WORK_DIR -- which, for a database quiesce hook, is a dump.
const RuntimeDirMode os.FileMode = 0o700

// ScriptFileMode is the mode of the runner-private copy of a step's
// captured bytes.
//
// 0500: readable and executable by the owner, writable by nobody at all,
// including the owner. bash is handed the path, so it needs read; nothing
// ever needs to write it, and a hook running as the same account is the
// thing most likely to try -- a script that rewrote its own file
// mid-execution would change what bash reads next, since bash reads a
// script incrementally rather than all at once.
const ScriptFileMode os.FileMode = 0o500

// TokenFileMode is the mode of the credential file. 0600, and this
// package refuses to read one that is not.
const TokenFileMode os.FileMode = 0o600

// ErrLayout is a refusal about the runtime layout itself: a relative
// prefix, an id that is not a path component, a credential file with the
// wrong mode.
var ErrLayout = errors.New("hostrunner: this runner runtime layout cannot be used")

// Layout is where one installation's runner keeps everything.
//
// It is derived from two absolute directories rather than from a single
// prefix, because the two are separately configurable in the installer
// (--state-dir names one, the secrets area sits beside the configuration)
// and a type that recomputed them from a prefix would be a second opinion
// about paths the .env already states.
type Layout struct {
	// RuntimeDir is <prefix>/run: the socket, and the per-run working
	// directories. This is the ONLY directory mounted into the engine
	// container, which is why nothing else is allowed to live here.
	RuntimeDir string

	// SecretsDir is <prefix>/secrets: where the installation-scoped
	// credential is. It is NOT mounted into the engine container as a
	// directory; the engine reads the one file it needs.
	SecretsDir string
}

// Validate reports the first way this layout is unusable.
//
// Both paths must be ABSOLUTE. This process creates directories under
// them, writes an executable file into one, and later removes trees from
// it, so a path whose meaning depends on the working directory is not
// something to be careful with, it is something to refuse.
func (l Layout) Validate() error {
	for _, d := range []struct {
		what string
		path string
	}{
		{"runtime directory", l.RuntimeDir},
		{"secrets directory", l.SecretsDir},
	} {
		if d.path == "" {
			return fmt.Errorf("%w: no %s was configured", ErrLayout, d.what)
		}
		if !filepath.IsAbs(d.path) {
			return fmt.Errorf("%w: the %s %q is relative, and this process creates, writes and removes files under it, so it must be an absolute path", ErrLayout, d.what, d.path)
		}
	}
	return nil
}

// SocketPath is the Unix-domain socket this runner listens on. There is
// no TCP equivalent anywhere in this package, deliberately: see
// Server.Listen.
func (l Layout) SocketPath() string { return filepath.Join(l.RuntimeDir, SocketName) }

// TokenPath is the installation-scoped credential file.
func (l Layout) TokenPath() string { return filepath.Join(l.SecretsDir, TokenName) }

// WorkflowRoot is the parent of every run's working directories.
func (l Layout) WorkflowRoot() string { return filepath.Join(l.RuntimeDir, WorkflowDirName) }

// RunDir, StepWorkDir and StepScriptPath are one run's and one step's
// paths, and every one of them goes through ValidID first: these strings
// arrive over a socket, and a "run id" of "../../../etc/cron.d" would
// otherwise be a directory this process creates, chmods and later removes
// recursively.
func (l Layout) RunDir(runID string) (string, error) {
	if err := ValidID("run id", runID); err != nil {
		return "", err
	}
	return filepath.Join(l.WorkflowRoot(), runID), nil
}

// StepWorkDir is the private per-step working directory, exposed to the
// hook as BACKUPD_WORK_DIR.
func (l Layout) StepWorkDir(runID, stepID string) (string, error) {
	run, err := l.RunDir(runID)
	if err != nil {
		return "", err
	}
	if err := ValidID("step id", stepID); err != nil {
		return "", err
	}
	return filepath.Join(run, stepID), nil
}

// StepScriptPath is where the captured bytes are written for bash to
// read.
//
// It is a SIBLING of the working directory rather than a file inside it,
// and that placement is the point: BACKUPD_WORK_DIR is the hook's, to
// write whatever it likes into, and a script living inside the directory
// its own hook is rummaging around in is a script the hook can replace
// while bash is still reading it.
func (l Layout) StepScriptPath(runID, stepID string) (string, error) {
	run, err := l.RunDir(runID)
	if err != nil {
		return "", err
	}
	if err := ValidID("step id", stepID); err != nil {
		return "", err
	}
	return filepath.Join(run, stepID+".script"), nil
}

// MaxIDLength bounds a run or step id. internal/workflow derives step ids
// from an order, a scope, a phase and a script basename, so a real one is
// tens of bytes; this is a bound rather than an expectation.
const MaxIDLength = 128

// ValidID is the one rule for every identifier that becomes a path
// component here.
//
// It is deliberately stricter than "contains no separator". The wire is
// not a trusted caller -- it is whatever connected to the socket -- so the
// question is not "could this be a traversal" but "is this one of the
// small set of shapes internal/workflow actually mints". Anything else is
// refused, including the empty string, "." and "..", a leading dash (which
// argv-adjacent code elsewhere would read as a flag), and any byte outside
// the alphabet below.
func ValidID(what, id string) error {
	if id == "" {
		return fmt.Errorf("%w: an empty %s cannot name a directory", ErrLayout, what)
	}
	if len(id) > MaxIDLength {
		return fmt.Errorf("%w: the %s is %d bytes, past the %d-byte bound", ErrLayout, what, len(id), MaxIDLength)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("%w: %q is not a %s, it is a directory reference", ErrLayout, id, what)
	}
	if strings.HasPrefix(id, "-") {
		return fmt.Errorf("%w: the %s %q starts with a dash, which reads as a flag wherever it is rendered", ErrLayout, what, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%w: the %s %q contains %q, and only letters, digits, dot, dash and underscore may become a path component under the runtime directory", ErrLayout, what, id, string(r))
		}
	}
	return nil
}

// EnsureDir creates one directory with RuntimeDirMode and holds an
// existing one to it.
//
// The chmod on an existing directory is not tidiness. A runtime directory
// that was created by an older installer, restored from a backup, or made
// by a `docker compose up` running as root can be group- or
// world-writable, and this process is about to put a socket and a
// hook's working directory inside it. Correcting it is cheap and is the
// only way "restrictive permissions" is a property rather than a hope.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, RuntimeDirMode); err != nil {
		return fmt.Errorf("hostrunner: this runner cannot create the directory %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("hostrunner: this runner cannot inspect the directory %s it just created: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symbolic link, and this process will not create a hook's working directory through one", ErrLayout, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s exists and is not a directory", ErrLayout, path)
	}
	if info.Mode().Perm() != RuntimeDirMode {
		if err := os.Chmod(path, RuntimeDirMode); err != nil {
			return fmt.Errorf("hostrunner: %s is mode %#o rather than %#o and this runner cannot correct it: %w", path, info.Mode().Perm(), RuntimeDirMode, err)
		}
	}
	return nil
}
