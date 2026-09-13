package workflowlint

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"mvdan.cc/sh/v3/syntax"
)

// One report per script, in the one shape every surface renders: the
// parse verdict, and the findings this product's own rules produced.
//
// # Why the parse verdict and the findings are separate fields
//
// Because they fail differently and a caller has to be able to act on
// that. A parse error means there is no program: nothing after it was
// analysed and the script cannot run at all. A finding means the program
// is well-formed and something in it is wrong -- sometimes fatally (an
// `rm -rf` that becomes `rm -rf /` the day a variable is unset), usually
// not (a `cd` whose failure nothing checks). Folding both into one list
// of "problems" would make the save gate's threshold unexpressible, and
// the threshold is the feature.
//
// # Why there is no error return
//
// The only way this can decline to answer is a script larger than
// MaxScriptBytes, which is a fact about the script rather than a fault of
// the caller, and one an operator has to be able to read. An error return
// would have callers choosing between discarding the answer they did get
// and inventing prose for a failure they cannot interpret.

// The severities a finding can carry.
//
// Four, and they are the four an operator already reads on this
// product's other validation surface, meaning the same things: `error` is
// a refusal, the rest are reports. Only `error` blocks a save
// (ScriptReport.Blocks), and which rule carries which severity is argued
// at the rule.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityInfo    = "info"
	SeverityStyle   = "style"
)

// MaxScriptBytes is the largest script this package will look at.
//
// It is workflow.DefaultMaxScriptSize's value and not a coincidence: that
// is how large a hook script may be by default, so the ordinary
// deployment is never told its script was too large to check. It is a
// constant rather than a parameter because it is not a policy: the parser
// is recursive descent, a stack overflow in Go is fatal and cannot be
// recovered, and the only thing standing between a megabyte of "$((((("
// and a dead process is a limit on how much of it is read.
//
// A deployment that raises workflows.max_script_size_bytes past this gets
// "not examined" for the scripts above it, which says what happened. It
// does not get a pass, and it does not get a refused save: refusing to
// save a legitimately-configured deployment because this package declined
// to look would be a gate on the wrong thing.
const MaxScriptBytes = 1 << 20

// Finding is one thing this product's rules reported about one script.
type Finding struct {
	// Code is this product's own rule id, "BSH" and a number. It is a
	// value rather than a sentence because it reaches a JSON surface, a
	// UI that groups by it and an operator who greps for one; the
	// MESSAGE is free to be reworded and this is not.
	Code string

	// Severity is one of the four above.
	Severity string

	// Line and Col are 1-based, counted the way every editor counts, so
	// "BSH001 at 12:6" is a place somebody can go to.
	Line int
	Col  int

	// Message is the operator-facing sentence: what is wrong, what it
	// does when it goes wrong, and what to write instead. It may quote a
	// variable NAME or a literal from the script -- the script's own text
	// -- and never a value, because nothing here resolves one.
	Message string
}

// ParseFault is the shell parser's verdict when there is no program.
//
// One fault and not a list: the parser stops at the first thing it cannot
// make sense of, and a second position derived from a guess about what
// the operator meant would be this product inventing a fault. The one it
// reports is the one to fix.
type ParseFault struct {
	Line    int
	Col     int
	Message string
}

// ScriptReport is everything this package can say about one script.
type ScriptReport struct {
	// Script is the basename it was reported under, so a caller holding
	// several reports does not need a parallel slice.
	Script string

	// ParseError is non-nil when the bytes are not a shell program.
	ParseError *ParseFault

	// Findings is what the rules reported, sorted by position then code.
	// Empty with Examined true is a clean script; empty with Examined
	// false means nobody looked.
	Findings []Finding

	// Examined says the script was read and checked. NotExaminedReason
	// is the sentence that says why it was not, and it is never empty
	// when Examined is false: "not examined" without a reason is the
	// finding an operator cannot act on.
	Examined          bool
	NotExaminedReason string
}

// Blocking is the findings that make a script unsafe to save: the
// error-severity ones.
//
// The threshold is here, in one function, rather than at each of the
// surfaces that need it (the save gate, the validation report, the CLI
// summary, the UI). A threshold spelled four times is four thresholds,
// and the first divergence would be a save the API refuses and the CLI
// performs.
func (r ScriptReport) Blocking() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			out = append(out, f)
		}
	}

	return out
}

// Blocks reports whether this script is a reason to refuse a save: it
// does not parse, or a rule reported something at error severity.
//
// warning, info and style do NOT block, and that is the documented
// default rather than an oversight. BSH002 ("this cd's failure is
// unguarded") is a warning on a line that has been in somebody's working
// backup hook for three years; refusing their next save over it would
// make this feature the thing an operator turns off. A parse error and an
// error-severity finding are different in kind: the script cannot run, or
// it does something other than what it says.
func (r ScriptReport) Blocks() bool {
	return r.ParseError != nil || len(r.Blocking()) > 0
}

// Report is the whole of this package's work: one script's bytes in, one
// report out.
//
// name is used for nothing but the report's own Script field and the
// parser's position prefix. Nothing is executed and no other file is
// read.
func Report(_ context.Context, name string, src []byte) ScriptReport {
	out := ScriptReport{Script: name}

	if len(src) > MaxScriptBytes {
		// Before the parser, because this is the bound that makes the
		// parser safe rather than a check on its result.
		out.NotExaminedReason = fmt.Sprintf(
			"not examined: %s is %d bytes and this check reads at most %d. A hook is a shell script; a prefix of one is a different program, so it is not examined rather than partly examined",
			name, len(src), MaxScriptBytes)

		return out
	}

	out.Examined = true

	file, fault := parse(name, src)
	out.ParseError = fault
	if fault != nil {
		// No AST, so no rules. The parse fault is the finding, and it is
		// the only one worth reporting: every rule below would be
		// commenting on a program the shell would refuse to run.
		return out
	}

	out.Findings = check(file, src)

	slices.SortFunc(out.Findings, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Col, b.Col),
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(a.Message, b.Message),
		)
	})

	return out
}

// parse runs mvdan.cc/sh over the bytes and returns the file, or the
// fault.
//
// The variant is bash, and that is not a default taken by accident: a
// hook's target is decided by its FILENAME (`NAME.local.sh` runs on the
// host runner, `NAME.remote.sh` on the source host), and this product
// executes both with bash. Parsing as POSIX sh would refuse arrays and
// `[[` in scripts that run correctly, which is a validation that fails on
// working configurations -- the one failure mode a precondition of saving
// must not have.
//
// Comments are KEPT, because a rule may need to read one: a directive an
// operator writes to say "I meant this" has nowhere else to live.
func parse(name string, src []byte) (*syntax.File, *ParseFault) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).
		Parse(bytes.NewReader(src), name)
	if err == nil {
		return file, nil
	}

	var perr syntax.ParseError
	if errors.As(err, &perr) {
		return nil, &ParseFault{
			Line:    int(perr.Pos.Line()),
			Col:     int(perr.Pos.Col()),
			Message: perr.Text,
		}
	}

	// Not a ParseError: the reader failed, which for a bytes.Reader it
	// cannot. Reported rather than dropped, with no position, because a
	// verdict this package cannot explain is still a verdict it must not
	// turn into a pass.
	return nil, &ParseFault{Message: err.Error()}
}
