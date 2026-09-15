package compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Whether this repository's conformance matrices are still describing it,
// or have quietly become prose.
//
// They live in this package because it is the same failure the corpus is
// built around, one level up. A cell that captured nothing passes every
// comparison; a PASS row citing a suite that does not exist reads clean
// and certifies nothing at all. Both are declarations that cannot fail,
// and this repository has shipped that shape before.
//
// So every path a PASS row names is resolved against the tree, and a PASS
// row that cites nothing is refused outright, since a claim with no
// evidence is the cheapest kind to write. BLOCKED rows are exempt from the
// existence check and only from that one: naming a file that does not
// exist yet is what being blocked means, and they still have to name an
// issue, which has to be OPEN. See
// TestEveryBlockedRowCitesAnIssueThatIsStillOpen for why that word is
// load-bearing and what it costs.

// matrixPair is one (spec, matrix) pair the four structural tests below run
// over.
//
// Until #887 this was two constants naming EPIC E's pair, and EPIC R's
// matrix -- same shape, same promises, written in #886 -- was checked by
// nobody at all. That is this package's own failure mode seen from one level
// up: a conformance matrix nothing verifies is a document that cannot fail.
//
// Each field is an assumption that turned out to belong to EPIC E rather
// than to matrices in general, and every one of them was a way for the
// generalisation to read green while inspecting nothing:
//
//   - the row-id shape. EPIC E numbers gate rows P1.n / P2.n and ledger rows
//     Vn; EPIC R numbers them R1.n / R2.n and V.n. A regexp written for one
//     parses ZERO rows out of the other, and a parser that matches nothing
//     looks exactly like a clean table -- which is why an empty parse is
//     fatal at every call site below rather than skipped.
//   - which column carries the outcome. See outcomeOf: the two tables of one
//     matrix already disagree, and the two matrices disagree with each other.
//   - whether the ledger carries a row per spec guard, which is EPIC E's
//     matrix's own promise and not EPIC R's. See
//     TestTheViolationLedgerIsHeldToTheSpecsGuardTable.
//   - whether some row must claim PASS. EPIC R's matrix is entirely BLOCKED
//     the day it lands, deliberately, so that floor is derived from the
//     spec's own ticked boxes rather than asserted outright. See
//     TestTheMatrixDoesNotCiteSuitesThatDoNotExist.
type matrixPair struct {
	// name labels the subtest, so a failure says which EPIC it is about.
	name string
	// spec is the contract the matrix is an account of. Its two exit gates
	// are checkbox lists, one line per gate line, and the matrix carries one
	// row per line of them.
	spec string
	// matrix is the conformance matrix cited by this repository's checks.
	matrix string
	// gateID and ledgerID are the row-id shapes of the two tables that
	// carry outcomes.
	gateID   *regexp.Regexp
	ledgerID *regexp.Regexp
	// gates pairs each row-id prefix with the spec heading whose checkbox
	// list it is numbered against.
	gates []gatePhase
	// ledger is the promise this matrix's own opening paragraph makes about
	// its section 4 ledger.
	ledger ledgerRule
}

// gatePhase is one exit gate: the prefix its matrix rows are numbered with,
// and the spec heading the checkbox list lives under.
type gatePhase struct {
	prefix  string
	heading string
}

// ledgerRule is which promise a matrix makes about its section 4 ledger.
type ledgerRule int

const (
	// ledgerRowPerGuard: one row per entry of the spec's section 4 table, so
	// the rows are held to that table exactly. EPIC E's matrix promises this
	// in its opening paragraph, and a missing row was a guard that was in the
	// ledger nowhere (V9).
	ledgerRowPerGuard ledgerRule = iota
	// ledgerRowPerUncoveredGuard: one row per planted violation the spec
	// names "that no gate line already carries" -- a subset, by
	// construction, and which subset is a judgement the documents make in
	// prose rather than something either of them encodes. What is checked
	// instead is what holds either way: the ledger parses, it is numbered
	// contiguously from 1, and it never claims more rows than the spec names
	// guards.
	ledgerRowPerUncoveredGuard
)

