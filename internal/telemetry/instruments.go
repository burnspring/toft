package telemetry

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The metric inventory.
//
// This table is the *contract*. An operator builds dashboards and alerts
// against these names, units and attribute keys, and they do not silently move:
// TestMetricInventoryMatchesSnapshot freezes the whole thing against a golden
// file, so an addition, a rename or a unit change fails the build until
// somebody updates the file on purpose. Without that test the contract is a
// paragraph in a document, which is to say nothing at all.
//
// It is also where the cardinality budget lives. Every attribute declares a
// Bound, and an attribute with no declared bound will not compile into the
// table — which is the actual mechanism preventing an unbounded label, since
// nobody has ever caught one by reading a diff.
//
// Names follow OpenTelemetry semantic conventions where a convention exists and
// the `toft.` namespace where none does. OTel names carry no unit suffix;
// the unit is a field. The Prometheus exporter appends `_seconds`, `_bytes` and
// `_total` itself, so `http.server.request.duration` is scraped as
// `http_server_request_duration_seconds`.
const (
	MetricHTTPServerDuration = "http.server.request.duration"
	MetricHTTPServerActive   = "http.server.active_requests"
	MetricHTTPServerBodySize = "http.server.request.body.size"
	MetricHTTPClientDuration = "http.client.request.duration"

	MetricAppUpstreamDuration = "toft.app.upstream.duration"
	MetricAppWakeDuration     = "toft.app.wake.duration"
	MetricAppLogDropped       = "toft.app.log.dropped"
	MetricAppTransitions      = "toft.app.state.transitions"
	MetricAppsByStatus        = "toft.apps.by_status"

	MetricBuildDuration   = "toft.build.duration"
	MetricBuildQueueDepth = "toft.build.queue.depth"
	MetricBuildQueueWait  = "toft.build.queue.wait"
	MetricBuildActive     = "toft.build.active"
	// MetricBuildCacheBytes is the size of the shared npm/uv/Go cache. It is
	// the one number nothing on any surface used to know, and the reason it is
	// worth exporting is that the failure it predicts is not a slow build but a
	// full volume: CACHE_DIR shares a filesystem with release checkouts and
	// every app's logs, so an unbounded cache takes down every deploy at once
	// while everything already serving keeps serving.
	//
	// An alert on this is a rate of change, not a threshold — nobody knows what
	// a large cache is yet, which is exactly why this is being measured before
	// anything is built to bound it.
	MetricBuildCacheBytes = "toft.build.cache.bytes"
	MetricDeployDuration  = "toft.deploy.duration"

	MetricJobRunDuration       = "toft.job.run.duration"
	MetricJobQueueDepth        = "toft.job.queue.depth"
	MetricJobRunMemoryPeak     = "toft.job.run.memory.peak"
	MetricScheduledRunDuration = "toft.scheduled.run.duration"

	MetricNotifySendDuration = "toft.notify.send.duration"
	// MetricIntegrationSends counts what apps send through the catalog. See
	// IntegrationKindBound for why `kind` is a safe label and a connection id
	// never is.
	MetricIntegrationSends = "toft.integration.sends"
	MetricAuthSignin       = "toft.auth.signin"
	MetricArtifactDuration = "toft.artifact.operation.duration"

	MetricDBPoolConnections  = "toft.db.pool.connections"
	MetricDBPoolWaits        = "toft.db.pool.waits"
	MetricDBPoolWaitDuration = "toft.db.pool.wait.duration"

	MetricSweepDuration          = "toft.sweep.duration"
	MetricSweepRemoved           = "toft.sweep.removed"
	MetricAutodeployPollDuration = "toft.autodeploy.poll.duration"
	MetricAutodeploySkipped      = "toft.autodeploy.skipped"
	// MetricGitHookDeliveries counts webhook deliveries by what became of them.
	// **A counter and not a row per delivery**: an unauthenticated endpoint
	// that writes to a table is a way to fill one from the internet, and the
	// only question anybody has about these is "are any arriving, and are they
	// verifying".
	MetricGitHookDeliveries   = "toft.git.hook.deliveries"
	MetricGallerySyncDuration = "toft.gallery.sync.duration"

	// Dependency policy (ADR §39). Four series, and what is *not* here is the
	// point: a package name and an advisory id are unbounded by construction —
	// the npm corpus is millions of names — and one careless label would spend
	// the install's whole 20,000-set budget on its own. They live on rows and
	// events, which is where unbounded strings belong.
	MetricDependencyScans    = "toft.dependency.scans"
	MetricDependencyRefusals = "toft.dependency.refusals"
	MetricDependencyWaivers  = "toft.dependency.waivers"
	MetricDependencyFeedAge  = "toft.dependency.feed.age"

	MetricTelemetryDropped        = "toft.telemetry.dropped"
	MetricTelemetryExportFailures = "toft.telemetry.export.failures"
	MetricBuildInfo               = "toft.build.info"
)

// Instrument kinds, as the snapshot spells them.
const (
	KindHistogram       = "histogram"
	KindCounter         = "counter"
	KindUpDownCounter   = "updowncounter"
	KindObservableGauge = "gauge"
)

// BoundApps is the sentinel for "however many apps the cap allows", so the
// budget arithmetic follows TOFT_METRIC_APP_CARDINALITY_CAP rather than
// restating 500 in fifteen places.
const BoundApps = -1

// AttrSpec declares one attribute key and how many distinct values it may take.
type AttrSpec struct {
	Key   string
	Bound int
	Note  string
}

// Spec declares one instrument.
//
// Series is the *modeled* number of active attribute sets on an install at the
// app cap — not the product of the bounds, which is a worst case that no real
// traffic reaches (nothing sends every method to every route and gets every
// status back). The budget is asserted against the sum of Series; the product
// is asserted as a ceiling *on* Series, so a model that claims less than its
// own bounds allow has to be wrong in the safe direction.
type Spec struct {
	Name    string
	Kind    string
	Unit    string
	Buckets []float64
	Attrs   []AttrSpec
	Series  int
	Why     string
}

// Duration buckets.
//
// The request set is the OpenTelemetry default, taken deliberately rather than
// by omission: an operator's existing dashboards and their existing p99 rules
// line up with it, and a bespoke set would make Toft the one service in
// the rack whose histogram quantiles are computed differently.
//
// Builds, jobs and scheduled runs get their own, because a bucket set that tops
// out at ten seconds reports every build as "> 10s" and answers nothing.
var (
	requestBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	longBuckets    = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}
	byteBuckets    = []float64{1024, 8192, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456}
)

// Closed enums, and their sizes. Written as variables so the budget table and
// the runtime both read one definition.
var (
	// The detector's closed list (internal/builder/detector.go) plus "unknown"
	// for an app deployed before detection recorded one.
	frameworkBound = 9
	// database.AppStatus*, and nothing else can appear on an app row.
	appStatusBound = 8
	// A run's three provenances: a person, a schedule, the app itself.
	triggerBound = 3
)

