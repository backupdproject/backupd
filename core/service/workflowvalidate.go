package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/hostrunner"
	"github.com/retnd/retnd/core/internal/workflow"
	"github.com/retnd/retnd/core/internal/workflowlint"
)

// `backupd validate workflow <source/backup-set>`: everything this
// product can find out about a set's hooks WITHOUT running one (#813).
//
// # The rule that shapes the whole file
//
// Validation never executes a script body. Not once, not "just the safe
// ones", not behind a flag. A hook is arbitrary code an operator wrote to
// quiesce a database, and a validation that ran it would be a command
// somebody types to check their configuration that stops a production
// database. So the only two things that ever reach an interpreter here
// are `bash -n` -- parse, never execute -- and the remote capability
// PROBE, which is internal/remoteexec's own fixed script and not the
// operator's.
//
// # Why the bulk of it is one call to Snapshot
//
// Because Snapshot is what a real run does, and validating through a
// second implementation would validate the second implementation. It
// resolves each stage directory inside the approved root, refuses a
// symlink or a group-writable ancestor at every level, parses every
// script name into a target, orders the plan bytewise, captures the bytes
// and records their sha256. Those are six of #813's items and they are
// exactly the six a run would refuse on. Re-deriving them here would
// produce a validation that can pass while the run fails, which is worse
// than no validation.
//
// What it costs is a spool, because capturing is what hashing means. So
// validation spools to a directory of its own and removes it: a real run
// keeps its spool because a recovery has to be able to re-execute the
// exact bytes, and a validation has nothing to recover.
//
// # Why a set can be valid for backup and invalid for workflows
//
// #813 requires this to be sayable and it is the normal case during
// setup: a backup set whose source connects, whose destination is
// writable and whose retention is sound, with a hook directory somebody
// has not created yet. Reporting the set as broken would tell an operator
// their backups are failing when they are not. So there are two verdicts
// and the report carries both.

// The check ids this validation reports, one per item #813 enumerates.
//
// A closed vocabulary rather than free text, because these reach a JSON
// surface and a UI: a client that groups by check, or an operator who
// greps for one, needs the id to be a value rather than a sentence. The
// SENTENCE is Detail, and it is free to be rewritten; the id is not.
const (
	WorkflowCheckDirectories        = "resolved_directories"
	WorkflowCheckScriptNames        = "script_names"
	WorkflowCheckOrdering           = "ordering"
	WorkflowCheckTarget             = "target"
	WorkflowCheckScriptHash         = "script_hash"
	WorkflowCheckPermissionAncestry = "permission_ancestry"
	WorkflowCheckRunnerHealth       = "runner_health"
	WorkflowCheckLocalBashSyntax    = "local_bash_syntax"
	WorkflowCheckExecConnection     = "exec_connection"
	WorkflowCheckExecCapability     = "exec_capability"
	WorkflowCheckRemoteBashSyntax   = "remote_bash_syntax"
	WorkflowCheckEnvConflicts       = "environment_conflicts"
	WorkflowCheckReservedEnv        = "reserved_variables"
	WorkflowCheckSecretRefs         = "secret_references"
	WorkflowCheckTimeouts           = "timeouts"
)

// WorkflowChecks lists every check, in report order.
//
// Exported so the CLI's own output, the API's enum and a test that
// asserts every item #813 names is covered can all read one list rather
// than three copies of it.
func WorkflowChecks() []string {
	return []string{
		WorkflowCheckDirectories,
		WorkflowCheckScriptNames,
		WorkflowCheckOrdering,
		WorkflowCheckTarget,
		WorkflowCheckScriptHash,
		WorkflowCheckPermissionAncestry,
		WorkflowCheckTimeouts,
		WorkflowCheckEnvConflicts,
		WorkflowCheckReservedEnv,
		WorkflowCheckSecretRefs,
		WorkflowCheckRunnerHealth,
		WorkflowCheckLocalBashSyntax,
		WorkflowCheckExecConnection,
		WorkflowCheckExecCapability,
		WorkflowCheckRemoteBashSyntax,
	}
}

// The severities a finding can carry.
//
// Four, and "skipped" is the one that earns its place: a deployment with
// no remote hooks has nothing to say about its exec capability, and
// reporting that as OK would be this product claiming it proved something
// it never looked at. An operator debugging a remote hook that does not
// run needs to see "skipped: this set has no remote scripts" rather than
// a green tick.
const (
	WorkflowSeverityOK      = "ok"
	WorkflowSeveritySkipped = "skipped"
	WorkflowSeverityWarning = "warning"
	WorkflowSeverityError   = "error"
)

// WorkflowFinding is one check's answer.
type WorkflowFinding struct {
	Check    string
	Severity string

	// Detail is the operator-facing sentence. It may name a script by
	// BASENAME, a stage directory as configured, a variable NAME and a
	// secret's LOCATION -- and never a secret's value, because nothing
	// on this path resolves one.
	Detail string

	// Scope, Phase, Target and Script narrow a finding to one step where
	// the check is per script. All empty for a check about the set.
	Scope  string
	Phase  string
	Target string
	Script string
}

// WorkflowValidation is one set's whole report.
type WorkflowValidation struct {
	BackupSetID string

	// ValidForBackup says nothing is wrong with the backup set ITSELF.
	// WorkflowValid says nothing is wrong with its hooks. They are
	// separate answers on purpose; see this file's doc.
	ValidForBackup bool
	WorkflowValid  bool

	// Configured says this set runs hooks at all. A set with no stages
	// reports WorkflowValid true and every check skipped, which is the
	// honest answer: there is nothing to be wrong.
	Configured bool

	Root   string
	Stages []WorkflowStage

	// Scripts is every hook this set would run, in plan order, with the
	// resolved bound each one gets.
	Scripts []WorkflowValidatedScript

	Findings []WorkflowFinding
}

