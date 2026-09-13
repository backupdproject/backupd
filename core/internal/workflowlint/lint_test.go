package workflowlint

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "cd.local.sh", tc.script...)

			for _, f := range r.Findings {
				if f.Code == CodeUncheckedCd {
					t.Errorf("%s fired on %q: %+v", f.Code, strings.Join(tc.script, "; "), f)
				}
			}
		})
	}
}

func TestBSH003BlocksARecursiveDeleteThatBecomesARootPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		line1Col,
		wantLine int
	}{
		{name: "an unquoted expansion with a trailing slash", line: "rm -rf $STAGING/", wantLine: 2},
		{name: "a quoted path whose first component is the variable", line: `rm -rf "$STAGING/tmp"`, wantLine: 2},
		{name: "a braced expansion", line: "rm -rf ${STAGING}/*", wantLine: 2},
		{name: "separate flags", line: "rm -r -f $STAGING/tmp", wantLine: 2},
		{name: "a command substitution", line: "rm -rf $(cat /tmp/target)/data", wantLine: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := report(t, "clean.local.sh", "#!/bin/bash", tc.line)

			f := finding(t, r, CodeRecursiveRemoveRoot)
			if f.Severity != SeverityError {
				t.Errorf("%s severity = %q, want %q", f.Code, f.Severity, SeverityError)
			}
			if f.Line != tc.wantLine {
				t.Errorf("%s line = %d, want %d", f.Code, f.Line, tc.wantLine)
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
		{"set -u, which aborts on an unset variable", []string{"#!/bin/bash", "set -u", "rm -rf $STAGING/tmp"}},
		{"set -o nounset", []string{"#!/bin/bash", "set -o nounset", "rm -rf $STAGING/tmp"}},
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

func TestHostileBytesProduceAVerdictRatherThanAPanic(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
	}{
		{"a NUL byte", []byte("#!/bin/bash\necho \x00hi\n")},
		{"invalid UTF-8", []byte("#!/bin/bash\necho '\xff\xfe'\n")},
		{"no newline at all", []byte("echo hi")},
		{"only a heredoc opener", []byte("#!/bin/bash\ncat <<EOF\n")},
		{"empty", []byte("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Report(context.Background(), "hostile.local.sh", tc.src)

			if !r.Examined && r.NotExaminedReason == "" {
				t.Error("the report neither examined the script nor said why")
			}
		})
	}
}

func TestPathologicalNestingIsBoundedRatherThanHanging(t *testing.T) {
	// Deeply nested command substitution: the shape that would drive a
	// recursive-descent parser off the stack if nothing bounded the
	// input, and a stack overflow in Go is fatal rather than
	// recoverable. It must answer -- either verdict is fine -- and it
	// must not panic or hang.
	var b strings.Builder
	for range 20_000 {
		b.WriteString("$(")
	}
	b.WriteString("true")
	for range 20_000 {
		b.WriteString(")")
	}

	done := make(chan ScriptReport, 1)
	go func() { done <- Report(context.Background(), "nested.local.sh", []byte(b.String())) }()

	select {
	case r := <-done:
		if !r.Examined && r.NotExaminedReason == "" {
			t.Error("the report neither examined the script nor said why")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a pathologically nested script did not produce a report")
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
