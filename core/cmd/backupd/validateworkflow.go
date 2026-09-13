package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/service"
)

// `validate workflow <source/backup-set>`: everything this product can
// find out about a backup set's hooks without running one (EPIC L, #813).
//
// # Why it is a form of `validate` rather than a verb of `workflow`
//
// Because it answers the question `validate` already answers, about a
// different subject. An operator who has typed `backupd validate
// <artifact>` to ask "is this still sound" asks "are my hooks sound" the
// same way, and a second noun would mean learning that this product calls
// one of those checking and the other one something else. cmdValidate
// therefore takes `workflow` as its first operand, the way `settings`
// takes `patch`.
//
// # The exit code is the verdict, and there are two verdicts
//
// Exit 0 when the hooks are sound, 1 when they are not, so this is usable
// in the monitoring shape `check` and `repository health` already are.
// What it must not do is fold the SET's own soundness into that: a backup
// set whose source connects, whose destination is writable and whose
// retention is sound, with a hook directory somebody has not created yet,
// is a set whose backups are fine and whose hooks are broken. #813
// requires that combination to be visibly sayable and
// core/service.WorkflowValidation carries both answers for it, so this
// prints a line of its own the moment they disagree. Reporting "this
// backup set is broken" there would tell an operator their backups are
// failing when they are not.
//
// # Nothing here executes a hook
//
// Not once, not behind a flag. core/service's ValidateWorkflow is where
// that rule lives and it is worth restating at the surface an operator
// types: a hook is arbitrary code somebody wrote to quiesce a database,
// and a validation that ran it would be a command you type to check your
// configuration that stops production. The only two things that reach an
// interpreter are `bash -n`, which parses and never executes, and this
// product's own fixed capability probe.

// validateWorkflowOperand is the word that turns `validate` into this
// command.
//
// A const rather than a literal because cmdValidate has to match on it
// BEFORE it parses an artifact id, and the two live in different files:
// a rename that reached only one of them would leave `validate workflow
// a/b` being refused by app.ParseArtifactID with a sentence about
// artifact ids, which is a refusal about the wrong thing and the exact
// mistake this operand exists to avoid.
const validateWorkflowOperand = "workflow"