// WorkflowValidatedScript is one discovered hook.
type WorkflowValidatedScript struct {
	StepID     string
	Order      int
	ScriptName string
	Scope      string
	Phase      string
	Target     string

	// SHA256 is the hash of the bytes as they are on disk right now. It
	// is what a run would record and what a recovery would re-verify
	// against, so an operator comparing two deployments can compare this.
	SHA256 string
	Size   int64

	TimeoutMillis int64

	ExecutionConnectionRef string

	// Lint is what this product's own shell verification established
	// about these exact bytes: whether they parse, and what its rules
	// found (#906). It is per script rather than on the report, because
	// a set with four hooks and one broken one is the ordinary case and a
	// single list of findings would leave a client joining them back up
	// by basename.
	Lint WorkflowScriptLint
}

// WorkflowLintFinding is one thing this product's shell rules reported
// about one script.
//
// The code is internal/workflowlint's own BSH namespace, and the
// severities are that package's four. Neither is translated here: a
// code an operator reads in the UI and greps in the CLI has to be the
// code the rule carries, and a severity that travelled through a mapping
// table is one nobody can cross-check against the rule that produced it.
type WorkflowLintFinding struct {
	Code     string
	Severity string

	// Line and Col are 1-based, so "BSH003 at 2:8" is a place an
	// operator can go to. That position is the whole point of this
	// surface: `bash -n` through a runner could only ever say "it does
	// not parse".
	Line int
	Col  int

	// Message is the rule's sentence: what is wrong, what happens when
	// it goes wrong, and what to write instead. It may quote a variable
	// NAME or a literal from the script -- the script's own text -- and
	// never a resolved secret, because nothing on this path resolves
	// one.
	Message string

	// Excerpt is the script's own line at Line, with one line either
	// side, so a client can show the code beside the position rather
	// than only the position. See WorkflowSourceExcerpt.
	Excerpt WorkflowSourceExcerpt
}

// WorkflowScriptLint is the static verification of one script.
//
// Three states and they are deliberately distinguishable: examined and
// parsed (Parsed true), examined and refused (Parsed false with a
// ParseError), and NOT EXAMINED, which is a script larger than the
// verification will read. A client that folded the third into either of
// the others would either report a pass nobody proved or a fault nobody
// found.
type WorkflowScriptLint struct {
	// Examined says the bytes were read and checked.
	Examined bool

	// NotExaminedReason is why they were not, and is never empty when
	// Examined is false.
	NotExaminedReason string

	// Parsed says the bytes are a shell program. False with Examined
	// true means ParseError says where and why.
	Parsed         bool
	ParseError     string
	ParseErrorLine int
	ParseErrorCol  int

	// ParseErrorExcerpt is the script's own line at that position, with
	// one line either side.
	ParseErrorExcerpt WorkflowSourceExcerpt

	// Findings is what the rules reported, in position order. It carries
	// every severity, including the ones that do not block a save, for
	// the reason the report's own vocabulary has four: a client that only
	// saw the blocking ones could not show an operator the warning they
	// are about to introduce.
	Findings []WorkflowLintFinding
}

// WorkflowSourceExcerpt is a few of a script's own lines, carried beside
// a position that names one of them.
//
// # Why the report carries source
//
// Because a position on its own is a lookup somebody has to perform on a
// machine they may not be on: "BSH003 at 24:10" sends an operator to a
// NAS over SSH to read one line. The excerpt is the finding and the line
// in one place, which is what the approved design for this feature draws
// -- a gutter, the line, and the reported column marked.
//
// # What is safe about it
//
// The lines come from internal/workflowlint, from the bytes this
// validation READ AND HASHED rather than from a re-read, so they cannot
// disagree with the positions beside them; and they arrive with control
// characters removed and their length bounded, at the point they are
// produced rather than at each surface that draws them. A hook script is
// arbitrary text and this text goes into a browser and a terminal.
//
// It is the script's own text and never a resolved secret: nothing on
// this path resolves one. A hook that has a credential WRITTEN INTO it
// as a literal is a hook whose own bytes say so, which is true of every
// surface this product has that echoes a script -- including the `bash
// -n` error a runner already returns.
type WorkflowSourceExcerpt struct {
	Lines []WorkflowSourceLine
}

// WorkflowSourceLine is one line of a script, numbered as an editor
// numbers it.
type WorkflowSourceLine struct {
	Number int
	Text   string

	// Truncated says the line was longer than the excerpt carries, so a
	// reader can tell the script from what they are being shown.
	Truncated bool
}

// BlockingLintFindings is the subset of one script's findings that would
// refuse a save: the error-severity ones.
//
// The THRESHOLD is not re-implemented here -- internal/workflowlint owns
// it -- and this is the same predicate applied to the reported shape, so
// a surface holding a report can say which findings are the blocking ones
// without asking the engine again.
func (l WorkflowScriptLint) BlockingLintFindings() []WorkflowLintFinding {
	var out []WorkflowLintFinding
	for _, f := range l.Findings {
		if f.Severity == workflowlint.SeverityError {
			out = append(out, f)
		}
	}

	return out
}

