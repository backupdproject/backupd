package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/service"
)

// `settings workflow`: the deployment-wide hook configuration, and the
// environment every hook in this deployment is handed (EPIC L, #813).
//
// It sits under `settings` rather than under `workflow` because that is
// where an operator looks for a deployment-wide policy, and because the
// thing it edits is a block of config.yaml exactly as the retention and
// capacity settings beside it are. `workflow` is the noun for RUNS
// (workflow.go's own preamble makes the same split from the other side).
//
// # Why a read exists at all next to config.yaml
//
// The same argument cmdSettings makes, and core/service's
// workflowconfig.go makes it again for this block: the read reports the
// RESOLVED configuration -- the timeout a hook will actually get, the
// default included; whether a bound came from the file or from
// internal/workflow's default; the execution connections a set may point
// at -- and a re-serialization of config.yaml cannot, because the whole
// point of that file is that it omits what it inherits.
//
// # Where a write goes, and the one answer this surface does not have
//
// Through openBackupService with writesConfig, which is `backup-set
// retention`'s door rather than `settings patch`'s: with nothing serving
// this deployment the change is written here and an engine started
// afterwards reads it; with something serving, the write is REFUSED with
// the file untouched, because there is no config watcher and no SIGHUP
// reload in this build, so a change left in the file is one that process
// would never read (#538, #543).
//
// What `settings patch` has and this does not is a ROUTE. PATCH
// /api/v1/settings carries a settings patch to the serving engine, so
// that verb is refused only when no route was named; there is no
// equivalent for this block in this build, so the refusal is
// unconditional beside a serving engine. That is a gap in the API surface
// rather than a decision here, and it is written down so the next person
// meets it as one: when the workflow routes exist on the wire and in
// core/internal/apiclient, moving these writes onto openConfigWriteRoute
// is the same shape #543 already applied to `settings patch`, plus a
// settingsRoute-style interface and the two engineRoute methods behind
// it. Nothing in the verbs below would change.
//
// Announcing the mode is not optional even so, and openBackupService
// does it: an operator has to be able to see from their scroll back
// whether the change they just made reached a running engine or a file it
// has not read (mode.go).
//
// # What a --json WRITE puts on stdout, and why it is not only the object
//
// Two other things land there first, and both are contracts rather than
// noise: the "mode:" line, for the reason above, and FR-23's two startup
// events, which every subcommand that can mutate anything emits
// (logStartup, setup.go). So a script reading a write's --json takes the
// last object on the stream rather than the whole of stdout.
//
// Suppressing either to tidy the stream was the alternative and it is the
// worse trade. The mode line is the only thing that says whether the
// change reached a running engine, and dropping the startup events under
// a flag would make the observability contract depend on how the operator
// asked for their output. The READS have no such problem: they emit no
// events and announce no mode, so their stdout is the object alone.
//
// # The rule that governs every env surface here
//
// A secret is a LOCATION and never a value. `env set` takes a literal
// (--value) or a place to read one from (--secret-file, --secret-env,
// --secret-command), and `env list` prints the name and the place. There
// is no flag anywhere on this surface that takes secret MATERIAL and no
// output anywhere that could print one, which is the same containment
// `medium import-credentials --stdin` has for S3 credentials and the same
// rule core/service's own workflowconfig.go states: nothing on this path
// ever calls internal/secretref, so there is no resolved value in this
// process to leak.

// settingsWorkflowOperand is the word that turns `settings` into this
// command. A const rather than a literal in two files: cmdSettings
// scans its arguments for it and this command checks that it is what it
// was dispatched on, so a rename cannot leave one of the two behind.
const settingsWorkflowOperand = "workflow"

