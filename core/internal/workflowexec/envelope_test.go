package workflowexec

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

// --- the capture streams --------------------------------------------------

type recordingSink struct {
	mu     sync.Mutex
	chunks []Chunk
	fail   error
}

func (r *recordingSink) Chunk(c Chunk) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.chunks = append(r.chunks, Chunk{Stream: c.Stream, Seq: c.Seq, Data: append([]byte(nil), c.Data...)})
	return nil
}

func (r *recordingSink) all() []Chunk {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Chunk(nil), r.chunks...)
}

// TestCaptureKeepsTheStreamsApartAndOrdersThemTogether is the log-layer
// contract: stdout and stderr stay separate logical streams, and the
// sequence numbers come from ONE counter so the interleaving is
// recoverable. Two independent counters would make "which came first"
// unanswerable, which is most of what an operator reads a hook's output to
// find out.
func TestCaptureKeepsTheStreamsApartAndOrdersThemTogether(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	capture := NewCapture(sink)

	mustWrite(t, capture, StreamStdout, "out-1")
	mustWrite(t, capture, StreamStderr, "err-1")
	mustWrite(t, capture, StreamStdout, "out-2")

	got := sink.all()
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(got), got)
	}

	want := []struct {
		stream StreamID
		seq    uint64
		data   string
	}{
		{StreamStdout, 1, "out-1"},
		{StreamStderr, 2, "err-1"},
		{StreamStdout, 3, "out-2"},
	}
	for i, w := range want {
		if got[i].Stream != w.stream || got[i].Seq != w.seq || string(got[i].Data) != w.data {
			t.Errorf("chunk %d = {%s %d %q}, want {%s %d %q}",
				i, got[i].Stream, got[i].Seq, got[i].Data, w.stream, w.seq, w.data)
		}
	}
}

func mustWrite(t *testing.T, c *Capture, stream StreamID, s string) {
	t.Helper()
	if _, err := c.Writer(stream).Write([]byte(s)); err != nil {
		t.Fatalf("write %s: %v", stream, err)
	}
}