var matrixPairs = []matrixPair{
	{
		name:     "EPIC E",
		spec:     "../../../docs/EPIC-E-alternative-storage.md",
		matrix:   "../../../docs/conformance/epic-e-matrix.md",
		gateID:   regexp.MustCompile(`^P[12]\.\d+$`),
		ledgerID: regexp.MustCompile(`^V\d+$`),
		gates: []gatePhase{
			{prefix: "P1", heading: "### Phase 1 exit gate"},
			{prefix: "P2", heading: "### Phase 2 exit gate"},
		},
		ledger: ledgerRowPerGuard,
	},
	{
		name:     "EPIC R",
		spec:     "../../../docs/EPIC-R-rename-backupd-to-retnd.md",
		matrix:   "../../../docs/conformance/epic-r-matrix.md",
		gateID:   regexp.MustCompile(`^R[12]\.\d+$`),
		ledgerID: regexp.MustCompile(`^V\.\d+$`),
		gates: []gatePhase{
			{prefix: "R1", heading: "### Phase 1 exit gate"},
			{prefix: "R2", heading: "### Phase 2 exit gate"},
		},
		ledger: ledgerRowPerUncoveredGuard,
	},
}

// matrixRepo is the repository whose issues a BLOCKED row cites. It is
// written down rather than inferred from a git remote because the reduced
// trees this suite also runs in (scripts/architecture deletes whole
// layers) are not always full clones, and a check that silently resolved
// against a different repository would answer confidently and wrongly.
const matrixRepo = "backupdproject/backupd"

// backtickPath matches a `like/this` span that looks like a repository
// path: it has a slash and no spaces.
var backtickPath = regexp.MustCompile("`([A-Za-z0-9_./-]+/[A-Za-z0-9_./-]+)`")

// issueCitation matches a `#123` issue reference in an outcome cell.
var issueCitation = regexp.MustCompile(`#(\d+)`)

