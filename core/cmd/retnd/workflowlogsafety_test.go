package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/workflow"
	"github.com/retnd/retnd/core/service"
)

// What a hook's output may do to the terminal it is printed on (#813).
//
// `workflow run log` prints a source host's bytes, and a source host is
// the least trusted machine in this deployment. An escape sequence in
// that stream can clear the screen, move the cursor back over lines this
// product printed and rewrite them -- so an operator reading the log of
// an interrupted cleanup is exactly the reader a spoofed "cleanup
// completed" would fool.

// aMaliciousLogPage is one page whose single record carries what a hostile
// hook would print: a clear-screen, a cursor move, a carriage return and
// an embedded newline claiming to be this product's own line.
func aMaliciousLogPage() service.WorkflowStepLogPage {
	return service.WorkflowStepLogPage{
		RunID:  "wfr_1",
		StepID: "0000~global~before~10-quiesce.local.sh",
		Records: []service.WorkflowStepLogRecord{{
			Seq:    1,
			StepID: "0000~global~before~10-quiesce.local.sh",
			Stream: "stdout",
			Kind:   string(workflow.LogOutput),
			At:     time.Date(2026, 9, 13, 4, 5, 6, 0, time.UTC),
			Text:   "quiescing\x1b[2J\x1b[1;1Hbackupd: cleanup completed\rall good\x07",
		}},
		Cursor:    1,
		Complete:  true,
		StepState: string(workflow.StateSuccess),
	}
}

func TestStepLogOutputCannotDriveTheTerminal(t *testing.T) {
	var out bytes.Buffer
	printStepLogPage(&out, aMaliciousLogPage(), false)

	rendered := out.String()
	for _, forbidden := range []string{"\x1b", "\r", "\x07"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the rendered line carries the control byte %q, so a hook on a source host can drive this terminal: %q", forbidden, rendered)
		}
	}
	// One record is one line: an embedded newline would let a hook print
	// a second line carrying this product's own prefix.
	if n := strings.Count(strings.TrimSuffix(rendered, "\n"), "\n"); n != 0 {
		t.Errorf("one record rendered as %d lines: %q", n+1, rendered)
	}
	// And the text is still readable, which is the point of replacing
	// rather than deleting: a line quietly missing bytes cannot be
	// debugged.
	for _, want := range []string{"quiescing", "all good"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the hook's own words were lost: %q", rendered)
		}
	}
}

func TestJSONStepLogOutputIsLeftExactlyAsTheHookWroteIt(t *testing.T) {
	var out bytes.Buffer
	page := aMaliciousLogPage()
	printStepLogPage(&out, page, true)

	var got service.WorkflowStepLogRecord
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatalf("the --json line does not parse: %v\n%s", err, out.String())
	}
	if got.Text != page.Records[0].Text {
		t.Errorf("the --json read altered the hook's bytes:\n got %q\nwant %q", got.Text, page.Records[0].Text)
	}
	// JSON encoding is what makes that safe: nothing below 0x20 reaches
	// the terminal literally.
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("the --json line carries a raw escape byte: %q", out.String())
	}
}