// toWorkflowScriptLint renders one engine report onto the reported shape,
// over the bytes it was produced from.
//
// Field by field rather than by embedding the engine's type, which is the
// discipline every boundary in this package keeps: a field added to the
// engine has to be given a name here by somebody, rather than arriving on
// an API surface because a struct grew.
//
// src is the source the report is ABOUT, and it is passed rather than
// re-read for the reason WorkflowSourceExcerpt gives: an excerpt read
// later is an excerpt of a file somebody may have edited since, and a
// line shown under the wrong position is worse than no line.
func toWorkflowScriptLint(r workflowlint.ScriptReport, src []byte) WorkflowScriptLint {
	out := WorkflowScriptLint{
		Examined:          r.Examined,
		NotExaminedReason: r.NotExaminedReason,
		Parsed:            r.Examined && r.ParseError == nil,
		Findings:          make([]WorkflowLintFinding, 0, len(r.Findings)),
	}

	if r.ParseError != nil {
		out.ParseError = r.ParseError.Message
		out.ParseErrorLine = r.ParseError.Line
		out.ParseErrorCol = r.ParseError.Col
		out.ParseErrorExcerpt = toWorkflowSourceExcerpt(workflowlint.Excerpt(src, r.ParseError.Line))
	}

	for _, f := range r.Findings {
		out.Findings = append(out.Findings, toWorkflowLintFinding(f, src))
	}

	return out
}

func toWorkflowLintFinding(f workflowlint.Finding, src []byte) WorkflowLintFinding {
	return WorkflowLintFinding{
		Code:     f.Code,
		Severity: f.Severity,
		Line:     f.Line,
		Col:      f.Col,
		Message:  f.Message,
		Excerpt:  toWorkflowSourceExcerpt(workflowlint.Excerpt(src, f.Line)),
	}
}

func toWorkflowSourceExcerpt(lines []workflowlint.SourceLine) WorkflowSourceExcerpt {
	out := WorkflowSourceExcerpt{Lines: make([]WorkflowSourceLine, 0, len(lines))}
	for _, l := range lines {
		out.Lines = append(out.Lines, WorkflowSourceLine{
			Number:    l.Number,
			Text:      l.Text,
			Truncated: l.Truncated,
		})
	}

	return out
}

// ErrWorkflowValidationUnavailable is what validation reports when it
// cannot even begin: no state directory to spool into.
var ErrWorkflowValidationUnavailable = errors.New("service: this deployment cannot validate workflows")

// ValidateWorkflow reports everything this product can establish about a
// backup set's hooks without running one.
//
// It takes a context because the two probes it makes are network calls (a
// Unix socket and an SSH connection) and an operator's Ctrl-C has to stop
// them. Both are bounded independently of that, at workflowProbeTimeout,
// so a validation against an unreachable host answers rather than hangs.
func (b *BackupService) ValidateWorkflow(ctx context.Context, id string) (WorkflowValidation, error) {
	cfg := b.state.Load().inner.Config

	bs, err := lookupConfiguredBackupSet(cfg, id)
	if err != nil {
		return WorkflowValidation{}, err
	}

	// The whole pass is bounded as well as each probe. Every CLI verb in
	// this repository passes context.Background(), so without this a set
	// with six remote hooks against a host that black-holes packets would
	// wait six probe timeouts in a row at somebody's terminal.
	ctx, cancel := context.WithTimeout(ctx, workflowValidationDeadline)
	defer cancel()

	v := &workflowValidator{
		svc: b,
		cfg: cfg,
		set: bs,
		report: WorkflowValidation{
			BackupSetID: bs.ID.String(),
			// A set this configuration holds has already been through
			// config.Validate, which is what "valid for backup" means:
			// the service refuses to load a configuration that is not.
			// The value is carried anyway rather than assumed by the
			// caller, because it is half of the answer #813 requires
			// this report to give and a reader must not have to know
			// that it is always true today to interpret the other half.
			ValidForBackup: true,
			Root:           cfg.Workflows.Root,
		},
	}

	// The spool outlives checkPlan and dies with the pass. Both syntax
	// checks read the captured bytes -- this product's own verification
	// inside checkPlan, and the runner's `bash -n` from checkExecutors
	// afterwards -- so the one thing that must not happen is the copy
	// disappearing between them.
	defer func() {
		if v.releaseSpool != nil {
			v.releaseSpool()
		}
	}()

	v.run(ctx)

	return v.report, nil
}

// workflowValidator accumulates one report.
type workflowValidator struct {
	svc *BackupService
	cfg *config.Config
	set config.BackupSet

	report WorkflowValidation
	plan   workflow.Plan

	// releaseSpool removes the throwaway spool checkPlan captured this
	// validation's scripts into, and it is held here rather than
	// deferred inside checkPlan because the spool has to OUTLIVE that
	// function.
	//
	// #816 found this from a browser. checkPlan used to
	// `defer cleanup()`, and checkExecutors -- which runs after it --
	// asks the host runner to parse each local hook's CAPTURED bytes,
	// through plan.OpenScript. With the spool already gone, every set
	// with a NAME.local.sh hook reported
	//
	//   local_bash_syntax  error  this workflow run plan cannot be
	//   built: <spool>/validate-NNN cannot be opened: no such file or
	//   directory
	//
	// once per local step, and an error-severity finding is what makes
	// WorkflowValid false -- so a deployment whose hooks were perfectly
	// sound reported "this backup set is sound and its hooks are not"
	// on the CLI and in the UI's validation report. Nothing was wrong
	// with the deployment, and nothing in the product's own tests could
	// see it: the report was built from a plan whose bytes had been
	// deleted between the two halves of the same pass.
	releaseSpool func()

	// syntax holds every observation about the two syntax checks until
	// settleSyntaxFindings collapses them. See scriptFinding.
	syntax []WorkflowFinding
}

func (v *workflowValidator) add(f WorkflowFinding) {
	v.report.Findings = append(v.report.Findings, f)
}

func (v *workflowValidator) ok(check, detail string) {
	v.add(WorkflowFinding{Check: check, Severity: WorkflowSeverityOK, Detail: detail})
}

func (v *workflowValidator) skip(check, detail string) {
	v.add(WorkflowFinding{Check: check, Severity: WorkflowSeveritySkipped, Detail: detail})
}

