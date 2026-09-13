package workflowlint

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The rule corpus. One triggering script per rule, asserting the exact
// code, severity and position, and one script per rule that must NOT
// trigger it -- which is the half that keeps this feature usable: a rule
// that fires on a working hook is a rule an operator turns off, and an
// error-severity one is a save they cannot perform.

func TestACleanScriptReportsNothing(t *testing.T) {
	r := report(t, "quiesce.local.sh",
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		"",
		"main() {",
		`  printf '%s\n' "$1"`,
		"}",
		"",
		`main "${BACKUPD_RUN_ID:-none}"`,
	)

	if r.ParseError != nil {
		t.Fatalf("a well-formed script reported a parse fault: %+v", r.ParseError)
	}
	if !r.Examined {
		t.Fatalf("the script was not examined: %s", r.NotExaminedReason)
	}
	if len(r.Findings) != 0 {
		t.Fatalf("a clean script produced findings: %+v", r.Findings)
	}
	if r.Blocks() {
		t.Fatal("a clean script blocks a save")
	}
}

func TestAParseErrorIsReportedWithItsPositionAndBlocks(t *testing.T) {
	r := report(t, "broken.local.sh",
		"#!/bin/bash",
		"if true",
		"then",
		"  echo hi",
	)

	if r.ParseError == nil {
		t.Fatal("an unterminated if reported no parse fault")
	}
	if r.ParseError.Line == 0 || r.ParseError.Col == 0 {
		t.Errorf("the parse fault carries no position: %+v", r.ParseError)
	}
	if r.ParseError.Message == "" {
		t.Error("the parse fault carries no message")
	}
	if !r.Blocks() {
		t.Error("a script that does not parse does not block a save")
	}
	if len(r.Findings) != 0 {
		t.Errorf("rules ran over a script with no parse tree: %+v", r.Findings)
	}
}

func TestTheParseVerdictAcceptsBashThatIsNotPosixSh(t *testing.T) {
	// The variant is load bearing: parsed as sh, both of these are
	// errors, and a precondition of saving that refuses working scripts
	// is the one failure mode it must not have.
	r := report(t, "arrays.local.sh",
		"#!/bin/bash",
		"declare -a paths=(/srv /var)",
		"if [[ ${#paths[@]} -gt 1 ]]; then",
		"  echo many",
		"fi",
	)

	if r.ParseError != nil {
		t.Fatalf("bash arrays and [[ ]] reported a parse fault: %+v", r.ParseError)
	}
}

func TestBSH001ReportsAnUnquotedExpansionInACommandArgument(t *testing.T) {
	r := report(t, "unquoted.local.sh",
		"#!/bin/bash",
		"target=$1",
		"tar -cf backup.tar $target",
	)

	f := finding(t, r, CodeUnquotedExpansion)
	if f.Severity != SeverityInfo {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityInfo)
	}
	if f.Line != 3 || f.Col != 20 {
		t.Errorf("%s position = %d:%d, want 3:20", f.Code, f.Line, f.Col)
	}
	if !strings.Contains(f.Message, "$target") {
		t.Errorf("%s message does not name the expansion: %q", f.Code, f.Message)
	}
	if r.Blocks() {
		t.Error("an info-severity finding blocks a save; only a parse error and an error finding may")
	}
}

func TestBSH001ReportsACommandSubstitution(t *testing.T) {
	r := report(t, "cmdsub.local.sh",
		"#!/bin/bash",
		"stat -c %s $(cat /etc/hostname)",
	)

	f := finding(t, r, CodeUnquotedExpansion)
	if f.Line != 2 || f.Col != 12 {
		t.Errorf("%s position = %d:%d, want 2:12", f.Code, f.Line, f.Col)
	}
}

