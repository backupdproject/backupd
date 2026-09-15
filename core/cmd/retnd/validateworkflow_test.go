package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/service"
)

// `validate workflow <source/backup-set>`: the report, the exit code, and
// the one sentence #813 asks for by name (the set is sound and its hooks
// are not).

// TestValidateWorkflowReportsEveryCheckAndExitsOnTheVerdict is the happy
// path: a deployment with a root, two empty stage directories and no
// hooks yet, which is where every deployment is on the day somebody turns
// this feature on.
//
// Two things are asserted and the second is the one that earns its place.
// Every check reports, skipped ones included, because a check that did
// not run is not a check that passed and an operator debugging a remote
// hook needs "skipped: this set has no remote scripts" rather than
// silence. And the exit code is the verdict, because that is what makes
// this usable in the monitoring shape `check` and `repository health`
// already are.
func TestValidateWorkflowReportsEveryCheckAndExitsOnTheVerdict(t *testing.T) {
	configPath, _ := workflowFixture(t)

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"validate", "workflow", "production/postgres-primary", "--config", configPath})
	})

	for _, check := range service.WorkflowChecks() {
		if !strings.Contains(out, check) {
			t.Errorf("the report does not mention the %q check at all, so an operator cannot tell whether it passed, failed or was never run:\n%s", check, out)
		}
	}
	if !strings.Contains(out, "valid for backup:") || !strings.Contains(out, "workflow valid:") {
		t.Fatalf("the report does not carry both verdicts:\n%s", out)
	}

	// The exit code has to agree with the verdict it printed, whichever
	// way the verdict went on this machine: a validation that reports
	// unsound hooks and exits 0 is a monitoring surface that never fires.
	sound := strings.Contains(out, "workflow valid:   true")
	switch {
	case sound && code != 0:
		t.Errorf("the report says the hooks are sound and the command exited %d", code)
	case !sound && code != 1:
		t.Errorf("the report says the hooks are not sound and the command exited %d, want 1:\n%s", code, out)
	}
}

// TestValidateWorkflowSaysWhenTheSetIsSoundAndItsHooksAreNot is the
// combination #813 requires to be visibly sayable, driven by the way it
// actually happens: a stage directory somebody has configured and not
// created.
//
// The set itself is untouched -- same source, same destination, same
// retention -- so a report that folded the two verdicts together would
// tell an operator their backups are broken when what is broken is a
// directory. That is the failure this test exists to keep out, and the
// line it looks for is the one that makes the distinction readable rather
// than leaving it to two booleans an operator has to interpret.
func TestValidateWorkflowSaysWhenTheSetIsSoundAndItsHooksAreNot(t *testing.T) {
	configPath, root := workflowFixture(t)

	// A configured stage that is not there. Snapshot refuses it, which is
	// what a real run would do, so this is the same refusal a backup of
	// this set would meet tonight.
	if code := run([]string{"settings", "workflow", "patch", "--before-dir", "not-created-yet", "--config", configPath}); code != 0 {
		t.Fatalf("configuring a missing stage directory = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(root, "not-created-yet")); !os.IsNotExist(err) {
		t.Fatalf("the fixture's missing stage directory exists after all (Stat err = %v)", err)
	}

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"validate", "workflow", "production/postgres-primary", "--config", configPath})
	})
	if code != 1 {
		t.Fatalf("a set whose hooks cannot be snapshotted exited %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "valid for backup: true") {
		t.Fatalf("this test needs a set that is sound on its own, and the fixture reported otherwise:\n%s", out)
	}
	if !strings.Contains(out, "this backup set is sound and its hooks are not") {
		t.Errorf("the report leaves the two verdicts to be interpreted from two booleans; #813 requires the combination to be said:\n%s", out)
	}
	if !strings.Contains(out, "resolved_directories") {
		t.Errorf("the failing check is not named in the report:\n%s", out)
	}
}

