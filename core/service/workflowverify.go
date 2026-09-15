package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/workflow"
	"github.com/retnd/retnd/core/internal/workflowlint"
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

	// BackupSetID names the set whose stage this is, and is empty for a
	// deployment-wide one.
	//
	// It exists because of what a ROOT change does (review BLOCKER 2): a
	// deployment-wide patch that moves workflows.root re-resolves every
	// set's stage directories underneath the new root, so the write has
	// to verify those too -- and a refusal naming "before" in
	// "pg.before.d" without saying whose set that is would send an
	// operator looking through every backup set they have.
	BackupSetID string

	// ParseError, with its position, when the script is not a shell
	// program at all.
	ParseError     string
	ParseErrorLine int
	ParseErrorCol  int

	// ParseErrorExcerpt is the script's own line at that position, so a
	// refusal can show what it is refusing rather than only where.
	ParseErrorExcerpt WorkflowSourceExcerpt

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
		where := s.Scope + " " + s.Phase
		if s.BackupSetID != "" {
			// Whose set, because a root change verifies every set's
			// stages and "set before" alone would not say which one.
			where = s.BackupSetID + ", " + where
		}
		fmt.Fprintf(&b, "\n  %s in %s (%s): ", s.ScriptName, s.Dir, where)

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

// verifiedStage is one stage directory the gate will visit, and whose
// set it belongs to.
//
// The set id is carried alongside rather than derived, because a stage
// cannot say whose it is: workflow.StageSpec has a scope ("set") and a
// directory, and two backup sets may name the same directory. A refusal
// has to be able to say which set it is about; see
// WorkflowRefusedScript.BackupSetID.
type verifiedStage struct {
	backupSetID string
	spec        workflow.StageSpec
}

// refuseUnverifiableScripts is the gate, applied to the stages a write
// leaves in force.
//
// stages are the ones the RESULTING configuration declares for the scope
// being written, and the callers decide that scope:
//
//   - a per-set patch verifies that set's own stages. Verifying the
//     deployment's globals as well would let a global hook somebody else
//     broke refuse an unrelated per-set patch;
//   - a deployment-wide patch verifies the global stages -- and, when it
//     moves workflows.ROOT, every set-owned stage too, because that one
//     field re-resolves all of them underneath a new directory. That was
//     review BLOCKER 2: without it, moving the root re-pointed every
//     set's hooks at whatever is under the new tree and the gate looked
//     at none of them.
func refuseUnverifiableScripts(ctx context.Context, cfg *config.Config, stages []verifiedStage) error {
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
		if stage.spec.Dir == "" {
			continue
		}

		dir, err := root.ResolveStage(stage.spec.Dir)
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

			refusal.Scripts = append(refusal.Scripts, refusedScript(stage, script.Name, body, report))
		}
	}

	return settleRefusal(refusal)
}

// refusedScript renders one blocking verdict, with the script's own
// lines at the positions it names.
//
// The excerpt comes from the bytes that were just read and verified,
// which is the only source that cannot disagree with the positions: a
// re-read could land on a file somebody edited in between, and a refusal
// pointing at line 4 of a file whose line 4 is now something else is
// worse than one showing no line at all.
func refusedScript(stage verifiedStage, name string, body []byte, report workflowlint.ScriptReport) WorkflowRefusedScript {
	out := WorkflowRefusedScript{
		ScriptName:  name,
		Dir:         stage.spec.Dir,
		Scope:       string(stage.spec.Scope),
		Phase:       string(stage.spec.Phase),
		BackupSetID: stage.backupSetID,
	}

	if report.ParseError != nil {
		out.ParseError = report.ParseError.Message
		out.ParseErrorLine = report.ParseError.Line
		out.ParseErrorCol = report.ParseError.Col
		out.ParseErrorExcerpt = toWorkflowSourceExcerpt(workflowlint.Excerpt(body, report.ParseError.Line))
	}

	for _, f := range report.Blocking() {
		out.Findings = append(out.Findings, toWorkflowLintFinding(f, body))
	}

	return out
}

func settleRefusal(r *WorkflowScriptRefusal) error {
	if len(r.Scripts) == 0 {
		return nil
	}

	return r
}

// globalWorkflowStages are the deployment-wide stages.
func globalWorkflowStages(cfg *config.Config) []verifiedStage {
	return withSet("", workflow.PlanStages(
		workflow.StageDirs{Before: cfg.Workflows.Global.BeforeDir, After: cfg.Workflows.Global.AfterDir},
		workflow.StageDirs{},
	))
}

// setWorkflowStages are one set's own stages.
//
// The id is passed in rather than read off the set, and that is not
// tidiness: a configuration freshly loaded for WRITING has not had its
// composite ids resolved (findBackupSetForWrite matches on the source and
// set NAMES for exactly that reason), so bs.ID.String() on this path is
// "/" and a refusal built from it would name no set at all.
func setWorkflowStages(backupSetID string, bs *config.BackupSet) []verifiedStage {
	if bs == nil || bs.Workflow == nil {
		return nil
	}

	return withSet(backupSetID, workflow.PlanStages(
		workflow.StageDirs{},
		workflow.StageDirs{Before: bs.Workflow.BeforeDir, After: bs.Workflow.AfterDir},
	))
}

// everyWorkflowStage is what a ROOT change has to verify: the global
// stages and every set-owned one, under the resulting configuration.
//
// # Why a root change is the one deployment-wide patch that reaches every
// set
//
// Because a stage directory is a NAME inside the root, usually relative.
// Moving the root does not change one line of any set's own block and
// still re-points every one of those names at a different directory, so
// the scripts a set would run after the save are scripts this deployment
// has never verified. A gate that looked only at the global stages would
// let a save do exactly what the gate exists to prevent, which is review
// BLOCKER 2.
//
// The order is deterministic -- globals, then sets in configuration
// order, then each set's stages in execution order -- so two attempts at
// the same save produce the same refusal in the same order.
func everyWorkflowStage(cfg *config.Config) []verifiedStage {
	stages := globalWorkflowStages(cfg)

	for i := range cfg.Sources {
		for j := range cfg.Sources[i].BackupSets {
			id := cfg.Sources[i].Name + "/" + cfg.Sources[i].BackupSets[j].Name
			stages = append(stages, setWorkflowStages(id, &cfg.Sources[i].BackupSets[j])...)
		}
	}

	return stages
}

func withSet(backupSetID string, specs []workflow.StageSpec) []verifiedStage {
	out := make([]verifiedStage, 0, len(specs))
	for _, spec := range specs {
		out = append(out, verifiedStage{backupSetID: backupSetID, spec: spec})
	}

	return out
}
