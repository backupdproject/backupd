package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// EPIC K's incremental sets, from a terminal (#788).
//
// Until this issue there was no way to make one: the engine, the
// repository domain and the whole verification budget were fields of
// service.CreateBackupSetRequest that no flag filled in, so `backup-set
// create` could only ever write an artifact set, and `backup-set patch`
// could not revise a budget at all. Everything an operator could not
// type here was reachable only from the wizard, which is the exact
// divergence suites/equivalence exists to catch.
//
// These drive the DIRECT route with nothing serving, so what they assert
// is the configuration file afterwards: a flag that reached a request and
// not the file would leave every screen showing the defaults.

// aDeploymentWithARepositoryDomain is writeTestConfig plus the one thing
// an incremental set cannot be created without: a declared domain, with
// the passphrase source every domain a set names has to have.
func aDeploymentWithARepositoryDomain(t *testing.T) string {
	t.Helper()
	configPath := writeTestConfig(t)

	passphrase := filepath.Join(filepath.Dir(configPath), "repo.passphrase")
	if err := os.WriteFile(passphrase, []byte("a-passphrase-long-enough-to-be-a-passphrase"), 0o600); err != nil {
		t.Fatalf("writing the repository passphrase file: %v", err)
	}

	existing := readFile(t, configPath)
	const anchor = "sources:\n"
	if !strings.Contains(existing, anchor) {
		t.Fatalf("the fixture configuration declares no sources block, so this helper cannot put a repository domain in front of it:\n%s", existing)
	}
	domains := "repository_domains:\n" +
		"  - id: production\n" +
		"    description: Snapshots for this deployment\n" +
		"    isolation: shared\n" +
		"    passphrase:\n" +
		"      file: " + passphrase + "\n"
	if err := os.WriteFile(configPath, []byte(strings.Replace(existing, anchor, domains+anchor, 1)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// incrementalCreateArgs is createArgs for a kopia set: the artifact
// pipeline's own keys are left off, because config.Validate refuses
// local_path and a completion strategy on a set with no finished file to
// recognise.
func incrementalCreateArgs(configPath, keyPath, id string, extra ...string) []string {
	args := []string{
		"backup-set", "--config", configPath, "create", id,
		"--host", "source.example.internal",
		"--port", "2222",
		"--user", "backupuser",
		"--ssh-key-file", keyPath,
		"--known-hosts-line", aKnownHostsLine,
		"--remote-path", "/srv/uploads",
		"--engine", "kopia",
		"--repository-domain", "production",
		"--no-verify",
	}
	return append(args, extra...)
}

func TestBackupSetCreate_WritesAnIncrementalSetWithItsWholeBudget(t *testing.T) {
	configPath := aDeploymentWithARepositoryDomain(t)
	keyPath := writeTestPrivateKey(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run(incrementalCreateArgs(configPath, keyPath, "api/uploads",
			"--source-consistency", "external_snapshot",
			"--verification-level", "content_sample",
			"--verification-sample-percent", "15",
			"--verification-full-every", "168h",
			"--verification-restore-drill-every", "720h"))
	})
	if code != exitOK {
		t.Fatalf("an incremental create exited %d, want %d; it printed %q", code, exitOK, stdout)
	}

	written := readFile(t, configPath)
	for _, want := range []string{
		"engine: kopia",
		"repository_domain: production",
		"source_consistency: external_snapshot",
		"verification_level: content_sample",
		"verification_sample_percent: 15",
		"verification_full_every: 168h",
		"verification_restore_drill_every: 720h",
	} {
		if !strings.Contains(written, want) {
			t.Errorf("the configuration this create wrote does not carry %q, so the flag reached a request and not the file:\n%s", want, written)
		}
	}

	// The lineage key, which no flag names and the service mints, because
	// a durable identifier an operator typed is one they can mistype.
	if !strings.Contains(written, "uuid:") {
		t.Errorf("the incremental set carries no uuid, so its snapshot lineage hangs off nothing:\n%s", written)
	}
}

func TestBackupSetPatch_RevisesTheVerificationBudget(t *testing.T) {
	configPath := aDeploymentWithARepositoryDomain(t)
	keyPath := writeTestPrivateKey(t)

	captureStdout(t, func() {
		if code := run(incrementalCreateArgs(configPath, keyPath, "api/uploads")); code != exitOK {
			t.Fatalf("seeding an incremental set exited %d", code)
		}
	})

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"backup-set", "--config", configPath, "patch", "api/uploads",
			"--source-consistency", "externally_quiesced",
			"--verification-level", "content_full",
			"--verification-sample-percent", "40",
			"--verification-full-every", "24h",
			"--verification-restore-drill-every", "168h"})
	})
	if code != exitOK {
		t.Fatalf("a patch of the verification budget exited %d, want %d; it printed %q", code, exitOK, stdout)
	}

	written := readFile(t, configPath)
	for _, want := range []string{
		"source_consistency: externally_quiesced",
		"verification_level: content_full",
		"verification_sample_percent: 40",
		"verification_full_every: 24h",
		"verification_restore_drill_every: 168h",
	} {
		if !strings.Contains(written, want) {
			t.Errorf("the patch did not persist %q:\n%s", want, written)
		}
	}
}

// The two create-only declarations are refused on patch rather than
// ignored, which is the rule every create-only flag on this verb keeps.
// It matters more here than for --disabled: changing the engine or the
// repository domain of a set that already has snapshots would fork the
// lineage rather than edit it, and an edit that exited 0 having done
// nothing would read as one that had.
func TestBackupSetPatch_RefusesTheDeclarationsThatFixWhatASetIs(t *testing.T) {
	configPath := aDeploymentWithARepositoryDomain(t)

	for _, flag := range [][]string{
		{"--engine", "kopia"},
		{"--repository-domain", "production"},
	} {
		args := append([]string{"backup-set", "--config", configPath, "patch", cliSet}, flag...)
		var code int
		stderr := captureStderr(t, func() {
			code = captureStdoutCode(t, func() int { return run(args) })
		})
		if code != exitUsage {
			t.Errorf("`backup-set patch %s` exited %d, want %d; exiting 0 would report success for a declaration nothing changed\nstderr: %s",
				flag[0], code, exitUsage, stderr)
		}
	}
}
