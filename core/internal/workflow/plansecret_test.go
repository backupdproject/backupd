package workflow

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/secretref"
)

// The persisted half of #808's secret claim: "the resolved value appears
// in no persisted artifact".
//
// env_test.go proves the in-memory half (no rendering of a Resolved leaks
// it). This file proves the durable half, over the three things a run
// actually leaves behind that this package owns: the Plan, the bytes that
// go into ResolvedPlanHash, and the spool on disk. The journal's half is
// in internal/state, against the schema.
//
// The test resolves the secret FIRST, so it is not passing because
// resolution never happened, and then scans for the sentinel.

func TestNoResolvedSecretReachesThePlanTheHashOrTheSpool(t *testing.T) {
	t.Parallel()

	const sentinel = "TOP-SECRET-4a91bd7c-DO-NOT-PERSIST"

	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "token")
	if err := os.WriteFile(secretFile, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	env, err := NewEnvironment(
		SanitizedBaseline(),
		[]EnvVar{
			{Name: "PGDATABASE", Value: "orders"},
			{Name: "API_TOKEN", Secret: secretref.Ref{File: secretFile}},
		},
	)
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}

	// Prove the secret really is resolvable through this environment.
	// Without this the scans below would pass against a configuration
	// whose secret was never reachable at all.
	resolved, err := env.Resolve(t.Context(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := environMap(t, resolved)["API_TOKEN"]; got != sentinel {
		t.Fatalf("the fixture's secret does not resolve to the sentinel (got %q), so this test proves nothing", got)
	}

	f := newFixture(t)
	req := f.request("run-1")
	req.Env = env

	plan, err := Snapshot(req)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// 1. The plan value itself, in every rendering something might log,
	//    marshal or dump it with.
	for _, rendering := range []string{
		fmt.Sprintf("%v", plan),
		fmt.Sprintf("%+v", plan),
		fmt.Sprintf("%#v", plan),
	} {
		if strings.Contains(rendering, sentinel) {
			t.Errorf("the plan renders the resolved secret:\n%s", rendering)
		}
	}

	// 2. The bytes ResolvedPlanHash is taken over. A hash over a
	//    credential is a credential oracle, so the material must not be
	//    in the pre-image either -- and the LOCATION must be, or two
	//    plans that differ in where a secret comes from would hash the
	//    same.
	canonical := plan.canonical(req)
	if strings.Contains(canonical, sentinel) {
		t.Errorf("the plan hash is taken over the resolved secret:\n%s", canonical)
	}
	if !strings.Contains(canonical, secretFile) {
		t.Errorf("the plan hash does not cover WHERE the secret comes from; two plans differing only in that would hash identically:\n%s", canonical)
	}

	// 3. The spool on disk: every path and every byte of every file.
	err = filepath.WalkDir(plan.ScriptSpoolRef, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if strings.Contains(path, sentinel) {
			t.Errorf("a spool path carries the resolved secret: %s", path)
		}

		if d.IsDir() {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), sentinel) {
			t.Errorf("the spooled file %s contains the resolved secret", path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the spool: %v", err)
	}
}

// Two plans whose only difference is where a secret comes from must not
// hash the same, and that is a property of secretLocation covering every
// source secretref.Ref has.
//
// A fourth source appearing on Ref without a case here would silently
// collapse to "none" and make every plan using it hash identically to
// every other.
func TestPlanHashCoversEverySecretSource(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	hashes := map[string]string{}

	for _, tc := range []struct {
		what string
		ref  secretref.Ref
	}{
		{"file", secretref.Ref{File: "/run/secrets/a"}},
		{"another file", secretref.Ref{File: "/run/secrets/b"}},
		{"env", secretref.Ref{Env: "TOKEN_A"}},
		{"another env", secretref.Ref{Env: "TOKEN_B"}},
		{"command", secretref.Ref{Command: []string{"/usr/bin/vault", "read", "a"}}},
		{"another command", secretref.Ref{Command: []string{"/usr/bin/vault", "read", "b"}}},
		// The argv is joined on a separator no path or variable name can
		// contain, so two argvs that concatenate to the same string are
		// still distinguishable.
		{"an argv that would collide under a naive join", secretref.Ref{Command: []string{"/usr/bin/vault read", "a"}}},
	} {
		env, err := NewEnvironment([]EnvVar{{Name: "API_TOKEN", Secret: tc.ref}})
		if err != nil {
			t.Fatalf("NewEnvironment(%s): %v", tc.what, err)
		}

		req := f.request("run-" + tc.what)
		req.Env = env

		plan, err := Snapshot(req)
		if err != nil {
			t.Fatalf("Snapshot(%s): %v", tc.what, err)
		}

		if previous, clash := hashes[plan.ResolvedPlanHash]; clash {
			t.Errorf("the %s secret source and the %s one produce the same plan hash %s; the plan hash does not distinguish where a secret comes from",
				tc.what, previous, plan.ResolvedPlanHash)
		}
		hashes[plan.ResolvedPlanHash] = tc.what
	}

	if len(hashes) < 7 {
		t.Errorf("only %d distinct hashes came out of 7 distinct secret locations", len(hashes))
	}
}

// The environment is part of the plan hash at all, which is the property
// the two tests above depend on: a change to a configured variable has to
// change the fingerprint of what this run executes with.
func TestPlanHashCoversTheConfiguredEnvironment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	hash := func(t *testing.T, runID string, vars []EnvVar) string {
		t.Helper()

		env, err := NewEnvironment(vars)
		if err != nil {
			t.Fatalf("NewEnvironment: %v", err)
		}

		req := f.request(runID)
		req.Env = env

		plan, err := Snapshot(req)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		return plan.ResolvedPlanHash
	}

	base := hash(t, "run-a", []EnvVar{{Name: "PGDATABASE", Value: "orders"}})
	sameAgain := hash(t, "run-b", []EnvVar{{Name: "PGDATABASE", Value: "orders"}})
	changedValue := hash(t, "run-c", []EnvVar{{Name: "PGDATABASE", Value: "invoices"}})
	changedName := hash(t, "run-d", []EnvVar{{Name: "PGSCHEMA", Value: "orders"}})
	extra := hash(t, "run-e", []EnvVar{{Name: "PGDATABASE", Value: "orders"}, {Name: "PGHOST", Value: "db"}})

	if base != sameAgain {
		t.Errorf("the same environment hashed differently twice (%s, %s)", base, sameAgain)
	}
	for what, got := range map[string]string{
		"a changed value": changedValue,
		"a changed name":  changedName,
		"an extra entry":  extra,
	} {
		if got == base {
			t.Errorf("%s did not change the plan hash, so the hash does not describe what this run executes with", what)
		}
	}
}
