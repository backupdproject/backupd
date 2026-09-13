package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/service"
)

// `workflow`: reading a hook run back, and dealing with one this product
// could not finish (EPIC L, #813).
//
// It is the inspection half of the workflow surface. The CONFIGURATION
// half lives under the nouns it belongs to -- `settings workflow` for the
// deployment-wide block, `backup-set workflow` for one set's, `validate
// workflow` for the report on both -- because an operator looking for
// "where do I set the hook timeout" looks under settings, not under a
// third noun that happens to share a word. What is here is everything
// that is about a RUN: what happened, what one step printed, and what is
// still owed.
//
// # Why the run reads answer from this host's journal
//
// Because that is where a workflow run IS. #813's three statuses, every
// step, every captured byte and the recovery axis are durable rows
// (internal/state's workflow tables), written by whichever process
// executed the run and readable by anything that can open the journal.
// So `workflow run list` beside a serving engine reads the same rows that
// engine wrote, rather than needing a route to it, which is the same
// position `activity` without --follow is in and for the same reason.
//
// One read is genuinely different and says so: `workflow recovery show`.
// See workflowRecoveryShow's own doc, which is the longest thing in this
// file, because the honest answer there cost an argument.
//
// # Why the two actions are not configuration writes
//
// `resume-cleanup` and `acknowledge` change the journal and never
// config.yaml, so they come through openBackupService with readsConfig,
// exactly as `restore` does (mode.go's own doc names restore as the
// precedent: "declared readsConfig and writes an operation row, which is
// true of the configuration and not of the deployment"). Neither is
// refused beside a serving engine, and neither may be: a run that left a
// machine quiesced is at its most dangerous precisely when the engine
// that abandoned it has been restarted and is serving again.
//
// What keeps two processes from unwinding one run at once is the journal
// rather than anything here. workflowrun's obligation state machine moves
// a scope to in_progress durably before it executes a hook, and refuses a
// second caller that finds it there, so a resume typed at a terminal and
// a resume clicked in a browser serialise on a row rather than racing.

// workflowNouns is the two things `workflow` has verbs about.
//
// A table rather than a switch, for workflowRunnerVerbs' reason:
// TestUsage_EveryRegisteredCommandIsPinned reads main.go's map, which has
// one entry for `workflow` and cannot see a level below it, so a verb
// added here with no line in usage() would be dispatchable,
// undiscoverable and pinned by nothing. TestUsage_NamesEveryWorkflowVerb
// holds every leaf of this tree against usage().
//
// Two levels, because `workflow run log` is three words and the middle
// one is a noun rather than a verb. Flattening it to `workflow run-log`
// would read as a fourth thing an operator has to learn instead of as the
// log of the run they were just looking at.
var workflowNouns = map[string]map[string]workflowVerb{
	"run":      workflowRunVerbs,
	"recovery": workflowRecoveryVerbs,
}

// workflowVerb is one leaf verb: the operand shape usage() spells, and
// the body that runs once the two nouns in front of it have been
// dispatched.
type workflowVerb struct {
	// operand is what this verb takes after its own name, spelled the way
	// usage() spells it. Empty for a verb that takes none.
	operand string

	run func(args []string) int
}

var workflowRunVerbs = map[string]workflowVerb{
	"list":  {run: workflowRunList},
	"show":  {operand: "<run-id>", run: workflowRunShow},
	"steps": {operand: "<run-id>", run: workflowRunSteps},
	"log":   {operand: "<run-id>", run: workflowRunLog},
}

var workflowRecoveryVerbs = map[string]workflowVerb{
	"show":           {run: workflowRecoveryShow},
	"resume-cleanup": {operand: "<run-id>", run: workflowResumeCleanup},
	"acknowledge":    {operand: "<run-id>", run: workflowAcknowledge},
}

// workflowNounNames and workflowVerbNames return the two levels in a
// stable order, so a refusal that lists them reads the same way twice.
func workflowNounNames() []string {
	names := make([]string, 0, len(workflowNouns))
	for noun := range workflowNouns {
		names = append(names, noun)
	}
	sort.Strings(names)

	return names
}

func workflowVerbNames(verbs map[string]workflowVerb) []string {
	names := make([]string, 0, len(verbs))
	for verb := range verbs {
		names = append(names, verb)
	}
	sort.Strings(names)

	return names
}

// workflowVerbShapes is workflowVerbNames with each verb's operand,
// which is what a refusal should say: "log" on its own tells an operator
// the word and not that it needs a run id, and mediumOperandShapes makes
// the same argument one command over. Rendered from the table rather
// than typed, so a verb added there turns up in the refusal without a
// second edit somebody has to remember.
func workflowVerbShapes(verbs map[string]workflowVerb) string {
	names := workflowVerbNames(verbs)
	shapes := make([]string, 0, len(names))
	for _, name := range names {
		if operand := verbs[name].operand; operand != "" {
			shapes = append(shapes, name+" "+operand)

			continue
		}
		shapes = append(shapes, name)
	}

	return strings.Join(shapes, ", ")
}