// TestValidateWorkflowJSONCarriesTheWholeReport pins what a script binds
// to.
//
// The whole report and not a summary: both verdicts, every finding
// including the skipped ones, and the resolved script list. A --json that
// filtered would be a second, quieter opinion about which checks matter,
// and the UI reading this surface has to be able to group by check id.
func TestValidateWorkflowJSONCarriesTheWholeReport(t *testing.T) {
	configPath, _ := workflowFixture(t)

	out := captureStdout(t, func() {
		// The exit code is the verdict and is not what this test is
		// about; it is asserted next door.
		run([]string{"validate", "workflow", "production/postgres-primary", "--config", configPath, "--json"})
	})

	var report service.WorkflowValidation
	if err := json.Unmarshal([]byte(jsonObjectIn(t, out)), &report); err != nil {
		t.Fatalf("--json did not emit a decodable report: %v\n%s", err, out)
	}
	if report.BackupSetID != "production/postgres-primary" {
		t.Errorf("the report names backup set %q", report.BackupSetID)
	}

	// One finding per check at least, and possibly several: a check that
	// is per script reports once per script (a syntax check over three
	// hooks is three answers), so this asserts COVERAGE rather than a
	// count. What must not happen is a check that goes unreported, which
	// is a surface claiming nothing about something it looked at.
	reported := map[string]bool{}
	for _, f := range report.Findings {
		reported[f.Check] = true
	}
	for _, check := range service.WorkflowChecks() {
		if !reported[check] {
			t.Errorf("--json carries no finding for the %q check, so a client grouping by check id has a silent hole in it", check)
		}
	}

	// The findings have to be identifiable by their closed vocabulary
	// rather than by their sentence, which is the whole reason the check
	// ids are values and the details are prose.
	known := map[string]bool{}
	for _, check := range service.WorkflowChecks() {
		known[check] = true
	}
	for _, f := range report.Findings {
		if !known[f.Check] {
			t.Errorf("the report carries a finding for check %q, which core/service does not declare", f.Check)
		}
		if f.Severity == "" {
			t.Errorf("the finding for %q carries no severity, so nothing can tell a pass from a skip", f.Check)
		}
	}
}

// workflowLintReport is a report carrying one of each verification
// state, assembled by hand.
//
// By hand rather than by validating a fixture, because the four states
// this surface has to keep distinguishable are not all reachable from a
// deployment a unit test can build: "not examined" needs a script larger
// than internal/workflowlint will read (a megabyte of hook per test run),
// and a parse error with a position needs the service's parser to have
// been asked. What is under test here is the RENDERING -- that an
// operator can tell a clean script from a refused one from one nobody
// looked at -- and the service's own tests own the question of which
// state it reports for which bytes.
func workflowLintReport() service.WorkflowValidation {
	return service.WorkflowValidation{
		BackupSetID:    "production/postgres-primary",
		ValidForBackup: true,
		WorkflowValid:  false,
		Configured:     true,
		Root:           "/srv/backupd/workflows",
		Stages: []service.WorkflowStage{
			{Scope: "global", Phase: "before", Dir: "/srv/backupd/workflows/global-before"},
		},
		Scripts: []service.WorkflowValidatedScript{{
			StepID: "s1", Order: 1, ScriptName: "10-quiesce.local.sh",
			Scope: "global", Phase: "before", Target: "local",
			SHA256: strings.Repeat("a", 64), TimeoutMillis: 30000,
			Lint: service.WorkflowScriptLint{Examined: true, Parsed: true},
		}, {
			StepID: "s2", Order: 2, ScriptName: "20-unterminated.local.sh",
			Scope: "global", Phase: "before", Target: "local",
			SHA256: strings.Repeat("b", 64), TimeoutMillis: 30000,
			Lint: service.WorkflowScriptLint{
				Examined:       true,
				Parsed:         false,
				ParseError:     "reached EOF without closing quote '",
				ParseErrorLine: 3,
				ParseErrorCol:  11,
			},
		}, {
			StepID: "s3", Order: 3, ScriptName: "30-prune.local.sh",
			Scope: "global", Phase: "before", Target: "local",
			SHA256: strings.Repeat("c", 64), TimeoutMillis: 30000,
			Lint: service.WorkflowScriptLint{
				Examined: true,
				Parsed:   true,
				// Position order, the way the service reports them: the
				// warning is at line 2 and the error at line 9.
				Findings: []service.WorkflowLintFinding{{
					Code: "BSH002", Severity: "warning", Line: 2, Col: 1,
					Message: "cd $STAGING is not guarded, so every command after it runs in whatever directory this script started in",
				}, {
					Code: "BSH003", Severity: "error", Line: 9, Col: 3,
					Message: "rm -rf $STAGING/ deletes / when STAGING is empty or unset",
				}},
			},
		}, {
			StepID: "s4", Order: 4, ScriptName: "40-generated.local.sh",
			Scope: "global", Phase: "before", Target: "local",
			SHA256: strings.Repeat("d", 64), TimeoutMillis: 30000,
			Lint: service.WorkflowScriptLint{
				NotExaminedReason: "this script is larger than the 1 MiB this verification will read, so nothing in it was checked",
			},
		}},
		Findings: []service.WorkflowFinding{{
			Check:    service.WorkflowCheckLocalBashSyntax,
			Severity: service.WorkflowSeverityError,
			Detail:   "20-unterminated.local.sh is not a shell program",
			Script:   "20-unterminated.local.sh",
		}},
	}
}

