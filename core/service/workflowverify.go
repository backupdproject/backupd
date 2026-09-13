package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/internal/workflowlint"
)

// The save gate: a workflow configuration is not written unless every
// hook script the change points at passes this product's own shell
// verification (#906).
//
// # Why saving is the moment, and not running
//
// Because the alternative is what this product did before: an operator
// writes a hook, saves it, and finds out at 2am from a backup that
// quiesced a database and then refused to continue. A run already
// refuses an unrunnable plan before the first "before" script
// (refuseUnrunnablePlan, #809), which is the right last line of defence
// and is a terrible first one -- it is a failure an operator hears about
// from a monitoring alert rather than from the form they were filling in.
//
// A save is also the one moment where refusing costs nothing. Nothing is
// quiesced, no window is open, the operator is at a keyboard looking at
// the thing they just changed, and the refusal can name the file, the
// line and the column.
//
// # What blocks and what does not
//
// A PARSE ERROR blocks: the script is not a shell program and no
// interpreter would run it. An ERROR-severity finding blocks: today that
// is BSH003, an `rm -rf` that becomes a recursive delete of a root-level
// path the day a variable is unset. Everything else -- warning, info,
// style -- is reported and saved, which is deliberate and documented:
// BSH002 ("this cd's failure is unguarded") is a line that is in a great
// many working hooks, and a gate that refused it would be a gate an
// operator works around by not using this product's configuration
// surfaces.
//
// # Why an unverifiable script does NOT block
//
// The gate refuses on what it ESTABLISHED, never on what it could not
// look at. A stage directory that does not exist yet, a script larger
// than the verification reads, a directory whose custody the run layer
// would refuse, a deadline reached -- none of those refuse the save.
// Two reasons, and the second is the one that decides it: a save is how
// an operator CORRECTS a broken deployment (pointing a stage at a
// different directory, clearing one that is wrong), so a gate that
// refused when it could not check would make the deployment
// unconfigurable exactly when it needs configuring; and every one of
// those conditions is already reported, loudly, by `validate workflow`
// and by the Workflow tab, which is where an operator goes to find out
// why a hook is not running.
//
// # Why it re-reads the directory instead of taking a Snapshot
//
// A Snapshot is what a run does: it mints a run id and spools a copy of
// every script, because a run has to be able to re-execute the exact
// bytes it verified. A configuration write has nothing to recover and no
// guarantee of a state directory, so requiring a spool would make
// `settings workflow patch` fail on precisely the deployments that want
// the check. What it does NOT do is re-implement custody: the bytes come
// from workflow.Script.Read, which is the reader a capture uses, so a
// file this gate accepts is a file a run would accept.

// ErrWorkflowScriptRefused is what a workflow configuration write reports
// when a hook script the change points at does not pass verification.
//
// Its own sentinel, because it is its own answer: the request is
// well-formed, the deployment is fine, and a FILE is wrong. The remedy is
// to edit a script at a named line -- not to correct a field, not to
// configure the deployment -- so a caller that reported it as an invalid
// request would send an operator back to a form they filled in
// correctly.
var ErrWorkflowScriptRefused = errors.New("service: a hook script this change points at does not pass shell verification, so the configuration was not saved")

// WorkflowScriptRefusal is the structured refusal: which scripts, and
// which findings.
//
// A type rather than a formatted string, because three surfaces need the
// parts separately -- the API answers with a distinct error code and the
// blocking findings as fields, the Web UI draws them beside the save
// button, and the CLI prints them under the script name -- and a client
// parsing them back out of prose would be parsing a sentence nobody
// promised to keep stable.
type WorkflowScriptRefusal struct {
	// Scripts is every script that blocked the save, in the order the
	// stages are declared and then by script name, so two attempts at
	// the same save produce the same message.
	Scripts []WorkflowRefusedScript
}

// WorkflowRefusedScript is one script that blocked a save.
type WorkflowRefusedScript struct {
	// ScriptName is the basename, and Dir is the stage directory as it
	// is CONFIGURED (not resolved), because that is the string the
	// operator typed and the one they will recognise.
	ScriptName string
	Dir        string
	Scope      string
	Phase      string

	// ParseError, with its position, when the script is not a shell
	// program at all.
	ParseError     string
	ParseErrorLine int
	ParseErrorCol  int

	// Findings is the error-severity findings only. The rest are
	// reported by the validation surface and do not block, so including
	// them here would make a refusal read as though they had.
	Findings []WorkflowLintFinding
}