// cmdWorkflow dispatches on the two words in front of the verb's own
// flags.
//
// The nouns come first and are read straight off argv rather than through
// a flag set, exactly as `workflow-runner`'s do, and the reason is that
// every leaf verb owns its own flags: --step and --follow belong to one
// verb, --reason to another, and a flag set that parsed before the verb
// was known would have to declare the union of all of them, which is how
// `medium` ends up refusing another verb's flags by hand. What that costs
// is that a flag written BEFORE the nouns (`workflow --json run list`) is
// refused as an unknown verb rather than parsed; the leaf verbs
// themselves accept flags on either side of their operand
// (parseFlagsAroundOperands), which is the form usage() describes and the
// form the Web UI echoes.
func cmdWorkflow(args []string) int {
	if len(args) == 0 {
		return usageError("workflow needs a noun: %s", strings.Join(workflowNounNames(), ", "))
	}

	noun, rest := args[0], args[1:]
	verbs, ok := workflowNouns[noun]
	if !ok {
		return usageError("workflow has no %q noun. It has %s", noun, strings.Join(workflowNounNames(), ", "))
	}
	if len(rest) == 0 {
		return usageError("workflow %s needs a verb: %s", noun, workflowVerbShapes(verbs))
	}

	verb, verbArgs := rest[0], rest[1:]
	entry, ok := verbs[verb]
	if !ok {
		return usageError("workflow %s has no %q verb. It has %s", noun, verb, workflowVerbShapes(verbs))
	}

	return entry.run(verbArgs)
}

// openWorkflowService is the door every workflow verb in this binary
// comes through, and the one place this binary's version reaches the
// service.
//
// SetBuildVersion is not decoration. The engine and the host workflow
// runner are one program cut in half by a socket
// (workflowrunner.go's own preamble), and the runner refuses a hello
// whose version is not its own, so a process that never states a version
// is refused by that check. Three things here reach the runner -- the
// local-script half of `validate workflow`, a `resume-cleanup` that
// unwinds a `.local.sh` hook, and nothing else -- and all three would
// otherwise fail against a perfectly healthy runner for a reason no
// operator could act on. The version is set for every verb rather than
// for those three, because "which verbs might reach the runner" is a fact
// about core/service that this file must not encode a second copy of.
func openWorkflowService(ctx context.Context, configPath string, intent configIntent) (*service.BackupService, func(), error) {
	svc, cleanup, err := openBackupService(ctx, configPath, intent)
	if err != nil {
		return nil, cleanup, err
	}
	svc.SetBuildVersion(version)

	return svc, cleanup, nil
}

// oneRunOperand reads the single run id a verb takes, refusing a command
// line that named none or named two before anything is opened.
//
// A run id is an opaque string this product minted, so unlike a backup
// set id there is no shape to check here: anything non-empty could be
// one, and whether it names a run is a question about the journal that
// only the journal can answer (ErrWorkflowRunNotFound, an ordinary
// failure, exit 1). What IS settled here is the arity, which is wrong on
// every deployment rather than on this one and therefore a 2.
func oneRunOperand(fs *flag.FlagSet, args []string, verb string) (string, int) {
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return "", exitUsage
	}
	if len(operands) != 1 {
		return "", usageError("workflow %s takes exactly one argument: <run-id>", verb)
	}

	return operands[0], exitOK
}

// noOperands is oneRunOperand for the verbs that take none.
func noOperands(fs *flag.FlagSet, args []string, verb string) int {
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(operands) > 0 {
		return usageError("workflow %s takes no argument; %q is one", verb, operands[0])
	}

	return exitOK
}

// workflowRunList is `workflow run list [--backup-set ID] [--limit N]`:
// the runs this deployment has on record, newest first.
func workflowRunList(args []string) int {
	fs, cfgPath := newFlagSet("workflow run list")
	setFlag := fs.String("backup-set", "", "only this backup set's runs, named <source/backup-set>; left out, every set's")
	limitFlag := fs.Int("limit", 0, "read at most this many runs (the journal's own maximum is 100, which is also the default)")
	asJSON := fs.Bool("json", false, "emit the runs as JSON instead of as a table")
	if code := noOperands(fs, args, "run list"); code != exitOK {
		return code
	}
	// The id's SHAPE is wrong on every deployment rather than on this
	// one, so it is refused here, the same rule `activity --backup-set`
	// applies to the identical flag. Whether the set exists is
	// deliberately not asked: a filter that matches nothing prints
	// nothing, because a set removed from the configuration still has
	// runs in the journal and this is exactly where somebody looks for
	// them.
	if *setFlag != "" {
		if _, _, ok := splitBackupSetID(*setFlag); !ok {
			return usageError("workflow run list: --backup-set %q is not a backup set id; a backup set id is exactly source/name", *setFlag)
		}
	}
	if *limitFlag < 0 {
		return usageError("workflow run list: --limit %d is not a count", *limitFlag)
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, *cfgPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	runs, err := svc.WorkflowRuns(ctx, *setFlag, *limitFlag)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		return printWorkflowJSON(struct {
			Runs []service.WorkflowRunDetail `json:"runs"`
		}{nonNilRuns(runs)})
	}
	if len(runs) == 0 {
		fmt.Println("no workflow run is on record here" + forTheSet(*setFlag) +
			". A deployment that configures no hooks records none, and so does one whose hooks have never run yet; `" +
			cliecho.Binary + " backup-set workflow <source/backup-set>` says which it is")

		return 0
	}
	for _, run := range runs {
		printWorkflowRunSummary(run)
	}

	return 0
}