// cmdSettingsWorkflow is every `settings workflow ...` form.
//
// It is reached from cmdSettings, which scans its arguments for the word
// before parsing anything, exactly as cmdBackupSet finds `retention`:
// these flags (--root, --value, --secret-env) are not declared on the
// settings flag set, so parsing them there would fail with "flag provided
// but not defined" before any dispatcher ran.
//
// One flag set for all five forms, the way `medium` declares one for all
// of its verbs: parseFlagsAroundOperands has to know every flag before it
// sees the operands, since a flag may legally be written on either side
// of them. The price is that each form has to refuse the other forms'
// flags rather than silently ignoring them, which is what
// refuseWorkflowFlagsExcept does and what `backup-set patch` already does
// for create's.
func cmdSettingsWorkflow(args []string) int {
	fs, cfgPath := newFlagSet("settings workflow")
	asJSON := fs.Bool("json", false, "emit the resolved workflow configuration as JSON instead of as text")
	patch := declareWorkflowSettingsFlags(fs)
	env := declareWorkflowEnvFlags(fs, "settings workflow env set")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(operands) == 0 || operands[0] != settingsWorkflowOperand {
		// Unreachable through cmdSettings, which only dispatches here
		// when it has seen the word. Kept as a refusal rather than an
		// assumption: a second caller that sliced the word off would
		// otherwise read its own first operand as a verb.
		return usageError(`settings workflow: expected "workflow" as the first argument`)
	}

	rest := operands[1:]
	switch {
	case len(rest) == 0:
		if named := visitedFlags(fs, "config", "json"); len(named) > 0 {
			return usageError("settings workflow: --%s changes nothing on a read; `settings workflow patch --%s ...` is the form that writes",
				strings.Join(named, ", --"), named[0])
		}

		return settingsWorkflowShow(*cfgPath, *asJSON)

	case rest[0] == "patch":
		if len(rest) != 1 {
			return usageError("settings workflow patch takes no argument; %q is one", rest[1])
		}
		if code := refuseWorkflowFlagsExcept(fs, "settings workflow patch", workflowSettingsPatchFlagNames, "config", "json"); code != exitOK {
			return code
		}

		return settingsWorkflowPatch(*cfgPath, fs, patch, *asJSON)

	case rest[0] == "env":
		return workflowEnvVerb(workflowEnvScope{
			noun:        "settings workflow env",
			configPath:  *cfgPath,
			backupSetID: "",
			operands:    rest[1:],
			fs:          fs,
			flags:       env,
			asJSON:      *asJSON,
		})
	}

	return usageError(`settings workflow: %q is not a form of this command; it is "settings workflow", "settings workflow patch", or "settings workflow env list|set NAME|unset NAME"`, rest[0])
}

// workflowSettingsFlags are the deployment-wide patch's own flags.
type workflowSettingsFlags struct {
	root               *string
	beforeDir          *string
	afterDir           *string
	scriptTimeout      *time.Duration
	maxScriptSizeBytes *int64
}

// workflowSettingsPatchFlagNames is every flag that only means something
// under `patch`, shared between the refusal that rejects them elsewhere
// and the builder that reads them, so the two lists cannot drift apart on
// which flags are patch-only.
var workflowSettingsPatchFlagNames = []string{"root", "before-dir", "after-dir", "script-timeout", "max-script-size-bytes"}

func declareWorkflowSettingsFlags(fs *flag.FlagSet) workflowSettingsFlags {
	return workflowSettingsFlags{
		root: fs.String("root", "", "patch only: workflows.root, the one approved absolute directory hook scripts may live under. Clearing it turns workflows off for this deployment"),
		beforeDir: fs.String("before-dir", "",
			"patch only: workflows.global.before_dir, the deployment-wide stage that runs before every backup set's pass. A path under the root; clearing it disables that stage"),
		afterDir: fs.String("after-dir", "",
			"patch only: workflows.global.after_dir, the deployment-wide stage that runs after every backup set's pass. A path under the root; clearing it disables that stage"),
		scriptTimeout: fs.Duration("script-timeout", 0,
			"patch only: workflows.script_timeout, the bound a hook gets when its backup set pins none. Zero clears this deployment's own bound, which leaves hooks on the built-in default rather than on no bound at all"),
		maxScriptSizeBytes: fs.Int64("max-script-size-bytes", 0,
			"patch only: workflows.max_script_size_bytes, the largest one hook script may be. Zero clears it, which leaves the built-in bound in force"),
	}
}

// settingsWorkflowShow is `settings workflow`: the resolved block.
func settingsWorkflowShow(configPath string, asJSON bool) int {
	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, configPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	settings, err := svc.WorkflowSettings(ctx)
	if err != nil {
		return fail(err)
	}

	if asJSON {
		return printWorkflowJSON(settings)
	}
	printWorkflowSettings(settings)

	return 0
}

