// Package workflowgate_test holds EPIC L's conformance matrix to the tree
// it makes claims about (#812).
//
// The matrix at docs/conformance/epic-l-matrix.md is the one artefact of
// this gate that is prose, and #812's technical requirement is that no
// gate item may be satisfied by prose alone. So the prose is checked: this
// package reads the matrix, and refuses it unless
//
//   - every one of the eleven adversarial-consensus findings #807 lists is
//     present as a row, by id and by the words the epic uses for it. A
//     consensus row that quietly stopped being tracked is the failure the
//     whole matrix exists to prevent, and a matrix that only had to be
//     self-consistent would let a row be deleted rather than answered;
//   - every row names at least one executable proof, and every proof it
//     names RESOLVES: a `<package dir>:<TestName>` reference has to be a
//     Go test function that exists in that directory today, and a bare
//     path has to be a file that exists. This is what makes a row a claim
//     about the tree rather than about a test somebody meant to write. A
//     test renamed by a later refactor turns the row red instead of
//     leaving a matrix that cites nothing;
//   - every row carries a falsification, because a green cell nobody has
//     watched fail is a cell nobody has checked;
//   - a row claiming PASS carries both. BLOCKED is allowed and needs a
//     reason, which is how a row whose code has not landed is recorded
//     honestly.
//
// The checker is a function over text plus a resolver, so the whole of it
// can be run against deliberately broken matrices. That is
// TestTheMatrixCheckerRefusesEveryWayAMatrixCanLie, and it is the mutation
// self-test docs/epic-checklist.md section 12 requires of a new guard:
// without it this file would be a guard whose only evidence is that it
// passes on the tree it shipped with.
package workflowgate_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// matrixPath is the matrix, relative to the repository root.
const matrixPath = "docs/conformance/epic-l-matrix.md"

// consensusRows are the eleven findings of #807's adversarial-review
// consensus table, in the order the epic lists them, each with a word that
// has to appear in the row's own claim.
//
// They are restated here rather than parsed out of the issue because the
// issue is not in the tree: a checkout has to be able to run this gate.
// The keyword is deliberately a distinctive noun from the finding rather
// than a whole sentence -- holding the matrix to an exact transcription
// would make a clearer wording a red build, while a row about the wrong
// subject is what this catches.
var consensusRows = []struct {
	id      string
	keyword string
	about   string
}{
	{"CR-01", "runner", "a distroless engine cannot honestly run a local shell, so local hooks go through the Host Workflow Runner"},
	{"CR-02", "exec-capable", "an SFTP-only or forced-command credential is refused for hooks and stays valid for backup"},
	{"CR-03", "recovery_required", "a crash after a before step leaves a durable obligation, immutable script copies and a blocked set"},
	{"CR-04", "pipefail", "fixed bash --noprofile --norc, the four startup variables sanitized, and no injected shell option"},
	{"CR-05", "unconfirmed", "termination certainty: a timeout or cancel records what was proved"},
	{"CR-06", "backpressure", "a follower never backpressures a script; persist first, fan out asynchronously, replay by cursor"},
	{"CR-07", "logical", "one logical terminal per step, only the selected viewer mounted"},
	{"CR-08", "OSC", "script output is untrusted terminal input"},
	{"CR-09", "sequence", "stdout and stderr keep their identity and share one capture sequence"},
	{"CR-10", "basename", "conservative ASCII basenames, regular files only, symlinks refused"},
	{"CR-11", "spool", "immutable run-scoped script copies in the protected spool until the run is terminal"},
}

// A row of the matrix table.
type row struct {
	id     string
	claim  string
	proofs []string
	falsif string
	status string
	line   int
}

// proofRef matches a backticked proof reference. Two shapes are allowed
// and the difference matters: `dir:TestName` is a Go test this checker
// resolves by reading that directory, and `path/to/file` is any other
// executable proof (a shell guard, a fixture) that only has to exist.
var proofRef = regexp.MustCompile("`([^`]+)`")

