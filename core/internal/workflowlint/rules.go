package workflowlint

import (
	"cmp"
	"path"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This product's own shell rules, written against mvdan.cc/sh's typed
// syntax tree (#906).
//
// # The bar a rule has to clear
//
// Sound on ordinary scripts. Every rule here fires on a shape that is
// WRONG rather than on one that is unusual, and where the difference was
// not clear the rule was narrowed until it was. That bar is not
// fastidiousness: a finding on a working hook is how this feature becomes
// the thing an operator turns off, and an error-severity false positive
// is a save somebody cannot perform at all.
//
// The narrowing is written at each rule, in terms of what it does NOT
// fire on, because that is the part a reader cannot reconstruct from the
// code and the part a later change would quietly break.
//
// # Why the rules read the tree rather than the text
//
// Because every interesting distinction here is syntactic. "Is this
// expansion quoted" is a question about whether it is a direct part of a
// word or a part of a double-quoted part; "is this `cd`'s failure
// checked" is a question about whether its statement is the left operand
// of `&&`/`||` or a condition of an `if`. A regular expression over the
// source can approximate both and will disagree with the shell on
// exactly the scripts worth reporting on -- a `#` inside a string, a
// heredoc that contains what looks like a command, a line continuation.
//
// # Why the whole file's `set` options are read first
//
// Three of the rules are about a failure going unnoticed, and whether it
// does depends on options the script may have set: `set -e` makes an
// unchecked `cd` abort, `set -u` makes an empty expansion abort, `set -o
// pipefail` makes a pipeline's earlier failure visible. A rule that
// ignored them would report a script that has already protected itself,
// which is the fastest way to teach an operator that these findings are
// noise. So the options are collected in one pass and the rules consult
// them.
//
// The options are read and never WRITTEN: nothing here rewrites an
// operator's script, and in particular this product never injects `set
// -e` into a hook. A hook is somebody's code and the only thing that may
// change it is them.

// This product's rule ids. BSH for "backupd shell": deliberately not
// ShellCheck's SC namespace, because these are not ShellCheck's checks
// and a code that looked like one would send an operator to a wiki page
// describing different analysis.
const (
	// CodeUnquotedExpansion: an expansion in a command argument that
	// will word-split and glob.
	CodeUnquotedExpansion = "BSH001"

	// CodeUncheckedCd: a directory change whose failure nothing notices.
	CodeUncheckedCd = "BSH002"

	// CodeRecursiveRemoveRoot: an `rm -rf` that becomes a recursive
	// delete of a root-level path the moment an expansion is empty.
	CodeRecursiveRemoveRoot = "BSH003"

	// CodeMaskedPipelineFailure: `set -e` without `pipefail`, with a
	// pipeline whose earlier commands can therefore fail unnoticed.
	CodeMaskedPipelineFailure = "BSH004"

	// CodeUnquotedTestOperand: an unquoted expansion inside `[ ... ]`,
	// which is a syntax error at run time when it is empty.
	CodeUnquotedTestOperand = "BSH005"

	// CodeMissingShebang: no interpreter line.
	CodeMissingShebang = "BSH006"
)

// check runs every rule over one parsed script.
func check(file *syntax.File, src []byte) []Finding {
	r := &rules{
		opts:    shellOptions(file),
		guarded: map[syntax.Node]bool{},
	}

	r.shebang(src)
	r.collectGuarded(file)
	r.walk(file)

	return r.findings
}

// shellOpts is what the script did to its own failure handling: the
// ordered list of `set` decisions rather than a set of booleans.
//
// Order is the whole point, because `set` is a statement and not a
// declaration. `set -e` protects what comes after it and nothing that
// came before, and `set +e` takes the protection away again. A pair of
// booleans -- what this used to be, because nothing here ever wrote
// false -- reads `set -e; set +e` as a protected script and silences
// three rules over a file whose second line turned the protection off.
type shellOpts struct {
	// events is every option change the file makes, in source order.
	events []optionEvent

	// enabledSomewhere is the fallback for a command with no `set`
	// before it at all. See enabledAt.
	enabledSomewhere [numShellOptions]bool
}

// The options the rules consult.
//
// `set -u` is deliberately not among them. It used to be, and it was the
// wrong question to ask: see recursiveRemove, the only rule that ever
// asked it.
type shellOption int

const (
	optErrexit  shellOption = iota // set -e, set -o errexit
	optPipefail                    // set -o pipefail

	numShellOptions
)

// optionEvent is one `set` turning one option on or off.
type optionEvent struct {
	// offset is where the `set` word is in the source, which is what
	// "before this command" is measured against.
	offset uint

	opt    shellOption
	enable bool

	// unconditional says this `set` is a top-level statement of the
	// file: it runs, once, in the order it is written. A `set` inside an
	// `if`, a function body or a subshell is not -- the walk that found
	// it has no control flow and cannot say whether it runs at all, and
	// a subshell's options do not outlive the subshell.
	unconditional bool
}

// enabledAt reports whether opt is in force at pos.
//
// The honest limitation of reading a tree rather than running it is that
// nothing here knows which statements execute. It is resolved
// asymmetrically, and the asymmetry is chosen so that uncertainty never
// produces a finding:
//
//   - an ENABLING `set` counts wherever it is written, at any depth. A
//     `set -e` inside a function is a script protecting itself, and
//     treating it as absent would report a script that has;
//   - a DISABLING `set` counts only when it is unconditional, and only
//     for the commands after it. In `set -u; if x; then set +u; fi; rm
//     -rf "$D/tmp"` the protection stands, because the `set +u` may
//     never run and the finding it would unlock is a refused save;
//   - a command with no `set` before it falls back to whether the file
//     enables the option anywhere. That is the shape where source order
//     and execution order genuinely differ: `main() { cd /srv; }` above
//     `set -e; main`, where the `cd` is written before the `set -e`
//     that will be in force when it runs.
func (o shellOpts) enabledAt(opt shellOption, pos syntax.Pos) bool {
	state, decided := false, false

	for _, ev := range o.events {
		if ev.offset >= pos.Offset() {
			break
		}
		if ev.opt != opt || (!ev.enable && !ev.unconditional) {
			continue
		}

		state, decided = ev.enable, true
	}

	if decided {
		return state
	}

	return o.enabledSomewhere[opt]
}

// rules accumulates one script's findings.
type rules struct {
	findings []Finding
	opts     shellOpts

	// guarded holds the statements whose failure the script DOES notice:
	// the left operand of `&&`/`||`, a negated statement, and the leaf
	// of an `if`/`while`/`until` condition that actually decides its
	// status -- which is not every statement in that condition. See
	// collectGuarded.
	guarded map[syntax.Node]bool

	// pipelineReported keeps BSH004 to one finding. The rule is about the
	// script's options, so a script with forty pipelines has one mistake
	// and not forty, and forty findings would bury the other rules'.
	pipelineReported bool
}

func (r *rules) add(pos syntax.Pos, code, severity, message string) {
	r.addAt(int(pos.Line()), int(pos.Col()), code, severity, message)
}

// addAt is for a finding about the file rather than about a node: a
// position of 0:0 would render as a place no editor can go to, and every
// surface that draws a finding draws its line and column.
func (r *rules) addAt(line, col int, code, severity, message string) {
	r.findings = append(r.findings, Finding{
		Code:     code,
		Severity: severity,
		Line:     line,
		Col:      col,
		Message:  message,
	})
}

// shebang reports a script with no interpreter line.
//
// Style and not a warning, because this product executes a hook by
// handing the bytes to bash on the target rather than by exec'ing the
// file: a missing `#!` changes nothing about how it runs here. It is
// still worth a line, because the same file run by hand -- which is how
// an operator tests a hook -- is run by whatever shell they happen to be
// in.
func (r *rules) shebang(src []byte) {
	if len(src) >= 2 && src[0] == '#' && src[1] == '!' {
		return
	}

	r.addAt(1, 1, CodeMissingShebang, SeverityStyle,
		"this script has no #! interpreter line. This product runs a hook by handing its bytes to bash, so this changes nothing about how it runs here; it changes what happens when somebody runs the file by hand to test it. Start the file with #!/usr/bin/env bash")
}

// collectGuarded records every statement whose failure the script
// notices.
//
// For `&&`, `||` and `!` that is a property of the operator. For a
// condition it is a property of the LIST, and this used to get it wrong
// in both directions: `if a; b; then` runs both and branches on b's
// status alone, so a failing `a` there is exactly as unnoticed as a
// failing `a` on a line of its own, and marking every statement of the
// condition silenced BSH002 on `if cd /missing; echo ready; then` --
// which is the shape the rule exists for.
func (r *rules) collectGuarded(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.BinaryCmd:
			// `cmd && next` and `cmd || fallback`: the left operand's
			// exit status decides what happens next, so its failure is
			// handled by construction.
			if n.Op == syntax.AndStmt || n.Op == syntax.OrStmt {
				r.guarded[n.X] = true
			}
		case *syntax.IfClause:
			r.guardCondition(n.Cond)
		case *syntax.WhileClause:
			r.guardCondition(n.Cond)
		case *syntax.Stmt:
			// `! cmd` is a test of cmd's status.
			if n.Negated {
				r.guarded[n] = true
			}
		}

		return true
	})
}

