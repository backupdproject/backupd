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

// `backup-set workflow`: one backup set's hook configuration, resolved
// against the deployment's (EPIC L, #813).
//
// # Why the read is worth a verb of its own
//
// Because inheritance is where operators get surprised, and a set's own
// block says nothing about what its hooks will actually meet. This
// reports both halves: what THIS SET pins, and what a hook of this set
// will get -- the stages it will run in execution order, the timeout that
// wins, and the names of every variable it will be handed after the
// deployment's layer, this set's layer and the sanitized baseline have
// been merged. None of that is readable off config.yaml, because the
// whole point of the file is that it omits what it inherits.
//
// # Where a write goes
//
// The same door `settings workflow patch` uses and for the same reasons:
// openBackupService with writesConfig, so a write happens here when
// nothing is serving this deployment and is REFUSED with the file
// untouched when something is. settingsworkflow.go's preamble holds the
// argument, including what it would take to give these writes a route to
// a serving engine later.
//
// # The set id, and why it is checked before anything opens
//
// A backup set id is exactly source/name (splitBackupSetID's rule), and a
// value that is not one names nothing on any deployment: pasting an
// artifact id in, which has the file name still on the end of it, is
// wrong wherever it is typed. So it is refused at exit 2 before the
// configuration is read, the same line `fetch --backup-set` and
// `unconfigured clear` draw (#569, and usage()'s own exit-code table).

// cmdBackupSetWorkflow is every `backup-set workflow ...` form.
//
// It is dispatched from backupSetVerbs with the WHOLE argument list, its
// own verb included, and finds that verb as its first operand -- the
// convention that table's doc spells out, and what lets a flag be written
// on either side of the verb so `backup-set --config X workflow a/b`
// works exactly as `settings --config X patch` already does.
func cmdBackupSetWorkflow(args []string) int {
	fs, cfgPath := newFlagSet("backup-set workflow")
	asJSON := fs.Bool("json", false, "emit this backup set's resolved workflow configuration as JSON instead of as text")
	patch := declareBackupSetWorkflowFlags(fs)
	env := declareWorkflowEnvFlags(fs, "backup-set workflow env set")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(operands) == 0 || operands[0] != "workflow" {
		// Unreachable through cmdBackupSet, which dispatches here only
		// because it saw the word. A refusal rather than an assumption,
		// for cmdSettingsWorkflow's reason.
		return usageError(`backup-set workflow: expected "workflow" as the first argument`)
	}

	rest := operands[1:]
	if len(rest) == 0 {
		return usageError(`backup-set workflow: expected "workflow <source/backup-set>", "workflow patch <source/backup-set>" or "workflow env <source/backup-set> list|set NAME|unset NAME"`)
	}

	switch rest[0] {
	case "patch":
		if len(rest) != 2 {
			return usageError("backup-set workflow patch takes exactly one argument: <source/backup-set>")
		}
		id, code := workflowBackupSetID("backup-set workflow patch", rest[1])
		if code != exitOK {
			return code
		}
		if code := refuseWorkflowFlagsExcept(fs, "backup-set workflow patch", backupSetWorkflowPatchFlagNames, "config", "json"); code != exitOK {
			return code
		}

		return backupSetWorkflowPatch(*cfgPath, id, fs, patch, *asJSON)

	case "env":
		if len(rest) < 2 {
			return usageError("backup-set workflow env: expected the backup set first, as \"env <source/backup-set> list|set NAME|unset NAME\"")
		}
		id, code := workflowBackupSetID("backup-set workflow env", rest[1])
		if code != exitOK {
			return code
		}

		return workflowEnvVerb(workflowEnvScope{
			noun:        "backup-set workflow env",
			configPath:  *cfgPath,
			backupSetID: id,
			operands:    rest[2:],
			fs:          fs,
			flags:       env,
			asJSON:      *asJSON,
		})
	}

	// The remaining form is the read, which takes the id and nothing
	// else. A second operand here is a verb this command does not have
	// (`workflow show a/b` is the shape somebody would try), so the
	// refusal names the forms rather than complaining about arity.
	if len(rest) != 1 {
		return usageError(`backup-set workflow: %q is not a form of this command; it is "workflow <source/backup-set>", "workflow patch <source/backup-set>" or "workflow env <source/backup-set> list|set NAME|unset NAME"`, rest[0])
	}
	id, code := workflowBackupSetID("backup-set workflow", rest[0])
	if code != exitOK {
		return code
	}
	if named := visitedFlags(fs, "config", "json"); len(named) > 0 {
		return usageError("backup-set workflow: --%s changes nothing on a read; `backup-set workflow patch %s --%s ...` is the form that writes",
			strings.Join(named, ", --"), id, named[0])
	}

	return backupSetWorkflowShow(*cfgPath, id, *asJSON)
}

