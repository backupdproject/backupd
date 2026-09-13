package workflowlint

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// The script's own line, beside the finding that names it (#906, review
// MAJOR 2).
//
// # Why the report carries source at all
//
// Because a position without the line it points at is a lookup somebody
// has to perform by hand, on a machine they may not be on. "BSH003 at
// 24:10" sends an operator to a NAS over SSH to read one line; the line
// itself, beside the message, is the whole finding in one place. The
// approved design for this feature shows exactly that -- a gutter, the
// line, and a caret under the reported column -- and the API could not
// carry it.
//
// # Why it comes from the bytes that were verified
//
// The caller passes the source it just read and hashed, which is the only
// source that cannot disagree with the positions in the report. A
// re-read at render time would be a re-read of a file somebody may have
// edited in between, and an excerpt showing the wrong line under the
// right position is worse than no excerpt: it is a report that lies about
// what it examined.
//
// # Why it is inert
//
// A hook script is arbitrary text, and this text is about to be put on an
// HTML page and into a terminal. So every line handed out here is
// stripped of control characters and bounded in length, at the point it
// is produced rather than at each of the surfaces that draw it. A
// sanitiser per surface is a sanitiser one surface will be missing.

// ExcerptContextLines is how many lines either side of the reported one
// travel with a finding.
//
// One. Enough for the case that actually needs context -- a continuation,
// a `then` on the line above, an unclosed quote opened on the line before
// -- and few enough that a script with forty findings does not put its
// whole self into one API response. The design shows one line with its
// neighbours implied; this is that, with the neighbours.
const ExcerptContextLines = 1

// ExcerptMaxLineRunes bounds one line of an excerpt.
//
// A hook can legitimately hold a very long line -- a generated rclone
// invocation, a base64 blob somebody pasted -- and the excerpt is not the
// place to deliver it. Two hundred runes is past the width of any
// terminal or panel this product draws into, so the truncation is
// invisible on a real script and decisive on a pathological one.
const ExcerptMaxLineRunes = 200

// SourceLine is one line of a script, numbered as an editor numbers it.
type SourceLine struct {
	// Number is 1-based, so it is the number in the gutter of whatever
	// the operator opens the file with.
	Number int

	// Text is the line with control characters removed and its length
	// bounded. Truncated says the line was longer than that.
	Text      string
	Truncated bool
}

// Excerpt returns the reported line with ExcerptContextLines either side,
// or nothing when the position names no line of this script.
//
// A position of zero returns nothing rather than the first line: zero is
// what a report carries when there is no position at all, and answering
// it with line 1 would attach an arbitrary line to a finding that never
// named one.
func Excerpt(src []byte, line int) []SourceLine {
	if line <= 0 || len(src) == 0 {
		return nil
	}

	lines := bytes.Split(src, []byte("\n"))

	// A trailing newline produces a final empty element that is not a
	// line of the script; dropping it keeps "the last line" meaning the
	// last line somebody wrote.
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}

	if line > len(lines) {
		return nil
	}

	first := max(line-ExcerptContextLines, 1)
	last := min(line+ExcerptContextLines, len(lines))

	out := make([]SourceLine, 0, last-first+1)
	for n := first; n <= last; n++ {
		text, truncated := inertLine(lines[n-1])
		out = append(out, SourceLine{Number: n, Text: text, Truncated: truncated})
	}

	return out
}

// inertLine strips what must never reach a terminal or a page and bounds
// what is left.
//
// Control characters go, including the carriage return a CRLF file leaves
// at the end of every line and the escape that starts a terminal control
// sequence. A tab becomes a single space rather than being dropped,
// because dropping it would move the reported COLUMN relative to the
// text an operator is looking at -- the one thing an excerpt exists to
// line up.
func inertLine(line []byte) (string, bool) {
	var b strings.Builder
	b.Grow(len(line))

	runes := 0
	truncated := false

	for _, r := range string(line) {
		if runes == ExcerptMaxLineRunes {
			truncated = true

			break
		}

		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// Dropped, and the column stays right: every one of these is
			// one byte and one column in the source, and a replacement
			// character would be the same width in the excerpt.
			b.WriteByte(' ')
		case r == utf8.RuneError:
			b.WriteRune('\ufffd')
		default:
			b.WriteRune(r)
		}
		runes++
	}

	return b.String(), truncated
}
