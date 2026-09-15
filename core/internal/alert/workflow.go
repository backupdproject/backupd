package alert

import (
	"fmt"
	"strings"
)

// This file is EPIC L's half of the translation layer conditions.go
// opens: it turns one finished workflow run, and the recovery holds an
// interrupted one left behind, into the Condition vocabulary Dispatcher
// de-duplicates on (#813).
//
// It obeys the same rule the rest of the package does -- it decides
// nothing about whether a hook was right to refuse, how long a script
// should have taken, or whether a failure is retryable. internal/workflow
// and internal/workflowrun reach those verdicts and record them; this
// file reads three status words and answers which of §71's notification
// slots they land in.
//
// # Why this takes strings and imports no engine
//
// internal/health is already treated this way (conditions.go takes a
// health.BackupSetHealth, and nothing else about the backup pipeline),
// and the workflow engine is a much larger dependency to acquire for
// three status words: internal/workflowrun reaches the journal, the
// script spool, the transport and the process table. Importing it here
// would put every one of those behind a package whose entire safety
// argument is what it cannot reach (see this package's doc and
// mechanism_test.go's TestAlertingNeverDeletes). The cost is that the
// status words arrive as plain strings and are matched against a copy of
// internal/workflow's Status vocabulary; workflowStatusWords below is
// that copy, and it is five constants that have to change together with
// a vocabulary the engine documents as closed.
//
// # A notification NEVER contains script output
//
// This is the hard rule of #813, and it is worth stating plainly because
// this is the one place in the product that runs code somebody else
// wrote, against machines this manager does not own. A hook's stdout and
// stderr are arbitrary bytes chosen by that script: a connection string,
// a dumped environment, an API token echoed by a careless `set -x`, the
// contents of a file. A notification is the opposite of a safe place for
// them -- it is delivered OUT of this process, to a platform notification
// centre, a phone, a mail relay, a chat webhook, all of which persist and
// forward it somewhere nobody audited.
//
// Two mechanisms keep that true, and the first is the one that matters:
//
//  1. There is no field to put it in. WorkflowRun carries a backup set,
//     a run id, a step id, a script BASENAME and three status words, and
//     that is the entire type. A caller holding a step's captured output,
//     an exit code's stderr tail or a resolved secret has nowhere to pass
//     it, so this is a shape the API refuses rather than a discipline
//     each call site has to remember. workflow_test.go asserts that
//     field set by reflection, so widening it fails the suite.
//
//  2. Every field is still validated before it is rendered, by
//     safeToken, safeSetName, safeScopeWord and
//     normalizeWorkflowStatus. Field (1) is about
//     intent; this one is about accident -- a caller that put a path, a
//     command line or a captured line into the step id field gets
//     withheldField in the sentence rather than the value. Validation
//     refuses rather than repairs (it does not quietly take the basename
//     of a path handed to FailedScript), because repairing means the
//     rendered text no longer corresponds to any value the caller
//     believed it passed.
//
// The bounded, redacted place for a hook's output is the per-step log
// internal/workflow/steplog.go writes. Nothing here reads it.

// WorkflowOutcome is which of #813's four notifiable shapes a finished
// run had, or none.
//
// The four are a classification of the two facts an operator acts on --
// did the backup happen, and was the source machine put back -- and not
// of the run's internal states, which internal/workflow already has a
// far larger vocabulary for. They exist as a named type rather than as
// two booleans because the mapping onto Kinds is the thing #813 asks to
// be tested as a table, and a table over a named outcome is readable in
// a way a table over (bool, bool) is not.
type WorkflowOutcome string

// The four notifiable outcomes, plus the silence that is the common case.
const (
	// WorkflowBeforeFailedBackupSkipped is a run whose "before" hooks
	// refused, so the backup never started, and whose cleanup did not
	// fail. There is no restore point from this run and nothing was
	// left changed on the source machine.
	WorkflowBeforeFailedBackupSkipped WorkflowOutcome = "before_failed_backup_skipped"

	// WorkflowBackupFailedCleanupOK is a run whose backup failed on its
	// own merits and whose cleanup did not fail. Same remedy as above,
	// different cause; both produce exactly one WorkflowFailed
	// condition, and the Detail is where the difference is stated.
	WorkflowBackupFailedCleanupOK WorkflowOutcome = "backup_failed_cleanup_succeeded"

	// WorkflowBackupOKCleanupFailed is a run whose "after" hooks did not
	// succeed while the backup is not known to have failed.
	//
	// The name records the common case, which is a clean backup and a
	// broken cleanup. It also covers the run whose backup status is not
	// yet known -- a resumed cleanup that fails carries an unknown
	// backup status, because the process that would have set it is the
	// one that died. Classifying that as "both failed" would tell an
	// operator they have no backup when nobody has established that,
	// and going silent would drop the cleanup alert, which is the one
	// thing here that is about another machine's current state.
	WorkflowBackupOKCleanupFailed WorkflowOutcome = "backup_succeeded_cleanup_failed"

	// WorkflowBothFailed is a run with no backup AND a failed cleanup:
	// the worst case, and the reason WorkflowConditions can return two
	// conditions. An operator has two separate jobs here, and each gets
	// its own notification (see WorkflowFailed's doc).
	WorkflowBothFailed WorkflowOutcome = "backup_failed_cleanup_failed"

	// WorkflowOutcomeNone is a run with nothing to notify anybody
	// about, which is every ordinary night.
	WorkflowOutcomeNone WorkflowOutcome = ""
)

