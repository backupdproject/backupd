package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Discovery: turning a configured directory name into an ordered list of
// scripts this process is willing to vouch for.
//
// # The custody rules are the SSH key's rules
//
// internal/secretref's ErrCustody and internal/transport/rclone/ssh.go's
// key-mode check both refuse a file another local account can read or
// replace, and they refuse it rather than warning, because a secret with
// somebody else's fingerprints on it is not a secret this product can make
// a claim about. A hook script is the same question with the stakes turned
// up: this daemon is about to EXECUTE the file, some of the time as root,
// some of the time on a different machine. So the rule is the same rule,
// with one difference in the mode test and it is worth stating why.
//
// A secret file is checked against 0o077 -- nobody but the owner may READ
// it -- because reading is the exposure. A script is checked against 0o022
// -- nobody but the owner may WRITE it -- because for a program, writing
// is the exposure and reading one is mostly harmless. A world-READABLE
// hook script is therefore accepted here, deliberately: refusing it would
// refuse the ordinary 0644 file every operator's editor produces, for no
// security gain at all.
//
// The ancestor walk is the same in both planes and for the same reason:
// a 0600 file inside a 0777 directory can be replaced by anybody, so its
// own mode proves nothing. firstWritableAncestor is a third copy of that
// walk (secretref and transport/rclone have the other two) and it is
// deliberately a copy: secretref's own doc argues that unifying them is a
// refactor of transport's most security-sensitive file, tracked
// separately.
//
// The RULE is NOT identical, and the difference is the one thing to carry
// away from this file: secretref exempts a sticky directory and this
// package does not. Sticky stops another account RENAMING or UNLINKING an
// entry that is already there, which is what protects an existing secret
// file; it has never stopped one CREATING a new entry, and creation is the
// whole exposure for a directory whose contents this daemon executes. A
// 1777 hook directory is a root shell for any local account. See
// checkDirectoryCustody, which also adds the question a mode cannot
// answer: WHO the owner is.
//
// The same rule extends to the SPOOL, which is where the scripts actually
// run from; spool.go carries that argument.
//
// # Why the root is canonicalized once and stages are constrained to it
//
// The workflow tree arrives read-only at a mount point an operator
// declared. That declaration is the trust boundary: it is the one path
// somebody looked at and approved. A stage directory is a NAME inside it,
// usually relative, and the danger is not an operator writing "../../etc"
// -- that is the easy case -- it is a symbolic link inside the tree
// pointing out of it, which no amount of lexical cleaning detects.
//
// So both happen: the lexical form must resolve under the root (which
// catches ".."), and the fully symlink-resolved form must ALSO be under
// the resolved root (which catches the link). Neither check subsumes the
// other, and a stage directory that is itself a symbolic link is refused
// outright rather than followed, exactly as a secret file that is a link
// is: the directories protecting the link say nothing about the ones
// protecting its target.

// ErrRoot is a refusal about the workflow root itself: it is not declared,
// not absolute, not a directory, or not in this process's custody. It is
// distinct from ErrStageDir because the two are different operator
// actions -- one is the mount, the other is a key in a config file.
var ErrRoot = errors.New("workflow: the configured workflow root cannot be used")

// ErrStageDir is a refusal about one configured hook directory: it escapes
// the root, is a symbolic link, is missing, or is not a directory.
var ErrStageDir = errors.New("workflow: this workflow stage directory cannot be used")

// ErrCustody is a refusal about a script file or a directory containing
// one: another local account can write it, so this process cannot vouch
// for what it is about to execute. It is the same refusal
// internal/secretref makes about a secret file, named the same way on
// purpose.
var ErrCustody = errors.New("workflow: this script is not in this process's sole custody")

// ErrScriptTooLarge is a refusal about a script's size. A hook is a shell
// script; something megabytes long is a payload that arrived where a
// script belongs, and the bound is enforced rather than truncated for
// internal/secretref's reason: a prefix of a program is a different
// program.
var ErrScriptTooLarge = errors.New("workflow: this workflow script is larger than a hook script may be")