// goFuncDecl finds a Go test or benchmark declaration by name.
func goFuncDecl(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(`)
}

// parseMatrix reads the rows out of the matrix's markdown tables.
//
// It reads EVERY table in the file rather than one, because the matrix is
// organised by gate class and splitting it into one table per class is a
// readability decision that must not change what is checked.
func parseMatrix(text string) []row {
	var rows []row

	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := splitRow(trimmed)
		if len(cells) < 5 {
			continue
		}
		// The header and its separator are not rows. A row's first cell
		// is an id, and an id is what the consensus list and the gate
		// classes are keyed on.
		id := cells[0]
		if !regexp.MustCompile(`^(CR|GC)-[0-9]{2}[a-z]?$`).MatchString(id) {
			continue
		}

		var proofs []string
		for _, m := range proofRef.FindAllStringSubmatch(cells[2], -1) {
			proofs = append(proofs, strings.TrimSpace(m[1]))
		}

		rows = append(rows, row{
			id:     id,
			claim:  cells[1],
			proofs: proofs,
			falsif: cells[3],
			status: strings.ToUpper(cells[4]),
			line:   i + 1,
		})
	}

	return rows
}

// splitRow splits one markdown table row into its cells.
func splitRow(line string) []string {
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")

	parts := strings.Split(line, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}

	return cells
}

// resolver answers whether one proof reference names something that
// exists. It is a parameter so the checker can be run against a tree that
// is not this one.
type resolver func(ref string) (bool, string)

// checkMatrix returns one finding per way the matrix does not hold. An
// empty result is the gate passing.
func checkMatrix(rows []row, resolve resolver) []string {
	var findings []string

	byID := map[string]row{}
	for _, r := range rows {
		if _, duplicate := byID[r.id]; duplicate {
			findings = append(findings, r.id+" appears twice, so one of the two is not being read")
		}
		byID[r.id] = r
	}

	for _, want := range consensusRows {
		got, ok := byID[want.id]
		if !ok {
			findings = append(findings,
				want.id+" ("+want.about+") has no row at all: every finding in #807's consensus table has to map to at least one executable test")

			continue
		}
		if !strings.Contains(strings.ToLower(got.claim), strings.ToLower(want.keyword)) {
			findings = append(findings,
				got.id+"'s claim does not mention "+want.keyword+", so it is not the consensus row it is numbered as: "+want.about)
		}
	}

	for _, r := range rows {
		switch {
		case len(r.proofs) == 0 && r.status != "BLOCKED":
			findings = append(findings, r.id+" names no executable proof, and only a BLOCKED row may have none")
		case r.falsif == "" || r.falsif == "-":
			findings = append(findings, r.id+" carries no falsification, so nobody can say what would turn it red")
		}
		if r.status != "PASS" && r.status != "BLOCKED" {
			findings = append(findings, r.id+" has status "+r.status+"; a row is PASS once its falsification has been watched to fail, or BLOCKED with a reason, and nothing else")
		}
		for _, ref := range r.proofs {
			if ok, why := resolve(ref); !ok {
				findings = append(findings, r.id+" cites "+ref+", which does not resolve: "+why)
			}
		}
	}

	sort.Strings(findings)

	return findings
}

// requiredStatements are the sentences the matrix has to carry in its own
// prose, because #812 asks for the trust boundary to be DOCUMENTED and a
// documented boundary that can be deleted without anything noticing is a
// paragraph rather than a deliverable.
//
// Substrings rather than whole paragraphs, for consensusRows' reason: the
// wording is allowed to improve, and what must not happen is the claim
// disappearing.
var requiredStatements = []struct {
	text  string
	about string
}{
	{"code-execution authority", "who a script author is: somebody who already has it, which is what the rest of the gate is scoped against"},
	{"Redaction cannot protect a value an arbitrary script deliberately", "the documented limit of redaction, so nobody reads a green redaction row as a promise about a hostile script"},
	{"outside the guarantee", "the documented limit of termination certainty for a detached remote descendant"},
}

// checkStatements returns one finding per documented statement the matrix
// has stopped making.
func checkStatements(text string) []string {
	var findings []string

	// Compared with runs of whitespace collapsed, because the file is
	// hard-wrapped prose: a sentence that moved across a line break is
	// the same sentence, and a check that went red on a rewrap would
	// teach people to delete it.
	flat := strings.Join(strings.Fields(text), " ")

	if !strings.Contains(flat, "## Trust boundary") {
		findings = append(findings, "the matrix has no `## Trust boundary` section, and #812 asks for that boundary to be documented")
	}
	for _, want := range requiredStatements {
		if !strings.Contains(flat, strings.Join(strings.Fields(want.text), " ")) {
			findings = append(findings, "the matrix no longer states "+want.text+", which is "+want.about)
		}
	}

	return findings
}