// workflowBackupSetID refuses a value that is not a backup set id,
// naming the form that was being typed.
//
// The shape rule is splitBackupSetID's and is not re-implemented: a value
// with no separator, two of them or an empty half names nothing, and
// letting it through would produce a not-found about a set that was never
// an id at all.
func workflowBackupSetID(form, raw string) (string, int) {
	if _, _, ok := splitBackupSetID(raw); !ok {
		return "", usageError("%s: %q is not a backup set id; a backup set id is exactly source/name", form, raw)
	}

	return raw, exitOK
}

// backupSetWorkflowFlags are the per-set patch's own flags.
type backupSetWorkflowFlags struct {
	beforeDir      *string
	afterDir       *string
	scriptTimeout  *time.Duration
	execConnection *string
}

// backupSetWorkflowPatchFlagNames is every flag that only means something
// under `patch`, so the refusal that rejects them on the read and the
// builder that reads them cannot disagree about which they are.
var backupSetWorkflowPatchFlagNames = []string{"before-dir", "after-dir", "script-timeout", "exec-connection"}

func declareBackupSetWorkflowFlags(fs *flag.FlagSet) backupSetWorkflowFlags {
	return backupSetWorkflowFlags{
		beforeDir: fs.String("before-dir", "",
			"patch only: this set's own before stage, a directory under the deployment's workflow root. Clearing it stops this set running a before stage of its own; the deployment's still runs"),
		afterDir: fs.String("after-dir", "",
			"patch only: this set's own after stage, a directory under the deployment's workflow root. Clearing it stops this set running an after stage of its own"),
		scriptTimeout: fs.Duration("script-timeout", 0,
			"patch only: the bound this set's hooks get, pinned for this set. Zero clears the pin, which returns the set to the deployment's bound rather than to no bound"),
		execConnection: fs.String("exec-connection", "",
			"patch only: the declared workflow execution connection this set's NAME.remote.sh hooks run over. Clearing it returns the set to running remote hooks over its own source connection"),
	}
}

// backupSetWorkflowShow is `backup-set workflow <source/backup-set>`.
func backupSetWorkflowShow(configPath, id string, asJSON bool) int {
	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, configPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	set, err := svc.BackupSetWorkflow(ctx, id)
	if err != nil {
		return fail(err)
	}

	if asJSON {
		return printWorkflowJSON(set)
	}
	printBackupSetWorkflow(set, workflowRootOf(ctx, svc))

	return 0
}

// backupSetWorkflowPatch is `backup-set workflow patch
// <source/backup-set>`.
//
// Every field is read through fs.Visit for settingsWorkflowPatch's
// reason, and one of them makes the case on its own:
// --script-timeout 0 CLEARS this set's pin and returns it to the
// deployment's bound, which is a thing operators do when they have
// finished debugging one slow hook. Read off the flag's value instead,
// that command line would mean "not mentioned" and the pin would survive
// a command that reported success for removing it.
func backupSetWorkflowPatch(configPath, id string, fs *flag.FlagSet, f backupSetWorkflowFlags, asJSON bool) int {
	req := service.UpdateBackupSetWorkflowRequest{}
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "before-dir":
			req.BeforeDir = f.beforeDir
		case "after-dir":
			req.AfterDir = f.afterDir
		case "script-timeout":
			req.ScriptTimeout = f.scriptTimeout
		case "exec-connection":
			req.RemoteExecConnectionRef = f.execConnection
		}
	})

	if req.BeforeDir == nil && req.AfterDir == nil && req.ScriptTimeout == nil && req.RemoteExecConnectionRef == nil {
		return usageError("backup-set workflow patch %s: name at least one of --%s; a patch that changes nothing would rewrite and reload the configuration to no effect",
			id, strings.Join(backupSetWorkflowPatchFlagNames, ", --"))
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, configPath, writesConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	set, err := svc.UpdateBackupSetWorkflow(ctx, id, req)
	if err != nil {
		return fail(err)
	}

	if asJSON {
		return printWorkflowJSON(set)
	}
	printBackupSetWorkflow(set, workflowRootOf(ctx, svc))

	return 0
}

