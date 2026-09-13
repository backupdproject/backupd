package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sticky bit, and why a hook directory is the one place it proves
// nothing.
//
// internal/secretref exempts a sticky directory from its writable-ancestor
// walk, and it is right to: POSIX restricts unlink and rename inside a
// sticky directory to the entry's owner, the directory's owner or root, so
// a 1777 directory cannot be used to REPLACE an existing 0600 secret file.
//
// A hook directory's threat is the other one. It is not "replace the file
// that is there", it is "CREATE a new file", because discovery executes
// every script it finds -- and sticky has never restricted creation. A
// 1777 hook directory (which is /tmp's mode, so it is the mode somebody
// reaches for when they are in a hurry) therefore means any local account
// can drop 00-root-shell.local.sh into the daemon's next backup.
//
// So this package refuses group- or other-writable directories outright,
// and these are the cases that would pass with secretref's rule copied.

func TestDiscoveryRefusesAStickyHookDirectory(t *testing.T) {
	t.Parallel()

	base := custodyTempDir(t)
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	root, err := NewRoot(base)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	stage := filepath.Join(base, "before")
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// 1777: exactly /tmp's mode, and exactly what a `chmod 1777` on a
	// hook directory an operator was debugging leaves behind.
	if err := os.Chmod(stage, os.ModeSticky|0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err = root.ResolveStage("before")
	if err == nil {
		t.Fatal("a 1777 hook directory was accepted. The sticky bit stops another account REPLACING a file that is already there; it has never stopped one CREATING a new one, and discovery executes every script it finds")
	}
	if !errors.Is(err, ErrCustody) {
		t.Errorf("ResolveStage returned %v, want an ErrCustody", err)
	}
	if !strings.Contains(err.Error(), "add a script to it") {
		t.Errorf("the refusal must say what the mode allows, got:\n\t%v", err)
	}
}

// The ancestor half of the same mistake: the hook directory's own mode is
// impeccable, and the directory holding it is world-writable with the
// sticky bit set.
//
// That is the shape an attacker-created stage has. A configuration naming
// /tmp/hooks/before, on a host where nobody has created /tmp/hooks yet, is
// a race any local account wins by running mkdir -p first: the directory
// it creates can be a perfectly ordinary 0755, and every script in it runs
// as this daemon. Sticky does not help, because nothing had to be replaced
// -- the path did not exist.
func TestDiscoveryRefusesAHookDirectoryUnderAStickyWritableAncestor(t *testing.T) {
	t.Parallel()

	base := custodyTempDir(t)

	sticky := filepath.Join(base, "shared")
	if err := os.Mkdir(sticky, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(sticky, os.ModeSticky|0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	rootDir := filepath.Join(sticky, "workflows")
	stage := filepath.Join(rootDir, "before")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(rootDir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	root, err := NewRoot(rootDir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	_, err = root.ResolveStage("before")
	if err == nil {
		t.Fatal("a hook directory under a 1777 ancestor was accepted; any local account that creates that path first owns every script this daemon executes out of it")
	}
	if !errors.Is(err, ErrCustody) {
		t.Errorf("ResolveStage returned %v, want an ErrCustody", err)
	}
	if !strings.Contains(err.Error(), sticky) {
		t.Errorf("the refusal must name the directory to fix (%s), got:\n\t%v", sticky, err)
	}
}

// Ownership, which is the fact a mode cannot express: a 0755 directory is
// only trustworthy if the account that may write it is one this product
// trusts. A hook directory owned by a service account nobody audited is a
// directory that account can fill with scripts the daemon runs as root.
//
// The chown needs privilege, so the assertion runs where the suite has it
// (a container build, a root CI shell) and is skipped where it does not.
// The two tests above are what hold the line for the unprivileged run.
func TestDiscoveryRefusesAHookDirectoryOwnedBySomebodyElse(t *testing.T) {
	t.Parallel()

	if os.Geteuid() != 0 {
		t.Skip("chowning a fixture to another account needs privilege")
	}

	base := custodyTempDir(t)
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	root, err := NewRoot(base)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	stage := filepath.Join(base, "before")
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// 65534 is nobody on both Linux and macOS: an account with no
	// business owning a directory this daemon executes out of.
	if err := os.Chown(stage, 65534, 65534); err != nil {
		t.Fatalf("chown: %v", err)
	}

	_, err = root.ResolveStage("before")
	if err == nil {
		t.Fatal("a hook directory owned by another account was accepted; that account can put any script it likes in it")
	}
	if !errors.Is(err, ErrCustody) {
		t.Errorf("ResolveStage returned %v, want an ErrCustody", err)
	}
	if !strings.Contains(err.Error(), "owned by") {
		t.Errorf("the refusal must say whose the directory is, got:\n\t%v", err)
	}
}

// The bound on one stage, which is the other thing an unbounded os.ReadDir
// hands somebody: a hook directory with a hundred thousand entries in it
// is a plan this process assembles in memory, hashes, and copies into a
// spool, one file at a time, while the backup window stands still.
//
// The refusal is the whole directory rather than the first N scripts, for
// captureScript's reason about a truncated file: a prefix of a plan is a
// different plan, and running half an operator's hooks is worse than
// running none and saying so.
func TestDiscoveryBoundsTheNumberOfScriptsInOneStage(t *testing.T) {
	t.Parallel()

	dir := custodyTempDir(t)

	for i := range MaxScriptsPerStage + 1 {
		writeScript(t, dir, scriptNumbered(i), "#!/bin/sh\n")
	}

	_, err := Discover(dir)
	if err == nil {
		t.Fatalf("a stage directory with %d scripts in it was accepted; the bound is what keeps one directory from becoming an unbounded plan", MaxScriptsPerStage+1)
	}
	if !errors.Is(err, ErrStageDir) {
		t.Errorf("Discover returned %v, want an ErrStageDir", err)
	}
	if !strings.Contains(err.Error(), "hook scripts") {
		t.Errorf("the refusal must say what the bound is about, got:\n\t%v", err)
	}

	// The positive control: exactly the bound is accepted, so the row
	// above is about the bound rather than about counting at all.
	atTheBound := custodyTempDir(t)
	for i := range MaxScriptsPerStage {
		writeScript(t, atTheBound, scriptNumbered(i), "#!/bin/sh\n")
	}

	scripts, err := Discover(atTheBound)
	if err != nil {
		t.Fatalf("a stage directory with exactly %d scripts was refused: %v", MaxScriptsPerStage, err)
	}
	if len(scripts) != MaxScriptsPerStage {
		t.Errorf("Discover returned %d scripts, want %d", len(scripts), MaxScriptsPerStage)
	}
}

// scriptNumbered names the i'th fixture script so that the names are
// distinct and every one of them passes the name rule.
func scriptNumbered(i int) string {
	return fmt.Sprintf("s%05d.local.sh", i)
}
