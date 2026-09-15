package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/internal/secretref"
)

// The environment model's suite. Three properties, and each of them is one
// somebody will try to relax:
//
//   - precedence, including the fact that a built-in is not a layer an
//     operator competes with;
//   - the reservation, which is enforced at VALIDATION time rather than by
//     being overridden at merge time, so a key that cannot work is refused
//     rather than silently discarded;
//   - that a resolved secret never leaves a Resolved except through the
//     two methods named for it.

func TestEnvironmentPrecedenceRunsBaselineThenGlobalThenSet(t *testing.T) {
	t.Parallel()

	env, err := NewEnvironment(
		SanitizedBaseline(),
		[]EnvVar{{Name: "SCOPE", Value: "global"}, {Name: "ONLY_GLOBAL", Value: "g"}, {Name: "PATH", Value: "/global/bin"}},
		[]EnvVar{{Name: "SCOPE", Value: "set"}, {Name: "ONLY_SET", Value: "s"}},
	)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	got := map[string]string{}
	for _, v := range env.Vars() {
		got[v.Name] = v.Value
	}

	for name, want := range map[string]string{
		"SCOPE":       "set",         // the set wins over the deployment
		"ONLY_GLOBAL": "g",           // and does not erase what it did not mention
		"ONLY_SET":    "s",           //
		"PATH":        "/global/bin", // a layer may override the baseline
	} {
		if got[name] != want {
			t.Errorf("%s resolved to %q, want %q", name, got[name], want)
		}
	}

	// Sorted by name, which is what makes a plan hash over an environment
	// deterministic rather than dependent on map iteration.
	names := env.Names()
	if !slices.IsSorted(names) {
		t.Errorf("Names() = %v, which is not sorted; a plan hash over an unordered environment is not reproducible", names)
	}
}

// The built-ins win, and they win by construction rather than by every
// configuration path having remembered to check.
func TestResolvePutsBuiltinsOverEverything(t *testing.T) {
	// No t.Parallel: this test uses t.Setenv, which the testing package
	// refuses to combine with a parallel test because the process
	// environment is shared.

	env, err := NewEnvironment([]EnvVar{{Name: "PGDATABASE", Value: "orders"}})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	resolved, err := env.Resolve(context.Background(), map[string]string{
		"RETND":               "1",
		"RETND_PHASE":         string(PhaseBefore),
		"RETND_BACKUP_STATUS": string(StatusUnknown),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got := environMap(t, resolved)

	for name, want := range map[string]string{
		"PGDATABASE":          "orders",
		"RETND":               "1",
		"RETND_PHASE":         "before",
		"RETND_BACKUP_STATUS": "unknown",
		"PATH":                "", // not in this environment: the baseline is a layer the CALLER supplies
	} {
		if want == "" {
			if _, present := got[name]; present {
				t.Errorf("%s is set to %q and this environment was built without the baseline", name, got[name])
			}

			continue
		}

		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}

	// A hook does NOT inherit this daemon's environment. The strongest
	// available assertion of that is that a variable this process
	// certainly has is not in the block.
	t.Setenv("WORKFLOW_TEST_LEAK_CANARY", "leaked")

	resolved, err = env.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, present := environMap(t, resolved)["WORKFLOW_TEST_LEAK_CANARY"]; present {
		t.Error("a variable from this process's own environment reached a hook's environment; the whole point of building the block is that nothing leaks in, including a secret an operator resolves through secretref's env source")
	}
}

// Only a documented built-in may be injected. A typo'd
// RETND_BAKCUP_STATUS would otherwise become part of a hook's contract
// and stay there for as long as somebody's script reads it.
func TestResolveRefusesAnUndocumentedBuiltin(t *testing.T) {
	t.Parallel()

	env, err := NewEnvironment(nil)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	_, err = env.Resolve(context.Background(), map[string]string{"RETND_BAKCUP_STATUS": "success"})
	if err == nil {
		t.Fatal("a variable outside the documented built-in set was injected")
	}
	if !errors.Is(err, ErrEnvName) {
		t.Errorf("Resolve returned %v, want an ErrEnvName", err)
	}

	// The positive control: every documented name is accepted, which is
	// what stops the check above from being satisfiable by a set that has
	// quietly emptied.
	all := map[string]string{}
	for _, name := range BuiltinEnvNames() {
		all[name] = "x"
	}
	if len(all) == 0 {
		t.Fatal("BuiltinEnvNames returned nothing, so this test proved nothing")
	}

	resolved, err := env.Resolve(context.Background(), all)
	if err != nil {
		t.Fatalf("the documented built-in set was refused: %v", err)
	}
	// Every built-in, plus the compat block: one deprecated spelling per
	// name (LegacyEnvName), and nothing else.
	if want := 2 * len(all); len(resolved.Names()) != want {
		t.Errorf("Resolve carried %d names for %d built-ins, want %d (each name plus its deprecated spelling)",
			len(resolved.Names()), len(all), want)
	}
}

// The reservation and the name rule, as one table. Every row is a key an
// operator might really write, and the refusal has to happen at validation
// time: a key this product silently discards is a key that looks like it
// works.
func TestValidateEnvNameRefusesUnusableAndReservedNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		name    string
		mustSay string
	}{
		{"empty", "", "must have a name"},
		{"a leading digit", "1PATH", EnvNamePattern},
		{"a dash", "MY-VAR", EnvNamePattern},
		{"a dot", "MY.VAR", EnvNamePattern},
		{"a space", "MY VAR", EnvNamePattern},
		{"an equals sign", "MY=VAR", EnvNamePattern},
		{"a NUL", "MY\x00VAR", "NUL"},
		{"non-ASCII", "MYVAR\u00e9", EnvNamePattern},
		{"the bare reserved name", "RETND", "is reserved"},
		{"the bare reserved name's deprecated spelling", "BACKUPD", "is reserved"},
		{"a reserved built-in", "RETND_RUN_ID", "is reserved"},
		{"a reserved name this product does not set yet", "RETND_ANYTHING_AT_ALL", "is reserved"},
		// Reserved for as long as it is still exported: a key an
		// operator saved under the deprecated prefix would be
		// overwritten by the compat block with no word said.
		{"a name under the deprecated prefix", "BACKUPD_BACKUP_STATUS", "is reserved"},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			err := ValidateEnvName(tc.name)
			if err == nil {
				t.Fatalf("ValidateEnvName(%q) accepted it", tc.name)
			}
			if !errors.Is(err, ErrEnvName) {
				t.Errorf("ValidateEnvName(%q) returned %v, want an ErrEnvName", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("ValidateEnvName(%q) said:\n\t%v\nwant it to contain %q", tc.name, err, tc.mustSay)
			}
		})
	}

	// The positive control. Without it every row above would pass against
	// a function that refused everything.
	for _, name := range []string{"PATH", "_", "_X", "PGDATABASE", "a", "A1_b2"} {
		if err := ValidateEnvName(name); err != nil {
			t.Errorf("ValidateEnvName(%q) refused a perfectly ordinary name: %v", name, err)
		}
	}

	// Every documented built-in is reserved. A built-in this product sets
	// that an operator could ALSO configure is the collision the prefix
	// rule exists to make impossible.
	for _, name := range BuiltinEnvNames() {
		if !IsReservedEnvName(name) {
			t.Errorf("the built-in %s is not reserved, so an operator can configure a variable this product overwrites", name)
		}
		if err := ValidateEnvName(name); err == nil {
			t.Errorf("ValidateEnvName accepted the built-in %s", name)
		}
	}
}

func TestEnvVarRefusesUnusableValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		what    string
		v       EnvVar
		mustSay string
	}{
		{
			what:    "a NUL in the value",
			v:       EnvVar{Name: "X", Value: "a\x00b"},
			mustSay: "NUL byte",
		},
		{
			what:    "a literal and a secret at once",
			v:       EnvVar{Name: "X", Value: "literal", Secret: secretref.Ref{Env: "SOMEWHERE"}},
			mustSay: "both a literal value and a secret reference",
		},
		{
			what:    "two secret sources",
			v:       EnvVar{Name: "X", Secret: secretref.Ref{Env: "A", File: "/b"}},
			mustSay: "more than one secret source",
		},
	}

	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			err := tc.v.Validate()
			if err == nil {
				t.Fatalf("EnvVar%+v was accepted", tc.v)
			}
			if !errors.Is(err, ErrEnvValue) {
				t.Errorf("Validate returned %v, want an ErrEnvValue", err)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("Validate said:\n\t%v\nwant it to contain %q", err, tc.mustSay)
			}
		})
	}

	// An empty literal is a real value, and a type that could not carry
	// one would silently drop a variable an operator asked for.
	if err := (EnvVar{Name: "EMPTY", Value: ""}).Validate(); err != nil {
		t.Errorf("an empty literal value was refused: %v", err)
	}
}

// A duplicate within one layer has no defined winner and is refused; the
// same name in two layers is exactly what the layering is for.
func TestNewEnvironmentRefusesADuplicateWithinOneLayer(t *testing.T) {
	t.Parallel()

	_, err := NewEnvironment([]EnvVar{{Name: "X", Value: "1"}, {Name: "X", Value: "2"}})
	if err == nil {
		t.Fatal("one environment block declaring X twice was accepted; there is no rule for choosing between them")
	}
	if !errors.Is(err, ErrEnvName) {
		t.Errorf("NewEnvironment returned %v, want an ErrEnvName", err)
	}

	if _, err := NewEnvironment([]EnvVar{{Name: "X", Value: "1"}}, []EnvVar{{Name: "X", Value: "2"}}); err != nil {
		t.Errorf("the same name in two layers is the whole point of the layering, and it was refused: %v", err)
	}
}