// TestCaptureCopiesWhatItIsHanded is not plumbing. io.Copy reuses one
// buffer for every read, so a sink that kept the slice it was given would
// watch every earlier chunk change under it on the next read: a log that
// rewrites its own history.
func TestCaptureCopiesWhatItIsHanded(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	capture := NewCapture(sink)

	buf := []byte("first")
	if _, err := capture.Writer(StreamStdout).Write(buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	copy(buf, "SECON")

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	if string(got[0].Data) != "first" {
		t.Errorf("the sink's chunk changed under it: %q", got[0].Data)
	}
}

// TestCaptureUnderConcurrentStreamsIssuesEverySequenceOnce is what one
// counter has to survive: the two streams are copied by two goroutines, and
// a sequence number handed out twice is two log lines claiming the same
// position.
func TestCaptureUnderConcurrentStreamsIssuesEverySequenceOnce(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	capture := NewCapture(sink)

	const each = 200
	var wg sync.WaitGroup
	for _, stream := range []StreamID{StreamStdout, StreamStderr} {
		wg.Add(1)
		go func(s StreamID) {
			defer wg.Done()
			w := capture.Writer(s)
			for range each {
				if _, err := w.Write([]byte("x")); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(stream)
	}
	wg.Wait()

	got := sink.all()
	if len(got) != 2*each {
		t.Fatalf("got %d chunks, want %d", len(got), 2*each)
	}
	seen := map[uint64]bool{}
	for _, c := range got {
		if seen[c.Seq] {
			t.Fatalf("sequence %d was issued twice", c.Seq)
		}
		seen[c.Seq] = true
	}
	for i := uint64(1); i <= uint64(2*each); i++ {
		if !seen[i] {
			t.Errorf("sequence %d was never issued", i)
		}
	}
}

// TestCaptureReportsASinkFailureToTheWriter matters because the writer is
// handed to an io.Copy: a sink error that did not come back as a write error
// would be a log that quietly stopped recording while the step ran to
// completion and was reported as captured.
func TestCaptureReportsASinkFailureToTheWriter(t *testing.T) {
	t.Parallel()

	boom := errors.New("sink is gone")
	capture := NewCapture(&recordingSink{fail: boom})
	if _, err := capture.Writer(StreamStdout).Write([]byte("x")); !errors.Is(err, boom) {
		t.Fatalf("write error = %v, want %v", err, boom)
	}
}

func TestCaptureRefusesAnUnknownStream(t *testing.T) {
	t.Parallel()

	capture := NewCapture(&recordingSink{})
	if _, err := capture.Writer(StreamID(9)).Write([]byte("x")); err == nil {
		t.Fatal("an unknown stream id was accepted; a chunk nobody can attribute is worse than a refusal")
	}
}

// --- the environment ------------------------------------------------------

func TestSanitizeBaselineDropsTheFourVariablesThatRunCodeAtStartup(t *testing.T) {
	t.Parallel()

	parent := []string{
		"PATH=/usr/bin",
		"BASH_ENV=/tmp/evil.sh",
		"ENV=/tmp/evil.sh",
		"SHELLOPTS=xtrace:errexit",
		"BASHOPTS=expand_aliases",
		"HOME=/root",
	}
	got := SanitizeBaseline(parent)

	for _, entry := range got {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS":
			t.Errorf("%s survived sanitization; it changes what bash does before the first line of a hook runs", name)
		}
	}
	if len(got) != 2 {
		t.Errorf("sanitized baseline = %q, want PATH and HOME kept", got)
	}
}

func TestProcessEnvAppliesLayersInOrderAndKeepsTheLastWord(t *testing.T) {
	t.Parallel()

	got, err := ProcessEnv(
		[]string{"PATH=/bin", "BASH_ENV=/tmp/x"},
		[]string{"PATH=/opt/bin", "PGDATABASE=orders", "BACKUPD_RUN_ID=r1"},
	)
	if err != nil {
		t.Fatalf("ProcessEnv: %v", err)
	}
	want := []string{"PATH=/opt/bin", "PGDATABASE=orders", "BACKUPD_RUN_ID=r1"}
	if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
		t.Errorf("ProcessEnv = %q, want %q", got, want)
	}
}

// TestNeitherEncodingCarriesAStartupVariableFromTheOperatorLayer is the
// half SanitizeBaseline cannot cover. The baseline is this product's own,
// and by the time an environment reaches either encoding the layers have
// already been merged, so a BASH_ENV an operator wrote in
// workflows.environment arrives as an ordinary resolved entry -- and on the
// remote path it would be exported AFTER the payload's unset line, which
// is the one position where it still redirects every child shell the hook
// starts.
func TestNeitherEncodingCarriesAStartupVariableFromTheOperatorLayer(t *testing.T) {
	t.Parallel()

	environ := []string{
		"PGDATABASE=orders",
		"BASH_ENV=/tmp/evil.sh",
		"ENV=/tmp/evil.sh",
		"SHELLOPTS=xtrace",
		"BASHOPTS=expand_aliases",
	}

	block, err := ProcessEnv([]string{"PATH=/bin"}, environ)
	if err != nil {
		t.Fatalf("ProcessEnv: %v", err)
	}
	for _, entry := range block {
		name, _, _ := strings.Cut(entry, "=")
		if isStartupVariable(name) {
			t.Errorf("the process environment carries %s from the operator layer: %q", name, block)
		}
	}

	payload, err := StdinPayload(environ, []byte("printf hi\n"))
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}
	assignments, _ := parsePayload(t, string(payload))
	for name := range assignments {
		if isStartupVariable(name) {
			t.Errorf("the payload exports %s, and it does so after the line that unsets it", name)
		}
	}
	if assignments["PGDATABASE"] != "orders" {
		t.Errorf("the ordinary variable beside them was lost: %v", assignments)
	}
}

func TestEnvRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		entry string
		says  string
	}{
		{"a NUL in the value", "FOO=a\x00b", "NUL"},
		{"a NUL in the name", "FO\x00O=bar", "NUL"},
		{"no equals sign at all", "FOO", "NAME=VALUE"},
		{"an empty name", "=bar", "not a usable"},
		{"a name with a dash", "FOO-BAR=x", "not a usable"},
		{"a name starting with a digit", "1FOO=x", "not a usable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ProcessEnv(nil, []string{c.entry}); err == nil {
				t.Fatalf("%q was accepted", c.entry)
			} else if !errors.Is(err, ErrEnvEncoding) {
				t.Errorf("error %v is not an ErrEnvEncoding, so a caller cannot tell an encoding refusal from a transport failure", err)
			} else if !strings.Contains(err.Error(), c.says) {
				t.Errorf("error %q does not say %q", err, c.says)
			}

			// The stdin path must refuse exactly what the process-env
			// path refuses. Two encoders with two answers is one host
			// accepting what another rejects.
			if _, err := StdinPayload([]string{c.entry}, []byte("true\n")); err == nil {
				t.Errorf("StdinPayload accepted %q that ProcessEnv refused", c.entry)
			}
		})
	}
}

func TestEnvAcceptsNewlinesAndUTF8(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"line one\nline two\n",
		"ünïcødé ✓",
		"tab\there",
		"$(touch /tmp/x)`id`",
		`single ' and "double"`,
		"",
	} {
		if _, err := ProcessEnv(nil, []string{"V=" + value}); err != nil {
			t.Errorf("ProcessEnv refused %q: %v", value, err)
		}
		if _, err := StdinPayload([]string{"V=" + value}, []byte("true\n")); err != nil {
			t.Errorf("StdinPayload refused %q: %v", value, err)
		}
	}
}

// --- the stdin payload ----------------------------------------------------

// TestStdinPayloadNeverLetsAValueBecomeSyntax is the injection contract,
// and it is asserted by PARSING the payload back rather than by looking
// for substrings in it.
//
// Substring assertions cannot express this property. A value containing
// "export EVIL=yes" is perfectly safe inside a single-quoted literal and
// perfectly fatal outside one, and the text contains it either way -- so
// "the payload does not contain export EVIL" fails on a correct payload,
// while "the payload contains the quoted line" passes on a payload that
// also contains a second, unquoted copy. What actually matters is that the
// payload, read the way a shell reads it, is EXACTLY the assignments this
// product intended and nothing else. So the test reads it that way: any
// value that escaped its literal shifts the parse and is caught.
func TestStdinPayloadNeverLetsAValueBecomeSyntax(t *testing.T) {
	t.Parallel()

	hostile := map[string]string{
		"V1": "$(touch /tmp/pwned)",
		"V2": "`touch /tmp/pwned`",
		"V3": "'; touch /tmp/pwned; '",
		"V4": "x' && touch /tmp/pwned && echo '",
		"V5": "$V1",
		"V6": `\'`,
		"V7": "a\nexport EVIL=yes\n",
		"V8": "';\ntouch /tmp/pwned\n'",
	}
	var environ []string
	for name, value := range hostile {
		environ = append(environ, name+"="+value)
	}
	script := []byte("printf done\n# a comment with ' in it\n")

	payload, err := StdinPayload(environ, script)
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}

	assignments, gotScript := parsePayload(t, string(payload))

	// The baseline's own PATH is exported alongside them, and it is the
	// only entry the caller did not supply.
	if len(assignments) != len(hostile)+1 {
		t.Errorf("the payload decodes to %d assignments, want %d: %v", len(assignments), len(hostile)+1, assignments)
	}
	if assignments["PATH"] == "" {
		t.Error("the payload exports no PATH, so a hook on a host that exported nothing has no way to find a command")
	}
	for name, want := range hostile {
		got, ok := assignments[name]
		if !ok {
			t.Errorf("%s is missing from the decoded payload", name)

			continue
		}
		if got != want {
			t.Errorf("%s decoded to %q, want %q", name, got, want)
		}
	}
	if gotScript != string(script) {
		t.Errorf("the script decoded to %q, want %q", gotScript, script)
	}
}

