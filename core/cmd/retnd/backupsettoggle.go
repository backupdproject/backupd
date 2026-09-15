package main

import (
	"context"
	"fmt"

	"github.com/retnd/retnd/core/service"
)

// `backup-set enabled` and `backup-set read-only`: the two post-creation
// toggles a terminal could not reach.
//
// # The gap these close
//
// --disabled and --read-only are create-only flags (backupSetCreateOnlyFlags),
// deliberately: they are not ordinary edits and folding them into `patch`
// would put a set's most consequential switch beside its port number,
// where a sparse edit body could carry it by accident. But the Web UI has
// had POST /backup-sets/{source}/{set}/enabled and .../read-only since
// #146 and #316, and core/cliecho's table had to answer both with "there
// is no verb", printed at an operator who had just watched somebody else
// do it in a browser. A backup set could be created disabled from a
// terminal and never enabled again from one.
//
// # Why a word rather than a flag
//
// `backup-set read-only <source/backup-set> on` reads as an instruction
// and `--read-only` reads as a property, and the difference matters for
// the one that is a safety declaration: read-only is what stops this
// manager ever deleting the remote originals, and turning it OFF is the
// dangerous direction. A bare flag has no off, a --read-only=false is a
// spelling people get wrong under pressure, and two verbs (enable /
// disable) would double the surface for one setting. One verb and one
// word says which setting, which set, and which way, in that order.
//
// # Why they are separate verbs rather than one
//
// Because they are separate settings with opposite polarities and
// different consequences. "Enabled" decides whether this manager LOOKS at
// a source; "read-only" decides whether it may ever DELETE from one. A
// single `backup-set set <flag> <on|off>` would make the two one keystroke
// apart.

// toggleWords are the two words these verbs accept, and the only two.
//
// true/false, yes/no and 1/0 are all deliberately refused. A toggle that
// accepts six spellings is a toggle whose scripts each pick a different
// one, and the refusal names both accepted words, so somebody who typed
// "true" is one line away from the right command rather than one search.
var toggleWords = map[string]bool{"on": true, "off": false}

// cmdBackupSetEnabled is `backupd backup-set enabled
// <source/backup-set> <on|off>`.
//
// Disabling is not a pause button for a running pass: it stops the
// SCHEDULER offering this set to the next cycle. A pass already inside
// the set finishes, which is the edit hold's job to interrupt, not this
// one's.
func cmdBackupSetEnabled(args []string) int {
	return backupSetToggle(args, "enabled",
		func(ctx context.Context, route backupSetRoute, id string, on bool) (service.BackupSet, error) {
			return route.SetBackupSetEnabled(ctx, id, on)
		},
		func(set service.BackupSet) string {
			if set.Disabled {
				return "disabled: the scheduler will not offer this backup set to a cycle"
			}

			return "enabled: the scheduler will offer this backup set to the next cycle"
		})
}

// cmdBackupSetReadOnly is `backupd backup-set read-only
// <source/backup-set> <on|off>`.
//
// Turning it ON is a promise this manager keeps forever after: it will
// pull from the source and never delete the originals. Turning it OFF
// gives that promise up, and the service is what refuses when the source
// itself cannot support the alternative -- nothing here re-decides it,
// because a second opinion about whether a source may be deleted from is
// a second answer.
func cmdBackupSetReadOnly(args []string) int {
	return backupSetToggle(args, "read-only",
		func(ctx context.Context, route backupSetRoute, id string, on bool) (service.BackupSet, error) {
			return route.SetBackupSetReadOnly(ctx, id, on)
		},
		func(set service.BackupSet) string {
			if set.ReadOnly {
				return "read-only: this manager will never delete this backup set's remote originals"
			}

			return "not read-only: this manager may delete a remote original once its backup is durable"
		})
}

// backupSetToggle is the shape both verbs share: parse, resolve the word,
// write, and print what the set says afterwards.
//
// It prints the state READ BACK from the write rather than the state that
// was asked for. A write that succeeded and a write that was coerced or
// refused-and-recovered are different outcomes, and a command that echoed
// its own request would report the first for all of them.
func backupSetToggle(
	args []string,
	verb string,
	write func(context.Context, backupSetRoute, string, bool) (service.BackupSet, error),
	describe func(service.BackupSet) string,
) int {
	fs, cfgPath := newFlagSet("backup-set " + verb)

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	// The verb itself is operands[0]: cmdBackupSet hands every handler
	// the whole argument list, so a flag written before the verb is not
	// silently dropped. See backupSetVerbs' own doc.
	if len(operands) != 3 || operands[0] != verb {
		return usageError("backup-set %s: expected %q <source/backup-set> <on|off>", verb, verb)
	}

	id := operands[1]
	if _, _, ok := splitBackupSetID(id); !ok {
		return usageError("backup-set %s: %q is not a backup set id; a backup set id is exactly source/name", verb, id)
	}

	on, ok := toggleWords[operands[2]]
	if !ok {
		return usageError("backup-set %s: %q is not on or off; those two words are the only ones this verb takes", verb, operands[2])
	}

	ctx := context.Background()
	// openConfigWriteRoute, not openBackupService: both toggles rewrite
	// config.yaml and hot-reload, exactly as `backup-set patch` does, so
	// they go through the same door and get the same three outcomes.
	// With nothing serving this deployment the write happens here; with
	// a serving engine this command has a route to, it IS POST
	// /api/v1/backup-sets/{source}/{set}/enabled (or .../read-only)
	// against the process that will act on it; with a serving engine and
	// no route it is refused and the file is untouched.
	//
	// They came through the direct door until #788, which made them the
	// last configuration writes in this binary that COULD be routed and
	// were not: an operator running a real engine could disable a set in
	// a browser and never from a terminal.
	route, cleanup, err := openConfigWriteRoute(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	set, err := write(ctx, route, id, on)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("%s\n", set.ID)
	fmt.Printf("  %s\n", describe(set))

	return 0
}