// Root is a canonicalized, custody-checked workflow root: the one
// directory an operator declared and every stage directory must live
// inside.
//
// It is a type rather than a string so that the canonicalization and the
// checks happen once, at the edge, and every stage resolution afterwards
// is a comparison against a value that has already been proven. A function
// taking a root as a string would be a function each caller could hand an
// unchecked path to.
type Root struct {
	// declared is the path as the operator wrote it, kept for messages:
	// an operator who wrote "/workflows" needs to be told about
	// "/workflows", even when the resolved path is somewhere else
	// entirely.
	declared string

	// resolved is the same directory with every symbolic link in it
	// followed. Stage containment is decided against this.
	resolved string
}

// NewRoot canonicalizes and checks the declared workflow root.
//
// The root itself MAY be a symbolic link, and that asymmetry with stage
// directories is deliberate: a mount point an operator declared is
// something they looked at and approved, and /workflows being a link to
// /mnt/user/appdata/workflows is the ordinary shape of a NAS deployment. A
// stage directory, by contrast, is a name this product joined onto that
// root on the operator's behalf, so a link there is a path nobody
// approved.
func NewRoot(path string) (Root, error) {
	if path == "" {
		return Root{}, fmt.Errorf("%w: no workflow root is configured, so there is no approved directory for hook scripts to live in", ErrRoot)
	}

	if !filepath.IsAbs(path) {
		return Root{}, fmt.Errorf(
			"%w: %q is not an absolute path. The root is resolved before this process knows what its working directory will be, so a relative one would mean a different directory depending on how the daemon was started",
			ErrRoot, path)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return Root{}, fmt.Errorf("%w: %q cannot be resolved: %w", ErrRoot, path, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return Root{}, fmt.Errorf("%w: %q cannot be read: %w", ErrRoot, path, err)
	}

	if !info.IsDir() {
		return Root{}, fmt.Errorf("%w: %q is a %s, not a directory", ErrRoot, path, fileKind(info.Mode()))
	}

	return Root{declared: filepath.Clean(path), resolved: resolved}, nil
}

// IsZero reports whether no root has been established. A zero Root is the
// state of every deployment that has not configured workflows at all, and
// it resolves no stages.
func (r Root) IsZero() bool { return r.resolved == "" }

// Path is the root as the operator declared it.
func (r Root) Path() string { return r.declared }

// Resolved is the root with every symbolic link followed: the value stage
// containment is decided against.
func (r Root) Resolved() string { return r.resolved }

// ResolveStage turns one configured hook directory into the canonical
// absolute path discovery will read, or refuses it.
//
// dir may be relative (the ordinary case: "global-before", resolved inside
// the root) or absolute (which must still be inside the root). The
// refusals, in the order they are checked, each close something the next
// one cannot:
//
//   - a lexical ".." escape, before any filesystem call, because the
//     cheapest refusal is the one that never touched the disk;
//   - the directory itself being a symbolic link, refused rather than
//     followed;
//   - the fully-resolved path leaving the resolved root, which is the
//     symlinked-ancestor case no lexical rule can see;
//   - a containing directory any other account can write, which would let
//     that account swap the whole stage out between runs.
func (r Root) ResolveStage(dir string) (string, error) {
	if r.IsZero() {
		return "", fmt.Errorf("%w: no workflow root is configured, so %q cannot be resolved", ErrRoot, dir)
	}

	if dir == "" {
		return "", fmt.Errorf("%w: an empty stage directory is not a directory; a stage that should not run is one with no directory configured at all", ErrStageDir)
	}

	lexical := dir
	if !filepath.IsAbs(lexical) {
		lexical = filepath.Join(r.declared, lexical)
	}
	lexical = filepath.Clean(lexical)

	if err := r.contains(dir, lexical, r.declared); err != nil {
		return "", err
	}

	info, err := os.Lstat(lexical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf(
				"%w: %q was configured as a hook directory and does not exist at %s. An unset directory means the stage is disabled; a configured one that is missing is a mistake this product refuses rather than silently running nothing",
				ErrStageDir, dir, lexical)
		}

		return "", fmt.Errorf("%w: %q cannot be read at %s: %w", ErrStageDir, dir, lexical, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, rerr := os.Readlink(lexical)
		if rerr != nil {
			target = "a target that cannot be read"
		}

		return "", fmt.Errorf(
			"%w: %s is a symbolic link to %s. The permissions protecting the link say nothing about the ones protecting its target, so this product will not execute scripts found through one; declare the target directory instead",
			ErrStageDir, lexical, target)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is a %s, not a directory", ErrStageDir, lexical, fileKind(info.Mode()))
	}

	resolved, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		return "", fmt.Errorf("%w: %s cannot be resolved: %w", ErrStageDir, lexical, err)
	}

	if err := r.contains(dir, resolved, r.resolved); err != nil {
		return "", err
	}

	if err := checkDirectoryCustody(resolved); err != nil {
		return "", err
	}

	return resolved, nil
}