// Literal values are literal. A config file that expanded "$HOME" would be
// a config file whose meaning depends on this daemon's own environment,
// which is what the sanitized baseline exists to sever.
func TestLiteralValuesAreNotExpanded(t *testing.T) {
	// No t.Parallel: this test uses t.Setenv, which the testing package
	// refuses to combine with a parallel test because the process
	// environment is shared.

	t.Setenv("HOME", "/root")

	env, err := NewEnvironment([]EnvVar{{Name: "TARGET", Value: "$HOME/backup"}})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	resolved, err := env.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got := environMap(t, resolved)["TARGET"]; got != "$HOME/backup" {
		t.Errorf("TARGET = %q, want the literal %q: this value is not a shell expression", got, "$HOME/backup")
	}
}

// The secret round trip, and the claim #808 asks for: the resolved value
// appears in no artifact anybody keeps.
//
// The sentinel is scanned for in every rendering of every value this
// package hands out apart from the two methods whose entire purpose is to
// return material -- Environ, which goes to an exec, and SecretValues,
// which goes to the run's redaction set.
func TestAResolvedSecretRoundTripsWithoutEverBeingExposed(t *testing.T) {
	t.Parallel()

	const sentinel = "TOP-SECRET-c7f1e0a9-DO-NOT-PERSIST"

	dir := custodyTempDir(t)
	secretFile := filepath.Join(dir, "token")
	if err := os.WriteFile(secretFile, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	ref := secretref.Ref{File: secretFile}

	env, err := NewEnvironment([]EnvVar{
		{Name: "PGDATABASE", Value: "orders"},
		{Name: "API_TOKEN", Secret: ref},
	})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	// The configured form carries a LOCATION and no material, which is
	// what makes it safe to put in a plan, a config file and an API
	// response.
	for _, rendering := range []string{
		fmt.Sprintf("%v", env),
		fmt.Sprintf("%+v", env),
		fmt.Sprintf("%#v", env),
		fmt.Sprint(env.Vars()),
	} {
		if strings.Contains(rendering, sentinel) {
			t.Fatalf("the configured environment rendered the resolved secret: %s", rendering)
		}
	}

	resolved, err := env.Resolve(context.Background(), map[string]string{"RETND": "1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Resolve really did resolve it: without this the scan below passes
	// against a function that returned nothing.
	if got := environMap(t, resolved)["API_TOKEN"]; got != sentinel {
		t.Fatalf("API_TOKEN resolved to %q, want the file's content %q", got, sentinel)
	}
	if values := resolved.SecretValues(); len(values) != 1 || values[0] != sentinel {
		t.Fatalf("SecretValues() = %v, want exactly the one resolved value: it is what the run's redaction set is built from", values)
	}

	// And every OTHER way of looking at it is redacted.
	for _, rendering := range []string{
		resolved.String(),
		resolved.GoString(),
		fmt.Sprintf("%v", resolved),
		fmt.Sprintf("%+v", resolved),
		fmt.Sprintf("%#v", resolved),
		fmt.Sprint(resolved.Names()),
	} {
		if strings.Contains(rendering, sentinel) {
			t.Errorf("a resolved environment rendered its secret: %s", rendering)
		}
	}

	// The variable is still THERE, named, in the ordinary rendering. A
	// type that hid the name as well would make a misconfigured hook
	// undiagnosable.
	if !strings.Contains(resolved.String(), "API_TOKEN") {
		t.Errorf("the rendering does not name the variable at all: %s", resolved.String())
	}
}

// A secret that will not resolve is a refusal, not an empty value. A hook
// handed an empty credential fails somewhere far away from the reason.
func TestResolveRefusesASecretItCannotRead(t *testing.T) {
	t.Parallel()

	env, err := NewEnvironment([]EnvVar{{Name: "API_TOKEN", Secret: secretref.Ref{File: filepath.Join(custodyTempDir(t), "absent")}}})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	_, err = env.Resolve(context.Background(), nil)
	if err == nil {
		t.Fatal("an unreadable secret resolved to something; a hook handed an empty credential fails somewhere far away from the cause")
	}
	if !strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("the refusal must name the variable whose secret could not be read, got: %v", err)
	}
}

// A secret file another account can read is refused, because that is
// internal/secretref's custody rule and this is the same custody model
// rather than a second one.
func TestResolveInheritsSecretrefCustody(t *testing.T) {
	t.Parallel()

	path := filepath.Join(custodyTempDir(t), "token")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	env, err := NewEnvironment([]EnvVar{{Name: "API_TOKEN", Secret: secretref.Ref{File: path}}})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	_, err = env.Resolve(context.Background(), nil)
	if !errors.Is(err, secretref.ErrCustody) {
		t.Errorf("Resolve returned %v, want secretref's own ErrCustody: this package must not be a second, weaker custody model", err)
	}
}

func environMap(t *testing.T, r Resolved) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, entry := range r.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("Environ produced %q, which is not NAME=VALUE", entry)
		}
		out[name] = value
	}

	return out
}
