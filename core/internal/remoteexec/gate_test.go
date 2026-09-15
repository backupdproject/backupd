package remoteexec

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/workflowexec"
)

// Two of #812's gate classes, against the fixture server, because neither
// can be asked of a real host on demand: a remote account whose own shell
// startup has already changed what a hook means, and a step whose stop
// this product could not prove.
//
// Both are implemented and neither was asserted. contaminationOf and the
// refusal above it decide whether a hook may run at all, and nothing
// reached them; Client.terminate's certainty rule was covered only as a
// field of a hand-built Result in the audit rendering, which says nothing
// about which of the three termination steps actually set it.

// answerWithProbe is answerInternalSessions with the probe's own answer as
// a parameter, so a test can present the shell startup state of a
// contaminated account. Everything else behaves as an exec-capable host:
// the syntax check succeeds, the reaper and the group scan find nothing,
// and the STEP's channel is recorded and refused nothing -- a
// contamination refusal has to happen before the step is started at all,
// and this fixture is what makes that observable.
func answerWithProbe(probe string) func(t *testing.T, s *fakeSession) {
	return func(t *testing.T, s *fakeSession) {
		t.Helper()

		switch {
		case isSyntaxCheck(s.command):
			s.ReadAll(t)
			s.Exit(0)
		case !strings.HasSuffix(s.command, "-s"):
			// The step's own channel: the token is on this command line
			// and on no other.
			s.ReadAll(t)
			s.Exit(0)
		default:
			if containsProbe(s.ReadAll(t)) {
				s.Print(t, probe)
				s.Exit(0)

				return
			}
			s.Print(t, "groups=\nsurvivors=\n")
			s.Exit(0)
		}
	}
}

// probeAnswerWith renders the fixture probe answer with one field
// replaced, so each row below differs from an exec-capable host in exactly
// one way.
func probeAnswerWith(t *testing.T, field, value string) string {
	t.Helper()

	lines := strings.Split(strings.TrimSuffix(probeAnswer, "\n"), "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, field+"=") {
			lines[i] = field + "=" + value
			replaced = true
		}
	}
	if !replaced {
		t.Fatalf("the fixture probe answer has no %q field, so this row would be testing nothing", field)
	}

	return strings.Join(lines, "\n") + "\n"
}

// TestAnAccountWhoseShellStartupChangedWhatAHookMeansIsRefused is the
// Bash-determinism consensus row from the far side of the connection.
//
// The envelope unsets BASH_ENV, ENV, SHELLOPTS and BASHOPTS as its first
// act, and for BASH_ENV and ENV that is already too late: bash sources
// them at startup, before the payload's first line. SHELLOPTS and BASHOPTS
// are readonly, so an inherited errexit cannot be unset either. The only
// place those hosts can be refused is the preflight, and the only thing
// that makes the refusal honest is the last row here: SHELLOPTS arrives
// non-empty on every host on earth, so a rule that refused a non-empty
// value would refuse everything and prove nothing.
func TestAnAccountWhoseShellStartupChangedWhatAHookMeansIsRefused(t *testing.T) {

	for _, row := range []struct {
		name    string
		field   string
		value   string
		refused string
	}{
		{
			// A file of somebody else's choosing has already run by the
			// time the hook's first line is reached.
			name:    "BASH_ENV points at a file",
			field:   "bash_env",
			value:   "/etc/profile.d/backupd-hooks.sh",
			refused: "BASH_ENV is set",
		},
		{
			name:    "ENV points at a file",
			field:   "env",
			value:   "/home/hookuser/.shinit",
			refused: "ENV is set",
		},
		{
			// errexit inherited from the account is the shell option
			// this product refuses to inject, reintroduced on one host.
			name:    "SHELLOPTS carries errexit",
			field:   "shellopts",
			value:   "braceexpand:errexit:hashall",
			refused: "SHELLOPTS carries errexit",
		},
		{
			name:    "SHELLOPTS carries pipefail",
			field:   "shellopts",
			value:   "braceexpand:hashall:pipefail",
			refused: "SHELLOPTS carries pipefail",
		},
		{
			// BASHOPTS is read against the same option list, so it is
			// not a blind spot. The two variables report overlapping
			// vocabularies depending on how bash was started, and a rule
			// that only looked at SHELLOPTS would be one variable away
			// from missing the same contamination.
			name:    "BASHOPTS carries noglob",
			field:   "bashopts",
			value:   "checkwinsize:noglob",
			refused: "BASHOPTS carries noglob",
		},
	} {
		t.Run(row.name, func(t *testing.T) {

			server := startFakeSSHD(t, answerWithProbe(probeAnswerWith(t, row.field, row.value)))
			client := connectTo(t, server)

			sink := &collectingSink{}
			res, err := client.Run(t.Context(), Request{
				Token:   "backupd-exec-contaminated",
				Script:  []byte("printf 'the hook ran\\n'\n"),
				Sink:    sink,
				Timeout: 30 * time.Second,
			})

			if err == nil {
				t.Fatal("a hook ran on an account whose shell startup had already changed what the script means")
			}
			if !errors.Is(err, ErrExecCapability) {
				t.Errorf("the refusal is not an ErrExecCapability, so a caller cannot tell it from a broken connection: %v", err)
			}
			if !strings.Contains(err.Error(), row.refused) {
				t.Errorf("the refusal does not say what was wrong with the account; it has to name the variable an operator has to clear.\n got: %v\nwant it to contain: %q", err, row.refused)
			}
			if res.ExitCode != nil {
				t.Errorf("a refused step reported exit code %d", *res.ExitCode)
			}

			// And it was refused BEFORE anything of the hook was sent.
			// A refusal after the fact would be a hook that ran under
			// the contaminated shell and was then reported as not having
			// run.
			for _, command := range server.commands() {
				if strings.Contains(command, "backupd-exec-contaminated") {
					t.Errorf("the hook's own command was started on a contaminated account anyway: %q", command)
				}
			}
		})
	}
}

