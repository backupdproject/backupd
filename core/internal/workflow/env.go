package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/backupdproject/backupd/core/internal/obs"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// The environment a hook script runs with: what an operator may put in it,
// what this product puts in it, and the one asymmetry between them.
//
// # Why there is a baseline at all
//
// A hook does NOT inherit this daemon's environment. That is the decision
// everything else here follows from. This process's environment carries
// whatever the init system, the container runtime and the operator's shell
// put there -- including, on a deployment using the env secret resolver,
// the repository passphrase itself (secretref.Ref.Env). Handing that block
// to a script an operator dropped into a directory would make every hook a
// credential dump, and it would do it silently.
//
// So the environment is BUILT, from a minimal baseline this package
// declares, plus exactly what configuration asked for, plus this product's
// own built-ins. Nothing leaks in. internal/secretref's fromCommand takes
// the same position for the same reason, with the same one-variable
// baseline.
//
// # Precedence, and the one layer that is not negotiable
//
// sanitized baseline < workflows.environment < backup-set environment <
// BACKUPD_* built-ins.
//
// The first three are ordinary: a more specific configuration wins, which
// is the rule every other per-set override in this product follows. The
// fourth is different in kind. The built-ins are not a layer an operator
// competes with, they are the run's own facts, and a hook that reads
// BACKUPD_BACKUP_STATUS has to be reading what this product observed --
// not a value somebody wrote into a config file, and not one left over
// from an earlier phase. So the whole BACKUPD_ prefix is REFUSED in
// configuration (not overridden at merge time, refused at validation
// time), because a key an operator can write and this product silently
// discards is a key that looks like it works.
//
// # Secrets
//
// A value is either a literal or a reference to a secret, never a pasted
// secret: config.Passphrase, config.MediumCredentials and
// config.KeyEncryption all take the same position, and secretref's package
// doc argues it. A resolved value exists only inside a Resolved, only for
// as long as a step is running, and is wrapped in obs.Secret so that
// logging it, formatting it or serialising it produces "[REDACTED]"
// instead.
//
// Nothing resolved is ever part of a Plan, a plan hash, a spool file or a
// journal row. That is not enforced by care at each call site: Plan simply
// has no field that could hold one, and Environment carries
// secretref.Ref, which is a location.

// envName is the key rule: a POSIX-portable shell variable name.
//
// Anything else is refused rather than mangled. A name with a dash in it
// can be put into an environment block by execve and then cannot be read
// by any shell script that receives it, which is a variable that exists
// and does not work -- the worst of the three possible outcomes.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvNamePattern is envName as a documented string, for the ADR and the
// operator docs. Call ValidateEnvName rather than matching it yourself.
const EnvNamePattern = `^[A-Za-z_][A-Za-z0-9_]*$`

// ReservedEnvPrefix is the namespace this product injects and an operator
// may not configure.
const ReservedEnvPrefix = "BACKUPD_"

// ReservedEnvName is the bare name this product also sets (to "1", so a
// script can tell it is running under this product at all), and which is
// reserved for the same reason as the prefix.
const ReservedEnvName = "BACKUPD"

// ErrEnvName is every refusal about an environment entry's NAME: the
// shape, the reservation, or a duplicate within one layer. One sentinel
// for the class, with the specific sentence beside it, for ErrScriptName's
// reason.
var ErrEnvName = errors.New("workflow: this is not a valid workflow environment variable name")

// ErrEnvValue is every refusal about an entry's VALUE: a NUL byte, or a
// literal and a secret reference declared at once.
var ErrEnvValue = errors.New("workflow: this workflow environment value cannot be used")

