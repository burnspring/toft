package telemetry

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the metric inventory golden file")

// TestMetricInventoryMatchesSnapshot *is* the contract.
//
// Goal 4 of the PRD says metric names, units and attributes are a published
// contract that does not silently move. Without this test that sentence is a
// paragraph in a document; with it, any addition, rename or unit change fails
// the build until somebody rewrites the golden file on purpose — which is
// exactly the moment to notice that an operator's dashboard is about to break
// and to write the migration note.
//
//	go test ./internal/telemetry/ -run Snapshot -update
func TestMetricInventoryMatchesSnapshot(t *testing.T) {
	var b strings.Builder
	for _, s := range SortedInventory() {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", s.Name, s.Kind, s.Unit)
		for _, a := range s.Attrs {
			fmt.Fprintf(&b, "\tattr %s bound=%s\n", a.Key, boundString(a.Bound))
		}
		if len(s.Buckets) > 0 {
			fmt.Fprintf(&b, "\tbuckets %v\n", s.Buckets)
		}
		fmt.Fprintf(&b, "\tseries %d\n", s.Series)
	}
	got := b.String()

	path := filepath.Join("testdata", "metrics.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden file: %v (create it with -update)", err)
	}
	if got != string(want) {
		t.Errorf("the metric inventory has changed.\n\n"+
			"This is a published contract: an operator's dashboards and alert rules are "+
			"written against these names, units and attribute keys. If the change is "+
			"deliberate, document the migration in docs/dev-reference.md § Telemetry and "+
			"re-run with -update.\n\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func boundString(b int) string {
	if b == BoundApps {
		return "apps-cap"
	}
	return fmt.Sprint(b)
}

// TestEveryMetricAttributeHasABound is the mechanism that actually prevents an
// unbounded label.
//
// Nobody has ever caught one by reading a diff. What catches one is that the
// table will not compile a meaning for "I did not think about it": a Bound of
// zero is not a small number, it is an undeclared one, and it fails here.
func TestEveryMetricAttributeHasABound(t *testing.T) {
	for _, s := range Inventory {
		for _, a := range s.Attrs {
			if a.Bound == 0 {
				t.Errorf("%s: attribute %q declares no bound — every metric attribute needs a "+
					"finite declared domain, or it is an unbounded label", s.Name, a.Key)
			}
			if a.Bound < 0 && a.Bound != BoundApps {
				t.Errorf("%s: attribute %q has a negative bound that is not BoundApps", s.Name, a.Key)
			}
		}
	}
}

// TestMetricCardinalityBudget holds the ceiling.
//
// 20,000 active attribute sets on an install with 500 apps. An operator whose
// install exceeds it should see Toft fold a dimension rather than see
// their Prometheus fall over, which is what the __other__ bucket is for.
//
// The sum is over each instrument's *modeled* series, not over the product of
// its bounds: no real traffic sends every method to every route and gets every
// status back, so the product is a ceiling rather than a figure. The product is
// still asserted as a ceiling on the model, so a model can only ever be wrong
// in the direction of claiming too much.
func TestMetricCardinalityBudget(t *testing.T) {
	const ceiling = 20000
	total := GoRuntimeSeries
	for _, s := range Inventory {
		if s.Series <= 0 {
			t.Errorf("%s declares no modeled series count", s.Name)
			continue
		}
		product := 1
		for _, a := range s.Attrs {
			bound := a.Bound
			if bound == BoundApps {
				bound = 500
			}
			product *= bound
		}
		if s.Series > product {
			t.Errorf("%s models %d series but its declared bounds allow only %d — "+
				"the model must not exceed the worst case", s.Name, s.Series, product)
		}
		total += s.Series
	}
	if total >= ceiling {
		t.Errorf("the inventory models %d active series at 500 apps, over the %d budget. "+
			"Narrow an instrument's attributes or fold a dimension; do not raise the ceiling "+
			"without a paragraph in ADR §31 D5 saying why.", total, ceiling)
	}
	t.Logf("modeled active series at 500 apps: %d of %d", total, ceiling)
}

// TestAppAttributeCapFolds is the fold, and the fold being visible.
//
// A ceiling that is hit silently is indistinguishable from a ceiling that is
// not hit, and the first question an operator asks about a suspiciously round
// dashboard is whether they are seeing all of it.
func TestAppAttributeCapFolds(t *testing.T) {
	const cap = 500
	c := newAppCap(cap)

	seen := map[string]bool{}
	for i := 0; i < 600; i++ {
		seen[c.value(fmt.Sprintf("app-%03d", i))] = true
	}
	if len(seen) != cap+1 {
		t.Fatalf("recorded %d distinct attribute values against %d apps with a cap of %d, want %d",
			len(seen), 600, cap, cap+1)
	}
	if !seen[OverflowApp] {
		t.Errorf("the overflow bucket %q was never used", OverflowApp)
	}
	// An app that already has a slot keeps it, however many arrive after.
	if got := c.value("app-000"); got != "app-000" {
		t.Errorf("an app already in the registry folded to %q", got)
	}
	// An empty id is not an app; it is the platform surface, and it must not
	// consume a slot.
	if got := c.value(""); got != "" {
		t.Errorf("the empty app id folded to %q", got)
	}
}

// TestAppCapIsConcurrencySafe: two gateway requests for two unseen apps can
// both pass the read-lock check, and without the re-check under the write lock
// the cap would be exceeded by however many were in flight.
func TestAppCapIsConcurrencySafe(t *testing.T) {
	const cap = 50
	c := newAppCap(cap)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.value(fmt.Sprintf("app-%03d", i))
		}(i)
	}
	wg.Wait()

	c.mu.RLock()
	n := len(c.seen)
	c.mu.RUnlock()
	if n > cap {
		t.Errorf("the registry holds %d apps with a cap of %d", n, cap)
	}
}