func (v *workflowValidator) fail(check, detail string) {
	v.add(WorkflowFinding{Check: check, Severity: WorkflowSeverityError, Detail: detail})
}

func (v *workflowValidator) warn(check, detail string) {
	v.add(WorkflowFinding{Check: check, Severity: WorkflowSeverityWarning, Detail: detail})
}

// run performs every check, in report order, and settles the verdict.
func (v *workflowValidator) run(ctx context.Context) {
	stages := v.cfg.WorkflowStagesFor(&v.set)
	v.report.Configured = len(stages) > 0

	for _, st := range stages {
		v.report.Stages = append(v.report.Stages, WorkflowStage{
			Scope: string(st.Scope), Phase: string(st.Phase), Dir: st.Dir,
		})
	}

	// The environment checks come first and run whatever the stages say,
	// because they are about CONFIGURATION rather than about what is on
	// disk: a reserved variable name or a secret reference pointing at a
	// file nobody created is worth reporting to an operator who has not
	// made their hook directory yet.
	v.checkEnvironment()

	if !v.report.Configured {
		for _, check := range []string{
			WorkflowCheckDirectories, WorkflowCheckScriptNames, WorkflowCheckOrdering,
			WorkflowCheckTarget, WorkflowCheckScriptHash, WorkflowCheckPermissionAncestry,
			WorkflowCheckTimeouts, WorkflowCheckRunnerHealth, WorkflowCheckLocalBashSyntax,
			WorkflowCheckExecConnection, WorkflowCheckExecCapability, WorkflowCheckRemoteBashSyntax,
		} {
			v.skip(check, "this backup set runs no workflow hooks: neither the deployment nor the set configures a stage directory")
		}
		v.settle()

		return
	}

	v.checkPlan(ctx, stages)
	v.checkTimeouts()
	v.checkExecutors(ctx)

	// After every observer, and before the verdict: the two syntax
	// checks have two of them. See settleSyntaxFindings.
	v.settleSyntaxFindings()
	v.settle()
}

// settle decides the two verdicts and sorts the findings into check
// order.
//
// Sorted, so two runs of this command over an unchanged deployment print
// identical output. A report an operator cannot diff is one they cannot
// use to find out what they just changed.
func (v *workflowValidator) settle() {
	order := make(map[string]int, len(WorkflowChecks()))
	for i, c := range WorkflowChecks() {
		order[c] = i
	}

	sort.SliceStable(v.report.Findings, func(i, j int) bool {
		return order[v.report.Findings[i].Check] < order[v.report.Findings[j].Check]
	})

	v.report.WorkflowValid = true
	for _, f := range v.report.Findings {
		if f.Severity == WorkflowSeverityError {
			v.report.WorkflowValid = false

			break
		}
	}
}

// checkPlan is the six on-disk checks, taken from one Snapshot.
func (v *workflowValidator) checkPlan(ctx context.Context, stages []workflow.StageSpec) {
	spool, cleanup, err := v.tempSpool()
	if err != nil {
		for _, check := range planChecks() {
			v.fail(check, err.Error())
		}

		return
	}
	v.releaseSpool = cleanup

	root, err := workflow.NewRoot(v.cfg.Workflows.Root)
	if err != nil {
		v.fail(WorkflowCheckDirectories, err.Error())
		for _, check := range planChecks()[1:] {
			v.skip(check, "the workflow root could not be approved, so nothing under it was examined")
		}

		return
	}

	var execRef string
	if v.set.Workflow != nil {
		execRef = v.set.Workflow.RemoteExecConnectionRef
	}

	plan, err := workflow.Snapshot(workflow.SnapshotRequest{
		RunID:                   "validate_" + uuid.New().String(),
		BackupSetID:             v.set.ID,
		Root:                    root,
		Stages:                  stages,
		Env:                     v.set.WorkflowEnvironment,
		Timeout:                 v.cfg.EffectiveScriptTimeout(&v.set),
		RemoteExecConnectionRef: execRef,
		MaxScriptSize:           v.cfg.EffectiveMaxScriptSize(),
		SpoolRoot:               spool,
	})
	if err != nil {
		v.attributeSnapshotFailure(err)

		return
	}

	v.plan = plan
	v.recordScripts(plan)

	v.ok(WorkflowCheckDirectories, fmt.Sprintf("every stage directory resolves inside the approved root %s", root.Path()))
	v.ok(WorkflowCheckScriptNames, fmt.Sprintf("%d script name(s) parse as NAME.local.sh or NAME.remote.sh", len(plan.Steps())))
	v.ok(WorkflowCheckOrdering, "the plan is ordered by scope, then phase, then script name, bytewise")
	v.ok(WorkflowCheckTarget, "every script's target is decided by its own file name and cannot be overridden")
	v.ok(WorkflowCheckScriptHash, "every script was read and hashed; a run re-verifies these hashes before it executes anything")
	v.ok(WorkflowCheckPermissionAncestry, "no stage directory, and no ancestor of one inside the root, is a symbolic link or writable by group or other")

	// Here rather than beside the call in `run`, because it reads the
	// CAPTURED bytes and this is where they have just been captured:
	// the verification is about what a run would execute and about the
	// hashes above, not about the files in the operator's tree. The
	// copy stays readable until the whole pass ends (releaseSpool), so
	// the runner's own `bash -n` in checkExecutors sees it too.
	v.checkScriptLint(ctx)
}

