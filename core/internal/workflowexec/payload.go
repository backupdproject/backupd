package workflowexec

import (
	"fmt"
	"strings"
)

// BashArgs are the arguments this envelope always invokes bash with, after
// the validated bash path and before nothing else.
//
//   - --noprofile and --norc: the hook's behaviour must come from its own
//     bytes and the environment this product built, never from an account's
//     startup files. On the remote side that is not merely tidiness: sshd
//     runs the fixed command through the account's login shell, so the
//     login shell's own rc file is the last thing before this.
//   - -s: read the script from standard input. It is explicit rather than
//     implied, because bash's "no operands, so read stdin" behaviour
//     depends on there being no operands, and an envelope that grew a
//     positional argument later would silently start treating it as a
//     script path.
//
// There is deliberately nothing else. No -e, no -u, no -o pipefail, no -x:
// a hook's bytes run exactly as captured.
var BashArgs = []string{"--noprofile", "--norc", "-s"}

// payloadScriptVar is the one shell variable this envelope needs on the far
// side.
//
// It is refused as an operator environment name (see validateName) rather
// than merely documented, because the alternative is a variable an
// operator configured that this product silently overwrites -- which is a
// variable that looks like it works, the exact failure
// workflow.ValidateEnvName refuses the BACKUPD_ prefix to avoid.
const payloadScriptVar = "__backupd_script"

// StdinPayload is the whole of what an executor writes to bash's standard
// input: the environment bootstrap, then the captured script bytes, then
// the one line that runs them.
//
// The bootstrap CLEARS before it sets (clearInheritedEnvironment), so the
// environment a remote hook sees is the sanitized baseline plus the
// resolved plan and nothing the remote account exported -- the same
// environment the host runner builds for a NAME.local.sh.
//
// # Why the environment is shell text here and a block everywhere else
//
// An SSH exec channel has no environment. The protocol has a way to ask
// (SSH_MSG_CHANNEL_REQUEST "env"), and every hardened sshd refuses it
// unless AcceptEnv was configured for those names, so an envelope that
// depended on it would work on the operator's laptop and not on the host
// they actually back up. The remaining places to put a value are the
// remote command line -- which publishes it in the remote process list,
// where any account on that host can read it -- and the channel's own
// stdin. So: stdin.
//
// # Why the values can never become syntax
//
// Every value is wrapped in a single-quoted shell literal. Inside single
// quotes a POSIX shell interprets NOTHING: no expansion, no substitution,
// no escapes, not even a backslash. The only byte that needs handling is
// the apostrophe itself, and the only thing that can be done with it is to
// close the literal, emit an escaped apostrophe, and open a new one --
// which is what ShellQuote does, and which is why a value containing
// $(...), backticks, quotes, backslashes or newlines arrives byte for byte
// rather than being executed.
//
// # Why the script is a literal too, and not read from the stream
//
// bash reads a script from a non-seekable stdin one command at a time, so
// a `read` builtin in the payload competes with bash's own parser for the
// same bytes -- measured: a `read` swallows the NEXT LINE OF THE SCRIPT as
// its data. Making the script a single-quoted literal that bash parses
// (rather than data something reads) removes that race entirely. It also
// means no here-document, which bash implements with a temporary file, and
// therefore no file on the remote host at any point.
//
// The trailing `0</dev/null` gives the hook the same standard input the
// host runner gives it: closed. Without it a hook that read stdin would be
// reading the remainder of its own script.
//
// One documented cost: because the script runs through eval, a RUNTIME
// diagnostic bash prints carries a line number offset by the payload's own
// prologue. Syntax errors do not, because they are caught by a separate
// `bash -n` preflight against the captured bytes alone, where the line
// numbers are the script's own.
func StdinPayload(environ []string, script []byte) ([]byte, error) {
	if err := ValidateScript(script); err != nil {
		return nil, err
	}
	pairs, err := parseEnviron(environ)
	if err != nil {
		return nil, err
	}

	var b strings.Builder

	// First, before any export can be reached: everything the remote
	// account, its login shell and sshd exported is removed, so what
	// follows is the whole of the hook's environment rather than a layer
	// on top of somebody else's.
	b.WriteString(clearInheritedEnvironment)

	for _, p := range layer(baselinePairs(), pairs) {
		b.WriteString("export ")
		b.WriteString(p.name)
		b.WriteString("=")
		b.WriteString(ShellQuote(p.value))
		b.WriteString("\n")
	}

	b.WriteString(payloadScriptVar)
	b.WriteString("=")
	b.WriteString(ShellQuote(string(script)))
	b.WriteString("\n")
	b.WriteString("eval \"$")
	b.WriteString(payloadScriptVar)
	b.WriteString("\" 0</dev/null\n")

	return []byte(b.String()), nil
}

