package obs

import (
	"bytes"
	"strings"
	"testing"
)

// Redaction over a STREAM rather than over a finished line (#811).
//
// The case this exists for is the one a per-chunk Filter cannot handle: a
// hook's output arrives in 64 KiB reads off a pipe, so a credential can
// straddle two of them. Filter sees "...passw" and then "ord123..." and
// matches neither, and the two halves go into the journal where the whole
// value is trivially reassembled by anybody reading it.

func TestStreamFilterCatchesANeedleSplitAcrossChunks(t *testing.T) {
	t.Parallel()

	r := NewRedactor().WithValues("s3cr3t-passphrase")
	if r == nil {
		t.Fatal("WithValues on an endpoint-less redactor returned nil, so there is nothing to filter with")
	}

	f := r.NewStreamFilter()

	var out bytes.Buffer
	for _, chunk := range []string{"psql: connecting with s3c", "r3t-pas", "sphrase now\n"} {
		out.Write(f.Filter([]byte(chunk)))
	}
	out.Write(f.Flush())

	got := out.String()
	if strings.Contains(got, "s3cr3t-passphrase") {
		t.Errorf("the whole secret survived the stream: %q", got)
	}
	for _, fragment := range []string{"s3c", "r3t-pas", "sphrase"} {
		if strings.Contains(got, fragment) {
			t.Errorf("the fragment %q survived, so the value can be reassembled from the log: %q", fragment, got)
		}
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("nothing was redacted at all: %q", got)
	}
	if !strings.Contains(got, "psql: connecting with ") || !strings.Contains(got, " now\n") {
		t.Errorf("the surrounding output did not survive: %q", got)
	}
}

// Every byte in, every byte out, exactly once and in order: a filter that
// dropped or duplicated a byte would be a log that is not a transcript.
func TestStreamFilterPreservesEverythingItIsNotRedacting(t *testing.T) {
	t.Parallel()

	r := NewRedactor().WithValues("needle")
	f := r.NewStreamFilter()

	// Deliberately not UTF-8, and deliberately containing a prefix of the
	// needle at the very end of a chunk.
	chunks := [][]byte{
		{0x00, 0xff, 'n', 'e', 'e'},
		[]byte("dle and then nee"),
		[]byte("d and nothing\n"),
	}

	var out bytes.Buffer
	for _, c := range chunks {
		out.Write(f.Filter(c))
	}
	out.Write(f.Flush())

	want := append([]byte{0x00, 0xff}, []byte(redacted+" and then need and nothing\n")...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("the filtered stream is\n\t%q\nwant\n\t%q", out.Bytes(), want)
	}
}

// A stream filter holds back at most one needle's worth of bytes, and it
// is the FLUSH that releases them. Without that, the tail of every hook's
// output would be silently missing.
func TestStreamFilterFlushReleasesTheHeldBackTail(t *testing.T) {
	t.Parallel()

	r := NewRedactor().WithValues("abcdef")
	f := r.NewStreamFilter()

	held := f.Filter([]byte("xyzabcd"))
	if strings.Contains(string(held), "abcd") {
		t.Errorf("a prefix of the needle was emitted before it could be completed: %q", held)
	}

	tail := f.Flush()
	if got := string(held) + string(tail); got != "xyzabcd" {
		t.Errorf("the stream came out as %q, want %q", got, "xyzabcd")
	}
}

// A nil Redactor -- a deployment with nothing marked sensitive and a step
// with no secret-backed variables -- costs nothing and changes nothing.
// This is the same "do nothing" guarantee Filter already makes, extended
// to the streaming path, because the streaming path is on every chunk of
// every hook's output.
func TestStreamFilterOnANilRedactorIsAPassThrough(t *testing.T) {
	t.Parallel()

	var r *Redactor
	f := r.NewStreamFilter()

	in := []byte("anything at all\n")
	out := f.Filter(in)

	if !bytes.Equal(out, in) {
		t.Errorf("a nil redactor changed the bytes: %q", out)
	}
	if len(f.Flush()) != 0 {
		t.Error("a nil redactor held bytes back")
	}
}

// WithValues layers a run's own secret material onto the deployment's
// configured endpoints, and both must still be redacted: the endpoints
// are why the Redactor exists, and the secrets are why #811 needs it on
// the log path.
func TestWithValuesKeepsTheEndpointNeedles(t *testing.T) {
	t.Parallel()

	r := NewRedactor(Endpoint{Host: "nas.internal", Port: 2222, User: "backup"}).
		WithValues("hunter2")

	f := r.NewStreamFilter()

	var out bytes.Buffer
	out.Write(f.Filter([]byte("ssh backup@nas.internal:2222 with hun")))
	out.Write(f.Filter([]byte("ter2\n")))
	out.Write(f.Flush())

	got := out.String()
	for _, leaked := range []string{"nas.internal", "hunter2", "backup@"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q survived: %q", leaked, got)
		}
	}
}

// An empty value contributes no needle. A zero-length needle would match
// at every position, which is a filter that replaces the whole stream
// with placeholders -- and the way that arrives is an environment
// variable whose secret resolved to "".
func TestWithValuesIgnoresEmptyValues(t *testing.T) {
	t.Parallel()

	r := NewRedactor().WithValues("", "   ")
	if r != nil {
		t.Fatalf("values that cannot be needles produced a redactor: %+v", r)
	}
}