// checkScriptLint is #906's half of the two syntax checks: this
// product's own shell verification of every captured script, reported
// against the same two check ids the runner's `bash -n` answers under.
//
// # Why it shares the check id rather than adding two more
//
// Because it is the same question with a better answer. `local_bash_
// syntax` has always meant "would a shell accept this script", and an
// operator scanning for it wants one verdict rather than two that can
// disagree about the same bytes. What changes is what produces it: a
// parse by mvdan.cc/sh, in this process, with a LINE AND COLUMN --
// where the runner could only ever report that `bash -n` exited
// non-zero, and only when there was a runner to ask.
//
// The runner's and the far side's `bash -n` are still asked
// (workflowpreflight.go) and still report under these ids, because they
// answer the other half: whether the executor that will run the script
// exists, answers, and accepts it. A validation that dropped them would
// stop noticing an SFTP-only source or a dead runner socket.
//
// # Why one finding per script and the detail is not the findings list
//
// The structured findings live on the script (WorkflowValidatedScript.
// Lint), because that is where a client can render them per script with
// their positions. A findings LIST that also carried every rule hit
// would put forty lines of style advice into a CLI report whose job is
// to say which of fifteen checks is wrong. So each script contributes
// one sentence -- refused, blocking, reported-but-not-blocking, not
// examined, or nothing -- and the positions are one field away.
func (v *workflowValidator) checkScriptLint(ctx context.Context) {
	for i := range v.report.Scripts {
		script := &v.report.Scripts[i]
		check := scriptSyntaxCheck(script.Target)

		spooled, err := v.plan.OpenScript(script.StepID)
		if err != nil {
			// The bytes the plan recorded could not be re-read. Reported
			// against this check rather than swallowed, because a
			// verification that silently examined nothing is the one
			// output worse than a failure.
			v.scriptFinding(check, WorkflowSeverityError, *script, err.Error())

			continue
		}

		report := workflowlint.Report(ctx, script.ScriptName, spooled.Body)
		script.Lint = toWorkflowScriptLint(report, spooled.Body)

		switch {
		case !report.Examined:
			v.scriptFinding(check, WorkflowSeveritySkipped, *script, report.NotExaminedReason)
		case report.ParseError != nil:
			v.scriptFinding(check, WorkflowSeverityError, *script, fmt.Sprintf(
				"%s is not a shell program: at line %d, column %d, %s. A run would refuse it, and this product's own parser answered without needing a shell, a runner or the source host",
				script.ScriptName, report.ParseError.Line, report.ParseError.Col, report.ParseError.Message))
		case len(report.Blocking()) > 0:
			v.scriptFinding(check, WorkflowSeverityError, *script,
				lintDetail(script.ScriptName, "does not pass", report.Blocking(), len(report.Findings)))
		case len(report.Findings) > 0:
			v.scriptFinding(check, WorkflowSeverityWarning, *script,
				lintDetail(script.ScriptName, "parses, and", report.Findings, len(report.Findings)))
		default:
			v.scriptFinding(check, WorkflowSeverityOK, *script, fmt.Sprintf(
				"%s parses, and this product's own shell rules report nothing about it. Nothing was executed: the bytes were parsed and walked in this process",
				script.ScriptName))
		}
	}
}

// lintDetail renders the sentence one script contributes to the findings
// list.
//
// It names the FIRST finding with its code and position and counts the
// rest, rather than listing them: this string is one line of a report an
// operator reads to find out which check is unhappy, and the full list
// with every position is carried structurally on the script itself.
func lintDetail(name, verb string, shown []workflowlint.Finding, total int) string {
	first := shown[0]

	detail := fmt.Sprintf("%s %s backupd's shell rules: %s at %d:%d (%s) %s",
		name, verb, first.Code, first.Line, first.Col, first.Severity, first.Message)

	if total > 1 {
		detail += fmt.Sprintf(" (and %d more finding(s) on this script)", total-1)
	}

	return detail
}

// scriptSyntaxCheck says which of the two syntax checks a script's
// verdict belongs under.
func scriptSyntaxCheck(target string) string {
	if target == string(workflow.TargetRemote) {
		return WorkflowCheckRemoteBashSyntax
	}

	return WorkflowCheckLocalBashSyntax
}

// scriptFinding records a finding narrowed to one step, so a client can
// group it under the script it is about.
//
// The two SYNTAX checks do not go straight onto the report, and that is
// review MAJOR 1: two things observe them -- this product's own parser,
// in process, and the executor's `bash -n`, over a socket or an SSH
// connection -- and both used to emit under the same check id
// unreconciled. A clean script produced two `ok` rows; a script with a
// blocking finding produced an `error` saying it does not pass and an
// `ok` saying the hooks parse, which is a report that contradicts itself
// about the same bytes. See settleSyntaxFindings.
func (v *workflowValidator) scriptFinding(check, severity string, script WorkflowValidatedScript, detail string) {
	v.recordSyntax(WorkflowFinding{
		Check:    check,
		Severity: severity,
		Detail:   detail,
		Scope:    script.Scope,
		Phase:    script.Phase,
		Target:   script.Target,
		Script:   script.ScriptName,
	})
}

// isSyntaxCheck reports whether a check is one of the two that more than
// one observer answers.
func isSyntaxCheck(check string) bool {
	return check == WorkflowCheckLocalBashSyntax || check == WorkflowCheckRemoteBashSyntax
}

// emitOrRecord puts a finding on the report, unless it is about one of
// the two checks two observers answer, in which case it waits for the
// other one.
//
// Every other check has exactly one observer, and routing those through
// the reconciler would be a map lookup to decide something already known.
func (v *workflowValidator) emitOrRecord(f WorkflowFinding) {
	if isSyntaxCheck(f.Check) {
		v.recordSyntax(f)

		return
	}

	v.add(f)
}

// recordSyntax holds one observation about a script's syntax until every
// observer has spoken.
func (v *workflowValidator) recordSyntax(f WorkflowFinding) {
	v.syntax = append(v.syntax, f)
}

