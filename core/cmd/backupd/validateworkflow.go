package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/internal/workflowlint"
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

	checkWidth := widestColumn(findings, func(f service.WorkflowFinding) string { return f.Check })
	severityWidth := widestColumn(findings, func(f service.WorkflowFinding) string { return f.Severity })

	fmt.Println("  findings:")
	for _, f := range findings {
		fmt.Printf("    %-*s  %-*s  %s%s\n", checkWidth, f.Check, severityWidth, f.Severity, f.Detail, workflowFindingScope(f))
	}
}

// widestColumn is how wide a column has to be to hold every value that
// will go in it.
//
// Extracted rather than written once per printer because this file now
// has two aligned tables over two different row types -- the findings
// list and one script's shell findings -- and both want the same
// property: a vocabulary that grows in core/service or in
// internal/workflowlint lines up here without a format string being
// edited. Generic over the row with the cell as a function, because the
// alternative is an interface every report type would have to implement
// to be printable, which is a lot of ceremony for `len`.
func widestColumn[Row any](rows []Row, cell func(Row) string) int {
	widest := 0
	for _, r := range rows {
		if n := len(cell(r)); n > widest {
			widest = n
		}
	}

	return widest
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
//
// # Why every row is followed by a verification verdict
//
// Because the row says which bytes would run and says nothing about
// whether they are a program. #906 put that answer on the script
// (service.WorkflowValidatedScript.Lint) with a line and a column for
// every finding, and the findings LIST above deliberately carries one
// sentence per script instead of all of them, so this is the only place
// in the text report where an operator reads "BSH003 at 4:3" for the
// hook they are looking at. Printing the rows without it would leave the
// most actionable half of this command's own answer reachable only
// through --json.
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
		printWorkflowScriptVerification(s.Lint)
	}

	printWorkflowVerificationSummary(scripts)
	fmt.Printf("  nothing above was executed: `%s validate workflow` parses scripts, walks each parse tree against this product's own shell rules -- the BSH codes above, which are %s's rules and not ShellCheck's -- and probes reachability, and never runs a hook body\n", cliecho.Binary, cliecho.Binary)
}

// printWorkflowScriptVerification prints what this product's own shell
// verification established about one script's exact bytes, indented under
// the row that names it.
//
// # The three states are printed as three different things
//
// Examined and parsed, examined and REFUSED by the parser, and NOT
// EXAMINED -- a script larger than the verification will read
// (internal/workflowlint.MaxScriptBytes). The third one gets the
// service's reason sentence verbatim and never a tick, for the same
// reason the findings vocabulary has a "skipped" severity: a green line
// about bytes nobody looked at is the one output that would actively
// mislead, and an operator who raised max_script_size_bytes past the
// verification's own ceiling has to be able to see that this is what
// happened.
//
// # Why the blocking findings are printed first
//
// The service reports them in position order, which is the order to READ
// a script in, and it is not the order to act in: one BSH003 at line 40
// is the reason a save would be refused and four style findings above it
// are not. So the error-severity ones lead and each group keeps the
// service's position order, which means the first line under a refused
// script is always the finding that refused it.
//
// # These are this product's rules, and nothing ran
//
// The codes are internal/workflowlint's own BSH namespace: this
// repository's rules, walked over a parse tree in this process. An
// operator who read "shell verification" and assumed ShellCheck would go
// looking for SC codes, a config file and a suppression syntax, none of
// which exist here, so the summary below says whose rules these are
// rather than leaving the codes to be guessed at.
func printWorkflowScriptVerification(lint service.WorkflowScriptLint) {
	const indent = "       "

	switch {
	case !lint.Examined:
		reason := lint.NotExaminedReason
		if reason == "" {
			// The service promises a reason whenever it examined
			// nothing, and there is one path that leaves neither: a
			// plan whose captured bytes could not be re-read, which
			// reports itself as an error finding under the syntax check
			// above. Said as an absence rather than printed as a blank,
			// because a script row with nothing under it reads exactly
			// like a clean one.
			reason = "this validation recorded no verdict for these bytes; the findings list above is where the reason for that is reported"
		}
		fmt.Printf("%snot examined: %s\n", indent, reason)

		return
	case !lint.Parsed:
		// The position is the whole point of this surface: a `bash -n`
		// through a runner could only ever report that a script was
		// refused, and this says where.
		fmt.Printf("%sdoes not parse at %d:%d: %s\n", indent, lint.ParseErrorLine, lint.ParseErrorCol, lint.ParseError)

		return
	case len(lint.Findings) == 0:
		fmt.Printf("%sparses, and this product's own shell rules reported nothing about it\n", indent)

		return
	}

	ordered := workflowLintFindingOrder(lint)
	codeWidth := widestColumn(ordered, func(f service.WorkflowLintFinding) string { return f.Code })
	severityWidth := widestColumn(ordered, func(f service.WorkflowLintFinding) string { return f.Severity })
	positionWidth := widestColumn(ordered, workflowLintPosition)

	for _, f := range ordered {
		fmt.Printf("%s%-*s  %-*s  %-*s  %s\n",
			indent, codeWidth, f.Code, severityWidth, f.Severity,
			positionWidth, workflowLintPosition(f), f.Message)
	}
}

