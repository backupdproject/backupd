package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The basename rule, and the refusals that make it worth having.
//
// A hook directory is a list of programs this daemon will execute, so the
// filename is not cosmetic: it is the only place a script's TARGET is
// declared, and it is a string that travels into a spool path, a log line,
// an audit record and an API response. One regular expression decides both
// questions at once, and everything else in this file exists to turn a
// non-match into a sentence an operator can act on.
//
// The refusals are separate checks rather than one "does not match"
// message for a reason that is easy to undervalue until you are the person
// reading it at 3am: "backup.sh does not match
// ^[0-9A-Za-z][0-9A-Za-z._-]*\.(local|remote)\.sh$" tells an operator
// nothing, and "backup.sh does not say where it runs; rename it
// backup.local.sh or backup.remote.sh" tells them everything. A regexp is
// the right implementation of the RULE and the wrong implementation of the
// EXPLANATION.

// ScriptNamePattern is the rule a hook script's basename must match. It is
// exported because it is a documented part of the product's contract with
// an operator (docs and the ADR quote it), not because anything outside
// this package should be matching against it: ParseScriptName is the
// function to call.
const ScriptNamePattern = `^[0-9A-Za-z][0-9A-Za-z._-]*\.(local|remote)\.sh$`

// scriptName is ScriptNamePattern compiled once. MustCompile at package
// scope: a constant pattern that does not compile is a build-time mistake
// this package cannot recover from at runtime anyway.
var scriptName = regexp.MustCompile(ScriptNamePattern)

// ErrScriptName is every refusal about a script's NAME.
//
// One sentinel for the whole class, with the specific sentence beside it,
// because the layers above have exactly one decision to make about all of
// them: this is a configuration mistake in the workflow tree, report it
// and run nothing. Nothing distinguishes "has a space in it" from "has no
// target" programmatically, and a caller that tried would be re-deriving
// the message this package already wrote.
var ErrScriptName = errors.New("workflow: this is not a valid workflow script name")

// ParseScriptName validates a script's basename and returns where that
// script runs.
//
// The target is a RETURN VALUE rather than a field somebody sets, which is
// the whole design: there is no path through this package that produces a
// step whose target was not read off the filename that passed this
// function. See Target's doc for why that matters more than it looks like
// it should.
func ParseScriptName(name string) (Target, error) {
	if name == "" {
		return "", fmt.Errorf("%w: a script's name must not be empty", ErrScriptName)
	}

	// The separator checks come first and are their own sentence, because
	// a name with a separator in it is the one failure that is not really
	// about naming at all: it means a caller handed this function a PATH
	// where a basename belongs, and telling them "does not match the
	// pattern" would send them to rename a file that is fine.
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf(
			"%w: %q contains a path separator, so it is a path rather than a script name; a hook script must sit directly in its stage directory and is never referred to by path",
			ErrScriptName, name)
	}

	if name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q names a directory, not a script", ErrScriptName, name)
	}

	if strings.HasPrefix(name, ".") {
		return "", fmt.Errorf(
			"%w: %q begins with a dot; a hidden file in a hook directory is an editor's backup, a partial download or a scratch copy far more often than it is a script somebody meant to run, so this product never runs one",
			ErrScriptName, name)
	}

	if err := scriptNameRunes(name); err != nil {
		return "", err
	}

	if !scriptName.MatchString(name) {
		// Everything structural has been accounted for above, so what is
		// left is a name made of acceptable characters that the pattern
		// still rejects, and there are two quite different reasons for
		// that.
		//
		// The common one is a plain *.sh, which states no target. It gets
		// its own sentence because it is the one mistake where the
		// operator's intent is obvious and this product still must not
		// guess: running a quiesce script on the wrong machine is the
		// failure this rule exists to prevent.
		//
		// The other is a name that DOES state a target and whose NAME
		// part is unacceptable ("-backup.local.sh"). Offering to rename
		// that one to "-backup.local.local.sh" would be advice that is
		// also refused, which is worse than no advice, so the suffix is
		// checked before the sentence is chosen.
		if strings.HasSuffix(name, ".sh") && namedTarget(name) == "" {
			base := strings.TrimSuffix(name, ".sh")

			return "", fmt.Errorf(
				"%w: %q does not say where it runs. Rename it %s.%s.sh to run it on this backup server, or %s.%s.sh to run it on the host this backup set pulls from. "+
					"This product will not choose for you: a hook that quiesces a database has to run on the machine holding the database, and guessing wrong is silent",
				ErrScriptName, name, base, TargetLocal, base, TargetRemote)
		}

		return "", fmt.Errorf(
			"%w: %q must be named NAME.%s.sh or NAME.%s.sh, where NAME starts with a letter or digit and is otherwise made of letters, digits, dots, dashes and underscores",
			ErrScriptName, name, TargetLocal, TargetRemote)
	}

	if target := namedTarget(name); target != "" {
		return target, nil
	}

	// Unreachable while ScriptNamePattern has exactly the two
	// alternatives, and present so that adding a third to the pattern
	// without adding it here fails loudly rather than returning the empty
	// target as if it were local.
	return "", fmt.Errorf("%w: %q matched the name rule without naming a known target", ErrScriptName, name)
}