// The three built-ins whose values this package cannot know, named as
// constants because a CALLER has to spell them.
//
// Everything else in builtinEnvNames is set from a fact the run itself
// holds -- its id, its phase, the step's name, the three statuses -- and
// is written by internal/workflowrun from the run in front of it. These
// three are answers only the deployment's configuration has: which host
// the source is on, which path is being backed up, and where the bytes
// land. They arrive through workflowrun.RunRequest.Facts, keyed by name,
// and a key that is not one of these is refused rather than passed
// through.
//
// Constants rather than three string literals at the one call site that
// fills them in, because a typo there is silent: the fact is dropped, the
// variable is exported empty, and `umount "$BACKUPD_SOURCE_PATH"`
// unmounts nothing and exits 0.
const (
	EnvSourceHost  = "BACKUPD_SOURCE_HOST"
	EnvSourcePath  = "BACKUPD_SOURCE_PATH"
	EnvDestination = "BACKUPD_DESTINATION"
)

// builtinEnvNames is the full set of variables this product injects, in
// the order they are documented.
//
// The list is exhaustive and it is checked against, not merely described:
// ValidateEnvName refuses the whole BACKUPD_ prefix, so a built-in added
// here later cannot collide with a key an operator already wrote. The
// reason to enumerate them anyway is that the enumeration IS the contract
// a hook author writes against, and a variable that is set but not listed
// is one nobody can rely on.
var builtinEnvNames = []string{
	ReservedEnvName,
	"BACKUPD_RUN_ID",
	"BACKUPD_BACKUP_SET_ID",
	"BACKUPD_BACKUP_SET_NAME",
	"BACKUPD_PHASE",
	"BACKUPD_STEP_ID",
	"BACKUPD_STEP_NAME",
	"BACKUPD_STEP_TARGET",
	EnvSourceHost,
	EnvSourcePath,
	EnvDestination,
	"BACKUPD_WORK_DIR",
	"BACKUPD_BACKUP_STATUS",
	"BACKUPD_WORKFLOW_STATUS",
	"BACKUPD_CLEANUP_STATUS",
	"BACKUPD_BACKUP_ERROR_CODE",
	"BACKUPD_STARTED_AT",
	"BACKUPD_RECOVERY",
	"BACKUPD_CLEANUP_REASON",
}

// BuiltinEnvNames returns every variable this product injects into a
// hook's environment, in documented order.
//
// A function rather than an exported slice, for StepStates' reason: a
// package-level slice is writable by every importer.
func BuiltinEnvNames() []string { return append([]string(nil), builtinEnvNames...) }

// IsReservedEnvName reports whether name belongs to this product rather
// than to an operator.
//
// The test is the PREFIX plus the bare name, not membership of
// builtinEnvNames, and the difference matters: reserving only the names
// that exist today would let an operator configure BACKUPD_SOMETHING_NEW,
// which then silently stops working the release this product starts
// setting it.
func IsReservedEnvName(name string) bool {
	return name == ReservedEnvName || strings.HasPrefix(name, ReservedEnvPrefix)
}

// ValidateEnvName is the one place a name an operator wrote is judged.
// internal/config calls it, so the config file and this package cannot
// drift into two rules.
func ValidateEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: an environment variable must have a name", ErrEnvName)
	}

	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: an environment variable name must not contain a NUL byte", ErrEnvName)
	}

	if !envName.MatchString(name) {
		return fmt.Errorf(
			"%w: %q must match %s. A name outside that rule can be placed in an environment block and then cannot be read by any shell script that receives it, so it is refused rather than passed through",
			ErrEnvName, name, EnvNamePattern)
	}

	if IsReservedEnvName(name) {
		return fmt.Errorf(
			"%w: %q is reserved. Every %s* variable is set by this product from the run it belongs to, and a hook reading %s has to be reading what this product observed rather than a value from a config file. Choose a name outside the %s namespace",
			ErrEnvName, name, ReservedEnvPrefix, "BACKUPD_BACKUP_STATUS", ReservedEnvPrefix)
	}

	return nil
}

