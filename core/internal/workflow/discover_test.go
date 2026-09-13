package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Discovery's refusal suite: the file-system situations a hook directory
// can be in, and the ones this product will not execute out of.
//
// Every case here builds a REAL tree under t.TempDir and drives the real
// code path, because every one of them is about what the operating system
// says rather than about what a string looks like. A fake filesystem would
// be testing this package's opinion of a symlink rather than a symlink.

// writeScript creates a hook script with a mode this package accepts, and
// returns its path. 0700 rather than 0600 because that is what an operator
// who chmod +x'd their script has, and it has to be acceptable.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
	// WriteFile applies the umask, and these tests assert on modes.
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("chmod fixture %s: %v", path, err)
	}

	return path
}

// newRoot builds a workflow root with owner-only ancestry, which is what
// a real deployment has and what the custody walk requires. t.TempDir's
// own parent is 0700 on every platform this runs on, so the walk from a
// stage directory up to / passes.
func newRoot(t *testing.T) (Root, string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}

	root, err := NewRoot(dir)
	if err != nil {
		t.Fatalf("NewRoot(%s): %v", dir, err)
	}

	return root, dir
}

func mkStage(t *testing.T, rootDir, name string) string {
	t.Helper()

	dir := filepath.Join(rootDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}

	return dir
}

func TestNewRootRefusesWhatIsNotAnApprovedRoot(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "workflows")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	cases := []struct {
		what    string
		path    string
		mustSay string
	}{
		{"unconfigured", "", "no workflow root is configured"},
		{"relative", "workflows", "not an absolute path"},
		{"missing", filepath.Join(t.TempDir(), "nope"), "cannot be resolved"},
		{"a file where a directory belongs", file, "not a directory"},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			root, err := NewRoot(tc.path)
			if err == nil {
				t.Fatalf("NewRoot(%q) = %+v with no error", tc.path, root)
			}
			if !errors.Is(err, ErrRoot) {
				t.Errorf("NewRoot(%q) returned %v, want an ErrRoot", tc.path, err)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("NewRoot(%q) said %v, want it to contain %q", tc.path, err, tc.mustSay)
			}
		})
	}
}

// The root itself may be a symbolic link, and that is deliberate rather
// than an oversight: /workflows -> /mnt/user/appdata/workflows is the
// ordinary shape of a NAS deployment, and the operator declared it.
func TestNewRootAcceptsASymlinkedRootBecauseAnOperatorDeclaredIt(t *testing.T) {
	t.Parallel()

	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "workflows")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this filesystem will not create symlinks: %v", err)
	}

	root, err := NewRoot(link)
	if err != nil {
		t.Fatalf("NewRoot on a symlinked root: %v", err)
	}
	if root.Path() != link {
		t.Errorf("Path() = %s, want the path the operator declared (%s): a refusal about a resolved path they never wrote is one they cannot find", root.Path(), link)
	}

	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if root.Resolved() != resolvedReal {
		t.Errorf("Resolved() = %s, want %s", root.Resolved(), resolvedReal)
	}
}

