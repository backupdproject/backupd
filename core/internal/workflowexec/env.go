package workflowexec

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/backupdproject/backupd/core/internal/workflow"
)

// ErrEnvEncoding is every refusal about an environment entry this envelope
// cannot carry: a name no shell could read back, or a NUL byte.
//
// One sentinel for the class, so a caller can tell "this environment
// cannot be encoded" from "the transport failed", which are an operator's
// problem and an infrastructure problem respectively.
var ErrEnvEncoding = errors.New("workflowexec: this environment entry cannot be encoded for a hook")

// ErrScriptEncoding is the same for the script bytes themselves.
var ErrScriptEncoding = errors.New("workflowexec: these script bytes cannot be encoded for a hook")

// envName is internal/workflow's own rule, compiled from the pattern that
// package exports rather than restated here.
//
// It is deliberately NOT workflow.ValidateEnvName: that function also
// refuses the whole BACKUPD_ prefix, which is correct for a name an
// OPERATOR wrote and wrong here, because by this point the built-ins have
// already been merged in and they are the thing that prefix is reserved
// for. What is checked at this layer is only the shape: whether a shell on
// the far side can read the variable back at all.
var envName = regexp.MustCompile(workflow.EnvNamePattern)

// startupVariables are the four variables that make bash run code, or
// change how it runs code, BEFORE the first line of a hook script is
// reached.
//
// BASH_ENV and ENV name a file a non-interactive shell sources at startup:
// whoever controls either one controls the hook. SHELLOPTS and BASHOPTS are
// applied as shell options at startup, so a value of "xtrace" or "errexit"
// arriving in the environment changes what a script MEANS without changing
// a byte of it -- which is the same argument this package makes for
// injecting no options of its own.
//
// They are cleared on both encodings, and the remote envelope unsets them
// again on the far side, because the two halves defend against different
// things: sanitizing here stops this product passing them on, and unsetting
// there stops the remote account's own environment reaching a hook's
// children. What neither can do is un-apply a SHELLOPTS that the remote
// shell already honoured at startup, which is why the remote preflight
// inspects them as well.
var startupVariables = []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS"}

// StartupVariables returns those four names, for a preflight that has to
// ask about them and for the docs. A function rather than an exported
// slice, because a package-level slice is writable by every importer.
func StartupVariables() []string { return append([]string(nil), startupVariables...) }

// SanitizeBaseline returns parent with the startup variables removed.
//
// It is a backstop rather than the main defence. A hook does not inherit
// this daemon's environment at all (workflow.SanitizedBaseline is the
// whole of what it starts from, and it is one variable), so in the
// ordinary case there is nothing here to remove. It exists because
// "nothing inherits" is a property of a call site, and this is the one
// function both executors pass their baseline through: a future caller
// that starts from os.Environ() gets the removal for free instead of
// reintroducing a hook-controls-the-shell bug nobody would look for.
func SanitizeBaseline(parent []string) []string {
	if len(parent) == 0 {
		return nil
	}

	out := make([]string, 0, len(parent))
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if isStartupVariable(name) {
			continue
		}
		out = append(out, entry)
	}

	return out
}

func isStartupVariable(name string) bool {
	for _, v := range startupVariables {
		if name == v {
			return true
		}
	}

	return false
}

// ProcessEnv is the environment block for an executor that can hand one to
// execve: the sanitized baseline, then environ layered over it, with a
// later entry replacing an earlier one of the same name.
//
// environ is already in precedence order and already merged by the caller
// (internal/workflow's Environment does the layering and
// Resolved.Environ the resolving), so this does not re-decide precedence.
// What it does is validate, in the same function StdinPayload validates
// with, so the two encodings accept and refuse exactly the same
// environments.
func ProcessEnv(baseline, environ []string) ([]string, error) {
	pairs, err := parseEnviron(environ)
	if err != nil {
		return nil, err
	}

	out := SanitizeBaseline(baseline)
	for _, p := range pairs {
		out = replaceOrAppend(out, p.name, p.name+"="+p.value)
	}

	return out, nil
}

// baselinePairs is the environment the remote encoding starts from, in the
// one form this file works in.
//
// It is workflow.SanitizedBaseline read here rather than restated, because
// the two executors must start a hook from the same place: the host runner
// passes that baseline to ProcessEnv, and the remote payload has no caller
// holding it -- the resolved environment it is handed is already layered.
// Emitting it as the first exports is what makes "PATH is always there"
// true on a connection whose account exported nothing at all.
func baselinePairs() []envPair {
	base := workflow.SanitizedBaseline()
	out := make([]envPair, 0, len(base))
	for _, v := range base {
		out = append(out, envPair{name: v.Name, value: v.Value})
	}

	return out
}