// EnvVar is one configured environment entry: a name, and either a
// literal value or a reference to a secret.
//
// The two are mutually exclusive and the empty literal is a real value, so
// "which one is this" is decided by Secret being zero rather than by Value
// being empty. An operator who writes `FOO: ""` means an empty FOO, and a
// type that could not represent that would be a type that silently dropped
// the variable.
type EnvVar struct {
	Name string

	// Value is the literal, passed through with no shell evaluation of any
	// kind. "$HOME" is four characters, not a path: a config file that
	// expanded variables would be a config file whose meaning depends on
	// this daemon's own environment, which is the thing the sanitized
	// baseline exists to sever.
	Value string

	// Secret names WHERE the value comes from, when it is not a literal.
	// It is the same file/env/command triple as everywhere else in this
	// product, and there is deliberately no fourth field to paste
	// material into.
	Secret secretref.Ref
}

// IsSecret reports whether this entry's value comes from a secret
// reference.
func (e EnvVar) IsSecret() bool { return !e.Secret.IsZero() }

// Validate reports the first way this entry is unusable.
func (e EnvVar) Validate() error {
	if err := ValidateEnvName(e.Name); err != nil {
		return err
	}

	if strings.ContainsRune(e.Value, 0) {
		return fmt.Errorf(
			"%w: the value of %s contains a NUL byte, which cannot be carried in an environment block at all: execve would truncate the value there, so the variable would exist with content nobody wrote",
			ErrEnvValue, e.Name)
	}

	if e.IsSecret() {
		if e.Value != "" {
			return fmt.Errorf(
				"%w: %s declares both a literal value and a secret reference. Choosing one would mean an operator who added a secret while leaving the old literal behind gets whichever this product happened to prefer, silently, for as long as both keep working",
				ErrEnvValue, e.Name)
		}

		if err := e.Secret.Validate(); err != nil {
			return fmt.Errorf("%w: the secret reference for %s: %w", ErrEnvValue, e.Name, err)
		}
	}

	return nil
}

// Environment is a resolved, ordered, duplicate-free set of configured
// entries: the merge of the baseline and every configuration layer, with
// precedence already applied.
//
// It is a value type with an unexported slice so that a merged environment
// cannot be edited after the precedence rules ran. The order is by name,
// which is what makes a plan hash over it deterministic; execution order
// of variables is meaningless in an environment block.
type Environment struct {
	vars []EnvVar
}

// NewEnvironment merges configuration layers in precedence order --
// earliest layer weakest -- validates every entry, and returns the result
// sorted by name.
//
// A duplicate name WITHIN one layer is refused rather than resolved,
// because there is no rule for choosing between two entries an operator
// wrote at the same level; a name in a later layer replaces the same name
// in an earlier one, which is the whole point of the layering.
//
// It does not resolve anything. No file is opened, no command is run, no
// environment variable is read: this is shape, exactly as
// config.Validate's own doc promises about the config file, and for the
// same reason -- a configuration that validated on one machine and not on
// another would be worse than one that failed everywhere.
func NewEnvironment(layers ...[]EnvVar) (Environment, error) {
	merged := map[string]EnvVar{}
	order := []string{}

	for _, layer := range layers {
		seen := map[string]bool{}

		for _, v := range layer {
			if err := v.Validate(); err != nil {
				return Environment{}, err
			}

			if seen[v.Name] {
				return Environment{}, fmt.Errorf(
					"%w: %s is declared twice in the same environment block, and there is no rule for choosing between them",
					ErrEnvName, v.Name)
			}
			seen[v.Name] = true

			if _, already := merged[v.Name]; !already {
				order = append(order, v.Name)
			}
			merged[v.Name] = v.clone()
		}
	}

	sort.Strings(order)

	out := make([]EnvVar, 0, len(order))
	for _, name := range order {
		out = append(out, merged[name])
	}

	return Environment{vars: out}, nil
}