// syntaxSeverityRank orders the severities so "worst" is a value rather
// than a comparison each caller writes.
//
// skipped outranks ok deliberately: "nobody examined this" is a weaker
// claim than "this passed", so when one observer passed a script and
// another could not look at it, the row has to be the one that does not
// claim a pass.
func syntaxSeverityRank(severity string) int {
	switch severity {
	case WorkflowSeverityError:
		return 3
	case WorkflowSeverityWarning:
		return 2
	case WorkflowSeveritySkipped:
		return 1
	default:
		return 0
	}
}

// settleSyntaxFindings collapses every observation about the two syntax
// checks into ONE row per script per check, at the worst severity
// observed (review MAJOR 1).
//
// # Why worst-severity rather than both rows
//
// Because the two observations are about the same question -- would a
// shell accept this script -- asked of the same bytes by two things that
// can both answer. A report carrying both is a report an operator has to
// reconcile, and the reconciliation is always the same: the worse answer
// is the one to act on. So it is done once, here, rather than by every
// reader.
//
// The losing sentence is not always thrown away: when it is itself a
// complaint (an error or a warning rather than a pass or a skip) it is
// appended, because "the parser refuses line 4" and "the runner's bash
// refuses it too" are two facts and an operator debugging a hook wants
// both.
//
// # Why a check-level row is dropped once any script has one
//
// The executor probe emits one aggregate pass for a whole target ("3
// local hook(s) parse") beside the per-script answers. With per-script
// rows present that aggregate is either redundant or, when one script
// failed, actively contradictory. The check-level row survives only when
// there is nothing per-script to say -- "this set runs no local hooks",
// which is the honest skip.
func (v *workflowValidator) settleSyntaxFindings() {
	worst := make(map[string]WorkflowFinding, len(v.syntax))
	order := make([]string, 0, len(v.syntax))
	perScript := map[string]bool{}

	for _, f := range v.syntax {
		key := syntaxKey(f.Check, f.Scope, f.Phase, f.Target, f.Script)

		if f.Script != "" {
			perScript[f.Check] = true
		}

		prev, seen := worst[key]
		if !seen {
			worst[key] = f
			order = append(order, key)

			continue
		}

		winner, loser := prev, f
		if syntaxSeverityRank(f.Severity) > syntaxSeverityRank(prev.Severity) {
			winner, loser = f, prev
		}
		if syntaxSeverityRank(loser.Severity) >= syntaxSeverityRank(WorkflowSeverityWarning) &&
			loser.Detail != winner.Detail {
			winner.Detail += " Also: " + loser.Detail
		}
		worst[key] = winner
	}

	// Per script, in PLAN order, then whatever is left in the order it
	// was observed: two runs of this validation over an unchanged
	// deployment have to print the same report.
	for _, check := range []string{WorkflowCheckLocalBashSyntax, WorkflowCheckRemoteBashSyntax} {
		for _, s := range v.report.Scripts {
			key := syntaxKey(check, s.Scope, s.Phase, s.Target, s.ScriptName)
			if f, ok := worst[key]; ok {
				v.add(f)
				delete(worst, key)
			}
		}
	}

	for _, key := range order {
		f, ok := worst[key]
		if !ok {
			continue
		}
		delete(worst, key)

		if f.Script == "" && perScript[f.Check] {
			continue
		}
		v.add(f)
	}
}

func syntaxKey(check, scope, phase, target, script string) string {
	return check + "\x00" + scope + "\x00" + phase + "\x00" + target + "\x00" + script
}

// planChecks are the checks one Snapshot answers, in report order.
func planChecks() []string {
	return []string{
		WorkflowCheckDirectories,
		WorkflowCheckScriptNames,
		WorkflowCheckOrdering,
		WorkflowCheckTarget,
		WorkflowCheckScriptHash,
		WorkflowCheckPermissionAncestry,
	}
}

// attributeSnapshotFailure puts one Snapshot error against the check it
// belongs to, and marks the rest as unexamined.
//
// The attribution is by SENTINEL and not by reading the message, because
// internal/workflow exports one error value per class of refusal for
// exactly this: a substring match on a sentence is a classification that
// silently stops working when somebody improves the wording, and this is
// the surface an operator reads to find out which of fifteen things is
// wrong.
//
// Every check the failure did not land on is reported as SKIPPED rather
// than as passing. Snapshot stops at the first refusal, so nothing after
// it was examined, and a green tick on a check that never ran is the one
// output that would actively mislead.
func (v *workflowValidator) attributeSnapshotFailure(err error) {
	blamed := WorkflowCheckDirectories

	switch {
	case errors.Is(err, workflow.ErrRoot), errors.Is(err, workflow.ErrStageDir):
		blamed = WorkflowCheckDirectories
	case errors.Is(err, workflow.ErrCustody):
		blamed = WorkflowCheckPermissionAncestry
	case errors.Is(err, workflow.ErrScriptName):
		blamed = WorkflowCheckScriptNames
	case errors.Is(err, workflow.ErrScriptTooLarge), errors.Is(err, workflow.ErrSpool):
		blamed = WorkflowCheckScriptHash
	case errors.Is(err, workflow.ErrPlan):
		blamed = WorkflowCheckOrdering
	}

	for _, check := range planChecks() {
		if check == blamed {
			v.fail(check, err.Error())

			continue
		}
		v.skip(check, "not examined: the plan could not be captured, and this check runs over a captured plan")
	}

	for _, check := range []string{WorkflowCheckRunnerHealth, WorkflowCheckLocalBashSyntax, WorkflowCheckExecConnection, WorkflowCheckExecCapability, WorkflowCheckRemoteBashSyntax} {
		v.skip(check, "not examined: there is no plan to check an executor against")
	}
}