// repoResolver resolves a reference against the repository at root.
func repoResolver(root string) resolver {
	return func(ref string) (bool, string) {
		dir, name, isGo := strings.Cut(ref, ":")
		if !isGo {
			if _, err := os.Stat(filepath.Join(root, ref)); err != nil {
				return false, "no file at that path"
			}

			return true, ""
		}

		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			return false, "no package directory " + dir
		}
		decl := goFuncDecl(name)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(root, dir, e.Name()))
			if err != nil {
				continue
			}
			if decl.Match(body) {
				return true, ""
			}
		}

		return false, "no func " + name + " in " + dir
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("%s does not look like the repository root: %v", root, err)
	}

	return root
}

// TestEveryConsensusFindingMapsToATestThatExists is the gate.
func TestEveryConsensusFindingMapsToATestThatExists(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	text, err := os.ReadFile(filepath.Join(root, matrixPath))
	if err != nil {
		t.Fatalf("reading the conformance matrix: %v", err)
	}

	rows := parseMatrix(string(text))
	if len(rows) < len(consensusRows) {
		t.Fatalf("the matrix parsed to %d rows and #807's consensus table alone has %d; either the table shape changed or this checker is reading nothing",
			len(rows), len(consensusRows))
	}

	findings := checkMatrix(rows, repoResolver(root))
	findings = append(findings, checkStatements(string(text))...)
	if len(findings) != 0 {
		t.Errorf("%s does not hold against this tree. %d finding(s):\n\n  %s",
			matrixPath, len(findings), strings.Join(findings, "\n  "))
	}
}

// TestTheMatrixCheckerRefusesEveryWayAMatrixCanLie is the mutation
// self-test. Each case is a matrix that is wrong in exactly one way, and
// the checker has to say so; the last case is the control that the same
// checker accepts the honest version, because a checker that refused
// everything would pass every row above for the wrong reason.
func TestTheMatrixCheckerRefusesEveryWayAMatrixCanLie(t *testing.T) {
	t.Parallel()

	// A minimal honest matrix: every consensus row, one resolvable proof
	// each, a falsification each, all PASS.
	honest := func() []row {
		var rows []row
		for _, c := range consensusRows {
			rows = append(rows, row{
				id:     c.id,
				claim:  "a claim about " + c.keyword,
				proofs: []string{"pkg:TestSomething"},
				falsif: "delete the check and watch it go red",
				status: "PASS",
			})
		}

		return rows
	}

	// A resolver that accepts exactly the one reference the honest
	// matrix uses, so a mutation that changes the reference is caught by
	// resolution rather than by a string comparison in the test.
	resolve := func(ref string) (bool, string) {
		if ref == "pkg:TestSomething" {
			return true, ""
		}

		return false, "no func in pkg"
	}

	for _, mutation := range []struct {
		name   string
		break_ func(rows []row) []row
		says   string
	}{
		{
			name:   "a consensus finding has no row",
			break_: func(rows []row) []row { return rows[1:] },
			says:   "has no row at all",
		},
		{
			name: "a row is numbered as a finding it is not about",
			break_: func(rows []row) []row {
				rows[3].claim = "something else entirely"

				return rows
			},
			says: "is not the consensus row it is numbered as",
		},
		{
			name: "a row names no test",
			break_: func(rows []row) []row {
				rows[2].proofs = nil

				return rows
			},
			says: "names no executable proof",
		},
		{
			name: "a row names a test that does not exist",
			break_: func(rows []row) []row {
				rows[5].proofs = []string{"pkg:TestNobodyWrote"}

				return rows
			},
			says: "which does not resolve",
		},
		{
			name: "a row claims PASS with no falsification",
			break_: func(rows []row) []row {
				rows[7].falsif = ""

				return rows
			},
			says: "carries no falsification",
		},
		{
			name: "a row is green because nobody looked",
			break_: func(rows []row) []row {
				rows[9].status = "OK"

				return rows
			},
			says: "a row is PASS once its falsification has been watched to fail",
		},
		{
			name: "the same finding is answered twice",
			break_: func(rows []row) []row {
				return append(rows, rows[0])
			},
			says: "appears twice",
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()

			findings := checkMatrix(mutation.break_(honest()), resolve)
			if len(findings) == 0 {
				t.Fatalf("the checker accepted a matrix where %s, so it would accept it in docs/conformance too", mutation.name)
			}
			if !strings.Contains(strings.Join(findings, "\n"), mutation.says) {
				t.Errorf("the checker refused the matrix but not for the reason it should have.\n got: %v\nwant a finding containing: %q", findings, mutation.says)
			}
		})
	}

	// The control.
	if findings := checkMatrix(honest(), resolve); len(findings) != 0 {
		t.Errorf("the checker refuses an honest matrix, so every mutation above passed for the wrong reason: %v", findings)
	}
}

