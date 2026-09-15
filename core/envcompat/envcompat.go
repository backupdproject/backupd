// Package envcompat reads an environment variable that this product has
// RENAMED, for the one release in which both names work.
//
// # The failure mode it exists for
//
// An input environment variable breaks SILENTLY when its name changes.
// Nothing reports it: the process starts, the variable the operator set
// is not the variable the process reads, and the setting reverts to its
// default. A deployment that was running with the incremental engine
// turned on by BACKUPD_INCREMENTAL_ENGINE=1 comes back up with the engine
// off, and the only evidence is the behaviour the operator turned the
// knob to change. EPIC R (#885) classifies every renamed runtime
// identifier by exactly that question (FR-37), and this is the mechanism
// for the population whose answer is "read both names for one release,
// the new one wins, and say so once".
//
// # Why this is one package and not a helper in each reader
//
// Two readers of the debug shortcut exist on purpose and cannot be
// merged: core/internal/obs.LevelFromEnv serves the engine and
// apps/common/webhost's envLogLevel serves the web host, because apps/
// may import core/ and never the reverse, and core/internal is
// unreachable from apps/ (issue #730 is what happens when the two
// disagree). core/envcompat is reachable from both, which matters for
// exactly one property: the deprecation notice is emitted ONCE PER NAME
// PER PROCESS, and `serve` runs both readers in one process. Two
// independent once-guards would warn twice for one variable, which is
// how a deprecation notice becomes noise an operator filters out.
//
// # Why the notice goes to stderr rather than through obs
//
// Because the first caller of this package is what DECIDES the log level.
// There is no logger yet when LevelFromEnv runs, and a deprecation notice
// about the variable that sets the level must not be suppressible by the
// level it is about to set. os.Stderr is captured by every runtime this
// product ships under -- systemd's journal, `docker logs`, a terminal --
// and is separate from the JSON event stream on stdout, so a notice here
// cannot be mistaken for an event.
package envcompat

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// Rename is one variable in transition: the name this product reads now,
// and the deprecated names it still accepts, in precedence order.
//
// Current is primary everywhere -- it is the name the documentation, the
// compose files and this product's own code spell. A Legacy name is never
// written, never documented as the way to do something, and exists only
// so an in-place upgrade keeps reading the environment the operator
// already has (FR-37).
type Rename struct {
	Current string
	Legacy  []string
}

// RemovalRelease is the window every name in Legacy has left, in the
// words FR-43's shim table uses. It is in the deprecation notice because
// "deprecated" without a date is a state nothing ever leaves.
const RemovalRelease = "the release after the one that renames this product"

// Which returns the NAME that carried the value, the value, and whether
// any name did.
//
// The name is the part a refusal needs. A gate that cannot parse what it
// was handed has to name the variable the operator actually set, not the
// one this release prefers: "BACKUPD_INCREMENTAL_ENGINE=ture is not a
// value this gate understands" is actionable, and the same sentence
// naming RETND_INCREMENTAL_ENGINE sends them looking for a variable that
// is not in their compose file.
//
// Precedence is Current, then each Legacy in order, so the new name wins
// whenever both are set: an operator who has added the new name to a
// compose file without removing the old one gets the value they just
// wrote, not the one they forgot.
//
// An EMPTY value counts as UNSET, for every name. That is the same rule
// apps/common/csrf and apps/common/auth/local apply to a cookie a client
// keeps echoing back after it was cleared, and it is what makes
// precedence useful rather than a trap: a runtime that exports the new
// name unconditionally -- an image's ENV line, a compose file mapping a
// variable to a variable that is itself unset -- would otherwise shadow
// the operator's real setting under the old name with an empty string.
//
// Every Legacy name that carries a value produces one deprecation notice
// per process, whether or not that value is the one returned: a legacy
// name that is being IGNORED because the new one is also set is exactly
// as worth removing from a compose file as one that is being honoured,
// and an operator who removes only the one they were told about is left
// with the other.
func Which(r Rename) (name, value string, found bool) {
	value, found = os.LookupEnv(r.Current)
	found = found && value != ""
	if found {
		name = r.Current
	}

	for _, legacy := range r.Legacy {
		legacyValue, legacySet := os.LookupEnv(legacy)
		if !legacySet || legacyValue == "" {
			continue
		}

		warnOnce(r.Current, legacy, found)

		if !found {
			name, value, found = legacy, legacyValue, true
		}
	}

	return name, value, found
}

// Lookup is Which for a caller that does not report what it read back to
// an operator.
func Lookup(r Rename) (string, bool) {
	_, value, found := Which(r)

	return value, found
}

// Value is Lookup for a caller that has no use for the distinction
// between unset and empty, which is most of them.
func Value(r Rename) string {
	value, _ := Lookup(r)

	return value
}

// Any reports whether ANY name in r carries exactly want, and warns once
// per process for each deprecated name that is set at all.
//
// This is the shape for a knob with no "off" value, of which this
// product has one: the debug shortcut, where only the documented "1"
// means anything under any spelling. Precedence would be the wrong rule
// there and would be a behaviour change rather than a rename -- an
// operator whose compose file says RETND_DEBUG=true (a typo; only "1"
// counts) and whose unit still says BACKUPD_DEBUG=1 has asked for debug
// twice, and Which would answer "the current name says `true`, which is
// not `1`, so no". EPIC R renames things; it does not change what a
// deployment does (spec §1).
//
// Everything else uses Which, where the value carries meaning in both
// directions and "the newest name an operator wrote wins" is the only
// defensible rule.
func Any(r Rename, want string) bool {
	found := os.Getenv(r.Current) == want

	for _, legacy := range r.Legacy {
		value, set := os.LookupEnv(legacy)
		if !set || value == "" {
			continue
		}

		warnOnce(r.Current, legacy, os.Getenv(r.Current) != "")
		found = found || value == want
	}

	return found
}

// warnState is the notice bookkeeping: which legacy names this process
// has already reported, and where a notice is written.
//
// The writer is a variable rather than os.Stderr inline so this package's
// own test can read what it produced, which is the only way to hold the
// once-per-name property -- the point of the whole mechanism is that a
// variable read on every cycle does not print on every cycle.
var warnState = struct {
	mu     sync.Mutex
	warned map[string]struct{}
	out    io.Writer
}{out: os.Stderr}

// warnOnce writes the deprecation notice for legacy the first time this
// process sees it, and nothing on any later read.
func warnOnce(current, legacy string, shadowed bool) {
	warnState.mu.Lock()
	defer warnState.mu.Unlock()

	if _, already := warnState.warned[legacy]; already {
		return
	}
	if warnState.warned == nil {
		warnState.warned = make(map[string]struct{})
	}
	warnState.warned[legacy] = struct{}{}

	honoured := "This process is reading " + legacy + "; set " + current + " instead."
	if shadowed {
		honoured = current + " is also set and wins, so " + legacy +
			" is being IGNORED; remove it."
	}

	fmt.Fprintf(warnState.out,
		"deprecated environment variable: %s has been renamed %s and will stop being read in %s. %s\n",
		legacy, current, RemovalRelease, honoured)
}

// WarnedNames returns the legacy names this process has already reported,
// sorted. It exists for the tests that hold the once-per-name property
// across package boundaries, and for nothing else: no behaviour in this
// product may depend on whether a notice has been printed.
func WarnedNames() []string {
	warnState.mu.Lock()
	defer warnState.mu.Unlock()

	out := make([]string, 0, len(warnState.warned))
	for name := range warnState.warned {
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}