// forTheSet is the fragment that names a filter in a sentence about
// having found nothing, and says nothing at all when there was no filter.
func forTheSet(id string) string {
	if id == "" {
		return ""
	}

	return " for " + id
}

// workflowRunShow is `workflow run show <run-id>`: one run in full.
func workflowRunShow(args []string) int {
	fs, cfgPath := newFlagSet("workflow run show")
	asJSON := fs.Bool("json", false, "emit the run as JSON instead of as text")
	runID, code := oneRunOperand(fs, args, "run show")
	if code != exitOK {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, *cfgPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	run, err := svc.WorkflowRun(ctx, runID)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		return printWorkflowJSON(run)
	}
	printWorkflowRunDetail(run)

	return 0
}

// workflowRunSteps is `workflow run steps <run-id>`: the plan, in
// execution order, with what became of each step.
func workflowRunSteps(args []string) int {
	fs, cfgPath := newFlagSet("workflow run steps")
	asJSON := fs.Bool("json", false, "emit the steps as JSON instead of as a table")
	runID, code := oneRunOperand(fs, args, "run steps")
	if code != exitOK {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, *cfgPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	steps, err := svc.WorkflowSteps(ctx, runID)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		return printWorkflowJSON(struct {
			RunID string                       `json:"run_id"`
			Steps []service.WorkflowStepDetail `json:"steps"`
		}{runID, nonNilSteps(steps)})
	}
	if len(steps) == 0 {
		fmt.Printf("workflow run %s declared no steps, which is what a run of a backup set with no hook scripts records\n", runID)

		return 0
	}
	for _, step := range steps {
		printWorkflowStep(step)
	}

	return 0
}