// Inventory is the full declared set. Order is irrelevant to the runtime and
// fixed for the snapshot, which sorts by name.
var Inventory = []Spec{
	{
		Name: MetricHTTPServerDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"http.request.method", 8, "semconv: anything not a known method is _OTHER"},
			{"http.route", 200, "chi route patterns, never r.URL.Path; unmatched folds to __unmatched__"},
			{"http.response.status_code", 25, "metricStatus: a closed set of 23 codes, anything else folds to its class's x00; on the app surface the code is the app's, so it is folded rather than trusted"},
			{"url.scheme", 2, "http | https"},
			{"toft.surface", 2, "platform | app"},
			{"toft.app.id", BoundApps, "capped registry with an __other__ fold; empty on the platform surface"},
			{"error.type", 15, "the closed enum in errortype.go"},
		},
		Series: 8100,
		Why: "platform: ~600 (registered method+route pairs actually served, times the three " +
			"or four status classes each really returns). gateway: 500 apps × ~15 " +
			"(method, status) combinations each, which is the figure the per-app " +
			"dimension was sized against.",
	},
	{
		Name: MetricHTTPServerActive, Kind: KindUpDownCounter, Unit: "{request}",
		Attrs: []AttrSpec{
			{"http.request.method", 8, ""},
			{"toft.surface", 2, ""},
		},
		Series: 16,
		Why:    "the full product; both dimensions are tiny.",
	},
	{
		Name: MetricHTTPServerBodySize, Kind: KindHistogram, Unit: "By", Buckets: byteBuckets,
		Attrs: []AttrSpec{
			{"http.request.method", 8, ""},
			{"http.route", 200, ""},
			{"toft.surface", 2, ""},
		},
		Series: 602,
		Why: "deliberately narrower than the duration histogram: no app id and no status. " +
			"Request size is a shape-of-traffic measure nobody alerts on, and carrying the " +
			"app dimension on a second histogram would double the largest term in the whole " +
			"budget to answer a question the upstream duration already answers.",
	},
	{
		Name: MetricHTTPClientDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"server.address", 10, "the handful of hosts this platform calls out to"},
			{"http.response.status_code", 25, "folded by metricStatus"},
			{"error.type", 15, ""},
		},
		Series: 200,
		Why: "outbound calls are OIDC discovery, GitHub, Slack, the sending API and the " +
			"gallery catalog — a closed set of hosts with a handful of outcomes each.",
	},
	{
		Name: MetricAppUpstreamDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"toft.app.id", BoundApps, ""},
			{"http.response.status_code", 25, "folded by metricStatus: the code is whatever the app answered"},
			{"error.type", 15, ""},
		},
		Series: 2500,
		Why:    "500 apps × ~5 outcomes each in practice.",
	},
	{
		Name: MetricAppWakeDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"toft.app.id", BoundApps, ""},
			{"toft.outcome", 6, ""},
		},
		Series: 1500,
		Why:    "only shared-mode apps ever sleep, and a wake has three outcomes worth the series.",
	},
	{
		Name: MetricAppLogDropped, Kind: KindCounter, Unit: "{line}",
		Attrs: []AttrSpec{
			{"toft.app.id", BoundApps, ""},
			{"reason", 2, "cap | backpressure"},
		},
		Series: 1000,
		Why: "a shared app's output lines the copy to the platform's stream refused " +
			"(internal/appout), for a reader without that stream. Most apps never appear: " +
			"only an app past its cap, or a copy the destination held back, drops anything.",
	},
	{
		Name: MetricAppTransitions, Kind: KindCounter, Unit: "{transition}",
		Attrs: []AttrSpec{
			{"from", appStatusBound, "database.AppStatus*"},
			{"to", appStatusBound, "database.AppStatus*"},
			{"reason", 10, "a closed set of causes: deploy, crash, sleep, wake, stop, rollback"},
		},
		Series: 100,
		Why:    "most of the status square never happens; running↔sleeping and building→running are nearly all of it.",
	},
	{
		Name: MetricAppsByStatus, Kind: KindObservableGauge, Unit: "{app}",
		Attrs: []AttrSpec{
			{"status", appStatusBound, ""},
		},
		Series: 7,
	},
	{
		Name: MetricBuildDuration, Kind: KindHistogram, Unit: "s", Buckets: longBuckets,
		Attrs: []AttrSpec{
			{"toft.build.phase", 6, "clone | deps | build | upload | deploy | total"},
			{"toft.outcome", 6, ""},
			{"toft.framework", frameworkBound, "the detector's closed list"},
			{"toft.execution_mode", 2, "shared | isolated"},
		},
		Series: 360,
		Why:    "the full product, which is small enough not to model.",
	},
	{Name: MetricBuildQueueDepth, Kind: KindObservableGauge, Unit: "{build}", Series: 1},
	// No attributes, and there never will be: the cache is shared by every
	// app's builds on purpose, and attributing a content-addressed store to one
	// app is not a thing that can be done honestly.
	{Name: MetricBuildCacheBytes, Kind: KindObservableGauge, Unit: "By", Series: 1},
	{Name: MetricBuildQueueWait, Kind: KindHistogram, Unit: "s", Buckets: longBuckets, Series: 1},
	{Name: MetricBuildActive, Kind: KindUpDownCounter, Unit: "{build}", Series: 1},
	{
		Name: MetricDeployDuration, Kind: KindHistogram, Unit: "s", Buckets: longBuckets,
		Attrs: []AttrSpec{
			{"toft.outcome", 6, ""},
			{"toft.execution_mode", 2, ""},
			{"toft.rollback", 2, "true | false"},
		},
		Series: 24,
	},
	{
		Name: MetricJobRunDuration, Kind: KindHistogram, Unit: "s", Buckets: longBuckets,
		Attrs: []AttrSpec{
			{"toft.outcome", 6, ""},
			{"toft.trigger", triggerBound, "person | schedule | app"},
		},
		Series: 18,
	},
	{Name: MetricJobQueueDepth, Kind: KindObservableGauge, Unit: "{run}", Series: 1},
	{
		Name: MetricJobRunMemoryPeak, Kind: KindHistogram, Unit: "By", Buckets: byteBuckets,
		Attrs: []AttrSpec{
			{"toft.outcome", 6, ""},
		},
		Series: 6,
	},
	{
		Name: MetricScheduledRunDuration, Kind: KindHistogram, Unit: "s", Buckets: longBuckets,
		Attrs: []AttrSpec{
			{"toft.outcome", 6, ""},
			{"toft.trigger", triggerBound, ""},
			{"toft.skip_reason", 8, "a closed set; a schedule that skips records why"},
		},
		Series: 144,
	},
	{
		Name: MetricNotifySendDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"channel", IntegrationKindBound + 1, "email, or a catalog kind"},
			{"audience", 4, "internal/notify's audience names"},
			{"toft.outcome", 6, ""},
		},
		// Modeled rather than the product: an install has one or two channels
		// configured, not eleven, and the audiences a given channel carries are
		// at most two.
		Series: 48,
	},
	{
		Name: MetricIntegrationSends, Kind: KindCounter, Unit: "{send}",
		Attrs: []AttrSpec{
			{"kind", IntegrationKindBound, "appint.Known's names"},
			{"action", 4, "post | file | send | attach | upload | trigger | resolve"},
			{"toft.outcome", 5, "queued | sent | failed | blocked | abandoned"},
		},
		// One kind has one or two actions, and an install runs two or three
		// kinds. **A connection id is never a label**: it is per install and
		// unbounded over time, and the ledger is where a specific connection's
		// history is read.
		Series: 40,
	},
	{
		Name: MetricAuthSignin, Kind: KindCounter, Unit: "{attempt}",
		Attrs: []AttrSpec{
			{"toft.provider.kind", 4, "local | oidc | github | saml"},
			{"toft.outcome", 8, "succeeded | bad_credential | locked | no_account | pending | must_change | error"},
		},
		Series: 32,
	},
	{
		Name: MetricArtifactDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"operation", 4, "put | get | presign | delete"},
			{"backend", 3, "local | s3"},
			{"toft.outcome", 6, ""},
		},
		Series: 66,
	},
	{
		Name: MetricDBPoolConnections, Kind: KindObservableGauge, Unit: "{connection}",
		Attrs: []AttrSpec{
			{"state", 4, "in_use | idle | open | max"},
		},
		Series: 4,
		Why: "`max` is a constant and is carried anyway: pool exhaustion is only " +
			"legible next to the ceiling it hit, and an operator should not have to " +
			"know this codebase's SetMaxOpenConns to read the graph.",
	},
	{
		Name: MetricDBPoolWaits, Kind: KindObservableGauge, Unit: "{wait}", Series: 1,
		Why: "cumulative count of acquires that had to wait. A gauge over a " +
			"monotonic total rather than a histogram, because this is a database/sql " +
			"pool over the pgx stdlib driver and sql.DBStats has no per-acquire " +
			"duration to bucket — see database.PoolStats. rate() over this and the " +
			"one below is what an exhaustion alert actually fires on.",
	},
	{
		Name: MetricDBPoolWaitDuration, Kind: KindObservableGauge, Unit: "s", Series: 1,
		Why: "cumulative seconds spent waiting for a connection.",
	},
	{
		Name: MetricSweepDuration, Kind: KindHistogram, Unit: "s", Buckets: longBuckets,
		Attrs: []AttrSpec{
			{"sweep", 12, "reconcile | health | idle | consumption | prune | dormancy | " +
				"integrations | jobs | releases | platform_log | " +
				"announcement_recipients | access_record"},
			{"toft.outcome", 6, ""},
		},
		Series: 72,
	},
	{
		// What a retention sweep *cost*, which is the half no signal in this
		// product carried. The settings change that caused it is already in
		// `audit_log`; its effect is a measurement, and a row per pass of every
		// sweep would be growth in the feed with exactly two readers. The one
		// exception is the access_record sweep, which writes audit_log.pruned
		// because what it deletes *is* the audit log — and those rows age out
		// on the clock they report, so they are bounded by the dial itself.
		//
		// `subject` exists because three sweeps remove more than one kind of
		// thing on one pass and the figures mean different things: jobs expire
		// log lines on a 14-day clock and run rows on a 90-day one, and one
		// number makes both unreadable. What is *not* here is the app id — the
		// unbounded dimension that would spend the whole budget to answer a
		// question rows already answer. Which app lost a release is on the span.
		Name: MetricSweepRemoved, Kind: KindCounter, Unit: "{row}",
		Attrs: []AttrSpec{
			{"sweep", 12, "reconcile | health | idle | consumption | prune | dormancy | " +
				"integrations | jobs | releases | platform_log | " +
				"announcement_recipients | access_record"},
			{"subject", 4, "rows | log_lines | objects | releases"},
		},
		Series: 48,
		Why: "one instrument with a dimension rather than nine — `toft.jobs.expired`, " +
			"`toft.releases.expired` and so on would make another sweep a schema change " +
			"instead of a new attribute value. Most of the 48 modeled sets never appear: " +
			"only some of the sweeps delete anything at all.",
	},
	{
		Name: MetricAutodeployPollDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"toft.outcome", 6, ""},
		},
		Series: 6,
	},
	{
		Name: MetricAutodeploySkipped, Kind: KindCounter, Unit: "{skip}",
		Attrs: []AttrSpec{
			{"toft.skip_reason", 8, "including the source-policy skip of ADR §29"},
		},
		Series: 8,
	},
	{
		Name: MetricGitHookDeliveries, Kind: KindCounter, Unit: "{delivery}",
		Attrs: []AttrSpec{
			{"toft.outcome", 3, "accepted, ignored, or unverified"},
		},
		Series: 3,
	},
	{
		Name: MetricGallerySyncDuration, Kind: KindHistogram, Unit: "s", Buckets: requestBuckets,
		Attrs: []AttrSpec{
			{"toft.outcome", 6, ""},
		},
		Series: 6,
	},
	{
		// One point per build that resolved dependencies. `ecosystem` is the
		// three this binary knows plus the mixed case; `outcome` says whether
		// the resolve worked, was unresolvable, or was skipped.
		Name: MetricDependencyScans, Kind: KindCounter, Unit: "{scan}",
		Attrs: []AttrSpec{
			{"toft.dependency.ecosystem", 4, "npm, pypi, go, mixed"},
			{"toft.outcome", 6, ""},
		},
		Series: 24,
	},
	{
		// One point per deploy this policy turned away. `severity` is the four
		// levels plus unknown plus malware — never the advisory id.
		Name: MetricDependencyRefusals, Kind: KindCounter, Unit: "{refusal}",
		Attrs: []AttrSpec{
			{"toft.dependency.severity", 6, "the four levels, unknown, malware"},
			{"toft.dependency.mode", 3, "off, warn, block"},
		},
		Series: 18,
	},
	{
		Name: MetricDependencyWaivers, Kind: KindCounter, Unit: "{waiver}",
		Attrs: []AttrSpec{
			{"toft.dependency.action", 2, "granted, revoked"},
		},
		Series: 2,
	},
	{
		// The age of the *oldest* corpus, in seconds, so an alert can fire on a
		// feed that has quietly stopped arriving. A gauge rather than a counter
		// because staleness is a level, and read on the export interval rather
		// than on any request path.
		Name: MetricDependencyFeedAge, Kind: KindObservableGauge, Unit: "s",
		Attrs: []AttrSpec{
			{"toft.dependency.ecosystem", 4, "npm, pypi, go, mixed"},
		},
		Series: 4,
	},
	{
		Name: MetricTelemetryDropped, Kind: KindCounter, Unit: "{item}",
		Attrs: []AttrSpec{
			{"signal", 3, "traces | metrics | logs"},
			{"reason", 4, "queue_full | app_cardinality_cap | export_timeout | shutdown"},
		},
		Series: 12,
		Why:    "the pipeline's own honesty metric: a ceiling that is hit silently is indistinguishable from one that is not.",
	},
	{
		Name: MetricTelemetryExportFailures, Kind: KindCounter, Unit: "{failure}",
		Attrs: []AttrSpec{
			{"signal", 3, ""},
			{"error.type", 15, ""},
		},
		Series: 45,
	},
	{
		Name: MetricBuildInfo, Kind: KindObservableGauge, Unit: "{info}",
		Attrs: []AttrSpec{
			{"version", 1, "one value per process"},
			{"revision", 1, "one value per process"},
		},
		Series: 1,
		Why: "always 1; the labels are the payload, which is the standard shape for a " +
			"build-info series. The unit is the pseudo-unit `{info}` rather than the " +
			"literal `1` the PRD wrote, because the OTel→Prometheus translator renders " +
			"a unitless gauge as `_ratio` — `toft_build_info_ratio` is not a name " +
			"anybody would look for.",
	},
}

