package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/health"
)

// EPIC R's metric compat window (#889, FR-37), held from the outside: the
// only evidence that matters is what a scrape reads.
//
// A renamed metric series is this epic's silent class. An alert rule
// whose series stopped existing does not fire, and a dashboard whose
// query matches nothing is blank; both look like a healthy deployment.
// So every gauge is emitted under both names for one release, and these
// tests pin the three properties an operator's alert rule depends on:
// both names are present, both carry the SAME value, and no counter is
// ever duplicated.
//
// The last one is not a style preference. A duplicated counter is a
// correctness bug in the operator's query rather than in this package:
// `sum(rate(...))` across two names holding the same monotonic series
// returns twice the real rate, and nothing in the scrape says so.
// duplicateUnderLegacyPrefix copies a family only when its own TYPE line
// reads `gauge`, so relaxing that condition -- which is the only way a
// counter can reach the compat block -- fails
// TestLegacyCompat_NeverDuplicatesACounter below.

// family is one parsed metric family: its type and its samples, keyed by
// the sample's label text so two names' samples can be compared.
type family struct {
	typ     string
	help    string
	samples map[string]string
}

// parseExposition parses the subset of the text exposition format this
// package emits: a HELP line, a TYPE line, then samples.
func parseExposition(t *testing.T, exposition string) map[string]family {
	t.Helper()

	out := map[string]family{}
	for line := range strings.Lines(exposition) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			f := out[name]
			f.help = help
			out[name] = f
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			f := out[name]
			f.typ = typ
			out[name] = f
		case line == "":
			continue
		default:
			head, value, ok := strings.Cut(line, " ")
			if !ok {
				t.Fatalf("unparseable exposition line %q", line)
			}
			name, labels := head, ""
			if i := strings.IndexByte(head, '{'); i >= 0 {
				name, labels = head[:i], head[i:]
			}
			f, known := out[name]
			if !known {
				// A histogram's samples are named after its family
				// plus a suffix, which the exposition format requires
				// and which is not a family of its own.
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					if base, cut := strings.CutSuffix(name, suffix); cut {
						if f, known = out[base]; known {
							name = base

							break
						}
					}
				}
			}
			if !known {
				t.Fatalf("sample for %s arrived before its HELP/TYPE lines", name)
			}
			if f.samples == nil {
				f.samples = map[string]string{}
			}
			f.samples[labels] = value
			out[name] = f
		}
	}

	return out
}

// populatedReport is a report with a reading in every gauge this package
// renders, so a family cannot pass these tests by being empty.
func populatedReport() health.Report {
	free := uint64(123456)
	age := 90 * time.Second
	polled := time.Unix(1700000100, 0).UTC()
	completed := time.Unix(1700000200, 0).UTC()
	retention := time.Unix(1700000300, 0).UTC()

	return health.NewReport(
		health.NewProcessHealth(health.ProcessInputs{BinaryVersion: "1.2.3", RcloneVersion: "v1.75.0"}),
		[]health.BackupSetHealth{{
			Set:                   mustSet("prod", "one"),
			State:                 health.Degraded,
			StaleThreshold:        6 * time.Hour,
			NewestGoodBackupAge:   &age,
			FreeBytes:             &free,
			LastSuccessfulPollAt:  &polled,
			LastCompletedBackupAt: &completed,
			LastRetentionRunAt:    &retention,
			PendingDeletes:        2,
			Failures:              3,
			QuarantinedCount:      4,
		}},
		time.Unix(1700000000, 0).UTC(),
	)
}

func TestLegacyCompat_EveryGaugeFamilyIsEmittedUnderBothNames(t *testing.T) {
	families := parseExposition(t, Render(populatedReport()))

	var current, legacy int
	for name, f := range families {
		switch {
		case strings.HasPrefix(name, namePrefix):
			current++
			if _, ok := families[legacyNamePrefix+strings.TrimPrefix(name, namePrefix)]; !ok {
				t.Errorf("%s (%s) has no deprecated copy; an alert rule written against the old name stops firing", name, f.typ)
			}
		case strings.HasPrefix(name, legacyNamePrefix):
			legacy++
		default:
			t.Errorf("family %q carries neither prefix", name)
		}
	}
	if current == 0 || current != legacy {
		t.Fatalf("%d current families and %d deprecated ones; the compat window is one copy per family", current, legacy)
	}
}

