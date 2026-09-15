package compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// FR-34's rule that no surface renders a cost figure or an invented ETA,
// asserted against the whole /api/v1 contract by looking for the fields
// rather than for the values.
//
// Absence rather than emptiness, because those are different promises. A
// field a client never learns about cannot be rendered; a field that
// exists and happens to be empty is one release away from somebody filling
// it with a plausible guess, and a plausible guess is exactly what this
// rule exists to prevent. The backend has no price list, no negotiated
// rates, and S3 reports no restore progress at all, so every number in the
// forbidden list would have to be invented to be produced.
//
// It scans the entire contract and not only the schemas EPIC E adds. The
// rule is a product rule, and scoping it to the new work would leave it
// enforced only where nobody was going to break it.
//
// The exception list is checked rather than trusted: an entry the contract
// has outgrown fails this test instead of sitting there granting
// permission nobody needs any more.

// forbiddenFieldWord is FR-34's list, as words rather than substrings.
//
// Words, with boundaries, because this repository has already shipped one
// scanner whose rule never matched between "_" and the letter after it.
// The names here are the ones a well-meaning contributor reaches for when
// they want to be helpful about a Glacier restore, and every one of them
// is a number this backend cannot compute honestly: it has no price list,
// no negotiated rates, and S3 reports no restore progress at all.
var forbiddenFieldWord = regexp.MustCompile(`(?i)(^|[^a-z])(cost|costs|price|pricing|usd|cents|billed|charge|charges|eta|estimate|estimated|estimates|percent|percentage)($|[^a-z])`)

// allowedDespiteTheRule is the declared exception list, and it is checked
// rather than trusted: an entry the contract has outgrown fails this test,
// the same way an outgrown declaration fails the phase 4 conformance
// matrix. Adding a line here is a decision somebody signs, not a way to
// make a red test go away.
//
// "progress" is deliberately NOT in the word list above rather than
// exempted here, because the word itself is not the problem. FR-34 forbids
// a percentage and an invented ETA for a restore, and the live transfer
// counters an operation carries while it runs are neither: the schema's
// own description says in so many words that nothing in it is a percentage
// of the whole operation, because no honest denominator for the whole
// exists. What would be caught, and should be, is anything spelling
// "percent" next to it.
var allowedDespiteTheRule = map[string]string{
	// The four entries below are one operator setting crossing the
	// contract in four places: the resolved value a read reports, the
	// value a create carries, the value a patch changes, and a one-run
	// override on the verify operation. They are declared together
	// because they stand or fall on the same argument, and each one
	// states it, because an exception nobody can read is an exception
	// nobody can withdraw.
	"BackupSet.verification_sample_percent": "The sample size this set's content_sample verifications read, reported back on a read of the set. " +
		"An operator supplies it through `retnd backup-set create --verification-sample-percent` or the equivalent API body, config.Validate refuses anything outside 1 through 100, " +
		"core/service stores it as the set's VerificationSamplePercentConfig in config.yaml, and this field hands that stored value back unchanged. " +
		"FR-34 forbids a number the backend would have to invent -- a price, a billing total, an invented restore ETA, a percentage of a restore's progress -- " +
		"and an operator's own setting read back out is none of those. EPIC K (#788) added it with the incremental engine's operator surface (#855).",

	"BackupSetSpec.verification_sample_percent": "How much of a snapshot's file content a sampled verification reads, on the spec an operator submits to create or replace a backup set. " +
		"The operator chooses the number and validation holds it to 1 through 100; the only thing the backend derives from it is how many FILES kopia's planVerification re-reads, " +
		"through backupengine.VerifyRequest.SamplePercent, which defaults to DefaultVerifySamplePercent when the key is left out. " +
		"That makes it an instruction travelling in rather than a figure travelling out, so FR-34's ban on numbers this backend cannot compute honestly does not reach it. " +
		"EPIC K (#788), the incremental engine's operator surface (#855).",

	"SnapshotVerifyRequest.sample_percent": "The one-run override on POST /operations' verify_snapshot action: how much of this snapshot's file content to read back now, whatever the set is configured for. " +
		"Whoever submits the operation supplies it, through `retnd snapshot verify --sample-percent` or the request body; app.VerifySnapshot falls back to the set's stored setting when it is omitted, " +
		"and the kopia adapter normalises it into 1 through 100. Nothing on that path is money or a prediction: the number selects files the backend then actually re-reads, " +
		"so there is no figure left for it to invent, which is the thing FR-34 is about. EPIC K (#788), the incremental engine's operator surface (#855).",

	"UpdateBackupSetRequest.verification_sample_percent": "The patch half of the same operator setting: it changes an existing set's sampled-verification size, and it is absent when the caller is not changing it, " +
		"which is why the contract marks it a pointer. The value comes from the operator, backupsetupdate writes it to VerificationSamplePercentConfig, and validation keeps it in 1 through 100. " +
		"A setting being written by the person who owns it is the opposite of the cost figure or the invented restore ETA FR-34 forbids, neither of which this backend can compute at all. " +
		"EPIC K (#788), the incremental engine's operator surface (#855).",
}