// TestTheMatrixDoesNotCiteSuitesThatDoNotExist keeps
// docs/conformance/epic-e-matrix.md from being prose.
//
// The matrix's whole value is that PASS means "there is a check, it runs,
// and it has been watched to fail". A PASS row citing a file the
// repository does not have is the same failure as the phase 4 matrix
// shrinking when a capability is omitted: the declaration reads clean and
// certifies nothing. So every path a PASS row names is resolved against
// the tree, and a row that cites nothing at all is refused too, because a
// PASS with no evidence is the cheapest kind to write.
//
// BLOCKED rows are deliberately exempt from the existence check: they name
// where a check WILL live, and that path not existing yet is the whole
// reason they are blocked. What they are not exempt from is naming an open
// issue, which is TestEveryBlockedRowCitesAnIssueThatIsStillOpen's job.
//
// # What replaced the blocked floor
//
// This test used to end with `if blocked == 0 { t.Error(...) }`, a floor
// asserting that SOME row was still blocked. It was true when it was
// written and it barred the finished state: the day the last row earned
// its PASS, the check that exists to keep the matrix honest would have
// failed for the matrix being complete, and whoever hit that would have
// deleted the line rather than replaced it. #522 replaced it with the
// thing the floor was standing in for, which is that the matrix has to
// keep having a row per gate line. A row cannot be quietly dropped to
// tidy the table, and the parser silently matching nothing still fails,
// because the ids are compared against the SPEC's own exit-gate lines
// rather than against a number written down here.
func TestTheMatrixDoesNotCiteSuitesThatDoNotExist(t *testing.T) {
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}

	for _, pair := range matrixPairs {
		t.Run(pair.name, func(t *testing.T) {
			blob, err := os.ReadFile(pair.matrix)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.matrix, err)
			}
			spec, err := os.ReadFile(pair.spec)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.spec, err)
			}

			rows := gateRows(string(blob), pair)
			if len(rows) == 0 {
				t.Fatalf("no gate rows parsed out of %s, so this check inspected nothing", pair.matrix)
			}

			var (
				pass, partial, blocked int
				missing                []string
			)
			for _, row := range rows {
				switch {
				case strings.HasPrefix(row.outcome, "PASS"):
					pass++
				case strings.HasPrefix(row.outcome, "PARTIAL"):
					partial++
				case strings.HasPrefix(row.outcome, "BLOCKED"):
					blocked++
					continue
				default:
					t.Errorf("row %s has outcome %q, which is not one of PASS, PARTIAL or BLOCKED", row.id, row.outcome)
					continue
				}

				cited := backtickPath.FindAllStringSubmatch(row.where, -1)
				if len(cited) == 0 {
					t.Errorf("row %s claims %s and cites no file at all, so there is nothing to check it against", row.id, row.outcome)
					continue
				}
				for _, m := range cited {
					// A citation whose whole top-level directory is absent is
					// not a matrix that has gone stale, it is this suite
					// running inside a deliberately reduced tree.
					// scripts/architecture's dependency proofs delete apps/
					// and distribution/ outright and then run core/'s tests,
					// to show core builds without them, and a row citing
					// evidence in apps/ has nothing to say about that.
					//
					// The distinction is exact rather than forgiving: if the
					// directory IS there and the file is not, that is a stale
					// citation and still fails. Only the wholesale absence of
					// a layer is treated as "not this tree's question", and
					// it is logged so a run that skipped a check says so out
					// loud rather than passing quietly.
					if top := topLevelDir(m[1]); top != "" {
						if _, err := os.Stat(filepath.Join(repoRoot, top)); err != nil {
							t.Logf("row %s cites %s and %s/ is not in this tree at all, so this is a reduced tree (the dependency-rule proof deletes whole layers) and the citation is not checked here", row.id, m[1], top)
							continue
						}
					}
					p := filepath.Join(repoRoot, m[1])
					if _, err := os.Stat(p); err != nil {
						missing = append(missing, row.id+" cites "+m[1])
					}
				}
			}

			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%d citation(s) in %s name something this repository does not have:\n  %s",
					len(missing), pair.matrix, strings.Join(missing, "\n  "))
			}

			// The floor: a matrix with a ticked exit-gate box and no PASS row
			// anywhere is a matrix whose outcomes stopped being read.
			//
			// This used to be a bare `pass == 0`, which was true of EPIC E
			// and false as a rule: EPIC R's matrix is entirely BLOCKED on the
			// day it lands, by design and stated in its own header, and the
			// bare floor would have failed it for being honest -- the same
			// mistake as the `blocked == 0` floor this replaced, which barred
			// the finished state. So the floor is derived from the spec
			// instead: while nothing is ticked there is nothing to certify,
			// and the moment anything is, some row has to claim PASS.
			// TestTheSpecsExitGateBoxesAgreeWithTheMatrix holds the other
			// direction, row by row.
			ticked := 0
			for _, gate := range pair.gates {
				for _, line := range specGateLines(string(spec), gate.heading) {
					if strings.HasPrefix(line, "- [x]") {
						ticked++
					}
				}
			}
			if ticked > 0 && pass == 0 {
				t.Errorf("%s ticks %d exit-gate box(es) and no row in %s claims PASS, which means either nothing is checked or the parser stopped working; both are worth failing over",
					pair.spec, ticked, pair.matrix)
			}
			t.Logf("%s rows: %d PASS, %d PARTIAL, %d BLOCKED (%d ticked box(es) in %s)", pair.matrix, pass, partial, blocked, ticked, pair.spec)
		})
	}
}