// guardCondition marks the statements of an `if`/`while`/`until`
// condition whose exit status the construct branches on.
//
// A condition is a LIST and its status is its last statement's. Every
// statement before that one runs for its effects and its failure is
// discarded, which is the same thing BSH002 says about a bare line.
func (r *rules) guardCondition(cond []*syntax.Stmt) {
	if len(cond) == 0 {
		return
	}

	r.guardStatusLeaves(cond[len(cond)-1])
}

// guardStatusLeaves marks the statements inside one statement that can
// decide its exit status.
//
//   - `a && b`: if a fails the status IS a's failure, and if a succeeds
//     the status is b's. Both are branched on, so both are marked;
//   - `a || b`: a's failure is what makes b run and is then discarded,
//     so b is the leaf. a is marked anyway, by the `||` case above --
//     the same claim from the other direction, and the reason the
//     operator cases here need not repeat it;
//   - `a | b`: a pipeline's status is its LAST command's. That is the
//     whole of BSH004's complaint and it is true here too, so only b
//     decides the condition.
//
// Anything else -- a simple command, a subshell, a `[[ ]]` -- is the
// leaf itself.
func (r *rules) guardStatusLeaves(stmt *syntax.Stmt) {
	if bin, ok := stmt.Cmd.(*syntax.BinaryCmd); ok {
		switch bin.Op {
		case syntax.AndStmt:
			r.guardStatusLeaves(bin.X)
			r.guardStatusLeaves(bin.Y)

			return
		case syntax.OrStmt, syntax.Pipe, syntax.PipeAll:
			r.guardStatusLeaves(bin.Y)

			return
		}
	}

	r.guarded[stmt] = true
}

