package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/workflow"
	"github.com/backupdproject/backupd/core/service"
)

// `workflow run log --follow`: the cursor arithmetic, driven directly.
//
// This is the one part of the workflow CLI with a loop in it, and the
// property it has to have is the one a terminal cannot demonstrate: a
// step's output, split across pages, with the read interrupted in the
// middle, is printed once each in order with nothing missed and nothing
// repeated. A follow of a real step could not be made to produce that
// sequence on demand, which is why followStepLogs takes a source
// interface and two writers -- the same reason followActivity does.

// scriptedStepLogs is a step's whole output, served a page at a time
// from whatever cursor the follower sends.
//
// It answers the way core/service does rather than the way a test would
// find convenient, because the follower is being tested against that
// contract: the page's Cursor is the last record IN THE PAGE (and the
// caller's own After when the page is empty), and Complete is only true
// once the end has actually been reached. A fake that advanced the cursor
// past records it had not served, or reported Complete early, would prove
// nothing about the loop.
type scriptedStepLogs struct {
	records  []service.WorkflowStepLogRecord
	pageSize int

	// failOnCall makes the Nth call fail, which is the interruption: a
	// journal read that failed, a connection dropped, an operator's
	// terminal closed. 0 never fails.
	failOnCall int

	calls int

	// seenAfter records every cursor the follower asked from, so a test
	// can assert it never asked twice from the same place or went
	// backwards.
	seenAfter []uint64
}

var errScriptedInterruption = errors.New("the scripted read was interrupted")

func (s *scriptedStepLogs) WorkflowStepLogs(_ context.Context, req service.WorkflowStepLogRequest) (service.WorkflowStepLogPage, error) {
	s.calls++
	s.seenAfter = append(s.seenAfter, req.After)
	if s.calls == s.failOnCall {
		return service.WorkflowStepLogPage{}, errScriptedInterruption
	}

	page := service.WorkflowStepLogPage{RunID: req.RunID, StepID: req.StepID, Cursor: req.After, StepState: string(workflow.StateSuccess)}
	for _, rec := range s.records {
		if rec.Seq <= req.After {
			continue
		}
		page.Records = append(page.Records, rec)
		page.Cursor = rec.Seq
		if len(page.Records) == s.pageSize {
			break
		}
	}
	if len(page.Records) > 0 {
		last := page.Records[len(page.Records)-1].Seq
		page.Complete = last == s.records[len(s.records)-1].Seq
	} else {
		page.Complete = true
	}

	return page, nil
}

func scriptedRecords(n int) []service.WorkflowStepLogRecord {
	out := make([]service.WorkflowStepLogRecord, 0, n)
	for i := range n {
		seq := uint64(i + 1)
		stream := workflow.LogStreamStdout
		if seq%3 == 0 {
			stream = workflow.LogStreamStderr
		}
		out = append(out, service.WorkflowStepLogRecord{
			Seq:    seq,
			StepID: "01-quiesce",
			Stream: stream,
			Kind:   string(workflow.LogOutput),
			At:     time.Date(2026, 4, 1, 12, 0, int(seq), 0, time.UTC),
			Text:   "line " + string(rune('a'+i)),
		})
	}

	return out
}

// TestFollowStepLogsResumesOnItsCursorWithNoGapsAndNoDuplicates is the
// requirement, stated as a sequence.
//
// Twelve records, four to a page, with the third read failing: the first
// follow prints some prefix and stops, the resume starts from the cursor
// it was told to resume with, and the two outputs concatenated have to be
// 1..12 exactly once each. A follower that took its cursor from the
// records it PRINTED rather than from the page would still pass this one;
// what it would fail is a run whose later steps are noisy, which is why
// the fake answers with the service's own cursor rule and why the
// assertion below also checks that no cursor was ever asked from twice.
func TestFollowStepLogsResumesOnItsCursorWithNoGapsAndNoDuplicates(t *testing.T) {
	const total = 12
	src := &scriptedStepLogs{records: scriptedRecords(total), pageSize: 4, failOnCall: 3}
	req := service.WorkflowStepLogRequest{RunID: "wfr_1", StepID: "01-quiesce"}

	var out, errOut bytes.Buffer
	cursor, err := followStepLogs(context.Background(), src, req, true, &out, &errOut)
	if !errors.Is(err, errScriptedInterruption) {
		t.Fatalf("the interrupted follow returned err = %v, want the scripted interruption; a follow that swallowed it would exit 0 on a partial read", err)
	}
	if cursor == 0 {
		t.Fatal("the interrupted follow reported cursor 0, so a resume would start again from the beginning and print everything twice")
	}
	// The operator's way back in. Without this line the cursor is a value
	// only the code has, and a resume is a guess.
	if !strings.Contains(errOut.String(), "--cursor") {
		t.Errorf("the interruption did not say how to resume:\n%s", errOut.String())
	}

	firstHalf := decodeFollowedSequences(t, out.String())
	if len(firstHalf) == 0 {
		t.Fatal("the interrupted follow printed nothing at all, so there is no resume to test")
	}

	// The resume: a second follow, from exactly the cursor the first one
	// reported, against a source that is no longer failing.
	src.failOnCall = 0
	resumeReq := req
	resumeReq.After = cursor
	out.Reset()
	if _, err := followStepLogs(context.Background(), src, resumeReq, true, &out, &errOut); err != nil {
		t.Fatalf("the resumed follow failed: %v", err)
	}
	secondHalf := decodeFollowedSequences(t, out.String())

	got := append(append([]uint64{}, firstHalf...), secondHalf...)
	if len(got) != total {
		t.Fatalf("the two reads printed %d record(s) (%v), want exactly %d once each", len(got), got, total)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("the printed sequence is %v, want 1..%d in order once each: position %d is %d", got, total, i, seq)
		}
	}

	for i := 1; i < len(src.seenAfter); i++ {
		if src.seenAfter[i] < src.seenAfter[i-1] {
			t.Errorf("the follower asked from cursor %d after asking from %d, so it re-read a window it had already printed: %v",
				src.seenAfter[i], src.seenAfter[i-1], src.seenAfter)
		}
	}
}