// namedTarget reads the target off a basename's suffix, or returns the
// empty Target when the name states none.
//
// It is a function rather than two inline HasSuffix calls because both
// ParseScriptName's success path and its refusal path have to ask the same
// question, and they have to get the same answer: one of them decides what
// runs where, and the other decides which sentence an operator reads.
func namedTarget(name string) Target {
	for _, t := range targets {
		if strings.HasSuffix(name, "."+string(t)+".sh") {
			return t
		}
	}

	return ""
}

// scriptNameRunes reports the character-level refusals separately from the
// pattern, so each one gets the sentence that names what is actually wrong.
//
// Whitespace, control characters and non-ASCII are three different
// operator situations. A space is usually a copied filename. A control
// character is usually a corrupted archive or a deliberate attempt to
// forge a log line. And a non-ASCII character is usually a homoglyph --
// Cyrillic "е" in "quiesce", a full-width ".ｓｈ" -- which is the case
// worth the most care, because it is the one where the name LOOKS right
// in every tool an operator has.
func scriptNameRunes(name string) error {
	for i, r := range name {
		switch {
		case r == 0:
			return fmt.Errorf("%w: %q contains a NUL byte", ErrScriptName, name)

		case r == unicode.ReplacementChar:
			return fmt.Errorf(
				"%w: %q is not valid UTF-8. A filename this process cannot even render is one it will not execute",
				ErrScriptName, name)

		case unicode.IsSpace(r):
			return fmt.Errorf(
				"%w: %q contains whitespace at byte %d. A hook script's name is rendered into audit records and passed across a connection, so it is restricted to characters that mean the same thing everywhere; rename the file without spaces",
				ErrScriptName, name, i)

		case r < 0x20 || r == 0x7f:
			return fmt.Errorf(
				"%w: %q contains a control character (U+%04X at byte %d). One of these in a filename turns a single audit line into two, which is what makes it worth refusing rather than escaping",
				ErrScriptName, name, r, i)

		case r > unicode.MaxASCII:
			return fmt.Errorf(
				"%w: %q contains the non-ASCII character %q (U+%04X at byte %d). It is refused because it cannot be told apart from an ASCII character by eye: a script named with a Cyrillic \"е\" reads identically to one named with a Latin \"e\" in every listing, review and audit an operator has. Rename the file using letters, digits, dots, dashes and underscores only",
				ErrScriptName, name, r, r, i)
		}
	}

	return nil
}

// compareScriptNames is the ordering rule, and it is one function because
// it is the thing two runs of the same directory have to agree on.
//
// The rule is LC_ALL=C bytewise over the whole basename, target suffix
// included. Not "locale-aware", which would make the order depend on the
// host's environment; not "local scripts first", which would be a second
// rule an operator has to know; not "by the NAME part with the target as a
// tiebreak", which sounds tidier and is worse -- it would put
// 10-dump.local.sh and 10-dump.remote.sh adjacent while separating
// 10-dump.local.sh from 20-sync.local.sh, so an operator numbering their
// scripts would get an order that is neither what they wrote nor what the
// listing shows.
//
// What an operator gets instead is exactly `LC_ALL=C ls`, which is the one
// ordering they can reproduce on their own machine without running this
// product.
func compareScriptNames(a, b string) int {
	return strings.Compare(a, b)
}
