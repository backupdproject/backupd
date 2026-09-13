package main

import (
	"strings"
	"testing"
)

// `backup-set enabled` and `backup-set read-only` against a real route.
//
// Both verbs rewrite config.yaml, so #538 refuses them beside a process
// that is serving this deployment, and until #788 there was nothing on
// the other side of that refusal: the two came through
// openBackupService, which has no route, so an operator running a real
// engine could not enable a set from a terminal at all. That is the same
// position `backup-set patch` was in before #543, and the same answer:
// the write is handed to the process that will serve it.
//
// The fake engine serves these off the real BackupService, so what is
// asserted here is the engine's own configuration afterwards rather than
// what this command decided to print.

func TestBackupSetEnabled_ReachesTheEngineThatIsServing(t *testing.T) {
	cliConfig, e := oneServedDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"backup-set", "--config", cliConfig, "enabled", "production/postgres-primary", "off"})
	})
	if code != exitOK {
		t.Fatalf("backup-set enabled off against a served engine = %d, want %d; %d is the refusal for a write with no route, and this one has one\nstdout: %s",
			code, exitOK, exitEngineHoldsDeployment, stdout)
	}
	if !strings.Contains(stdout, "disabled") {
		t.Errorf("the command does not say the set is disabled now:\n%s", stdout)
	}

	// Asked of the ENGINE. A command that printed its own request back
	// would pass everything above while the serving process went on
	// offering the set to every cycle.
	set, err := e.svc.GetBackupSet(t.Context(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("reading the set back off the engine: %v", err)
	}
	if !set.Disabled {
		t.Error("the engine still has this backup set enabled, so the write reached nothing that will act on it")
	}

	// And back on, because a toggle that only goes one way is half a
	// verb: disabling from a terminal and having to enable in a browser
	// is the gap this closes.
	captureStdout(t, func() {
		code = run([]string{"backup-set", "--config", cliConfig, "enabled", "production/postgres-primary", "on"})
	})
	if code != exitOK {
		t.Fatalf("backup-set enabled on against a served engine = %d, want %d", code, exitOK)
	}
	if set, err = e.svc.GetBackupSet(t.Context(), "production/postgres-primary"); err != nil {
		t.Fatalf("reading the set back off the engine: %v", err)
	}
	if set.Disabled {
		t.Error("the engine still has this backup set disabled after `enabled on`")
	}
}

func TestBackupSetReadOnly_ReachesTheEngineThatIsServing(t *testing.T) {
	cliConfig, e := oneServedDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"backup-set", "--config", cliConfig, "read-only", "production/postgres-primary", "on"})
	})
	if code != exitOK {
		t.Fatalf("backup-set read-only on against a served engine = %d, want %d\nstdout: %s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "read-only") {
		t.Errorf("the command does not say what posture the set is in now:\n%s", stdout)
	}

	set, err := e.svc.GetBackupSet(t.Context(), "production/postgres-primary")
	if err != nil {
		t.Fatalf("reading the set back off the engine: %v", err)
	}
	if !set.ReadOnly {
		t.Error("the engine does not hold this set read-only, so the safety declaration reached nothing: the process that deletes remote originals never heard it")
	}
}