// contains is the containment test, run twice per stage against the two
// forms of the root. Both calls matter: see this file's preamble.
func (r Root) contains(declaredDir, candidate, root string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return fmt.Errorf("%w: %q cannot be compared against the workflow root %s: %w", ErrStageDir, declaredDir, root, err)
	}

	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf(
			"%w: %q resolves to %s, which is outside the approved workflow root %s. Every hook script must live inside the root an operator declared: that declaration is the only thing that makes the tree trusted",
			ErrStageDir, declaredDir, candidate, root)
	}

	return nil
}

// Script is one discovered, name-validated script. It carries no content:
// reading the bytes is Snapshot's job and happens exactly once, through a
// descriptor whose custody has been checked.
type Script struct {
	// Name is the basename, as it passed ParseScriptName.
	Name string

	// Target is where it runs, read off Name.
	Target Target

	// path is the absolute path it was discovered at, inside a resolved
	// stage directory. It is the path Snapshot opens, ONCE, and nothing
	// opens afterwards.
	//
	// It is unexported, and that is the point rather than an accident of
	// scope. A Script that handed out the path inside the workflow root
	// would be a value somebody outside this package could re-open --
	// after the snapshot, when the tree is no longer the authority and
	// the file may have been replaced by anybody with write access to it.
	// The spool exists precisely so that no such path survives planning;
	// see Plan.OpenScript for the only path this feature exposes, and
	// what it checks before it opens it.
	path string
}

// Read returns this script's bytes, held to exactly the custody rules a
// capture holds them to, and without spooling anything.
//
// # Why this exists beside Snapshot, which also reads the bytes
//
// Because #906 made a passing static verification a PRECONDITION OF
// SAVING a workflow, and a save is not a run. Snapshot is what a run
// does: it mints a run id, takes a spool directory, writes a copy of
// every script into it and hashes the plan, because a run has to be able
// to re-execute the exact bytes it verified. A configuration write has
// nothing to recover and no state directory guarantee, so asking it to
// build a spool in order to look at a file would make `settings workflow
// patch` fail on a deployment where the verification is precisely what
// is wanted.
//
// What it must NOT be is a second reader. Every rule about what this
// process may vouch for -- no symbolic link, no fifo, nothing another
// account can write, an owner this process trusts, a bounded size, a
// file that grew mid-read refused rather than truncated -- lives in
// readScriptBytes, and this is that function with the spool left out. A
// verification that accepted a file a run would refuse, or the reverse,
// would be worse than none.
//
// maxSize is the deployment's configured bound
// (config.EffectiveMaxScriptSize), so the answer about "is this file too
// big" is the same answer a run would give.
//
// The bytes are returned and the PATH still is not: Script.path stays
// unexported for the reason its own field doc gives, and a caller that
// wants to look at a script gets what it contained when this process
// read it rather than a path it could re-open later.
func (s Script) Read(maxSize int64) ([]byte, error) {
	return readScriptBytes(s.path, maxSize)
}