func (v *workflowValidator) recordScripts(plan workflow.Plan) {
	for _, s := range plan.Steps() {
		v.report.Scripts = append(v.report.Scripts, WorkflowValidatedScript{
			StepID:                 s.ID,
			Order:                  s.Order,
			ScriptName:             s.ScriptName,
			Scope:                  string(s.Scope),
			Phase:                  string(s.Phase),
			Target:                 string(s.Target),
			SHA256:                 s.ScriptSHA256,
			Size:                   s.ScriptSize,
			TimeoutMillis:          s.Timeout.Milliseconds(),
			ExecutionConnectionRef: s.ExecutionConnectionRef,
		})
	}
}

// tempSpool makes a throwaway spool for this validation and returns the
// function that removes it.
//
// Under the state directory's own spool root rather than in /tmp, because
// the guarantee that makes a spool safe is that it is 0600 files in a
// 0700 directory nothing else can reach, and /tmp on a NAS is a directory
// every account on the box can reach.
func (v *workflowValidator) tempSpool() (string, func(), error) {
	root := v.cfg.WorkflowSpoolDir()
	if root == "" {
		return "", nil, ErrWorkflowValidationUnavailable
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("service: preparing a workflow spool at %s: %w", root, err)
	}

	dir, err := os.MkdirTemp(root, "validate-")
	if err != nil {
		return "", nil, fmt.Errorf("service: preparing a workflow spool at %s: %w", root, err)
	}

	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// checkTimeouts reports the bound each script will get and where it came
// from.
//
// It is a check rather than a bare field because the inheritance is where
// operators get surprised: a set that pins thirty seconds while the
// deployment allows five minutes is a hook that will be killed, and the
// only place that is visible is a report that says which value won.
func (v *workflowValidator) checkTimeouts() {
	effective := v.cfg.EffectiveScriptTimeout(&v.set)

	source := "internal default"
	switch {
	case v.set.Workflow != nil && v.set.Workflow.ScriptTimeout.Duration() > 0:
		source = "this backup set's own script_timeout"
	case v.cfg.Workflows.ScriptTimeout.Duration() > 0:
		source = "the deployment's script_timeout"
	}

	v.ok(WorkflowCheckTimeouts, fmt.Sprintf("every hook of this set is bounded at %s, from %s", effective, source))
}

// checkEnvironment covers three of #813's items: name conflicts between
// the two layers, reserved names, and secret references that do not
// resolve to anything.
func (v *workflowValidator) checkEnvironment() {
	v.checkEnvConflicts()
	v.checkReservedNames()
	v.checkSecretRefs()
}

// checkEnvConflicts reports a variable configured in both layers.
//
// A WARNING and not an error, because it is legal and often deliberate:
// the whole point of a per-set layer is that it wins over the
// deployment's. What makes it worth a line is that the loser is invisible
// -- an operator who changed the deployment value and saw no effect is
// looking at exactly this -- so the finding names which one wins.
func (v *workflowValidator) checkEnvConflicts() {
	global := map[string]bool{}
	for _, e := range v.cfg.Workflows.Environment {
		global[e.Name] = true
	}

	var clashes []string
	for _, e := range v.set.Environment {
		if global[e.Name] {
			clashes = append(clashes, e.Name)
		}
	}
	sort.Strings(clashes)

	if len(clashes) == 0 {
		v.ok(WorkflowCheckEnvConflicts, "no variable is configured in both the deployment's environment and this set's")

		return
	}

	v.warn(WorkflowCheckEnvConflicts, fmt.Sprintf(
		"configured in both layers, and this backup set's value wins: %s", strings.Join(clashes, ", ")))
}

// checkReservedNames reports a configured variable in this product's own
// namespace.
//
// It should be unreachable through a validated configuration --
// config.Validate refuses the whole RETND_ prefix -- and it is checked
// anyway, because this report is also read against a configuration
// somebody is in the middle of editing by hand, and "your hook cannot see
// RETND_RUN_ID because you set it yourself" is a sentence worth having.
func (v *workflowValidator) checkReservedNames() {
	var reserved []string

	for _, layer := range [][]config.EnvironmentVariable{v.cfg.Workflows.Environment, v.set.Environment} {
		for _, e := range layer {
			if workflow.IsReservedEnvName(e.Name) {
				reserved = append(reserved, e.Name)
			}
		}
	}
	sort.Strings(reserved)

	if len(reserved) == 0 {
		v.ok(WorkflowCheckReservedEnv, fmt.Sprintf(
			"no configured variable uses this product's reserved %s namespace, so every built-in a hook reads is this product's own",
			workflow.ReservedEnvPrefix))

		return
	}

	v.fail(WorkflowCheckReservedEnv, fmt.Sprintf(
		"configured in this product's reserved %s namespace and shadowed at every run: %s",
		workflow.ReservedEnvPrefix, strings.Join(reserved, ", ")))
}

// checkSecretRefs reports a secret reference that names a location this
// deployment cannot see.
//
// # What it does and does not do
//
// It establishes PRESENCE, and deliberately not the value. A file is
// stat'ed, an environment variable is looked up by name, and a command's
// executable is looked up on PATH -- and the command is NOT RUN. That
// asymmetry is the whole design: `secretref.Resolve` executes a program
// the operator named, and this is a command an operator types to check
// their configuration. A validation that ran `vault read ...` would make
// `backupd validate` an authenticated call against somebody's secret
// store, and on a misconfigured deployment, a call that hangs.
//
// So the honest report for a command reference is that the program
// exists, which is the failure mode that actually happens (a helper that
// is not installed in the engine's container), and it says that is all it
// checked.
func (v *workflowValidator) checkSecretRefs() {
	type layer struct {
		what string
		vars []config.EnvironmentVariable
	}

	var problems []string
	checked := 0

	for _, l := range []layer{
		{"the deployment's environment", v.cfg.Workflows.Environment},
		{"this backup set's environment", v.set.Environment},
	} {
		for _, e := range l.vars {
			if e.FromSecret == nil {
				continue
			}
			checked++
			if detail := secretRefProblem(*e.FromSecret); detail != "" {
				problems = append(problems, fmt.Sprintf("%s in %s: %s", e.Name, l.what, detail))
			}
		}
	}

	switch {
	case checked == 0:
		v.skip(WorkflowCheckSecretRefs, "no workflow environment variable takes its value from a secret reference")
	case len(problems) == 0:
		v.ok(WorkflowCheckSecretRefs, fmt.Sprintf(
			"%d secret reference(s) name a location this deployment can see. A command reference is checked for the program's presence only: this validation never runs it", checked))
	default:
		sort.Strings(problems)
		v.fail(WorkflowCheckSecretRefs, strings.Join(problems, "; "))
	}
}

// secretRefProblem reports what is wrong with one reference, or "".
func secretRefProblem(s config.SecretSource) string {
	switch {
	case s.File != "":
		info, err := os.Lstat(s.File)
		switch {
		case err != nil:
			return fmt.Sprintf("the secret file %s cannot be read (%v)", s.File, err)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Sprintf("the secret file %s is a symbolic link, and this product will not follow one to a credential", s.File)
		case !info.Mode().IsRegular():
			return fmt.Sprintf("the secret file %s is not a regular file", s.File)
		case info.Mode().Perm()&0o077 != 0:
			return fmt.Sprintf("the secret file %s is readable by group or other (mode %04o), so it is not a credential", s.File, info.Mode().Perm())
		}

		return ""
	case s.Env != "":
		if _, ok := os.LookupEnv(s.Env); !ok {
			return fmt.Sprintf("the environment variable %s is not set in this process, so the reference resolves to nothing", s.Env)
		}

		return ""
	case len(s.Command) > 0:
		if _, err := exec.LookPath(s.Command[0]); err != nil {
			return fmt.Sprintf("the program %s is not on this process's PATH", filepath.Base(s.Command[0]))
		}

		return ""
	default:
		return "the reference names no location: set exactly one of file, env or command"
	}
}

// checkExecutors asks the Host Workflow Runner and the execution
// connection whether they can run this plan, and renders their answers
// as findings.
//
// The probes themselves are NOT here, and that is #813's own
// requirement rather than tidiness: a run has to refuse an invalid
// required hook before the first "before" script executes, so the same
// five answers are needed on the way into a run
// (refuseUnrunnablePlan). They live in workflowpreflight.go and both
// callers reach them through probePlanExecutors, so a validation that
// passes and a run that refuses cannot disagree about one plan.
//
// Both questions about a local hook go to the RUNNER rather than to bash
// in this process, and that is the point of the runner existing: a
// `.local.sh` hook means "run this on the machine backupd is installed
// on", the engine's canonical runtime is a distroless container with no
// shell at all, and a syntax check performed by a bash this container
// does not have would be a check against an interpreter the hook will
// never meet.
//
// The translation is a mapping and never a second classification: the
// probe already answers in the report's own three shapes (passed,
// skipped, refused), because a green tick on a check that never ran is
// the one output that would actively mislead.
func (v *workflowValidator) checkExecutors(ctx context.Context) {
	v.svc.probePlanExecutors(ctx, v.plan, func(f workflowPlanFinding) bool {
		switch {
		case f.Skipped:
			v.emitOrRecord(WorkflowFinding{Check: f.Check, Severity: WorkflowSeveritySkipped, Detail: f.Detail})
		case f.Err != nil:
			finding := WorkflowFinding{Check: f.Check, Severity: WorkflowSeverityError, Detail: f.Err.Error()}
			if f.Step != nil {
				finding.Scope = string(f.Step.Scope)
				finding.Phase = string(f.Step.Phase)
				finding.Target = string(f.Step.Target)
				finding.Script = f.Step.ScriptName
			}
			v.emitOrRecord(finding)
		default:
			v.emitOrRecord(WorkflowFinding{Check: f.Check, Severity: WorkflowSeverityOK, Detail: f.Detail})
		}

		// Every answer, always: this is the verb an operator typed to
		// find out which of fifteen things is wrong, so stopping at the
		// first failure would answer a narrower question than the
		// command asks.
		return true
	})
}

// hostRunnerStatus is the slice of hostrunner.Client a validation and a
// run's preflight use, named as an interface so a test can answer both
// questions without a real socket.
//
// Narrow on purpose: a probe may ask what the runner IS and ask it to
// PARSE, and there is deliberately no Execute on this interface, so the
// rule that neither a validation nor a preflight runs a script body is a
// property of the type rather than of a reviewer noticing.
type hostRunnerStatus interface {
	Status(ctx context.Context) (hostrunner.Status, error)
	SyntaxCheck(ctx context.Context, runID, stepID string, script []byte) error
}

// stepsWithTarget filters a plan, tolerating the zero plan a failed
// snapshot leaves behind.
func stepsWithTarget(plan workflow.Plan, target workflow.Target) []workflow.Step {
	if plan.IsZero() {
		return nil
	}

	var out []workflow.Step
	for _, s := range plan.Steps() {
		if s.Target == target {
			out = append(out, s)
		}
	}

	return out
}

// workflowValidationDeadline is how long a whole validation may take.
//
// It exists so a caller that passed context.Background() -- which is
// every CLI verb in this repository -- still gets an answer from a
// deployment whose source host is down: two probes at
// workflowProbeTimeout each, per script, could otherwise add up past any
// operator's patience. It is generous rather than tight because the sum
// is legitimately several probes on a set with several remote hooks.
const workflowValidationDeadline = 5 * time.Minute
