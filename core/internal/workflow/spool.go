package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The spool's own file: creating the directory a run's scripts are captured
// into, and opening them again afterwards.
//
// # Why this is not MkdirAll and os.OpenFile
//
// The spool is the run's ONLY authority over what it executes (plan.go's
// preamble), so every guarantee this feature makes reduces to "nobody but
// this process could have put those bytes there". MkdirAll cannot support
// that claim, for two separate reasons:
//
//   - it FOLLOWS symbolic links in every component. A pre-existing link at
//     <state dir>/workflow-runs, or at any component above it, silently
//     relocates the whole spool somewhere another account chose -- and the
//     0700 this code then applies protects the attacker's directory rather
//     than the run;
//   - it accepts a path that already exists, so a run id that has been
//     seen before writes into the earlier run's directory.
//
// So every component is opened with O_NOFOLLOW from the descriptor of its
// parent (openat), which is the only way to make "this component is not a
// link, and it is the component I checked" one operation rather than two.
// Each descriptor is then custody-checked before it is used as the parent
// of the next, so a group-writable directory anywhere on the way to the
// spool is a refusal rather than a place somebody can substitute a
// subtree.
//
// # Why the ancestry is checked at all, given the state directory is ours
//
// Because it being ours is exactly the claim under test. The state
// directory's path comes from a configuration file an operator edits, the
// directories above it are whatever the deployment made them, and the
// spool holds the scripts this daemon executes as root. discover.go makes
// the same argument for the workflow tree; this is the same rule applied
// to the copy, which is the half that actually runs.

// ErrSpool is a refusal about a spooled script: it is not where the plan
// says it is, it is not contained in the run's own spool, or its bytes are
// not the bytes the plan hashed.
//
// It is distinct from ErrCustody (somebody else can write this) and
// ErrPlan (this request cannot be built) because it is the refusal that
// fires when a plan and a disk disagree, which is the case a recovery pass
// has to tell apart from both.
var ErrSpool = errors.New("workflow: this spooled script cannot be used")

// spoolDirs is one run's spool, held open.
//
// The descriptors are kept rather than the paths re-opened, because a path
// re-opened is a path that can have changed in between. They are also what
// makes the fsyncs at the end possible without a second walk.
type spoolDirs struct {
	root    *os.File
	run     *os.File
	scripts *os.File

	// createdRunPath is the run directory THIS call made, and "" when it
	// made none. It is what makes cleaning up on a refusal safe: a
	// directory that was already there belongs to another run, and
	// another run's spool is what a recovery pass is going to read.
	//
	// It is a path rather than a bool because the clean-up has to work
	// after the descriptors are closed, and on the refusal path where the
	// directory was created and could not then be opened.
	createdRunPath string
}

// createSpool prepares <spoolRoot>/<runID>/scripts and returns it open.
//
// The run directory is created with mkdirat, which fails with EEXIST
// rather than succeeding on a directory that is already there: a second
// plan for one run id is refused, and the first run's captured scripts are
// left untouched. os.MkdirAll's tolerance is the wrong behaviour here in
// the most literal way -- it is how a re-used run id would come to execute
// a mixture of two plans.
func createSpool(spoolRoot, runID string) (*spoolDirs, error) {
	root, err := openTrustedDir(spoolRoot, true)
	if err != nil {
		return nil, err
	}

	// The spool root itself is held at 0700 whatever the umask and
	// whatever it was before. Its ancestors are NOT touched: they belong
	// to the deployment, and a product that quietly chmods the directory
	// its state database lives in is one nobody can predict.
	if err := root.Chmod(0o700); err != nil {
		root.Close() //nolint:errcheck // the chmod already failed

		return nil, fmt.Errorf("%w: protecting the script spool root %s: %w", ErrPlan, root.Name(), err)
	}

	dirs := &spoolDirs{root: root}

	if err := unix.Mkdirat(int(root.Fd()), runID, 0o700); err != nil {
		defer dirs.close()
		// close, not removeAndClose: createdRunPath is still empty, and
		// the directory this refusal is ABOUT belongs to another run.

		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf(
				"%w: run %q already has a spool at %s. A run's spool is created once and is the only authority over what that run executes, so a second plan for the same id is refused rather than written over the first -- the directory that is there may be what a recovery of the earlier run is about to read",
				ErrPlan, runID, filepath.Join(root.Name(), runID))
		}

		return nil, fmt.Errorf("%w: creating the script spool at %s: %w", ErrPlan, filepath.Join(root.Name(), runID), err)
	}
	dirs.createdRunPath = filepath.Join(root.Name(), runID)

	run, err := openTrustedChild(root, runID)
	if err != nil {
		dirs.removeAndClose()

		return nil, err
	}
	dirs.run = run

	if err := unix.Mkdirat(int(run.Fd()), scriptsDirName, 0o700); err != nil {
		defer dirs.removeAndClose()

		return nil, fmt.Errorf("%w: creating the script spool at %s: %w", ErrPlan, filepath.Join(run.Name(), scriptsDirName), err)
	}

	scripts, err := openTrustedChild(run, scriptsDirName)
	if err != nil {
		dirs.removeAndClose()

		return nil, err
	}
	dirs.scripts = scripts

	// Explicit modes, because MkdirAt applies the process umask: 0700 &
	// ~umask can only be narrower, and a spool directory this process
	// cannot traverse is a run that cannot execute. A deployment running
	// with umask 077 would otherwise get 0600 here.
	for _, d := range []*os.File{dirs.run, dirs.scripts} {
		if err := d.Chmod(0o700); err != nil {
			dirs.removeAndClose()

			return nil, fmt.Errorf("%w: protecting the script spool at %s: %w", ErrPlan, d.Name(), err)
		}
	}

	return dirs, nil
}

