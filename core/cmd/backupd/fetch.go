package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/backupdproject/backupd/core/cliecho"
	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/lifecycle"
)

// cmdFetch is `backupd fetch --source S --backup-set B [--dry-run]`:
// an operator-triggered, on-demand run of exactly one configured backup
// set's cycle share. See internal/app.Service.Fetch's doc for exactly what
// --dry-run does and does not do (it never touches the journal at all;
// without the flag, Fetch runs the same reconcile/discover/transfer/
// verify/commit/delete sequence RunCycle would for this one backup set).
//
// # --skip-workflow-scripts is REFUSED here, and that is the honest answer
//
// EPIC L (#813) gives a per-set run the ability to take the backup and
// run none of that set's hook scripts, for one situation: a hook is
// broken at three in the morning and somebody needs tonight's backup. It
// is an administrator action, it is refused on a scheduled run, it is
// refused outright for a set with an unresolved interruption, and every
// bypass is recorded twice -- bypassed=1 on the durable workflow run row
// and a warn-level event naming the actor
// (service.RunBackupSetRequest.SkipWorkflowScripts).
//
// None of that can happen here, and the reason is structural rather than
// a missing wire-up. This command reaches internal/app.Service.Fetch
// through openService, which builds that service directly from a loaded
// configuration and a journal; the five-stage lifecycle is installed on
// an app.Service only by core/service's own BackupService, in
// installWorkflowLifecycle, and only after ReconcileWorkflows has
// succeeded. So the pass this command performs runs NO hooks at all --
// app.Service.Workflow is nil, which app/workflow.go documents as "every
// deployment that configures no hook scripts, and every use of this
// package written before EPIC L" -- and there is nothing here for a skip
// to skip.
//
// Three things follow, and the flag exists to say the first two out loud:
//
//   - Accepting it silently would be the worst available outcome. The
//     command would exit 0, no hook would have run, and the operator
//     would have learned that `fetch` normally runs hooks and that this
//     flag is how you stop it. Both halves of that are false, and the
//     belief is exactly what #813's audit trail exists to prevent: a flag
//     somebody added to a cron line months ago while everybody went on
//     believing the hooks ran.
//
//   - Implementing it here would mean bypassing something. The route
//     that really runs a set's hooks is the serving engine's per-set run
//     (core/service.SubmitRunBackupSet), which is a durable,
//     idempotency-keyed operation on that process's own single-flight
//     lock. Turning this command into a submission against that would
//     change what `fetch` IS -- its output, its exit status, its
//     --dry-run, and which process moves the bytes -- and every one of
//     those is pinned by FR-35. That is a piece of work with its own
//     issue, not a flag.
//
//   - So the flag is declared, refused with a sentence that says where a
//     bypass really lives, and exits 2. A refusal an operator can read is
//     worth having where a silent no-op is not.
//
// What --dry-run does instead is report the workflow a real run of this
// set WOULD execute, which is the question somebody reaching for the flag
// is usually actually asking. It resolves it from the configuration and
// executes nothing at all: see reportResolvedWorkflow below.
func cmdFetch(args []string) int {
	fs, cfgPath := newFlagSet("fetch")
	sourceFlag := fs.String("source", "", "the source to fetch (required unless --backup-set names it)")
	setFlag := fs.String("backup-set", "", "the backup set to fetch, named <source/backup-set> or with --source (required)")
	dryRun := fs.Bool("dry-run", false, "list what discovery would find, without transferring or recording anything")
	skipWorkflow := fs.Bool("skip-workflow-scripts", false,
		"refused here, and never ignored: this command's pass runs none of a backup set's hook scripts in the first place, so there is nothing to skip. See this command's own doc for why, and for where a real bypass is performed and recorded")
	// parseFlagsAroundOperands rather than a bare fs.Parse, and then a
	// refusal, because this command takes no operand at all: both of its
	// subjects arrive in flags. fs.Parse stops at the first argument that
	// is not a flag and leaves it in fs.Args() for somebody to read, and
	// nobody read it, so `fetch --source S --backup-set B prod/db` ran a
	// real cycle against S/B, moved real bytes and exited 0 without a word
	// about the third of the command line it dropped. That is issue #568's
	// defect one command over, and sharper here, because this one writes.
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) > 0 {
		return usageError("fetch takes no arguments; name the backup set with --backup-set %s", operands[0])
	}

	// Issue #569 was reported against `artifacts`, and this is the same
	// flag on the same binary: --backup-set takes the "source/backup-set"
	// id every surface that prints a backup set prints, so an operator can
	// paste the id they were just shown rather than splitting it by hand
	// and getting "no configured backup set named
	// production/production/nightly" for their trouble.
	//
	// The ambiguity half of #569 cannot arise here and nothing resolves
	// anything: this command has always needed a source, so the pair it
	// looks a backup set up by is exact either way. All that changes is
	// which of the two flags the source is allowed to arrive in, and a
	// --source naming a different one than the id does is refused as the
	// contradiction it is rather than one of them quietly winning.
	//
	// A value carrying a separator is either that id or it is not an id at
	// all, and the second one is a 2 here for the same reason the
	// contradiction below is: nothing about this deployment has to be read
	// to know it. It used to be cut at the FIRST separator and the
	// remainder handed to the service as a set name, so pasting an
	// artifact id in refused with "no configured backup set named
	// api-server/var-backups/alternatives.tar", about a deployment that
	// configures api-server/var-backups perfectly well, and an id with an
	// empty half fell through to "fetch requires --source and
	// --backup-set", told to an operator who had just passed --backup-set.
	// splitBackupSetID is the same shape rule `artifacts`, `retention`,
	// `backup-set` and `unconfigured clear` read this id with.
	source, set := *sourceFlag, *setFlag
	if named, bare, ok := splitBackupSetID(set); ok {
		if source != "" && source != named {
			return usageError("fetch: --backup-set %s names source %s, which --source %s contradicts", set, named, source)
		}
		source, set = named, bare
	} else if strings.Contains(set, "/") {
		return usageError("fetch: --backup-set %q is not a backup set id; a backup set id is exactly source/name, and an artifact id pasted whole has the file name on the end of it", set)
	}
	if source == "" || set == "" {
		return usageError("fetch requires --source and --backup-set")
	}

	// Refused before anything is opened, because the command line is what
	// is wrong and it is wrong on every deployment: no configuration has
	// to be read to know that this process runs no hooks. That is the
	// line usage()'s exit-code table draws between 2 and 1, and it is why
	// this is a usage refusal rather than a service one. It is settled
	// after the id above so the sentence can name the set the operator
	// meant rather than echoing two half-filled flags back at them.
	if *skipWorkflow {
		return usageError("fetch: --skip-workflow-scripts cannot be honoured by this command, and is refused rather than ignored. A fetch pass runs in your own shell and installs no workflow lifecycle, so it runs none of %s/%s's hook scripts and there is nothing here to skip. A bypass is an administrator action performed BY the process serving this deployment, which records it durably on the run row and in a warn-level event naming who asked, and %s workflow run list is where a bypassed run shows up. To see what a real run of this set would execute without running any of it, pass --dry-run, or run %s validate workflow %s/%s",
			source, set, cliecho.Binary, cliecho.Binary, source, set)
	}

	ctx := context.Background()
	svc, cfg, cleanup, err := openService(ctx, *cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	if !*dryRun {
		logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))
	}

	result, err := svc.Fetch(ctx, source, set, *dryRun)
	if err != nil {
		return fail(err)
	}

	if result.DryRun {
		stuck := 0
		for _, p := range result.Preview {
			known := ""
			if p.Known {
				known = "  (already known)"
				// Issue #662: "(already known)" is true of a settled
				// object and of one whose artifact this run will never
				// touch again, and it was the only thing said about
				// either. The state is the difference, and an operator
				// who has just been told the set is FAILING is reading
				// this line to find out why running it does nothing.
				//
				// The marker itself is left exactly as it was and the
				// state is appended, so anything reading this line for
				// that phrase still finds it and what is added is only
				// the part that was missing.
				if lifecycle.IsExceptionalState(lifecycle.State(p.State)) {
					known += " " + p.State + ": this run will not re-attempt it"
					stuck++
				}
			}
			fmt.Printf("%-60s %12d bytes%s\n", p.RemotePath, p.Size, known)
		}
		fmt.Printf("%d object(s) on the remote\n", len(result.Preview))
		if stuck > 0 {
			fmt.Printf("%d of them belong to artifacts a cycle will not attempt again: a real run would transfer nothing for these.\n", stuck)
			fmt.Println("`" + cliecho.Binary + " status` names them and what to run; `" + cliecho.Binary +
				" retry <artifact-id>` puts one back in the pipeline.")
		}
		reportResolvedWorkflow(cfg, source, set)

		return 0
	}

	fmt.Printf("discovered=%d already_known=%d pending=%d rejected=%d conflicts=%d errors=%d failed=%d\n",
		len(result.Discovery.Discovered), len(result.Discovery.AlreadyKnown), len(result.Discovery.Pending),
		len(result.Discovery.Rejected), len(result.Discovery.Conflicts), len(result.Discovery.Errors), result.FailedArtifacts)
	fmt.Printf("reconciliation: %d finding(s), %d error(s)\n", len(result.Reconcile.Findings), len(result.Reconcile.Errors))
	// failed counts artifacts that ended this call in FAILED, QUARANTINED
	// or QUARANTINED_LOST: either a this-cycle transfer/verify/commit
	// failure, or a previously-durable artifact reconciliation (above)
	// found rotten on its own and quarantined before this cycle's own
	// pipeline ever touched it. A reconciliation pass that successfully
	// finds and records rot returns no error at all, so nothing else
	// here would see it. Reading the exit status off the same count the
	// line above just printed is what issue #283 asks for: the exit code
	// and the reported number cannot drift apart, because they are the
	// same number.
	//
	// The verdict itself is built by internal/app, from the same fields
	// RunCycle fills in, and handed to the same cycleExit `run` calls
	// (setup.go). Issue #361 is why that is worth insisting on: the two
	// commands used to build their own arguments here, and they had
	// quietly grown two different definitions of a failed cycle. `fetch`
	// failed a whole cycle over a single per-candidate discovery error
	// that `run` correctly ignored, and ignored the per-artifact
	// reconcile errors `run` now shares with it. Neither difference was
	// deliberate and no test covered either.
	return cycleExit(os.Stderr, result.Verdict())
}

