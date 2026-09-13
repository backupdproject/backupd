// The structural half of "retention never prunes repository storage".
//
// kopiablobs_test.go proves it behaviourally, against a real repository,
// by counting what is left in the storage after a pass. That test is the
// evidence; this file is what stops the next change from making it
// obsolete, because the behavioural proof only covers the calls this
// package makes TODAY. A port wide enough to reclaim storage is a port
// somebody eventually reclaims storage through, and the review that would
// have caught it is the review nobody schedules.

package snapshotretention_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/snapshotretention"
)

// TestRetentionsRepositoryPortCannotReachRepositoryStorage pins the method
// set of the only engine surface this package has.
//
// Two methods, named here rather than counted, because the failure this
// guards against is an ADDITION with a plausible name: a Maintain "so the
// pass can tidy up after itself", a Stats "so the preview can show how
// much this would free", a bulk delete "because one call per snapshot is
// slow". Each of those turns backupd's retention from a decision about
// manifests into a second, competing owner of the repository's storage,
// which is exactly the arrangement EPIC K's #785 forbids and #786 exists
// to own.
//
// A widening that is genuinely wanted changes this list, and changing it
// means reading this comment first.
func TestRetentionsRepositoryPortCannotReachRepositoryStorage(t *testing.T) {
	port := reflect.TypeOf((*snapshotretention.Repository)(nil)).Elem()

	got := make([]string, 0, port.NumMethod())
	for i := range port.NumMethod() {
		got = append(got, port.Method(i).Name)
	}
	sort.Strings(got)

	want := []string{"DeleteSnapshot", "LookupSnapshot"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshotretention.Repository has methods %v, want exactly %v.\n"+
			"Retention decides which SNAPSHOT may go; reclaiming the storage behind one is repository maintenance (#786) "+
			"and must not be reachable from a retention pass.", got, want)
	}
}

// TestNothingInThisPackageNamesRepositoryStorage is the same claim made
// against the source rather than the interface, because the interface is
// not the only way to reach an engine: a concrete *kopia.Repository
// accepted as a field, or an unexported helper type-asserting its way to a
// wider one, would leave the port above untouched.
//
// It scans identifiers, not comments and not strings, so this file's own
// prose and the package doc's explanation of what maintenance is may say
// the words freely. What it refuses is a NAME: a call, a field or a type
// in this package that speaks of packs, indexes, blobs or maintenance is
// this package doing something it must not.
func TestNothingInThisPackageNamesRepositoryStorage(t *testing.T) {
	// Only non-test files are scanned, so this pattern can be written out
	// plainly: the samples at the end of this test are what prove it
	// matches anything at all.
	forbidden := regexp.MustCompile(`(?i)(maint|blob|packfile|packblob|reclaim|garbagecollect)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var scanned int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if forbidden.MatchString(id.Name) {
				t.Errorf("%s names %q, which is repository-storage vocabulary: reclaiming storage is maintenance (#786), never retention",
					fset.Position(id.Pos()), id.Name)
			}
			return true
		})
	}

	// The control: a scan that walked no files would pass this silently.
	if scanned < 3 {
		t.Fatalf("scanned %d production files in this package, expected at least the doc, the timeline and the prune path", scanned)
	}

	// And the scanner has to actually match something, or the clean result
	// above says nothing about the sources.
	for _, sample := range []string{"Maintain", "deleteBlob", "reclaimSpace"} {
		if !forbidden.MatchString(sample) {
			t.Errorf("the scanner does not match %q, so its clean result over this package proves nothing", sample)
		}
	}
}