// Error renders the refusal for a terminal and for an API message.
//
// One line per script, each naming a position, because the whole value of
// this gate over `bash -n` is that it can say where. It is deliberately
// multi-line: `backupd` prints an error with Fprintln and a browser shows
// the same string, and folding four broken scripts onto one line would
// produce a sentence nobody reads to the end of.
func (r *WorkflowScriptRefusal) Error() string {
	var b strings.Builder

	b.WriteString(ErrWorkflowScriptRefused.Error())
	fmt.Fprintf(&b, " (%d script(s) refused; warnings and style findings do not block a save)", len(r.Scripts))

	for _, s := range r.Scripts {
		fmt.Fprintf(&b, "\n  %s in %s (%s %s): ", s.ScriptName, s.Dir, s.Scope, s.Phase)

		switch {
		case s.ParseError != "":
			fmt.Fprintf(&b, "does not parse at %d:%d: %s", s.ParseErrorLine, s.ParseErrorCol, s.ParseError)
		default:
			parts := make([]string, 0, len(s.Findings))
			for _, f := range s.Findings {
				parts = append(parts, fmt.Sprintf("%s at %d:%d: %s", f.Code, f.Line, f.Col, f.Message))
			}
			b.WriteString(strings.Join(parts, "; "))
		}
	}

	return b.String()
}

// Unwrap makes errors.Is(err, ErrWorkflowScriptRefused) the way every
// caller classifies this, so the API's error code and the CLI's exit path
// are decided by a sentinel rather than by a type assertion each of them
// would have to repeat.
func (r *WorkflowScriptRefusal) Unwrap() error { return ErrWorkflowScriptRefused }

// workflowVerificationDeadline bounds the whole gate.
//
// The verification is a parse and a tree walk per script, which is
// microseconds, so this is never reached by a real deployment: it is
// there because the number of scripts is bounded by
// workflow.MaxScriptsPerStage per stage rather than absolutely, and
// because a configuration write must not be able to sit in front of an
// operator indefinitely. Reaching it means the save proceeds on what was
// established so far, which is this file's own rule about what a gate may
// refuse on.
const workflowVerificationDeadline = 30 * time.Second

// refuseUnverifiableScripts is the gate, applied to the stages a write
// changes.
//
// stages are the ones the RESULTING configuration declares for the scope
// being written: a deployment-wide patch verifies the global stages, a
// per-set patch verifies that set's own. Verifying every set's hooks on
// every deployment-wide patch would make one save's refusal depend on a
// script in a different set that the operator did not touch and may not
// be able to fix.
func refuseUnverifiableScripts(ctx context.Context, cfg *config.Config, stages []workflow.StageSpec) error {
	if len(stages) == 0 || !cfg.WorkflowsConfigured() {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, workflowVerificationDeadline)
	defer cancel()

	root, err := workflow.NewRoot(cfg.Workflows.Root)
	if err != nil {
		// The root is not usable. Reported by validation, and not a
		// reason to refuse a write: a patch naming a different root is
		// the remedy for exactly this state.
		return nil
	}

	maxSize := cfg.EffectiveMaxScriptSize()
	refusal := &WorkflowScriptRefusal{}

	for _, stage := range stages {
		if stage.Dir == "" {
			continue
		}

		dir, err := root.ResolveStage(stage.Dir)
		if err != nil {
			continue
		}

		scripts, err := workflow.Discover(dir)
		if err != nil {
			continue
		}

		for _, script := range scripts {
			if ctx.Err() != nil {
				// Out of time. What was established still refuses; what
				// was not looked at does not.
				return settleRefusal(refusal)
			}

			body, err := script.Read(maxSize)
			if err != nil {
				continue
			}

			report := workflowlint.Report(ctx, script.Name, body)
			if !report.Blocks() {
				continue
			}

			refused := WorkflowRefusedScript{
				ScriptName: script.Name,
				Dir:        stage.Dir,
				Scope:      string(stage.Scope),
				Phase:      string(stage.Phase),
			}
			if report.ParseError != nil {
				refused.ParseError = report.ParseError.Message
				refused.ParseErrorLine = report.ParseError.Line
				refused.ParseErrorCol = report.ParseError.Col
			}
			for _, f := range report.Blocking() {
				refused.Findings = append(refused.Findings, WorkflowLintFinding{
					Code:     f.Code,
					Severity: f.Severity,
					Line:     f.Line,
					Col:      f.Col,
					Message:  f.Message,
				})
			}

			refusal.Scripts = append(refusal.Scripts, refused)
		}
	}

	return settleRefusal(refusal)
}

func settleRefusal(r *WorkflowScriptRefusal) error {
	if len(r.Scripts) == 0 {
		return nil
	}

	return r
}

// globalWorkflowStages are the stages a deployment-wide write changes.
func globalWorkflowStages(cfg *config.Config) []workflow.StageSpec {
	return workflow.PlanStages(
		workflow.StageDirs{Before: cfg.Workflows.Global.BeforeDir, After: cfg.Workflows.Global.AfterDir},
		workflow.StageDirs{},
	)
}

// setWorkflowStages are the stages a per-set write changes.
//
// The set's OWN stages and not its resolved plan: the deployment's
// globals run against this set too, and they were verified when they were
// saved. Re-verifying them here would let a global hook somebody else
// broke refuse an unrelated per-set patch.
func setWorkflowStages(bs *config.BackupSet) []workflow.StageSpec {
	if bs == nil || bs.Workflow == nil {
		return nil
	}

	return workflow.PlanStages(
		workflow.StageDirs{},
		workflow.StageDirs{Before: bs.Workflow.BeforeDir, After: bs.Workflow.AfterDir},
	)
}
