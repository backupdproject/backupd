package workflowlint

import (
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

// shellOpts is what the script did to its own failure handling.
type shellOpts struct {
	errexit  bool // set -e
	nounset  bool // set -u
	pipefail bool // set -o pipefail
}

// rules accumulates one script's findings.
type rules struct {
	findings []Finding
	opts     shellOpts

	// guarded holds the statements whose failure the script DOES notice:
	// the left operand of `&&`/`||`, a condition of `if`/`while`/`until`,
	// and a negated statement.
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
			for _, s := range n.Cond {
				r.guarded[s] = true
			}
		case *syntax.WhileClause:
			for _, s := range n.Cond {
				r.guarded[s] = true
			}
		case *syntax.Stmt:
			// `! cmd` is a test of cmd's status.
			if n.Negated {
				r.guarded[n] = true
			}
		}

		return true
	})
}

func (r *rules) walk(file *syntax.File) {
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			if call, ok := n.Cmd.(*syntax.CallExpr); ok {
				r.call(n, call)
			}
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
	if r.opts.errexit || r.guarded[stmt] || len(call.Args) < 2 {
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
//   - the word has to hold an expansion with no default and no error
//     branch. `${DIR:?}` (fail if unset), `${DIR:-/srv/fallback}` and
//     friends have already handled this and are not reported;
//   - the word's literal text, with every expansion removed, has to be
//     an absolute path of at most one component. `/`, `/tmp`, `/*` are
//     reported; `/srv/backupd/cache/$NAME` is NOT, because an empty NAME
//     there deletes a directory the deployment owns rather than a
//     root-level one;
//   - the script must not use `set -u` (or `set -o nounset`), which
//     aborts on an unset variable and therefore makes the whole class
//     impossible.
func (r *rules) recursiveRemove(call *syntax.CallExpr) {
	if r.opts.nounset {
		return
	}

	recursive, force := false, false
	for _, arg := range call.Args[1:] {
		flag := literal(arg)
		if !strings.HasPrefix(flag, "-") || strings.HasPrefix(flag, "--") {
			continue
		}
		if strings.ContainsAny(flag, "rR") {
			recursive = true
		}
		if strings.Contains(flag, "f") {
			force = true
		}
	}
	if !recursive || !force {
		return
	}

	for _, arg := range call.Args[1:] {
		if strings.HasPrefix(literal(arg), "-") {
			continue
		}
		if !hasUnguardedExpansion(arg) {
			continue
		}
		collapsed := collapse(arg)
		if !rootLevel(collapsed) {
			continue
		}

		r.add(arg.Pos(), CodeRecursiveRemoveRoot, SeverityError,
			"this recursive, forced delete targets "+quoteForMessage(collapsed)+" whenever the expansion in it is empty, because an unset or empty variable leaves the literal path behind. That is a root-level directory. Write ${NAME:?} so the script fails instead, give the expansion a default, or put `set -u` at the top of the script")
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
// It does NOT fire on a script without `set -e` (nothing was claiming to
// stop on failure, so there is nothing misleading about it) or on one
// that sets `pipefail`. One finding per script, at the first pipeline:
// the mistake is in the options, not in each pipeline.
func (r *rules) pipeline(cmd *syntax.BinaryCmd) {
	if r.pipelineReported || !r.opts.errexit || r.opts.pipefail {
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
//   - `${#x}` and `${!x}`, which are a length and a name.
//
// Info severity: it is the most common finding by a wide margin, it is
// usually harmless in a hook with no spaces in its paths, and it is the
// one that becomes noise if it shouts.
func (r *rules) unquotedArguments(call *syntax.CallExpr) {
	for _, arg := range call.Args[1:] {
		for _, part := range arg.Parts {
			if pos, what, ok := splittable(part); ok {
				r.add(pos, CodeUnquotedExpansion, SeverityInfo,
					"this "+what+" is not quoted, so the shell splits its value on whitespace and expands any glob characters in it before the command sees it: a path with a space in it becomes two arguments, and one with a `*` becomes whatever that matched. Put it in double quotes")
			}
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
func splittableParam(p *syntax.ParamExp) bool {
	if p.Length || p.Excl || p.Width || p.Param == nil {
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
// that contributes NOTHING when the variable is unset or empty -- the
// property BSH003 is about.
//
// An expansion carrying a default, an assignment or an error branch is
// guarded: the script has already decided what happens when the value is
// missing.
func hasUnguardedExpansion(word *syntax.Word) bool {
	found := false

	syntax.Walk(word, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.ParamExp:
			if n.Exp == nil || !guardsUnset(n.Exp.Op) {
				found = true
			}
		case *syntax.CmdSubst:
			// A command whose output is empty -- it failed, it printed
			// nothing -- leaves the same hole a variable does.
			found = true
		}

		return !found
	})

	return found
}

func guardsUnset(op syntax.ParExpOperator) bool {
	switch op {
	case syntax.DefaultUnset, syntax.DefaultUnsetOrNull,
		syntax.ErrorUnset, syntax.ErrorUnsetOrNull,
		syntax.AssignUnset, syntax.AssignUnsetOrNull,
		syntax.AlternateUnset, syntax.AlternateUnsetOrNull:
		return true
	default:
		return false
	}
}

// collapse renders a word with every expansion replaced by nothing: what
// the shell would pass to the command if every variable in it were unset.
//
// Single-quoted and double-quoted text contributes its literal
// characters, because the quotes are not part of the value; a nested
// expansion inside double quotes contributes nothing, for the same reason
// an unquoted one does.
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
		}
	}
}

// rootLevel reports whether a path is the filesystem root or one
// component inside it: the paths whose recursive deletion is not
// something a deployment recovers from by restoring a directory.
func rootLevel(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}

	components := 0
	for _, c := range strings.Split(path, "/") {
		if c != "" {
			components++
		}
	}

	return components <= 1
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
func quoteForMessage(path string) string {
	if strings.Trim(path, "/") == "" {
		return "the filesystem root (/)"
	}

	return path
}

// shellOptions reads what the script does to its own failure handling.
//
// Every `set` in the file counts, at any depth, and that is deliberately
// generous: a `set -e` inside a function or an `if` is unusual, and
// treating it as absent would make this package report a script that has
// in fact protected itself. The rules that consult these options are all
// of the form "this failure goes unnoticed", so over-detecting protection
// produces silence and under-detecting it produces a false finding -- and
// only one of those two is acceptable.
func shellOptions(file *syntax.File) shellOpts {
	var opts shellOpts

	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 || literal(call.Args[0]) != "set" {
			return true
		}

		expectOptionName := false
		for _, arg := range call.Args[1:] {
			word := literal(arg)

			if expectOptionName {
				expectOptionName = false
				switch word {
				case "errexit":
					opts.errexit = true
				case "nounset":
					opts.nounset = true
				case "pipefail":
					opts.pipefail = true
				}

				continue
			}

			if !strings.HasPrefix(word, "-") {
				continue
			}

			// `set -euo pipefail`: the flags are one word and the `o`
			// says the NEXT word is an option name.
			if strings.ContainsRune(word, 'e') {
				opts.errexit = true
			}
			if strings.ContainsRune(word, 'u') {
				opts.nounset = true
			}
			if strings.ContainsRune(word, 'o') {
				expectOptionName = true
			}
		}

		return true
	})

	return opts
}