// TestTheContractServesNoCostFigureAndNoInventedETA is EPIC E's Phase 2
// exit-gate line "no surface anywhere renders a cost figure or an invented
// ETA (asserted by the contract tests on the response schemas: the fields
// do not exist)".
//
// The gate line says the fields do not exist, so this asserts their
// absence rather than their emptiness. An absent field cannot be rendered
// by a client that never learns about it; a field that exists and is
// usually empty is one release away from being filled in with a plausible
// guess, which is the exact failure FR-34 was written to prevent.
//
// This is a check on the whole /api/v1 contract, not only on the parts
// EPIC E adds. That is deliberate: the rule is a product rule ("nothing
// renders a number the backend cannot compute", issue #211), and scoping
// it to EPIC E's own schemas would leave it enforceable only where nobody
// was going to break it.
func TestTheContractServesNoCostFigureAndNoInventedETA(t *testing.T) {
	contractPath := filepath.Join("..", "..", "..", "api", "v1", "openapi.json")
	blob, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("reading %s: %v", contractPath, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("parsing %s: %v", contractPath, err)
	}

	schemas := mapAt(doc, "components", "schemas")
	if len(schemas) == 0 {
		t.Fatal("the contract declares no schemas, so this check would inspect nothing and pass")
	}

	inspected := 0
	var offenders []string
	for _, name := range sortedAnyKeys(schemas) {
		props := mapOf(mapOf(schemas[name])["properties"])
		for _, prop := range sortedAnyKeys(props) {
			inspected++
			if forbiddenFieldWord.MatchString(prop) {
				offenders = append(offenders, name+"."+prop)
			}
		}
	}
	if inspected == 0 {
		t.Fatal("no schema in the contract has a single property, so this check inspected nothing")
	}

	// The positive control lives here rather than in a separate test,
	// because a scanner that never fires is exactly what this repository
	// keeps finding, and the cheapest way to know this one fires is to
	// hand it the string it is looking for.
	control := []string{"estimated_cost_usd", "restore_eta", "percent_complete", "monthly_price", "restore_progress_percent", "egress_charge_cents"}
	for _, c := range control {
		if !forbiddenFieldWord.MatchString(c) {
			t.Fatalf("the field-name rule does not match %q, so it would not catch the thing it exists to catch", c)
		}
	}
	for _, ok := range []string{"storage_class", "size_bytes", "access", "restore_expires_at", "etag", "progress", "bytes_transferred"} {
		if forbiddenFieldWord.MatchString(ok) {
			t.Fatalf("the field-name rule matches %q, which is a field FR-34 explicitly does serve, so it is too broad to live with", ok)
		}
	}

	var stale []string
	for field := range allowedDespiteTheRule {
		found := false
		for _, o := range offenders {
			if o == field {
				found = true
				break
			}
		}
		if !found {
			stale = append(stale, field)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("these fields are declared as exceptions to FR-34's no-cost rule and the contract no longer has them, so the declaration is stale and the next real offender could hide behind it:\n  %s",
			strings.Join(stale, "\n  "))
	}
	kept := offenders[:0]
	for _, o := range offenders {
		if _, ok := allowedDespiteTheRule[o]; !ok {
			kept = append(kept, o)
		}
	}
	offenders = kept

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("EPIC E's Phase 2 exit gate says no surface renders a cost figure or an invented ETA, and the /api/v1 contract now declares %d field(s) that do:\n  %s\n\nFR-34's rule is that the backend serves what it holds: bytes, storage class, and a plain statement that retrieval from this class is billed by the provider. If a future FR adds operator-entered price tables, that FR gets to change this test.",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	t.Logf("inspected %d properties across %d schemas", inspected, len(schemas))
}