// workflowRunLog is `workflow run log <run-id> --step <step-id>`: one
// step's captured output, from a cursor, optionally followed.
//
// The step is a FLAG rather than a second operand, and that is worth a
// sentence because it is the one place this noun's grammar could have
// gone either way. A run id and a step id are both opaque strings, so two
// bare operands would be two values nothing could tell apart if they were
// given in the wrong order: `log STEP RUN` would be refused by the
// journal as an unknown run rather than by this command as a swap. Naming
// one of them makes the mistake impossible to make silently.
func workflowRunLog(args []string) int {
	fs, cfgPath := newFlagSet("workflow run log")
	stepFlag := fs.String("step", "", "the step whose output to read, as `workflow run steps` prints it (required)")
	cursorFlag := fs.Int64("cursor", 0, "read only records after this sequence; 0 reads from the beginning. This is the value a previous read printed as its cursor, so a follow that was interrupted resumes exactly where it stopped")
	followFlag := fs.Bool("follow", false, "keep reading until the step's output ends, waiting for more rather than returning at the end of the current page")
	limitFlag := fs.Int("limit", 0, "read at most this many records per page (the service clamps this to its own maximum)")
	asJSON := fs.Bool("json", false, "emit one JSON record per line, so a script reads this with a line reader rather than an incremental JSON parser")
	runID, code := oneRunOperand(fs, args, "run log")
	if code != exitOK {
		return code
	}
	// Required rather than defaulted to the run's first step. A run's
	// steps are the operator's own scripts and picking one for them would
	// be this command choosing which hook they meant to read.
	if strings.TrimSpace(*stepFlag) == "" {
		return usageError("workflow run log: --step names the step whose output to read; `%s workflow run steps %s` lists them", cliecho.Binary, runID)
	}
	if *cursorFlag < 0 {
		return usageError("workflow run log: --cursor %d is not a sequence; sequences start at 1 and 0 means from the beginning", *cursorFlag)
	}
	if *limitFlag < 0 {
		return usageError("workflow run log: --limit %d is not a count", *limitFlag)
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, *cfgPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	req := service.WorkflowStepLogRequest{
		RunID:  runID,
		StepID: *stepFlag,
		After:  uint64(*cursorFlag),
		Limit:  *limitFlag,
	}

	if !*followFlag {
		page, err := svc.WorkflowStepLogs(ctx, req)
		if err != nil {
			return fail(err)
		}
		printStepLogPage(os.Stdout, page, *asJSON)
		printStepLogFooter(stepLogFooterWriter(os.Stdout, os.Stderr, *asJSON), page)

		return 0
	}

	// Its own context and its own signal handling, the shape `activity
	// --follow` already uses: this is an invocation that does not end on
	// its own, and an operator's Ctrl-C is how they end it rather than a
	// failure.
	followCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if _, err := followStepLogs(followCtx, svc, req, *asJSON, os.Stdout, os.Stderr); err != nil {
		return fail(err)
	}

	return 0
}

// stepLogSource is the one call a follow makes, named as an interface so
// the loop can be driven without a journal.
//
// A loop that only ends when a real step finishes cannot be tested at
// all, and a test that could not drive it would be a test of the flag
// parsing in front of it -- followActivity's own argument for taking a
// context and two writers. What this seam buys beyond that is the one
// property #813 actually requires: that a page sequence split across
// pages, with an interruption in the middle, is printed once each with no
// gap. That is a property of the cursor arithmetic below and of nothing
// else, so it is worth being able to drive it directly.
type stepLogSource interface {
	WorkflowStepLogs(ctx context.Context, req service.WorkflowStepLogRequest) (service.WorkflowStepLogPage, error)
}

// followStepLogs prints one step's output until it ends, and reports the
// cursor it reached.
//
// # The cursor rule, which is the whole of it
//
// After is the last sequence this loop PROCESSED, and it advances only to
// what the page says its cursor is (service.WorkflowStepLogPage.Cursor,
// which echoes the caller's own After on an empty page so a follower does
// not forget where it was). Nothing here derives a cursor from the
// records it printed: the run's sequence counter is per RUN and a page
// filtered to one step legitimately advances the cursor past records
// belonging to other steps, so a follower that took max(printed) would
// re-read the same window forever on a run whose later steps are noisy.
// That is readStepLogPage's own reasoning read from this end.
//
// # Why the cursor is RETURNED
//
// Because a follow can stop without the output ending: a journal read can
// fail, and an operator can Ctrl-C. The cursor is the only thing that
// makes the next read continue rather than start again, so it is handed
// back, and the caller prints it on a failure. An operator who lost a
// terminal resumes with --cursor N and sees no line twice and no line
// missed, which is the same protocol the engine's own followers use and
// the reason the service takes After rather than holding a stream open.
//
// It returns nil on cancellation, for followActivity's reason: a follow
// an operator ended did what it was asked.
func followStepLogs(ctx context.Context, src stepLogSource, req service.WorkflowStepLogRequest, asJSON bool, out, errOut io.Writer) (uint64, error) {
	// The wait is what makes this a follow rather than a poll loop: the
	// service blocks, briefly and boundedly, for output newer than the
	// cursor before answering empty (workflowLogWaitCeiling clamps
	// anything longer). Asking for the ceiling rather than for a shorter
	// interval is deliberate -- a hook that prints a line a minute should
	// cost one request every few seconds, not one every tick of a timer
	// this file chose.
	req.Wait = followStepLogWait

	for {
		page, err := src.WorkflowStepLogs(ctx, req)
		if err != nil {
			// A cancelled context surfaces here as a failed read and is
			// not one: the operator ended the follow.
			if ctx.Err() != nil {
				return req.After, nil
			}

			_, _ = fmt.Fprintf(errOut, "%sthe read stopped at sequence %d; resume it with --cursor %d and nothing will be printed twice or skipped\n",
				modeLinePrefix, req.After, req.After)

			return req.After, err
		}

		printStepLogPage(out, page, asJSON)

		// Only ever forward. A service that answered with a lower cursor
		// than the caller sent would otherwise make this loop re-print
		// what it has already printed, which is the one failure a
		// follower can produce that looks like data rather than like a
		// bug.
		if page.Cursor > req.After {
			req.After = page.Cursor
		}

		if page.Complete {
			printStepLogFooter(stepLogFooterWriter(out, errOut, asJSON), page)

			return req.After, nil
		}

		if ctx.Err() != nil {
			return req.After, nil
		}
	}
}

// followStepLogWait is how long each read may wait for new output.
//
// It asks for the service's own ceiling (workflowLogWaitCeiling, five
// seconds) rather than naming a smaller number, so the pacing is the
// service's decision and not this file's. A value above the ceiling is
// clamped there, which is why asking for exactly it is safe rather than
// merely tolerated.
const followStepLogWait = 5 * time.Second

// printStepLogPage writes one page's records.
//
// The stream is printed per record and never merged, because a hook that
// writes progress to stdout and errors to stderr is telling an operator
// something and a merged log throws it away
// (service.WorkflowStepLogRecord.Stream's own rule). The truncation
// marker is rendered differently from output for a sharper reason: its
// text is THIS PRODUCT's sentence, not the hook's, and a line that looked
// like the rest would attribute our words to somebody's script.
func printStepLogPage(out io.Writer, page service.WorkflowStepLogPage, asJSON bool) {
	for _, rec := range page.Records {
		if asJSON {
			// One object per line, activity --follow's own shape, so a
			// script tails this with a line reader. An error writing to
			// stdout is not reported here: a closed pipe is the ordinary
			// way `| head` ends a follow, and a diagnostic about it would
			// be printed to a terminal nobody is watching.
			line, err := json.Marshal(rec)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintln(out, string(line))

			continue
		}

		if rec.Kind != string(workflow.LogOutput) {
			_, _ = fmt.Fprintf(out, "%s  [%s] %s: %s\n", workflowClock(rec.At), rec.Kind, cliecho.Binary, rec.Text)

			continue
		}
		_, _ = fmt.Fprintf(out, "%s  %-6s %s\n", workflowClock(rec.At), rec.Stream, rec.Text)
	}
}

// printStepLogFooter says where the read got to and what it could not
// show.
//
// The cursor is printed on every read rather than only on a failure,
// because it is what makes a later read continue: a caller that piped
// this into a file and wants the rest asks for --cursor N. The
// truncation is printed whenever the log carries a marker at or before
// the cursor, which is a property of the LOG and not of this page
// (WorkflowStepLogPage.Truncated's own doc), so a follower that joined
// late is still told the record is incomplete.
func printStepLogFooter(out io.Writer, page service.WorkflowStepLogPage) {
	_, _ = fmt.Fprintf(out, "cursor: %d  step: %s  state: %s\n", page.Cursor, page.StepID, page.StepState)
	if page.Truncated {
		_, _ = fmt.Fprintf(out, "this step's output was not recorded in full: the persisted-output bound was reached, so what is above is the beginning of it and the hook went on printing\n")
	}
	if !page.Complete {
		_, _ = fmt.Fprintf(out, "the step has not finished, so there may be more: read it again with --cursor %d, or use --follow\n", page.Cursor)
	}
}

// stepLogFooterWriter decides where the footer's prose goes.
//
// Under --json it goes to STDERR, and that is not tidiness: the stdout of
// a --json read is one record object per line, for a script reading it
// with a line reader, and a sentence in the middle of that stream breaks
// every such consumer. It is the identical rule activity --follow applies
// to its own out-of-band notice ("said on stderr, so it never lands in a
// piped feed") and the one cycleExit states for this binary's event
// stream.
//
// A human reading a terminal sees the cursor either way, because both
// streams land there. A script that wants to resume a --json read has the
// Seq of the last record it processed, which the service's own cursor
// contract accepts as an After: the page's cursor can only be at or ahead
// of that record, so resuming from it re-reads a window that yields this
// step nothing rather than repeating a record.
func stepLogFooterWriter(out, errOut io.Writer, asJSON bool) io.Writer {
	if asJSON {
		return errOut
	}

	return out
}

// workflowRecoveryShow is `workflow recovery show`: every run that is
// still owed something, oldest first.
//
// # Why this does not call WorkflowRecovery
//
// It is the obvious call and it would be wrong here, which is worth
// spelling out at length because the next person to read this file will
// reach for it.
//
// service.WorkflowRecovery returns the ENGINE's own in-memory refusal
// set: the map workflowrun.Engine builds during its startup
// reconciliation and moves only through writes made in that same process
// (internal/workflowrun/recovery.go says so in as many words: "why the
// refusal is in memory and the truth is on disk"). A serving engine's
// answer is therefore exact and free. This binary's answer would be
// EMPTY, always, because the BackupService a CLI verb opens builds a
// fresh engine that has reconciled nothing -- so `workflow recovery show`
// would print "nothing outstanding" at an operator whose source machine
// is sitting quiesced. That is the single worst output this command could
// produce, and it would be produced by the API call whose name says it is
// the right one.
//
// The fix that suggests itself is worse. Calling ReconcileWorkflows here
// would populate that map, and a reconciliation pass from a CLI process
// would also mark a SERVING engine's in-flight run as interrupted: the
// "is this set locked" guard that protects a live run is an in-memory
// lock table belonging to the process executing it (Engine.setIsLocked),
// so a second process cannot see it and reconciles straight over the top.
// The result would be a `show` that blocks a backup set underneath a
// backup that is still running. A read verb that writes is bad enough; a
// read verb that can block the deployment it was asked about is not
// shippable.
//
// So this reads the DURABLE truth, which is what the reconciliation reads
// too: the run rows themselves. A run whose recovery state is unsettled
// is owed a cleanup or a person; a run still in a non-terminal state is
// either executing right now or was abandoned by a process that is gone,
// and the journal alone cannot tell those two apart -- so both are
// reported, under headings that say which is which, rather than one of
// them being guessed at.
//
// What this cannot report, and the reason it is not a gap worth closing
// here: the per-SCOPE breakdown and the spool path
// (service.WorkflowRecoveryHold's Scope and SpoolRef). Those come off the
// hold, which is the engine's, and the surface that has them is the
// serving process's own -- its API and the browser reading it. An
// operator here gets the run, the set, how long it has been like that and
// the two actions, which is everything needed to act; whoever wants to
// read the hooks a resume would execute has the run id to ask that
// process with.
//
// # Why oldest first
//
// Every other run read in this file is newest first, because the question
// is "what just happened". This one's question is "how long has a machine
// been left like this", and the answer an operator has to act on first is
// the oldest one.
func workflowRecoveryShow(args []string) int {
	fs, cfgPath := newFlagSet("workflow recovery show")
	asJSON := fs.Bool("json", false, "emit the outstanding runs as JSON instead of as text")
	if code := noOperands(fs, args, "recovery show"); code != exitOK {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, *cfgPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	outstanding, inFlight, err := outstandingWorkflowRuns(ctx, svc)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		return printWorkflowJSON(struct {
			Outstanding []service.WorkflowRunDetail `json:"outstanding"`
			InFlight    []service.WorkflowRunDetail `json:"in_flight"`
		}{nonNilRuns(outstanding), nonNilRuns(inFlight)})
	}

	if len(outstanding) == 0 {
		fmt.Println("no workflow run is waiting for a cleanup or for a person: every run this journal holds has settled its recovery")
	}
	for _, run := range outstanding {
		printWorkflowRecoveryRun(run)
	}
	for _, run := range inFlight {
		fmt.Printf("in flight or abandoned: run %s  set %s  state %s  started %s\n",
			run.RunID, run.BackupSetID, run.State, workflowClock(run.StartedAt))
		fmt.Println("  this journal cannot tell those two apart: a run is recorded as running until the process executing it records an outcome, and a process that died recorded none. The next engine start decides, by finding it with nothing holding it, and moves it to recovery_required")
	}

	return 0
}

// outstandingWorkflowRuns separates the runs that are owed something from
// the ones that may simply be running.
//
// # Why the read is per backup set as well as deployment-wide
//
// The journal bounds one history read at a hundred rows
// (internal/state's maxWorkflowRunHistory), and a deployment-wide read
// therefore returns the newest hundred runs across every set. On a busy
// deployment an old blocked run could be pushed out of that window by
// newer runs belonging to OTHER sets, and this is the one read where
// missing a row means telling an operator a machine is fine.
//
// A per-set read cannot have that problem, and the reason is the feature
// itself: a set with an unresolved interruption is REFUSED its next run
// (Engine.SuspendedBackupSets), so no newer run of that set can
// accumulate above the blocked one. Its interrupted run is therefore
// always inside that set's own newest hundred.
//
// Both passes are made rather than only the per-set one, because a run
// whose backup set has since been removed from the configuration belongs
// to no set this loop would ask about, and it is still a run that may
// have left a machine quiesced.
func outstandingWorkflowRuns(ctx context.Context, svc *service.BackupService) (outstanding, inFlight []service.WorkflowRunDetail, err error) {
	seen := map[string]bool{}
	consider := func(runs []service.WorkflowRunDetail) {
		for _, run := range runs {
			if seen[run.RunID] {
				continue
			}
			seen[run.RunID] = true

			switch {
			case !workflow.RecoveryState(run.RecoveryState).Settled():
				outstanding = append(outstanding, run)
			case !workflow.State(run.State).Terminal():
				inFlight = append(inFlight, run)
			}
		}
	}

	deploymentWide, err := svc.WorkflowRuns(ctx, "", 0)
	if err != nil {
		return nil, nil, err
	}
	consider(deploymentWide)

	sets, err := svc.ListBackupSets(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, set := range sets {
		runs, err := svc.WorkflowRuns(ctx, set.ID, 0)
		if err != nil {
			return nil, nil, err
		}
		consider(runs)
	}

	sort.Slice(outstanding, func(i, j int) bool { return outstanding[i].StartedAt.Before(outstanding[j].StartedAt) })
	sort.Slice(inFlight, func(i, j int) bool { return inFlight[i].StartedAt.Before(inFlight[j].StartedAt) })

	return outstanding, inFlight, nil
}

// workflowResumeCleanup is `workflow recovery resume-cleanup <run-id>`:
// run what that interrupted run still owes.
//
// Everything it executes comes out of the run's own spool and is
// re-verified against the sha256 recorded when the plan was captured
// (service.ResumeWorkflowCleanup's own doc), so an operator who has
// edited the hook directory since the interruption has changed nothing
// about what this runs. That property is why this is safe to offer from a
// terminal at all.
func workflowResumeCleanup(args []string) int {
	fs, cfgPath := newFlagSet("workflow recovery resume-cleanup")
	asJSON := fs.Bool("json", false, "emit the run as JSON instead of as text")
	runID, code := oneRunOperand(fs, args, "recovery resume-cleanup")
	if code != exitOK {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, *cfgPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	run, err := svc.ResumeWorkflowCleanup(ctx, runID)
	if err != nil {
		return fail(err)
	}

	if *asJSON {
		return printWorkflowJSON(run)
	}
	printWorkflowRunDetail(run)

	// The verdict is read off the row this command just read back rather
	// than off the call returning nil, because a resume that ran every
	// hook and still could not account for a scope leaves the set
	// blocked, and the operator's next backup will be refused. Reporting
	// success there would be this command saying the thing it was
	// supposed to fix is fixed.
	if !workflow.RecoveryState(run.RecoveryState).Settled() {
		fmt.Printf("this run is still not settled: its recovery state is %s, so the backup set stays blocked. Read the steps above, then either resume again or acknowledge it with `%s workflow recovery acknowledge %s --reason \"...\"`\n",
			run.RecoveryState, cliecho.Binary, run.RunID)

		return 1
	}

	return 0
}

// workflowAcknowledge is `workflow recovery acknowledge <run-id>
// --reason "..."`: a person taking responsibility for a run this product
// cannot account for.
//
// # Why a missing --reason is a usage error and not a service refusal
//
// The service refuses it too (ErrWorkflowAcknowledgeReasonRequired), and
// that refusal is the one that matters: it is what stops every other
// surface shipping a button with no reason box. But reaching it from here
// would mean opening the journal, taking the claim, resolving the run and
// then complaining about the command line -- and beside a serving engine
// an operator who forgot a flag could meet a completely different
// message on the way. The command line is wrong on every deployment, so
// it is refused before anything is opened, which is exactly the rule
// `settings patch` with no patch flag follows and the reason usage()'s
// exit-code table puts "a missing or surplus argument" at 2.
//
// The ACTOR is not a flag, and is not going to be one. There is no login
// on this surface: reaching this binary means reaching the host, and the
// authority it acts with is the same one `settings patch` acts with. So
// it records cliActor, the same string a run submitted from a terminal
// records, rather than believing a --actor somebody could type anything
// into.
func workflowAcknowledge(args []string) int {
	fs, cfgPath := newFlagSet("workflow recovery acknowledge")
	reason := fs.String("reason", "", "what you did about it, or why it does not matter (required). It is recorded durably against the run, because the whole value of an acknowledgement is answering, six months later, why a backup set was unblocked without its cleanup ever running")
	runID, code := oneRunOperand(fs, args, "recovery acknowledge")
	if code != exitOK {
		return code
	}
	if strings.TrimSpace(*reason) == "" {
		return usageError("workflow recovery acknowledge: --reason is required, and a blank one is the same as none. It records what was done about a run whose cleanup never ran, which is the difference between an operator who checked the machine and an alarm somebody silenced")
	}

	ctx := context.Background()
	svc, cleanup, err := openWorkflowService(ctx, *cfgPath, readsConfig)
	defer cleanup()
	if err != nil {
		return fail(err)
	}

	if err := svc.AcknowledgeWorkflowRecovery(ctx, runID, service.WorkflowAcknowledgement{
		Actor:  cliActor,
		Reason: *reason,
	}); err != nil {
		return fail(err)
	}

	fmt.Printf("workflow run %s acknowledged\n", runID)
	fmt.Printf("  by: %s\n", cliActor)
	fmt.Printf("  reason: %s\n", *reason)
	fmt.Println("  the backup set may run again. Nothing was executed: an acknowledgement records that a person dealt with the scope this product could not account for")

	return 0
}

// printWorkflowRunSummary is one line per run, which is what a list is
// for: the three statuses, whether the hooks were bypassed, and when.
//
// The three are printed separately and are never folded into one verdict.
// That is #810 and #811's rule and the reason the journal has three
// columns: "the backup succeeded and the cleanup did not" is the single
// most operationally important thing this feature can say -- it means a
// machine may be sitting quiesced with a good backup beside it -- and a
// surface that printed one summary word would make exactly that case
// unsayable.
func printWorkflowRunSummary(run service.WorkflowRunDetail) {
	fmt.Printf("%-28s %-22s %-18s backup=%-10s cleanup=%-10s workflow=%-10s %s\n",
		run.RunID, run.BackupSetID, run.State,
		run.BackupStatus, run.CleanupStatus, run.WorkflowStatus,
		workflowClock(run.StartedAt))
	if run.Bypassed {
		fmt.Println("  hooks bypassed: this run took the backup and ran none of this set's scripts")
	}
	if !workflow.RecoveryState(run.RecoveryState).Settled() {
		fmt.Printf("  recovery %s: this run is blocking its backup set\n", run.RecoveryState)
	}
}

// printWorkflowRunDetail is the `show` form: everything the row carries,
// plus every step.
func printWorkflowRunDetail(run service.WorkflowRunDetail) {
	fmt.Printf("workflow run %s\n", run.RunID)
	fmt.Printf("  backup set: %s\n", run.BackupSetID)
	fmt.Printf("  state: %s\n", run.State)
	// One line each, deliberately, rather than three values on one line:
	// an operator greps this output for "cleanup" when they are trying to
	// find out whether a machine was left quiesced.
	fmt.Printf("  backup status: %s\n", run.BackupStatus)
	fmt.Printf("  cleanup status: %s\n", run.CleanupStatus)
	fmt.Printf("  workflow status: %s\n", run.WorkflowStatus)
	fmt.Printf("  recovery: %s\n", run.RecoveryState)
	fmt.Printf("  hooks bypassed: %v\n", run.Bypassed)
	if run.Bypassed {
		fmt.Println("    this run was asked to skip this set's scripts, and the request is recorded here durably as well as in a warn-level event naming who asked")
	}
	fmt.Printf("  scripts: %d\n", run.ScriptCount)
	if run.FailedStep != "" {
		// The FIRST failed step, which is what the row records: everything
		// after it is a consequence, and naming the last one would send an
		// operator to the wrong script.
		fmt.Printf("  failed step: %s (%s)\n", run.FailedStep, run.FailedScript)
	}
	fmt.Printf("  started: %s\n", workflowClock(run.StartedAt))
	if run.FinishedAt != nil {
		fmt.Printf("  finished: %s\n", workflowClock(*run.FinishedAt))
	} else {
		fmt.Println("  finished: not yet")
	}
	fmt.Printf("  duration: %s\n", workflowDuration(run.DurationMillis))

	for _, step := range run.Steps {
		printWorkflowStep(step)
	}
}

// printWorkflowStep is one step: where it sits in the five stages, where
// it ran, and what became of it.
func printWorkflowStep(step service.WorkflowStepDetail) {
	fmt.Printf("  %2d %-26s %-8s %-7s %-7s %-12s %s\n",
		step.Order, step.ScriptName, step.Scope, step.Phase, step.Target, step.State,
		workflowDuration(step.DurationMillis))
	fmt.Printf("     step: %s  timeout: %s\n", step.StepID, workflowDuration(step.TimeoutMillis))
	if step.ExecutionConnectionRef != "" {
		fmt.Printf("     over: %s\n", step.ExecutionConnectionRef)
	}
	// Nil and zero are different answers, which is #810's rule: a
	// transport that lost the connection is never reported as a known
	// exit code, so an unobserved status says so rather than printing 0.
	if step.ExitCode != nil {
		fmt.Printf("     exit code: %d\n", *step.ExitCode)
	} else {
		fmt.Println("     exit code: not observed; no process status reached this product for this step")
	}
	if step.TerminationConfirmed {
		fmt.Println("     termination confirmed: this product proved the process was gone after asking it to stop")
	}
}

// printWorkflowRecoveryRun is one outstanding run, with the two things
// that can be done about it named in the words that would do them.
func printWorkflowRecoveryRun(run service.WorkflowRunDetail) {
	fmt.Printf("outstanding: run %s  set %s  state %s  recovery %s\n", run.RunID, run.BackupSetID, run.State, run.RecoveryState)
	fmt.Printf("  entered: %s (%s ago by this host's clock)\n", workflowClock(run.StartedAt), workflowSince(run.StartedAt))
	fmt.Printf("  backup=%s cleanup=%s workflow=%s\n", run.BackupStatus, run.CleanupStatus, run.WorkflowStatus)
	if run.FailedStep != "" {
		fmt.Printf("  failed step: %s (%s)\n", run.FailedStep, run.FailedScript)
	}
	fmt.Printf("  run the cleanup it owes:   %s workflow recovery resume-cleanup %s\n", cliecho.Binary, run.RunID)
	fmt.Printf("  or take it on by hand:     %s workflow recovery acknowledge %s --reason \"...\"\n", cliecho.Binary, run.RunID)
}

// workflowClock renders an instant as an operator reads a clock, in this
// host's own zone, to the second. --json keeps the RFC 3339 the service's
// own type carries, exactly as activityClock and followClock split the
// same way: this column is for somebody scanning a terminal.
func workflowClock(at time.Time) string {
	if at.IsZero() {
		return "never"
	}

	return at.Local().Format("2006-01-02 15:04:05")
}

// workflowSince is how long ago something was, rounded to the second.
//
// Rounded, because the question it answers is "how long has this machine
// been quiesced" and nobody reads a nanosecond. A future timestamp
// reports zero rather than a negative duration: a clock that disagrees
// with the one that wrote the row is a real deployment (the repository
// health report has a whole skew field for it) and a "-3m0s ago" would
// read as a bug in this command.
func workflowSince(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	d := time.Since(at).Round(time.Second)
	if d < 0 {
		d = 0
	}

	return d.String()
}

// workflowDuration renders the millisecond counts every workflow row
// carries.
//
// Zero is printed as a duration rather than as "none", because a step
// that took under a millisecond really did take about that long; the
// fields that mean "nothing here" are nil pointers and are rendered by
// their own callers.
func workflowDuration(millis int64) string {
	return (time.Duration(millis) * time.Millisecond).String()
}

// printWorkflowJSON is the --json half of every verb in this file.
//
// It emits core/service's own types, field for field, which is `medium
// list --json`'s convention rather than `activity --json`'s. The
// difference between the two is which world the read came from: activity
// renders apicontract types because that read ANSWERS from the wire
// beside a serving engine, and these read core/service in this process,
// whose types carry no wire spelling at all. Inventing a snake_case
// mirror here would be a second contract nothing generates and nothing
// checks, which is how two surfaces come to disagree about a field name.
func printWorkflowJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(err)
	}

	return 0
}

// nonNilRuns and nonNilSteps keep an empty list an empty ARRAY rather
// than null, so a caller iterating the JSON does not have to guard for
// absence. printActivityJSON makes the same substitution for the same
// reason, and the route handlers it mirrors do too.
func nonNilRuns(runs []service.WorkflowRunDetail) []service.WorkflowRunDetail {
	if runs == nil {
		return []service.WorkflowRunDetail{}
	}

	return runs
}

func nonNilSteps(steps []service.WorkflowStepDetail) []service.WorkflowStepDetail {
	if steps == nil {
		return []service.WorkflowStepDetail{}
	}

	return steps
}