func (r *rules) walk(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			if call, ok := n.Cmd.(*syntax.CallExpr); ok {
				r.call(n, call)
			}
		case *syntax.DeclClause:
			r.declaration(n)
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				r.pipeline(n)
			}
		}

		return true
	})
}

// call is every rule that is about one simple command.
func (r *rules) call(stmt *syntax.Stmt, call *syntax.CallExpr) {
	if len(call.Args) == 0 {
		return
	}

	name := literal(call.Args[0])

	switch name {
	case "cd", "pushd":
		r.uncheckedCd(stmt, call, name)
	case "rm":
		r.recursiveRemove(call)
	case "[", "test":
		r.unquotedTestOperands(call)

		// `[ $x = y ]` is reported as BSH005 and not also as BSH001: the
		// two are the same mistake and the specific message is the
		// useful one.
		return
	}

	r.unquotedArguments(call)
}

// declaration is BSH001 over a declaration builtin's arguments.
//
// `export`, `readonly`, `declare`, `typeset` and `local` are not
// CallExprs in bash: the parser gives them their own node, whose
// arguments arrive already sorted into assignments and naked words. That
// distinction is exactly the one this rule needs, and it is why this is
// a separate walk case rather than a list of command names inside
// unquotedArguments:
//
//   - an assignment's right-hand side is not word-split. `export
//     NAME=$x` is as safe as the bare `NAME=$x` the rule already
//     excludes, and telling an operator to quote it is advice that
//     changes nothing on the line they are most likely to have copied
//     from a manual;
//   - a naked argument IS an ordinary word. `export $names` splits on
//     whitespace and globs exactly like `tar $args`, and it went
//     unreported for as long as these commands were left out
//     altogether.
//
// A flag is a naked argument too (`declare -r NAME=$x` holds `-r` as
// one), which costs nothing: a flag is a literal and holds no expansion.
func (r *rules) declaration(decl *syntax.DeclClause) {
	for _, arg := range decl.Args {
		if !arg.Naked || arg.Value == nil {
			continue
		}

		r.unquotedWord(arg.Value)
	}
}