// reportResolvedWorkflow says what hooks a real run of this set would
// execute, and executes none of them.
//
// # Why --dry-run reports this at all
//
// Because --dry-run's promise is "what would a real run do", and since
// EPIC L a real run of a configured set does more than transfer: it runs
// a five-stage workflow around the pass, and a hook that quiesces a
// database is the most consequential thing in the whole invocation. A
// preview that listed the objects and said nothing about the stages was
// answering a narrower question than the flag asks.
//
// It is also the answer to the request --skip-workflow-scripts is
// refused for: an operator reaching for that flag wants to know what the
// hooks are before they run anything.
//
// # Why it resolves from the configuration and nothing else
//
// The stages are config.Config.WorkflowStagesFor's, which is
// workflow.PlanStages under a different name, so the ORDER printed here
// is the order a run would use rather than this file's opinion about
// unwinding. What it deliberately does not do is walk those directories:
// discovering, ordering, size-bounding and hashing the scripts is
// workflow.Snapshot's job, it captures bytes into a spool to do it, and
// `validate workflow` is the verb that performs exactly that and reports
// it. Re-deriving a plan here would be a second implementation that can
// disagree with the one a run uses, which is the failure
// core/service.ValidateWorkflow's own doc refuses to introduce.
//
// # Why it says this invocation runs none of them
//
// Because it does not, and nothing else on this screen would say so. See
// cmdFetch's own doc: the lifecycle is installed by core/service on the
// process that serves the deployment, and this command is not it. An
// operator reading a dry run that listed five stages would otherwise
// reasonably conclude that dropping --dry-run runs them.
func reportResolvedWorkflow(cfg *config.Config, source, set string) {
	if cfg == nil {
		return
	}
	if !cfg.WorkflowsConfigured() {
		fmt.Println("workflow: this deployment configures no workflow root, so a run of this backup set would run no hook scripts")

		return
	}

	bs := configuredBackupSetFor(cfg, source, set)
	stages := cfg.WorkflowStagesFor(bs)
	if len(stages) == 0 {
		fmt.Printf("workflow: neither this deployment nor %s/%s configures a stage directory, so a run of it would run no hook scripts\n", source, set)

		return
	}

	// The root is named beside the stages because a stage directory is
	// configured RELATIVE to it (config.Workflows.Root, and
	// workflow.Root.ResolveStage is what joins them), so a list of bare
	// names like "global-before" is not something an operator can go and
	// look at.
	fmt.Printf("workflow: a run of %s/%s would execute %d stage(s) under %s, in this order, with a %s bound per script:\n",
		source, set, len(stages), cfg.Workflows.Root, cfg.EffectiveScriptTimeout(bs))
	for _, st := range stages {
		fmt.Printf("  %-10s %-7s %s\n", st.Scope, st.Phase, st.Dir)
	}
	fmt.Println("  this dry run executed none of them, and neither does a fetch without --dry-run: hooks are run by the process serving this deployment, which is not this one")
	fmt.Printf("  `%s validate workflow %s/%s` lists the individual scripts, their order, their hashes and what is wrong with them, and runs no hook either\n",
		cliecho.Binary, source, set)
}

// configuredBackupSetFor finds the set the pass just previewed, so the
// stage resolution above can read its own overrides.
//
// A nil return is a set this configuration does not carry, which
// WorkflowStagesFor and EffectiveScriptTimeout both accept and read as
// "the deployment's values, with nothing pinned over them". That cannot
// happen on this path -- svc.Fetch has already looked the same set up and
// failed if it was not there -- and returning nil rather than refusing
// keeps this a reporting function: a preview that had already printed
// every object on the remote must not end in an error about a lookup
// nobody asked for.
func configuredBackupSetFor(cfg *config.Config, source, set string) *config.BackupSet {
	for si := range cfg.Sources {
		if cfg.Sources[si].Name != source {
			continue
		}
		for bi := range cfg.Sources[si].BackupSets {
			if cfg.Sources[si].BackupSets[bi].Name == set {
				return &cfg.Sources[si].BackupSets[bi]
			}
		}
	}

	return nil
}