// settingsWorkflowPatch is `settings workflow patch`.
//
// Every field is read through fs.Visit rather than off the flag's own
// value, which is load bearing on four of the five: an explicitly-passed
// --root="", --before-dir="", --script-timeout 0 or
// --max-script-size-bytes 0 each mean "clear this", and each of those is
// a real operation an operator performs (clearing a stage directory
// disables that stage; clearing a timeout returns hooks to the built-in
// default). A builder that tested for non-zero values would read all four
// as "not mentioned" and silently drop the only change the command line
// asked for, which is buildSettingsPatch's own argument over the identical
// shape.
func settingsWorkflowPatch(configPath string, fs *flag.FlagSet, f workflowSettingsFlags, asJSON bool) int {
	req := service.UpdateWorkflowSettingsRequest{}
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "root":
			req.Root = f.root
		case "before-dir":
			req.BeforeDir = f.beforeDir
		case "after-dir":
			req.AfterDir = f.afterDir
		case "script-timeout":
			req.ScriptTimeout = f.scriptTimeout
		case "max-script-size-bytes":
			req.MaxScriptSizeBytes = f.maxScriptSizeBytes
		}
	})

	// A patch that names nothing is refused here rather than by the
	// service, for cmdSettings' reason: beside a serving engine
	// everything downstream of this point answers with the engine
	// refusal, so an operator who forgot a flag would be sent off to stop
	// a daemon over a missing --root.
	if req.Root == nil && req.BeforeDir == nil && req.AfterDir == nil && req.ScriptTimeout == nil && req.MaxScriptSizeBytes == nil {
		return usageError("settings workflow patch: name at least one of --%s; a patch that changes nothing would rewrite and reload the configuration to no effect",
			strings.Join(workflowSettingsPatchFlagNames, ", --"))
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, configPath, writesConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	settings, err := svc.UpdateWorkflowSettings(ctx, req)
	if err != nil {
		return fail(err)
	}

	if asJSON {
		return printWorkflowJSON(settings)
	}
	printWorkflowSettings(settings)

	return 0
}

// workflowEnvFlags are the four ways `env set` can name a value, and they
// are shared by both scopes: `settings workflow env` and `backup-set
// workflow env` declare the same four and mean the same thing by them.
type workflowEnvFlags struct {
	value         *string
	secretFile    *string
	secretEnv     *string
	secretCommand *stringList
}

// workflowEnvFlagNames is every flag `env set` reads, for the refusals
// that reject them on the forms that do not write a variable.
var workflowEnvFlagNames = []string{"value", "secret-file", "secret-env", "secret-command"}

// declareWorkflowEnvFlags puts the four on fs.
//
// # Why --secret-command repeats rather than taking a quoted string
//
// There are two spellings of a command reference in this binary and this
// takes the stricter one. `repository create --passphrase-command` is
// repeated once per argv word; `medium add --credentials-command` takes
// one string and splits it on spaces. The repeated form is what a
// command that reaches exec directly should use, because an argv
// assembled from separate occurrences cannot be re-split by a quoting
// mistake: `--secret-command "/opt/my helper/get-pw"` under the splitting
// form silently becomes two words and a program that does not exist,
// while under this form it is one word and works. A hook environment is
// also the place where a wrong split is least visible -- the variable
// simply arrives unset, and the hook fails much later for a reason that
// names neither the flag nor the command.
func declareWorkflowEnvFlags(fs *flag.FlagSet, verb string) workflowEnvFlags {
	var command stringList
	fs.Var(&command, "secret-command",
		verb+": a command whose stdout is this variable's value; repeat the flag once per argv word. The command is recorded, never run by this command, and its output is never printed anywhere")

	return workflowEnvFlags{
		value: fs.String("value", "",
			verb+": the literal value, which is not a secret and is stored in config.yaml as written. An explicitly empty --value=\"\" configures the variable as an empty string, which is a different thing from not configuring it"),
		secretFile: fs.String("secret-file", "",
			verb+": the PATH of a file whose contents are this variable's value. A location, never material: nothing here reads the file"),
		secretEnv: fs.String("secret-env", "",
			verb+": the NAME of an environment variable this deployment's own process holds the value in. A name, never a value"),
		secretCommand: &command,
	}
}