// scriptsDirName is the subdirectory of a run's spool that holds the
// captured scripts, named in one place because the recovery path has to
// derive the same value from a journal row.
const scriptsDirName = "scripts"

// close releases the descriptors. It does not remove anything, and it is
// idempotent: Snapshot defers it AND calls removeAndClose on its refusal
// paths, so the second call has to be a no-op rather than a close of a
// descriptor number something else has since been handed.
func (s *spoolDirs) close() {
	for _, d := range []**os.File{&s.scripts, &s.run, &s.root} {
		if *d == nil {
			continue
		}

		(*d).Close() //nolint:errcheck // read-only descriptors
		*d = nil
	}
}

// removeAndClose is close plus removing the run directory THIS call
// created, which is what a refused snapshot leaves behind: nothing.
//
// It removes the run directory and never the spool root, and only when
// created is set. A refusal that deleted a directory it found rather than
// made would be a refusal that destroys another run's evidence.
func (s *spoolDirs) removeAndClose() {
	s.close()

	if s.createdRunPath != "" {
		os.RemoveAll(s.createdRunPath) //nolint:errcheck // the caller's refusal is the report
	}
}

// sync flushes the spool's directory entries, innermost first.
//
// This is the step that makes "durably committed before any hook may
// execute" true of the DIRECTORY as well as of the files in it. Each
// script is fsynced as it is written (writeScript), but a crash before the
// directories are synced can leave a spool whose files exist and whose
// names do not -- which a recovery pass reads as a plan with missing
// scripts, the one state it cannot tell apart from tampering.
func (s *spoolDirs) sync() error {
	for _, d := range []*os.File{s.scripts, s.run, s.root} {
		if err := syncDir(d); err != nil {
			return fmt.Errorf("%w: flushing the script spool directory %s: %w", ErrPlan, d.Name(), err)
		}
	}

	return nil
}

// syncDir is the directory fsync, as a variable for exactly one reason:
// the test that asserts the spool is flushed before Snapshot returns has
// no other way to observe it. A real fsync cannot be distinguished from a
// no-op without pulling the power, which internal/lifecycle's
// TestFsyncFileAndFsyncDir says about its own copy of this primitive.
var syncDir = func(d *os.File) error { return d.Sync() }

// writeScript captures one script into the spool and returns its path.
//
// The file is created through the scripts directory's own descriptor, so
// the name cannot be redirected by anything that happens to the path in
// between, and with O_EXCL, so a name that is already there is a refusal:
// two steps sharing one spool file would mean one of them executes the
// other's bytes.
//
// Not executable, deliberately. Execution runs these through an
// interpreter with the script as an argument rather than relying on the
// file's exec bit, so a file under the state directory that is not
// executable is one fewer thing a mistake elsewhere can turn into a
// running program.
func (s *spoolDirs) writeScript(name string, body []byte) (string, error) {
	path := filepath.Join(s.scripts.Name(), name)

	fd, err := unix.Openat(int(s.scripts.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", fmt.Errorf("%w: writing the captured script to %s: %w", ErrPlan, path, err)
	}

	f := os.NewFile(uintptr(fd), path)

	if _, err := f.Write(body); err != nil {
		f.Close() //nolint:errcheck // the write already failed

		return "", fmt.Errorf("%w: writing the captured script to %s: %w", ErrPlan, path, err)
	}

	// Explicit, for the umask reason above: a spool file this process
	// cannot read back is a run that cannot execute.
	if err := f.Chmod(0o600); err != nil {
		f.Close() //nolint:errcheck // the chmod already failed

		return "", fmt.Errorf("%w: protecting the captured script at %s: %w", ErrPlan, path, err)
	}

	// Sync before Close, and check Close: this copy is the run's only
	// authority over what it executes, so "durably committed before any
	// hook may execute" has to include the bytes actually reaching the
	// disk rather than a page cache that a crash discards.
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck // the sync already failed

		return "", fmt.Errorf("%w: flushing the captured script at %s: %w", ErrPlan, path, err)
	}

	if err := f.Close(); err != nil {
		return "", fmt.Errorf("%w: closing the captured script at %s: %w", ErrPlan, path, err)
	}

	return path, nil
}

