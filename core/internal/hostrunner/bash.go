package hostrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Preflight: fixing the interpreter ONCE, at startup, and reporting it.
//
// A runner that resolved `bash` through PATH at each execution would be a
// runner whose interpreter can change between two steps of the same run:
// PATH is part of the hook environment, which is operator-configurable per
// backup set, so a set that sets PATH=/opt/busybox/bin would silently run
// its hooks under a different shell from the deployment's other sets. That
// is not a hypothetical on the NAS platforms this product targets, where
// /bin/sh is ash and a second bash arrives with every package manager.
//
// So: resolved once, at serve time, recorded as an absolute path,
// reported by `status`, and used verbatim for every exec afterwards --
// including the `bash -n` preflight, so the shell that judges a script's
// syntax is the shell that will run it.
//
// The absence of bash is a first-class answer rather than an error at
// execution time. #809's acceptance criteria require `.local.sh`
// validation to fail BEFORE a backup starts when bash is missing, and a
// runner that only discovers it at exec time cannot make that true.

// ErrBash is every refusal about the interpreter: not found, not a
// regular file, not executable, or unwilling to report a version.
var ErrBash = errors.New("hostrunner: this host has no usable bash for workflow hooks")

// BashCandidates are the paths searched, in order, when no interpreter is
// configured.
//
// PATH is deliberately not consulted: see this file's preamble. The list
// is the three places bash actually lives on the platforms this product
// supports -- /bin/bash on Linux and in the Debian-family NAS packages,
// /usr/bin/bash on the distributions that merged /bin, and
// /usr/local/bin/bash where a BSD-ish or macOS host got it from a package
// manager. An operator whose bash is somewhere else configures the path,
// which is a fact worth stating in their configuration anyway.
func BashCandidates() []string {
	return []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash"}
}

// Bash is the interpreter this runner has fixed on.
type Bash struct {
	// Path is absolute, and is what every exec uses.
	Path string

	// Version is the first line of `bash --version`, trimmed. It is
	// reported rather than parsed: nothing here branches on a bash
	// version, and a parser would be a thing that can be wrong about a
	// string whose only job is to be shown to a human.
	Version string
}

// FindBash fixes the interpreter for this process's lifetime.
//
// configured is an operator-supplied absolute path, or empty to search
// BashCandidates. A configured path that does not work is a REFUSAL
// rather than a fallback to the search: an operator who named an
// interpreter and silently got a different one is in the worst of both
// worlds, since their hooks then work until the day the other one is
// removed.
func FindBash(ctx context.Context, configured string) (Bash, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return Bash{}, fmt.Errorf("%w: the configured interpreter %q is not an absolute path, and a relative interpreter is one that depends on where this process was started", ErrBash, configured)
		}
		return inspectBash(ctx, configured)
	}

	var tried []string
	for _, candidate := range BashCandidates() {
		b, err := inspectBash(ctx, candidate)
		if err == nil {
			return b, nil
		}
		tried = append(tried, candidate)
	}
	return Bash{}, fmt.Errorf("%w: none of %s is an executable bash. A deployment whose hooks are shell scripts needs one; install bash, or name its path", ErrBash, strings.Join(tried, ", "))
}

// inspectBash checks one candidate and asks it what it is.
//
// The Lstat is deliberate: a symbolic link at /bin/bash is entirely
// normal (it is a link to /usr/bin/bash on a merged-/usr host), so this
// does NOT refuse links -- it refuses a path that is not ultimately a
// regular, executable file. That is the opposite of internal/workflow's
// rule for a hook script, and for the opposite reason: a script in an
// operator's directory is an artifact whose custody this product is
// arguing about, and the system's shell is part of the platform.
func inspectBash(ctx context.Context, path string) (Bash, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Bash{}, fmt.Errorf("%w: %s: %v", ErrBash, path, err)
	}
	if !info.Mode().IsRegular() {
		return Bash{}, fmt.Errorf("%w: %s is not a regular file", ErrBash, path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return Bash{}, fmt.Errorf("%w: %s is not executable (mode %#o)", ErrBash, path, info.Mode().Perm())
	}

	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = []string{"PATH=" + DefaultPath}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return Bash{}, fmt.Errorf("%w: %s would not report its version: %v", ErrBash, path, err)
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	version := strings.TrimSpace(first)
	if version == "" {
		return Bash{}, fmt.Errorf("%w: %s reported an empty version, so this is not the interpreter it claims to be", ErrBash, path)
	}
	return Bash{Path: path, Version: version}, nil
}

// SyntaxCheck parses bytes with `bash -n` and runs nothing.
//
// The bytes go in on STDIN rather than into a temporary file, which is
// the one place this differs from execution. A syntax check has no
// working directory, no environment and no process group to own, so a
// file would be a thing to create, chmod, remove and get wrong for no
// gain; and `bash -n` reading stdin parses exactly the same grammar.
//
// --noprofile --norc is here too, for the reason it is everywhere in this
// package: without them, a /etc/bash.bashrc that sets `shopt -s
// expand_aliases` or `extglob` changes what parses.
//
// A non-zero exit is a syntax error and is returned as a *Failure with
// CodeSyntax, carrying bash's own message. bash's diagnostic names the
// line, which is the only useful thing anybody can say about a script
// that does not parse, so it is passed through rather than replaced.
func (b Bash) SyntaxCheck(ctx context.Context, script []byte) error {
	cmd := exec.CommandContext(ctx, b.Path, "--noprofile", "--norc", "-n")
	cmd.Env = []string{"PATH=" + DefaultPath}
	cmd.Stdin = bytes.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = "bash refused the script and said nothing about why"
		}
		return &Failure{Code: CodeSyntax, Message: message}
	}
	return &Failure{
		Code:    CodeInternal,
		Message: fmt.Sprintf("this runner could not run %s to check the script's syntax: %v", b.Path, err),
	}
}