// parsePayload reads a payload the way a POSIX shell reads single-quoted
// text, and refuses anything that is not exactly the envelope's own
// grammar: the clearing prologue, then `export NAME='...'` lines, then the
// script assignment, then the eval line, then end of input.
//
// It is strict on purpose, and the prologue is matched as a WHOLE prefix
// rather than searched for: an export that appeared before it, or between
// its lines, would be an export the loop then removes again, and the only
// way to state "nothing is exported until everything inherited is gone" is
// to insist the clearing is the first thing in the payload.
func parsePayload(t *testing.T, text string) (map[string]string, string) {
	t.Helper()

	rest, ok := strings.CutPrefix(text, clearInheritedEnvironment)
	if !ok {
		t.Fatalf("the payload does not begin by clearing every inherited variable: %q", firstLine(text))
	}

	assignments := map[string]string{}
	var script string
	haveScript := false

	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "export "):
			line := rest[len("export "):]
			name, after, found := strings.Cut(line, "=")
			if !found {
				t.Fatalf("an export with no assignment in it: %q", firstLine(rest))
			}
			value, tail := cutQuoted(t, after)
			if _, already := assignments[name]; already {
				t.Fatalf("%s is assigned twice", name)
			}
			assignments[name] = value
			rest = tail
		case strings.HasPrefix(rest, "__backupd_script="):
			value, tail := cutQuoted(t, rest[len("__backupd_script="):])
			script, haveScript = value, true
			rest = tail
		case rest == "eval \"$__backupd_script\" 0</dev/null\n":
			rest = ""
		default:
			t.Fatalf("the payload carries a command the envelope never emits, which is what a value escaping its literal looks like: %q", firstLine(rest))
		}
	}
	if !haveScript {
		t.Fatal("the payload carries no script assignment")
	}

	return assignments, script
}

// cutQuoted consumes one single-quoted shell literal from the front of s,
// applying the only escaping single quotes have, and returns the value it
// denotes plus whatever follows the terminating newline.
func cutQuoted(t *testing.T, s string) (string, string) {
	t.Helper()

	if !strings.HasPrefix(s, "'") {
		t.Fatalf("a value that is not a single-quoted literal: %q", firstLine(s))
	}
	s = s[1:]

	var value strings.Builder
	for {
		before, after, found := strings.Cut(s, "'")
		if !found {
			t.Fatalf("an unterminated single-quoted literal: %q", firstLine(s))
		}
		value.WriteString(before)
		if strings.HasPrefix(after, `\''`) {
			value.WriteString("'")
			s = after[len(`\''`):]

			continue
		}
		// The literal closed. What follows must be exactly a newline.
		tail, ok := strings.CutPrefix(after, "\n")
		if !ok {
			t.Fatalf("a closing quote followed by something other than a line break: %q", firstLine(after))
		}

		return value.String(), tail
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}

// TestStdinPayloadClearsEveryInheritedVariableBeforeItExportsAnything is
// the environment-parity contract: whatever the remote account, its login
// shell and sshd exported is gone before the first export, so a hook sees
// the baseline plus the plan and nothing else.
//
// The order is the assertion, not the presence of the clearing text. A
// payload that exported the plan and THEN cleared would pass a
// "does it unset" check while handing the hook an empty environment, and a
// payload that cleared only the four startup names would pass one while
// leaving SSH_CONNECTION, HOME and LANG in place.
func TestStdinPayloadClearsEveryInheritedVariableBeforeItExportsAnything(t *testing.T) {
	t.Parallel()

	payload, err := StdinPayload([]string{"FOO=bar"}, []byte("printf hi\n"))
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}
	text := string(payload)

	clearEnd := strings.Index(text, "unset -v __backupd_name")
	if clearEnd < 0 {
		t.Fatalf("the payload has no clearing prologue at all:\n%s", text)
	}
	if i := strings.Index(text, "export "); i >= 0 && i < clearEnd {
		t.Errorf("the payload exports %q before it has finished clearing what it inherited, so that export is removed again by the loop", firstLine(text[i:]))
	}
	if !strings.Contains(text[:clearEnd], "unset BASH_ENV ENV SHELLOPTS BASHOPTS") {
		t.Error("the payload does not clear the variables that make bash run code before the hook does")
	}
	if !strings.Contains(text[:clearEnd], "compgen -e") {
		t.Error("the payload never enumerates the inherited exported variables, so it can only be clearing the names it already knew about")
	}
	for _, injected := range []string{"set -e", "set -u", "set -o pipefail", "set -x", "set -o nounset", "set -o errexit"} {
		if strings.Contains(text, injected) {
			t.Errorf("the payload injects %q; a hook's bytes run exactly as captured, and a shell option this product added silently changes what a script means", injected)
		}
	}
}

