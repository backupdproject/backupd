package obs

import (
	"os"
	"strings"

	"github.com/retnd/retnd/core/envcompat"
)

// LevelFromEnv is how a deployment turns this sink up without a flag, a
// config key or a restart argument nobody can remember over a phone call:
// LOG_LEVEL=debug, or RETND_DEBUG=1 as the shortcut.
//
// It exists here, rather than at the one or two places that build a
// Logger, because issue #730's whole difficulty was that the two
// containers of one deployment answered "how loud am I" differently. The
// UI host read the environment (apps/common/webhost's own envLogLevel)
// and the engine hard-coded LevelInfo, so an operator who set LOG_LEVEL
// on both got the proxy's trace and none of the engine events the trace
// was supposed to be joined to.
//
// The two readers are deliberately not one shared helper: apps/ may
// import core/, never the reverse, and core/internal is unreachable from
// apps/ by construction. What keeps them honest is that they read the
// same variables with the same precedence and the same fallback,
// each covered by its own package's test, and that this comment and
// webhost's own say so. The one thing they DO share is core/envcompat,
// which is reachable from both and owns the deprecated names, because
// "one notice per name per process" is a property neither reader can
// hold on its own when `serve` runs both of them in one process.
//
// RETND_DEBUG wins over LOG_LEVEL because it is the shortcut an
// operator is told to set, and an unparseable LOG_LEVEL falls back to
// LevelInfo rather than refusing to start: a typo in a diagnostic knob
// must never take a backup host down.
//
// Two DEPRECATED spellings of that shortcut are still read, both under
// names this project used before (EPIC R, #885, FR-37): BACKUPD_DEBUG,
// which is the name docs/deployment.md and the compose files named until
// this rename, and RM_DEBUG, which is rclone-manager's (#794). Both are
// still honoured so an upgrade does not silently turn a diagnosing
// operator's logs back off, and each produces one deprecation notice per
// process the first time it is read. RETND_DEBUG is the only spelling
// this product writes or documents.
func LevelFromEnv() Level {
	if debugShortcut() {
		return LevelDebug
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		return LevelDebug
	case "warn":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// debugEnv is the one-variable debug shortcut and the two deprecated
// names it has had. Package-level so both this file's reader and its test
// name the same thing, and so webhost's copy can be compared against it
// by eye in a review.
var debugEnv = envcompat.Rename{
	Current: "RETND_DEBUG",
	Legacy:  []string{"BACKUPD_DEBUG", "RM_DEBUG"},
}

// debugShortcut reports whether the one-variable debug shortcut is set,
// under its own name or under either deprecated alias. Only the
// documented "1" counts, under every name: a knob whose typos mean
// something is a knob that surprises the operator reading it back.
//
// The three names are OR'd rather than ranked (envcompat.Any), which is
// the rule this knob has always had and the reason it exists: none of
// them has ever had an "off" value, so a deployment that sets two of
// them, or the old one on one container and the new one on the other,
// gets debug either way. Ranking them would make an operator who asked
// for debug twice get INFO, which is a behaviour change and not a
// rename. The variables whose values mean something in both directions
// are ranked instead (core/internal/config's incremental-engine gate).
func debugShortcut() bool {
	return envcompat.Any(debugEnv, "1")
}