// clearInheritedEnvironment is the payload's first act, and the whole of
// what gives the remote executor the SAME environment contract as the host
// runner: a hook sees the resolved plan on top of
// workflow.SanitizedBaseline, and nothing else.
//
// # Why unsetting the four startup variables is not enough
//
// An SSH exec channel inherits whatever the server and the account put
// there: HOME, USER, LANG, LOGNAME, MAIL, SSH_CONNECTION, SSH_CLIENT,
// anything an sshd SetEnv or an account's own arrangement adds. A payload
// that only exported the plan would leave every one of those visible to
// the hook, so the same NAME.remote.sh would mean one thing here and
// another thing on the machine it runs on -- which is the failure the
// shared envelope exists to prevent. The host runner hands execve a block
// built from SanitizedBaseline (one variable, PATH), so the remote side
// has to REMOVE what it could not choose.
//
// # Why an unset loop and not "env -i"
//
// Re-execing under env -i would mean a second bash reading this same
// non-seekable stdin, competing with the first one's parser for the
// remaining bytes. The loop runs in the shell that is already reading the
// payload and needs no second process.
//
// # Why the four names are unset first as well
//
// SHELLOPTS and BASHOPTS are readonly, so the loop's unset cannot remove
// them and only the export attribute could be dropped; the explicit line
// keeps the intent legible and its stderr quiet. Neither line can un-apply
// a BASH_ENV bash already sourced at startup -- that is the preflight's
// question, against the server, and it is refused there.
//
// # What is deliberately kept
//
// PWD, OLDPWD, SHLVL and _ are bash's own bookkeeping, set by the shell
// rather than inherited: bash exports them at startup whichever way it was
// invoked, so a hook run by the host runner under execve sees them too.
// Removing them here would make the two executors differ, in the direction
// of a $PWD that is empty until the hook happens to cd.
//
// Exported FUNCTIONS are removed as well. An exported function is the one
// environment entry that can redefine a command a hook calls by name, and
// nothing this product sends has defined one at this point, so every
// function present is inherited.
const clearInheritedEnvironment = `unset BASH_ENV ENV SHELLOPTS BASHOPTS 2>/dev/null
for __backupd_name in $(compgen -e 2>/dev/null); do
case "$__backupd_name" in PWD|OLDPWD|SHLVL|_) continue ;; esac
unset -v "$__backupd_name" 2>/dev/null || :
done
for __backupd_name in $(compgen -A function 2>/dev/null); do
unset -f "$__backupd_name" 2>/dev/null || :
done
unset -v __backupd_name 2>/dev/null || :
`

// ValidateScript refuses script bytes this envelope cannot carry.
//
// A NUL is the only refusal. bash itself does not treat one as fatal -- it
// warns and drops it -- and "warns and drops" is the problem: a script
// whose bytes are not the bytes that ran is a script whose sha256 in the
// audit trail describes something that never executed. Refusing is the
// only answer that keeps the hash meaningful.
func ValidateScript(script []byte) error {
	for i, c := range script {
		if c == 0 {
			return fmt.Errorf("%w: there is a NUL byte at offset %d, and a shell cannot carry one: bash drops it with a warning, so the bytes that ran would not be the bytes that were captured and hashed",
				ErrScriptEncoding, i)
		}
	}

	return nil
}

// ShellQuote renders s as a single-quoted POSIX shell literal.
//
// This is the entire injection defence, so it is written as the one thing
// single quotes allow and nothing more: every byte is literal, and an
// apostrophe is spelled by closing the literal, escaping the apostrophe
// outside it, and reopening. There is no escape character inside single
// quotes in any POSIX shell, which is exactly why this encoding has no
// cases in it and cannot be got wrong by a value.
//
// The only byte it cannot carry is NUL, which validateValue and
// ValidateScript refuse before anything reaches here.
func ShellQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for {
		before, after, found := strings.Cut(s, "'")
		b.WriteString(before)
		if !found {
			break
		}
		b.WriteString(`'\''`)
		s = after
	}
	b.WriteByte('\'')

	return b.String()
}