// Vars returns the merged entries in name order. It copies, because an
// Environment that a caller could edit is one whose plan hash stops
// describing it.
//
// The copy is DEEP, and the depth is the whole reason this is not one
// append. An EnvVar is a value type with one slice in it -- a
// command-sourced secret's argv -- and a shallow copy shares that slice's
// backing array with the caller. So this:
//
//	argv := []string{"vault", "read", "secret/db"}
//	env, _ := NewEnvironment([]EnvVar{{Name: "PW", Secret: secretref.Ref{Command: argv}}})
//	plan, _ := Snapshot(...)
//	argv[2] = "secret/root"
//
// would change what the plan resolves at execution time, after the plan
// was hashed, journaled and declared immutable: the hash still describes
// secret/db and the run reads secret/root. Cloning on the way in
// (NewEnvironment) and on the way out (here) is what makes "immutable
// after snapshot" a property of the value rather than a request to the
// caller.
func (e Environment) Vars() []EnvVar {
	out := make([]EnvVar, 0, len(e.vars))
	for _, v := range e.vars {
		out = append(out, v.clone())
	}

	return out
}

// clone returns this entry with its secret's argv copied rather than
// shared. See Environment.Vars for why.
func (e EnvVar) clone() EnvVar {
	if len(e.Secret.Command) != 0 {
		e.Secret.Command = append([]string(nil), e.Secret.Command...)
	}

	return e
}

// Names returns the merged entry names in order.
func (e Environment) Names() []string {
	out := make([]string, 0, len(e.vars))
	for _, v := range e.vars {
		out = append(out, v.Name)
	}

	return out
}

// SanitizedBaseline is the environment every hook starts from, before any
// configuration is applied.
//
// One variable, deliberately. PATH has to be there or a script cannot
// invoke anything without an absolute path, and the value is the same
// fixed list internal/secretref's command resolver uses, for the same
// reason: a resolver -- or a hook -- is meant to be self-sufficient rather
// than handed ambient state this daemon holds for something unrelated.
//
// Anything an operator's script needs beyond this is something they
// declare, in workflows.environment or on the backup set, where it is
// visible in the config file and in the run's plan. That is the trade:
// slightly more to write, and a hook whose inputs are auditable.
func SanitizedBaseline() []EnvVar {
	return []EnvVar{{Name: "PATH", Value: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}}
}

// Resolved is one step's fully assembled environment: every configured
// entry with its secrets resolved, plus this product's built-ins.
//
// It exists only for the duration of a step. It is never persisted, never
// part of a Plan and never part of a plan hash, and its secret values are
// obs.Secret so that a %v, a log field, a JSON body or a fmt.Sprintf of
// this value produces "[REDACTED]". Environ is the only method that
// returns material, and its only legitimate caller is the code that is
// about to hand it to an exec.
type Resolved struct {
	// names is in assembly order: sorted configured names, then sorted
	// built-in names. Two lists rather than one sorted merge, because a
	// reader of a plan or a log wants to see what was configured
	// separately from what this product added.
	names []string

	literals map[string]string
	secrets  map[string]obs.Secret
}

