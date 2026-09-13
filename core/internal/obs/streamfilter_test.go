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

// A needle that ENDS a chunk and whose own prefix is also its suffix is
// the case a filter which holds back before redacting leaks.
//
// "aba" is bordered: its last byte is also its first. A chunk ending in
// the complete needle therefore has a one-byte tail that is a proper
// prefix of the same needle, so a filter that decides the held-back
// amount from the RAW buffer keeps "a", emits "xab" -- and "xab" has
// never been through the redactor, because the complete match it was
// part of was cut in half before the redactor ever saw it. The two
// journal rows then read "xab" and "a", and the value is reassembled by
// anybody looking at two adjacent rows, which is the whole failure
// StreamFilter exists to prevent.
func TestStreamFilterRedactsABorderedNeedleThatEndsAChunk(t *testing.T) {
	t.Parallel()

	f := NewRedactor().WithValues("aba").NewStreamFilter()

	var out bytes.Buffer
	out.Write(f.Filter([]byte("x aba")))
	out.Write(f.Flush())

	got := out.String()
	if strings.Contains(got, "aba") {
		t.Errorf("the whole needle survived: %q", got)
	}
	if strings.Contains(got, "ab") || strings.Contains(got, "ba") {
		t.Errorf("a fragment of the needle survived, so it can be reassembled across two rows: %q", got)
	}
	if got != "x "+redacted {
		t.Errorf("the stream came out as %q, want %q", got, "x "+redacted)
	}
}

// The same leak, arrived at from the other direction: one needle's tail
// is the beginning of ANOTHER needle.
//
// "abc" ends the chunk and "c" is a proper prefix of "cde", so a filter
// computing its holdback before redacting keeps "c" and emits "x ab" --
// two thirds of a complete match, unredacted, because the match was
// split before the redactor ran.
func TestStreamFilterRedactsANeedleWhoseTailBeginsAnotherNeedle(t *testing.T) {
	t.Parallel()

	f := NewRedactor().WithValues("abc", "cde").NewStreamFilter()

	var out bytes.Buffer
	out.Write(f.Filter([]byte("x abc")))
	out.Write(f.Flush())

	got := out.String()
	for _, fragment := range []string{"abc", "ab", "bc"} {
		if strings.Contains(got, fragment) {
			t.Errorf("the fragment %q survived: %q", fragment, got)
		}
	}
	if got != "x "+redacted {
		t.Errorf("the stream came out as %q, want %q", got, "x "+redacted)
	}
}

// The property the two tests above are instances of: WHERE the chunk
// boundaries fall cannot change the output.
//
// For a needle with no self-overlap and surroundings that contain no
// needle, the streamed result at every possible split point is exactly
// what the one-shot Filter produces over the whole string. That is the
// strongest statement available here, and it is stronger than "the
// secret does not appear": a filter that dropped or duplicated the
// surrounding bytes would pass a leak check and fail this.
func TestStreamFilterIsIndependentOfWhereTheChunksSplit(t *testing.T) {
	t.Parallel()

	for _, needle := range []string{"aba", "abc", "s3cr3t", "aaaa", "ab"} {
		r := NewRedactor().WithValues(needle)
		whole := "before " + needle + " after " + needle
		want := r.Filter(whole)

		for split := 0; split <= len(whole); split++ {
			f := r.NewStreamFilter()

			var out bytes.Buffer
			out.Write(f.Filter([]byte(whole[:split])))
			out.Write(f.Filter([]byte(whole[split:])))
			out.Write(f.Flush())

			if got := out.String(); got != want {
				t.Fatalf("needle %q split at %d filtered to\n\t%q\nwant\n\t%q", needle, split, got, want)
			}

			// Flush is idempotent: a second call after the stream has
			// ended releases nothing, so a caller that flushes twice
			// does not duplicate the tail of every hook's output.
			if extra := f.Flush(); len(extra) != 0 {
				t.Fatalf("a second Flush released %q", extra)
			}
		}
	}
}

// Three-way splits, over a needle set where one needle's suffix begins
// another: the case a single split point cannot reach, asserted for the
// property that has to hold whatever the needles overlap -- no fragment
// of a complete occurrence is ever emitted.
func TestStreamFilterNeverEmitsPartOfACompleteNeedle(t *testing.T) {
	t.Parallel()

	const needle = "abc"

	r := NewRedactor().WithValues(needle, "cde", "bcd")
	whole := "log " + needle + " tail"

	for i := 0; i <= len(whole); i++ {
		for j := i; j <= len(whole); j++ {
			f := r.NewStreamFilter()

			var out bytes.Buffer
			out.Write(f.Filter([]byte(whole[:i])))
			out.Write(f.Filter([]byte(whole[i:j])))
			out.Write(f.Filter([]byte(whole[j:])))
			out.Write(f.Flush())

			got := out.String()
			for _, fragment := range []string{needle, "ab", "bc"} {
				if strings.Contains(got, fragment) {
					t.Fatalf("split at %d/%d emitted %q, which contains %q", i, j, got, fragment)
				}
			}
			if !strings.Contains(got, "log ") || !strings.Contains(got, " tail") {
				t.Fatalf("split at %d/%d lost the surrounding output: %q", i, j, got)
			}
		}
	}
}