// TestBSH001IsSilentWhereTheShellDoesNotSplit is the rule's whole claim
// to being sound. Each of these is an ordinary line in a working hook,
// and each is a context in which an expansion needs no quotes.
func TestBSH001IsSilentWhereTheShellDoesNotSplit(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"double quoted", `echo "$target"`},
		{"an assignment's right-hand side", "copy=$target"},
		{"a for loop's list, where splitting is the point", "for f in $files; do echo \"$f\"; done"},
		{"a [[ ]] operand, which does not split", "if [[ -n $target ]]; then echo yes; fi"},
		{"a case subject", "case $target in a) echo a ;; esac"},
		{"an arithmetic expression", "echo $((count + 1))"},
		{"an exit status", "echo $?"},
		{"a length", "echo ${#target}"},
		{"the command name, built on purpose", "$runner --version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "quiet.local.sh", "#!/bin/bash", "target=/srv", "files=/srv", "runner=echo", "count=1", tc.line)

			for _, f := range r.Findings {
				if f.Code == CodeUnquotedExpansion {
					t.Errorf("%s fired on %q: %+v", f.Code, tc.line, f)
				}
			}
		})
	}
}

// TestBSH001ReportsAnIndirectExpansion is the half of ParamExp.Excl that
// is not a name expansion. `${!x}` produces the VALUE of the variable x
// names, and that value splits on whitespace like any other; only
// `${!prefix*}` and `${!prefix@}` are lists of names. Excluding all
// three, which is what a check on Excl alone does, hides this.
func TestBSH001ReportsAnIndirectExpansion(t *testing.T) {
	r := report(t, "indirect.local.sh",
		"#!/bin/bash",
		"ref=target",
		"tar -cf out.tar ${!ref}",
	)

	f := finding(t, r, CodeUnquotedExpansion)
	if f.Severity != SeverityInfo {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityInfo)
	}
	if f.Line != 3 || f.Col != 17 {
		t.Errorf("%s position = %d:%d, want 3:17", f.Code, f.Line, f.Col)
	}
	if !strings.Contains(f.Message, "indirect") {
		t.Errorf("%s message does not say the expansion is indirect: %q", f.Code, f.Message)
	}
}

