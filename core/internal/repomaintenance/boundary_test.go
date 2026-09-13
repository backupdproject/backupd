package repomaintenance_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/repomaintenance"
	"github.com/backupdproject/backupd/core/internal/snapshotretention"
)

// The structural claims. Each of them is a sentence from issue #786 that
// a behavioural test can only prove about the calls this package makes
// TODAY, and that a later change could quietly break without failing any
// of them.

// TestMaintenanceCannotDeleteASnapshot pins the method set of the only
// engine surface this package has.
//
// Two methods, named rather than counted, because the failure this guards
// against is an ADDITION with a plausible name: a DeleteSnapshot "so a
// window can tidy up the manifest it just orphaned", a ListSnapshots "so
// it can decide what to reclaim". Either one turns maintenance into a
// second retention policy, deciding what stops being kept without the
// catalog, the holds or the last-known-good protection that
// internal/snapshotretention decides with.
//
// The mirror of this test lives in that package
// (TestRetentionsRepositoryPortCannotReachRepositoryStorage) and pins the
// opposite half: retention cannot reach storage, maintenance cannot reach
// a manifest.
func TestMaintenanceCannotDeleteASnapshot(t *testing.T) {
	t.Parallel()

	port := reflect.TypeOf((*repomaintenance.Repository)(nil)).Elem()

	got := make([]string, 0, port.NumMethod())
	for i := range port.NumMethod() {
		got = append(got, port.Method(i).Name)
	}

	sort.Strings(got)

	want := []string{"Maintain", "Stats"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repomaintenance.Repository has methods %v, want exactly %v.\n"+
			"Maintenance reclaims the storage behind manifests something else deleted; deciding WHICH snapshot stops being kept "+
			"is internal/snapshotretention (#785) and must not be reachable from a maintenance window.", got, want)
	}
}

// TestABackupengineRepositorySatisfiesTheMaintenancePort: the port is
// narrow, and it is still the real thing. A port nothing implements is a
// port that drifts away from the boundary it was carved out of.
func TestABackupengineRepositorySatisfiesTheMaintenancePort(t *testing.T) {
	t.Parallel()

	engine := reflect.TypeOf((*backupengine.Repository)(nil)).Elem()
	port := reflect.TypeOf((*repomaintenance.Repository)(nil)).Elem()

	if !engine.Implements(port) {
		t.Fatal("backupengine.Repository no longer satisfies repomaintenance.Repository")
	}
}

// TestAFencedRepositoryIsWhatARetentionPassCanUse is the compile-time
// half of the fencing claim: GuardSnapshots produces exactly the port
// internal/snapshotretention's Pruner takes, so fencing a retention pass
// is a wrapping and not a rewrite of it.
//
// If these two method sets ever diverge, this fails here rather than at
// the one wiring call site in a future #788.
func TestAFencedRepositoryIsWhatARetentionPassCanUse(t *testing.T) {
	t.Parallel()

	fence := repomaintenance.NewFence()

	// A typed assignment rather than a type assertion: the claim is that
	// the two ports agree at COMPILE time, so a divergence has to fail
	// the build of this package's tests and not one run of one test.
	var guarded snapshotretention.Repository = fence.GuardSnapshots(testDomain(t, "wiring-domain"), nil)

	if guarded == nil {
		t.Fatal("GuardSnapshots returned nothing to fence")
	}
}

// TestNothingInThisPackageNamesTheVendor is backupengine's boundary rule,
// asked of this package: every import of the embedded engine lives in the
// one adapter package, and a maintenance scheduler that reached for the
// vendor's own maintenance API would be a second place that knows its
// name -- and, worse, the one place from which its safety parameters
// could be weakened.
func TestNothingInThisPackageNamesTheVendor(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		for _, imp := range file.Imports {
			if strings.Contains(imp.Path.Value, "kopia") {
				t.Errorf("%s imports %s: the embedded engine is reachable only through internal/backupengine and its one adapter", name, imp.Path.Value)
			}
		}
	}
}