// GoRuntimeSeries is what contrib/instrumentation/runtime adds. Counted in the
// budget without being declared instrument by instrument: the set is the Go
// team's, not this project's, and pinning it here would make a dependency
// upgrade fail a test about *our* cardinality.
const GoRuntimeSeries = 40

// specIndex is built at package init so a typo in a metric name is a panic on
// the first run rather than an instrument that silently never records.
var specIndex = func() map[string]Spec {
	m := make(map[string]Spec, len(Inventory))
	for _, s := range Inventory {
		if _, dup := m[s.Name]; dup {
			panic("telemetry: duplicate instrument " + s.Name)
		}
		m[s.Name] = s
	}
	return m
}()

func spec(name string) Spec {
	s, ok := specIndex[name]
	if !ok {
		panic("telemetry: no declared spec for " + name + " — add it to Inventory")
	}
	return s
}

// largestSeries is the modeled series count of the largest instrument, which
// is what the meter provider's per-instrument cardinality limit is set to.
func largestSeries() int {
	n := 0
	for _, s := range Inventory {
		if s.Series > n {
			n = s.Series
		}
	}
	return n
}

// SortedInventory returns the inventory by name, for the snapshot test.
func SortedInventory() []Spec {
	out := append([]Spec(nil), Inventory...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- construction -----------------------------------------------------------

// Instruments holds every instrument. Built from the specs above, so a unit or
// a bucket set cannot drift from the table the snapshot test freezes.
type Instruments struct {
	httpServerDuration metric.Float64Histogram
	httpServerActive   metric.Int64UpDownCounter
	httpServerBodySize metric.Int64Histogram
	httpClientDuration metric.Float64Histogram

	upstreamDuration metric.Float64Histogram
	wakeDuration     metric.Float64Histogram
	appTransitions   metric.Int64Counter
	appLogDropped    metric.Int64Counter

	buildDuration  metric.Float64Histogram
	buildQueueWait metric.Float64Histogram
	buildActive    metric.Int64UpDownCounter
	deployDuration metric.Float64Histogram

	jobRunDuration     metric.Float64Histogram
	jobRunMemoryPeak   metric.Int64Histogram
	scheduledRunDur    metric.Float64Histogram
	notifySendDuration metric.Float64Histogram
	integrationSends   metric.Int64Counter
	signinAttempts     metric.Int64Counter
	artifactDuration   metric.Float64Histogram

	sweepDuration     metric.Float64Histogram
	sweepRemoved      metric.Int64Counter
	autodeployPoll    metric.Float64Histogram
	autodeploySkipped metric.Int64Counter
	gitHookDeliveries metric.Int64Counter
	gallerySync       metric.Float64Histogram

	dependencyScans    metric.Int64Counter
	dependencyRefusals metric.Int64Counter
	dependencyWaivers  metric.Int64Counter

	telemetryDropped metric.Int64Counter
	telemetryFailed  metric.Int64Counter
}

func newInstruments(m metric.Meter) (*Instruments, error) {
	var err error
	fh := func(name string) metric.Float64Histogram {
		s := spec(name)
		h, e := m.Float64Histogram(name,
			metric.WithUnit(s.Unit),
			metric.WithExplicitBucketBoundaries(s.Buckets...))
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", name, e)
		}
		return h
	}
	ih := func(name string) metric.Int64Histogram {
		s := spec(name)
		h, e := m.Int64Histogram(name,
			metric.WithUnit(s.Unit),
			metric.WithExplicitBucketBoundaries(s.Buckets...))
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", name, e)
		}
		return h
	}
	ic := func(name string) metric.Int64Counter {
		s := spec(name)
		c, e := m.Int64Counter(name, metric.WithUnit(s.Unit))
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", name, e)
		}
		return c
	}
	iu := func(name string) metric.Int64UpDownCounter {
		s := spec(name)
		c, e := m.Int64UpDownCounter(name, metric.WithUnit(s.Unit))
		if e != nil && err == nil {
			err = fmt.Errorf("%s: %w", name, e)
		}
		return c
	}

	i := &Instruments{
		httpServerDuration: fh(MetricHTTPServerDuration),
		httpServerActive:   iu(MetricHTTPServerActive),
		httpServerBodySize: ih(MetricHTTPServerBodySize),
		httpClientDuration: fh(MetricHTTPClientDuration),
		upstreamDuration:   fh(MetricAppUpstreamDuration),
		wakeDuration:       fh(MetricAppWakeDuration),
		appTransitions:     ic(MetricAppTransitions),
		appLogDropped:      ic(MetricAppLogDropped),
		buildDuration:      fh(MetricBuildDuration),
		buildQueueWait:     fh(MetricBuildQueueWait),
		buildActive:        iu(MetricBuildActive),
		deployDuration:     fh(MetricDeployDuration),
		jobRunDuration:     fh(MetricJobRunDuration),
		jobRunMemoryPeak:   ih(MetricJobRunMemoryPeak),
		scheduledRunDur:    fh(MetricScheduledRunDuration),
		notifySendDuration: fh(MetricNotifySendDuration),
		integrationSends:   ic(MetricIntegrationSends),
		signinAttempts:     ic(MetricAuthSignin),
		artifactDuration:   fh(MetricArtifactDuration),
		sweepDuration:      fh(MetricSweepDuration),
		sweepRemoved:       ic(MetricSweepRemoved),
		autodeployPoll:     fh(MetricAutodeployPollDuration),
		autodeploySkipped:  ic(MetricAutodeploySkipped),
		gitHookDeliveries:  ic(MetricGitHookDeliveries),
		gallerySync:        fh(MetricGallerySyncDuration),
		dependencyScans:    ic(MetricDependencyScans),
		dependencyRefusals: ic(MetricDependencyRefusals),
		dependencyWaivers:  ic(MetricDependencyWaivers),
		telemetryDropped:   ic(MetricTelemetryDropped),
		telemetryFailed:    ic(MetricTelemetryExportFailures),
	}
	if err != nil {
		return nil, err
	}
	if err := registerObservers(m); err != nil {
		return nil, err
	}
	return i, nil
}