// workflowEnvScope is one env verb's whole context: which scope it edits,
// what it was asked to do, and the flags it reads.
//
// A struct rather than eight arguments because both scopes call this with
// the same shape and the empty backupSetID IS the deployment-wide layer,
// which is core/service's own convention (ListWorkflowEnv's doc: "an
// empty backupSetID is the deployment-wide layer. One method for both
// rather than two, because the two are the same operation on two lists").
// Two copies of this dispatcher would be two places the secret-source
// rule has to be kept.
type workflowEnvScope struct {
	// noun is how this scope spells itself in a refusal: "settings
	// workflow env" or "backup-set workflow env".
	noun string

	configPath string

	// backupSetID is the set this edits, or empty for the deployment-wide
	// layer.
	backupSetID string

	// operands is whatever followed the word "env".
	operands []string

	fs     *flag.FlagSet
	flags  workflowEnvFlags
	asJSON bool
}

// workflowEnvVerb dispatches list, set and unset for either scope.
func workflowEnvVerb(s workflowEnvScope) int {
	if len(s.operands) == 0 {
		return usageError("%s needs a verb: list, set NAME or unset NAME", s.noun)
	}

	switch s.operands[0] {
	case "list":
		if len(s.operands) != 1 {
			return usageError("%s list takes no argument; %q is one", s.noun, s.operands[1])
		}
		if code := refuseWorkflowFlagsExcept(s.fs, s.noun+" list", nil, "config", "json"); code != exitOK {
			return code
		}

		return workflowEnvList(s)

	case "set":
		if len(s.operands) != 2 {
			return usageError("%s set takes exactly one argument: the variable NAME", s.noun)
		}
		if code := refuseWorkflowFlagsExcept(s.fs, s.noun+" set", workflowEnvFlagNames, "config", "json"); code != exitOK {
			return code
		}

		return workflowEnvSet(s, s.operands[1])

	case "unset":
		if len(s.operands) != 2 {
			return usageError("%s unset takes exactly one argument: the variable NAME", s.noun)
		}
		if code := refuseWorkflowFlagsExcept(s.fs, s.noun+" unset", nil, "config", "json"); code != exitOK {
			return code
		}

		return workflowEnvUnset(s, s.operands[1])
	}

	return usageError("%s has no %q verb; it has list, set NAME and unset NAME", s.noun, s.operands[0])
}

func workflowEnvList(s workflowEnvScope) int {
	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, s.configPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	vars, err := svc.ListWorkflowEnv(ctx, s.backupSetID)
	if err != nil {
		return fail(err)
	}

	return reportWorkflowEnv(vars, s.asJSON, s.backupSetID)
}

// workflowEnvSet writes one variable.
//
// # Why exactly one source, refused here
//
// core/service refuses two sources as well (validateWorkflowEnvVar), and
// that refusal is the one that protects every surface. This one is about
// the command line: naming none and naming two are both wrong on every
// deployment, so neither needs a journal opened or an engine asked to
// find out, and both would otherwise reach an operator as a service
// refusal after a mode announcement about a daemon they then went and
// stopped.
//
// # Why --value is read through fs.Visit
//
// Because an explicitly empty value is a REQUEST. `--value=""` configures
// the variable as an empty string, which core/service carries as
// HasValue true with an empty Value and config.yaml writes as `value:
// ""`; "nobody passed --value" is the different fact that this variable
// has no literal at all. A builder reading the flag's value would collapse
// the two and turn "set this to empty" into "name no source", refused as a
// mistake the operator did not make.
func workflowEnvSet(s workflowEnvScope, name string) int {
	v := service.WorkflowEnvVar{Name: name}
	sources := 0
	s.fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "value":
			v.Value, v.HasValue = *s.flags.value, true
			sources++
		case "secret-file":
			v.Secret.File = *s.flags.secretFile
			sources++
		case "secret-env":
			v.Secret.Env = *s.flags.secretEnv
			sources++
		case "secret-command":
			v.Secret.Command = *s.flags.secretCommand
			sources++
		}
	})

	if sources != 1 {
		return usageError("%s set %s: name exactly one of --value, --secret-file, --secret-env or --secret-command. A variable is a name and one source: naming none configures nothing, and naming two would be a literal and a secret reference in one entry, which this product refuses to load",
			s.noun, name)
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, s.configPath, writesConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	vars, err := svc.SetWorkflowEnv(ctx, s.backupSetID, v)
	if err != nil {
		return fail(err)
	}

	return reportWorkflowEnv(vars, s.asJSON, s.backupSetID)
}