// layer applies over on top of baseline, replacing rather than appending a
// name that is already there.
//
// Duplicate-free for replaceOrAppend's reason, and in the same order: the
// shell would honour the last assignment, but a payload that exported one
// name twice would be a payload whose text does not say what the hook gets.
func layer(baseline, over []envPair) []envPair {
	out := make([]envPair, len(baseline), len(baseline)+len(over))
	copy(out, baseline)

	for _, p := range over {
		replaced := false
		for i := range out {
			if out[i].name == p.name {
				out[i] = p
				replaced = true

				break
			}
		}
		if !replaced {
			out = append(out, p)
		}
	}

	return out
}

// replaceOrAppend keeps the block duplicate-free. execve does not promise
// which of two entries with the same name wins, and a variable whose value
// depends on the libc is not a variable a hook can be written against.
func replaceOrAppend(block []string, name, entry string) []string {
	prefix := name + "="
	for i, existing := range block {
		if strings.HasPrefix(existing, prefix) {
			block[i] = entry

			return block
		}
	}

	return append(block, entry)
}

type envPair struct {
	name  string
	value string
}

// parseEnviron validates the entries and drops the four startup variables,
// wherever in the layering they came from.
//
// The drop is here rather than in either encoding because both encodings
// go through this function, and the entries reaching it are the merged,
// RESOLVED environment: a BASH_ENV that an operator configured in
// workflows.environment, or in one backup set's own environment, arrives
// as an ordinary entry with nothing left to say where it came from. The
// baseline is sanitized (SanitizeBaseline) for the same reason at the
// other end, so neither layer can carry one through.
//
// It is a silent drop rather than a refusal: the remote payload's first
// act is to unset these names on the far side, so honouring one here would
// mean emitting an export the envelope immediately contradicts -- and a
// refusal would take a whole deployment's hooks out of service for a
// variable that has never had an effect this product would keep.
func parseEnviron(environ []string) ([]envPair, error) {
	pairs := make([]envPair, 0, len(environ))
	for _, entry := range environ {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("%w: %q has no \"=\" in it, so it is not a NAME=VALUE entry at all", ErrEnvEncoding, redactedEntry(entry))
		}
		if err := validateName(name); err != nil {
			return nil, err
		}
		if err := validateValue(name, value); err != nil {
			return nil, err
		}
		if isStartupVariable(name) {
			continue
		}
		pairs = append(pairs, envPair{name: name, value: value})
	}

	return pairs, nil
}

func validateName(name string) error {
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: an environment variable name contains a NUL byte", ErrEnvEncoding)
	}
	if !envName.MatchString(name) {
		return fmt.Errorf("%w: %q is not a usable environment variable name (it must match %s); a name a shell cannot read back is a variable that exists and does not work",
			ErrEnvEncoding, name, workflow.EnvNamePattern)
	}
	if name == payloadScriptVar {
		// Refused rather than overwritten. The remote encoding needs one
		// shell variable of its own, and an operator's variable that this
		// product silently replaces is a variable that looks like it
		// works -- the same reason workflow.ValidateEnvName refuses the
		// whole BACKUPD_ prefix instead of winning the merge quietly.
		return fmt.Errorf("%w: %s is the one variable name the remote execution envelope uses for the script itself, so it cannot also carry a configured value",
			ErrEnvEncoding, name)
	}

	return nil
}

// validateValue refuses a NUL and nothing else.
//
// A NUL cannot be represented: neither an execve environment block nor a
// shell word can contain one, so the choice is to refuse the value or to
// truncate it silently at the NUL, and a credential silently truncated to
// its first eight characters is the worst of the three possible outcomes.
//
// Newlines, tabs, quotes, backslashes, dollar signs, backticks and any
// UTF-8 are all accepted. They are not dangerous because nothing here ever
// interprets a value: ProcessEnv passes bytes to execve, and StdinPayload
// wraps them in a single-quoted shell literal, inside which the shell
// itself interprets nothing at all.
func validateValue(name, value string) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: the value of %s contains a NUL byte, which neither an environment block nor a shell word can carry; it would be silently truncated there rather than passed",
			ErrEnvEncoding, name)
	}

	return nil
}

// redactedEntry is what a malformed entry is allowed to say about itself.
// A string with no "=" in it cannot be split into a name and a value, so
// there is no way to know whether it is a variable name or a resolved
// secret that lost its name -- and the safe reading of "cannot tell" is to
// print the length rather than the bytes.
func redactedEntry(entry string) string {
	return fmt.Sprintf("[%d bytes]", len(entry))
}