// --- observable gauges ------------------------------------------------------
//
// The four gauges read state this package does not own: the database's view of
// the estate, the builder's queue, the job service's queue, the pgx pool. They
// are registered as callbacks that read a pointer set later, because Init runs
// before any of those exist — telemetry has to be up before the first thing
// worth measuring is constructed, or the construction itself is invisible.
//
// A nil source observes nothing at all rather than observing zero. Zero is a
// claim ("the queue is empty"); absent is the truth ("nothing is reporting").

var (
	appsByStatusSource atomic.Pointer[func() map[string]int64]
	buildQueueSource   atomic.Pointer[func() int64]
	buildCacheSource   atomic.Pointer[func() (int64, bool)]
	jobQueueSource     atomic.Pointer[func() int64]
	dbPoolSource       atomic.Pointer[func() PoolStats]
	buildInfoAttrs     atomic.Pointer[[]attribute.KeyValue]
	feedAgeSource      atomic.Pointer[func() map[string]float64]
)

// ObserveAppsByStatus supplies the estate inventory. Called once, after the
// database is open.
func ObserveAppsByStatus(f func() map[string]int64) { appsByStatusSource.Store(&f) }

// ObserveBuildQueue supplies the pending-build count.
func ObserveBuildQueue(f func() int64) { buildQueueSource.Store(&f) }

