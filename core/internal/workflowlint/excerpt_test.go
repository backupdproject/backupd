package workflowlint

import (
	"strings"
	"testing"
)

func TestAnExcerptCarriesTheReportedLineAndOneEitherSide(t *testing.T) {
	src := []byte("#!/bin/bash\nset -e\ncd /srv/data\nrm -rf ./old\necho done\n")

	got := Excerpt(src, 3)

	if len(got) != 3 {
		t.Fatalf("excerpt = %+v, want the reported line and one either side", got)
	}
	for i, want := range []SourceLine{
		{Number: 2, Text: "set -e"},
		{Number: 3, Text: "cd /srv/data"},
		{Number: 4, Text: "rm -rf ./old"},
	} {
		if got[i] != want {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestAnExcerptAtTheEdgesDoesNotInventLines(t *testing.T) {
	src := []byte("first\nsecond\n")

	if got := Excerpt(src, 1); len(got) != 2 || got[0].Number != 1 {
		t.Errorf("excerpt of the first line = %+v, want lines 1 and 2", got)
	}
	if got := Excerpt(src, 2); len(got) != 2 || got[len(got)-1].Number != 2 {
		t.Errorf("excerpt of the last line = %+v, want lines 1 and 2", got)
	}
	if got := Excerpt(src, 3); got != nil {
		t.Errorf("excerpt past the end = %+v, want nothing", got)
	}
	if got := Excerpt(src, 0); got != nil {
		t.Errorf("excerpt of position zero = %+v, want nothing: zero is what a report carries when it has no position", got)
	}
}

// The property that makes this safe to put on a page and in a terminal:
// nothing that could be read as a control sequence survives, and the
// COLUMN still lines up with the text, which is the only reason an
// excerpt is worth carrying.
func TestAnExcerptIsInertAndKeepsItsColumns(t *testing.T) {
	src := []byte("echo \x1b]0;pwned\x07 hi\n\tindented \r\n")

	got := Excerpt(src, 1)
	if len(got) == 0 {
		t.Fatal("no excerpt")
	}

	for _, line := range got {
		for _, r := range line.Text {
			if r < 0x20 || r == 0x7f {
				t.Errorf("line %d carries the control character %q, which a terminal would act on: %q", line.Number, r, line.Text)
			}
		}
	}

	if len(got) < 2 {
		t.Fatalf("excerpt = %+v, want the second line too", got)
	}
	if !strings.HasPrefix(got[1].Text, " indented") {
		t.Errorf("line 2 = %q, want the leading tab rendered as one space so the reported column still points at the same character", got[1].Text)
	}
}

func TestALongLineIsBoundedAndSaysSo(t *testing.T) {
	src := []byte(strings.Repeat("x", ExcerptMaxLineRunes+50) + "\n")

	got := Excerpt(src, 1)
	if len(got) != 1 {
		t.Fatalf("excerpt = %+v", got)
	}
	if n := len([]rune(got[0].Text)); n != ExcerptMaxLineRunes {
		t.Errorf("line length = %d runes, want it bounded at %d", n, ExcerptMaxLineRunes)
	}
	if !got[0].Truncated {
		t.Error("a truncated line does not say it was truncated, so a reader cannot tell the script from the excerpt")
	}
}