// TestValidateWorkflowPrintsEachScriptsShellVerification is what #906
// adds to this report, driven through the real printer.
//
// The assertions are about what an operator can ACT on, which for a
// shell finding is three things and not one: the code, because that is
// the value they grep and the UI groups by; the position, because
// "BSH003 somewhere in this script" is not a place anybody can go to;
// and the sentence, because the code alone says nothing about what is
// wrong. A report that printed two of the three would look complete and
// be unusable.
//
// Whole lines are deliberately not asserted. The columns are computed
// from the values present (widestColumn), so pinning the spacing would
// make a rule id of a different length a failing test about nothing.
func TestValidateWorkflowPrintsEachScriptsShellVerification(t *testing.T) {
	out := captureStdout(t, func() { printWorkflowValidation(workflowLintReport()) })

	for _, want := range []string{
		// The clean script says so, rather than saying nothing: silence
		// under a row is indistinguishable from a row nobody checked.
		"parses, and this product's own shell rules reported nothing about it",

		// The parse error, with the position `bash -n` through a runner
		// could never report.
		"does not parse at 3:11: reached EOF without closing quote",

		// Both findings, both severities, both positions, both
		// sentences.
		"BSH003", "error", "9:3", "rm -rf $STAGING/ deletes / when STAGING is empty or unset",
		"BSH002", "warning", "2:1", "cd $STAGING is not guarded",

		// The reason the bytes were not read, verbatim from the service.
		"this script is larger than the 1 MiB this verification will read, so nothing in it was checked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q, so an operator cannot act on it from the CLI:\n%s", want, out)
		}
	}

	// The blocking finding leads its script's block, because one BSH003
	// is the reason a save would be refused and the warning above it in
	// position order is not.
	if strings.Index(out, "BSH003") > strings.Index(out, "BSH002") {
		t.Errorf("the error-severity finding is printed after the warning, so the finding that would refuse a save is not the first thing read under the script:\n%s", out)
	}

	// The not-examined script must not be described as anything having
	// passed. This is the same rule the "skipped" severity exists for: a
	// green line about bytes nobody looked at is the one output that
	// would actively mislead.
	//
	// The block is cut at the summary, which is allowed to say the word
	// "parse" about the report as a whole: what must not appear is a
	// verdict about THESE bytes.
	notExamined := out[strings.Index(out, "40-generated.local.sh"):]
	notExamined = notExamined[:strings.Index(notExamined, "  shell verification:")]
	for _, forbidden := range []string{"reported nothing", "parses", " ok", "clean"} {
		if strings.Contains(notExamined, forbidden) {
			t.Errorf("the block for the script that was never read contains %q, which claims a verification nobody performed:\n%s", forbidden, notExamined)
		}
	}
	if !strings.Contains(notExamined, "not examined:") {
		t.Errorf("the script that was never read is not reported as not examined:\n%s", notExamined)
	}
}

// TestValidateWorkflowSummarisesTheSaveGateThreshold pins the sentence an
// operator reads before they edit a workflow.
//
// The per-script blocks answer "what is wrong with this hook". This
// answers the different question somebody about to save is actually
// asking -- "will this be accepted" -- and the answer is not derivable
// from a severity column: sixty style findings save and one BSH003 does
// not. So the counts and the threshold are both asserted, because a
// summary that counted findings without saying which of them refuses a
// save would leave the rule to be guessed.
func TestValidateWorkflowSummarisesTheSaveGateThreshold(t *testing.T) {
	out := captureStdout(t, func() { printWorkflowValidation(workflowLintReport()) })

	for _, want := range []string{
		// Three of the four scripts were read, two of those parse, one
		// carries the blocking finding, one was never read.
		"3 of 4 script(s) examined",
		"2 parse",
		"1 carry an error-severity finding",
		"1 not examined",

		// Every severity totalled, including the ones with nothing in
		// them: a severity that is absent from the line is
		// indistinguishable from a severity nobody counted.
		"1 error", "1 warning", "0 info", "0 style",

		// The threshold, said rather than implied, and the count of
		// scripts it would refuse (the unparseable one and the one with
		// the BSH003).
		"a parse error or an error-severity finding is what would refuse a workflow save",
		"2 of the script(s) above would be refused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, out)
		}
	}

	// Whose rules these are, and that nothing ran. An operator who read
	// "shell verification" and assumed ShellCheck would go looking for SC
	// codes, a config file and a suppression syntax, none of which exist
	// here.
	if !strings.Contains(out, "not ShellCheck's") {
		t.Errorf("the report never says the BSH codes are this product's own rules rather than ShellCheck's:\n%s", out)
	}
	if !strings.Contains(out, "never runs a hook body") {
		t.Errorf("the report never says that nothing was executed:\n%s", out)
	}
}