// uncheckedCd reports a directory change whose failure nothing notices.
//
// The shape it is about: `cd /srv/data` followed by `rm -rf ./old`. When
// the `cd` fails -- the mount is not there, the directory was renamed --
// the script carries on in whatever directory it was already in and the
// next line operates on the wrong tree. This is the mistake that makes a
// cleanup hook delete the wrong thing.
//
// It does NOT fire on:
//
//   - a script with `set -e` (or `set -o errexit`), where a failed `cd`
//     ends the script;
//   - `cd x || exit`, `cd x && ...`, `if cd x; then`, `while cd x`,
//     `! cd x` -- every construction whose semantics are "notice whether
//     this worked";
//   - `cd` with no argument, which goes to $HOME and is not the shape
//     this is about.
//
// A warning rather than an error: the script is well-formed, the failure
// needs a second thing to go wrong, and it is a line that is in a great
// many working hooks.
func (r *rules) uncheckedCd(stmt *syntax.Stmt, call *syntax.CallExpr, name string) {
	if r.opts.enabledAt(optErrexit, call.Pos()) || r.guarded[stmt] || len(call.Args) < 2 {
		return
	}

	r.add(call.Pos(), CodeUncheckedCd, SeverityWarning,
		"this "+name+" does not check whether it worked, and nothing in this script does either. When it fails -- the directory is gone, the mount is not there -- the commands after it run in the directory the script was already in, against the wrong tree. Write `"+name+" ... || exit 1`, or put `set -e` at the top of the script")
}

// recursiveRemove reports an `rm -rf` that becomes a recursive delete of
// a root-level path when one of its expansions is empty.
//
// The shape it is about: `rm -rf "$STAGING/tmp"` with STAGING unset is
// `rm -rf /tmp`, and `rm -rf $DIR/` with DIR unset is `rm -rf /`. This is
// the finding this package carries at ERROR severity, because it is the
// one whose failure mode is unrecoverable and instantaneous, and because
// the fix is one character.
//
// It is narrowed hard, and every part of the narrowing is load bearing:
//
//   - the command has to be `rm` with BOTH recursion and force, which is
//     what makes an accidental target a silent deletion rather than a
//     prompt or an error;
//   - the word has to hold an expansion that can produce NOTHING.
//     `${DIR:?}` aborts, `${DIR:-/srv/fallback}` cannot be empty and
//     `${#list}` is a number, so none of the three is reported;
//   - the word's literal text, with every expansion that can vanish
//     removed and the result cleaned of `.` and `..`, has to be an
//     absolute path of at most one component. `/`, `/tmp`, `/*` and
//     `"$ROOT/var/../tmp"` are reported; `/srv/backupd/cache/$NAME` is
//     NOT, because an empty NAME there deletes a directory the
//     deployment owns rather than a root-level one.
//
// # Why `set -u` is not an exemption
//
// This rule used to return early on a script with `set -u` or `set -o
// nounset`, reasoning that nounset aborts on an unset variable and
// therefore makes the whole class impossible. It does not. nounset
// aborts on an UNSET parameter and says nothing about one that is SET to
// the empty string, so
//
//	set -u; STAGING=; rm -rf "$STAGING/tmp"
//
// deletes /tmp with the option in force. Asked per expansion -- would
// nounset abort THIS one? -- the answer is no for every name a script
// can assign, which is every name this rule sees; the expansions nounset
// would abort on are unset ones, and a variable that is merely empty
// reaches rm either way. So the option is not consulted, and the remedy
// in the message no longer offers it.
//
// # Two deliberate silences
//
// Both in the direction of not refusing a save over an arguable shape:
//
//   - `${NAME?}` and `${NAME?message}` are read as guards even though
//     the non-colon form aborts only when NAME is unset, so a NAME set
//     to the empty string still collapses. A script that wrote `?` has
//     said out loud that the variable must be there, and an error on it
//     would be this product arguing about the difference between unset
//     and empty in the one place where being wrong costs somebody a
//     save;
//   - nothing here does dataflow, so `D=/srv/stage; rm -rf "$D/tmp"` IS
//     reported. The rule reads one word and not the script. That is the
//     aggressive half of the policy and it stays: the remedy it prints
//     is one character long and correct for the reported word.
func (r *rules) recursiveRemove(call *syntax.CallExpr) {
	recursive, force := false, false

	var targets []*syntax.Word

	endOfOptions := false
	for _, arg := range call.Args[1:] {
		word := literal(arg)

		switch {
		case endOfOptions || len(word) < 2 || word[0] != '-':
			targets = append(targets, arg)
		case word == "--":
			// `rm -- -rf "$D/tmp"` deletes a file named `-rf` and a
			// path: after an exact `--` every word is an operand,
			// however much it looks like a flag. Reading `-rf` there as
			// recursion and force made this rule refuse a save over a
			// delete that is neither.
			endOfOptions = true
		case strings.HasPrefix(word, "--"):
			// The long forms rm actually has. An unrecognised long
			// option is neither of them: `--one-file-system` is not
			// force, and skipping every `--...` word -- what this used
			// to do -- missed `rm --recursive --force` entirely.
			switch word {
			case "--recursive":
				recursive = true
			case "--force":
				force = true
			}
		default:
			// A short cluster: `-rf`, `-r -f`, `-Rf`.
			if strings.ContainsAny(word[1:], "rR") {
				recursive = true
			}
			if strings.ContainsRune(word[1:], 'f') {
				force = true
			}
		}
	}

	if !recursive || !force {
		return
	}

	for _, target := range targets {
		if !hasUnguardedExpansion(target) {
			continue
		}
		cleaned, ok := rootLevelPath(collapse(target))
		if !ok {
			continue
		}

		r.add(target.Pos(), CodeRecursiveRemoveRoot, SeverityError,
			"this recursive, forced delete targets "+quoteForMessage(cleaned)+" whenever the expansion in it is empty, because an unset or empty variable leaves the literal path behind. That is a root-level directory. Write ${NAME:?} so the script fails instead, or give the expansion a default")
	}
}