// workflowRootOf reads the deployment's approved root, for the one line
// that makes every stage directory below it legible.
//
// A stage directory is configured RELATIVE to the root
// (workflow.Root.ResolveStage is what joins the two), and
// service.BackupSetWorkflow carries the stages without it, so a report
// that printed "global-before" and stopped would be naming a directory
// an operator cannot go and look at. It is a second read rather than a
// field because the root belongs to the deployment rather than to this
// set, and it costs an in-memory configuration read: WorkflowSettings
// opens nothing.
//
// An error is reported as no root rather than failing the command. The
// answer an operator asked for is this set's configuration, and refusing
// to print it because a second, decorative read failed would be the
// wrong trade -- the same one backupSetRemoveWith makes about its count.
func workflowRootOf(ctx context.Context, svc *service.BackupService) string {
	settings, err := svc.WorkflowSettings(ctx)
	if err != nil {
		return ""
	}

	return settings.Root
}

// printBackupSetWorkflow renders one set's RESOLVED configuration.
func printBackupSetWorkflow(s service.BackupSetWorkflow, root string) {
	fmt.Printf("workflow for %s\n", s.BackupSetID)
	if root != "" {
		fmt.Printf("  workflow root: %s (every stage directory below is relative to it)\n", root)
	}
	if s.Configured {
		fmt.Println("  this set has a workflow block of its own")
		fmt.Printf("  before stage: %s\n", workflowStageDir(s.BeforeDir))
		fmt.Printf("  after stage:  %s\n", workflowStageDir(s.AfterDir))
	} else {
		// Said explicitly, because "this set configures nothing" and
		// "this set runs no hooks" are different facts: a set with no
		// block of its own still runs the deployment's global stages,
		// which is why the stage list below is the answer and this line
		// is only half of it.
		fmt.Println("  this set has no workflow block of its own, so it runs the deployment's stages and nothing else")
	}

	// Both timeouts, because an operator changing the deployment's
	// default needs to know which sets are pinned and which will follow
	// (service.BackupSetWorkflow's own doc for the pair).
	if s.ScriptTimeout > 0 {
		fmt.Printf("  script timeout: %s (pinned by this set)\n", s.ScriptTimeout)
	} else {
		fmt.Printf("  script timeout: %s (inherited; this set pins none)\n", s.EffectiveScriptTimeout)
	}

	if s.RemoteExecConnectionRef != "" {
		fmt.Printf("  remote hooks run over: %s\n", s.RemoteExecConnectionRef)
	} else {
		fmt.Println("  remote hooks run over: this set's own source connection, since it names no execution connection")
	}

	if len(s.Stages) == 0 {
		fmt.Println("  stages: none, so a pass over this set runs exactly as it would with hooks switched off")
	} else {
		fmt.Println("  stages, in execution order:")
		for _, st := range s.Stages {
			fmt.Printf("    %-10s %-7s %s\n", st.Scope, st.Phase, st.Dir)
		}
	}

	// NAMES, never values, and not because of a filter: the merged view
	// is what config.Validate already resolved and core/service reports
	// as names for the standing reason that a variable's value may be a
	// secret reference somebody would then expect this to resolve.
	if len(s.ResolvedEnvironmentNames) == 0 {
		fmt.Println("  environment a hook of this set will have: nothing configured in either layer")
	} else {
		fmt.Printf("  environment a hook of this set will have: %s\n", strings.Join(s.ResolvedEnvironmentNames, ", "))
	}

	if len(s.Environment) == 0 {
		fmt.Printf("  this set configures no environment of its own; `%s backup-set workflow env %s set NAME --value V` adds one\n", cliecho.Binary, s.BackupSetID)

		return
	}
	fmt.Println("  this set's own environment:")
	for _, v := range s.Environment {
		fmt.Print("  ")
		printWorkflowEnvVar(v)
	}
}
