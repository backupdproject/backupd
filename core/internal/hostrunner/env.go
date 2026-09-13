package hostrunner

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The environment a hook is given, and the four names that are deleted
// from it no matter who asked for them.
//
// internal/workflow decides WHAT the environment contains: the sanitized
// baseline, the deployment layer, the backup-set layer, and this
// product's BACKUPD_* built-ins, merged with precedence already applied
// (env.go over there argues every one of those). By the time a block
// reaches this package it is a finished list, and this file does exactly
// two things to it: it refuses entries that cannot survive an execve, and
// it deletes the four variables that make bash execute something the plan
// never captured.
//
// # The four
//
// BASH_ENV names a file bash SOURCES before running a non-interactive
// script. ENV is the same thing for sh-mode. SHELLOPTS and BASHOPTS are
// read-only in a running shell but are honoured FROM THE ENVIRONMENT at
// startup, so `SHELLOPTS=xtrace` or `BASHOPTS=expand_aliases` changes how
// every captured byte is interpreted before the first line of it runs.
//
// All four are ordinary variable names by internal/workflow's rules:
// they are not BACKUPD_-prefixed, so ValidateEnvName accepts them, and an
// operator can put BASH_ENV in workflows.environment today. That is the
// case this deletion exists for. It is not defending against a hostile
// engine -- the engine is this product -- it is defending against a
// configuration file in which one line silently prepends a script to
// every hook in the deployment, including the ones somebody else wrote.
//
// Deleting rather than refusing, deliberately. A refusal would fail the
// whole run for a variable the operator probably set for their own
// interactive shell and copied in by accident, and the remedy ("take it
// out") is exactly what deleting does. The runner reports the names it
// dropped in its status vocabulary rather than silently; see
// DroppedEnvNames.
//
// # Why cmd.Env and not `export`
//
// The block is handed to the process through execve. It is never rendered
// as shell text, which is the other way a runner could do this and the
// way that breaks: a value containing a single quote, a newline, a
// backslash or a `$(` is a value that either corrupts the script or
// executes. There is no quoting function that is worth trusting with a
// database password. core/internal/remoteexec cannot use execve over SSH
// and carries the quoting problem in one audited place for that reason;
// this side simply does not have it.

// CursedEnvNames are the four variables deleted from every hook's
// environment. See this file's preamble.
//
// A function rather than an exported slice, for internal/workflow's
// reason: a package-level slice is writable by every importer, and this
// one is a security rule.
func CursedEnvNames() []string {
	return []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS"}
}

func isCursed(name string) bool {
	for _, c := range CursedEnvNames() {
		if name == c {
			return true
		}
	}
	return false
}

// DefaultPath is the PATH a hook gets if the environment it arrived with
// declares none.
//
// It is the same fixed list internal/workflow's SanitizedBaseline and
// internal/secretref's command resolver use, and it is here as a floor
// rather than as a default anybody should rely on: a hook with no PATH at
// all cannot invoke `cat`, and the resulting "command not found" is a
// failure whose cause is three layers away from where it is read.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// envName is the same POSIX-portable rule internal/workflow holds
// configuration to. It is restated rather than imported because this
// package validates what arrived OVER A SOCKET, which is a different
// trust question from validating a config file: the check has to happen
// here whether or not the sender did it.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ErrEnv is every refusal about an environment entry that reached this
// package.
var ErrEnv = errors.New("hostrunner: this hook environment cannot be used")

// EnvVar is one entry of a hook's environment: a name and a resolved
// value.
//
// The value is MATERIAL here, unlike internal/workflow's EnvVar, which
// carries a secret's location. This is the last hop before execve, so it
// has to be. Nothing in this package logs, echoes or persists a value:
// the only place one goes is the block handed to the child process, and
// String below is what keeps a stray %v from undoing that.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// String renders the NAME only. A hook's environment is the single most
// likely thing to be printed while somebody is debugging a failing step,
// and it routinely holds a repository passphrase.
func (v EnvVar) String() string { return "hostrunner.EnvVar{" + v.Name + "}" }

// GoString makes %#v as safe as %v, for obs.Secret's reason.
func (v EnvVar) GoString() string { return v.String() }

// EnvSet is a hook's whole environment in precedence order: later entries
// win.
//
// Order is preserved rather than sorted because it is the sender's
// statement of precedence (global, then per-set, then BACKUPD_*), and
// re-sorting it here would be this package forming a second opinion about
// a decision internal/workflow already made.
type EnvSet struct {
	Vars []EnvVar `json:"vars"`
}

