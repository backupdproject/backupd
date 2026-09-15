package envcompat

import (
	"strings"
	"testing"
)

// The pairs under test are the REAL ones -- RETND_DEBUG/BACKUPD_DEBUG and
// RETND_INCREMENTAL_ENGINE/BACKUPD_INCREMENTAL_ENGINE -- rather than
// invented names, because an invented name under the retired prefix
// would itself be a NEW
// old-brand identifier and scripts/rename/check-brand-drift.sh is right to
// refuse one. These are the two variables this package exists for, they
// are on that guard's alias list with the issue that deletes them, and a
// test naming them goes away with them.

// capture points the notice writer at a buffer and clears the
// already-warned set, so each case starts from a fresh process's
// worth of state.
func capture(t *testing.T) *strings.Builder {
	t.Helper()

	var b strings.Builder

	warnState.mu.Lock()
	previousOut, previousWarned := warnState.out, warnState.warned
	warnState.out, warnState.warned = &b, nil
	warnState.mu.Unlock()

	t.Cleanup(func() {
		warnState.mu.Lock()
		warnState.out, warnState.warned = previousOut, previousWarned
		warnState.mu.Unlock()
	})

	return &b
}

func TestLookup_NewNameWinsWhenBothAreSet(t *testing.T) {
	capture(t)
	t.Setenv("RETND_DEBUG", "new")
	t.Setenv("BACKUPD_DEBUG", "old")

	got, found := Lookup(Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG"}})
	if !found || got != "new" {
		t.Fatalf("Lookup = %q, %v; want the current name's value to win", got, found)
	}
}

func TestLookup_LegacyNameIsHonouredWhenTheNewOneIsUnset(t *testing.T) {
	capture(t)
	t.Setenv("BACKUPD_DEBUG", "old")

	got, found := Lookup(Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG"}})
	if !found || got != "old" {
		t.Fatalf("Lookup = %q, %v; want the deprecated name to still be read", got, found)
	}
}

// An empty value is what a runtime that exports the new name
// unconditionally produces, and it must not shadow the operator's real
// setting under the old one.
func TestLookup_EmptyCurrentValueDoesNotShadowTheLegacyName(t *testing.T) {
	capture(t)
	t.Setenv("RETND_DEBUG", "")
	t.Setenv("BACKUPD_DEBUG", "old")

	got, found := Lookup(Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG"}})
	if !found || got != "old" {
		t.Fatalf("Lookup = %q, %v; want an empty current value to count as unset", got, found)
	}
}

func TestLookup_LegacyPrecedenceFollowsTheDeclaredOrder(t *testing.T) {
	capture(t)
	t.Setenv("RM_DEBUG", "first-brand")
	t.Setenv("BACKUPD_DEBUG", "second-brand")

	got, _ := Lookup(Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG", "RM_DEBUG"}})
	if got != "second-brand" {
		t.Fatalf("Lookup = %q; want the earlier legacy name in the declared order", got)
	}
}

func TestLookup_NothingSetIsNotFound(t *testing.T) {
	capture(t)

	if got, found := Lookup(Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG"}}); found {
		t.Fatalf("Lookup = %q, %v; want not found", got, found)
	}
}

// The property the whole mechanism turns on: a variable read on every
// cycle must not print on every cycle. A notice per read is a log an
// operator silences, which is the same outcome as no notice at all.
func TestLookup_WarnsOncePerNamePerProcessAndNotPerRead(t *testing.T) {
	out := capture(t)
	t.Setenv("BACKUPD_DEBUG", "old")
	t.Setenv("BACKUPD_INCREMENTAL_ENGINE", "old")

	thing := Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG"}}
	other := Rename{Current: "RETND_INCREMENTAL_ENGINE", Legacy: []string{"BACKUPD_INCREMENTAL_ENGINE"}}
	for range 5 {
		Lookup(thing)
		Lookup(other)
	}

	notices := strings.Count(out.String(), "deprecated environment variable:")
	if notices != 2 {
		t.Fatalf("emitted %d notices for 10 reads of 2 names:\n%s\nwant exactly one per name", notices, out)
	}
	if got := WarnedNames(); len(got) != 2 || got[0] != "BACKUPD_DEBUG" || got[1] != "BACKUPD_INCREMENTAL_ENGINE" {
		t.Fatalf("WarnedNames = %v, want both names once", got)
	}
}

// The notice has to be actionable on its own: an operator reading one
// line in a container log needs the name to set, the name to remove and
// the release it stops working in.
func TestLookup_NoticeNamesTheReplacementAndTheRemovalRelease(t *testing.T) {
	out := capture(t)
	t.Setenv("BACKUPD_DEBUG", "old")

	Lookup(Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG"}})

	notice := out.String()
	for _, want := range []string{"BACKUPD_DEBUG", "RETND_DEBUG", RemovalRelease} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not name %q", notice, want)
		}
	}
}

// A legacy name being ignored is still a legacy name to delete, and the
// notice has to say which of the two situations the operator is in.
func TestLookup_NoticeSaysWhenTheLegacyNameIsBeingIgnored(t *testing.T) {
	out := capture(t)
	t.Setenv("RETND_DEBUG", "new")
	t.Setenv("BACKUPD_DEBUG", "old")

	Lookup(Rename{Current: "RETND_DEBUG", Legacy: []string{"BACKUPD_DEBUG"}})

	if notice := out.String(); !strings.Contains(notice, "IGNORED") {
		t.Fatalf("notice %q does not say the deprecated name was ignored", notice)
	}
}