// TestSweepEnumMatchesTheConstants is the guard the drift got past.
//
// `sweep` declared a bound of six and a note naming five, while a sixth —
// `dormancy` — was already being emitted, and two of the declared constants had
// never been passed to anything. The golden file froze the instrument and the
// prose bound to it went stale anyway, because nothing compared the note to the
// constants. This is that comparison.
//
// It reads every instrument carrying the attribute rather than one named
// instrument, so the second reader of the enum is covered the day it is added
// and not the day somebody remembers this test exists.
func TestSweepEnumMatchesTheConstants(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range SweepNames {
		if seen[n] {
			t.Errorf("SweepNames lists %q twice; the bound counts it once", n)
		}
		seen[n] = true
	}
	for _, n := range []string{
		SweepReconcile, SweepHealth, SweepIdle, SweepConsumption, SweepPrune,
		SweepDormancy, SweepIntegrations,
		SweepJobs, SweepReleases,
	} {
		if !seen[n] {
			t.Errorf("%q is a declared sweep name but is not in SweepNames, so no bound counts it", n)
		}
	}

	assertEnumAttr(t, "sweep", SweepNames)
}

// TestSweepSubjectEnumMatchesTheConstants holds the other half of
// toft.sweep.removed, for the reason above.
func TestSweepSubjectEnumMatchesTheConstants(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range SubjectNames {
		seen[n] = true
	}
	for _, n := range []string{SubjectRows, SubjectLogLines, SubjectObjects, SubjectReleases} {
		if !seen[n] {
			t.Errorf("%q is a declared subject but is not in SubjectNames", n)
		}
	}
	assertEnumAttr(t, "subject", SubjectNames)
}

// assertEnumAttr checks every instrument carrying `key` against the constants.
func assertEnumAttr(t *testing.T, key string, values []string) {
	t.Helper()
	note := strings.Join(values, " | ")
	found := 0
	for _, s := range Inventory {
		for _, a := range s.Attrs {
			if a.Key != key {
				continue
			}
			found++
			if a.Bound != len(values) {
				t.Errorf("%s: `%s` declares a bound of %d against %d names — "+
					"the cardinality budget is counting the wrong set",
					s.Name, key, a.Bound, len(values))
			}
			if a.Note != note {
				t.Errorf("%s: `%s` is noted as %q but the constants are %q — "+
					"an operator reads the note, so it is the half that has to be true",
					s.Name, key, a.Note, note)
			}
		}
	}
	if found == 0 {
		t.Errorf("no instrument carries a `%s` attribute; has one been renamed?", key)
	}
}

// TestErrorTypeIsAClosedEnum: every classification ErrorType can return is in
// the declared list, because that list is what the cardinality budget counts.
func TestErrorTypeIsAClosedEnum(t *testing.T) {
	in := map[string]bool{}
	for _, v := range errorTypeValues {
		in[v] = true
	}
	if len(errorTypeValues) > 15 {
		t.Errorf("error.type declares %d values; the budget assumes 15", len(errorTypeValues))
	}
	for _, v := range []string{ErrTimeout, ErrCanceled, ErrNetwork, ErrOther, ErrHTTPStatus} {
		if !in[v] {
			t.Errorf("%q is returned by ErrorType but is not in errorTypeValues", v)
		}
	}
}

// TestStatusLabelIsAClosedSet is TL-6. On the apps surface the status code is
// the app's — ReverseProxy relays anything from 100 to 999 — so the label has
// to be folded into the set the inventory declares, not trusted.
func TestStatusLabelIsAClosedSet(t *testing.T) {
	var bound int
	for _, s := range Inventory {
		if s.Name != MetricHTTPServerDuration {
			continue
		}
		for _, a := range s.Attrs {
			if a.Key == "http.response.status_code" {
				bound = a.Bound
			}
		}
	}
	if bound == 0 {
		t.Fatal("http.server.request.duration declares no status_code bound")
	}
	seen := map[int]bool{}
	for code := 100; code <= 999; code++ {
		got := metricStatus(code)
		seen[got] = true
		if code <= 599 && got/100 != code/100 {
			t.Errorf("metricStatus(%d) = %d, outside its class", code, got)
		}
		if code >= 600 && got != 500 {
			t.Errorf("metricStatus(%d) = %d, want 500 for a code in no class", code, got)
		}
	}
	if len(seen) > bound {
		t.Errorf("metricStatus returns %d distinct values; the inventory declares %d", len(seen), bound)
	}
}

// TL-8(a): the SDK's per-instrument limit defaults to 2000 attribute sets,
// below what the inventory budgets for http.server.request.duration. Past it
// new sets fold into otel.metric.overflow, which nothing here counts.
func TestSDKLimitCoversTheLargestInstrument(t *testing.T) {
	reader, _, _ := startInMemory(t)
	ctx := context.Background()
	for i := 0; i < 2001; i++ {
		RecordHTTPServer(ctx, SurfacePlatform, "GET", fmt.Sprintf("/r/%d", i), "http", 200, "", time.Millisecond, 0)
	}
	points := histogramPoints(t, reader, MetricHTTPServerDuration)
	if len(points) < 2001 {
		t.Errorf("got %d points, want 2001", len(points))
	}
	for _, p := range points {
		if _, ok := p.Attributes.Value("otel.metric.overflow"); ok {
			t.Fatal("a point carries otel.metric.overflow: the SDK limit is below the budget")
		}
	}
}
