package metrics_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/health"
	"github.com/retnd/retnd/core/internal/metrics"
	"github.com/retnd/retnd/core/internal/model"
)

// EPIC K's "metrics never leak secrets" requirement (#788), checked as
// the thing it actually is: what ends up between the braces.
//
// A scrape endpoint is the least protected surface this product has. It
// is polled by an agent, stored for months in somebody's time-series
// database, rendered on dashboards that get screenshotted, and exported
// into support tickets. A label value that carried a source path, an
// endpoint, a bucket name or any part of a credential would be in all
// four places, permanently, and no amount of care later takes it back
// out.
//
// The check is on LABELS specifically, not on the whole output, because
// the help text is written here and the sample values are numbers: the
// only place a caller-supplied string can reach this rendering is a label
// value, so that is where the guard belongs and where it can be complete.

// labelValues extracts every label value in a rendered exposition text.
var labelPairs = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="([^"]*)"`)

// secretsAndPaths is a fixture whose every string field is something that
// must never be scraped, each distinctive enough that finding it in the
// output is unambiguous.
//
// The engine-side strings are the interesting half. A repository domain
// is an operator-chosen name and is safe; a repository PATH, a snapshot
// id's storage location, a verification finding naming a file in
// somebody's source tree and a maintenance error carrying a bucket URL
// are not, and every one of them is a value that exists inside this
// product one layer below the health report.
var secretsAndPaths = []string{
	"/srv/nas/volume1/customer-data",
	"s3://acme-prod-backups/tenant-4471",
	"AKIAIOSFODNN7EXAMPLE",
	"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	"hunter2-repository-passphrase",
	"backup-agent@prod-db-01.internal",
	"/home/alice/.ssh/id_ed25519",
}

// workflowSecretsAndPaths is the same fixture for EPIC L's workflow half
// (#813), whose strings are a different shape of the same hazard.
//
// A workflow is made almost entirely of things an operator named: a
// socket path, a hook's filename, a step id minted per run, a run id, a
// runner credential. None of them is a dimension anybody would want to
// group a time series by, and every one of them is a value that exists
// one layer below this package and reaches it through a Detail sentence
// or a recovery hold.
var workflowSecretsAndPaths = []string{
	"/var/run/backupd/host-workflow-runner.sock",
	"dump-pgsql.remote.sh",
	"0007~set~before~dump-pgsql.remote.sh",
	"run-2026-09-13T02:00:03Z-7f21",
	"Tr0ub4dor&3-host-runner-token",
}

func labelledReport(t *testing.T) health.Report {
	t.Helper()

	set, err := model.NewBackupSetID("production", "uploads-tree")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	// Every string this report can carry, filled with something that
	// must not be scraped. If any of them ever reaches a label, this
	// test says which one.
	drill := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	report := health.NewReport(
		health.NewProcessHealth(health.ProcessInputs{
			BinaryVersion: "1.2.3",
			RcloneVersion: "1.66.0",
		}),
		[]health.BackupSetHealth{{
			Set:   set,
			State: health.Degraded,
			Snapshot: &health.SnapshotHealth{
				RunID:      "run_" + secretsAndPaths[0],
				SnapshotID: "k" + secretsAndPaths[1],
				Phase:      "SUCCESS",

				EntriesScanned:         9,
				Files:                  4,
				LogicalBytes:           4096,
				SourceBytesRead:        2048,
				RepositoryBytesWritten: 512,
				ContentReusedBytes:     1536,
				Measured:               true,
				Duration:               90 * time.Second,

				VerificationStatus:   "failed",
				VerificationLevel:    "content_sample",
				VerificationAchieved: secretsAndPaths[2],
				VerificationFailed:   true,

				LastKnownGoodAt: &drill,
				UnfinishedRuns:  1,
			},
		}},
		time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	)

	// The workflow half, filled the same way. Every string it carries
	// that is not a declared connection id or a version is something
	// that must never be scraped, and all of them are genuinely
	// reachable from here: a Detail sentence is the field a careless
	// implementation fills with the dialer's own error text, which has
	// the socket path in it, and a recovery hold carries the run id an
	// operator is going to be told to name.
	report.Workflow = health.WorkflowHealth{
		Configured: true,
		Runner: health.WorkflowRunnerHealth{
			Configured: true,
			Reachable:  false,
			Version:    "1.4.0",

			// Neither of these is rendered, and that is the
			// assertion: a bash version is a line of text a
			// third-party binary chose the shape of, so if it ever
			// becomes a label the whitelist below fails on it.
			BashVersion: "GNU bash, version 5.2.15(1)-release",
			Detail: "the runner did not answer on " + workflowSecretsAndPaths[0] +
				" (token " + workflowSecretsAndPaths[4] + ")",
		},
		ExecConnections: []health.WorkflowExecConnectionHealth{
			{
				Ref:     "db-hooks",
				Capable: false,
				Detail:  "could not prove bash for " + secretsAndPaths[5] + " and " + workflowSecretsAndPaths[1],
			},
			{Ref: "nas-hooks", Capable: true},
		},
		RecoveryHolds: []health.WorkflowRecoveryHold{{
			RunID:     workflowSecretsAndPaths[3],
			BackupSet: set,
			Scope:     "global",
			EnteredAt: drill,
		}},
	}

	return report
}

