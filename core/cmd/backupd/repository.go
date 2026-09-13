package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/service"
)

// `backupd repository <verb>`: the store an incremental backup set's
// snapshots live in, as an operator asks about it (#788).
//
// # Why this is a verb group of its own
//
// A repository domain is a deployment-level declaration several backup
// sets may share, so its health is nobody's set's property. `status`
// reports backup sets, correctly, and a repository that is unwritable
// changes none of their verdicts until the next run fails -- which is
// exactly the window this command exists to close.
//
// # Why the health read costs something and the maintenance read does not
//
// `health` opens every declared repository, because "writable" is a claim
// nothing can make from a read. `maintenance` opens none: the ownership
// record is a file this deployment writes beside its own state, which is
// what makes it answerable in the case an operator actually asks in --
// the repository is usually the thing that is not answering.

var repositoryVerbs = map[string]func(args []string) int{
	"create":      repositoryCreate,
	"health":      repositoryHealth,
	"maintenance": repositoryMaintenance,
}

func repositoryVerbNames() []string {
	names := make([]string, 0, len(repositoryVerbs))
	for name := range repositoryVerbs {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// cmdRepository dispatches `backupd repository <verb> ...`.
func cmdRepository(args []string) int {
	for _, a := range args {
		if verb, ok := repositoryVerbs[a]; ok {
			return verb(args)
		}
	}

	return usageError("repository: expected a verb; the verbs are %s", strings.Join(repositoryVerbNames(), ", "))
}

// repositoryCreate is `backupd repository create <domain> ...` (#862):
// the terminal's way to declare a repository domain, through the same
// *BackupService method POST /api/v1/repositories calls.
//
// It goes through openConfigWriteRoute rather than openBackupService,
// like every other verb in this binary that rewrites config.yaml (#538,
// #543): beside a serving engine, a declaration written into the file by
// this process is a change that process never reads, and its next
// configuration write would put the file back without it.
//
// It creates no store. The repository is realized by the first backup run
// that stores a snapshot in the domain, which is the lifecycle a domain
// named on the add-backup-set wizard's repository step already has; see
// core/service's repositorydomain.go for why eager creation would be the
// worse half of that choice.
func repositoryCreate(args []string) int {
	fs, cfgPath := newFlagSet("repository create")
	isolation := fs.String("isolation", "", "shared or isolated: whether more than one backup set may store snapshots here. Required; there is no default")
	description := fs.String("description", "", "what this domain holds, in the operator's own words")
	location := fs.String("location", "", "where the repository is stored; empty is this deployment's own storage location, which is the only one it can honour")
	owner := fs.String("owner", "", "this or another-instance: which deployment maintains the repository (ADR 0017). Empty is this one")
	passphraseFile := fs.String("passphrase-file", "", "path to a file holding this repository's passphrase")
	passphraseEnv := fs.String("passphrase-env", "", "name of an environment variable holding this repository's passphrase")
	var passphraseCommand stringList
	fs.Var(&passphraseCommand, "passphrase-command", "a command whose stdout is this repository's passphrase; repeat the flag once per argv word")

	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 || operands[0] != "create" {
		return usageError(`repository create: expected "create <repository-domain>" and exactly one domain`)
	}

	// The two required answers are refused here rather than at the
	// service, because a terminal can say what to type and an API
	// refusal cannot. Isolation has no default for the reason the
	// configuration gives at length: defaulting to shared makes an
	// isolation boundary a belief, and defaulting to isolated silently
	// forgoes the deduplication the engine exists for.
	if *isolation == "" {
		return usageError("repository create: --isolation is required and is shared or isolated; a domain that does not state its co-tenancy is not a boundary")
	}
	named := 0
	for _, set := range []bool{*passphraseFile != "", *passphraseEnv != "", len(passphraseCommand) > 0} {
		if set {
			named++
		}
	}
	if named != 1 {
		return usageError("repository create: name exactly one of --passphrase-file, --passphrase-env or --passphrase-command; a repository this product creates is always encrypted, and there is no flag to type a passphrase into")
	}

	ctx := context.Background()
	route, cleanup, err := openConfigWriteRoute(ctx, *cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	created, err := route.CreateRepositoryDomain(ctx, service.CreateRepositoryDomainRequest{
		ID:          operands[1],
		Description: *description,
		Isolation:   *isolation,
		Passphrase: service.RepositoryPassphraseRef{
			File:    *passphraseFile,
			Env:     *passphraseEnv,
			Command: passphraseCommand,
		},
		Location:         *location,
		MaintenanceOwner: *owner,
	})
	if err != nil {
		return fail(err)
	}

	fmt.Printf("declared repository domain %s\n", created.Domain)
	fmt.Printf("  no store was created: the repository is written the first time a backup set stores a snapshot in this domain\n")
	printRepositoryHealth(created)

	return 0
}

// stringList collects a flag given more than once, which is how an argv
// array reaches a command line: one word per occurrence, so nothing has
// to guess where a shell would have split a single string.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, " ") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)

	return nil
}

// repositoryHealth is `backupd repository health`.
//
// The exit code is the verdict: a deployment with a failing repository
// exits non-zero, so this is usable in the same monitoring shape `check`
// already is. A degraded one does not, for the reason it is degraded
// rather than failing -- overdue maintenance is worth telling somebody
// and is not a reason to page them.
func repositoryHealth(args []string) int {
	fs, cfgPath := newFlagSet("repository health")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 1 || operands[0] != "health" {
		return usageError(`repository health: expected "health" and no operand`)
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	report, err := svc.ListRepositories(ctx)
	if err != nil {
		return fail(err)
	}

	if len(report.Repositories) == 0 {
		fmt.Println("this deployment declares no repository domain, so it stores no snapshots")

		return 0
	}

	failing := false
	for _, r := range report.Repositories {
		if r.State == "FAILING" {
			failing = true
		}
		printRepositoryHealth(r)
	}

	if failing {
		return 1
	}

	return 0
}

func printRepositoryHealth(r service.RepositoryHealth) {
	fmt.Printf("%s  %s\n", r.Domain, r.State)
	fmt.Printf("  shared:          %v\n", r.MayShare)
	fmt.Printf("  reachable:       %v\n", r.Reachable)
	fmt.Printf("  readable:        %v\n", r.Readable)
	fmt.Printf("  writable:        %v\n", r.Writable)
	fmt.Printf("  credentials:     %s\n", credentialWords(r.CredentialsValid))
	fmt.Printf("  clock:           %s\n", clockWords(r))
	fmt.Printf("  maintenance:     %s\n", maintenanceWords(r))
	fmt.Printf("  last snapshot:   %s\n", eventWords(r.LastSnapshotAt, r.LastSnapshotStatus))
	fmt.Printf("  last verified:   %s\n", eventWords(r.LastVerificationAt, r.LastVerificationStatus))
	for _, set := range r.BackupSets {
		fmt.Printf("  backup set:      %s\n", set)
	}
	if r.Detail != "" {
		fmt.Printf("  %s\n", r.Detail)
	}
}

func credentialWords(valid bool) string {
	if valid {
		return "the declared passphrase opens this repository"
	}

	return "the declared passphrase did not open this repository"
}

// clockWords reports the comparison rather than a bare boolean, because
// "your clock is wrong" with no number is not something anybody can act
// on.
func clockWords(r service.RepositoryHealth) string {
	if r.ClockSkew == nil {
		return "nothing durable to compare against yet"
	}

	skew := r.ClockSkew.Round(time.Second)
	word := "sane"
	if !r.ClockSane {
		word = "NOT SANE"
	}

	if skew < 0 {
		return fmt.Sprintf("%s (this machine reads %s earlier than the newest record it has written)", word, (-skew).String())
	}

	return fmt.Sprintf("%s (this machine reads %s later than the newest record it has written)", word, skew.String())
}

func maintenanceWords(r service.RepositoryHealth) string {
	if r.LastMaintenanceAt.IsZero() {
		return "never run"
	}

	words := fmt.Sprintf("%s at %s", orNotRecorded(r.LastMaintenanceResult), r.LastMaintenanceAt.UTC().Format(time.RFC3339))
	if r.MaintenanceOverdue {
		words += " (OVERDUE)"
	}

	return words
}

func eventWords(at time.Time, status string) string {
	if at.IsZero() {
		return "none"
	}

	return fmt.Sprintf("%s at %s", orNotRecorded(status), at.UTC().Format(time.RFC3339))
}

// repositoryMaintenance is `backupd repository maintenance <domain>`.
func repositoryMaintenance(args []string) int {
	fs, cfgPath := newFlagSet("repository maintenance")
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) != 2 || operands[0] != "maintenance" {
		return usageError(`repository maintenance: expected "maintenance <repository-domain>" and exactly one domain`)
	}

	ctx := context.Background()
	svc, cleanup, err := openBackupService(ctx, *cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	state, err := svc.RepositoryMaintenanceState(ctx, operands[1])
	if err != nil {
		return fail(err)
	}

	fmt.Printf("%s\n", state.Domain)
	fmt.Printf("  owner:         %s\n", ownerWords(state.Owner))
	fmt.Printf("  last quick:    %s\n", instantWords(state.LastQuickAt))
	fmt.Printf("  last full:     %s\n", instantWords(state.LastFullAt))
	fmt.Printf("  next eligible: %s\n", instantWords(state.NextEligibleAt))
	fmt.Printf("  due:           %s\n", dueWords(state))
	fmt.Printf("  overdue:       %v\n", state.Overdue)
	fmt.Printf("  runs:          %d (%d failed)\n", state.Runs, state.Failures)
	fmt.Printf("  reclaimed:     %d bytes\n", state.ReclaimedBytes)
	if state.Failing {
		fmt.Printf("  the most recent maintenance window failed. No snapshot was deleted and no restore point was affected; storage freed by deleted snapshots stays occupied until a window succeeds\n")
	}

	return 0
}

// ownerWords distinguishes "nobody owns maintenance for this repository"
// from "somebody else does", which are different situations with
// different fixes and which a blank would collapse.
func ownerWords(owner string) string {
	if owner == "" {
		return "nobody has claimed maintenance for this repository"
	}

	return owner
}

func instantWords(at time.Time) string {
	if at.IsZero() {
		return "never"
	}

	return at.UTC().Format(time.RFC3339)
}

func dueWords(state service.RepositoryMaintenance) string {
	if !state.Due {
		return "no: " + state.DueReason
	}

	return fmt.Sprintf("yes, %s: %s", state.DueMode, state.DueReason)
}
