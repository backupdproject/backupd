package hostrunner

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestProcessEnv_DeletesTheFourVariablesThatMakeBashRunSomethingElse is
// the environment model's security claim.
//
// Every one of these four is an ordinary variable name by
// internal/workflow's rules, so an operator can put any of them in
// workflows.environment today and this package is the only thing standing
// between that line and a preamble script running before every hook in
// the deployment.
//
// The test states them individually rather than looping over
// CursedEnvNames(), because a loop over the list under test passes when
// the list is empty.
func TestProcessEnv_DeletesTheFourVariablesThatMakeBashRunSomethingElse(t *testing.T) {
	set := EnvSet{Vars: []EnvVar{
		{Name: "BASH_ENV", Value: "/tmp/preamble.sh"},
		{Name: "ENV", Value: "/tmp/preamble.sh"},
		{Name: "SHELLOPTS", Value: "xtrace"},
		{Name: "BASHOPTS", Value: "expand_aliases"},
		{Name: "KEPT", Value: "yes"},
	}}

	block, err := set.ProcessEnv(nil)
	if err != nil {
		t.Fatalf("building the environment: %v", err)
	}

	for _, name := range []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS"} {
		if slices.ContainsFunc(block, func(entry string) bool { return strings.HasPrefix(entry, name+"=") }) {
			t.Errorf("%s survived into the hook's environment. It makes bash execute, or reinterpret, something the plan never captured.", name)
		}
	}
	if !slices.Contains(block, "KEPT=yes") {
		t.Errorf("an ordinary variable was lost while the cursed ones were removed: %q", block)
	}
	if dropped := set.DroppedEnvNames(); len(dropped) != 4 {
		t.Errorf("the runner reports %v as dropped, so an operator whose BASH_ENV silently stopped working has no way to find out why", dropped)
	}
}

// TestProcessEnv_DeletesThemFromAnInheritedBlockToo covers the other
// direction: a variable this process itself was started with.
//
// A service manager, a NAS package's wrapper script or a developer's
// shell can all export BASH_ENV, and a sanitizer that only looked at what
// the engine sent would let that one through.
func TestProcessEnv_DeletesThemFromAnInheritedBlockToo(t *testing.T) {
	block, err := EnvSet{}.ProcessEnv([]string{"BASH_ENV=/tmp/inherited.sh", "PATH=/bin"})
	if err != nil {
		t.Fatalf("building the environment: %v", err)
	}
	if slices.Contains(block, "BASH_ENV=/tmp/inherited.sh") {
		t.Errorf("an inherited BASH_ENV reached the hook: %q", block)
	}
}

// TestProcessEnv_RefusesANulRatherThanTruncatingASecret is the failure
// mode worth a test of its own.
//
// execve terminates each entry at the first NUL, so a value carrying one
// arrives as its own prefix -- a repository passphrase silently cut in
// half, with no error anywhere and a hook that fails somewhere else
// entirely.
func TestProcessEnv_RefusesANulRatherThanTruncatingASecret(t *testing.T) {
	_, err := EnvSet{Vars: []EnvVar{{Name: "PASSPHRASE", Value: "good\x00bad"}}}.ProcessEnv(nil)
	if err == nil {
		t.Fatal("a value containing a NUL was accepted, and execve would have silently truncated it")
	}
	if !errors.Is(err, ErrEnv) {
		t.Fatalf("the refusal is not an ErrEnv: %v", err)
	}
	if strings.Contains(err.Error(), "good") {
		t.Errorf("the refusal quotes the VALUE, which is how a repository passphrase ends up in a log file: %v", err)
	}
}

// TestProcessEnv_AppliesTheSendersPrecedence checks the one ordering rule
// this package is responsible for: later wins, because the sender ordered
// the layers (deployment, then backup set, then RETND_*).
func TestProcessEnv_AppliesTheSendersPrecedence(t *testing.T) {
	block, err := EnvSet{Vars: []EnvVar{
		{Name: "PGHOST", Value: "deployment"},
		{Name: "PGHOST", Value: "backup-set"},
	}}.ProcessEnv(nil)
	if err != nil {
		t.Fatalf("building the environment: %v", err)
	}
	if !slices.Contains(block, "PGHOST=backup-set") {
		t.Errorf("the later layer did not win: %q", block)
	}
	if slices.Contains(block, "PGHOST=deployment") {
		t.Errorf("both layers reached the hook, so the variable is declared twice: %q", block)
	}
}

// TestProcessEnv_RefusesANameNoShellCouldRead keeps the wire's rule the
// same as the configuration file's.
//
// A name with a dash in it can be placed in an environment block by
// execve and then read by no shell script that receives it: a variable
// that exists and does not work, which is the worst of the three possible
// outcomes.
func TestProcessEnv_RefusesANameNoShellCouldRead(t *testing.T) {
	if _, err := (EnvSet{Vars: []EnvVar{{Name: "NOT-A-NAME", Value: "x"}}}).ProcessEnv(nil); err == nil {
		t.Fatal("a name no shell can read was accepted")
	}
}

// TestEnvVar_RendersNamesAndNeverValues protects the diagnostic path.
//
// A hook's environment is the single most likely thing to be printed
// while somebody is debugging a failing step, and it routinely holds a
// repository passphrase.
func TestEnvVar_RendersNamesAndNeverValues(t *testing.T) {
	set := EnvSet{Vars: []EnvVar{{Name: "PASSPHRASE", Value: "hunter2"}}}

	for _, rendering := range []string{set.String(), set.GoString(), set.Vars[0].String(), set.Vars[0].GoString()} {
		if strings.Contains(rendering, "hunter2") {
			t.Errorf("a rendering of the environment carries the value: %q", rendering)
		}
		if !strings.Contains(rendering, "PASSPHRASE") {
			t.Errorf("a rendering of the environment does not name the variable, so it is useless for debugging: %q", rendering)
		}
	}
}
