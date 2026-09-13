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

	// First line, before anything else can be reached: the four variables
	// that make bash run code at startup are cleared, so a hook's CHILD
	// shells cannot be redirected by whatever the remote account exports.
	// stderr is discarded on this line alone because SHELLOPTS and
	// BASHOPTS are readonly in bash and unset says so, and that complaint
	// would otherwise be the first thing in the step's captured stderr.
	b.WriteString("unset BASH_ENV ENV SHELLOPTS BASHOPTS 2>/dev/null\n")

	for _, p := range pairs {
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