// TestTheMatrixHasARowPerGateLine is the replacement for the blocked
// floor, and it is aimed at the same failure from the side that stays true
// when the work is finished.
//
// The matrix opens by promising "one row per line of the spec's two exit
// gates, with nothing dropped for not being ready yet". That promise is
// what the floor was really protecting: a row nobody can make green is the
// row most worth deleting, and deleting it leaves a matrix that reads
// complete. So the row ids are compared against the SPEC's own exit-gate
// checkbox lists, which is a count nobody maintains by hand, and they have
// to be contiguous from 1, so a row cannot go missing out of the middle
// either.
func TestTheMatrixHasARowPerGateLine(t *testing.T) {
	for _, pair := range matrixPairs {
		t.Run(pair.name, func(t *testing.T) {
			matrix, err := os.ReadFile(pair.matrix)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.matrix, err)
			}
			spec, err := os.ReadFile(pair.spec)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.spec, err)
			}

			byPhase := map[string][]string{}
			for _, row := range gateRows(string(matrix), pair) {
				phase := row.id[:strings.Index(row.id, ".")]
				byPhase[phase] = append(byPhase[phase], row.id)
			}

			for _, gate := range pair.gates {
				lines := specGateLines(string(spec), gate.heading)
				if len(lines) == 0 {
					t.Errorf("no checkbox lines parsed out of %q in %s, so this check inspected nothing for %s", gate.heading, pair.spec, gate.prefix)
					continue
				}
				want := make([]string, 0, len(lines))
				for i := range lines {
					want = append(want, fmt.Sprintf("%s.%d", gate.prefix, i+1))
				}
				got := append([]string(nil), byPhase[gate.prefix]...)
				sort.Strings(got)
				sort.Strings(want)
				if !sameStrings(got, want) {
					t.Errorf("%s names %d line(s) under %q and %s carries rows %v; it has to carry one row per line (%v), because the row nobody can make green is the row most worth deleting",
						pair.spec, len(lines), gate.heading, pair.matrix, byPhase[gate.prefix], want)
				}
			}
		})
	}
}

// TestTheViolationLedgerIsHeldToTheSpecsGuardTable is the same promise as
// TestTheMatrixHasARowPerGateLine, aimed at the other table.
//
// Section 4 of a spec is where every guard the EPIC adds names the mutation
// that has to make it fire, so the ledger's rows are counted against that
// table rather than against a number written down here.
//
// It doubles as the coverage check for the parser two tables share. The
// citation guard reads ledger rows through outcomeRows, and the reason it
// has to is that gateRows never did: V4 and V8 spent months BLOCKED citing
// closed issues while the old check looked straight past them. A parser that
// stopped seeing that table again would leave the guard reading clean and
// checking nothing, which is a failure with no symptom, so the rows are
// counted rather than assumed.
//
// It found one on the way in. EPIC E's spec names nine guards and its matrix
// carried eight: FR-29's "a migration variant that backfills every artifact
// row" had no row at all, so the one guard about a backfill that hands a
// `.partial` an ACTIVE placement, which is what lets a move delete a source
// against an incomplete copy, was in the ledger nowhere. V9 is that row, and
// it is V9 rather than V8 (where the spec puts that guard) because
// renumbering an existing row breaks every reference anybody has written to
// it, #522 included.
//
// # Why the rule is per pair
//
// This test used to require one ledger row per spec guard, full stop, and
// that is EPIC E's matrix's own promise rather than a property of matrices.
// EPIC R's opens by promising one row "per planted violation the spec names
// that no gate line already carries": its spec's section 4 names eighteen
// guards, twelve of which its gate rows already carry, and its ledger is the
// six that are left. Held to EPIC E's rule it would have to restate those
// twelve, which is how a table gets padded with rows nobody reads.
//
// Which gate row covers which guard is a judgement both documents make in
// prose, so it cannot be computed here. What can, and what #887 checks
// instead for that shape of matrix, is everything that stays true either
// way: the ledger parses at all, its numbering is contiguous from 1 so a row
// cannot go missing out of the middle, and it never claims more rows than the
// spec names guards.
func TestTheViolationLedgerIsHeldToTheSpecsGuardTable(t *testing.T) {
	for _, pair := range matrixPairs {
		t.Run(pair.name, func(t *testing.T) {
			matrix, err := os.ReadFile(pair.matrix)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.matrix, err)
			}
			spec, err := os.ReadFile(pair.spec)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.spec, err)
			}

			var ids []string
			var nums []int
			for _, row := range outcomeRows(string(matrix), pair) {
				if !pair.ledgerID.MatchString(row.id) {
					continue
				}
				ids = append(ids, row.id)
				n, ok := ledgerNumber(row.id)
				if !ok {
					t.Errorf("ledger row %q in %s does not end in a number, so it cannot be held to the spec's guard table", row.id, pair.matrix)
					continue
				}
				nums = append(nums, n)
			}
			if len(nums) == 0 {
				t.Fatalf("no section 4 violation row was parsed out of %s at all, so the citation guard is reading one of its two tables and calling it both", pair.matrix)
			}

			guards := specGuardRows(string(spec))
			if len(guards) == 0 {
				t.Fatalf("no guard rows parsed out of %s's section 4 table, so this check has nothing to compare against", pair.spec)
			}

			// How long the run of ids has to be. Per guard for a matrix that
			// promises one row each; otherwise as many as it carries, which
			// still has to be a run from 1 and still cannot exceed the
			// guards the spec names.
			wantLen := len(nums)
			if pair.ledger == ledgerRowPerGuard {
				wantLen = len(guards)
			} else if len(nums) > len(guards) {
				t.Errorf("%s names %d guard(s) in its section 4 table and the ledger in %s carries %d row(s) (%v); a ledger row that is not one of the spec's guards is a row nothing certifies",
					pair.spec, len(guards), pair.matrix, len(nums), ids)
			}

			sort.Ints(nums)
			want := make([]int, 0, wantLen)
			for i := 1; i <= wantLen; i++ {
				want = append(want, i)
			}
			if !sameInts(nums, want) {
				t.Errorf("%s names %d guard(s) in its section 4 table and the ledger in %s carries %v; the rows have to be numbered %v, contiguously, because the row nobody can make green is the row most worth deleting. The guards the spec names are:\n  %s",
					pair.spec, len(guards), pair.matrix, ids, want, strings.Join(guards, "\n  "))
			}
		})
	}
}