// WorkflowRun is one finished run as this package needs it: the two
// identities an operator needs to find it again, the two names that say
// where inside it things went wrong, and the three status words.
//
// It is strings rather than internal/workflow types so internal/alert
// keeps importing no engine, exactly as it imports no backup pipeline to
// read a health verdict (see this file's doc).
//
// This field set is the whole of what a notification may name. Read the
// doc above before adding to it: a field carrying a hook's output, an
// exit code's stderr, a script path, an environment value or a resolved
// secret is the one change this type exists to make impossible.
type WorkflowRun struct {
	// BackupSet is the set the run belongs to, rendered as
	// model.BackupSetID does it ("source/set"). It becomes the
	// Condition's Scope, so it is the de-duplication identity as well
	// as part of the sentence.
	BackupSet string

	// RunID is the workflow run's own id, which is what an operator
	// types into `backupd workflow show` to see the rest. Naming it is
	// what keeps a notification's Detail short: the notification says
	// what happened and where to look, not everything that is known.
	RunID string

	// BackupStatus, CleanupStatus and WorkflowStatus are
	// internal/workflow's Status words (unknown, running, success,
	// failed, skipped) -- the same three values a hook itself reads
	// from RETND_BACKUP_STATUS, RETND_CLEANUP_STATUS and
	// RETND_WORKFLOW_STATUS. A word outside that vocabulary is
	// rendered as unknown rather than passed through.
	BackupStatus   string
	CleanupStatus  string
	WorkflowStatus string

	// FailedStep is the id of the step that failed, if the run had one.
	// It is a step id from the plan, never a path and never a command
	// line.
	FailedStep string

	// FailedScript is the failed step's script BASENAME, if known --
	// "10-pg-quiesce.before.remote.sh", not the directory it lives in.
	//
	// The basename is the most an operator can be given here without
	// disclosing layout: internal/workflow derives a step's target from
	// this name (see workflow.Target), so it is the one string that
	// says what ran and where it ran, and it is chosen by whoever
	// deployed the hook rather than by whoever ran it. A value with a
	// separator in it is withheld rather than trimmed down to its last
	// element; see this file's doc for why validation refuses instead
	// of repairing.
	FailedScript string

	// Bypassed is whether this run had its hooks bypassed. It changes
	// no classification (see ClassifyWorkflowRun) and appears in the
	// Detail only, where it answers the first question an operator asks
	// about a failed run with hooks configured: was a hook involved at
	// all.
	Bypassed bool
}

// The status vocabulary, copied from internal/workflow.Status. See this
// file's doc for why it is copied rather than imported.
const (
	statusUnknown = "unknown"
	statusRunning = "running"
	statusSuccess = "success"
	statusFailed  = "failed"
	statusSkipped = "skipped"
)

var workflowStatusWords = []string{statusUnknown, statusRunning, statusSuccess, statusFailed, statusSkipped}

