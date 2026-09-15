package metrics_test

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/metrics"
)

// What these tests are for, and why they are shaped like this.
//
// The seven workflow families (#813) are scraped by something that is not
// in this repository, so the two ways they can break are both invisible
// here: a rename, which silently empties a dashboard, and a new label,
// which silently multiplies the number of time series a deployment pays
// for forever. Neither one fails to compile and neither one looks wrong
// in a diff.
//
// So the names and TYPE lines are pinned as literals (the same thing
// metrics_test.go does for the health families, for the same reason), and
// the cardinality is asserted as an invariant rather than eyeballed:
// series count must not be a function of how many hooks ran. The
// histogram gets its own test because its correctness is arithmetic --
// cumulative buckets, an +Inf that agrees with _count, a _sum that
// agrees with what was observed -- and a histogram that is wrong about
// any of those is one a scraper reads without complaining.

// samplePattern matches one rendered sample line: a metric name, an
// optional label set, and a value. The leading anchor is what keeps it
// off the "# HELP" and "# TYPE" lines.
var samplePattern = regexp.MustCompile(`(?m)^([a-zA-Z_][a-zA-Z0-9_]*)(\{[^}]*\})? (\S+)$`)

// typePattern matches one TYPE line's family name and type word.
var typePattern = regexp.MustCompile(`(?m)^# TYPE (\S+) (\S+)$`)

// sampleLine is one parsed sample. The label set is kept as the rendered
// text rather than a map, because a series IS its rendered label list:
// two samples with the same labels in a different order are two series to
// this exporter and one to nobody.
type sampleLine struct {
	name   string
	labels string
	value  float64
}

func parseSamples(t *testing.T, rendered string) []sampleLine {
	t.Helper()

	matches := samplePattern.FindAllStringSubmatch(rendered, -1)
	out := make([]sampleLine, 0, len(matches))
	for _, m := range matches {
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("the rendered sample %q%q has value %q, which is not a number a scraper can read: %v", m[1], m[2], m[3], err)
		}
		out = append(out, sampleLine{name: m[1], labels: m[2], value: v})
	}

	return out
}

// TestRenderWorkflowFamilyNamesAndTypes pins the seven families #813
// names, exactly, including their types.
//
// Both directions are asserted: every one of the seven is present, and
// nothing else is. The second half is the one that catches an eighth
// family being added without anybody deciding it should exist, which on
// a scrape endpoint is a decision about somebody's storage bill.
func TestRenderWorkflowFamilyNamesAndTypes(t *testing.T) {
	t.Parallel()

	// A counter set with nothing observed, because the HELP and TYPE
	// lines must be there before anything has happened: a family that
	// appeared only once a hook had failed would break every dashboard
	// built on it on exactly the deployments where it is working.
	var empty metrics.Workflow

	want := map[string]string{
		"retnd_workflow_runs_total":                 "counter",
		"retnd_workflow_run_duration_seconds":       "histogram",
		"retnd_workflow_step_duration_seconds":      "histogram",
		"retnd_workflow_step_failures_total":        "counter",
		"retnd_workflow_step_timeouts_total":        "counter",
		"retnd_workflow_remote_exec_failures_total": "counter",
		"retnd_workflow_log_truncations_total":      "counter",
	}

	for _, rendered := range []string{empty.RenderWorkflow(), labelledWorkflow(t).RenderWorkflow()} {
		got := map[string]string{}
		for _, m := range typePattern.FindAllStringSubmatch(rendered, -1) {
			got[m[1]] = m[2]
		}

		for name, typ := range want {
			switch {
			case got[name] == "":
				t.Errorf("no TYPE line for %s.\n"+
					"A metric name is a contract for every dashboard, alert rule and recording rule scraping it, none of which live in this repository to fail.", name)
			case got[name] != typ:
				t.Errorf("%s is declared %q, want %q", name, got[name], typ)
			}

			if !strings.Contains(rendered, "# HELP "+name+" ") {
				t.Errorf("no HELP line for %s; an operator reading a scrape has nothing else to tell them what it measures", name)
			}
		}

		for name := range got {
			if want[name] == "" {
				t.Errorf("the workflow rendering publishes %s, which is not one of the seven families #813 defines.\n"+
					"A new family is a new thing every deployment stores forever, so it belongs in that list, in this test, and in the issue -- or nowhere.", name)
			}
		}
	}
}