// labelledWorkflow drives a counter set across every dimension the seven
// workflow families (#813) have, from two backup sets.
//
// None of the canaries above appears in it, and that absence is the
// design rather than an omission: metrics.WorkflowRun and
// metrics.WorkflowStep have no field a script name, a step id or a run
// id could be passed through in the first place, which is held
// structurally by TestWorkflowObservationsCarryNoScriptNameStepIDOrRunID
// in workflow_test.go. What this fixture is for is the other half of the
// guard: rendering every combination those types CAN express, so the
// whitelist below is asserted against a fully loaded counter set rather
// than against whichever three series a smaller fixture happened to
// produce.
func labelledWorkflow(t *testing.T) *metrics.Workflow {
	t.Helper()

	var w metrics.Workflow

	// The vocabularies, as literals. This package deliberately imports
	// no engine (see metrics.WorkflowRun's doc), and a test that
	// imported internal/workflow to build them would be the first
	// thing to take that dependency back. Holding these words against
	// the engine's own constants is core/service's job, which is the
	// layer that already imports both.
	for _, set := range []string{"production/uploads-tree", "staging/archive"} {
		for _, status := range []string{"unknown", "running", "success", "failed", "skipped"} {
			for _, bypassed := range []bool{false, true} {
				w.ObserveRun(metrics.WorkflowRun{
					BackupSet: set,
					Status:    status,
					Bypassed:  bypassed,
					Duration:  90 * time.Second,
				})
			}
		}

		for _, scope := range []string{"global", "set"} {
			for _, phase := range []string{"before", "after"} {
				for _, target := range []string{"local", "remote"} {
					for _, state := range []string{"pending", "running", "success", "failed", "timed_out", "canceled", "skipped", "interrupted"} {
						for _, disposition := range []string{"exited", "timed_out", "canceled", "transport_lost", "not_attempted", "signaled"} {
							w.ObserveStep(metrics.WorkflowStep{
								BackupSet:   set,
								Scope:       scope,
								Phase:       phase,
								Target:      target,
								State:       state,
								Disposition: disposition,
								Duration:    3 * time.Second,
							})
						}
					}
				}
			}
		}
	}

	for _, target := range []string{"local", "remote"} {
		w.ObserveLogTruncation(target)
	}

	return &w
}

// allowedReportLabels is every label value health.Report's rendering may
// publish: a backup set id, a health state, this process's own build
// strings, the workflow runner's version, and the ids of the execution
// connections the configuration declares.
func allowedReportLabels() map[string]bool {
	allowed := map[string]bool{
		"production/uploads-tree": true,
		"1.2.3":                   true,
		"1.66.0":                  true,

		// The runner's product version. A version is a dimension of
		// the reachability reading (see writeWorkflowRunner), and it
		// is a string this product's own release produced.
		"1.4.0": true,

		// Declared execution connection ids. They are free text, and
		// they are bounded by the configuration file an operator has
		// to edit and restart to change -- the same bound backup_set
		// has, and the reason this label is allowed where a script
		// name is not.
		"db-hooks":  true,
		"nas-hooks": true,
	}
	for _, state := range []health.State{health.Healthy, health.Degraded, health.Stale, health.Failing} {
		allowed[strings.ToLower(state.String())] = true
	}

	// Deliberately no entry for the empty string. A runner that never
	// answered has no version, and the fixture gives it one anyway
	// precisely so that this whitelist stays a list of real values: an
	// allowance for "" would pass any label whose source string
	// happened to be unset, which is the one hole a whitelist can have.

	return allowed
}

