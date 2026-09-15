package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/state"
)

// EPIC K's read verbs and the two toggles, driven as an operator drives
// them (#788): a real configuration, a real journal, and the words this
// binary prints.
//
// The measurement words are the ones worth a test of their own. A
// snapshot row carries NULLABLE counters, because a run that failed
// before it scanned anything measured nothing, and the difference
// between "measured zero" and "not measured" is the difference between a
// source that is empty and a run that never looked. A renderer that
// printed 0 for both would report an empty source every time a scan
// crashed.

// incrementalSetUUID is the lineage key the fixture below declares, fixed
// rather than minted so a seeded snapshot run can be keyed on the same
// value the configuration names.
const incrementalSetUUID = "6f1d2b7a-1c4e-4f8b-9a2d-3e5c7b9d1f00"

// aDeploymentWithSnapshots is one configuration declaring a repository
// domain and one incremental set in it, with a journal alongside.
func aDeploymentWithSnapshots(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	passphrase := filepath.Join(dir, "repo.passphrase")
	if err := os.WriteFile(passphrase, []byte("a-passphrase-long-enough-to-be-a-passphrase"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"incremental_engine:\n  enabled: true\n" +
		"repository_domains:\n" +
		"  - id: production\n" +
		"    description: Snapshots for this deployment\n" +
		"    isolation: shared\n" +
		"    passphrase:\n" +
		"      file: " + passphrase + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: uploads-tree\n" +
		"        engine: kopia\n" +
		"        uuid: " + incrementalSetUUID + "\n" +
		"        repository_domain: production\n" +
		"        source_consistency: external_snapshot\n" +
		"        verification_level: content_sample\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + tree + "\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// seedSnapshotRun puts one run in the journal and leaves every counter
// unset, which is what a run that has not scanned anything yet looks
// like.
func seedSnapshotRun(t *testing.T, configPath, runID string) {
	t.Helper()

	dbPath := filepath.Join(filepath.Dir(configPath), "state.db")
	j, err := state.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() {
		if err := j.Close(); err != nil {
			t.Errorf("closing the journal: %v", err)
		}
	}()

	setID, err := model.NewBackupSetID("production", "uploads-tree")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	if _, err := j.BeginSnapshotRun(t.Context(), state.SnapshotRunRequest{
		RunID:             runID,
		IdempotencyKey:    "seed:" + runID,
		Set:               setID,
		SetUUID:           incrementalSetUUID,
		Engine:            string(model.EngineKopia),
		Domain:            "production",
		SourceIdentity:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ConsistencyMode:   "external_snapshot",
		VerificationLevel: "content_sample",
		StartedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("BeginSnapshotRun: %v", err)
	}
}

func TestSnapshotShow_ARunThatMeasuredNothingSaysSoRatherThanPrintingZero(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)
	seedSnapshotRun(t, configPath, "run_seeded_1")

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"snapshot", "--config", configPath, "show", "production/uploads-tree", "run_seeded_1"})
	})
	if code != exitOK {
		t.Fatalf("snapshot show = %d, want %d; it printed %q", code, exitOK, stdout)
	}

	// Every counter on this row is NULL, so every one of these lines has
	// to say it was not measured. A 0 here would read as a source with no
	// files in it.
	for _, line := range []string{
		"entries scanned: not measured",
		"files:           not measured",
		"directories:     not measured",
		"logical size:    not measured",
		"written:         not measured",
		"reused:          not measured",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("the report does not carry %q, so an unmeasured counter is being printed as a measurement:\n%s", line, stdout)
		}
	}
	if strings.Contains(stdout, "files:           0") {
		t.Errorf("the report prints 0 for a counter the run never took:\n%s", stdout)
	}
}

// A hold with no reason is refused before anything is submitted. The
// refusal is a usage error rather than a service one because nothing
// about it depends on the deployment: a hold nobody explained is one
// nobody dares release, which makes it permanent by accident.
func TestSnapshotHold_RefusesAHoldNobodyExplained(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)

	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int {
			return run([]string{"snapshot", "--config", configPath, "hold", "production/uploads-tree"})
		})
	})
	if code != exitUsage {
		t.Fatalf("snapshot hold with no --reason = %d, want %d\nstderr: %s", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "--reason is required") {
		t.Errorf("the refusal does not say which flag is missing:\n%s", stderr)
	}
}