// pipeline reports a pipeline whose earlier commands can fail unnoticed
// under `set -e`.
//
// The shape it is about: `set -e` at the top, and then `pg_dump db |
// gzip > out.gz`. `set -e` looks like it covers this and does not: a
// pipeline's exit status is its LAST command's, so a `pg_dump` that dies
// halfway leaves a gzip that succeeded, a truncated dump on disk and a
// script that keeps going. That is a backup that reports success and
// cannot be restored.
//
// It does NOT fire on a script without `set -e` in force at the pipeline
// (nothing was claiming to stop on failure, so there is nothing
// misleading about it) or on one where `pipefail` is in force there.
// "In force" and not "mentioned anywhere": `set -o pipefail` followed by
// `set +o pipefail` is a script that turned it back off, and reading the
// second line as protection was how this rule went quiet on the file
// that needed it. One finding per script, at the first pipeline: the
// mistake is in the options, not in each pipeline.
func (r *rules) pipeline(cmd *syntax.BinaryCmd) {
	if r.pipelineReported ||
		!r.opts.enabledAt(optErrexit, cmd.Pos()) ||
		r.opts.enabledAt(optPipefail, cmd.Pos()) {
		return
	}
	r.pipelineReported = true

	r.add(cmd.Pos(), CodeMaskedPipelineFailure, SeverityWarning,
		"this script uses `set -e` and a pipeline, without `set -o pipefail`. A pipeline's exit status is its LAST command's, so a failure earlier in this pipeline -- the dump, not the compressor -- leaves `set -e` with nothing to trip on: the script continues and the hook reports success. Write `set -euo pipefail`, or check ${PIPESTATUS[@]}")
}

// unquotedArguments reports an expansion in a command argument that the
// shell will split on whitespace and expand as a glob.
//
// The shape it is about: `rm -rf $target` where target is
// "/srv/my backups" removes "/srv/my" and "backups", and `cp $src $dst`
// where src contains a `*` copies whatever that matched in the current
// directory.
//
// It does NOT fire on:
//
//   - anything inside double quotes, which is the fix;
//   - the command NAME itself (argument zero), where an operator
//     building a command line out of a variable is doing it on purpose;
//   - `for f in $list`, a `case` subject, an assignment's right-hand
//     side, an arithmetic expression or a `[[ ]]` operand -- none of
//     which word-split, so an expansion there needs no quotes. Those are
//     not CallExpr arguments, so they are excluded by construction
//     rather than by a list this file has to keep up to date;
//   - `$?`, `$#`, `$$`, `$!`, `$-`, `$0`, `$@` and `$*`, whose values
//     either cannot contain whitespace or are being split deliberately;
//   - `${#x}`, which is a length, and `${!prefix*}` / `${!prefix@}`,
//     which are lists of NAMES. `${!x}` is NOT among them: it is
//     indirect expansion, it produces the value of the variable x names,
//     and that value splits like any other. The exclusion used to be
//     written against ParamExp.Excl, which the parser sets for all
//     three, and so hid a real finding;
//   - a declaration builtin's assignment argument -- `export NAME=$x`,
//     `local dir=$1` -- which is an assignment context and does not
//     split. Those commands are not CallExprs at all; see declaration
//     for the half of them that IS reported.
//
// Info severity: it is the most common finding by a wide margin, it is
// usually harmless in a hook with no spaces in its paths, and it is the
// one that becomes noise if it shouts.
func (r *rules) unquotedArguments(call *syntax.CallExpr) {
	for _, arg := range call.Args[1:] {
		r.unquotedWord(arg)
	}
}