// TestValidateWorkflowJSONCarriesTheShellFindings pins the structured
// half of #906 at this surface.
//
// It goes through the same printer the command's --json branch calls,
// over a report carrying findings, because "the whole report" is a claim
// that has to survive a field being added to it: a --json that filtered
// would be a second, quieter opinion about which findings matter, and the
// UI rendering a position per finding has nowhere else to read it from.
func TestValidateWorkflowJSONCarriesTheShellFindings(t *testing.T) {
	out := captureStdout(t, func() {
		if code := printWorkflowJSON(workflowLintReport()); code != 0 {
			t.Errorf("printing the report as JSON failed with code %d", code)
		}
	})

	var report service.WorkflowValidation
	if err := json.Unmarshal([]byte(jsonObjectIn(t, out)), &report); err != nil {
		t.Fatalf("--json did not emit a decodable report: %v\n%s", err, out)
	}
	if len(report.Scripts) != 4 {
		t.Fatalf("--json carries %d script(s), want 4:\n%s", len(report.Scripts), out)
	}

	// The parse verdict, with its position: a client showing an operator
	// where a hook is broken needs the line and the column, not a
	// boolean.
	refused := report.Scripts[1].Lint
	if refused.Parsed || refused.ParseErrorLine != 3 || refused.ParseErrorCol != 11 || refused.ParseError == "" {
		t.Errorf("--json lost the parse verdict or its position: %+v", refused)
	}

	// Every field of every finding, because each one is load-bearing for
	// a different reader: the code for grouping, the severity for the
	// save gate, the position for navigation, the message for the human.
	findings := report.Scripts[2].Lint.Findings
	if len(findings) != 2 {
		t.Fatalf("--json carries %d finding(s) for the script with two, so it is filtering: %+v", len(findings), findings)
	}
	blocking := report.Scripts[2].Lint.BlockingLintFindings()
	if len(blocking) != 1 || blocking[0].Code != "BSH003" || blocking[0].Severity != "error" || blocking[0].Line != 9 || blocking[0].Col != 3 || blocking[0].Message == "" {
		t.Errorf("--json does not carry the blocking finding intact: %+v", blocking)
	}

	// And the state a client must not fold into either of the others.
	unread := report.Scripts[3].Lint
	if unread.Examined || unread.NotExaminedReason == "" {
		t.Errorf("--json lost the not-examined state or its reason, so a client would report a pass nobody proved: %+v", unread)
	}
}

// TestValidateStillValidatesAnArtifact is the regression guard for the
// operand this form shares a command with.
//
// cmdValidate now reads its first operand as a possible verb, and the
// artifact form is the one that was there first. An id that is not the
// word "workflow" has to reach app.ParseArtifactID exactly as it did
// before, so the refusal below is the artifact form's own (a bad artifact
// id, exit 1) rather than the new form's usage complaint.
func TestValidateStillValidatesAnArtifact(t *testing.T) {
	configPath := writeTestConfig(t)

	var code int
	out := captureStderr(t, func() {
		code = run([]string{"validate", "production/postgres-primary/never-collected.dump", "--config", configPath})
	})
	if code != 1 {
		t.Fatalf("validating an artifact this deployment does not hold = %d, want 1 (an ordinary failure about the deployment); stderr:\n%s", code, out)
	}
}

// A set whose stage directories exist and hold nothing still gets the
// two sentences an operator is reading this command for: what the
// verification would refuse on, and that nothing was executed.
//
// This branch used to return before both (review minor), which made the
// report for a deployment part-way through being set up the one report
// that said neither.
func TestValidateWorkflowSummarisesASetWithNoScripts(t *testing.T) {
	report := service.WorkflowValidation{
		BackupSetID:    "production/postgres-primary",
		ValidForBackup: true,
		WorkflowValid:  true,
		Configured:     true,
		Stages: []service.WorkflowStage{
			{Scope: "global", Phase: "before", Dir: "/srv/backupd/workflows/global-before"},
		},
	}

	out := captureStdout(t, func() { printWorkflowValidation(report) })

	for _, want := range []string{
		"none discovered",
		"shell verification:",
		"a parse error or an error-severity finding is what would refuse a workflow save",
		"nothing above was executed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("a set with no scripts does not report %q:\n%s", want, out)
		}
	}
}