// ObserveBuildCacheBytes supplies the shared build cache's size.
//
// The supplier must not walk the directory: it reads a figure measured on the
// builder's own slow ticker. The export interval is not a safe place to do I/O
// proportional to the thing being measured, and a metrics endpoint that gets
// slower exactly as the cache gets more interesting is the observability that
// breaks the install it is observing.
//
// It returns a second value where the queue-depth supplier does not, because
// this quantity has a state the others have not got: **not measured yet**. A
// gauge is not allowed to answer that with a zero — an install in the first
// quarter of an hour after a restart has an unmeasured cache, not an empty one,
// and a series that reads 0 after every restart is a graph nobody can alert on.
// False means observe nothing at all this round.
func ObserveBuildCacheBytes(f func() (int64, bool)) { buildCacheSource.Store(&f) }

// ObserveJobQueue supplies the queued-run count.
func ObserveJobQueue(f func() int64) { jobQueueSource.Store(&f) }

// PoolStats is what the database pool can honestly report. Pool exhaustion is
// the failure that looks like "everything is slow", and these four numbers are
// what name it.
type PoolStats struct {
	InUse, Idle, Open, Max int64
	// Waits and WaitSeconds are monotonic totals since process start. An alert
	// is written as a rate over them.
	Waits       int64
	WaitSeconds float64
}

// ObserveDBPool supplies the pool's own accounting.
func ObserveDBPool(f func() PoolStats) { dbPoolSource.Store(&f) }

// ObserveDependencyFeedAge supplies each ecosystem's corpus age in seconds.
//
// A gauge because staleness is a level, and read on the export interval rather
// than on any request path. An ecosystem that has never loaded reports nothing
// at all rather than zero — zero would read as "perfectly fresh", which is the
// exact opposite of the truth, and an alert on a feed that stopped arriving
// must not be silenced by the case where it never started.
func ObserveDependencyFeedAge(f func() map[string]float64) { feedAgeSource.Store(&f) }

// clearObservers drops every source, so a final collection during shutdown
// observes nothing rather than reading a database that is closing.
func clearObservers() {
	appsByStatusSource.Store(nil)
	buildQueueSource.Store(nil)
	buildCacheSource.Store(nil)
	jobQueueSource.Store(nil)
	dbPoolSource.Store(nil)
	buildInfoAttrs.Store(nil)
	feedAgeSource.Store(nil)
}

func registerObservers(m metric.Meter) error {
	appsGauge, err := m.Int64ObservableGauge(MetricAppsByStatus, metric.WithUnit(spec(MetricAppsByStatus).Unit))
	if err != nil {
		return err
	}
	buildQueue, err := m.Int64ObservableGauge(MetricBuildQueueDepth, metric.WithUnit(spec(MetricBuildQueueDepth).Unit))
	if err != nil {
		return err
	}
	buildCache, err := m.Int64ObservableGauge(MetricBuildCacheBytes, metric.WithUnit(spec(MetricBuildCacheBytes).Unit))
	if err != nil {
		return err
	}
	jobQueue, err := m.Int64ObservableGauge(MetricJobQueueDepth, metric.WithUnit(spec(MetricJobQueueDepth).Unit))
	if err != nil {
		return err
	}
	poolGauge, err := m.Int64ObservableGauge(MetricDBPoolConnections, metric.WithUnit(spec(MetricDBPoolConnections).Unit))
	if err != nil {
		return err
	}
	poolWaits, err := m.Int64ObservableGauge(MetricDBPoolWaits, metric.WithUnit(spec(MetricDBPoolWaits).Unit))
	if err != nil {
		return err
	}
	poolWaitSeconds, err := m.Float64ObservableGauge(MetricDBPoolWaitDuration, metric.WithUnit(spec(MetricDBPoolWaitDuration).Unit))
	if err != nil {
		return err
	}
	infoGauge, err := m.Int64ObservableGauge(MetricBuildInfo, metric.WithUnit(spec(MetricBuildInfo).Unit))
	if err != nil {
		return err
	}
	feedAge, err := m.Float64ObservableGauge(MetricDependencyFeedAge,
		metric.WithUnit(spec(MetricDependencyFeedAge).Unit))
	if err != nil {
		return err
	}

	_, err = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		if f := appsByStatusSource.Load(); f != nil {
			for status, n := range (*f)() {
				o.ObserveInt64(appsGauge, n, metric.WithAttributes(attribute.String("status", status)))
			}
		}
		if f := buildQueueSource.Load(); f != nil {
			o.ObserveInt64(buildQueue, (*f)())
		}
		// Absent until the first measurement completes, rather than zero. An
		// install whose cache has never been walked has not got an empty cache,
		// and a gauge that says 0 for the first quarter of an hour after every
		// restart is a graph nobody can read.
		if f := buildCacheSource.Load(); f != nil {
			if bytes, measured := (*f)(); measured {
				o.ObserveInt64(buildCache, bytes)
			}
		}
		if f := jobQueueSource.Load(); f != nil {
			o.ObserveInt64(jobQueue, (*f)())
		}
		if f := dbPoolSource.Load(); f != nil {
			st := (*f)()
			o.ObserveInt64(poolGauge, st.InUse, metric.WithAttributes(attribute.String("state", "in_use")))
			o.ObserveInt64(poolGauge, st.Idle, metric.WithAttributes(attribute.String("state", "idle")))
			o.ObserveInt64(poolGauge, st.Open, metric.WithAttributes(attribute.String("state", "open")))
			o.ObserveInt64(poolGauge, st.Max, metric.WithAttributes(attribute.String("state", "max")))
			o.ObserveInt64(poolWaits, st.Waits)
			o.ObserveFloat64(poolWaitSeconds, st.WaitSeconds)
		}
		if a := buildInfoAttrs.Load(); a != nil {
			o.ObserveInt64(infoGauge, 1, metric.WithAttributes(*a...))
		}
		if f := feedAgeSource.Load(); f != nil {
			for ecosystem, age := range (*f)() {
				o.ObserveFloat64(feedAge, age, metric.WithAttributes(
					attribute.String("toft.dependency.ecosystem", ecosystem)))
			}
		}
		return nil
	}, appsGauge, buildQueue, buildCache, jobQueue, poolGauge, poolWaits, poolWaitSeconds, infoGauge, feedAge)
	return err
}

// buildInfo publishes the version-and-revision series. Always 1; the labels are
// the payload, which is the shape every Prometheus-shaped stack expects and the
// reason a dashboard can say which build produced a regression.
func buildInfo(opts Options) {
	attrs := []attribute.KeyValue{
		attribute.String("version", opts.Version),
		attribute.String("revision", opts.Revision),
	}
	buildInfoAttrs.Store(&attrs)
}