func (r *rules) unquotedWord(word *syntax.Word) {
	for _, part := range word.Parts {
		if pos, what, ok := splittable(part); ok {
			r.add(pos, CodeUnquotedExpansion, SeverityInfo,
				"this "+what+" is not quoted, so the shell splits its value on whitespace and expands any glob characters in it before the command sees it: a path with a space in it becomes two arguments, and one with a `*` becomes whatever that matched. Put it in double quotes")
		}
	}
}

// unquotedTestOperands reports an unquoted expansion inside `[ ... ]`.
//
// The shape it is about: `[ -n $reply ]` with reply empty becomes
// `[ -n ]`, which is not a comparison failing -- it is a syntax error
// from the test builtin ("unary operator expected"), on a line that looks
// like it is handling the empty case.
//
// A warning rather than an info, because unlike BSH001 the failure
// happens on the EMPTY value, which is the case the condition was
// usually written to handle. `[[ ... ]]` does not split and is the other
// fix; it is not reported, because it is not a CallExpr.
func (r *rules) unquotedTestOperands(call *syntax.CallExpr) {
	for _, arg := range call.Args[1:] {
		for _, part := range arg.Parts {
			if pos, what, ok := splittable(part); ok {
				r.add(pos, CodeUnquotedTestOperand, SeverityWarning,
					"this "+what+" is not quoted inside `[ ... ]`. When it is empty the test sees one operand fewer than it was written for and fails with a syntax error rather than a false -- which is the case the condition is usually there to handle. Put it in double quotes, or use `[[ ... ]]`, which does not split")
			}
		}
	}
}

// splittable reports whether one word part is an expansion the shell will
// split, and what to call it in a message.
func splittable(part syntax.WordPart) (syntax.Pos, string, bool) {
	switch p := part.(type) {
	case *syntax.ParamExp:
		if !splittableParam(p) {
			return syntax.Pos{}, "", false
		}
		if p.Excl {
			return p.Pos(), "indirect expansion of ${!" + p.Param.Value + "}", true
		}

		return p.Pos(), "expansion of $" + p.Param.Value, true
	case *syntax.CmdSubst:
		return p.Pos(), "command substitution", true
	default:
		return syntax.Pos{}, "", false
	}
}

// splittableParam decides whether a parameter expansion is one whose
// value could split into several words. See unquotedArguments for the
// argument behind each exclusion.
//
// ParamExp.Excl is set for `${!x}`, `${!prefix*}` and `${!prefix@}`
// alike, and only the last two are name expansions. The first is
// indirect expansion -- the value of the variable whose name x holds --
// which splits exactly like the value it reaches. ParamExp.Names is what
// tells the three apart, and it is set only for the name-list forms.
func splittableParam(p *syntax.ParamExp) bool {
	if p.Length || p.Width || p.Names != 0 || p.Param == nil {
		return false
	}

	switch p.Param.Value {
	case "?", "#", "$", "!", "-", "0", "@", "*":
		return false
	default:
		return true
	}
}

// hasUnguardedExpansion reports whether this word contains an expansion
// that can produce NOTHING -- the property BSH003 is about.
//
// "Can produce nothing" rather than "carries no guard": the two differ
// in both directions and each difference was a wrong answer on a real
// script. `${#list}` carries no guard and cannot be empty -- it is a
// number -- and reading it as a hole turned `rm -rf "/${#list}/tmp"`
// into a refused save. `${D-/srv}` carries a guard that does not cover a
// D set to the empty string, and reading it as covered missed a delete
// of /tmp.
func hasUnguardedExpansion(word *syntax.Word) bool {
	return partsCanVanish(word.Parts)
}

// partsCanVanish reports whether any expansion among these parts can
// produce nothing.
//
// It does not descend blindly into an expansion's own words the way a
// syntax.Walk would -- paramCanBeEmpty decides those, in terms of what
// the operator does with them. A blind walk finds the `$OTHER` in
// `${D:-/srv/$OTHER}` and calls the whole expansion a hole, when what it
// produces is at least "/srv/".
func partsCanVanish(parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.DblQuoted:
			if partsCanVanish(p.Parts) {
				return true
			}
		case *syntax.ParamExp:
			if paramCanBeEmpty(p) {
				return true
			}
		case *syntax.CmdSubst:
			// A command whose output is empty -- it failed, it printed
			// nothing -- leaves the same hole a variable does.
			return true
		}
	}

	return false
}

// wordCanBeEmpty reports whether a whole word can expand to nothing at
// all: every part of it empty at once.
func wordCanBeEmpty(word *syntax.Word) bool {
	if word == nil {
		return true
	}

	for _, part := range word.Parts {
		if !partCanBeEmpty(part) {
			return false
		}
	}

	return true
}