// TestAnExecCapableAccountIsNotRefusedForTheOptionsEveryBashHas is the
// negative control for the table above, and it is the row that makes the
// rest of it mean anything: bash sets braceexpand and hashall on every
// non-interactive shell, so the refusal has to be about the options that
// change what a script MEANS and not about SHELLOPTS being non-empty.
func TestAnExecCapableAccountIsNotRefusedForTheOptionsEveryBashHas(t *testing.T) {

	ran := false
	hook := func(t *testing.T, s *fakeSession) {
		t.Helper()
		ran = true
		s.ReadAll(t)
		s.Print(t, "the hook ran\n")
		s.Exit(0)
	}

	server := startFakeSSHD(t, answerInternalSessions(hook))
	client := connectTo(t, server)

	sink := &collectingSink{}
	if _, err := client.Run(t.Context(), Request{
		Token:   "backupd-exec-clean",
		Script:  []byte("printf 'the hook ran\\n'\n"),
		Sink:    sink,
		Timeout: 30 * time.Second,
	}); err != nil {
		t.Fatalf("an ordinary exec-capable account was refused: %v", err)
	}
	if !ran {
		t.Fatal("the hook's own channel was never opened, so the accepted case proves nothing")
	}
}

// TestAStepWhoseSessionNeverCompletesRecordsTerminationAsUnconfirmed is
// the termination-certainty consensus row: timeout and cancel do not prove
// a remote descendant is dead, so what is recorded is what was PROVED.
//
// The fixture holds the step's channel open after the bound expires --
// which is what a deliberately detached descendant does, since it keeps
// the channel's stdout open -- and ignores the exec-channel signal
// request, which a real server is free to do. So the reaper runs, finds
// nothing it can prove, the signal changes nothing, and the honest answer
// is unconfirmed.
func TestAStepWhoseSessionNeverCompletesRecordsTerminationAsUnconfirmed(t *testing.T) {

	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })

	hook := func(t *testing.T, s *fakeSession) {
		t.Helper()
		s.ReadAll(t)
		s.Print(t, "quiescing\n")
		<-hold
	}

	server := startFakeSSHD(t, answerInternalSessions(hook))
	client := connectTo(t, server)

	sink := &collectingSink{}
	res, err := client.Run(t.Context(), Request{
		Token:   "backupd-exec-detached",
		Script:  []byte("printf 'quiescing\\n'\n"),
		Sink:    sink,
		Timeout: 500 * time.Millisecond,
	})

	if err == nil {
		t.Fatal("a step whose session never completed was reported as having finished")
	}
	if !errors.Is(err, ErrStepTimeout) {
		t.Errorf("error %v is not an ErrStepTimeout", err)
	}
	if res.Certainty != workflowexec.TerminationUnconfirmed {
		t.Errorf("termination was recorded as %q; nothing proved the far side stopped, so the only honest answer is %q",
			res.Certainty, workflowexec.TerminationUnconfirmed)
	}
	if res.ExitCode != nil {
		t.Errorf("a step nobody saw finish reported exit code %d; transport loss is never an exit code", *res.ExitCode)
	}

	// The uncertainty is surfaced rather than buried: the error an
	// operator reads says which of the two it was.
	if !strings.Contains(err.Error(), string(workflowexec.TerminationUnconfirmed)) {
		t.Errorf("the error does not carry the certainty, so an operator cannot tell a proved stop from an unproved one: %v", err)
	}

	// And the reaper's account is kept, because "what did we try" is the
	// first question asked about a step that may still be running.
	if res.Reaper == "" {
		t.Error("no reaper account was recorded for a terminated step")
	}
}

// TestAStepThatEndsWhileTerminationWaitsIsRecordedAsConfirmed is the other
// half, and it is what stops the test above from passing on a client that
// simply always says unconfirmed.
//
// Certainty is decided by one rule -- did the SESSION complete -- and not
// by which of the three termination steps ran. Here the far side ends
// shortly after the bound expires, inside the reaper's own grace, so the
// stop is proved.
func TestAStepThatEndsWhileTerminationWaitsIsRecordedAsConfirmed(t *testing.T) {

	hook := func(t *testing.T, s *fakeSession) {
		t.Helper()
		s.ReadAll(t)
		s.Print(t, "quiescing\n")
		// Longer than the step's bound and far shorter than reapGrace:
		// the session completes while termination is still waiting for
		// exactly that.
		time.Sleep(600 * time.Millisecond)
		s.Exit(143)
	}

	server := startFakeSSHD(t, answerInternalSessions(hook))
	client := connectTo(t, server)

	sink := &collectingSink{}
	res, err := client.Run(t.Context(), Request{
		Token:   "backupd-exec-stops",
		Script:  []byte("printf 'quiescing\\n'\n"),
		Sink:    sink,
		Timeout: 200 * time.Millisecond,
	})

	if err == nil {
		t.Fatal("a step that outran its bound was reported as having finished normally")
	}
	if !errors.Is(err, ErrStepTimeout) {
		t.Errorf("error %v is not an ErrStepTimeout", err)
	}
	if res.Certainty != workflowexec.TerminationConfirmed {
		t.Errorf("termination was recorded as %q for a session that completed while termination waited for it, want %q",
			res.Certainty, workflowexec.TerminationConfirmed)
	}
	// Still no exit code: the status the far side reported after being
	// stopped is not the hook's own verdict, and recording it as one is
	// how a killed step becomes a step that "failed with 143".
	if res.ExitCode != nil {
		t.Errorf("a terminated step reported exit code %d", *res.ExitCode)
	}
}
