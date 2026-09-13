package obs

import (
	"sort"
	"strings"
)

// Redaction over a STREAM, and the run-scoped needles it needs (#811).
//
// # Why Filter is not enough on this path
//
// Everywhere else in this product, redaction acts on a finished line: a
// log message, a journal detail. A hook's output is not a finished line.
// It is whatever a 64 KiB read off a pipe returned, so a value can
// straddle two reads -- "...pas" then "sphrase..." -- and a per-chunk
// Filter matches neither half. Both halves then land in the durable log,
// where anybody reading it reassembles the credential by looking at two
// adjacent rows.
//
// So the filtering has to be stateful, and the state is small: hold back
// the longest suffix of what has been seen that could still turn out to
// be the start of a needle, emit everything before it, and release the
// held-back tail when the stream ends. That is what StreamFilter is.
//
// # Why the held-back amount is what it is
//
// It is the longest suffix of the buffer that is a PROPER PREFIX of some
// needle -- not a fixed "longest needle minus one". The fixed version is
// the obvious implementation and it leaks: with needles of length n, it
// emits everything but the last n-1 bytes, and a needle that STARTS
// inside that emitted region and ends inside the retained one has already
// had its first bytes written out. Asking "could what I am about to emit
// be the beginning of a needle" is the question that actually has to be
// answered, and the answer is bounded by the longest needle anyway.
//
// # Why the run's secrets are needles here and nowhere else
//
// obs.Secret stops this product rendering a credential it holds. It can
// do nothing about a hook that prints its own: `set -x` around a psql
// invocation puts the password on stderr, and that stderr is something
// this product is about to write into its journal and stream to a browser.
// The step's resolved environment is the one place the material is known
// (workflow.Resolved.SecretValues), so a run-scoped Redactor is built
// from it, used for the duration of the step, and dropped.

// WithValues returns a Redactor that redacts everything this one does,
// plus each of values as a literal needle.
//
// A new Redactor rather than a mutation, for two reasons. The deployment's
// endpoint redactor is shared by every logger and the journal, so adding a
// run's secrets to it in place would leak one step's material into the
// needle set of everything else -- harmless to output, but it would keep
// the value alive in memory for the life of the process. And needles are
// sorted longest-first (see NewRedactor), which is a property of the whole
// slice rather than of each entry.
//
// It is nil-receiver-safe, so NewRedactor(...)  returning nil for a
// deployment with nothing marked sensitive composes without a branch at
// the call site.
//
// A value that is empty, or only whitespace, contributes NO needle. A
// zero-length needle matches at every position, which would replace an
// entire hook's output with placeholders, and the way that arrives in
// production is an environment variable whose secret file happens to be
// empty.
func (r *Redactor) WithValues(values ...string) *Redactor {
	out := &Redactor{}
	if r != nil {
		out.needles = append(out.needles, r.needles...)
	}

	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		out.addNeedle(v)
	}

	if len(out.needles) == 0 {
		return nil
	}

	sort.Slice(out.needles, func(i, j int) bool { return len(out.needles[i]) > len(out.needles[j]) })

	return out
}

// StreamFilter redacts a byte stream arriving in arbitrary pieces.
//
// It is NOT safe for concurrent use, and it is not meant to be: one
// filter belongs to one stream of one step, and the two streams of a hook
// get one each. Sharing one between stdout and stderr would interleave
// their held-back tails, which is both a correctness bug and a merge of
// two streams this product keeps separate on purpose.
type StreamFilter struct {
	redactor *Redactor

	// pending is the held-back tail: the suffix of everything seen so
	// far that could still be the beginning of a needle. It is bounded
	// by the longest needle, so it cannot grow with the output.
	pending []byte

	// longest is the length of the longest needle, which is the bound on
	// pending and the window this filter has to look back over.
	longest int
}

// NewStreamFilter returns a filter for one stream. A nil Redactor
// produces a filter that copies its input through unchanged and holds
// nothing back, which is the same "costs nothing" guarantee Filter makes.
func (r *Redactor) NewStreamFilter() *StreamFilter {
	f := &StreamFilter{}
	if r == nil || len(r.needles) == 0 {
		return f
	}

	f.redactor = r
	for _, n := range r.needles {
		if len(n) > f.longest {
			f.longest = len(n)
		}
	}

	return f
}

// Filter returns the redacted bytes that are safe to emit now.
//
// "Safe" means no later byte can change them: anything that might still
// become part of a needle is retained until it either completes one or is
// released by Flush. The returned slice is freshly allocated and the
// caller owns it, because the callers on this path persist it and hand it
// to subscribers -- both of which outlive the read buffer it came from.
func (f *StreamFilter) Filter(p []byte) []byte {
	if f.redactor == nil {
		out := make([]byte, len(p))
		copy(out, p)

		return out
	}
	if len(p) == 0 {
		return nil
	}

	buf := p
	if len(f.pending) > 0 {
		buf = make([]byte, 0, len(f.pending)+len(p))
		buf = append(buf, f.pending...)
		buf = append(buf, p...)
	}

	keep := f.holdback(buf)

	emit := buf[:len(buf)-keep]
	f.pending = append(f.pending[:0:0], buf[len(buf)-keep:]...)

	if len(emit) == 0 {
		return nil
	}

	return []byte(f.redactor.Filter(string(emit)))
}

// Flush releases whatever is still held back, redacted, and empties the
// filter. It is called when the stream ends, and forgetting it would mean
// the last few bytes of every hook's output silently disappearing.
func (f *StreamFilter) Flush() []byte {
	if len(f.pending) == 0 {
		return nil
	}

	out := []byte(f.redactor.Filter(string(f.pending)))
	f.pending = nil

	return out
}

// holdback returns how many bytes of buf's tail must be retained: the
// length of the longest suffix of buf that is a proper prefix of some
// needle.
//
// The cost is bounded by the longest needle times the number of needles,
// per chunk -- a few hundred byte comparisons against a 64 KiB read --
// and it is checked longest-first so the answer is the largest suffix
// that is still ambiguous.
func (f *StreamFilter) holdback(buf []byte) int {
	max := f.longest - 1
	if max > len(buf) {
		max = len(buf)
	}

	for k := max; k > 0; k-- {
		suffix := buf[len(buf)-k:]
		for _, n := range f.redactor.needles {
			if len(n) > k && strings.HasPrefix(n, string(suffix)) {
				return k
			}
		}
	}

	return 0
}
