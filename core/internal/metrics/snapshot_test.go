package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/metrics"
	"github.com/backupdproject/backupd/core/internal/model"
)

// EPIC K's metrics requirement, checked as the thing it actually is: four
// numbers a scrape can tell apart. The failure it guards against is not a
// missing metric, it is a plausible one -- a single "bytes backed up"
// series that reads as an upload of the whole source every night.

func snapshotReport(t *testing.T, snap *health.SnapshotHealth) health.Report {
	t.Helper()

	set, err := model.NewBackupSetID("production", "uploads-tree")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}

	return health.NewReport(
		health.NewProcessHealth(health.ProcessInputs{BinaryVersion: "test", RcloneVersion: "test"}),
		[]health.BackupSetHealth{{Set: set, State: health.Healthy, Snapshot: snap}},
		time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
	)
}

func sampleValue(t *testing.T, rendered, metric string) (string, bool) {
	t.Helper()

	prefix := "backupd_backup_set_" + metric + "{"

	for line := range strings.Lines(rendered) {
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("sample %q is not a name/value pair", line)
		}

		return fields[1], true
	}

	return "", false
}

func TestRender_ScannedReadWrittenAndReusedAreFourDistinctSeries(t *testing.T) {
	t.Parallel()

	rendered := metrics.Render(snapshotReport(t, &health.SnapshotHealth{
		RunID:      "run-1",
		SnapshotID: "snap-1",
		Phase:      "SUCCESS",

		EntriesScanned:         12,
		LogicalBytes:           100_000,
		SourceBytesRead:        100_000,
		RepositoryBytesWritten: 4_000,
		ContentReusedBytes:     96_000,
		Measured:               true,
	}))

	want := map[string]string{
		"snapshot_entries_scanned":          "12",
		"snapshot_logical_bytes":            "100000",
		"snapshot_source_bytes_read":        "100000",
		"snapshot_repository_bytes_written": "4000",
		"snapshot_content_reused_bytes":     "96000",
	}

	for metric, value := range want {
		got, ok := sampleValue(t, rendered, metric)
		if !ok {
			t.Errorf("%s has no sample; a deduplicated run that cannot be distinguished from a full upload is the defect this metric exists for", metric)

			continue
		}

		if got != value {
			t.Errorf("%s = %s, want %s", metric, got, value)
		}
	}

	// The distinctness itself: written must not be rendered from the
	// logical size. A renderer that read the wrong field would still
	// produce five series and pass every existence check above.
	written, _ := sampleValue(t, rendered, "snapshot_repository_bytes_written")
	logical, _ := sampleValue(t, rendered, "snapshot_logical_bytes")

	if written == logical {
		t.Errorf("repository_bytes_written and logical_bytes both render as %s; the scrape cannot tell a 100 GB snapshot from a 100 GB upload", written)
	}
}

func TestRender_ASetThatTakesNoSnapshotsHasNoSnapshotSeriesAtAll(t *testing.T) {
	t.Parallel()

	rendered := metrics.Render(snapshotReport(t, nil))

	for _, metric := range []string{
		"snapshot_entries_scanned",
		"snapshot_logical_bytes",
		"snapshot_source_bytes_read",
		"snapshot_repository_bytes_written",
		"snapshot_content_reused_bytes",
		"snapshot_unfinished_runs",
		"snapshot_last_known_good_timestamp_seconds",
	} {
		if _, ok := sampleValue(t, rendered, metric); ok {
			t.Errorf("%s has a sample for an artifact set; a fabricated zero reads as a real reading", metric)
		}
	}
}

func TestRender_AnUnmeasuredRunReportsNoBytesRatherThanZeroBytes(t *testing.T) {
	t.Parallel()

	// A snapshot adopted by crash reconciliation: the manifest exists,
	// the process that would have counted the bytes died.
	rendered := metrics.Render(snapshotReport(t, &health.SnapshotHealth{
		RunID:          "run-1",
		SnapshotID:     "snap-1",
		Phase:          "SUCCESS",
		Measured:       false,
		UnfinishedRuns: 0,
	}))

	for _, metric := range []string{
		"snapshot_source_bytes_read",
		"snapshot_repository_bytes_written",
		"snapshot_content_reused_bytes",
	} {
		if got, ok := sampleValue(t, rendered, metric); ok {
			t.Errorf("%s = %s for a run nobody measured; an unmeasured counter must have no series", metric, got)
		}
	}

	// The run itself is still visible: a set whose newest snapshot was
	// adopted rather than measured has not stopped having snapshots.
	if _, ok := sampleValue(t, rendered, "snapshot_unfinished_runs"); !ok {
		t.Error("snapshot_unfinished_runs has no sample; zero unfinished runs is a real and important reading")
	}
}