func partCanBeEmpty(part syntax.WordPart) bool {
	switch p := part.(type) {
	case *syntax.Lit:
		return p.Value == ""
	case *syntax.SglQuoted:
		return p.Value == ""
	case *syntax.DblQuoted:
		for _, inner := range p.Parts {
			if !partCanBeEmpty(inner) {
				return false
			}
		}

		return true
	case *syntax.ParamExp:
		return paramCanBeEmpty(p)
	case *syntax.CmdSubst:
		return true
	default:
		// An arithmetic expansion prints a number, a process
		// substitution prints a path, and a part this package does not
		// recognise is assumed to print something. Being wrong in this
		// direction is silence.
		return false
	}
}

// paramCanBeEmpty reports whether a parameter expansion can produce
// nothing, decided per operator rather than per "has an operator".
//
// The cases, and why each is what it is:
//
//   - `${#x}` and mksh's `${%x}` are a length and a width: a number, so
//     at least one character, whatever x holds;
//   - `$?`, `$$`, `$#`, `$0` and `$-` are set by the shell itself and
//     are never empty. `$!` is empty until a background job exists and
//     `$@`/`$*` are empty with no arguments, so those three are not in
//     the list;
//   - `${x:?}` and `${x:?message}` abort the script rather than expand.
//     `${x?}` is read the same way, which is an over-reading argued at
//     recursiveRemove;
//   - `${x:-word}` and `${x:=word}` are empty only if the WORD is: the
//     colon forms cover unset and null alike. `${x:-/srv/stage}` cannot
//     vanish; `${x:-$OTHER}` can, so the fallback is analysed rather
//     than accepted categorically;
//   - `${x-word}` and `${x=word}` do NOT cover null. An x assigned the
//     empty string is SET, so the fallback is never reached and the
//     expansion is empty;
//   - `${x+word}` and `${x:+word}` are the other way round: they produce
//     the word only when x has a value and nothing when it does not, so
//     an alternate operator guards nothing here;
//   - a prefix or suffix removal, a case conversion, a replacement, a
//     slice, an indirect `${!x}` and a name list `${!p*}` can all come
//     out empty.
func paramCanBeEmpty(p *syntax.ParamExp) bool {
	if p.Length || p.Width {
		return false
	}

	if p.Exp == nil {
		if p.Param == nil || p.Excl || p.Names != 0 || p.Repl != nil || p.Slice != nil {
			return true
		}

		switch p.Param.Value {
		case "?", "$", "#", "0", "-":
			return false
		default:
			return true
		}
	}

	switch p.Exp.Op {
	case syntax.ErrorUnset, syntax.ErrorUnsetOrNull:
		return false
	case syntax.DefaultUnsetOrNull, syntax.AssignUnsetOrNull:
		return wordCanBeEmpty(p.Exp.Word)
	default:
		return true
	}
}

// nonEmptyExpansion is what collapse writes for an expansion that cannot
// be empty.
//
// One character, and deliberately not the expansion's source text: a
// default word carries slashes (`${D:-/srv/stage}`) and splicing them in
// would invent path components as surely as erasing the expansion
// removes them.
const nonEmptyExpansion = "x"

// collapse renders a word as what the shell would pass to the command if
// every expansion in it that CAN be empty were empty: the worst case
// BSH003 is about.
//
// Single-quoted and double-quoted text contributes its literal
// characters, because the quotes are not part of the value. An expansion
// that can vanish contributes nothing; one that cannot contributes a
// stand-in, because erasing that one too invents a shorter path.
// `"/$D/${#list}/data"` is `//5/data` at its worst -- three components,
// not this rule's business -- and erasing both expansions makes it read
// as `/data`, which is a refused save on a safe script.
func collapse(word *syntax.Word) string {
	var b strings.Builder
	collapseParts(&b, word.Parts)

	return b.String()
}

func collapseParts(b *strings.Builder, parts []syntax.WordPart) {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			collapseParts(b, p.Parts)
		default:
			if !partCanBeEmpty(part) {
				b.WriteString(nonEmptyExpansion)
			}
		}
	}
}

