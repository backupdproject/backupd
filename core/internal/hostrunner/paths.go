package hostrunner

import (
	"errors"
	"fmt"
	"io/fs"
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
	// <prefix>/run/workflow-runner.sock. That directory holds the
	// socket and NOTHING else, because it is the one the engine's
	// container can write.
	SocketName = "workflow-runner.sock"

	// WorkflowDirName holds one directory per run, and one working
	// directory per step inside that, under the runner-private
	// WORKSPACE rather than under the runtime directory:
	// <prefix>/workspace/workflow/<run>/<step>/.
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
// It is derived from three absolute directories rather than from a single
// prefix, because they are separately configurable in the installer
// (--state-dir names one, the secrets area sits beside the configuration)
// and a type that recomputed them from a prefix would be a second opinion
// about paths the .env already states.
//
// # Why the socket and the working directories are not the same directory
//
// The engine reaches this runner by CONNECTING to a Unix socket, and
// connecting is a write: the directory holding that socket has to be
// writable by whatever the engine's container runs as, which is why
// container/compose.yaml binds it read-write. Anything else living in
// that directory is therefore engine-writable too.
//
// The per-step working directories and the runner's private copies of
// captured scripts are the things this process CREATES and later REMOVES
// RECURSIVELY. A compromised or merely authenticated engine that could
// place a symbolic link at <runtime>/workflow/<run id> would be choosing
// which host directory this runner writes an executable file into and
// which host directory it then deletes. So they live in a separate
// WorkspaceDir that no container mount names, and Validate refuses a
// layout that puts one inside the other.
type Layout struct {
	// RuntimeDir is <prefix>/run: the socket, and only the socket. This
	// is the ONLY directory mounted into the engine container, which is
	// why nothing else is allowed to live here.
	RuntimeDir string

	// WorkspaceDir is <prefix>/workspace: the per-run and per-step
	// working directories and the runner's private copy of each step's
	// script. It is mounted nowhere; only this process ever opens it.
	WorkspaceDir string

	// SecretsDir is <prefix>/secrets: where the installation-scoped
	// credential is. It is NOT mounted into the engine container as a
	// directory; the engine reads the one file it needs.
	SecretsDir string
}

// Validate reports the first way this layout is unusable.
//
// All three paths must be ABSOLUTE. This process creates directories
// under them, writes an executable file into one, and later removes trees
// from it, so a path whose meaning depends on the working directory is
// not something to be careful with, it is something to refuse.
//
// And neither the workspace nor the secrets area may sit inside the
// runtime directory, because the runtime directory is the engine's: a
// layout that nested them would hand the thing on the other end of the
// socket a say in the paths this process creates, chmods and removes.
func (l Layout) Validate() error {
	for _, d := range []struct {
		what string
		path string
	}{
		{"runtime directory", l.RuntimeDir},
		{"workspace directory", l.WorkspaceDir},
		{"secrets directory", l.SecretsDir},
	} {
		if d.path == "" {
			return fmt.Errorf("%w: no %s was configured", ErrLayout, d.what)
		}
		if !filepath.IsAbs(d.path) {
			return fmt.Errorf("%w: the %s %q is relative, and this process creates, writes and removes files under it, so it must be an absolute path", ErrLayout, d.what, d.path)
		}
	}
	for _, d := range []struct {
		what string
		path string
	}{
		{"workspace directory", l.WorkspaceDir},
		{"secrets directory", l.SecretsDir},
	} {
		if within(l.RuntimeDir, d.path) {
			return fmt.Errorf("%w: the %s %q is inside the runtime directory %q, which is the directory the engine's container can write. The working directories and the credential must be somewhere that mount does not reach", ErrLayout, d.what, d.path, l.RuntimeDir)
		}
	}
	return nil
}

// within reports whether child is dir itself or a path under it.
func within(dir, child string) bool {
	dir = filepath.Clean(dir)
	child = filepath.Clean(child)
	if dir == child {
		return true
	}
	return strings.HasPrefix(child, dir+string(filepath.Separator))
}

// SocketPath is the Unix-domain socket this runner listens on. There is
// no TCP equivalent anywhere in this package, deliberately: see
// Server.Listen.
func (l Layout) SocketPath() string { return filepath.Join(l.RuntimeDir, SocketName) }

// TokenPath is the installation-scoped credential file.
func (l Layout) TokenPath() string { return filepath.Join(l.SecretsDir, TokenName) }

// WorkflowRoot is the parent of every run's working directories, inside
// the runner-private workspace.
func (l Layout) WorkflowRoot() string { return filepath.Join(l.WorkspaceDir, WorkflowDirName) }

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
	return filepath.Join(run, stepID+ScriptFileSuffix), nil
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

// The workspace, and why every operation in it goes through a DIRECTORY
// DESCRIPTOR rather than a path.
//
// EnsureDir above is fine for the three directories the INSTALLER names:
// they are absolute, they are created before anything is listening, and
// an operator who can plant a symbolic link at <prefix>/run already owns
// the account this process runs as. It is not fine for anything derived
// from a request. os.MkdirAll walks a path and follows whatever it finds
// at each component, so a symbolic link at <workflow>/<run id> pointing
// at /etc turns "create this step's working directory" into "create a
// directory in /etc" and the later cleanup into "remove /etc/<step>".
// The final-component Lstat EnsureDir does catches the last hop and
// nothing before it.
//
// os.Root is the openat(2) form of the same walk: every component is
// resolved relative to an open descriptor, and a symbolic link that
// leaves the root is refused by the kernel-adjacent code rather than
// followed. This package goes one step further and refuses a symbolic
// link at ANY component, escaping or not, because nothing in this design
// has a reason to create one and a link that appeared under a
// runner-private directory is evidence rather than a layout to work
// with.

// ScriptFileSuffix names a step's script file beside its working
// directory.
const ScriptFileSuffix = ".script"

// openWorkflowRoot opens <workspace>/workflow as a rooted handle,
// creating the workspace and that directory if they are not there yet.
func (l Layout) openWorkflowRoot() (*os.Root, error) {
	if err := EnsureDir(l.WorkspaceDir); err != nil {
		return nil, err
	}
	base, err := os.OpenRoot(l.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("hostrunner: this runner cannot open its workspace %s: %w", l.WorkspaceDir, err)
	}
	defer base.Close()

	if err := ensureDirAt(base, WorkflowDirName); err != nil {
		return nil, err
	}
	root, err := base.OpenRoot(WorkflowDirName)
	if err != nil {
		return nil, fmt.Errorf("hostrunner: this runner cannot open %s inside its workspace %s: %w", WorkflowDirName, l.WorkspaceDir, err)
	}
	return root, nil
}

// ensureDirAt creates one directory INSIDE an open root and holds it to
// RuntimeDirMode, refusing a symbolic link at that name.
func ensureDirAt(root *os.Root, name string) error {
	if err := root.Mkdir(name, RuntimeDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("hostrunner: this runner cannot create %s inside %s: %w", name, root.Name(), err)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("hostrunner: this runner cannot inspect %s inside %s: %w", name, root.Name(), err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s inside %s is a symbolic link, and this process will not create, write or remove a hook's directory through one", ErrLayout, name, root.Name())
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s inside %s exists and is not a directory", ErrLayout, name, root.Name())
	}
	if info.Mode().Perm() != RuntimeDirMode {
		if err := root.Chmod(name, RuntimeDirMode); err != nil {
			return fmt.Errorf("hostrunner: %s inside %s is mode %#o rather than %#o and this runner cannot correct it: %w", name, root.Name(), info.Mode().Perm(), RuntimeDirMode, err)
		}
	}
	return nil
}

// openDirAt opens a subdirectory of an open root as a root of its own,
// having first proved it is a directory and not a symbolic link.
func openDirAt(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s inside %s is a symbolic link, and this process will not descend through one", ErrLayout, name, root.Name())
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s inside %s is not a directory", ErrLayout, name, root.Name())
	}
	return root.OpenRoot(name)
}

// prepareStep creates one step's working directory and writes the
// verified bytes to the runner's private copy beside it, through
// descriptors only, and returns the two absolute paths bash is given.
//
// A failure LEAVES what it created, deliberately. There are two ways to
// reach one: a full disk, and a script file that is already there. The
// second is either a duplicate execute -- which Server.claim refuses
// before this is called -- or the residue of a step whose termination
// could not be confirmed, which this runner keeps on purpose. Cleaning
// up on the way out would delete exactly that evidence.
func (l Layout) prepareStep(runID, stepID string, script []byte) (workDir, scriptPath string, err error) {
	workDir, err = l.StepWorkDir(runID, stepID)
	if err != nil {
		return "", "", err
	}
	scriptPath, err = l.StepScriptPath(runID, stepID)
	if err != nil {
		return "", "", err
	}

	root, err := l.openWorkflowRoot()
	if err != nil {
		return "", "", err
	}
	defer root.Close()

	if err := ensureDirAt(root, runID); err != nil {
		return "", "", err
	}
	run, err := openDirAt(root, runID)
	if err != nil {
		return "", "", fmt.Errorf("hostrunner: this runner cannot open the run directory %s: %w", runID, err)
	}
	defer run.Close()

	if err := ensureDirAt(run, stepID); err != nil {
		return "", "", err
	}
	if err := writeScriptAt(run, stepID+ScriptFileSuffix, script); err != nil {
		return "", "", err
	}
	return workDir, scriptPath, nil
}

// removeStep removes one step's script and working directory, and the
// run directory once it is empty, through the same descriptors.
//
// Best effort and silent, for cleanupStep's reasons. What it must not do
// is remove something it did not create, which is why the ids go through
// ValidID, the descent refuses a symbolic link, and the recursive removal
// is os.Root's, which cannot leave the workspace even if one appeared
// between the check and the call.
func (l Layout) removeStep(runID, stepID string) {
	if err := ValidID("run id", runID); err != nil {
		return
	}
	if err := ValidID("step id", stepID); err != nil {
		return
	}
	// OpenRoot rather than openWorkflowRoot: cleanup never creates.
	root, err := os.OpenRoot(l.WorkflowRoot())
	if err != nil {
		return
	}
	defer root.Close()

	run, err := openDirAt(root, runID)
	if err != nil {
		return
	}
	_ = run.Remove(stepID + ScriptFileSuffix)
	_ = run.RemoveAll(stepID)
	run.Close()

	// Remove, not RemoveAll: it succeeds only when this was the run's
	// last step, and leaves another step's directory alone.
	_ = root.Remove(runID)
}

// writeScriptAt puts the verified bytes where bash will read them.
//
// O_EXCL, so a file already at that path is a refusal rather than a
// truncate-and-overwrite: the same run and step executing twice at once
// is a bug worth seeing, and following an existing path would be
// following whatever is there. A symbolic link at that name is an
// existing path, so O_EXCL refuses it too -- and the open is relative to
// a descriptor for the step's own run directory, so no component before
// it can be followed anywhere either.
func writeScriptAt(dir *os.Root, name string, body []byte) error {
	f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, ScriptFileMode)
	if err != nil {
		return fmt.Errorf("hostrunner: this runner cannot write the step's script to %s in %s: %w", name, dir.Name(), err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = dir.Remove(name)
		return fmt.Errorf("hostrunner: this runner cannot write the step's script to %s in %s: %w", name, dir.Name(), err)
	}
	// The mode is set again through the descriptor because O_CREATE's
	// mode is masked by the process umask, and a umask of 0077 is not
	// the only one a service manager might hand this process.
	if err := f.Chmod(ScriptFileMode); err != nil {
		f.Close()
		_ = dir.Remove(name)
		return fmt.Errorf("hostrunner: this runner cannot set the mode of %s in %s: %w", name, dir.Name(), err)
	}
	if err := f.Close(); err != nil {
		_ = dir.Remove(name)
		return fmt.Errorf("hostrunner: this runner cannot close %s in %s: %w", name, dir.Name(), err)
	}
	return nil
}
