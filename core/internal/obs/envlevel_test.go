package obs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Issue #730. The engine built its sink at LevelInfo whatever the
// environment said, so the one deployment that could not be reproduced
// was also the one that could not be turned up: an operator setting
// LOG_LEVEL=debug on both containers got the UI host's proxy trace and
// nothing from the engine to match it against.
//
// The cases below are the precedence and the fallback, because those are
// the two things a second reader of the same variables can get subtly
// wrong (apps/common/webhost has one, and cannot import this one - see
// LevelFromEnv's own doc), plus the two deprecated aliases,
// BACKUPD_DEBUG (EPIC R, #885) and RM_DEBUG (#794): neither rename may
// turn a diagnosing operator's logs back off on upgrade.

func TestLevelFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name         string
		retndDebug   string
		backupdDebug string
		rmDebug      string
		logLevel     string
		want         Level
	}{
		{name: "nothing set is unchanged INFO", want: LevelInfo},
		{name: "LOG_LEVEL selects a level", logLevel: "debug", want: LevelDebug},
		{name: "LOG_LEVEL is case and space insensitive", logLevel: " WARN ", want: LevelWarn},
		{name: "LOG_LEVEL error", logLevel: "error", want: LevelError},
		// The shortcut an operator is told to set wins, so a deployment
		// that already had LOG_LEVEL=warn in its compose file still gets
		// diagnostics from one extra variable.
		{name: "RETND_DEBUG raises the level to debug", retndDebug: "1", want: LevelDebug},
		{name: "RETND_DEBUG wins over LOG_LEVEL", retndDebug: "1", logLevel: "warn", want: LevelDebug},
		// A typo in a diagnostic knob must never change how loud a backup
		// host is in some unpredictable direction, and must never stop it.
		{name: "an unparseable LOG_LEVEL falls back to INFO", logLevel: "verbose", want: LevelInfo},
		{name: "RETND_DEBUG only counts as the documented 1", retndDebug: "true", want: LevelInfo},
		// Each rename renamed the knob. A deployment upgraded without its
		// compose file or its unit being touched still has an older
		// spelling, and an operator mid-diagnosis must not have their
		// logs silently go quiet under them.
		{name: "the deprecated BACKUPD_DEBUG alias still works", backupdDebug: "1", want: LevelDebug},
		{name: "the deprecated BACKUPD_DEBUG alias still wins over LOG_LEVEL", backupdDebug: "1", logLevel: "warn", want: LevelDebug},
		{name: "the deprecated BACKUPD_DEBUG alias only counts as the documented 1", backupdDebug: "true", want: LevelInfo},
		{name: "the deprecated RM_DEBUG alias still works", rmDebug: "1", want: LevelDebug},
		{name: "the deprecated RM_DEBUG alias still wins over LOG_LEVEL", rmDebug: "1", logLevel: "warn", want: LevelDebug},
		{name: "the deprecated RM_DEBUG alias only counts as the documented 1", rmDebug: "true", want: LevelInfo},
		// One container upgraded before the other, or a half-edited
		// compose file: any spelling alone is enough, so the two
		// containers of one deployment cannot disagree about how loud
		// they are (the #730 failure this knob exists for). This is why
		// the three names are OR'd and not ranked: an operator who
		// asked for debug under an older name and typo'd the current one
		// still gets debug.
		{name: "any spelling alone is enough", retndDebug: "true", rmDebug: "1", want: LevelDebug},
		{name: "a typo'd current name does not cancel a deprecated one", retndDebug: "true", backupdDebug: "1", want: LevelDebug},
		{name: "all three spellings set is still debug", retndDebug: "1", backupdDebug: "1", rmDebug: "1", want: LevelDebug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RETND_DEBUG", tc.retndDebug)
			t.Setenv("BACKUPD_DEBUG", tc.backupdDebug)
			t.Setenv("RM_DEBUG", tc.rmDebug)
			t.Setenv("LOG_LEVEL", tc.logLevel)

			if got := LevelFromEnv(); got != tc.want {
				t.Errorf("LevelFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLevelFromEnvActuallyReachesTheSink is the half that matters to a
// deployment: a level nothing is built with is a level nobody gets. It
// goes through New rather than asserting on the constant, so a Logger
// wired at this level really does write the debug line an operator turned
// it on for.
func TestLevelFromEnvActuallyReachesTheSink(t *testing.T) {
	t.Setenv("RETND_DEBUG", "1")
	t.Setenv("RM_DEBUG", "")

	var buf bytes.Buffer
	New(&buf, LevelFromEnv()).emit(context.Background(), slog.LevelDebug, "diagnostic", "a debug line")

	if !strings.Contains(buf.String(), "a debug line") {
		t.Errorf("a sink built at the environment's level dropped its debug line; wrote %q", buf.String())
	}
}