// validateWorkflow is `validate workflow <source/backup-set>`.
func validateWorkflow(configPath, id string, asJSON bool) int {
	if _, _, ok := splitBackupSetID(id); !ok {
		return usageError("validate workflow: %q is not a backup set id; a backup set id is exactly source/name", id)
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, configPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	report, err := svc.ValidateWorkflow(ctx, id)
	if err != nil {
		return fail(err)
	}

	if asJSON {
		// The whole report, exactly as the service assembled it: every
		// finding including the skipped ones, every script, and both
		// verdicts. A --json that filtered would be a second, quieter
		// opinion about which checks matter.
		if code := printWorkflowJSON(report); code != 0 {
			return code
		}
	} else {
		printWorkflowValidation(report)
	}

	if !report.WorkflowValid {
		return 1
	}

	return 0
}

// printWorkflowValidation renders one report: the verdicts, every
// finding, and the resolved script list.
func printWorkflowValidation(r service.WorkflowValidation) {
	fmt.Printf("workflow validation for %s\n", r.BackupSetID)
	if !r.Configured {
		fmt.Println("  this backup set runs no hook scripts, so there is nothing here to be wrong")
	}
	if r.Root != "" {
		fmt.Printf("  workflow root: %s\n", r.Root)
	}
	fmt.Printf("  valid for backup: %v\n", r.ValidForBackup)
	fmt.Printf("  workflow valid:   %v\n", r.WorkflowValid)

	// The line #813 asks for by name. It is printed only when the two
	// verdicts disagree in this direction, because that is the one
	// combination an operator will otherwise misread: every other pairing
	// is already obvious from the two lines above, and this one looks
	// like "my backups are broken" unless somebody says otherwise.
	if r.ValidForBackup && !r.WorkflowValid {
		fmt.Println("  this backup set is sound and its hooks are not: backups of it will be refused for as long as its workflow cannot be snapshotted, and nothing below is a fault in the backup set itself")
	}

	printWorkflowFindings(r.Findings)
	printWorkflowValidatedScripts(r.Stages, r.Scripts)
}

// printWorkflowFindings prints every finding as CHECK SEVERITY detail, in
// aligned columns.
//
// # Why every finding and not only the failures
//
// Because a skipped check is a claim this product is careful not to make.
// A deployment with no remote hooks has nothing to say about its
// execution capability, and printing nothing there is indistinguishable
// from a check that passed -- which is precisely what an operator
// debugging a remote hook that does not run would misread. The severity
// vocabulary has four values for that reason
// (core/service's own severity block) and this prints all four.
//
// The widths are computed rather than hard-coded, so a check id added in
// core/service lines up here without this file being edited: the ids are
// a closed vocabulary the service exports (service.WorkflowChecks) and
// nothing about their length belongs in a format string.
func printWorkflowFindings(findings []service.WorkflowFinding) {
	if len(findings) == 0 {
		// Not reachable through ValidateWorkflow, which always reports
		// every check. Kept as a hole rather than deleted: a build that
		// got here printed a report with no checks in it at all, and
		// silence would be that arriving unnoticed.
		fmt.Println("  no check reported anything, which should be impossible: a validation reports every check, skipped ones included")

		return
	}

	checkWidth, severityWidth := 0, 0
	for _, f := range findings {
		if len(f.Check) > checkWidth {
			checkWidth = len(f.Check)
		}
		if len(f.Severity) > severityWidth {
			severityWidth = len(f.Severity)
		}
	}

	fmt.Println("  findings:")
	for _, f := range findings {
		fmt.Printf("    %-*s  %-*s  %s%s\n", checkWidth, f.Check, severityWidth, f.Severity, f.Detail, workflowFindingScope(f))
	}
}

// workflowFindingScope names the step a per-script finding belongs to, or
// nothing for a finding about the set.
//
// It is appended to the detail rather than given columns of its own,
// because most findings carry none of it and four empty columns on every
// line would push the sentence an operator is reading off the screen.
func workflowFindingScope(f service.WorkflowFinding) string {
	var parts []string
	if f.Script != "" {
		parts = append(parts, f.Script)
	}
	if f.Scope != "" {
		parts = append(parts, f.Scope)
	}
	if f.Phase != "" {
		parts = append(parts, f.Phase)
	}
	if f.Target != "" {
		parts = append(parts, f.Target)
	}
	if len(parts) == 0 {
		return ""
	}

	return " [" + strings.Join(parts, " ") + "]"
}

// printWorkflowValidatedScripts prints the plan this set would execute:
// the stages, then every script in plan order with everything a run would
// pin about it.
//
// # Why the hash is printed, and why only a prefix of it
//
// The sha256 is what a run records and what a recovery re-verifies
// against (service.WorkflowValidatedScript.SHA256's own doc), so it is
// the value an operator compares when they are asking whether two
// deployments are running the same hooks, or whether the script on disk
// is still the one an interrupted run captured. A prefix, because the
// comparison people actually make is by eye and sixty-four hex characters
// per line would wrap every terminal; the full value is in --json, which
// is what a script would compare with.
func printWorkflowValidatedScripts(stages []service.WorkflowStage, scripts []service.WorkflowValidatedScript) {
	if len(stages) > 0 {
		fmt.Println("  stages, in execution order:")
		for _, st := range stages {
			fmt.Printf("    %-10s %-7s %s\n", st.Scope, st.Phase, st.Dir)
		}
	}

	if len(scripts) == 0 {
		fmt.Println("  scripts: none discovered, so a run of this set would execute no hook")

		return
	}

	fmt.Println("  scripts, in plan order:")
	for _, s := range scripts {
		fmt.Printf("    %2d %-28s %-10s %-7s %-7s %-10s %s\n",
			s.Order, s.ScriptName, s.Scope, s.Phase, s.Target,
			workflowHashPrefix(s.SHA256),
			(time.Duration(s.TimeoutMillis) * time.Millisecond).String())
		if s.ExecutionConnectionRef != "" {
			fmt.Printf("       over: %s\n", s.ExecutionConnectionRef)
		}
	}
	fmt.Printf("  nothing above was executed: `%s validate workflow` parses scripts and probes reachability, and never runs a hook body\n", cliecho.Binary)
}

// workflowHashPrefix is how much of a sha256 goes in a column.
//
// Twelve hex characters: long enough that two scripts in one deployment
// colliding by accident is not a thing that happens, short enough to sit
// in a line beside a script name and a timeout. An empty hash is said to
// be missing rather than printed as an empty column, because it means the
// bytes were never captured -- a snapshot that failed before this script
// -- and that is a fact rather than a blank.
func workflowHashPrefix(sha string) string {
	const prefix = 12
	if sha == "" {
		return "(not hashed)"
	}
	if len(sha) <= prefix {
		return sha
	}

	return sha[:prefix]
}