// ClassifyWorkflowRun says which notifiable shape r had.
//
// # The two axes, and why only one value on each is actionable
//
// Cleanup is judged on one question: did it FAIL. Anything else --
// succeeded, skipped because there were no "after" hooks, or not yet
// known -- is not a cleanup alert, and reading "not yet known" as failed
// would raise one for every run still executing.
//
// The backup axis is judged on whether the run is KNOWN not to have
// produced a backup, which is failed or skipped. An unknown or running
// backup is not evidence of a missing backup, and saying "you have no
// backup" on the strength of a status nobody has written yet is the one
// wrong answer here that an operator cannot check cheaply -- they would
// go looking for a restore point that may well exist.
//
// # Why Bypassed is not tested here
//
// #813 asks that a bypassed run whose backup succeeded notify nothing,
// and it does -- by the default arm, with no branch needed. A bypassed
// run ran no hooks, so its cleanup status is skipped by construction and
// its backup either succeeded (nothing failed, so nothing is reported)
// or failed on its own merits. A branch on Bypassed could therefore only
// ever suppress a backup failure that had nothing to do with hooks,
// which is a backup an operator still does not have. A bypass is
// recorded in the run row and in the audit record, which is where a
// deliberate operator action belongs; it is not a licence to stop
// reporting failures.
//
// A run the engine called a success is silent unconditionally, which is
// belt and braces rather than a second opinion: internal/workflowrun
// does not mark a run successful with a failed cleanup, and if it ever
// did, this function would still not have the standing to overrule it.
func ClassifyWorkflowRun(r WorkflowRun) WorkflowOutcome {
	backup := normalizeWorkflowStatus(r.BackupStatus)
	cleanup := normalizeWorkflowStatus(r.CleanupStatus)

	if normalizeWorkflowStatus(r.WorkflowStatus) == statusSuccess {
		return WorkflowOutcomeNone
	}

	cleanupFailed := cleanup == statusFailed
	noBackup := backup == statusFailed || backup == statusSkipped

	switch {
	case noBackup && cleanupFailed:
		return WorkflowBothFailed
	case cleanupFailed:
		return WorkflowBackupOKCleanupFailed
	case backup == statusSkipped:
		return WorkflowBeforeFailedBackupSkipped
	case backup == statusFailed:
		return WorkflowBackupFailedCleanupOK
	default:
		return WorkflowOutcomeNone
	}
}

// WorkflowConditions returns the conditions r implies: at most one
// WorkflowFailed and at most one WorkflowCleanupFailed, both scoped to
// the backup set.
//
// WorkflowBothFailed produces BOTH, and that is the point of there being
// two kinds. An operator facing that run has to re-run a backup AND go
// and check a machine that may still be quiesced, the two are done at
// different times by possibly different people, and one notification
// naming both is one notification that gets acted on halfway.
//
// Scope is the backup set id rather than the run id, so Dispatcher's
// (Kind, Scope) de-duplication behaves here the way it does for every
// other condition: a set whose workflow keeps failing alerts once and
// then stays quiet, and a set that starts succeeding again re-alerts on
// the next failure. Scoping to the run id would defeat that completely,
// since every run has a new id, and would turn a broken hook into one
// notification per night forever.
//
// Scope carries r.BackupSet unchanged, because it is an identity the
// dispatcher matches on and a caller's unevaluated Subject has to agree
// with it exactly. The sentence an operator reads renders the same value
// through safeSetName; the two differ only when a caller passed
// something that is not a backup set id at all.
func WorkflowConditions(r WorkflowRun) []Condition {
	outcome := ClassifyWorkflowRun(r)
	if outcome == WorkflowOutcomeNone {
		return nil
	}

	set := safeSetName(r.BackupSet)
	statuses := fmt.Sprintf("Statuses: backup %s, cleanup %s, workflow %s.",
		normalizeWorkflowStatus(r.BackupStatus),
		normalizeWorkflowStatus(r.CleanupStatus),
		normalizeWorkflowStatus(r.WorkflowStatus))

	var out []Condition

	// The backup half comes first because it is the condition most
	// operators are expecting to see, and because a reader comparing
	// two notifications about one run should meet them in the order the
	// run produced them.
	if outcome == WorkflowBeforeFailedBackupSkipped || outcome == WorkflowBackupFailedCleanupOK || outcome == WorkflowBothFailed {
		cause := "the backup did not complete"
		if normalizeWorkflowStatus(r.BackupStatus) == statusSkipped {
			cause = `a "before" hook refused the run, so the backup was skipped`
		}

		tail := "Cleanup did not fail, so the source machine should have been left as it was found."
		if outcome == WorkflowBothFailed {
			tail = "Its cleanup failed as well, which is reported separately."
		}
		if r.Bypassed {
			tail += " Hooks were bypassed for this run, so no hook was involved in the failure."
		}

		out = append(out, Condition{
			Kind:  WorkflowFailed,
			Scope: r.BackupSet,
			Detail: fmt.Sprintf("Backup set %s has no backup from this run: %s (%s). %s %s",
				set, cause, r.origin(), tail, statuses),
		})
	}

	if outcome == WorkflowBackupOKCleanupFailed || outcome == WorkflowBothFailed {
		tail := "The backup did not happen either, which is reported separately."
		if outcome == WorkflowBackupOKCleanupFailed {
			tail = "The backup itself is not affected."
		}

		out = append(out, Condition{
			Kind:  WorkflowCleanupFailed,
			Scope: r.BackupSet,
			Detail: fmt.Sprintf(`Backup set %s did not finish its "after" hooks (%s), so the source machine may still be changed: a database left quiesced, a snapshot still held, a filesystem still mounted. Check it before relying on that machine. %s %s`,
				set, r.origin(), tail, statuses),
		})
	}

	return out
}

