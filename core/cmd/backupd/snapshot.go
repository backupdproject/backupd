package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/service"
)

// `backupd snapshot <verb>`: EPIC K's operator surface on a terminal
// (#788).
//
// # Why one grouped verb rather than eight top-level ones
//
// Because that is how this binary already groups a noun's actions --
// `medium`, `quarantine`, `catalog`, `backup-set` -- and because eight
// top-level verbs about one thing is a menu nobody can scan. The group is
// `snapshot` and not `kopia`, which is EPIC K's own rule: no disconnected
// engine namespace anywhere an operator can see.
//
// # Why every mutating verb submits a durable operation
//
// The Web UI's buttons submit one, and CLI/Web parity is not "both can do
// it", it is "both do the same thing". A verb that reached the engine by
// some other path would be a second implementation of hold, verify and
// restore, with its own idempotency behaviour and its own way of failing
// halfway; it would also be invisible to `activity` and to the operations
// list, which is where an operator looks afterwards to find out what
// happened.
//
// So each of them writes exactly the row the API writes, with an
// idempotency key of its own. The key is fresh per invocation, for
// `restore`'s reason: a CLI run is a deliberate act by a person at a
// keyboard, so two runs are two requests, and reusing a key would return
// the first run's operation to somebody who meant to start another.

// snapshotVerbs is every verb `snapshot` dispatches, and the list usage()
// is checked against.
//
// A map read as data as well as walked, exactly like the top-level
// commands table: a verb that is dispatchable and undiscoverable is the
// defect that list exists to prevent.
var snapshotVerbs = map[string]func(args []string) int{
	"list":      snapshotList,
	"show":      snapshotShow,
	"holds":     snapshotHolds,
	"retention": snapshotRetention,
	"verify":    snapshotVerify,
	"restore":   snapshotRestore,
	"hold":      snapshotHold,
	"unhold":    snapshotUnhold,
}