// TestTheDocumentedTrustBoundaryCannotBeQuietlyDeleted is the same kind of
// control for the prose half. #812 asks for a documented trust boundary,
// and the two limits it records -- what redaction cannot do about a
// hostile script, and what termination certainty does not promise about a
// detached remote descendant -- are the sentences a reader of a green
// matrix most needs. A paragraph nothing checks is a paragraph that gets
// tidied away.
func TestTheDocumentedTrustBoundaryCannotBeQuietlyDeleted(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	text, err := os.ReadFile(filepath.Join(root, matrixPath))
	if err != nil {
		t.Fatalf("reading the conformance matrix: %v", err)
	}

	if findings := checkStatements(string(text)); len(findings) != 0 {
		t.Errorf("%s no longer documents the trust boundary:\n  %s", matrixPath, strings.Join(findings, "\n  "))
	}

	// And the check fires when a statement goes. One removal per
	// required statement, plus the heading, so a rule that only looked
	// for the heading is visible here.
	//
	// The removal happens on the flattened text for checkStatements' own
	// reason: a statement the file hard-wraps is not a substring of the
	// raw bytes, so a mutation against those would delete nothing and
	// this control would be watching an unmutated document.
	flat := strings.Join(strings.Fields(string(text)), " ")
	for _, want := range append([]struct {
		text  string
		about string
	}{{text: "## Trust boundary", about: "the section itself"}}, requiredStatements...) {
		phrase := strings.Join(strings.Fields(want.text), " ")
		without := strings.ReplaceAll(flat, phrase, "")
		if without == flat {
			t.Fatalf("%q is not in the matrix even before the mutation, so this control is checking nothing", phrase)
		}
		if findings := checkStatements(without); len(findings) == 0 {
			t.Errorf("the check accepted a matrix with %q removed, so it is not defending %s", want.text, want.about)
		}
	}
}

// TestTheMatrixParserReadsTheRealTablesRatherThanTheirShape is the other
// half of the self-test, and the one that catches the most likely silent
// failure: a parser that matched nothing would make the gate above pass
// over an empty row set.
//
// It asserts against the real file, which is why it is not a fixture test:
// what could go wrong is the real matrix being written in a table shape
// this parser skips.
func TestTheMatrixParserReadsTheRealTablesRatherThanTheirShape(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	text, err := os.ReadFile(filepath.Join(root, matrixPath))
	if err != nil {
		t.Fatalf("reading the conformance matrix: %v", err)
	}

	rows := parseMatrix(string(text))

	// Every id the file spells has to have been parsed. The ids are read
	// off the raw text with a pattern that does not care about table
	// shape at all, so a row the parser skipped is visible here.
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.id] = true
	}
	for _, m := range regexp.MustCompile(`(?m)^\|\s*((?:CR|GC)-[0-9]{2}[a-z]?)\s*\|`).FindAllStringSubmatch(string(text), -1) {
		if !ids[m[1]] {
			t.Errorf("%s is a row in the file and the parser did not read it, so the gate is checking less than the matrix claims", m[1])
		}
	}

	withProofs := 0
	for _, r := range rows {
		if len(r.proofs) > 0 {
			withProofs++
		}
	}
	if withProofs == 0 {
		t.Fatal("no row parsed to a single proof reference; the proof column is not being read")
	}
}