// openTrustedDir opens an absolute path one component at a time, refusing
// a symbolic link, a foreign owner or a group- or other-writable directory
// anywhere along it.
//
// createLeaf creates the FINAL component when it is missing, which is what
// the spool root needs on a deployment's first workflow run. No other
// component is created: an intermediate directory that does not exist
// means the path is not the one the deployment thinks it is, and creating
// it would be this product inventing a location under the state directory.
func openTrustedDir(path string, createLeaf bool) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf(
			"%w: the script spool root %q is not an absolute path. This process creates directories under it and later executes what it finds there",
			ErrPlan, path)
	}

	clean := filepath.Clean(path)

	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: the filesystem root cannot be opened: %w", ErrPlan, err)
	}

	dir := os.NewFile(uintptr(fd), "/")

	components := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if clean == "/" {
		components = nil
	}

	for i, name := range components {
		child, err := openTrustedComponent(dir, name, createLeaf && i == len(components)-1)
		dir.Close() //nolint:errcheck // read-only descriptor

		if err != nil {
			return nil, err
		}

		dir = child
	}

	return dir, nil
}

// openTrustedChild is openTrustedComponent for a directory this process
// has just created, named separately because the caller's intent (walk vs
// descend) is worth reading at the call site.
func openTrustedChild(parent *os.File, name string) (*os.File, error) {
	return openTrustedComponent(parent, name, false)
}

// openTrustedComponent opens one directory entry through its parent's
// descriptor and checks what it got.
//
// O_NOFOLLOW is the whole point: a component that is a symbolic link fails
// with ELOOP rather than being followed, so a path whose meaning another
// account chose is a refusal rather than a spool in somebody else's
// directory. ENOTDIR distinguishes a file left where a directory belongs,
// which is a different operator situation and gets its own sentence.
func openTrustedComponent(parent *os.File, name string, create bool) (*os.File, error) {
	path := filepath.Join(parent.Name(), name)

	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)

	if errors.Is(err, os.ErrNotExist) && create {
		if mkErr := unix.Mkdirat(int(parent.Fd()), name, 0o700); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
			return nil, fmt.Errorf("%w: creating %s: %w", ErrPlan, path, mkErr)
		}

		fd, err = unix.Openat(int(parent.Fd()), name,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}

	// ELOOP and ENOTDIR are both what O_NOFOLLOW|O_DIRECTORY produces for
	// a symbolic link, and WHICH one depends on the kernel: Linux says
	// ELOOP, and the BSDs (macOS included) say ENOTDIR when the link's
	// target is a directory. Neither is a sentence an operator can act
	// on, so the entry is lstat'ed -- through the parent's descriptor, so
	// it is still the same entry -- purely to write the refusal. The
	// stat's answer is never trusted for anything else: the open above
	// already failed, and that is the decision.
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		var st unix.Stat_t
		if statErr := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); statErr == nil &&
			st.Mode&unix.S_IFMT == unix.S_IFLNK {
			target, rerr := os.Readlink(path)
			if rerr != nil {
				target = "a target that cannot be read"
			}

			return nil, fmt.Errorf(
				"%w: %s is a symbolic link to %s. A spool this product creates through a link is a spool whose location another account chose, and the modes it then sets protect that account's directory rather than the run's scripts; declare the real directory instead",
				ErrCustody, path, target)
		}

		return nil, fmt.Errorf("%w: %s is not a directory, and the script spool's whole path has to be", ErrPlan, path)
	}

	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be opened: %w", ErrPlan, path, err)
	}

	dir := os.NewFile(uintptr(fd), path)

	if err := checkOpenDirCustody(dir); err != nil {
		dir.Close() //nolint:errcheck // read-only descriptor

		return nil, err
	}

	return dir, nil
}

// checkOpenDirCustody holds one open directory to the spool's custody rule:
// owned by this process or root, and writable by nobody else.
//
// It asks the DESCRIPTOR rather than the path, so the answer is about the
// directory this code is holding open and not about whatever the path
// names by the time the next call runs.
func checkOpenDirCustody(dir *os.File) error {
	info, err := dir.Stat()
	if err != nil {
		return fmt.Errorf("%w: %s cannot be inspected: %w", ErrCustody, dir.Name(), err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%w: %s is a %s, not a directory", ErrPlan, dir.Name(), fileKind(info.Mode()))
	}

	if mode := info.Mode(); mode.Perm()&0o022 != 0 {
		return fmt.Errorf(
			"%w: %s has permissions %04o, which lets an account other than its owner write in it. It is on the path to the script spool, so an account that can write there can substitute the scripts this daemon executes; correct it (chmod go-w %s)",
			ErrCustody, dir.Name(), mode.Perm(), dir.Name())
	}

	return checkOwnership(dir.Name(), info)
}
