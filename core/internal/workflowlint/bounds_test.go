package workflowlint

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The bound that keeps a hook directory from killing the daemon (#906,
// review BLOCKER 1).
//
// These tests are in their own file because of what the first one does: a
// regression here is not a failed assertion, it is `fatal error: stack
// overflow` and a dead test binary with no output attributable to a test.
// That is the whole point -- the failure mode being defended against
// cannot be observed as a normal failure -- so the test that provokes it
// is kept where somebody reading the crash can find it.

// TestAFullSizedScriptOfNestedSubstitutionAnswersRatherThanCrashing drives
// the exact shape that was reproduced as a process kill: a full
// MaxScriptBytes of `$(`, which is a legal script at the DEFAULT
// workflows.max_script_size_bytes, since that default and MaxScriptBytes
// are the same 1 MiB.
//
// Before the pre-scan this input reached mvdan.cc/sh's recursive descent
// and exhausted the goroutine stack. It asserts three things and each one
// matters: that a verdict comes back at all (the process is alive), that
// the verdict is NOT EXAMINED with a reason rather than a silent pass, and
// that it does not refuse a save -- fail-open, because refusing every
// write over a script this package declined to read would be a gate on
// the wrong thing.
func TestAFullSizedScriptOfNestedSubstitutionAnswersRatherThanCrashing(t *testing.T) {
	src := []byte(strings.Repeat("$(", MaxScriptBytes/2))
	if len(src) != MaxScriptBytes {
		t.Fatalf("the fixture is %d bytes, want exactly MaxScriptBytes (%d) so this is the shape a default deployment accepts", len(src), MaxScriptBytes)
	}

	done := make(chan ScriptReport, 1)
	go func() { done <- Report(context.Background(), "nested.local.sh", src) }()

	select {
	case r := <-done:
		if r.Examined {
			t.Fatalf("a script nested %d deep was handed to the parser", MaxScriptBytes/2)
		}
		if !strings.Contains(r.NotExaminedReason, "nest") {
			t.Errorf("reason = %q, want it to say the nesting is what stopped the check", r.NotExaminedReason)
		}
		if r.ParseError != nil {
			t.Errorf("a script nobody parsed reports a parse fault: %+v", r.ParseError)
		}
		if r.Blocks() {
			t.Error("a script this check declined to read refuses a save; the bound fails open on purpose")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no verdict came back for a maximally nested script")
	}
}

// The other shapes of the same bound. Each is a construct the parser
// descends through, and a bound that only counted `$(` would be a bound
// somebody walks around by writing `((((`.
//
// The old backtick spelling is deliberately NOT here, and that is a
// property worth pinning rather than an omission: see
// TestBackticksCannotReachTheCapAndDoNotNeedTo.
func TestEveryRecursionProducingConstructIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"subshells", strings.Repeat("(", MaxNestingDepth+1)},
		{"brace groups", strings.Repeat("{ ", MaxNestingDepth+1)},
		{"arithmetic", strings.Repeat("$((", MaxNestingDepth+1)},
		{"parameter expansions", strings.Repeat("${x", MaxNestingDepth+1)},
		{"test brackets", strings.Repeat("[ ", MaxNestingDepth+1)},
		{"command substitution", strings.Repeat("$(", MaxNestingDepth+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Report(context.Background(), "deep.local.sh", []byte(tc.src))

			if r.Examined {
				t.Errorf("%s nested past the cap was parsed anyway", tc.name)
			}
			if r.NotExaminedReason == "" {
				t.Errorf("%s nested past the cap was declined with no reason", tc.name)
			}
		})
	}
}

// A backtick TOGGLES a level rather than opening one, which is what the
// shell does with it, so a run of them never reaches the cap. That is
// correct rather than a hole: nesting backticks requires escaping every
// inner one, and the escaping doubles per level, so a megabyte of source
// buys about twenty levels of recursion. The construct cannot be used to
// drive the parser deep, and a script made of backticks is examined
// normally.
func TestBackticksCannotReachTheCapAndDoNotNeedTo(t *testing.T) {
	if got := maxNestingDepth([]byte(strings.Repeat("`", 2*(MaxNestingDepth+1)))); got > 1 {
		t.Errorf("maxNestingDepth over a run of backticks = %d, want at most 1: a backtick closes the one before it", got)
	}

	r := Report(context.Background(), "backtick.local.sh", []byte("#!/bin/bash\necho \"`date -u`\"\n"))
	if !r.Examined {
		t.Errorf("an ordinary backtick substitution was declined: %s", r.NotExaminedReason)
	}
}

// And the other direction, which is what stops the bound being a bound on
// ordinary scripts: a hook that nests the way a person writes still gets
// a real verdict.
func TestOrdinaryNestingIsExaminedNormally(t *testing.T) {
	r := report(t, "ordinary.local.sh",
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		`stamp="$(date -u +%Y%m%dT%H%M%SZ)"`,
		`size=$(( $(stat -c %s "/srv/dump-${stamp}.sql") / 1024 ))`,
		`if [[ -n "${size}" && "${size}" -gt 0 ]]; then`,
		`  printf '%s\n' "${stamp}: ${size} KiB"`,
		"fi",
	)

	if !r.Examined {
		t.Fatalf("an ordinary hook was declined: %s", r.NotExaminedReason)
	}
	if r.ParseError != nil {
		t.Fatalf("an ordinary hook reported a parse fault: %+v", r.ParseError)
	}
}

// The counter itself, over the shapes where being wrong in the wrong
// direction would matter. Over-counting is safe (it only fails open);
// under-counting is what ends the process, so the closing behaviour is
// pinned: a balanced script must not accumulate depth across it.
func TestTheNestingCounterDoesNotAccumulateAcrossBalancedConstructs(t *testing.T) {
	var b strings.Builder
	for range MaxNestingDepth + 100 {
		b.WriteString("$(true)")
	}

	if got := maxNestingDepth([]byte(b.String())); got != 1 {
		t.Errorf("maxNestingDepth over %d balanced substitutions = %d, want 1; a counter that accumulated would decline every long script", MaxNestingDepth+100, got)
	}

	if got := maxNestingDepth([]byte("$(a $(b $(c)))")); got != 3 {
		t.Errorf("maxNestingDepth of three nested substitutions = %d, want 3", got)
	}
	if got := maxNestingDepth([]byte("echo hello")); got != 0 {
		t.Errorf("maxNestingDepth of a flat script = %d, want 0", got)
	}
}