// Discover lists the scripts in one resolved stage directory, in the
// documented order, or refuses the directory's contents.
//
// stageDir must have come from Root.ResolveStage. An existing, empty
// directory returns no scripts and no error -- "a stage with nothing to
// run" is a legitimate configuration and a common one while an operator is
// building a workflow up.
//
// EVERY entry is held to the name rule, including ones that obviously are
// not scripts. That is the deliberate direction and it is worth defending,
// because the friendlier alternative is worse: silently ignoring
// "backup.sh" means an operator who forgot the target suffix gets a backup
// that runs no hooks and reports success, which is precisely the quiet
// failure #808 exists to make impossible. A README in a hook directory is
// refused too, and the refusal says how to name a script; a directory that
// is documentation belongs beside the stage directory rather than in it.
func Discover(stageDir string) ([]Script, error) {
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be listed: %w", ErrStageDir, stageDir, err)
	}

	if len(entries) > MaxScriptsPerStage {
		return nil, fmt.Errorf(
			"%w: %s holds %d entries and one stage may declare at most %d hook scripts. The whole directory is refused rather than the first %d executed, for the reason an oversized script is: a prefix of a plan is a different plan",
			ErrStageDir, stageDir, len(entries), MaxScriptsPerStage, MaxScriptsPerStage)
	}

	scripts := make([]Script, 0, len(entries))

	for _, entry := range entries {
		name := entry.Name()

		target, err := ParseScriptName(name)
		if err != nil {
			return nil, fmt.Errorf("%w (found in %s)", err, stageDir)
		}

		// The type check is here as well as at capture time, and both are
		// needed. Here it produces the message that names what was found
		// where a script belongs; at capture time it is repeated on the
		// open descriptor, which is the copy that cannot be raced.
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf(
				"%w: %s is a %s, not a regular file. A hook is a file this process reads once and copies; anything else is something whose contents are supplied by whoever is on the other end of it",
				ErrCustody, filepath.Join(stageDir, name), fileKind(entry.Type()))
		}

		scripts = append(scripts, Script{Name: name, Target: target, path: filepath.Join(stageDir, name)})
	}

	slices.SortFunc(scripts, func(a, b Script) int { return compareScriptNames(a.Name, b.Name) })

	return scripts, nil
}

// MaxScriptsPerStage bounds how many entries one hook directory may have.
//
// os.ReadDir is unbounded, and everything downstream of it is per-entry
// work this process does inside the backup window: an open, a read, a
// sha256, a spooled copy, a journal row. A directory with a hundred
// thousand entries in it is not a workflow somebody wrote, it is a
// directory something else filled up (a log rotation pointed at the wrong
// path, a build output, an attacker padding a plan), and assembling a plan
// out of it is how one mistake becomes a backup that never starts.
//
// Sixty-four is far past any workflow a person maintains by hand -- the
// stages are already split four ways -- and small enough that the whole
// plan is bounded work. It is a constant rather than a setting because a
// deployment that needs to raise it has a different problem.
const MaxScriptsPerStage = 64

// checkDirectoryCustody refuses a hook directory another account can write
// to, one with such an ancestor, or one this process does not own.
//
// # Why the sticky exception is NOT here
//
// internal/secretref's walk exempts a directory carrying os.ModeSticky,
// and for a secret FILE that is correct: POSIX restricts unlink and rename
// inside a sticky directory to the entry's owner, the directory's owner or
// root, so 1777 cannot be used to REPLACE an existing 0600 file.
//
// A hook directory's exposure is the opposite operation. Discovery
// executes every script it FINDS, so the attack is CREATING a new entry --
// and sticky has never restricted creation. Copying secretref's rule here
// would accept a 1777 hook directory, which is /tmp's mode and therefore
// the mode somebody reaches for, and any local account could then drop
// 00-root-shell.local.sh into tonight's backup. The ancestor walk drops
// the exception for the same reason one step removed: a world-writable
// ancestor is a path any account can CREATE the missing components of, so
// the stage directory it wins the race to make is one it owns outright.
//
// # Why ownership is checked as well as mode
//
// A mode says who MAY write; it does not say whether the account that may
// is one this product trusts. A 0755 directory owned by an unaudited
// service account is a directory that account fills with scripts this
// daemon runs as root. So the owner has to be this process's own user or
// root -- the two accounts that can already do anything this daemon can.
func checkDirectoryCustody(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%w: %s cannot be read: %w", ErrStageDir, dir, err)
	}

	if mode := info.Mode(); mode.Perm()&0o022 != 0 {
		sticky := ""
		if mode&os.ModeSticky != 0 {
			sticky = ". The sticky bit does not make this safe: it restricts renaming and deleting the entries that are already there, and what matters for a directory of programs is that a new one can be created"
		}

		return fmt.Errorf(
			"%w: the hook directory %s has permissions %04o, which lets an account other than its owner add a script to it. Every script in it would run with this daemon's privileges; correct it (chmod go-w %s)%s",
			ErrCustody, dir, mode.Perm(), dir, sticky)
	}

	if err := checkOwnership(dir, info); err != nil {
		return err
	}

	if ancestor, mode, err := firstWritableAncestor(dir); err != nil {
		return fmt.Errorf("%w: checking the directories containing %s: %w", ErrCustody, dir, err)
	} else if ancestor != "" {
		return fmt.Errorf(
			"%w: the hook directory %s has a containing directory %s with permissions %04o: a group- or world-writable directory lets any local account replace the whole directory of scripts, or create the path in the first place and own everything under it, regardless of the modes further down; correct it (chmod go-w %s)",
			ErrCustody, dir, ancestor, mode.Perm(), ancestor)
	}

	return nil
}