// TestEveryBlockedRowCitesAnIssueThatIsStillOpen is the citation guard, and
// the word that matters in its name is "open".
//
// It used to be one line inside the test above: a BLOCKED outcome had to
// contain a "#". That check could not fail in the way it needed to. P1.3,
// P1.4 and P1.5 cited #235, P1.7 cited #237, P1.8 cited all three, and
// every one of those issues had been closed for weeks. The rows read as
// "somebody is working on this" and pointed at work nobody could pick up,
// and EPIC E was closed with seven of them standing (#522). A citation is
// not evidence of a live blocker unless somebody checks the issue is live.
//
// Nothing in this repository can know that. Closing an issue changes
// nothing in the tree, which is exactly why the drift was invisible, so
// this asks GitHub. That has two consequences worth stating rather than
// discovering:
//
//   - it only asks when a row is actually BLOCKED. A matrix with none, which
//     is the state #522 left it in, touches no network at all;
//   - when a row IS blocked and GitHub cannot be reached, this FAILS. A
//     BLOCKED row whose citation nobody could resolve is precisely the state
//     this check exists to refuse, and a skip there would read as a pass,
//     which is the failure mode the whole conformance matrix is built
//     around. core/tests/machines takes the same line about a missing
//     container image, deliberately and for the same reason (#160).
//
// Its ability to fail is proven twice: offline on every run, against stub
// resolvers, in TestTheBlockedCitationGuardCanFail; and through the real
// `gh` wiring by scripts/conformance/selftest.sh, which points a PASS row
// at #235 and requires this to say so.
func TestEveryBlockedRowCitesAnIssueThatIsStillOpen(t *testing.T) {
	for _, pair := range matrixPairs {
		t.Run(pair.name, func(t *testing.T) {
			blob, err := os.ReadFile(pair.matrix)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.matrix, err)
			}

			rows := blockedRows(string(blob), pair)
			if len(rows) == 0 {
				t.Logf("no row in %s is BLOCKED, so there is no citation to resolve and GitHub is not asked", pair.matrix)
				return
			}

			for _, complaint := range blockedCitationComplaints(rows, ghIssueState) {
				t.Error(complaint)
			}
		})
	}
}

