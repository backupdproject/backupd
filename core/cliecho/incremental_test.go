package cliecho

import (
	"strings"
	"testing"
)

// EPIC K's incremental sets on this surface (#788).
//
// The line this package prints is a claim that running it does what the
// request did. For an incremental set that claim was false rather than
// incomplete: the builder echoed every field an ARTIFACT set has and
// none of the fields that make a set incremental, so a wizard that had
// just written a kopia set into a declared repository domain printed a
// `backup-set create` that would have written a whole-file artifact set
// somewhere else. An operator pasting it would not have reproduced the
// set they were looking at; they would have made a different kind of set
// under the same id, and nothing on the line said so.
//
// Both halves are asserted the same way: the flags have to be there AND
// the command has to be one the binary takes, which is what
// core/cmd/retnd's dispatcher test settles for the examples.

// anIncrementalCreate is the body a Web create of a kopia set sends.
const anIncrementalCreate = `{"source_name":"api-server","name":"var-backups","host":"10.0.0.14","user":"backups",` +
	`"remote_path":"/var/backups","local_path":"/data/backups","ssh_key_id":"key_1",` +
	`"known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","completion_strategy":"rename",` +
	`"engine":"kopia","repository_domain":"production-vault","source_consistency":"quiesced",` +
	`"verification_level":"sample","verification_sample_percent":10,` +
	`"verification_full_every_seconds":604800,"verification_restore_drill_every_seconds":2592000}`

func TestAnIncrementalCreateEchoesTheCommandThatMakesTheSameSet(t *testing.T) {
	line := Echo(Action{Method: "POST", Route: "/backup-sets", Body: []byte(anIncrementalCreate)})
	got := line.Shell()

	for _, want := range []string{
		// The two that decide what the set IS. Without either of these
		// the line reads as an artifact create, which is a different
		// product against the same source.
		"--engine kopia",
		"--repository-domain production-vault",
		// The budget. A set reproduced without it silently takes the
		// defaults, which is a quieter and more expensive machine than
		// the one the operator was looking at.
		"--source-consistency quiesced",
		"--verification-level sample",
		"--verification-sample-percent 10",
		"--verification-full-every 168h",
		"--verification-restore-drill-every 720h",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("an incremental create does not echo %s, so the printed command makes a different set from the one the request made:\n  %s", want, got)
		}
	}
}

// The control, and it is the half that keeps the flags from becoming
// noise: an artifact create carries none of these fields, and a line that
// named them would be telling an operator to declare a repository domain
// for a set that must not have one (config.Validate refuses it).
func TestAnArtifactCreateEchoesNoneOfTheIncrementalFlags(t *testing.T) {
	line := Echo(Action{Method: "POST", Route: "/backup-sets",
		Body: []byte(`{"source_name":"api-server","name":"var-backups","host":"10.0.0.14","user":"backups","remote_path":"/var/backups","local_path":"/data/backups","ssh_key_id":"key_1","known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","completion_strategy":"rename"}`)})
	got := line.Shell()

	for _, unwanted := range []string{
		"--engine", "--repository-domain", "--source-consistency",
		"--verification-level", "--verification-sample-percent",
		"--verification-full-every", "--verification-restore-drill-every",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("an artifact create echoes %s, which its request never carried:\n  %s", unwanted, got)
		}
	}
}

// The edit half. A verification budget is the part of an incremental set
// an operator actually revises, so the PATCH builder has to name what
// changed: a line that echoed `backup-set patch` with nothing after it
// would be a command that exits 2 for naming no field at all.
func TestAnIncrementalPatchEchoesTheBudgetItChanged(t *testing.T) {
	line := Echo(Action{
		Method: "PATCH", Route: "/backup-sets/{source}/{set}",
		Params: map[string]string{"source": "api-server", "set": "var-backups"},
		Body: []byte(`{"source_consistency":"snapshot","verification_level":"full",` +
			`"verification_sample_percent":25,"verification_full_every_seconds":86400,` +
			`"verification_restore_drill_every_seconds":0}`),
	})
	got := line.Shell()

	for _, want := range []string{
		"backup-set patch api-server/var-backups",
		"--source-consistency snapshot",
		"--verification-level full",
		"--verification-sample-percent 25",
		"--verification-full-every 24h",
		// An explicit zero is "stop drilling", which is a request rather
		// than an omission, so it has to reach the line as a value.
		"--verification-restore-drill-every 0s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("an incremental patch does not echo %s:\n  %s", want, got)
		}
	}
}

// And the sparseness rule this builder has always kept: a patch that
// named one field echoes that field and nothing else, because every flag
// on the line is another thing the pasted command would change.
func TestAPatchEchoesOnlyTheBudgetFieldsItCarried(t *testing.T) {
	line := Echo(Action{
		Method: "PATCH", Route: "/backup-sets/{source}/{set}",
		Params: map[string]string{"source": "api-server", "set": "var-backups"},
		Body:   []byte(`{"verification_level":"metadata"}`),
	})
	if got, want := line.Shell(), Binary+" backup-set patch api-server/var-backups --verification-level metadata"; got != want {
		t.Errorf("a patch naming one field prints\n  %s\nwant\n  %s", got, want)
	}
}