// --- signals and reasons ----------------------------------------------------

// Sign-in outcomes, beyond the shared succeeded/failed vocabulary.
//
// `must_change` is deliberately not a success: the password verified and
// nothing was minted but the right to replace it (the published-default-password
// rule in CLAUDE.md). A graph counting it as a sign-in would report an install
// still holding its default password as signing people in perfectly well.
const (
	SigninBadCredential = "bad_credential"
	SigninNoAccount     = "no_account"
	SigninLocked        = "locked"
	// SigninPending is an account that exists and has not been activated. Its
	// own outcome rather than a shade of `locked` or `no_account`, because it
	// is the one refusal here that resolves by somebody in this organization
	// doing something — a graph that folded it into either of those would hide
	// a queue nobody is working.
	SigninPending    = "pending"
	SigninThrottled  = "throttled"
	SigninMustChange = "must_change"
	SigninError      = "error"
)

// The notification channels. `internal/slack` never imports `internal/mail`
// (ADR §24), so the shared vocabulary lives where both can reach it — and this
// is the metrics half of the same arrangement `internal/notify` is for
// audiences.
//
// `channel` is mail or a catalog kind, which is why there are two constants and
// not eleven: every other value is `appint.Known`'s own name, drawn from a list
// this package deliberately does not import. Its *bound* is here, because a
// budget is this package's business and a name is not.
const (
	ChannelEmail = "email"
	ChannelSlack = "slack"
)

// IntegrationKindBound is how many integration kinds the catalog may grow to
// before this budget has to be argued again.
//
// It is a bound rather than a count, and it is the reason `kind` is a label at
// all: the catalog is a compiled-in list, so its length is a property of the
// binary rather than of an install's traffic. A **connection id** has neither
// property — it is per install and unbounded over time — and it is never a
// label anywhere. Where a specific connection's history is read is the ledger.
const IntegrationKindBound = 16

// Trigger names — a run's three provenances, which ADR §26 and §27 both insist
// stay distinguishable: a person, a schedule, or the app itself. Never the
// actor's id; a person is not a metric attribute.
const (
	TriggerPerson   = "person"
	TriggerSchedule = "schedule"
	TriggerApp      = "app"
)

// Why an app changed status, for toft.app.state.transitions. A closed set,
// and the reason the instrument is worth having at all: `sleeping` is not
// `stopped` (ADR §16), and a graph that could not tell a platform decision from
// a person's decision would flatten exactly the distinction that made them two
// statuses.
const (
	ReasonDeploy   = "deploy"
	ReasonRollback = "rollback"
	ReasonCrash    = "crash"
	ReasonIdle     = "idle"
	ReasonWake     = "wake"
	ReasonStop     = "stop"
	// ReasonOutOfMemory is the health watcher stopping a workload the backend
	// kept killing for its memory cap. Its own value rather than ReasonCrash
	// for the reason the set exists: this is a platform decision with a lever
	// behind it — the install's memory ceiling, or the app's own appetite — and an
	// estate where it is the common transition is one an operator can act on,
	// which "crash" would never have told them.
	ReasonOutOfMemory = "out_of_memory"
)

// Sweep names, for toft.sweep.duration. A closed set: `sweep` is a metric
// attribute, and a background job that invented its own name would be an
// unbounded label arriving from the one place nobody watches.
//
// **A sweep is named for the subsystem whose data it touches**, not for the
// goroutine that runs it or the trigger that starts it. Stated because the next
// sweep to be added should not have to guess, and because the list drifted from
// its own declared bound once already — a name was being emitted that the
// attribute's note did not mention, and nothing compared the two until
// TestSweepEnumMatchesTheConstants existed.
//
// The one pair that needs a word is cron's. Two passes touch the same rows on
// two different clocks — an hourly retention DELETE and a per-tick recovery of
// rows nothing is tracking any more — and folding them into one name would put
// a batch delete and a single UPDATE in one duration histogram, which makes
// both unreadable for exactly the reason `subject` splits the removal counter.
const (
	SweepReconcile    = "reconcile"
	SweepHealth       = "health"
	SweepIdle         = "idle"
	SweepConsumption  = "consumption"
	SweepPrune        = "prune"
	SweepDormancy     = "dormancy"
	SweepIntegrations = "integrations"
	SweepJobs         = "jobs"
	SweepReleases     = "releases"
	SweepPlatformLog  = "platform_log"
	// SweepAnnouncementRecipients prunes who each announcement was mailed to,
	// and never the announcement or its counts. Its own name rather than a
	// share of `prune`, for the reason the platform log's is: two sweeps with
	// two clocks and two cutoffs are indistinguishable in the operator's metric
	// otherwise, which is the one question that metric is for.
	SweepAnnouncementRecipients = "announcement_recipients"
	// SweepAccessRecord cuts audit_log and the two usage rollups on
	// access_record_retention_days, one cutoff for all three.
	SweepAccessRecord = "access_record"
)

// SweepNames is the enum, in one place a test can read.
//
// The inventory's `sweep` attribute keeps its bound and its note as literals,
// because that table is the contract and a derived cell is a cell nobody reads.
// TestSweepEnumMatchesTheConstants is what makes the literals honest: a constant
// added without a bound raise — or a note left describing the set it used to be
// — fails the build rather than shipping a label the budget did not count.
var SweepNames = []string{
	SweepReconcile,
	SweepHealth,
	SweepIdle,
	SweepConsumption,
	SweepPrune,
	SweepDormancy,
	SweepIntegrations,
	SweepJobs,
	SweepReleases,
	SweepPlatformLog,
	SweepAnnouncementRecipients,
	SweepAccessRecord,
}

// What a sweep removed, for toft.sweep.removed.
//
// Four rather than three: `objects` and `releases` are both "things the artifact
// store and the disk lost", and collapsing them would save twelve series, which
// is nothing. They stay apart because **a release is a rollback target and an
// object is not** — one of them going is the deletion somebody walks into.
const (
	SubjectRows     = "rows"
	SubjectLogLines = "log_lines"
	SubjectObjects  = "objects"
	SubjectReleases = "releases"
)

// SubjectNames is that enum, checked against the attribute the same way
// SweepNames is.
var SubjectNames = []string{SubjectRows, SubjectLogLines, SubjectObjects, SubjectReleases}

const (
	SignalTraces  = "traces"
	SignalMetrics = "metrics"
	SignalLogs    = "logs"

	DropReasonQueueFull = "queue_full"
	DropReasonAppCap    = "app_cardinality_cap"
	DropReasonTimeout   = "export_timeout"
	DropReasonShutdown  = "shutdown"
)

// --- record paths -----------------------------------------------------------
//
// Every function below returns on `metricsOn` before it touches an argument.
// That is what makes an uninstrumented install pay one atomic load per call
// site and allocate nothing — asserted by TestNoopRecordAllocatesNothing rather
// than assumed.

