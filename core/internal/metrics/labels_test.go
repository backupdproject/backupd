package metrics_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/metrics"
	"github.com/backupdproject/backupd/core/internal/model"
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

	return health.NewReport(
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
}

// TestRender_NoLabelCarriesASecretOrAPath is the requirement.
//
// It asserts on the label values rather than on the body, and it asserts
// a whitelist rather than a blacklist of the specific strings above:
// "none of these seven appear" would pass the day somebody adds an eighth
// field and renders it. What a scrape may carry is a backup set's id, a
// health state, and this process's own build strings, and every one of
// those is a value this product chose rather than one it read off a
// source.
func TestRender_NoLabelCarriesASecretOrAPath(t *testing.T) {
	t.Parallel()

	rendered := metrics.Render(labelledReport(t))

	allowed := map[string]bool{
		"production/uploads-tree": true,
		"1.2.3":                   true,
		"1.66.0":                  true,
	}
	for _, state := range []health.State{health.Healthy, health.Degraded, health.Stale, health.Failing} {
		allowed[strings.ToLower(state.String())] = true
	}

	seen := 0
	for _, match := range labelPairs.FindAllStringSubmatch(rendered, -1) {
		name, value := match[1], match[2]
		seen++

		if allowed[value] {
			continue
		}

		t.Errorf("the scrape carries %s=%q, which is not one of the values a scrape may publish (a backup set id, a health state, or this process's own build strings).\n"+
			"A metric label is stored for months, rendered on dashboards and pasted into support tickets, so anything that reaches one is public forever.\n"+
			"Either the value belongs in the metric's HELP text, or it does not belong in this package at all.", name, value)
	}

	if seen == 0 {
		t.Fatal("no label was rendered at all, so this test proves nothing about what labels may carry")
	}

	// The control: the fixture's secrets have to be genuinely reachable
	// from the report, or the assertion above is a statement about an
	// empty report rather than about this renderer.
	for _, secret := range secretsAndPaths[:3] {
		if strings.Contains(rendered, secret) {
			t.Errorf("the rendered scrape contains %q anywhere at all, label or not", secret)
		}
	}
}