func TestResolveStageRefusesEveryEscapeAndUnsafeShape(t *testing.T) {
	t.Parallel()

	root, rootDir := newRoot(t)
	mkStage(t, rootDir, "before")

	outside := t.TempDir()

	linkedDir := filepath.Join(rootDir, "linked")
	symlinksWork := os.Symlink(outside, linkedDir) == nil

	// An ANCESTOR of the stage directory is a link out of the root, and
	// the stage directory itself is an ordinary directory. This is the
	// case no lexical rule can see: "via/before" is textually inside the
	// root, every component of it exists, and the directory the scripts
	// actually live in is somewhere nobody approved.
	if symlinksWork {
		if err := os.MkdirAll(filepath.Join(outside, "before"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(rootDir, "via")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	worldWritable := mkStage(t, rootDir, "loose")
	if err := os.Chmod(worldWritable, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	notADir := filepath.Join(rootDir, "file-stage")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	cases := []struct {
		what     string
		dir      string
		wantIs   error
		mustSay  string
		needLink bool
	}{
		{what: "unset", dir: "", wantIs: ErrStageDir, mustSay: "empty stage directory"},
		{what: "a lexical .. escape", dir: "../elsewhere", wantIs: ErrStageDir, mustSay: "outside the approved workflow root"},
		{what: "a .. escape buried mid-path", dir: "before/../../elsewhere", wantIs: ErrStageDir, mustSay: "outside the approved workflow root"},
		{what: "an absolute path outside the root", dir: outside, wantIs: ErrStageDir, mustSay: "outside the approved workflow root"},
		{what: "a configured directory that is missing", dir: "gone", wantIs: ErrStageDir, mustSay: "does not exist"},
		{what: "a file where a stage directory belongs", dir: "file-stage", wantIs: ErrStageDir, mustSay: "not a directory"},
		{what: "the stage directory is a symlink", dir: "linked", wantIs: ErrStageDir, mustSay: "symbolic link", needLink: true},
		{what: "a symlinked ancestor escapes", dir: "via/before", wantIs: ErrStageDir, mustSay: "outside the approved workflow root", needLink: true},
		{what: "a world-writable hook directory", dir: "loose", wantIs: ErrCustody, mustSay: "add a script to it"},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			if tc.needLink && !symlinksWork {
				t.Skip("this filesystem will not create symlinks")
			}

			got, err := root.ResolveStage(tc.dir)
			if err == nil {
				t.Fatalf("ResolveStage(%q) = %s with no error", tc.dir, got)
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("ResolveStage(%q) returned %v, want it to wrap %v", tc.dir, err, tc.wantIs)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("ResolveStage(%q) said:\n\t%v\nwant it to contain %q", tc.dir, err, tc.mustSay)
			}
		})
	}

	// The positive control. Without it every row above would also pass
	// against a ResolveStage that refused everything.
	resolved, err := root.ResolveStage("before")
	if err != nil {
		t.Fatalf("a plain hook directory inside the root must resolve: %v", err)
	}
	if filepath.Base(resolved) != "before" {
		t.Errorf("ResolveStage(\"before\") = %s", resolved)
	}
	if !filepath.IsAbs(resolved) {
		t.Errorf("ResolveStage returned %s, which is not absolute; the value is opened later by a process whose working directory is not ours", resolved)
	}
}

// An ancestor of the hook directory that anybody can write is the case a
// per-file mode check cannot catch: the scripts can be 0700 and owned by
// root, and any local account can still replace the directory holding them
// with one of its own.
func TestResolveStageRefusesAWritableAncestor(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	loose := filepath.Join(base, "loose")
	if err := os.MkdirAll(loose, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	rootDir := filepath.Join(loose, "workflows")
	if err := os.MkdirAll(filepath.Join(rootDir, "before"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	root, err := NewRoot(rootDir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	_, err = root.ResolveStage("before")
	if err == nil {
		t.Fatal("a hook directory whose grandparent is world-writable was accepted; any local account can replace the whole tree of scripts this daemon executes")
	}
	if !errors.Is(err, ErrCustody) {
		t.Errorf("ResolveStage returned %v, want an ErrCustody", err)
	}
	if !strings.Contains(err.Error(), loose) {
		t.Errorf("the refusal must name the directory to fix (%s), got:\n\t%v", loose, err)
	}
}

// A symbolic link where a SCRIPT belongs, as opposed to where a stage
// directory belongs. Both are refused and they are different messages,
// because the fix is different: one is "declare the target directory", the
// other is "put the script itself in the hook directory".
func TestDiscoverRefusesASymlinkedScript(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	real := writeScript(t, t.TempDir(), "real.local.sh", "#!/bin/sh\n")

	if err := os.Symlink(real, filepath.Join(dir, "linked.local.sh")); err != nil {
		t.Skipf("this filesystem will not create symlinks: %v", err)
	}

	_, err := Discover(dir)
	if err == nil {
		t.Fatal("a symlinked hook script was accepted; the directories protecting the link say nothing about the ones protecting the file")
	}
	if !errors.Is(err, ErrCustody) {
		t.Errorf("Discover returned %v, want an ErrCustody", err)
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("the refusal must say what was found, got:\n\t%v", err)
	}
}