func TestBSH001IsSilentOnTheNameListForms(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"${!prefix*}, which is a list of names", `printf '%s\n' ${!BACKUPD_*}`},
		{"${!prefix@}, the same list", `printf '%s\n' ${!BACKUPD_@}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "names.local.sh", "#!/bin/bash", tc.line)
			parsed(t, r)

			for _, f := range r.Findings {
				if f.Code == CodeUnquotedExpansion {
					t.Errorf("%s fired on %q: %+v", f.Code, tc.line, f)
				}
			}
		})
	}
}

// A declaration builtin's `NAME=value` argument is an assignment
// context: the shell does not split or glob the right-hand side, so
// advice to quote it is advice that changes nothing.
func TestBSH001IsSilentOnADeclarationBuiltinsAssignment(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"export", "export NAME=$x"},
		{"readonly", "readonly ROOT=$root"},
		{"declare behind a flag", "declare -r NAME=$x"},
		{"typeset", "typeset NAME=$x"},
		{"local, inside the function it belongs to", "f() { local dir=$1; }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "decl.local.sh", "#!/bin/bash", tc.line)
			parsed(t, r)

			for _, f := range r.Findings {
				if f.Code == CodeUnquotedExpansion {
					t.Errorf("%s fired on %q: %+v", f.Code, tc.line, f)
				}
			}
		})
	}
}

// The other half of the same claim: a declaration builtin's NON
// assignment argument is an ordinary word and does split.
func TestBSH001ReportsADeclarationBuiltinsNakedArgument(t *testing.T) {
	r := report(t, "decl.local.sh",
		"#!/bin/bash",
		"export $names",
	)

	f := finding(t, r, CodeUnquotedExpansion)
	if f.Severity != SeverityInfo {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityInfo)
	}
	if f.Line != 2 || f.Col != 8 {
		t.Errorf("%s position = %d:%d, want 2:8", f.Code, f.Line, f.Col)
	}
}

func TestBSH002ReportsACdWhoseFailureNothingNotices(t *testing.T) {
	r := report(t, "cd.local.sh",
		"#!/bin/bash",
		"cd /srv/data",
		"rm -rf ./old",
	)

	f := finding(t, r, CodeUncheckedCd)
	if f.Severity != SeverityWarning {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityWarning)
	}
	if f.Line != 2 || f.Col != 1 {
		t.Errorf("%s position = %d:%d, want 2:1", f.Code, f.Line, f.Col)
	}
	if r.Blocks() {
		t.Error("a warning blocks a save; the documented threshold is a parse error or an error finding")
	}
}

func TestBSH002IsSilentWhereTheFailureIsHandled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script []string
	}{
		{"set -e", []string{"#!/bin/bash", "set -e", "cd /srv/data"}},
		{"set -o errexit", []string{"#!/bin/bash", "set -o errexit", "cd /srv/data"}},
		{"|| exit", []string{"#!/bin/bash", "cd /srv/data || exit 1"}},
		{"&& work", []string{"#!/bin/bash", "cd /srv/data && echo there"}},
		{"an if condition", []string{"#!/bin/bash", "if cd /srv/data; then echo there; fi"}},
		{"a negated test", []string{"#!/bin/bash", "if ! cd /srv/data; then exit 1; fi"}},
		{"a while condition", []string{"#!/bin/bash", "while cd /srv/data; do break; done"}},
		{"no argument, so not this shape", []string{"#!/bin/bash", "cd"}},
		{"the right operand of && in a condition, which decides its status", []string{"#!/bin/bash", "if true && cd /srv/data; then echo there; fi"}},
		{"the last command of a pipeline condition", []string{"#!/bin/bash", "if echo x | cd /srv/data; then echo there; fi"}},
		{"an until condition", []string{"#!/bin/bash", "until cd /srv/data; do break; done"}},
		{"set +e and then set -e again", []string{"#!/bin/bash", "set -e", "set +e", "set -e", "cd /srv/data"}},
		{"a set +e that may never run", []string{"#!/bin/bash", "set -e", `if [ -n "$1" ]; then set +e; fi`, "cd /srv/data"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "cd.local.sh", tc.script...)
			parsed(t, r)

			for _, f := range r.Findings {
				if f.Code == CodeUncheckedCd {
					t.Errorf("%s fired on %q: %+v", f.Code, strings.Join(tc.script, "; "), f)
				}
			}
		})
	}
}

// An `if` condition is a LIST, and a list's exit status is its LAST
// statement's. `if cd /missing; echo ready; then` branches on the echo,
// so the cd's failure is exactly as unnoticed as it would be on a line
// of its own -- and the script then runs its `then` body in the wrong
// directory, which is the whole shape BSH002 is about.
func TestBSH002ReportsACdThatDoesNotDecideItsConditionsStatus(t *testing.T) {
	r := report(t, "cd.local.sh",
		"#!/bin/bash",
		"if cd /missing; echo ready; then",
		"  rm -rf ./old",
		"fi",
	)

	f := finding(t, r, CodeUncheckedCd)
	if f.Severity != SeverityWarning {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityWarning)
	}
	if f.Line != 2 || f.Col != 4 {
		t.Errorf("%s position = %d:%d, want 2:4", f.Code, f.Line, f.Col)
	}
}

// `set -e` protects what comes after it, and `set +e` takes the
// protection away again. Reading the first line and ignoring the second
// -- which is what a write-only flag does -- silences this rule over a
// script that turned errexit off on purpose.
func TestBSH002ReportsACdAfterErrexitWasTurnedBackOff(t *testing.T) {
	r := report(t, "cd.local.sh",
		"#!/bin/bash",
		"set -e",
		"set +e",
		"cd /srv/data",
	)

	f := finding(t, r, CodeUncheckedCd)
	if f.Severity != SeverityWarning {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityWarning)
	}
	if f.Line != 4 || f.Col != 1 {
		t.Errorf("%s position = %d:%d, want 4:1", f.Code, f.Line, f.Col)
	}
}

func TestBSH003BlocksARecursiveDeleteThatBecomesARootPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		script   []string
		wantLine int
		wantCol  int
	}{
		{
			name:     "an unquoted expansion with a trailing slash",
			script:   []string{"#!/bin/bash", "rm -rf $STAGING/"},
			wantLine: 2, wantCol: 8,
		},
		{
			name:     "a quoted path whose first component is the variable",
			script:   []string{"#!/bin/bash", `rm -rf "$STAGING/tmp"`},
			wantLine: 2, wantCol: 8,
		},
		{
			name:     "a braced expansion",
			script:   []string{"#!/bin/bash", "rm -rf ${STAGING}/*"},
			wantLine: 2, wantCol: 8,
		},
		{
			name:     "separate flags",
			script:   []string{"#!/bin/bash", "rm -r -f $STAGING/tmp"},
			wantLine: 2, wantCol: 10,
		},
		{
			name:     "a command substitution",
			script:   []string{"#!/bin/bash", "rm -rf $(cat /tmp/target)/data"},
			wantLine: 2, wantCol: 8,
		},
		{
			// set -u aborts on an UNSET parameter and says nothing about
			// one that is set to the empty string. This is the script
			// the old blanket exemption silenced, and it deletes /tmp.
			name:     "set -u, with a variable that is set to nothing",
			script:   []string{"#!/bin/bash", "set -u", "STAGING=", `rm -rf "$STAGING/tmp"`},
			wantLine: 4, wantCol: 8,
		},
		{
			name:     "an option that was turned back off before the delete",
			script:   []string{"#!/bin/bash", "set -u", "set +u", `rm -rf "$STAGING/tmp"`},
			wantLine: 4, wantCol: 8,
		},
		{
			name:     "rm's long options, which the flag scan used to skip",
			script:   []string{"#!/bin/bash", `rm --recursive --force "$STAGING/tmp"`},
			wantLine: 2, wantCol: 24,
		},
		{
			name:     "-R, which is recursion too",
			script:   []string{"#!/bin/bash", `rm -R -f "$STAGING/tmp"`},
			wantLine: 2, wantCol: 10,
		},
		{
			// /var/../tmp is three components of text and one directory
			// inside the root, which is what rm acts on.
			name:     "a path that cleans down to a root-level one",
			script:   []string{"#!/bin/bash", `rm -rf "$ROOT/var/../tmp"`},
			wantLine: 2, wantCol: 8,
		},
		{
			name:     "a default whose fallback can itself be empty",
			script:   []string{"#!/bin/bash", `rm -rf "${STAGING:-$OTHER}/tmp"`},
			wantLine: 2, wantCol: 8,
		},
		{
			// The non-colon form covers UNSET only: a STAGING assigned
			// the empty string is set, so the fallback never runs.
			name:     "a non-colon default, which leaves an empty value empty",
			script:   []string{"#!/bin/bash", `rm -rf "${STAGING-/srv/stage}/tmp"`},
			wantLine: 2, wantCol: 8,
		},
		{
			name:     "an alternate operator, which produces nothing without a value",
			script:   []string{"#!/bin/bash", `rm -rf "${STAGING:+/srv/stage}/tmp"`},
			wantLine: 2, wantCol: 8,
		},
		{
			// The stand-in for an expansion that cannot be empty keeps
			// the component count honest in BOTH directions: /N/ is
			// still one directory inside the root.
			name:     "a hole beside an expansion that cannot be one, still root-level",
			script:   []string{"#!/bin/bash", `rm -rf "/${#items}/$STAGING"`},
			wantLine: 2, wantCol: 8,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "clean.local.sh", tc.script...)

			f := finding(t, r, CodeRecursiveRemoveRoot)
			if f.Severity != SeverityError {
				t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityError)
			}
			if f.Line != tc.wantLine || f.Col != tc.wantCol {
				t.Errorf("%s position = %d:%d, want %d:%d", f.Code, f.Line, f.Col, tc.wantLine, tc.wantCol)
			}
			if !r.Blocks() {
				t.Error("an error-severity finding does not block a save")
			}
			if len(r.Blocking()) == 0 {
				t.Error("Blocking() does not carry the error finding")
			}
		})
	}
}

func TestBSH003IsSilentWhereTheDeleteIsNotRootLevel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script []string
	}{
		{"a path the deployment owns", []string{"#!/bin/bash", "rm -rf /srv/backupd/cache/$NAME"}},
		{"an expansion that fails when unset", []string{"#!/bin/bash", `rm -rf "${STAGING:?}/tmp"`}},
		{"an expansion with a default", []string{"#!/bin/bash", `rm -rf "${STAGING:-/srv/backupd/stage}/tmp"`}},
		{"an assign-default that cannot be empty", []string{"#!/bin/bash", `rm -rf "${STAGING:=/srv/backupd/stage}/tmp"`}},
		{"a non-colon error branch, read as a guard on purpose", []string{"#!/bin/bash", `rm -rf "${STAGING?}/tmp"`}},
		{"a length, which is a number and never empty", []string{"#!/bin/bash", `rm -rf "/${#items}/tmp"`}},
		{"a hole beside an expansion that cannot be one", []string{"#!/bin/bash", `rm -rf "/$STAGING/${#items}/data"`}},
		{"everything after -- is an operand and not a flag", []string{"#!/bin/bash", `rm -- -rf "$STAGING/tmp"`}},
		{"set -u with an expansion that really does abort", []string{"#!/bin/bash", "set -u", `rm -rf "${STAGING:?}/tmp"`}},
		{"no force flag, so it prompts rather than deleting", []string{"#!/bin/bash", "rm -r $STAGING/tmp"}},
		{"no recursion", []string{"#!/bin/bash", "rm -f $STAGING/tmp"}},
		{"a literal path with no expansion at all", []string{"#!/bin/bash", "rm -rf /tmp/backupd.lock"}},
		{"a relative target", []string{"#!/bin/bash", "rm -rf ./$NAME/data"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "clean.local.sh", tc.script...)

			for _, f := range r.Findings {
				if f.Code == CodeRecursiveRemoveRoot {
					t.Errorf("%s fired on %q: %+v", f.Code, strings.Join(tc.script, "; "), f)
				}
			}
			if r.Blocks() {
				t.Errorf("%q blocks a save: %+v", strings.Join(tc.script, "; "), r.Blocking())
			}
		})
	}
}

func TestBSH004ReportsAPipelineWhoseEarlierFailureSetEWillNotSee(t *testing.T) {
	r := report(t, "dump.remote.sh",
		"#!/bin/bash",
		"set -e",
		"pg_dump mydb | gzip > /srv/out.gz",
	)

	f := finding(t, r, CodeMaskedPipelineFailure)
	if f.Severity != SeverityWarning {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityWarning)
	}
	if f.Line != 3 || f.Col != 1 {
		t.Errorf("%s position = %d:%d, want 3:1", f.Code, f.Line, f.Col)
	}
}

// `set -o pipefail` followed by `set +o pipefail` is a script that
// turned the protection back off, and the pipeline below it is exactly
// the one this rule exists for.
func TestBSH004ReportsAPipelineAfterPipefailWasTurnedBackOff(t *testing.T) {
	r := report(t, "dump.remote.sh",
		"#!/bin/bash",
		"set -e",
		"set -o pipefail",
		"set +o pipefail",
		"pg_dump mydb | gzip > /srv/out.gz",
	)

	f := finding(t, r, CodeMaskedPipelineFailure)
	if f.Severity != SeverityWarning {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityWarning)
	}
	if f.Line != 5 || f.Col != 1 {
		t.Errorf("%s position = %d:%d, want 5:1", f.Code, f.Line, f.Col)
	}
}

func TestBSH004IsOneFindingPerScriptAndSilentWhenHandled(t *testing.T) {
	t.Run("pipefail", func(t *testing.T) {
		r := report(t, "dump.remote.sh",
			"#!/bin/bash",
			"set -euo pipefail",
			"pg_dump mydb | gzip > /srv/out.gz",
			"cat /srv/out.gz | wc -c",
		)

		for _, f := range r.Findings {
			if f.Code == CodeMaskedPipelineFailure {
				t.Errorf("%s fired on a script that sets pipefail: %+v", f.Code, f)
			}
		}
	})

	t.Run("set -e turned back off, so nothing claimed to stop", func(t *testing.T) {
		r := report(t, "dump.remote.sh",
			"#!/bin/bash",
			"set -e",
			"set +e",
			"pg_dump mydb | gzip > /srv/out.gz",
		)

		for _, f := range r.Findings {
			if f.Code == CodeMaskedPipelineFailure {
				t.Errorf("%s fired on a script that turned errexit off: %+v", f.Code, f)
			}
		}
	})

	t.Run("a set +o pipefail that may never run", func(t *testing.T) {
		r := report(t, "dump.remote.sh",
			"#!/bin/bash",
			"set -e",
			"set -o pipefail",
			`if [ -n "$1" ]; then set +o pipefail; fi`,
			"pg_dump mydb | gzip > /srv/out.gz",
		)

		for _, f := range r.Findings {
			if f.Code == CodeMaskedPipelineFailure {
				t.Errorf("%s fired on a conditional set +o pipefail: %+v", f.Code, f)
			}
		}
	})

	t.Run("no set -e, so nothing claimed to stop", func(t *testing.T) {
		r := report(t, "dump.remote.sh", "#!/bin/bash", "pg_dump mydb | gzip > /srv/out.gz")

		for _, f := range r.Findings {
			if f.Code == CodeMaskedPipelineFailure {
				t.Errorf("%s fired without set -e: %+v", f.Code, f)
			}
		}
	})

	t.Run("several pipelines, one finding", func(t *testing.T) {
		r := report(t, "dump.remote.sh",
			"#!/bin/bash",
			"set -e",
			"pg_dump a | gzip > /srv/a.gz",
			"pg_dump b | gzip > /srv/b.gz",
			"pg_dump c | gzip > /srv/c.gz",
		)

		seen := 0
		for _, f := range r.Findings {
			if f.Code == CodeMaskedPipelineFailure {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("%s reported %d times; the mistake is in the options, not in each pipeline", CodeMaskedPipelineFailure, seen)
		}
	})
}

func TestBSH005ReportsAnUnquotedOperandInSingleBracketTest(t *testing.T) {
	r := report(t, "test.local.sh",
		"#!/bin/bash",
		"reply=$(cat /tmp/reply)",
		"if [ -n $reply ]; then",
		"  echo something",
		"fi",
	)

	f := finding(t, r, CodeUnquotedTestOperand)
	if f.Severity != SeverityWarning {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityWarning)
	}
	if f.Line != 3 || f.Col != 9 {
		t.Errorf("%s position = %d:%d, want 3:9", f.Code, f.Line, f.Col)
	}

	// The specific message and not also the generic one: the two are the
	// same mistake and only one of them is useful here.
	for _, other := range r.Findings {
		if other.Code == CodeUnquotedExpansion && other.Line == 3 {
			t.Errorf("%s also fired inside [ ... ]: %+v", other.Code, other)
		}
	}
}

func TestBSH005IsSilentOnQuotedAndDoubleBracketOperands(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"quoted", `if [ -n "$reply" ]; then echo yes; fi`},
		{"[[ ]], which does not split", "if [[ -n $reply ]]; then echo yes; fi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "test.local.sh", "#!/bin/bash", "reply=x", tc.line)

			for _, f := range r.Findings {
				if f.Code == CodeUnquotedTestOperand {
					t.Errorf("%s fired on %q: %+v", f.Code, tc.line, f)
				}
			}
		})
	}
}

func TestBSH006ReportsAMissingShebangAsStyleOnly(t *testing.T) {
	r := report(t, "noshebang.local.sh", "echo hello")

	f := finding(t, r, CodeMissingShebang)
	if f.Severity != SeverityStyle {
		t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityStyle)
	}
	if f.Line != 1 || f.Col != 1 {
		t.Errorf("%s position = %d:%d, want 1:1", f.Code, f.Line, f.Col)
	}
	if r.Blocks() {
		t.Error("a style finding blocks a save")
	}
}

func TestBSH006IsSilentWithAShebang(t *testing.T) {
	r := report(t, "shebang.local.sh", "#!/usr/bin/env bash", "echo hello")

	for _, f := range r.Findings {
		if f.Code == CodeMissingShebang {
			t.Errorf("%s fired on a script with a shebang: %+v", f.Code, f)
		}
	}
}

func TestTwoReportsOverTheSameBytesAreIdentical(t *testing.T) {
	src := []byte(strings.Join([]string{
		"#!/bin/bash",
		"set -e",
		"cd /srv/data",
		"tar -cf out.tar $files",
		"cat out.tar | wc -c",
		"",
	}, "\n"))

	first := Report(context.Background(), "d.local.sh", src)
	second := Report(context.Background(), "d.local.sh", src)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two reports over the same bytes differ:\n%+v\n%+v", first, second)
	}
	if len(first.Findings) < 2 {
		t.Fatalf("expected several findings to order, got %+v", first.Findings)
	}

	// Sorted by position, which is what makes a report diffable.
	for i := 1; i < len(first.Findings); i++ {
		prev, cur := first.Findings[i-1], first.Findings[i]
		if prev.Line > cur.Line || (prev.Line == cur.Line && prev.Col > cur.Col) {
			t.Errorf("findings are not in position order: %+v then %+v", prev, cur)
		}
	}
}

func TestAScriptLargerThanTheBoundIsNotExaminedRatherThanTruncated(t *testing.T) {
	src := make([]byte, MaxScriptBytes+1)
	for i := range src {
		src[i] = '\n'
	}

	r := Report(context.Background(), "huge.local.sh", src)

	if r.Examined {
		t.Fatal("a script past the bound was examined")
	}
	if r.ParseError != nil {
		t.Errorf("a script past the bound was parsed anyway: %+v", r.ParseError)
	}
	if !strings.Contains(r.NotExaminedReason, "not examined") {
		t.Errorf("reason = %q, want a sentence saying it was not examined", r.NotExaminedReason)
	}
	if r.Blocks() {
		t.Error("a script this check declined to read blocks a save; a gate on what nobody looked at is a gate on the wrong thing")
	}
}

func TestHostileBytesProduceTheRightVerdictRatherThanAPanic(t *testing.T) {
	// Every one of these is small enough to be examined, so the verdict
	// worth asserting is the parse one, per case. "it did not panic" on
	// its own -- the assertion this replaces -- also passes on a report
	// that quietly dropped the script, which is the failure that would
	// matter here.
	for _, tc := range []struct {
		name      string
		src       []byte
		wantFault bool
		wantCodes []string
	}{
		{name: "a NUL byte", src: []byte("#!/bin/bash\necho \x00hi\n")},
		{name: "invalid UTF-8", src: []byte("#!/bin/bash\necho '\xff\xfe'\n"), wantFault: true},
		{name: "no newline at all", src: []byte("echo hi"), wantCodes: []string{CodeMissingShebang}},
		{name: "only a heredoc opener", src: []byte("#!/bin/bash\ncat <<EOF\n"), wantFault: true},
		{name: "empty", src: []byte(""), wantCodes: []string{CodeMissingShebang}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Report(context.Background(), "hostile.local.sh", tc.src)

			if !r.Examined {
				t.Fatalf("the script was not examined: %q", r.NotExaminedReason)
			}

			if tc.wantFault {
				if r.ParseError == nil {
					t.Fatalf("no parse fault; findings %+v", r.Findings)
				}
				if r.ParseError.Line == 0 || r.ParseError.Col == 0 || r.ParseError.Message == "" {
					t.Errorf("the parse fault carries no position or no message: %+v", r.ParseError)
				}
				if !r.Blocks() {
					t.Error("a script that does not parse does not block a save")
				}

				return
			}

			if r.ParseError != nil {
				t.Fatalf("reported a parse fault: %+v", r.ParseError)
			}

			var codes []string
			for _, f := range r.Findings {
				codes = append(codes, f.Code)
			}
			if !reflect.DeepEqual(codes, tc.wantCodes) {
				t.Errorf("findings = %v, want %v: %+v", codes, tc.wantCodes, r.Findings)
			}
			if r.Blocks() {
				t.Errorf("a script with no error finding blocks a save: %+v", r.Blocking())
			}
		})
	}
}

func TestEveryRuleCarriesAPositionAndARemedy(t *testing.T) {
	// A finding with a code and no remedy is a finding an operator reads
	// twice and acts on never, and one with no position is a place
	// nobody can go to. Both are checked for every rule this package
	// has; the WORDING is free to be improved and is not asserted.
	//
	// Two of the rules are mutually exclusive by construction -- BSH002
	// fires only WITHOUT `set -e` and BSH004 only WITH it -- so this is
	// a script per rule rather than one script that triggers all of
	// them.
	for code, script := range map[string][]string{
		CodeUnquotedExpansion:     {"#!/bin/bash", "tar -cf out.tar $files"},
		CodeUncheckedCd:           {"#!/bin/bash", "cd /srv/data", "echo there"},
		CodeRecursiveRemoveRoot:   {"#!/bin/bash", "rm -rf $STAGING/tmp"},
		CodeMaskedPipelineFailure: {"#!/bin/bash", "set -e", "pg_dump db | gzip > /srv/out.gz"},
		CodeUnquotedTestOperand:   {"#!/bin/bash", "if [ -n $reply ]; then echo yes; fi"},
		CodeMissingShebang:        {"echo hi"},
	} {
		t.Run(code, func(t *testing.T) {
			f := finding(t, report(t, "rule.local.sh", script...), code)

			if len(f.Message) < 40 {
				t.Errorf("%s carries no remedy: %q", f.Code, f.Message)
			}
			if f.Line == 0 || f.Col == 0 {
				t.Errorf("%s carries no position: %+v", f.Code, f)
			}
			if f.Severity != SeverityError && f.Severity != SeverityWarning &&
				f.Severity != SeverityInfo && f.Severity != SeverityStyle {
				t.Errorf("%s carries the severity %q, which is not one of this package's four", f.Code, f.Severity)
			}
		})
	}
}

func report(t *testing.T, name string, lines ...string) ScriptReport {
	t.Helper()

	return Report(context.Background(), name, []byte(strings.Join(lines, "\n")+"\n"))
}

// parsed fails a "this rule stays silent here" row that is silent only
// because there was nothing to look at. An unexamined or unparsed script
// carries no findings at all, so a table row that passes for that reason
// is asserting nothing about the rule it names.
func parsed(t *testing.T, r ScriptReport) {
	t.Helper()

	if !r.Examined {
		t.Fatalf("the script was not examined: %s", r.NotExaminedReason)
	}
	if r.ParseError != nil {
		t.Fatalf("the script did not parse: %+v", r.ParseError)
	}
}

func finding(t *testing.T, r ScriptReport, code string) Finding {
	t.Helper()

	if !r.Examined {
		t.Fatalf("the script was not examined: %s", r.NotExaminedReason)
	}
	if r.ParseError != nil {
		t.Fatalf("the script did not parse: %+v", r.ParseError)
	}
	for _, f := range r.Findings {
		if f.Code == code {
			return f
		}
	}
	t.Fatalf("no %s in %+v", code, r.Findings)

	return Finding{}
}