func workflowEnvUnset(s workflowEnvScope, name string) int {
	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, s.configPath, writesConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	vars, err := svc.UnsetWorkflowEnv(ctx, s.backupSetID, name)
	if err != nil {
		return fail(err)
	}

	return reportWorkflowEnv(vars, s.asJSON, s.backupSetID)
}

// reportWorkflowEnv prints one scope's entries, in either form.
func reportWorkflowEnv(vars []service.WorkflowEnvVar, asJSON bool, backupSetID string) int {
	if asJSON {
		if vars == nil {
			vars = []service.WorkflowEnvVar{}
		}

		return printWorkflowJSON(struct {
			BackupSetID string                   `json:"backup_set_id"`
			Environment []service.WorkflowEnvVar `json:"environment"`
		}{backupSetID, vars})
	}

	if len(vars) == 0 {
		fmt.Println("no workflow environment variable is configured in this scope")

		return 0
	}
	for _, v := range vars {
		printWorkflowEnvVar(v)
	}

	return 0
}

// printWorkflowEnvVar is one entry: the name, and where the value comes
// from.
//
// # What this prints for a secret, and what it structurally cannot
//
// It prints the LOCATION: the file path, the variable name, or the argv
// of the command. It cannot print a resolved value because there is none
// to print anywhere on this path -- internal/secretref is called by
// internal/workflow at the moment a hook is about to run, inside the
// engine, and neither this package nor core/service's workflow config
// surfaces ever call it. So this is not a redaction that somebody has to
// remember to apply; there is no value in this process to redact.
//
// A literal IS printed, and the distinction is the point of the two
// spellings below. `--value` is for things that are not secrets (a
// PGDATABASE, a LANG, a flag a script reads) and is stored in config.yaml
// as written, so printing it back reports the file. Anything that must
// not be in the file is one of the three references, and this is where an
// operator sees which of the two they configured.
func printWorkflowEnvVar(v service.WorkflowEnvVar) {
	switch {
	case !v.Secret.IsZero():
		fmt.Printf("%-28s %s\n", v.Name, workflowSecretLocation(v.Secret))
		fmt.Println("    the value is read from that location when a hook runs, by the process running it, and is never reported by any surface of this product")
	case v.HasValue && v.Value == "":
		// Said in words rather than printed as an empty column, because
		// `NAME  ""` and `NAME` a line apart are indistinguishable in a
		// terminal, and the difference between them is exactly what
		// HasValue exists to carry.
		fmt.Printf("%-28s (configured as an empty string)\n", v.Name)
	case v.HasValue:
		fmt.Printf("%-28s %s\n", v.Name, v.Value)
	default:
		// Not reachable through a configuration this product will load:
		// config.Validate refuses an entry with neither a literal nor a
		// reference. Printed rather than skipped, because a row nobody
		// can explain is better than a row nobody can see.
		fmt.Printf("%-28s (no value and no secret reference configured, which this product refuses to load)\n", v.Name)
	}
}

// workflowSecretLocation renders where a value comes from, in the
// spelling the flag that set it uses, so the output reads back as the
// command that would reproduce it.
func workflowSecretLocation(s service.WorkflowSecretRef) string {
	switch {
	case s.File != "":
		return "secret from file " + s.File
	case s.Env != "":
		return "secret from environment variable " + s.Env
	case len(s.Command) > 0:
		// The argv as configured, joined for reading. It is a program and
		// its arguments, which is a location in the same sense a path is:
		// what it PRINTS is the secret, and nothing here runs it.
		return "secret from the output of: " + strings.Join(s.Command, " ")
	}

	return "no secret reference"
}