// `repository health` renders the verdict for every declared domain,
// rather than only the ones something has already written to. A
// deployment that declares a domain and has never snapshotted into it is
// exactly the state an operator is in the day they configure one, and a
// report that said nothing then would be a report nobody trusts later.
func TestRepositoryHealth_RendersTheVerdictForADeclaredDomain(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)

	stdout := captureStdout(t, func() {
		// The exit code IS the verdict on this verb, so it is not
		// asserted here: what this case is about is that a declared
		// domain is reported at all, in the words an operator reads.
		run([]string{"repository", "--config", configPath, "health"})
	})

	if strings.Contains(stdout, "declares no repository domain") {
		t.Fatalf("the report says this deployment declares no domain, and its configuration declares one:\n%s", stdout)
	}
	for _, want := range []string{
		"production",
		"credentials:",
		"clock:",
		"maintenance:",
		"last snapshot:",
		"last verified:",
		"backup set:      production/uploads-tree",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the verdict does not carry %q:\n%s", want, stdout)
		}
	}
}

// The enabled toggle, end to end on the direct route: it persists, and it
// refuses a word that is not on or off.
func TestBackupSetEnabled_PersistsAndTakesOnlyTwoWords(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)
	const id = "production/uploads-tree"

	for _, tc := range []struct {
		word         string
		wantDisabled bool
	}{
		{"off", true},
		{"on", false},
	} {
		var code int
		stdout := captureStdout(t, func() {
			code = run([]string{"backup-set", "--config", configPath, "enabled", id, tc.word})
		})
		if code != exitOK {
			t.Fatalf("backup-set enabled %s = %d, want %d; it printed %q", tc.word, code, exitOK, stdout)
		}
		if got := strings.Contains(readFile(t, configPath), "disabled: true"); got != tc.wantDisabled {
			t.Errorf("after `enabled %s` the configuration says disabled: %v, want %v:\n%s",
				tc.word, got, tc.wantDisabled, readFile(t, configPath))
		}
	}

	// A third word, and the refusal that names the only two. true/false,
	// yes/no and 1/0 are all refused deliberately: a toggle that accepts
	// six spellings is one whose scripts each pick a different one.
	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int {
			return run([]string{"backup-set", "--config", configPath, "enabled", id, "true"})
		})
	})
	if code != exitUsage {
		t.Fatalf("backup-set enabled true = %d, want %d\nstderr: %s", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "on or off") {
		t.Errorf("the refusal does not name the two words this verb takes:\n%s", stderr)
	}
}

// Turning read-only OFF is asking this manager to start deleting a
// source's originals, so it is the one direction that has to be earned:
// the source is asked whether these credentials may write there, and a
// source that says no is refused (#852).
//
// Turning it ON is the control. A set that never deletes needs no
// permission to delete, so the same source that refused above must not
// stop an operator making the set SAFER.
func TestBackupSetReadOnly_TurningItOffNeedsASourceThatCanBeWrittenTo(t *testing.T) {
	configPath := aDeploymentWithSnapshots(t)
	const id = "production/uploads-tree"

	captureStdout(t, func() {
		if code := run([]string{"backup-set", "--config", configPath, "read-only", id, "on"}); code != exitOK {
			t.Fatalf("backup-set read-only on = %d, want %d; making a set safer must not need the source's permission", code, exitOK)
		}
	})
	if !strings.Contains(readFile(t, configPath), "read_only: true") {
		t.Fatalf("`read-only on` did not persist:\n%s", readFile(t, configPath))
	}

	// The source, made unwritable: the tree can be listed and nothing
	// can be created in it, which is exactly the posture an operator's
	// read-only export has.
	tree := filepath.Join(filepath.Dir(configPath), "tree")
	if err := os.Chmod(tree, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(tree, 0o755) })

	var code int
	stderr := captureStderr(t, func() {
		code = captureStdoutCode(t, func() int {
			return run([]string{"backup-set", "--config", configPath, "read-only", id, "off"})
		})
	})
	if code != exitFailure {
		t.Fatalf("backup-set read-only off against a source that cannot be written to = %d, want %d\nstderr: %s", code, exitFailure, stderr)
	}
	// The refusal names what the source said and what to do about it,
	// which is the half that makes it actionable: skip_connection_check
	// is deliberately not a way past this one.
	if !strings.Contains(stderr, "cannot write to it") {
		t.Errorf("the refusal does not say the source refused the write, so an operator cannot tell it from an unreachable one:\n%s", stderr)
	}
	if strings.Contains(readFile(t, configPath), "read_only: false") {
		t.Errorf("the refused command still cleared the set's read-only declaration:\n%s", readFile(t, configPath))
	}
}
