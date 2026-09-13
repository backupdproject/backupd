package kopia

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kopia/kopia/repo/maintenance"
)

// Issue #786's third requirement, and the one with no behavioural test
// available: "preserve upstream safety intervals -- never use unsafe
// equivalents of 'ignore safety' / 'force immediate content deletion' for
// production automation".
//
// It has no behavioural test because the failure it describes does not
// look like a failure. A repository maintained with the vendor's safety
// parameters zeroed passes every assertion in this package: it reclaims
// MORE space, faster, and it restores fine -- right up until a
// maintenance window overlaps a snapshot that is still being written, or
// a bucket's listing is a few seconds stale, and the content the new
// snapshot references has already been collected. So the guard is on the
// source and on the vendor's own constants, and it is checked rather than
// remembered.

// TestMaintenanceRunsAtTheVendorsFullSafety reads the actual call.
//
// It parses this package's production sources, finds every call to the
// vendor's maintenance entry point, and requires the safety argument to
// be maintenance.SafetyFull, spelled exactly. A test that grepped for the
// absence of "SafetyNone" would pass against a locally-constructed
// SafetyParameters value with every margin at zero, which is the same
// mistake with a different name.
func TestMaintenanceRunsAtTheVendorsFullSafety(t *testing.T) {
	calls := 0

	for path, file := range productionFiles(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "snapshotmaintenance" || sel.Sel.Name != "Run" {
				return true
			}

			calls++

			if len(call.Args) == 0 {
				t.Fatalf("%s calls snapshotmaintenance.Run with no arguments", path)
			}

			if got := render(call.Args[len(call.Args)-1]); got != "maintenance.SafetyFull" {
				t.Errorf("%s runs maintenance at safety %s, want maintenance.SafetyFull.\n"+
					"Anything weaker lets the vendor collect content a concurrent snapshot is still writing; "+
					"see issue #786's safety-interval requirement before changing this.", path, got)
			}

			return true
		})
	}

	if calls == 0 {
		t.Fatal("no call to the vendor's maintenance entry point was found; this test has stopped watching anything")
	}
}

// TestNoProductionSourceNamesAWeakenedSafetyLevel is the same claim made
// against the whole package, because the argument above is only the call
// that exists today.
func TestNoProductionSourceNamesAWeakenedSafetyLevel(t *testing.T) {
	banned := []string{"SafetyNone", "SafetyParameters{", "DisableEventualConsistencySafety"}

	for path, file := range productionFiles(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			for _, name := range banned {
				if strings.Contains(render(n), name) {
					if _, isFile := n.(*ast.File); isFile {
						return true
					}

					t.Errorf("%s names %s: this product does not choose maintenance safety parameters, it uses the vendor's full set", path, name)

					return false
				}
			}

			return true
		})
	}
}

// TestTheVendorsFullSafetyStillHasMarginsInIt is the other direction, and
// it is the one a version bump breaks.
//
// SafetyFull is only a safety level because of what is in it: content is
// not collected until it is old, blobs are not deleted until they have
// been unreferenced for a day, and a second GC cycle has to agree with
// the first. If a future pin ships a SafetyFull with those at zero, every
// other test in this package still passes and this product is running
// unsafe maintenance under a safe-sounding name.
func TestTheVendorsFullSafetyStillHasMarginsInIt(t *testing.T) {
	full := maintenance.SafetyFull

	if full.MinContentAgeSubjectToGC <= 0 {
		t.Error("SafetyFull would collect content of any age: a snapshot being written right now is content of no age")
	}

	if full.PackDeleteMinAge <= 0 {
		t.Error("SafetyFull would delete a pack blob the moment it looked unreferenced, with no margin for a stale listing")
	}

	if full.MarginBetweenSnapshotGC <= 0 || !full.RequireTwoGCCycles {
		t.Error("SafetyFull would drop deleted contents from the index after a single GC cycle, with no in-flight snapshot allowed to become visible in between")
	}

	if full.DisableEventualConsistencySafety {
		t.Error("SafetyFull no longer waits for eventually-consistent writes to settle, which is the property a bucket repository depends on")
	}
}

// productionFiles parses every non-test Go file in this package.
func productionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	out := map[string]*ast.File{}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		out[name] = file
	}

	if len(out) == 0 {
		t.Fatal("no production sources found in this package")
	}

	return out
}

// render prints one node back as source, which is how an argument is
// compared against the spelling it must have.
func render(n ast.Node) string {
	switch v := n.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return render(v.X) + "." + v.Sel.Name
	case *ast.CompositeLit:
		return render(v.Type) + "{"
	case *ast.CallExpr:
		return render(v.Fun) + "("
	default:
		return ""
	}
}