// TestFollowStepLogsStopsWhenTheStepIsComplete pins the only thing that
// ends a follow other than an error or a signal.
//
// Complete is what distinguishes "nothing new yet" from "there will never
// be anything new" (service.WorkflowStepLogPage.Complete's own doc), and
// a loop that ignored it would tail a step that finished hours ago
// forever, five seconds at a time.
func TestFollowStepLogsStopsWhenTheStepIsComplete(t *testing.T) {
	src := &scriptedStepLogs{records: scriptedRecords(3), pageSize: 10}

	var out, errOut bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := followStepLogs(context.Background(), src, service.WorkflowStepLogRequest{RunID: "wfr_1", StepID: "01-quiesce"}, false, &out, &errOut); err != nil {
			t.Errorf("followStepLogs = %v, want nil", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the follow did not return on a complete page, so it would tail a finished step forever")
	}

	if !strings.Contains(out.String(), "cursor: 3") {
		t.Errorf("the finished follow did not print the cursor it reached:\n%s", out.String())
	}
}

// TestFollowStepLogsReturnsOnCancellation is the Ctrl-C case: a follow an
// operator ended did what it was asked, so it is not a failure and must
// not become a non-zero exit.
func TestFollowStepLogsReturnsOnCancellation(t *testing.T) {
	// A source that never completes, which is a step still running.
	src := &scriptedStepLogs{records: scriptedRecords(1), pageSize: 1}
	src.records[0].Seq = 1

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out, errOut bytes.Buffer
	if _, err := followStepLogs(ctx, src, service.WorkflowStepLogRequest{RunID: "wfr_1", StepID: "01-quiesce"}, false, &out, &errOut); err != nil {
		t.Fatalf("a cancelled follow returned %v, want nil: an operator's Ctrl-C is not a failure", err)
	}
}

// TestStepLogPrintingKeepsTheStreamsApartAndMarksATruncationAsOurs is
// about the two things the plain form has to say about each record.
//
// stdout and stderr are kept apart end to end because a hook that writes
// progress to one and errors to the other is telling an operator
// something, and a merged log throws it away. The truncation marker is
// this product's own sentence rather than the hook's, so a line that
// looked like the rest would attribute our words to somebody's script --
// and the operator reading a log that stops mid-sentence is exactly the
// one who needs to know which of the two happened.
func TestStepLogPrintingKeepsTheStreamsApartAndMarksATruncationAsOurs(t *testing.T) {
	page := service.WorkflowStepLogPage{
		RunID:  "wfr_1",
		StepID: "01-quiesce",
		Records: []service.WorkflowStepLogRecord{
			{Seq: 1, Stream: workflow.LogStreamStdout, Kind: string(workflow.LogOutput), Text: "pausing the database"},
			{Seq: 2, Stream: workflow.LogStreamStderr, Kind: string(workflow.LogOutput), Text: "warning: replica lag 4s"},
			{Seq: 3, Stream: workflow.LogStreamStdout, Kind: string(workflow.LogTruncated), Text: "output recording stopped at the configured bound"},
		},
		Cursor:    3,
		Truncated: true,
		StepState: string(workflow.StateSuccess),
	}

	var out bytes.Buffer
	printStepLogPage(&out, page, false)
	printStepLogFooter(&out, page)
	printed := out.String()

	lines := strings.Split(strings.TrimSpace(printed), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected a line per record plus a footer:\n%s", printed)
	}
	if !strings.Contains(lines[0], "stdout") || !strings.Contains(lines[0], "pausing the database") {
		t.Errorf("the stdout record does not name its stream:\n%s", lines[0])
	}
	if !strings.Contains(lines[1], "stderr") || !strings.Contains(lines[1], "replica lag") {
		t.Errorf("the stderr record does not name its stream:\n%s", lines[1])
	}
	if strings.Contains(lines[2], "stdout") {
		t.Errorf("the truncation marker was rendered as though the hook had printed it on stdout:\n%s", lines[2])
	}
	if !strings.Contains(lines[2], string(workflow.LogTruncated)) {
		t.Errorf("the truncation marker does not say what it is:\n%s", lines[2])
	}
	if !strings.Contains(printed, "not recorded in full") {
		t.Errorf("a truncated log did not say so in the footer, so an operator would read a partial log as a whole one:\n%s", printed)
	}
}

// decodeFollowedSequences reads the sequence of every record the --json
// form printed, one object per line.
//
// The JSON form is what this test reads rather than the column layout,
// because the property being asserted is about WHICH records were
// printed and in what order, and a test that parsed a formatted line
// would fail the next time somebody widens a column.
func decodeFollowedSequences(t *testing.T, out string) []uint64 {
	t.Helper()
	var seqs []uint64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec service.WorkflowStepLogRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a --json follow printed a line that is not one record: %v\n%s", err, line)
		}
		seqs = append(seqs, rec.Seq)
	}

	return seqs
}