// printWorkflowSettings renders the RESOLVED deployment-wide block.
func printWorkflowSettings(s service.WorkflowSettings) {
	if !s.Configured {
		fmt.Println("this deployment configures no workflow root, so it runs no hook scripts at all")
		fmt.Printf("  set one with `%s settings workflow patch --root /path/to/workflows`; until then every backup set's pass runs exactly as it did before hooks existed\n", cliecho.Binary)
	} else {
		fmt.Printf("workflow root: %s\n", s.Root)
	}
	fmt.Printf("  before stage: %s\n", workflowStageDir(s.BeforeDir))
	fmt.Printf("  after stage:  %s\n", workflowStageDir(s.AfterDir))
	// The resolved bound and where it came from, because "we chose five
	// minutes" and "nobody has chosen" are different facts about a
	// deployment and only one of them is worth changing.
	if s.ScriptTimeoutConfigured {
		fmt.Printf("  script timeout: %s (configured)\n", s.ScriptTimeout)
	} else {
		fmt.Printf("  script timeout: %s (the built-in default; this deployment configures none)\n", s.ScriptTimeout)
	}
	fmt.Printf("  max script size: %d bytes\n", s.MaxScriptSizeBytes)

	if len(s.ExecConnections) == 0 {
		fmt.Println("  execution connections: none declared, so a remote hook can only run over its own backup set's source connection")
	} else {
		fmt.Printf("  execution connections: %s\n", strings.Join(s.ExecConnections, ", "))
	}

	// The runner's two paths are reported and are not writable here
	// (service.WorkflowRunnerSettings' own doc): they are how THIS
	// process sees the host, they differ between a container and a
	// bare-metal install of the same deployment, and the installer writes
	// them.
	if s.Runner.Configured {
		fmt.Printf("  host runner socket: %s\n", s.Runner.Socket)
		fmt.Printf("  host runner credential: %s\n", s.Runner.TokenFile)
	} else {
		fmt.Printf("  host runner: not configured, so a NAME.local.sh hook has nothing to run on. `%s workflow-runner status` is how to check one that should be there\n", cliecho.Binary)
	}

	if len(s.Environment) == 0 {
		fmt.Println("  environment: nothing configured deployment-wide")

		return
	}
	fmt.Println("  environment:")
	for _, v := range s.Environment {
		fmt.Print("  ")
		printWorkflowEnvVar(v)
	}
}

// workflowStageDir renders a stage directory, saying what an empty one
// means rather than printing a blank column: an unset stage is a stage
// that does not run, which is a configuration decision and not a missing
// value.
func workflowStageDir(dir string) string {
	if dir == "" {
		return "(none, so this stage runs nothing)"
	}

	return dir
}

// visitedFlags returns the names of the flags actually passed, minus the
// ones the caller says are always allowed.
//
// fs.Visit rather than comparing values, for buildSettingsPatch's reason:
// a flag passed as its own zero value has still been passed, and on these
// surfaces an explicit zero is usually the whole point of the command
// line.
func visitedFlags(fs *flag.FlagSet, allowed ...string) []string {
	var named []string
	fs.Visit(func(fl *flag.Flag) {
		if contains(allowed, fl.Name) {
			return
		}
		named = append(named, fl.Name)
	})

	return named
}

// refuseWorkflowFlagsExcept returns a usage exit code when the command
// line carried a flag that belongs to a different form of this command.
//
// Refusing rather than ignoring, which is the rule every verb in this
// binary that shares a flag set follows (refuseFlagsOfTheOtherVerb,
// refuseEveryFlagBut) and the reason each of them exists: `settings
// workflow env list --value x` exiting 0 having listed the environment
// would be a command that did something other than what it was told, and
// the operator would believe they had set a variable.
func refuseWorkflowFlagsExcept(fs *flag.FlagSet, form string, mine []string, alwaysAllowed ...string) int {
	allowed := append(append([]string(nil), alwaysAllowed...), mine...)
	named := visitedFlags(fs, allowed...)
	if len(named) == 0 {
		return exitOK
	}

	return usageError("%s: --%s is not a flag of this form, and ignoring it would report success for something that did not happen",
		form, strings.Join(named, ", --"))
}