// TestTheBlockedCitationGuardCanFail is the offline half, and it is the
// half that runs on every gate.
//
// The guard above resolves issues against GitHub, so on a matrix with
// nothing blocked it never executes a single comparison. An assertion that
// usually inspects nothing is the shape this repository keeps getting
// caught by, so the decision it makes is a pure function over a resolver
// and it is exercised here in all four directions: an open citation is
// silent, a closed one complains and names the issue, a citation nobody
// could resolve complains rather than passing, and a BLOCKED row citing
// nothing at all complains too.
func TestTheBlockedCitationGuardCanFail(t *testing.T) {
	rows := []outcomeRow{{id: "P1.3", outcome: "BLOCKED (#235)", issues: []int{235}}}

	t.Run("a closed issue is refused", func(t *testing.T) {
		got := blockedCitationComplaints(rows, func(int) (string, error) { return "CLOSED", nil })
		if len(got) != 1 {
			t.Fatalf("complaints = %v, want exactly one", got)
		}
		for _, want := range []string{"P1.3", "#235", "CLOSED"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("the complaint %q does not name %q, so a reader cannot act on it", got[0], want)
			}
		}
	})

	t.Run("an open issue is accepted", func(t *testing.T) {
		if got := blockedCitationComplaints(rows, func(int) (string, error) { return "OPEN", nil }); len(got) != 0 {
			t.Errorf("complaints = %v for a BLOCKED row citing an OPEN issue, want none", got)
		}
	})

	t.Run("a citation that could not be resolved is refused, not skipped", func(t *testing.T) {
		got := blockedCitationComplaints(rows, func(int) (string, error) { return "", errors.New("gh: not found") })
		if len(got) != 1 {
			t.Fatalf("complaints = %v, want exactly one; a BLOCKED row nobody could check is the state this guard exists to refuse", got)
		}
		if !strings.Contains(got[0], "#235") {
			t.Errorf("the complaint %q does not name the issue it could not resolve", got[0])
		}
	})

	t.Run("a BLOCKED row citing nothing is refused", func(t *testing.T) {
		bare := []outcomeRow{{id: "V4", outcome: "BLOCKED"}}
		got := blockedCitationComplaints(bare, func(int) (string, error) {
			t.Error("the resolver was called for a row that cites no issue")
			return "OPEN", nil
		})
		if len(got) != 1 {
			t.Fatalf("complaints = %v, want exactly one", got)
		}
	})
}

// TestTheSpecsExitGateBoxesAgreeWithTheMatrix stops the spec's checkboxes
// from drifting away from the evidence again.
//
// Both exit gates in docs/EPIC-E-alternative-storage.md sat entirely
// unticked long after phase 2 landed, which is the same defect as a
// BLOCKED row citing closed work seen from the other end: the document
// somebody reads to find out where the EPIC stands was stale, and nothing
// in the repository could tell. The matrix already carries an outcome per
// gate line, watched, so the boxes are held to it: a PASS row's box is
// ticked, and a row that is anything else is not. PARTIAL is deliberately
// on the untick side. A box is one bit and a PARTIAL row is a paragraph
// about which half holds, so ticking one would be the more misleading of
// the two answers.
func TestTheSpecsExitGateBoxesAgreeWithTheMatrix(t *testing.T) {
	for _, pair := range matrixPairs {
		t.Run(pair.name, func(t *testing.T) {
			matrix, err := os.ReadFile(pair.matrix)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.matrix, err)
			}
			spec, err := os.ReadFile(pair.spec)
			if err != nil {
				t.Fatalf("reading %s: %v", pair.spec, err)
			}

			outcome := map[string]string{}
			for _, row := range gateRows(string(matrix), pair) {
				outcome[row.id] = row.outcome
			}

			for _, gate := range pair.gates {
				for i, line := range specGateLines(string(spec), gate.heading) {
					id := fmt.Sprintf("%s.%d", gate.prefix, i+1)
					got, ok := outcome[id]
					if !ok {
						// TestTheMatrixHasARowPerGateLine owns this failure
						// and says it better; saying it twice would make one
						// missing row look like two problems.
						continue
					}
					ticked := strings.HasPrefix(line, "- [x]")
					want := strings.HasPrefix(got, "PASS")
					if ticked == want {
						continue
					}
					if want {
						t.Errorf("%s is %q in %s and its box under %q is not ticked; the spec is the document somebody reads to find out where this EPIC stands:\n  %s",
							id, got, pair.matrix, gate.heading, truncate(line, 160))
						continue
					}
					t.Errorf("%s is %q in %s and its box under %q is ticked, which claims more than anything has been watched to prove:\n  %s",
						id, got, pair.matrix, gate.heading, truncate(line, 160))
				}
			}
		})
	}
}

