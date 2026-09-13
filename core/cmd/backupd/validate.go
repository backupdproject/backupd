package main

import (
	"context"
	"fmt"

	"github.com/backupdproject/backupd/core/internal/app"
)

// cmdValidate is `backupd validate <source/backup-set/artifact>`:
// an on-demand re-check of one already-committed artifact's durable copy,
// wherever that copy actually is. See internal/app.ValidateArtifact's doc
// for exactly what it checks and why a failure quarantines the artifact
// with no --dry-run guard (the consequence is protective, never
// destructive).
//
// # Why this one opens a transport and the other read-only commands do not
//
// openService's withTransport argument is what fills in
// Service.MediumStore, because FR-28's medium boundary IS the embedded
// rclone adapter (see app.Service.MediumStore). Since issue #435 this
// command can be asked about an artifact whose only durable copy is an
// object on a storage medium, and without that adapter it has nothing to
// ask, so it would refuse every moved artifact in every deployment,
// forever, for a reason no operator could act on. rclone.New() allocates
// an empty struct and opens no connection, so a `validate` against a
// local copy pays nothing for holding one.
//
// # The two subjects, and why one command takes both
//
// `validate <source/backup-set/artifact>` re-checks a stored backup.
// `validate workflow <source/backup-set>` reports on a backup set's hook
// scripts (EPIC L, #813, validateworkflow.go). They are forms of one
// verb rather than two commands because an operator asking "is this
// sound" asks it the same way about both, and because the word after
// `validate` says which subject unambiguously: a backup set id is
// exactly source/name and an artifact id has the file name on the end of
// it, so neither can ever be the literal word "workflow".
func cmdValidate(args []string) int {
	fs, cfgPath := newFlagSet("validate")
	// --content is FR-31's operator-initiated content check. It is a flag
	// rather than the default because it downloads the object from the
	// storage medium, and egress is a bill; see app.ValidateOptions.
	content := fs.Bool("content", false,
		"re-verify a copy on a storage medium at the content class: download the object and re-hash it against the hash recorded at ingestion. This costs egress. A local copy is always content-checked, so this changes nothing for one")
	asJSON := fs.Bool("json", false,
		"workflow form only: emit the whole validation report as JSON, every finding and every resolved script included")
	// Flags may come before or after the operand; see
	// parseFlagsAroundOperands in setup.go for why.
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	// The workflow form is settled before the artifact form parses its
	// operand, because ParseArtifactID would refuse the word "workflow"
	// with a sentence about artifact ids and send an operator looking for
	// a mistake they did not make.
	if len(operands) > 0 && operands[0] == validateWorkflowOperand {
		if len(operands) != 2 {
			return usageError("validate workflow takes exactly one argument: <source/backup-set>")
		}
		// Refused rather than ignored, the rule every shared flag set in
		// this binary follows: --content asks for a stored object to be
		// downloaded and re-hashed, and there is no object anywhere in a
		// workflow validation for it to mean anything about.
		if *content {
			return usageError("validate workflow: --content re-downloads a stored backup and re-hashes it, and a workflow validation reads no backup at all; it hashes the hook scripts on disk, which it always does")
		}

		return validateWorkflow(*cfgPath, operands[1], *asJSON)
	}

	if len(operands) != 1 {
		return usageError("validate takes exactly one argument: <source/backup-set/artifact>, or \"workflow <source/backup-set>\"")
	}
	// The artifact form has no JSON rendering, and a --json that was
	// accepted and dropped there would report success for output nobody
	// got. Whether to give it one is a separate piece of work about a
	// different report (app.ValidateResult), not something to fold in
	// under a flag that arrived for the workflow form.
	if *asJSON {
		return usageError("validate: --json is a flag of the workflow form; `validate workflow <source/backup-set> --json` emits that report, and this form has no JSON rendering to give you")
	}

	id, err := app.ParseArtifactID(operands[0])
	if err != nil {
		return fail(err)
	}

	ctx := context.Background()
	svc, _, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))

	result, err := svc.ValidateArtifact(ctx, id, app.ValidateOptions{Content: *content})
	if err != nil {
		return fail(err)
	}

	fmt.Printf("%s: checked=%v passed=%v\n  %s\n", id, result.Checked, result.Passed, result.Reason)
	if result.NewState != "" {
		fmt.Printf("  -> %s\n", result.NewState)
	}
	if !result.Passed {
		return 1
	}
	return 0
}