// RecordHTTPServer records one served request. Called from the middleware after
// the response is written, which is also when chi can finally say which route
// pattern matched.
func RecordHTTPServer(ctx context.Context, surface, method, route, scheme string, status int, appID string, dur time.Duration, bodySize int64) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("http.route", route),
		attribute.Int("http.response.status_code", metricStatus(status)),
		attribute.String("url.scheme", scheme),
		attribute.String("toft.surface", surface),
	}
	if appID != "" {
		attrs = append(attrs, attribute.String("toft.app.id", AppAttr(appID)))
	}
	if et := StatusErrorType(status); et != ErrNone {
		attrs = append(attrs, attribute.String("error.type", et))
	}
	p.inst.httpServerDuration.Record(ctx, seconds(dur), metric.WithAttributes(attrs...))

	if bodySize > 0 {
		p.inst.httpServerBodySize.Record(ctx, bodySize, metric.WithAttributes(
			attribute.String("http.request.method", method),
			attribute.String("http.route", route),
			attribute.String("toft.surface", surface),
		))
	}
}

// metricStatus folds a status code into the closed set a metric may carry.
//
// On the apps surface the code is the app's — ReverseProxy relays anything
// from 100 to 999 — and ADR §31 says a metric attribute is never something an
// app supplies. The codes this platform and an ordinary app actually answer
// are kept; anything else folds to its class's x00, which is how RFC 9110 §15
// tells a client to treat a code it does not recognise. A code outside 1xx–5xx
// belongs to no class and folds to 500: the upstream answered something that
// is not HTTP, which is a server fault. 23 kept codes plus the 100 and 300
// folds is the 25 the inventory declares. Span attributes stay raw; a span is
// not a series.
func metricStatus(code int) int {
	switch code {
	case 101, 200, 201, 204, 206, 301, 302, 304, 307, 308,
		400, 401, 403, 404, 405, 409, 413, 422, 429,
		500, 502, 503, 504:
		return code
	}
	if code < 100 || code > 599 {
		return 500
	}
	return code / 100 * 100
}

// AddHTTPActive moves the in-flight gauge. Two calls per request, both cheap,
// and the only instrument here that is touched *before* the handler runs.
func AddHTTPActive(ctx context.Context, surface, method string, delta int64) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.httpServerActive.Add(ctx, delta, metric.WithAttributes(
		attribute.String("http.request.method", method),
		attribute.String("toft.surface", surface),
	))
}

// RecordHTTPClient records an outbound call the platform makes.
func RecordHTTPClient(ctx context.Context, host string, status int, err error, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("server.address", host)}
	if status > 0 {
		attrs = append(attrs, attribute.Int("http.response.status_code", metricStatus(status)))
	}
	if et := firstErrorType(err, status); et != ErrNone {
		attrs = append(attrs, attribute.String("error.type", et))
	}
	p.inst.httpClientDuration.Record(ctx, seconds(dur), metric.WithAttributes(attrs...))
}

// RecordUpstream records the app's own half of a gateway request — the time
// Toft spent waiting for somebody else's Flask process, as opposed to the
// time the request spent in the gateway.
func RecordUpstream(ctx context.Context, appID string, status int, err error, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("toft.app.id", AppAttr(appID))}
	if status > 0 {
		attrs = append(attrs, attribute.Int("http.response.status_code", metricStatus(status)))
	}
	if et := firstErrorType(err, status); et != ErrNone {
		attrs = append(attrs, attribute.String("error.type", et))
	}
	p.inst.upstreamDuration.Record(ctx, seconds(dur), metric.WithAttributes(attrs...))
}

// RecordWake records an app waking from sleep (ADR §16).
func RecordWake(ctx context.Context, appID, outcome string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.wakeDuration.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("toft.app.id", AppAttr(appID)),
		attribute.String("toft.outcome", outcome),
	))
}

// RecordAppLogDropped counts a shared app's output lines the copy refused.
func RecordAppLogDropped(ctx context.Context, appID, reason string, n int64) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.appLogDropped.Add(ctx, n, metric.WithAttributes(
		attribute.String("toft.app.id", AppAttr(appID)),
		attribute.String("reason", reason),
	))
}

// RecordAppTransition counts a change of app status.
func RecordAppTransition(ctx context.Context, from, to, reason string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.appTransitions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("from", from),
		attribute.String("to", to),
		attribute.String("reason", reason),
	))
}

// RecordBuildPhase records one phase of a build. The `total` phase is the whole
// build, recorded once at the end — an operator asking "why was that deploy
// slow" wants the split and the total from one series family, not from two.
func RecordBuildPhase(ctx context.Context, phase, outcome, framework, mode string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.buildDuration.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("toft.build.phase", phase),
		attribute.String("toft.outcome", outcome),
		attribute.String("toft.framework", framework),
		attribute.String("toft.execution_mode", mode),
	))
}

// RecordBuildQueueWait records how long a deployment sat before a worker took
// it — the figure that turns "builds are slow" into "builds are queued".
func RecordBuildQueueWait(ctx context.Context, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.buildQueueWait.Record(ctx, seconds(dur))
}

// AddBuildActive moves the concurrent-build gauge.
func AddBuildActive(ctx context.Context, delta int64) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.buildActive.Add(ctx, delta)
}

// RecordDeploy records the deploy phase on its own, split by whether it was a
// rollback — a rollback is the fast path, and averaging the two hides it.
func RecordDeploy(ctx context.Context, outcome, mode string, rollback bool, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.deployDuration.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("toft.outcome", outcome),
		attribute.String("toft.execution_mode", mode),
		attribute.Bool("toft.rollback", rollback),
	))
}

// RecordJobRun records a finished job run. peakBytes is 0 when the runner never
// reported one, and 0 is not recorded — a missing measurement must not land in
// the smallest bucket and drag the distribution down.
func RecordJobRun(ctx context.Context, outcome, trigger string, dur time.Duration, peakBytes int64) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.jobRunDuration.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("toft.outcome", outcome),
		attribute.String("toft.trigger", trigger),
	))
	if peakBytes > 0 {
		p.inst.jobRunMemoryPeak.Record(ctx, peakBytes, metric.WithAttributes(
			attribute.String("toft.outcome", outcome),
		))
	}
}

// RecordScheduledRun records a scheduled task run, including a skipped one.
//
// A skip is a first-class outcome here for the same reason it is a row in the
// database (ADR §27): "it did not happen" must never have to be inferred from a
// gap in a graph.
func RecordScheduledRun(ctx context.Context, outcome, trigger, skipReason string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.scheduledRunDur.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("toft.outcome", outcome),
		attribute.String("toft.trigger", trigger),
		attribute.String("toft.skip_reason", skipReason),
	))
}

// RecordNotify records one notification send attempt, on either channel.
func RecordNotify(ctx context.Context, channel, audience, outcome string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.notifySendDuration.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("channel", channel),
		attribute.String("audience", audience),
		attribute.String("toft.outcome", outcome),
	))
}

// RecordIntegrationSend counts one app-initiated send by kind, action and the
// outcome the ledger recorded.
//
// The **destination** is never an attribute: it is a workspace's name for a
// room or an organization's name for a folder — free text somebody typed, and
// an unbounded label — and it is on the list ADR §31 D7 says never leaves this
// process at all.
func RecordIntegrationSend(ctx context.Context, kind, action, outcome string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.integrationSends.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", kind),
		attribute.String("action", action),
		attribute.String("toft.outcome", outcome),
	))
}