// WorkflowRecoveryHoldSubject is one outstanding recovery hold as this
// package needs it: internal/workflowrun.RecoveryHold with everything a
// notification may not name removed.
//
// The retained spool reference and the hold's timestamp are deliberately
// absent. The spool reference is a filesystem path, which is the exact
// class of value this file's doc rules out of a notification; the
// timestamp is a fact the run record already carries, and a notification
// that repeats it would still be the same single alert, since Dispatcher
// fires once per (Kind, Scope) however long the hold has stood.
type WorkflowRecoveryHoldSubject struct {
	// BackupSet is the blocked set, rendered as model.BackupSetID does
	// it. It is what these conditions are grouped and scoped by.
	BackupSet string

	// RunID is the interrupted run.
	RunID string

	// Scope is internal/workflow's Scope word for the nested scope
	// whose cleanup is outstanding: global or set. A word outside that
	// pair is withheld.
	Scope string
}

// WorkflowRecoveryConditions returns one WorkflowRecoveryRequired
// condition per BACKUP SET named by holds, naming the run or runs that
// are blocking it and the scope or scopes that cannot be accounted for.
//
// # Why per set and not per hold
//
// internal/workflowrun records a hold per interrupted SCOPE, because
// that is the granularity a resume-cleanup pass operates at: a run
// interrupted inside a set-scoped "after" hook nested in a global one
// leaves two, and both have to be discharged. That is the right shape
// for the engine and the wrong shape for a notification. An operator
// acts per backup set -- they run one resume command, or they
// acknowledge one set -- and two scopes of one interrupted run are one
// problem with one remedy, not two things to do.
//
// It also has to be per set for the dispatcher to behave. Scope is half
// the de-duplication key, so one condition per hold would mean the same
// backup set alerting twice on the same pass, and a set whose second
// scope is discharged first would look like a resolved condition
// followed by a fresh one.
//
// Sets appear in the order holds first names them, and so do the runs
// and scopes inside each sentence. That is a deterministic rendering
// with no sort: the engine returns holds in a stable order, a stable
// input then gives a stable Detail, and Detail is not part of the
// de-duplication key anyway, so re-ordering could not re-alert -- it
// would only make two readings of the same unchanged state read
// differently to a human.
func WorkflowRecoveryConditions(holds []WorkflowRecoveryHoldSubject) []Condition {
	if len(holds) == 0 {
		return nil
	}

	// order preserves first-appearance order of the sets; grouped holds
	// the per-set accumulation, so one pass over holds is enough.
	var order []string
	grouped := make(map[string]*recoveryGroup, len(holds))

	for _, h := range holds {
		g, seen := grouped[h.BackupSet]
		if !seen {
			g = &recoveryGroup{}
			grouped[h.BackupSet] = g
			order = append(order, h.BackupSet)
		}
		g.addRun(safeToken(h.RunID))
		g.addScope(safeScopeWord(h.Scope))
	}

	out := make([]Condition, 0, len(order))
	for _, set := range order {
		g := grouped[set]
		out = append(out, Condition{
			Kind:  WorkflowRecoveryRequired,
			Scope: set,
			Detail: fmt.Sprintf("Backup set %s is blocked: this manager never saw %s finish, and %s cannot be accounted for. No further run of this set will start until you resume the outstanding cleanup or acknowledge it, and the source machine may still be changed.",
				safeSetName(set), g.runPhrase(), g.scopePhrase()),
		})
	}

	return out
}

// recoveryGroup accumulates one backup set's holds. The two slices stay
// de-duplicated as they are built rather than being sorted and
// compacted afterwards, because the whole point of the grouping is that
// one run interrupted in two scopes reads as one run.
type recoveryGroup struct {
	runs   []string
	scopes []string
}

func (g *recoveryGroup) addRun(run string)     { g.runs = appendUnique(g.runs, run) }
func (g *recoveryGroup) addScope(scope string) { g.scopes = appendUnique(g.scopes, scope) }