func TestStdinPayloadCarriesTheScriptAndDetachesTheHooksStdin(t *testing.T) {
	t.Parallel()

	script := []byte("#!/bin/bash\nprintf 'it'\\''s here\\n'\nexit 3\n")
	payload, err := StdinPayload(nil, script)
	if err != nil {
		t.Fatalf("StdinPayload: %v", err)
	}
	if !bytes.Contains(payload, []byte(ShellQuote(string(script)))) {
		t.Error("the payload does not carry the captured bytes as a single-quoted literal")
	}
	if !bytes.Contains(payload, []byte("0</dev/null")) {
		t.Error("the payload does not detach the hook's stdin; bash reads its own script from stdin, so a hook that read it would eat the rest of itself")
	}
}

func TestStdinPayloadRefusesANULInTheScript(t *testing.T) {
	t.Parallel()

	_, err := StdinPayload(nil, []byte("printf a\x00b\n"))
	if err == nil {
		t.Fatal("a script with a NUL byte was accepted; a shell word cannot contain one, so it would be silently dropped")
	}
	if !errors.Is(err, ErrScriptEncoding) {
		t.Errorf("error %v is not an ErrScriptEncoding", err)
	}
}

func TestShellQuoteRoundTripsEveryByteButNUL(t *testing.T) {
	t.Parallel()

	var b []byte
	for i := 1; i < 256; i++ {
		b = append(b, byte(i))
	}
	quoted := ShellQuote(string(b))
	if !strings.HasPrefix(quoted, "'") || !strings.HasSuffix(quoted, "'") {
		t.Fatalf("quoted form is not a single-quoted literal: %q", quoted)
	}
	// Undo it by the only escaping single quotes have.
	if unquoted := strings.ReplaceAll(quoted[1:len(quoted)-1], `'\''`, "'"); unquoted != string(b) {
		t.Error("the round trip lost bytes")
	}
}

// --- termination certainty ------------------------------------------------

func TestTerminationCertaintyVocabulary(t *testing.T) {
	t.Parallel()

	for _, valid := range []TerminationCertainty{TerminationConfirmed, TerminationUnconfirmed, TerminationNotRequested} {
		if !valid.Valid() {
			t.Errorf("%q is not accepted", valid)
		}
	}
	if TerminationCertainty("probably").Valid() {
		t.Error("an invented certainty was accepted; there are two answers and a not-asked, and \"probably\" is the one thing this may never say")
	}
	if TerminationConfirmed == TerminationUnconfirmed {
		t.Error("the two answers are the same value")
	}
}