type matrixRow struct {
	id      string
	outcome string
	where   string
}

// outcomeRow is one row of the matrix that declares an outcome, and the
// issues that outcome cites.
type outcomeRow struct {
	id      string
	outcome string
	issues  []int
}

// blockedCitationComplaints is the decision the citation guard makes,
// separated from where the answers come from so it can be exercised
// against a resolver that cannot reach anything.
//
// An issue is resolved once per run rather than once per row, because
// P1.8 cited three of them and rows share citations by design.
func blockedCitationComplaints(rows []outcomeRow, resolve func(int) (string, error)) []string {
	type answer struct {
		state string
		err   error
	}
	seen := map[int]answer{}
	var out []string

	for _, row := range rows {
		if len(row.issues) == 0 {
			out = append(out, fmt.Sprintf(
				"row %s is BLOCKED and names no issue, so nobody can tell who unblocks it: %q", row.id, row.outcome))
			continue
		}
		for _, n := range row.issues {
			a, ok := seen[n]
			if !ok {
				state, err := resolve(n)
				a = answer{state: state, err: err}
				seen[n] = a
			}
			switch {
			case a.err != nil:
				out = append(out, fmt.Sprintf(
					"row %s is BLOCKED citing #%d and this check could not find out whether #%d is open: %v. "+
						"A BLOCKED row nobody can resolve is the state this guard exists to refuse, so it fails here rather than passing quietly",
					row.id, n, n, a.err))
			case !strings.EqualFold(a.state, "OPEN"):
				out = append(out, fmt.Sprintf(
					"row %s is BLOCKED citing #%d, and #%d is %s. A BLOCKED row has to point at work somebody can pick up; "+
						"citing closed work is how seven rows of this matrix stayed BLOCKED after the code they certify had landed (#522)",
					row.id, n, n, strings.ToUpper(a.state)))
			}
		}
	}
	return out
}

// ghIssueState asks GitHub what state an issue is in.
//
// Through the `gh` CLI rather than the REST API directly, because this
// repository is private and gh is where the credential for it already
// lives; nothing here holds, reads or writes a token.
func ghIssueState(number int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "issue", "view", strconv.Itoa(number),
		"--repo", matrixRepo, "--json", "state", "--jq", ".state")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("gh issue view %d --repo %s: %s", number, matrixRepo, detail)
	}
	state := strings.TrimSpace(stdout.String())
	if state == "" {
		return "", fmt.Errorf("gh issue view %d --repo %s answered with nothing", number, matrixRepo)
	}
	return state, nil
}

// gateRows pulls the two exit-gate tables out of one pair's matrix.
//
// It keys on the row id shape (P1.n / P2.n for EPIC E, R1.n / R2.n for
// EPIC R) rather than on table position, so reordering the document, or
// adding prose between the tables, does not silently empty this check. The
// shape comes from the pair rather than from a literal here, because a
// regexp written for one epic's numbering parses ZERO rows out of the
// other's and reads exactly like a clean table while doing it. An empty
// result is a failure at every call site, which is what makes that safe
// rather than merely tidy.
func gateRows(md string, pair matrixPair) []matrixRow {
	var out []matrixRow
	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cols := splitRow(line)
		if len(cols) < 5 {
			continue
		}
		id := cols[0]
		if !pair.gateID.MatchString(id) {
			continue
		}
		// Both matrices put Where fourth; the outcome is found rather than
		// counted, for the reason outcomeOf gives.
		out = append(out, matrixRow{id: id, outcome: outcomeOf(cols), where: cols[3]})
	}
	return out
}