// RecordSignin counts a sign-in attempt by provider kind and outcome.
//
// The provider *kind*, never its id or its display name: an administrator names
// providers, and a name somebody types is not a bounded set.
func RecordSignin(ctx context.Context, providerKind, outcome string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.signinAttempts.Add(ctx, 1, metric.WithAttributes(
		attribute.String("toft.provider.kind", providerKind),
		attribute.String("toft.outcome", outcome),
	))
}

// RecordArtifact records one artifact-store operation.
func RecordArtifact(ctx context.Context, operation, backend, outcome string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.artifactDuration.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.String("backend", backend),
		attribute.String("toft.outcome", outcome),
	))
}

// RecordSweep records one pass of a background sweep.
func RecordSweep(ctx context.Context, sweep, outcome string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.sweepDuration.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("sweep", sweep),
		attribute.String("toft.outcome", outcome),
	))
}

// Removed is what one sweep pass deleted, by subject.
//
// A struct rather than four calls because a pass reports once: the counter and
// the span take their figures from the same value, so a series and a trace
// cannot end up disagreeing about a number they both came from.
type Removed struct {
	Rows     int64
	LogLines int64
	Objects  int64
	Releases int64
}

// RecordSweepRemoved counts what one pass deleted and puts the same figures on
// its span.
//
// Zero is not recorded, on either. A counter that never moves and a counter
// incremented by nothing are the same series to a backend, and a sweep with
// nothing to do is the ordinary case for most of these — the duration histogram
// is what says the pass happened at all.
func RecordSweepRemoved(ctx context.Context, span Span, sweep string, r Removed) {
	// Both halves off, and this is the whole of the zero-allocation property:
	// the table below is built after the return, not before it.
	p := live.Load()
	metricsOn := p != nil && p.metrics
	if !metricsOn && span.span == nil {
		return
	}
	for _, s := range [...]struct {
		subject string
		key     attribute.Key
		n       int64
	}{
		{SubjectRows, "toft.sweep.removed.rows", r.Rows},
		{SubjectLogLines, "toft.sweep.removed.log_lines", r.LogLines},
		{SubjectObjects, "toft.sweep.removed.objects", r.Objects},
		{SubjectReleases, "toft.sweep.removed.releases", r.Releases},
	} {
		if s.n <= 0 {
			continue
		}
		span.Set(s.key.Int64(s.n))
		if metricsOn {
			p.inst.sweepRemoved.Add(ctx, s.n, metric.WithAttributes(
				attribute.String("sweep", sweep),
				attribute.String("subject", s.subject),
			))
		}
	}
}

// Span attribute keys a sweep sets beside its counts. Constants because they
// are on the allow-list in redact.go and a typo would be a silently dropped
// attribute rather than a compile error.
const (
	// AttrSweepCutoff is the horizon the pass removed against — derived from a
	// retention setting, and a fact about the policy rather than about any row
	// that was on the wrong side of it.
	AttrSweepCutoff = attribute.Key("toft.sweep.cutoff")
	// AttrSweepTruncated marks a pass that stopped at its batch limit. A span
	// attribute rather than a seventh `toft.outcome` value: the enum is
	// six today and ten instruments declare it, so a seventh value re-prices
	// all of them permanently to describe one sweep's edge case. The rate of
	// the removal counter is the better alert anyway.
	AttrSweepTruncated = attribute.Key("toft.sweep.truncated")
)

// RecordAutodeployPoll records one pass of the auto-deploy poller.
func RecordAutodeployPoll(ctx context.Context, outcome string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.autodeployPoll.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("toft.outcome", outcome),
	))
}

// RecordAutodeploySkip counts an auto-deploy that did not happen, and why —
// including the source-policy skip of ADR §29, which is the one an
// administrator who has just narrowed the allowlist needs to see.
func RecordAutodeploySkip(ctx context.Context, reason string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.autodeploySkipped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("toft.skip_reason", reason),
	))
}

// Git webhook delivery outcomes. A closed set, because it is a metric
// attribute and cardinality is budgeted.
const (
	// GitHookAccepted: verified, named a repository, and marked at least one app.
	GitHookAccepted = "accepted"
	// GitHookIgnored: verified and there was nothing to do — a repository no app
	// deploys from, or a payload that named none.
	GitHookIgnored = "ignored"
	// GitHookUnverified: the signature did not check out, or the connection's
	// secret could not be read. Counted and never recorded per delivery.
	GitHookUnverified = "unverified"
)

// RecordGitHookDelivery counts one webhook delivery by what became of it.
func RecordGitHookDelivery(ctx context.Context, outcome string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.gitHookDeliveries.Add(ctx, 1, metric.WithAttributes(
		attribute.String("toft.outcome", outcome),
	))
}

// RecordGallerySync records one catalog fetch.
func RecordGallerySync(ctx context.Context, outcome string, dur time.Duration) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.gallerySync.Record(ctx, seconds(dur), metric.WithAttributes(
		attribute.String("toft.outcome", outcome),
	))
}

// RecordTelemetryDropped counts something this pipeline threw away.
func RecordTelemetryDropped(signal, reason string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.telemetryDropped.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("signal", signal),
		attribute.String("reason", reason),
	))
}

// RecordTelemetryExportFailure counts an export that failed.
func RecordTelemetryExportFailure(signal, errType string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.telemetryFailed.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("signal", signal),
		attribute.String("error.type", errType),
	))
}

// firstErrorType prefers a transport error over an HTTP status: a call that
// never got a response has no status worth reporting, and a 502 that followed a
// dial failure is the gateway's own synthesis rather than the upstream's answer.
func firstErrorType(err error, status int) string {
	if et := ErrorType(err); et != ErrNone {
		return et
	}
	return StatusErrorType(status)
}

// RecordDependencyScan counts one build that resolved its dependencies.
//
// The ecosystem is the bounded set this binary knows, and "mixed" for an app
// with two manifests. **No package name and no advisory id ever reaches a
// metric attribute** — both are unbounded by construction, and one careless
// label would spend the install's whole cardinality budget on its own.
func RecordDependencyScan(ctx context.Context, ecosystem, outcome string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.dependencyScans.Add(ctx, 1, metric.WithAttributes(
		attribute.String("toft.dependency.ecosystem", ecosystem),
		attribute.String("toft.outcome", outcome),
	))
}

// RecordDependencyRefusal counts one deploy this policy turned away.
//
// `severity` is the worst blocking finding's — one of the four levels, unknown,
// or "malware", which is a class rather than a level and is why it is a value
// here rather than a second attribute.
func RecordDependencyRefusal(ctx context.Context, severity, mode string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.dependencyRefusals.Add(ctx, 1, metric.WithAttributes(
		attribute.String("toft.dependency.severity", severity),
		attribute.String("toft.dependency.mode", mode),
	))
}

// RecordDependencyWaiver counts a risk accepted or withdrawn. The advisory it
// was about is on the audit row, not here.
func RecordDependencyWaiver(ctx context.Context, action string) {
	p := live.Load()
	if p == nil || !p.metrics {
		return
	}
	p.inst.dependencyWaivers.Add(ctx, 1, metric.WithAttributes(
		attribute.String("toft.dependency.action", action),
	))
}