// checkOwnership refuses a path owned by an account that is neither this
// process's own nor root.
//
// Root is accepted whatever this process's own user is, because root can
// already replace this binary: refusing a root-owned hook tree would be
// protection against an account that does not need the hook tree to get
// what it wants, at the cost of refusing the ordinary deployment where
// /workflows is laid down by root and the daemon runs as a service user.
//
// The FileInfo is passed in rather than re-stat'd so that the mode check
// above and this check are answers about the same inode: two stats are two
// windows.
func checkOwnership(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Not a platform this daemon ships on. Refusing here would be a
		// refusal nobody can act on, and pretending the check passed
		// would be a claim this code cannot make, so say which it is.
		return fmt.Errorf("%w: the owner of %s cannot be read on this platform, and a directory of programs whose owner is unknown is one this product cannot vouch for", ErrCustody, path)
	}

	euid := os.Geteuid()
	if int(stat.Uid) == euid || stat.Uid == 0 {
		return nil
	}

	return fmt.Errorf(
		"%w: %s is owned by uid %d, and this process runs as uid %d. A directory this daemon executes scripts out of has to be owned by this process's own account or by root; any other owner is an account that can put a script there without touching this configuration (chown %d %s)",
		ErrCustody, path, stat.Uid, euid, euid, path)
}

// firstWritableAncestor walks from path's own directory to the filesystem
// root and reports the first ancestor any account other than its owner can
// write to, or "" when none of them is.
//
// It is a third copy of internal/secretref's function of the same name,
// and secretref's own doc says why the mechanism is duplicated rather than
// shared: unifying it with internal/transport/rclone's copy is a refactor
// of that package's most security-sensitive file, tracked separately.
//
// The RULE is NOT identical, and the difference is the one thing to read
// before copying this back the other way: secretref's walk exempts a
// sticky directory and this one does not. Sticky protects the entries that
// EXIST in a directory from being renamed or unlinked by another account,
// which is what a secret file needs. It says nothing about creating a new
// entry, and creating is the whole exposure for a tree this daemon
// executes: a world-writable /tmp lets any account win the race to create
// /tmp/hooks, and every script under the directory it then owns runs with
// this daemon's privileges. See checkDirectoryCustody.
func firstWritableAncestor(path string) (dir string, mode os.FileMode, err error) {
	dir, err = filepath.Abs(filepath.Dir(path))
	if err != nil {
		return "", 0, fmt.Errorf("resolving the directory containing %s: %w", path, err)
	}

	for {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			return "", 0, fmt.Errorf("checking permissions on %s: %w", dir, statErr)
		}

		m := info.Mode()
		if m.Perm()&0o022 != 0 {
			return dir, m, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", 0, nil
		}

		dir = parent
	}
}

// fileKind names what was found where a regular file or a directory was
// expected, so the refusal is one an operator can act on rather than "not
// a regular file". It is internal/secretref's function of the same name,
// duplicated for the reason stated above it.
func fileKind(m os.FileMode) string {
	switch {
	case m&os.ModeSymlink != 0:
		return "symbolic link"
	case m.IsDir():
		return "directory"
	case m&os.ModeNamedPipe != 0:
		return "named pipe"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device"
	case m.IsRegular():
		return "regular file"
	default:
		return "special file"
	}
}