// Resolve assembles the environment for one step: the configured entries
// with every secret reference resolved through the product's existing
// custody rules, then the built-ins over the top.
//
// The built-ins win unconditionally and are not validated against
// ValidateEnvName's reservation rule -- they are the thing it reserves.
// They ARE checked for being known built-ins, so a caller cannot smuggle
// an arbitrary variable in through this argument: a typo'd
// BACKUPD_BAKCUP_STATUS would otherwise become part of a hook's contract
// and stay there.
//
// A secret that will not resolve is a refusal, not an empty value. A hook
// handed an empty credential fails somewhere far away from the reason, and
// "the backup did not run because this secret could not be read" is the
// only honest report.
func (e Environment) Resolve(ctx context.Context, builtins map[string]string) (Resolved, error) {
	r := Resolved{
		literals: make(map[string]string, len(e.vars)+len(builtins)),
		secrets:  make(map[string]obs.Secret),
	}

	for _, v := range e.vars {
		if !v.IsSecret() {
			r.names = append(r.names, v.Name)
			r.literals[v.Name] = v.Value

			continue
		}

		secret, err := secretref.Resolve(ctx, v.Secret)
		if err != nil {
			// err names the SOURCE and nothing from inside it: that is
			// secretref's own contract, and it is why this can be
			// wrapped without a custody review.
			return Resolved{}, fmt.Errorf("workflow: resolving the secret behind environment variable %s: %w", v.Name, err)
		}

		r.names = append(r.names, v.Name)
		r.secrets[v.Name] = secret
	}

	builtinOrder := make([]string, 0, len(builtins))
	for name := range builtins {
		if !slices.Contains(builtinEnvNames, name) {
			return Resolved{}, fmt.Errorf(
				"%w: %q is not one of this product's built-in variables, and only built-ins may be injected over a configured environment. Add it to the documented set or configure it as an ordinary variable",
				ErrEnvName, name)
		}

		if strings.ContainsRune(builtins[name], 0) {
			return Resolved{}, fmt.Errorf("%w: the built-in %s contains a NUL byte", ErrEnvValue, name)
		}

		builtinOrder = append(builtinOrder, name)
	}
	sort.Strings(builtinOrder)

	for _, name := range builtinOrder {
		if _, shadowed := r.literals[name]; !shadowed {
			if _, shadowedSecret := r.secrets[name]; !shadowedSecret {
				r.names = append(r.names, name)
			}
		}

		// A built-in wins over anything with the same name. Nothing
		// should be able to reach this state -- ValidateEnvName refuses
		// the prefix -- and the assignment is unconditional anyway,
		// because "the built-ins win" must be true by construction
		// rather than by every configuration path having checked.
		delete(r.secrets, name)
		r.literals[name] = builtins[name]
	}

	return r, nil
}

// Names returns every variable in this environment, configured ones first
// in name order, then the built-ins in name order.
func (r Resolved) Names() []string { return append([]string(nil), r.names...) }

// Environ returns the environment block, as exec expects it.
//
// This is the one method that returns secret material, and it exists for
// exactly one caller: the code that is about to pass it to a process. It
// is named after os/exec's own field so that a grep for who reads secrets
// out of this type is a grep for one word.
func (r Resolved) Environ() []string {
	out := make([]string, 0, len(r.names))

	for _, name := range r.names {
		if s, ok := r.secrets[name]; ok {
			out = append(out, name+"="+s.Reveal())

			continue
		}

		out = append(out, name+"="+r.literals[name])
	}

	return out
}

// SecretValues returns the resolved secret material, for one purpose: the
// run's redaction set (obs.Redactor), so that a hook which prints its own
// credential to stderr does not get that credential written into a log or
// a journal detail.
//
// It is deliberately not the same method as Environ. A caller that needs
// the redaction set does not need the environment block, and vice versa,
// and keeping them apart keeps the audit of "who touches secret material"
// to two short answers.
func (r Resolved) SecretValues() []string {
	out := make([]string, 0, len(r.secrets))
	for _, name := range r.names {
		if s, ok := r.secrets[name]; ok {
			out = append(out, s.Reveal())
		}
	}

	return out
}

// String renders the variable NAMES and nothing else.
//
// Not the values, including the literal ones. A literal an operator wrote
// into a config file is not secret by declaration, but it is routinely a
// database name, a hostname or a path, and this type is the one thing a
// step's diagnostics are most likely to print; a rendering that is safe
// only until somebody configures the wrong variable is not safe.
func (r Resolved) String() string {
	return "workflow.Resolved{" + strings.Join(r.names, " ") + "}"
}

// GoString makes %#v as safe as %v. obs.Secret takes the same precaution
// for the same reason: a debug format verb is exactly what somebody
// reaches for when a step is misbehaving.
func (r Resolved) GoString() string { return r.String() }
