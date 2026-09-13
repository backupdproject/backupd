package workflow

import (
	"errors"
	"strings"
	"testing"
)

// The naming rule's own suite. It is a table because the rule is a table:
// every row here is a filename somebody will really put in a hook
// directory, and the assertion is not only that it is refused but that the
// refusal is one they can act on.
//
// The "actionable" half is tested rather than trusted, because a refusal
// that does not name the file is a refusal an operator cannot use, and
// that regression is invisible to a test that only checks for an error.

func TestParseScriptNameAcceptsTheDocumentedShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want Target
	}{
		{"quiesce.local.sh", TargetLocal},
		{"quiesce.remote.sh", TargetRemote},
		{"10-dump.local.sh", TargetLocal},
		{"A.remote.sh", TargetRemote},
		{"0.local.sh", TargetLocal},
		{"snap_shot.pre.local.sh", TargetLocal},
		{"a-b_c.d.remote.sh", TargetRemote},
		// A name whose NAME part itself ends in "local" or "remote" is
		// unambiguous and legal: the target is the last two suffixes.
		{"local.local.sh", TargetLocal},
		{"remote.local.sh", TargetLocal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseScriptName(tc.name)
			if err != nil {
				t.Fatalf("ParseScriptName(%q): %v, want target %s", tc.name, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("ParseScriptName(%q) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// The refusal table. Each row names what an operator did, and `mustSay` is
// the phrase the message has to contain for the refusal to be usable: not
// the whole sentence (which is free to be reworded), but the fact without
// which the operator is left guessing.
func TestParseScriptNameRefusesEveryUnsafeName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		name    string
		mustSay string
	}{
		{"empty", "", "must not be empty"},
		{"a plain .sh says nothing about where it runs", "backup.sh", "does not say where it runs"},
		{"a plain .sh is told both names to rename it to", "backup.sh", "backup.local.sh"},
		{"a bare name", "backup", "must be named"},
		{"the wrong extension", "backup.local.bash", "must be named"},
		{"a path, not a name", "sub/backup.local.sh", "path separator"},
		{"a windows path", `sub\backup.local.sh`, "path separator"},
		{"the current directory", ".", "names a directory"},
		{"the parent directory", "..", "names a directory"},
		{"a leading dot", ".backup.local.sh", "begins with a dot"},
		{"an editor swap file", ".backup.local.sh.swp", "begins with a dot"},
		{"a space", "back up.local.sh", "whitespace"},
		{"a leading space", " backup.local.sh", "whitespace"},
		{"a tab", "back\tup.local.sh", "whitespace"},
		{"a newline, which forges an audit line", "backup\n.local.sh", "whitespace"},
		{"a bare control character", "backup\x01.local.sh", "control character"},
		{"a delete character", "backup\x7f.local.sh", "control character"},
		{"a NUL", "backup\x00.local.sh", "NUL"},
		{"a Cyrillic e that reads as an ASCII one", "qui\u0435sce.local.sh", "non-ASCII"},
		{"a full-width suffix", "backup.local\uff0esh", "non-ASCII"},
		{"invalid UTF-8", "backup\xffx.local.sh", "not valid UTF-8"},
		{"a dash first", "-backup.local.sh", "must be named"},
		{"a target this product does not have", "backup.both.sh", "does not say where it runs"},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			target, err := ParseScriptName(tc.name)
			if err == nil {
				t.Fatalf("ParseScriptName(%q) = %s with no error; this name must be refused", tc.name, target)
			}
			if !errors.Is(err, ErrScriptName) {
				t.Errorf("ParseScriptName(%q) returned %v, which is not an ErrScriptName; the layers above classify the whole class by that sentinel", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("ParseScriptName(%q) said:\n\t%v\nand an operator reading that cannot act on it: it has to contain %q",
					tc.name, err, tc.mustSay)
			}
		})
	}
}

// The ordering rule, pinned against the exact sequence `LC_ALL=C ls`
// produces, because that reproducibility on the operator's own machine is
// the whole reason the rule is bytewise rather than anything cleverer.
//
// The mixed-target rows are the ones that matter. A rule that sorted by
// the NAME part with the target as a tiebreak would put 10-dump.local.sh
// and 10-dump.remote.sh adjacent and separate 10-dump.local.sh from
// 20-sync.local.sh; this order is neither, and it is the one an operator
// can see in a directory listing.
func TestScriptOrderIsBytewiseOverTheWholeBasename(t *testing.T) {
	t.Parallel()

	// Deliberately jumbled, and deliberately containing the tricky
	// neighbourhoods: digits against letters, and '-' (0x2D) against '.'
	// (0x2E) against '_' (0x5F).
	//
	// No two names here differ only by case. That is not an oversight
	// about the ordering rule -- which is bytewise and therefore puts
	// every uppercase letter before every lowercase one -- it is because
	// this fixture has to be creatable on a case-INSENSITIVE filesystem,
	// and macOS's default is one. "A.local.sh" and "a.local.sh" would be
	// one file there, so the case axis is covered by names that differ in
	// more than case ("Z.remote.sh" against "a-b.local.sh").
	input := []string{
		"b.local.sh",
		"10-dump.remote.sh",
		"10-dump.local.sh",
		"2-early.local.sh",
		"Z.remote.sh",
		"a-b.local.sh",
		"a.b.local.sh",
		"a_b.local.sh",
		"aa.local.sh",
		"10.local.sh",
	}

	// What `LC_ALL=C sort` gives, byte for byte: '1' before '2', every
	// uppercase letter before every lowercase one, and '-' (0x2D) before
	// '.' (0x2E) before '_' (0x5F) before 'a' (0x61).
	want := []string{
		"10-dump.local.sh",
		"10-dump.remote.sh",
		"10.local.sh",
		"2-early.local.sh",
		"Z.remote.sh",
		"a-b.local.sh",
		"a.b.local.sh",
		"a_b.local.sh",
		"aa.local.sh",
		"b.local.sh",
	}

	dir := t.TempDir()
	for _, name := range input {
		writeScript(t, dir, name, "#!/bin/sh\n")
	}

	scripts, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got := make([]string, 0, len(scripts))
	for _, s := range scripts {
		got = append(got, s.Name)
	}

	if len(got) != len(want) {
		t.Fatalf("Discover found %d scripts, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Discover returned\n\t%v\nwant\n\t%v\n(first difference at index %d)", got, want, i)
		}
	}
}

// A directory that exists and holds nothing is a legal configuration:
// "this stage is set up and I have not written the script yet". It is a
// different fact from a stage that was never configured, and only one of
// the two is an error.
func TestDiscoverAcceptsAnEmptyStageDirectory(t *testing.T) {
	t.Parallel()

	scripts, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("an existing, empty hook directory must be zero steps rather than a refusal: %v", err)
	}
	if len(scripts) != 0 {
		t.Errorf("Discover found %d scripts in an empty directory", len(scripts))
	}
}

// Everything in a hook directory is held to the name rule, including the
// files that are obviously not scripts. See Discover's doc for why the
// friendlier alternative is the dangerous one.
func TestDiscoverRefusesAnEntryThatIsNotAScript(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeScript(t, dir, "good.local.sh", "#!/bin/sh\n")
	writeScript(t, dir, "README", "these are the hooks\n")

	if _, err := Discover(dir); err == nil {
		t.Fatal("a hook directory containing a README was accepted; a file that is not a script in a directory of scripts is a mistake this product refuses rather than ignores")
	} else if !errors.Is(err, ErrScriptName) {
		t.Errorf("Discover returned %v, want an ErrScriptName", err)
	}
}