// String renders the names only, for EnvVar.String's reason.
func (s EnvSet) String() string {
	names := make([]string, 0, len(s.Vars))
	for _, v := range s.Vars {
		names = append(names, v.Name)
	}
	return "hostrunner.EnvSet{" + strings.Join(names, " ") + "}"
}

// GoString makes %#v as safe as %v.
func (s EnvSet) GoString() string { return s.String() }

// ValidateEnvName reports whether a name can be an environment variable
// at all.
func ValidateEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: an environment entry with no name", ErrEnv)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: an environment name contains a NUL byte, which cannot survive execve", ErrEnv)
	}
	if !envName.MatchString(name) {
		return fmt.Errorf("%w: %q is not a portable shell variable name (%s), and a variable a script cannot read is worse than one that is absent", ErrEnv, name, EnvNamePattern)
	}
	return nil
}

// EnvNamePattern is envName as a documented string, for the ADR and for
// error messages.
const EnvNamePattern = `^[A-Za-z_][A-Za-z0-9_]*$`

// ValidateEnvValue reports whether a value can be carried.
//
// A NUL is the only refusal. An environment block is NUL-terminated
// strings, so a value containing one is silently TRUNCATED at that byte
// by execve -- a password that ends up half a password, with no error
// anywhere. Newlines, tabs, quotes, backslashes and arbitrary non-UTF-8
// bytes are all fine and are deliberately allowed: they are legal in an
// environment value, and a runner that refused them would be refusing
// real credentials.
func ValidateEnvValue(name, value string) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%w: the value of %s contains a NUL byte, and execve would silently truncate it there rather than fail", ErrEnv, name)
	}
	return nil
}

// ProcessEnv builds the block handed to execve.
//
// The steps, in order, each of which is load-bearing:
//
//  1. every entry is validated, so a malformed block is a refusal before
//     anything is created rather than a surprise at exec time;
//  2. later entries replace earlier ones by name, which is how the
//     sender's precedence is applied;
//  3. the four cursed names are deleted, whoever set them;
//  4. PATH is given DefaultPath if nothing declared one;
//  5. the result is sorted by name.
//
// The sort is for reproducibility rather than for correctness: order in
// an environment block is meaningless to execve, and a stable rendering
// is what lets a test assert on the whole block and a diagnostic be
// compared between two runs.
//
// The baseline argument is the environment this process would otherwise
// pass on, and passing os.Environ() here is exactly what callers must NOT
// do -- it is nil in every real call. It exists so a test can prove the
// sanitization runs over an inherited block too, and so the one place
// that could ever want inheritance has to say so out loud.
func (s EnvSet) ProcessEnv(baseline []string) ([]string, error) {
	values := map[string]string{}
	order := []string{}

	add := func(name, value string) {
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = value
	}

	for _, entry := range baseline {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("%w: the baseline entry %q has no '='", ErrEnv, name)
		}
		if err := ValidateEnvName(name); err != nil {
			return nil, err
		}
		if err := ValidateEnvValue(name, value); err != nil {
			return nil, err
		}
		add(name, value)
	}

	for _, v := range s.Vars {
		if err := ValidateEnvName(v.Name); err != nil {
			return nil, err
		}
		if err := ValidateEnvValue(v.Name, v.Value); err != nil {
			return nil, err
		}
		add(v.Name, v.Value)
	}

	for _, cursed := range CursedEnvNames() {
		delete(values, cursed)
	}
	if _, ok := values["PATH"]; !ok {
		add("PATH", DefaultPath)
	}

	names := make([]string, 0, len(values))
	for _, name := range order {
		if _, ok := values[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	block := make([]string, 0, len(names))
	for _, name := range names {
		block = append(block, name+"="+values[name])
	}
	return block, nil
}

// DroppedEnvNames reports which of the cursed four this set declared, so
// the runner can say what it removed instead of removing it silently.
//
// Silence is the failure mode worth spending a function on: an operator
// who set BASH_ENV on purpose and finds their preamble is not running has
// no way to discover why, and the answer -- "this product deletes it" --
// is not guessable from anything they can see.
func (s EnvSet) DroppedEnvNames() []string {
	var dropped []string
	for _, v := range s.Vars {
		if isCursed(v.Name) {
			dropped = append(dropped, v.Name)
		}
	}
	sort.Strings(dropped)
	return dropped
}