// outcomeRows collects every row of one pair's matrix that declares an
// outcome, from BOTH tables that carry one.
//
// The section 4 planted-violation table is here as well as the two exit
// gates, and that is not tidiness: V4 and V8 were BLOCKED citing #235 and
// #237 the entire time, and gateRows never looked at them, so the old
// citation check was not weak about those two rows, it was absent.
func outcomeRows(md string, pair matrixPair) []outcomeRow {
	var out []outcomeRow

	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cols := splitRow(line)
		switch {
		case len(cols) >= 5 && pair.gateID.MatchString(cols[0]):
		case len(cols) >= 4 && pair.ledgerID.MatchString(cols[0]):
		default:
			continue
		}
		outcome := outcomeOf(cols)
		row := outcomeRow{id: cols[0], outcome: outcome}
		for _, m := range issueCitation.FindAllStringSubmatch(outcome, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			row.issues = append(row.issues, n)
		}
		out = append(out, row)
	}
	return out
}

// outcomeOf is the cell of a row that declares an outcome.
//
// It is found rather than counted, and #887 is why. The two tables of one
// matrix are already different shapes -- EPIC E's gate rows have five
// columns with the outcome second and its ledger rows four with the outcome
// last -- and EPIC R's ledger has five columns with the outcome second,
// so "the last column" and "the second column" are each right for some
// table and wrong for another. Position is not a stable property across
// documents; starting with an outcome word is, and a cell that starts with
// one is not describing a falsification.
func outcomeOf(cols []string) string {
	for _, col := range cols[1:] {
		for _, word := range []string{"PASS", "PARTIAL", "BLOCKED"} {
			if strings.HasPrefix(col, word) {
				return col
			}
		}
	}
	// No cell declares one. Handing back the column the gate tables keep it
	// in lets the caller's "not one of PASS, PARTIAL or BLOCKED" complaint
	// name what was actually there.
	if len(cols) > 1 {
		return cols[1]
	}
	return ""
}

// ledgerNumber is the n of a ledger row id: V9 -> 9, V.6 -> 6.
func ledgerNumber(id string) (int, bool) {
	m := ledgerTail.FindStringSubmatch(id)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

var ledgerTail = regexp.MustCompile(`(\d+)$`)

// blockedRows is outcomeRows narrowed to the declarations the citation
// guard is about.
func blockedRows(md string, pair matrixPair) []outcomeRow {
	var out []outcomeRow
	for _, row := range outcomeRows(md, pair) {
		if strings.HasPrefix(row.outcome, "BLOCKED") {
			out = append(out, row)
		}
	}
	return out
}

// specGateLines returns the checkbox lines of one exit gate in the spec,
// in the order the spec writes them, which is the order the matrix
// numbers its rows in.
func specGateLines(md, heading string) []string {
	var out []string
	inside := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == heading {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			break
		}
		if strings.HasPrefix(trimmed, "- [ ]") || strings.HasPrefix(trimmed, "- [x]") {
			out = append(out, trimmed)
		}
	}
	return out
}

// specGuardRows returns the Guard column of the spec's section 4
// planted-violation table, in the order the spec writes them, which is the
// order the matrix numbers V1..Vn in.
//
// It keys on the table's own header rather than on a section heading,
// because the table is the thing being counted and a heading somebody
// renames would silently empty this. An empty result is a failure at the
// call site.
func specGuardRows(md string) []string {
	var out []string
	inside := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inside {
			if strings.HasPrefix(trimmed, "| Guard | Planted violation") {
				inside = true
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "|") {
			break
		}
		cols := splitRow(trimmed)
		if len(cols) < 2 || strings.Trim(cols[0], "- ") == "" {
			continue
		}
		out = append(out, cols[0])
	}
	return out
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// topLevelDir is the first path segment of a repository-relative citation,
// or "" when the citation names a file at the root.
func topLevelDir(rel string) string {
	rel = filepath.ToSlash(rel)
	if i := strings.Index(rel, "/"); i > 0 {
		return rel[:i]
	}
	return ""
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