func snapshotVerbNames() []string {
	names := make([]string, 0, len(snapshotVerbs))
	for name := range snapshotVerbs {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// cmdSnapshot dispatches `backupd snapshot <verb> ...`.
//
// The verb is found by scanning the arguments rather than by taking
// args[0], for cmdBackupSet's reason: a flag written before the verb must
// not silently turn the verb into a flag value.
func cmdSnapshot(args []string) int {
	for _, a := range args {
		if verb, ok := snapshotVerbs[a]; ok {
			return verb(args)
		}
	}

	return usageError("snapshot: expected a verb; the verbs are %s", strings.Join(snapshotVerbNames(), ", "))
}

// snapshotOperand reads the one backup set id a verb takes, after its
// own verb word.
func snapshotOperand(verb string, operands []string, want int) ([]string, int) {
	if len(operands) != want+1 || operands[0] != verb {
		return nil, usageError("snapshot %s: expected %q followed by exactly %d operand(s)", verb, verb, want)
	}

	return operands[1:], 0
}

func snapshotList(args []string) int {
	fs, cfgPath := newFlagSet("snapshot list")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("list", operands, 1)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	snapshots, err := svc.ListSnapshots(ctx, rest[0])
	if err != nil {
		return fail(err)
	}

	if len(snapshots) == 0 {
		fmt.Printf("%s has taken no snapshots\n", rest[0])

		return 0
	}

	for _, s := range snapshots {
		printSnapshotLine(s)
	}

	return 0
}

// printSnapshotLine is one row of the list: what it is, whether it is a
// restore point, and the four measurements that must never be collapsed
// into one.
func printSnapshotLine(s service.Snapshot) {
	marker := " "
	if s.LastKnownGood {
		marker = "*"
	}

	fmt.Printf("%s %-24s %-20s %s\n", marker, s.RunID, s.Phase, s.StartedAt.UTC().Format(time.RFC3339))
	fmt.Printf("    snapshot:      %s\n", orNotRecorded(s.SnapshotID))
	fmt.Printf("    verification:  %s\n", verificationWords(s))
	fmt.Printf("    scanned:       %s entries, %s logical\n", counterWords(s.EntriesScanned), byteWords(s.LogicalBytes))
	fmt.Printf("    read/written:  %s read from source, %s written, %s reused\n",
		byteWords(s.SourceBytesRead), byteWords(s.RepositoryBytesWritten), byteWords(s.ContentReusedBytes))
	if s.Duration != nil {
		fmt.Printf("    duration:      %s\n", s.Duration.Round(time.Second))
	}
	if len(s.Holds) > 0 {
		fmt.Printf("    holds:         %d\n", len(s.Holds))
	}
	if s.Reason != "" {
		fmt.Printf("    reason:        %s\n", s.Reason)
	}
}

func snapshotShow(args []string) int {
	fs, cfgPath := newFlagSet("snapshot show")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("show", operands, 2)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	detail, err := svc.GetSnapshot(ctx, rest[0], rest[1])
	if err != nil {
		return fail(err)
	}

	s := detail.Snapshot
	fmt.Printf("%s\n", s.RunID)
	fmt.Printf("  backup set:      %s\n", s.BackupSetID)
	fmt.Printf("  engine:          %s\n", s.Engine)
	fmt.Printf("  repository:      %s\n", s.RepositoryDomain)
	fmt.Printf("  snapshot:        %s\n", orNotRecorded(s.SnapshotID))
	fmt.Printf("  phase:           %s\n", s.Phase)
	fmt.Printf("  restore point:   %v\n", s.LastKnownGood)
	fmt.Printf("  consistency:     %s\n", orNotRecorded(s.SourceConsistency))
	fmt.Printf("  verification:    %s\n", verificationWords(s))
	fmt.Printf("  entries scanned: %s\n", counterWords(s.EntriesScanned))
	fmt.Printf("  files:           %s\n", counterWords(s.Files))
	fmt.Printf("  directories:     %s\n", counterWords(s.Directories))
	fmt.Printf("  logical size:    %s\n", byteWords(s.LogicalBytes))
	fmt.Printf("  read from source:%s\n", " "+byteWords(s.SourceBytesRead))
	fmt.Printf("  written:         %s\n", byteWords(s.RepositoryBytesWritten))
	fmt.Printf("  reused:          %s\n", byteWords(s.ContentReusedBytes))
	fmt.Printf("  source complete: %s\n", boolWords(s.SourceComplete))
	fmt.Printf("  started:         %s\n", s.StartedAt.UTC().Format(time.RFC3339))
	if s.CompletedAt != nil {
		fmt.Printf("  completed:       %s\n", s.CompletedAt.UTC().Format(time.RFC3339))
	}
	if s.Reason != "" {
		fmt.Printf("  reason:          %s\n", s.Reason)
	}

	for _, h := range s.Holds {
		fmt.Printf("  hold %s: %s (placed by %s at %s)\n", h.HoldID, h.Reason, h.PlacedBy, h.PlacedAt.UTC().Format(time.RFC3339))
	}

	for _, t := range detail.Transitions {
		from := t.From
		if from == "" {
			from = "-"
		}
		fmt.Printf("  %s  %s -> %s  %s\n", t.At.UTC().Format(time.RFC3339), from, t.To, t.Detail)
	}

	return 0
}

func snapshotHolds(args []string) int {
	fs, cfgPath := newFlagSet("snapshot holds")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("holds", operands, 1)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	holds, err := svc.ListSnapshotHolds(ctx, rest[0])
	if err != nil {
		return fail(err)
	}

	if len(holds) == 0 {
		fmt.Printf("%s holds no snapshot\n", rest[0])

		return 0
	}

	for _, h := range holds {
		fmt.Printf("%s  run %s\n", h.HoldID, h.RunID)
		fmt.Printf("  reason:    %s\n", h.Reason)
		fmt.Printf("  placed by: %s at %s\n", h.PlacedBy, h.PlacedAt.UTC().Format(time.RFC3339))
	}

	return 0
}

func snapshotRetention(args []string) int {
	fs, cfgPath := newFlagSet("snapshot retention")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("retention", operands, 1)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	preview, err := svc.SnapshotRetention(ctx, rest[0])
	if err != nil {
		return fail(err)
	}

	fmt.Printf("as of %s (this is a preview; nothing is deleted)\n", preview.GeneratedAt.UTC().Format(time.RFC3339))
	for _, v := range preview.Verdicts {
		fmt.Printf("%-8s %-24s %s\n", v.Action, v.RunID, v.StartedAt.UTC().Format(time.RFC3339))
		fmt.Printf("    %s\n", v.Reason)
		if len(v.Tiers) > 0 {
			words := make([]string, 0, len(v.Tiers))
			for _, t := range v.Tiers {
				if t.SelectedBy == "" {
					words = append(words, t.Tier)
					continue
				}
				words = append(words, t.Tier+"("+strings.ToLower(t.SelectedBy)+")")
			}
			fmt.Printf("    kept by: %s\n", strings.Join(words, ", "))
		}
		for _, h := range v.Holds {
			fmt.Printf("    hold %s: %s\n", h.HoldID, h.Reason)
		}
		if v.HoldReason != "" {
			fmt.Printf("    set-wide refusal: %s\n", v.HoldReason)
		}
	}

	return 0
}

func snapshotVerify(args []string) int {
	fs, cfgPath := newFlagSet("snapshot verify")
	run := fs.String("run", "", "the snapshot run to verify; the set's last known good snapshot when not given")
	level := fs.String("level", "", "how deep to look: structural, content_sample, content_full or restore_drill. The set's configured level when not given")
	sample := fs.Int("sample-percent", 0, "how much of the snapshot's file content a content_sample reads, 1 to 100")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("verify", operands, 1)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	op, err := svc.SubmitSnapshotVerify(ctx, service.SnapshotVerifyRequest{
		Actor:          "cli",
		IdempotencyKey: "cli-verify-snapshot:" + uuid.NewString(),
		ConfigRevision: svc.ConfigRevision(),
		BackupSetID:    rest[0],
		RunID:          *run,
		Level:          *level,
		SamplePercent:  *sample,
	})
	if err != nil {
		return fail(err)
	}

	return awaitSnapshotOperation(ctx, svc, op, "verification")
}

func snapshotRestore(args []string) int {
	fs, cfgPath := newFlagSet("snapshot restore")
	to := fs.String("to", "", "the local directory to restore into (required)")
	snapshotID := fs.String("snapshot", "", "the restore point to read; the set's last known good snapshot when not given")
	path := fs.String("path", "", "one path inside the snapshot to restore; the whole tree when not given")
	conflict := fs.String("conflict", "refuse", "what to do when the destination already holds something: refuse, skip or overwrite")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("restore", operands, 1)
	if code != 0 {
		return code
	}

	if *to == "" {
		return usageError("snapshot restore: --to is required; a restore writes a tree of your data onto a disk and this command will not choose which one")
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	op, err := svc.SubmitSnapshotRestore(ctx, service.SnapshotRestoreRequest{
		Actor:          "cli",
		IdempotencyKey: "cli-restore-snapshot:" + uuid.NewString(),
		ConfigRevision: svc.ConfigRevision(),
		BackupSetID:    rest[0],
		SnapshotID:     *snapshotID,
		SourcePath:     *path,
		TargetPath:     *to,
		Conflict:       *conflict,
	})
	if err != nil {
		return fail(err)
	}

	return awaitSnapshotOperation(ctx, svc, op, "restore")
}

func snapshotHold(args []string) int {
	fs, cfgPath := newFlagSet("snapshot hold")
	run := fs.String("run", "", "the snapshot run to hold; the set's last known good snapshot when not given")
	reason := fs.String("reason", "", "why this snapshot must not be deleted (required)")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("hold", operands, 1)
	if code != 0 {
		return code
	}

	if *reason == "" {
		return usageError("snapshot hold: --reason is required. A hold nobody explained is one nobody dares release, which makes it permanent by accident")
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	op, err := svc.SubmitSnapshotHold(ctx, service.SnapshotHoldRequest{
		Actor:          "cli",
		IdempotencyKey: "cli-hold-snapshot:" + uuid.NewString(),
		ConfigRevision: svc.ConfigRevision(),
		BackupSetID:    rest[0],
		RunID:          *run,
		Reason:         *reason,
	})
	if err != nil {
		return fail(err)
	}

	return reportSnapshotOperation(op, "hold")
}

func snapshotUnhold(args []string) int {
	fs, cfgPath := newFlagSet("snapshot unhold")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}

	rest, code := snapshotOperand("unhold", operands, 2)
	if code != 0 {
		return code
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	op, err := svc.SubmitSnapshotHoldRelease(ctx, service.SnapshotHoldReleaseRequest{
		Actor:          "cli",
		IdempotencyKey: "cli-release-hold:" + uuid.NewString(),
		ConfigRevision: svc.ConfigRevision(),
		BackupSetID:    rest[0],
		HoldID:         rest[1],
	})
	if err != nil {
		return fail(err)
	}

	return reportSnapshotOperation(op, "release")
}

// snapshotOperationPoll is how often a waiting verb re-reads the
// operation it submitted.
//
// A second, because the two acts that wait -- a verification and a
// restore -- are measured in minutes at least, and a tighter loop would
// spend a query per tick on a journal a cycle may also be writing.
const snapshotOperationPoll = time.Second

// awaitSnapshotOperation waits for an operation this process is
// executing and reports how it ended.
//
// It waits rather than printing an id and exiting, and that is the
// difference between this and `restore` (the archived-copy retrieval).
// That one hands the work to a storage provider that carries on across a
// restart; these two run HERE, on a goroutine inside this process, so a
// command that returned immediately would exit and take its own work with
// it. An operator on a terminal also expects a verification to tell them
// whether it passed.
//
// The exit code is the answer: a verification that found damage and a
// restore that did not finish both exit non-zero, so a script branches on
// the status rather than on the text.
func awaitSnapshotOperation(ctx context.Context, svc *service.BackupService, op service.Operation, what string) int {
	fmt.Printf("operation: %s\n", op.ID)

	for {
		current, err := svc.GetOperation(ctx, op.ID)
		if err != nil {
			return fail(err)
		}

		if current.Status == "completed" || current.Status == "failed" {
			return reportSnapshotOperation(current, what)
		}

		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(snapshotOperationPoll):
		}
	}
}

// reportSnapshotOperation prints a finished operation's outcome and
// returns the exit code that matches it.
func reportSnapshotOperation(op service.Operation, what string) int {
	switch op.Status {
	case "completed":
		fmt.Printf("%s: completed\n", what)
		if op.Result != "" {
			fmt.Printf("  %s\n", op.Result)
		}

		return 0
	case "failed":
		fmt.Printf("%s: failed\n", what)
		if op.Error != "" {
			fmt.Printf("  %s\n", op.Error)
		}

		return 1
	default:
		fmt.Printf("%s: %s (operation %s)\n", what, op.Status, op.ID)
		fmt.Printf("  check on it with: %s status, or GET /api/v1/operations/%s\n", cliecho.Binary, op.ID)

		return 0
	}
}

// counterWords renders a nullable counter, saying "not measured" rather
// than zero when nobody took it.
//
// This is the whole reason the counters are pointers all the way from the
// catalog to here: a run that died before its manifest was recorded read
// as a run that scanned nothing, which is a different and much more
// alarming claim.
func counterWords(v *int64) string {
	if v == nil {
		return "not measured"
	}

	return fmt.Sprintf("%d", *v)
}

func byteWords(v *int64) string {
	if v == nil {
		return "not measured"
	}

	return fmt.Sprintf("%d bytes", *v)
}

func boolWords(v *bool) string {
	if v == nil {
		return "no verdict recorded"
	}

	return fmt.Sprintf("%v", *v)
}

func orNotRecorded(v string) string {
	if v == "" {
		return "not recorded"
	}

	return v
}

// verificationWords says what was asked for and what was actually
// proven, which are two claims and are never printed as one.
func verificationWords(s service.Snapshot) string {
	status := s.VerificationStatus
	if status == "" {
		status = "none attempted"
	}

	achieved := s.VerificationAchieved
	if achieved == "" {
		achieved = "nothing proven"
	}

	return fmt.Sprintf("%s (asked for %s, proved %s)", status, orNotRecorded(s.VerificationLevel), achieved)
}