// workflowLintPosition is a finding's place in the script, as one column.
//
// One column rather than two, because line and column are read together
// and always have been: `file:4:3` is the shape every compiler and every
// editor uses, and splitting it would give this report two numeric
// columns an operator has to recombine by eye.
func workflowLintPosition(f service.WorkflowLintFinding) string {
	return fmt.Sprintf("%d:%d", f.Line, f.Col)
}

// workflowLintFindingOrder puts the findings that would refuse a save
// first and leaves every group in the order the service reported it.
//
// The blocking subset comes from WorkflowScriptLint.BlockingLintFindings
// rather than from a severity comparison written here, because the
// threshold that decides what refuses a save is owned by
// internal/workflowlint and a CLI that re-derived it could come to hold a
// second opinion about it -- which would print an error-severity finding
// under a summary saying the save would be accepted.
func workflowLintFindingOrder(lint service.WorkflowScriptLint) []service.WorkflowLintFinding {
	blocking := lint.BlockingLintFindings()
	if len(blocking) == 0 || len(blocking) == len(lint.Findings) {
		return lint.Findings
	}

	ordered := make([]service.WorkflowLintFinding, 0, len(lint.Findings))
	ordered = append(ordered, blocking...)
	for _, f := range lint.Findings {
		if f.Severity != workflowlint.SeverityError {
			ordered = append(ordered, f)
		}
	}

	return ordered
}

// printWorkflowVerificationSummary is where the save gate's threshold is
// stated rather than left to be inferred.
//
// # Why a summary under a report that already printed every finding
//
// Because the per-script lines answer "what is wrong with this hook",
// and an operator who runs this command before editing a workflow is
// asking a different question: "will this be accepted". A set with sixty
// style findings and no errors saves; a set with one BSH003 does not,
// because core/service refuses the save on a parse error or an
// error-severity finding and reports the other three severities without
// blocking on them. Nothing in a column of severities says which of
// those two situations an operator is in, so this counts them and says
// the rule in one sentence.
//
// # Why the counts are of scripts and of findings, separately
//
// A script is what gets refused and a finding is what refuses it, and
// the two numbers answer different questions: "how many of my hooks are
// a problem" is a triage question, "how much is there to fix" is a work
// estimate. Reporting only one of them would leave the other to be
// counted off the screen.
func printWorkflowVerificationSummary(scripts []service.WorkflowValidatedScript) {
	var examined, parsed, blocking, refused int
	for _, s := range scripts {
		if !s.Lint.Examined {
			continue
		}
		examined++
		if s.Lint.Parsed {
			parsed++
		}
		if len(s.Lint.BlockingLintFindings()) > 0 {
			blocking++
			refused++
		} else if !s.Lint.Parsed {
			refused++
		}
	}

	fmt.Printf("  shell verification: %d of %d script(s) examined, %d parse, %d carry an error-severity finding, %d not examined\n",
		examined, len(scripts), parsed, blocking, len(scripts)-examined)
	fmt.Printf("  findings by severity: %s\n", workflowLintSeverityTotals(scripts))
	fmt.Printf("  a parse error or an error-severity finding is what would refuse a workflow save; warning, info and style are reported only, and never block one. %d of the script(s) above would be refused today\n", refused)
}

// workflowLintSeverityTotals counts every finding on every script, by
// severity.
//
// The VALUES are internal/workflowlint's, because it owns the vocabulary
// and a fifth severity added there must not turn into a count this file
// silently drops -- so anything the loop below has never heard of is
// printed after the four rather than skipped. The ORDER is a display
// decision and belongs here: most serious first, so the number that
// decides whether a save is accepted is the first one read.
func workflowLintSeverityTotals(scripts []service.WorkflowValidatedScript) string {
	counts := map[string]int{}
	for _, s := range scripts {
		for _, f := range s.Lint.Findings {
			counts[f.Severity]++
		}
	}

	known := []string{
		workflowlint.SeverityError,
		workflowlint.SeverityWarning,
		workflowlint.SeverityInfo,
		workflowlint.SeverityStyle,
	}

	totals := make([]string, 0, len(known)+len(counts))
	for _, severity := range known {
		totals = append(totals, fmt.Sprintf("%d %s", counts[severity], severity))
		delete(counts, severity)
	}

	rest := make([]string, 0, len(counts))
	for severity, n := range counts {
		rest = append(rest, fmt.Sprintf("%d %s", n, severity))
	}
	slices.Sort(rest)

	return strings.Join(append(totals, rest...), ", ")
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