// rootLevelPath cleans a collapsed path and reports whether it is the
// filesystem root or one component inside it: the paths whose recursive
// deletion is not something a deployment recovers from by restoring a
// directory. The cleaned path is what the message quotes.
//
// The cleaning is load bearing rather than cosmetic. `rm -rf
// "$ROOT/var/../tmp"` with an empty ROOT deletes /tmp, but the text left
// behind is `/var/../tmp`, which counts as three components and walked
// past this rule. path.Clean resolves the `.` and `..` lexically,
// without asking the filesystem -- which is also all this package is
// allowed to do.
//
// A relative path is never root-level, before or after cleaning:
// `./$NAME/data` with an empty NAME is `.//data`, inside whatever
// directory the hook is already in.
func rootLevelPath(collapsed string) (string, bool) {
	if !strings.HasPrefix(collapsed, "/") {
		return "", false
	}

	cleaned := path.Clean(collapsed)

	components := 0
	for _, c := range strings.Split(cleaned, "/") {
		if c != "" {
			components++
		}
	}

	return cleaned, components <= 1
}

// literal renders a word that is exactly one unquoted literal, or "".
//
// Used to recognise a command name and a flag. A word this cannot render
// is deliberately not matched: `"rm"` and `$cmd` are both words this
// package declines to make claims about, which is the conservative
// direction -- it reports nothing rather than guessing.
func literal(word *syntax.Word) string {
	if len(word.Parts) != 1 {
		return ""
	}
	lit, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return ""
	}

	return lit.Value
}

// quoteForMessage renders a collapsed path for a message, saying "the
// filesystem root" rather than printing a bare slash that reads like
// punctuation.
func quoteForMessage(target string) string {
	if strings.Trim(target, "/") == "" {
		return "the filesystem root (/)"
	}

	return target
}

// shellOptions reads what the script does to its own failure handling,
// as an ordered list of changes rather than a verdict.
//
// Every `set` in the file is read, at any depth. Whether one COUNTS at a
// given command is enabledAt's question, and that is where the direction
// of the uncertainty is argued; this function only records what the
// script wrote, including the `+` forms it used to ignore entirely.
//
// `-u` and `+u` are parsed like every other flag and deliberately not
// recorded: no rule consults nounset any more. recursiveRemove did, and
// was wrong to -- nounset aborts on an unset parameter and not on an
// empty one, so it never made that rule's hole impossible.
func shellOptions(file *syntax.File) shellOpts {
	// A `set` that is a top-level statement of the file runs, once, in
	// the order it is written. Nothing else about the tree says that.
	unconditional := map[uint]bool{}
	for _, stmt := range file.Stmts {
		if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && isSetCall(call) {
			unconditional[call.Pos().Offset()] = true
		}
	}

	var opts shellOpts

	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || !isSetCall(call) {
			return true
		}

		opts.record(call, unconditional[call.Pos().Offset()])

		return true
	})

	// syntax.Walk visits a file in source order, but enabledAt's
	// last-one-wins scan depends on that rather than assuming it.
	slices.SortStableFunc(opts.events, func(a, b optionEvent) int {
		return cmp.Compare(a.offset, b.offset)
	})

	return opts
}

func isSetCall(call *syntax.CallExpr) bool {
	return len(call.Args) > 0 && literal(call.Args[0]) == "set"
}

// record reads one `set` and appends what it changes.
//
// `set -euo pipefail` is one word of flags and then an option NAME;
// `set +o pipefail` is the same shape turning one off; a word starting
// with `+` turns off whatever the same letter after `-` turns on. A word
// that is neither is not an option: `set -- "$@"` replaces the
// positional parameters and everything after the `--` is a value.
func (o *shellOpts) record(call *syntax.CallExpr, unconditional bool) {
	offset := call.Pos().Offset()

	pendingName, pendingEnable := false, false
	for _, arg := range call.Args[1:] {
		word := literal(arg)

		if pendingName {
			pendingName = false

			switch word {
			case "errexit":
				o.add(optionEvent{offset: offset, opt: optErrexit, enable: pendingEnable, unconditional: unconditional})
			case "pipefail":
				o.add(optionEvent{offset: offset, opt: optPipefail, enable: pendingEnable, unconditional: unconditional})
			}

			continue
		}

		if word == "--" {
			return
		}
		if len(word) < 2 || (word[0] != '-' && word[0] != '+') {
			continue
		}

		enable := word[0] == '-'
		for _, flag := range word[1:] {
			switch flag {
			case 'e':
				o.add(optionEvent{offset: offset, opt: optErrexit, enable: enable, unconditional: unconditional})
			case 'o':
				pendingName, pendingEnable = true, enable
			}
		}
	}
}

func (o *shellOpts) add(ev optionEvent) {
	o.events = append(o.events, ev)
	if ev.enable {
		o.enabledSomewhere[ev.opt] = true
	}
}