// runPhrase names the blocking run, or runs. The singular and plural are
// spelled out rather than assembled from an "s" because the sentence
// reads to an operator at 3am and "1 run(s)" is how a notification
// announces that nobody read it.
func (g *recoveryGroup) runPhrase() string {
	if len(g.runs) == 1 {
		return "workflow run " + g.runs[0]
	}
	return "workflow runs " + strings.Join(g.runs, ", ")
}

// scopePhrase names the cleanup scope, or scopes, left outstanding.
func (g *recoveryGroup) scopePhrase() string {
	if len(g.scopes) == 1 {
		return "its " + g.scopes[0] + " cleanup scope"
	}
	return "its " + strings.Join(g.scopes, " and ") + " cleanup scopes"
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// origin is where inside the run things went wrong, as much of it as the
// caller knew. It always names the run, because that is the one value
// that turns a notification into something an operator can look up, and
// it adds the step and the script only when they were given: a run with
// no failed step (a backup that failed between the two hook phases) gets
// a shorter sentence rather than one padded with empty parentheses.
func (r WorkflowRun) origin() string {
	parts := []string{"workflow run " + safeToken(r.RunID)}
	if r.FailedStep != "" {
		parts = append(parts, "step "+safeToken(r.FailedStep))
	}
	if r.FailedScript != "" {
		parts = append(parts, "script "+safeToken(r.FailedScript))
	}
	return strings.Join(parts, ", ")
}

// withheldField replaces a value that is not the shape this package
// agreed to render. It says withheld rather than going blank because an
// operator reading "step (withheld)" knows to go and look at the run,
// whereas a sentence with a hole in it reads like a formatting bug.
const withheldField = "(withheld)"

// maxDetailField bounds one rendered field. The values these fields
// really hold -- a backup set id, a run id, a step id, a script basename
// -- are all far shorter, so this is not a limit anybody meets by
// accident; it is a ceiling on how much of an unexpected value can reach
// a sink that will store and forward it.
const maxDetailField = 128

// safeToken returns v if it is drawn from the shape an identifier in this
// product has, and withheldField otherwise.
//
// The allowlist is deliberately narrower than "printable": no space, no
// quote, no separator, no shell metacharacter, no byte above ASCII. That
// is not about rendering. A hook's captured output, a command line, a
// path and a resolved secret all contain something outside this set in
// practice, so a caller that put one of them in a field meant for an id
// gets withheldField rather than a notification carrying it onward. The
// primary defence is still that WorkflowRun has nowhere to put such a
// value in the first place (see this file's doc); this is the second
// line, for the accident rather than the design.
//
// An empty value is not safe either, because a sentence reading "step ,
// script x" is a sentence that lost a field silently. Callers with
// nothing to say omit the clause instead; see origin.
func safeToken(v string) string {
	if v == "" || len(v) > maxDetailField {
		return withheldField
	}
	// Bytes rather than runes: every byte of a multi-byte rune is above
	// ASCII and therefore outside the allowlist anyway, so this decides
	// the same thing without decoding, and a malformed encoding cannot
	// slip through as one replacement character.
	for i := range len(v) {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return withheldField
		}
	}
	return v
}

// safeSetName renders a backup set id, which is the one field here whose
// shape is not a single token: model.BackupSetID is "source/set", with
// exactly one separator and neither half empty or containing one.
//
// Checking that structure rather than simply allowing "/" through
// safeToken is what keeps an absolute path out of the sentence: a path
// has more separators than a backup set id has, and a leading one.
func safeSetName(v string) string {
	source, set, found := strings.Cut(v, "/")
	if !found {
		return withheldField
	}
	if safeToken(source) == withheldField || safeToken(set) == withheldField {
		return withheldField
	}
	return source + "/" + set
}

// safeScopeWord renders a recovery hold's scope, which is a closed
// vocabulary of two words (internal/workflow's ScopeGlobal and ScopeSet)
// and is therefore checked against the vocabulary itself rather than
// against a character set.
func safeScopeWord(v string) string {
	switch v {
	case "global", "set":
		return v
	default:
		return withheldField
	}
}

// normalizeWorkflowStatus maps a status word onto internal/workflow's
// vocabulary, answering unknown for anything outside it.
//
// Unknown is the right answer for an unrecognised word rather than the
// word itself: "unknown" is already this vocabulary's honest value for
// "nobody has established this yet" (see workflow.Status), so a caller
// that passed something else is saying exactly that, and echoing their
// word into a notification would put an unvalidated string in front of
// an operator to no purpose.
func normalizeWorkflowStatus(v string) string {
	for _, known := range workflowStatusWords {
		if v == known {
			return known
		}
	}
	return statusUnknown
}