// TestWorkflowStepSeriesDoNotGrowWithTheNumberOfSteps is the cardinality
// invariant, checked the way it actually fails.
//
// The failure it is built against: somebody adds a `step` or a `script`
// label, because a dashboard would have been nicer with one, and nothing
// breaks. The exporter's series count then grows with every hook a
// deployment runs and never shrinks, because this process does not forget
// a series it has emitted -- a nightly backup with eight hooks and
// per-run step ids reaches five figures within a year, and the first
// anybody hears of it is a scrape timeout.
//
// So this observes ten thousand steps whose per-step identity -- run id,
// script name, step id -- is different every single time, and asserts
// that the rendering is the same size as it was after ONE. The per-step
// strings are built in the loop rather than omitted, and are asserted
// absent from the output afterwards, because the two halves of the
// requirement are "they cannot become labels" and "they do not reach the
// text at all".
func TestWorkflowStepSeriesDoNotGrowWithTheNumberOfSteps(t *testing.T) {
	t.Parallel()

	const observations = 10000

	// The per-step facts, in the shapes the engine really mints them
	// in: internal/workflow's StepID is order~scope~phase~script, a run
	// id is timestamped, and a script name is whatever the operator
	// called their file.
	identity := func(i int) (runID, scriptName, stepID string) {
		runID = fmt.Sprintf("run-2026-09-13T02:00:%02dZ-%04x", i%60, i)
		scriptName = fmt.Sprintf("%02d-dump-pgsql-%d.remote.sh", i%100, i)
		stepID = fmt.Sprintf("%04d~set~before~%s", i, scriptName)

		return runID, scriptName, stepID
	}

	// One step, and a step per identity, observed into two counter sets
	// that are otherwise driven identically. Everything a label can
	// carry is held constant; everything that varies is a fact the
	// observation type has nowhere to put.
	step := metrics.WorkflowStep{
		BackupSet:   "production/uploads-tree",
		Scope:       "set",
		Phase:       "before",
		Target:      "remote",
		State:       "failed",
		Disposition: "transport_lost",
		Duration:    2 * time.Second,
	}

	var one, many metrics.Workflow
	one.ObserveStep(step)

	for i := range observations {
		runID, scriptName, stepID := identity(i)
		if runID == "" || scriptName == "" || stepID == "" {
			t.Fatal("the fixture minted an empty per-step identity, so this test would prove nothing")
		}

		many.ObserveStep(step)
	}

	baseline := parseSamples(t, one.RenderWorkflow())
	loaded := parseSamples(t, many.RenderWorkflow())

	// The arithmetic, spelled out so that a failure says which label
	// arrived: one step_duration histogram is eight bucket lines plus
	// +Inf plus _sum plus _count, and this step also increments
	// step_failures_total and remote_exec_failures_total. Nothing else
	// about it is a series.
	const wantSeries = 13

	if len(baseline) != wantSeries {
		t.Errorf("one observed step renders %d sample lines, want %d.\n"+
			"Either a label was added to a workflow family, or the duration ladder changed length. Both are cardinality decisions and both belong in a comment before they belong in a release.", len(baseline), wantSeries)
	}

	if len(loaded) != len(baseline) {
		t.Fatalf("%d steps differing only in run id, script name and step id render %d sample lines, while one step renders %d.\n"+
			"A workflow label is now carrying a per-step value. That is unbounded cardinality in a process that never forgets a series: the exporter grows until somebody restarts it, and the time-series database it feeds does not even get that.\n"+
			"The per-step facts belong in the step's own log and on the run row, where they already are.", observations, len(loaded), len(baseline))
	}

	// The same series, and only the values moved. A rename that
	// happened to preserve the count would otherwise pass.
	for i := range loaded {
		if loaded[i].name != baseline[i].name || loaded[i].labels != baseline[i].labels {
			t.Errorf("series %d is %s%s after %d steps and %s%s after one; the label sets must not depend on how much ran",
				i, loaded[i].name, loaded[i].labels, observations, baseline[i].name, baseline[i].labels)
		}
	}

	rendered := many.RenderWorkflow()
	for _, i := range []int{0, observations / 2, observations - 1} {
		runID, scriptName, stepID := identity(i)
		for _, fact := range []string{runID, scriptName, stepID} {
			if strings.Contains(rendered, fact) {
				t.Errorf("the rendered scrape contains %q, which is a per-step value. It is in a hook's log and on the run row; a scrape keeps it for months.", fact)
			}
		}
	}
}

// TestWorkflowObservationsCarryNoScriptNameStepIDOrRunID is the
// structural half of the guard above.
//
// The cardinality test proves that today's rendering does not publish a
// per-step value. This one proves the stronger property the observation
// types were designed for: there is nowhere to PUT one. A field named
// ScriptName on WorkflowStep would be wired up by the first person who
// wanted it on a dashboard, and the review that should have caught it is
// this test instead.
//
// The allowed field names are pinned rather than derived, so adding a
// field to either type is a decision somebody has to make here, with the
// rest of this file's reasoning in front of them.
func TestWorkflowObservationsCarryNoScriptNameStepIDOrRunID(t *testing.T) {
	t.Parallel()

	allowed := map[reflect.Type]map[string]bool{
		reflect.TypeFor[metrics.WorkflowRun](): {
			"BackupSet": true,
			"Status":    true,
			"Bypassed":  true,
			"Duration":  true,
		},
		reflect.TypeFor[metrics.WorkflowStep](): {
			"BackupSet":   true,
			"Scope":       true,
			"Phase":       true,
			"Target":      true,
			"State":       true,
			"Disposition": true,
			"Duration":    true,
		},
	}

	for typ, fields := range allowed {
		for i := range typ.NumField() {
			name := typ.Field(i).Name
			if !fields[name] {
				t.Errorf("%s has a field %q, which is not one of the facts an observation may carry.\n"+
					"Every label these families publish comes from a closed vocabulary or from a backup set id (see workflow.go's rule one). A field holding a script name, a step id, a run id, a path or a hook's output is one somebody will eventually render, and then it is in a time-series database forever.", typ.Name(), name)
			}
		}

		for name := range fields {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("%s no longer has the field %q this package's label contract is written against", typ.Name(), name)
			}
		}
	}
}