// allowedWorkflowLabels is every label value the seven workflow counter
// families may publish: two backup set ids, the closed vocabularies, and
// the two spellings of a boolean.
func allowedWorkflowLabels() map[string]bool {
	allowed := map[string]bool{
		"production/uploads-tree": true,
		"staging/archive":         true,

		"true":  true,
		"false": true,
	}
	for _, word := range []string{
		"global", "set",
		"before", "after",
		"local", "remote",
		"unknown", "running", "success", "failed", "skipped",
		"pending", "timed_out", "canceled", "interrupted",
		"exited", "transport_lost", "not_attempted", "signaled",
	} {
		allowed[word] = true
	}

	return allowed
}

// TestRender_NoLabelCarriesASecretOrAPath is the requirement.
//
// It asserts on the label values rather than on the body, and it asserts
// a whitelist rather than a blacklist of the specific strings above:
// "none of these seven appear" would pass the day somebody adds an eighth
// field and renders it. What a scrape may carry is a backup set's id, a
// health state, this process's own build strings, a declared connection
// id, a version, one of the workflow vocabularies and a bucket bound, and
// every one of those is a value this product chose rather than one it
// read off a source.
//
// Both renderings are covered, because they are two functions with two
// label sets and a guard on one of them would say nothing about the
// other: Render turns an already-computed health.Report into gauges, and
// RenderWorkflow turns a live counter set into the seven workflow
// families.
func TestRender_NoLabelCarriesASecretOrAPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		what     string
		rendered string
		allowed  map[string]bool

		// canaries are the fixture strings that have to be genuinely
		// reachable from the input, so that the whitelist above is a
		// statement about this renderer rather than about an empty
		// value. The counter set has none: the observation types have
		// nowhere to put one, which is a stronger property and a
		// different test.
		canaries []string
	}{{
		what:     "the health report",
		rendered: metrics.Render(labelledReport(t)),
		allowed:  allowedReportLabels(),
		canaries: append(append([]string(nil), secretsAndPaths[:3]...), workflowSecretsAndPaths...),
	}, {
		what:     "the workflow counters",
		rendered: labelledWorkflow(t).RenderWorkflow(),
		allowed:  allowedWorkflowLabels(),
	}} {
		t.Run(tc.what, func(t *testing.T) {
			seen := 0
			for _, match := range labelPairs.FindAllStringSubmatch(tc.rendered, -1) {
				name, value := match[1], match[2]
				seen++

				// A bucket bound comes from this package's own
				// ladder and never from anything a caller said, so
				// it is checked for being a number rather than
				// listed as a permitted string.
				if name == "le" {
					if _, err := strconv.ParseFloat(value, 64); err != nil && value != "+Inf" {
						t.Errorf(`a histogram bucket carries le=%q, which is neither a number nor "+Inf"`, value)
					}

					continue
				}

				if tc.allowed[value] {
					continue
				}

				t.Errorf("%s carries %s=%q, which is not one of the values a scrape may publish (a backup set id, a health state, this process's own build strings, a declared connection id, a version, or one of the workflow vocabularies).\n"+
					"A metric label is stored for months, rendered on dashboards and pasted into support tickets, so anything that reaches one is public forever.\n"+
					"Either the value belongs in the metric's HELP text, or it does not belong in this package at all.", tc.what, name, value)
			}

			if seen == 0 {
				t.Fatal("no label was rendered at all, so this test proves nothing about what labels may carry")
			}

			for _, secret := range tc.canaries {
				if strings.Contains(tc.rendered, secret) {
					t.Errorf("the rendered scrape contains %q anywhere at all, label or not", secret)
				}
			}
		})
	}
}