// The identical-values requirement. Not "both names exist": both names
// have to read the same number, per sample, or an operator comparing a
// dashboard against a new one is debugging this package instead of their
// deployment.
func TestLegacyCompat_BothNamesCarryIdenticalValues(t *testing.T) {
	families := parseExposition(t, Render(populatedReport()))

	for name, f := range families {
		if !strings.HasPrefix(name, namePrefix) {
			continue
		}

		old := families[legacyNamePrefix+strings.TrimPrefix(name, namePrefix)]
		if len(f.samples) != len(old.samples) {
			t.Errorf("%s has %d samples and its deprecated copy has %d", name, len(f.samples), len(old.samples))

			continue
		}
		for labels, value := range f.samples {
			if got := old.samples[labels]; got != value {
				t.Errorf("%s%s = %s but its deprecated copy = %s", name, labels, value, got)
			}
		}
		if old.typ != f.typ {
			t.Errorf("%s is a %s and its deprecated copy is a %s", name, f.typ, old.typ)
		}
	}
}

// The rule that protects the operator's arithmetic: gauges and info
// series only, never a counter.
//
// Asserted at the duplicator rather than only end to end, and that is
// the whole point of this test. Render emits no counter today, so a
// check over Render's output alone would pass on a build whose
// duplicator had been changed to copy counters -- it would go red only
// later, when somebody added the first counter family here, which is
// exactly when nobody is looking at this file. So the seven real
// counter and histogram families from workflow.go are fed THROUGH
// duplicateUnderLegacyPrefix, which is the code path a future counter
// would take.
func TestLegacyCompat_NeverDuplicatesACounter(t *testing.T) {
	var w Workflow
	w.ObserveRun(WorkflowRun{BackupSet: "prod/one", Status: "success", Duration: time.Second})
	w.ObserveStep(WorkflowStep{
		BackupSet: "prod/one", Scope: "backup", Phase: "before",
		Target: "local", State: "failed", Disposition: "exit_nonzero", Duration: time.Second,
	})

	everything := render(populatedReport()) + w.RenderWorkflow()
	sawCounter := false
	for _, f := range parseExposition(t, everything) {
		if f.typ == "counter" || f.typ == "histogram" {
			sawCounter = true
		}
	}
	if !sawCounter {
		t.Fatal("no counter or histogram family rendered at all, so this test proved nothing")
	}

	for name, f := range parseExposition(t, duplicateUnderLegacyPrefix(everything)) {
		if f.typ != "gauge" {
			t.Errorf("the compat block carries %s, a %s: duplicating anything but a gauge double-counts under aggregation", name, f.typ)
		}
		if !strings.HasPrefix(name, legacyNamePrefix) {
			t.Errorf("the compat block carries %s, which is not a deprecated name", name)
		}
	}

	// And the same rule as a scraper sees it: nothing under the
	// deprecated prefix in a real scrape is anything but a gauge.
	for name, f := range parseExposition(t, Render(populatedReport())+w.RenderWorkflow()) {
		if strings.HasPrefix(name, legacyNamePrefix) && f.typ != "gauge" {
			t.Errorf("scrape carries deprecated series %s as a %s", name, f.typ)
		}
	}
}

// A scrape has to carry its own deprecation notice: the operator who
// finds a legacy series in a dashboard next year reads what to replace it
// with out of the same scrape.
func TestLegacyCompat_LegacyHelpNamesItsReplacement(t *testing.T) {
	families := parseExposition(t, Render(populatedReport()))

	for name, f := range families {
		if !strings.HasPrefix(name, legacyNamePrefix) {
			continue
		}

		replacement := namePrefix + strings.TrimPrefix(name, legacyNamePrefix)
		if !strings.Contains(f.help, "DEPRECATED") || !strings.Contains(f.help, replacement) {
			t.Errorf("HELP for %s does not name %s as its replacement: %q", name, replacement, f.help)
		}
		if !strings.Contains(f.help, "double-counts") {
			t.Errorf("HELP for %s does not warn that summing both names double-counts: %q", name, f.help)
		}
	}
}