// TestWorkflowHistogramsAreArithmeticallySound checks the three things a
// scraper assumes about a histogram and will never tell you it assumed:
// the buckets are cumulative, the +Inf bucket agrees with _count, and
// _sum is the total that was actually observed.
//
// It matters here specifically because these histograms are hand-rolled
// (workflow.go accumulates at render time rather than on every
// observation, and says why), and because an observation above the top of
// the ladder takes the one code path that records no bucket at all.
func TestWorkflowHistogramsAreArithmeticallySound(t *testing.T) {
	t.Parallel()

	// Exactly-representable seconds, so the expected sum is an exact
	// float64 and the assertion below needs no tolerance. The last one
	// is above the top of the ladder (900s) on purpose: that is the
	// observation that lands in no bucket and must still reach _count
	// and _sum.
	seconds := []float64{0.5, 1, 2, 4, 8, 64, 512, 1024}

	var w metrics.Workflow
	wantSum := 0.0
	for _, s := range seconds {
		wantSum += s
		w.ObserveRun(metrics.WorkflowRun{
			BackupSet: "production/uploads-tree",
			Status:    "success",
			Duration:  time.Duration(s * float64(time.Second)),
		})
		w.ObserveStep(metrics.WorkflowStep{
			BackupSet:   "production/uploads-tree",
			Scope:       "set",
			Phase:       "before",
			Target:      "local",
			State:       "success",
			Disposition: "exited",
			Duration:    time.Duration(s * float64(time.Second)),
		})
	}

	samples := parseSamples(t, w.RenderWorkflow())

	for _, family := range []string{
		"retnd_workflow_run_duration_seconds",
		"retnd_workflow_step_duration_seconds",
	} {
		t.Run(family, func(t *testing.T) {
			var (
				bounds   []float64
				buckets  []float64
				infSeen  bool
				inf      float64
				sum      float64
				count    float64
				sawSum   bool
				sawCount bool
			)

			for _, s := range samples {
				switch s.name {
				case family + "_bucket":
					le := bucketBound(t, s.labels)
					if le == "+Inf" {
						infSeen, inf = true, s.value

						continue
					}
					v, err := strconv.ParseFloat(le, 64)
					if err != nil {
						t.Fatalf("bucket bound %q is not a number", le)
					}
					bounds = append(bounds, v)
					buckets = append(buckets, s.value)
				case family + "_sum":
					sawSum, sum = true, s.value
				case family + "_count":
					sawCount, count = true, s.value
				}
			}

			if !sawSum || !sawCount || !infSeen {
				t.Fatalf("%s rendered sum=%v count=%v +Inf=%v; a histogram missing any of the three is one no scraper can use", family, sawSum, sawCount, infSeen)
			}

			if !sort.Float64sAreSorted(bounds) {
				t.Errorf("%s renders its bucket bounds out of order: %v", family, bounds)
			}

			for i := 1; i < len(buckets); i++ {
				if buckets[i] < buckets[i-1] {
					t.Errorf("%s bucket le=%v holds %v while le=%v holds %v; cumulative buckets can only grow, and a scraper computing a quantile from these gets an answer rather than an error",
						family, bounds[i], buckets[i], bounds[i-1], buckets[i-1])
				}
			}

			if inf != count {
				t.Errorf("%s has +Inf=%v and _count=%v. The +Inf bucket IS the count by definition, and an observation that fell past the top of the ladder is the way they come apart.", family, inf, count)
			}

			if count != float64(len(seconds)) {
				t.Errorf("%s counted %v observations, want %d", family, count, len(seconds))
			}

			if sum != wantSum {
				t.Errorf("%s reports _sum=%v, want %v. _sum is what an operator divides by _count to get a mean, so a sum that drops an observation reports every hook as faster than it was.", family, sum, wantSum)
			}

			// The boundary the last observation is for: 1024s is above
			// the top bound, so every bucket line must exclude it and
			// only +Inf may see it.
			if last := buckets[len(buckets)-1]; last != float64(len(seconds))-1 {
				t.Errorf("%s's top bucket le=%v holds %v, want %d: an observation above the highest bound belongs in no bucket at all",
					family, bounds[len(bounds)-1], last, len(seconds)-1)
			}
		})
	}
}

// bucketBound pulls the le value out of a rendered label set. The le
// label is always rendered last (writeLabels appends it), which is the
// only assumption here.
func bucketBound(t *testing.T, labels string) string {
	t.Helper()

	const marker = `le="`
	i := strings.Index(labels, marker)
	if i < 0 {
		t.Fatalf("a bucket line rendered labels %q with no le bound, so it is not a bucket a scraper can place", labels)
	}
	rest := labels[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("a bucket line rendered an unterminated le bound in %q", labels)
	}

	return rest[:j]
}
