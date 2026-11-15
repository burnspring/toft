package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
	"toft/internal/toolchain"
)

// The estate census — what this install's apps are written in, as a number an
// administrator can act on.
//
// The database counts; this file is where counting becomes vocabulary. Three
// derivations live here and nowhere else: an app with no recorded language gets
// one from its framework (`builder.LanguageOf`, the detector's own mapping and
// never a second copy of it), a recorded version becomes the series it belongs
// to (`toolchain`, because patch never matters and a census split across
// 3.13.2 and 3.13.4 fragments the count an end-of-life decision needs), and an
// empty version becomes one of two words rather than a blank.

// Version states. Absent is a state with a name and never reads as zero.
const (
	// estateResolved: the serving release recorded a version.
	estateResolved = "resolved"
	// estateUnresolved: the app has a language and no recorded version. Two
	// causes — Go left to the go command, and a release predating migration
	// 00009 — and they are indistinguishable in the data, so pretending
	// otherwise on screen would be a lie.
	estateUnresolved = "unresolved"
	// estateNotApplicable: a static app resolves no interpreter because it runs
	// none. Counted in the estate, excluded from every version total.
	estateNotApplicable = "not_applicable"
)

type estateFramework struct {
	Framework string `json:"framework"`
	Apps      int    `json:"apps"`
}

type estateSeries struct {
	// Series is "3.13", "22", "1.25" — never a patch. Empty when State is not
	// `resolved`, in which case the word is the answer.
	Series string `json:"series"`
	State  string `json:"state"`
	Apps   int    `json:"apps"`
	// Shared and Isolated are the second axis: what is still shared, and what
	// is it written in. One more GROUP BY term rather than a second page,
	// because the interesting question needs both halves on one screen.
	Shared     int               `json:"shared"`
	Isolated   int               `json:"isolated"`
	Frameworks []estateFramework `json:"frameworks"`
}

type estateLanguage struct {
	// Language is the identifier — "python", "node", "go", "static" — or empty
	// for an app whose framework the detector never named.
	Language string         `json:"language"`
	Label    string         `json:"label"`
	Apps     int            `json:"apps"`
	Shared   int            `json:"shared"`
	Isolated int            `json:"isolated"`
	Series   []estateSeries `json:"series"`
}

// estateResponse is one census.
//
// **The coverage rides on it and the page leads with the words.** The argument
// is packageSearchResponse's and is stronger here: a search returns rows a
// reader knows are a subset, where a census returns a total, and a total feels
// exhaustive. "84 apps, 61 with a recorded runtime version" is the sentence
// that stops the other number being read as an estate-wide all-clear.
type estateResponse struct {
	Languages []estateLanguage `json:"languages"`

	AppsTotal     int `json:"apps_total"`
	AppsVersioned int `json:"apps_versioned"`
	// AppsLocked counts retired and held-down apps, which are in every total
	// above. Said once, here, rather than as a parenthesis on every row.
	AppsLocked int `json:"apps_locked"`

	// Kinds counts the estate by what answers a request (builder.AppKind);
	// the three sum to AppsTotal. AppsWithJobs overlaps them.
	Kinds        estateKinds `json:"kinds"`
	AppsWithJobs int         `json:"apps_with_jobs"`

	// ShowModes gates the two mode columns. The install's capability, not a
	// count of what happens to be present — a census carrying "Isolated 0" is
	// answering the question, where a per-row badge repeating "Shared" is noise,
	// which is why this does not inherit the apps list's `anyIsolated` rule.
	//
	// Or-ed with what is actually running, because an administrator who turned
	// isolation off after using it must not be shown a census that quietly
	// stops mentioning the workloads still out there.
	ShowModes bool `json:"show_modes"`
}

// handleEstateLanguages answers "what is this estate written in, and what is
// still shared".
//
// Administrator only, 404 otherwise — the same gate and the same status as
// /admin/packages, for a narrower version of the same reason. An estate-wide
// map of which internal tool runs which interpreter version is a target map:
// it names, per app, a set of published advisories. An app's own editor can
// read their app's version on its Dependencies tab; what they cannot have is
// everybody else's.
type estateKinds struct {
	Serving  int `json:"serving"`
	Rendered int `json:"rendered"`
	Batch    int `json:"batch"`
}

func (s *Server) handleEstateLanguages(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	census, err := s.db.EstateByLanguage(scopeFor(p))
	if err != nil {
		// Never a page with rows and no coverage line: the two come from one
		// query, so a failure takes both or neither.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := foldEstate(census)
	resp.ShowModes = s.capabilities().IsolatedAvailable() || resp.anyIsolated()
	writeJSON(w, http.StatusOK, resp)
}

func (e estateResponse) anyIsolated() bool {
	for _, l := range e.Languages {
		if l.Isolated > 0 {
			return true
		}
	}
	return false
}

// foldEstate turns recorded columns into the census a person reads.
//
// Pure, and separate from the handler, so every derivation in it can be
// exercised without a database or an HTTP request.
func foldEstate(census database.EstateCensus) estateResponse {
	resp := estateResponse{
		Languages:     []estateLanguage{},
		AppsTotal:     census.AppsTotal,
		AppsVersioned: census.AppsVersioned,
		AppsLocked:    census.AppsLocked,
		AppsWithJobs:  census.AppsWithJobs,
	}
	for _, g := range census.Groups {
		switch builder.AppKind(g.Framework) {
		case builder.AppKindBatch:
			resp.Kinds.Batch += g.Count
		case builder.AppKindRendered:
			resp.Kinds.Rendered += g.Count
		default:
			resp.Kinds.Serving += g.Count
		}
	}

	// language -> state|series -> framework -> count, built in one pass.
	type seriesKey struct{ state, series string }
	langs := map[string]map[seriesKey]map[string][2]int{}
	order := []string{}

	for _, g := range census.Groups {
		lang := estateLanguageOf(g)
		key := seriesKey{state: estateNotApplicable}
		if lang != frameworkStatic {
			key.state = estateUnresolved
		}
		if g.Version != "" {
			key = seriesKey{state: estateResolved, series: estateSeriesOf(lang, g.Version)}
		}

		byState, ok := langs[lang]
		if !ok {
			byState = map[seriesKey]map[string][2]int{}
			langs[lang] = byState
			order = append(order, lang)
		}
		byFramework, ok := byState[key]
		if !ok {
			byFramework = map[string][2]int{}
			byState[key] = byFramework
		}
		counts := byFramework[g.Framework]
		if isEstateIsolated(g.RunningMode) {
			counts[1] += g.Count
		} else {
			counts[0] += g.Count
		}
		byFramework[g.Framework] = counts
	}

	sort.Slice(order, func(i, j int) bool {
		return estateLanguageRank(order[i]) < estateLanguageRank(order[j])
	})

	for _, lang := range order {
		row := estateLanguage{Language: lang, Label: estateLanguageLabel(lang), Series: []estateSeries{}}
		for key, byFramework := range langs[lang] {
			sr := estateSeries{Series: key.series, State: key.state, Frameworks: []estateFramework{}}
			for fw, counts := range byFramework {
				sr.Shared += counts[0]
				sr.Isolated += counts[1]
				sr.Frameworks = append(sr.Frameworks, estateFramework{Framework: fw, Apps: counts[0] + counts[1]})
			}
			sr.Apps = sr.Shared + sr.Isolated
			sort.Slice(sr.Frameworks, func(i, j int) bool {
				if sr.Frameworks[i].Apps != sr.Frameworks[j].Apps {
					return sr.Frameworks[i].Apps > sr.Frameworks[j].Apps
				}
				return sr.Frameworks[i].Framework < sr.Frameworks[j].Framework
			})
			row.Series = append(row.Series, sr)
			row.Apps += sr.Apps
			row.Shared += sr.Shared
			row.Isolated += sr.Isolated
		}
		sort.Slice(row.Series, func(i, j int) bool { return estateSeriesBefore(row.Series[i], row.Series[j]) })
		resp.Languages = append(resp.Languages, row)
	}
	return resp
}

// frameworkStatic is the detector's identifier for a folder of HTML. Named here
// because this file has to ask "is this the one framework with no interpreter"
// and a bare string in three places is how the fourth one ends up misspelt.
const frameworkStatic = string(builder.FrameworkStatic)

// estateLanguageOf is what an app is written in.
//
// The recorded language first, because it is the one the build actually
// resolved against. Failing that the framework's, through the detector's own
// mapping — a static app is its own answer, and a framework nobody recognises
// has none, which the census says rather than guesses.
func estateLanguageOf(g database.EstateGroup) string {
	if g.Language != "" {
		return g.Language
	}
	if g.Framework == frameworkStatic {
		return frameworkStatic
	}
	return builder.LanguageOf(builder.Framework(g.Framework))
}

func estateLanguageLabel(lang string) string {
	switch lang {
	case "":
		return "Unknown"
	case frameworkStatic:
		return "Static"
	}
	return toolchain.Label(lang)
}

// estateLanguageRank orders the languages. Four of them, in the order a person
// reads them, with the two non-interpreters last.
//
// This is not the version set, which is discovered — see estateSeriesOf's
// callers. No version string appears here or anywhere else in this file.
func estateLanguageRank(lang string) int {
	for i, l := range toolchain.Languages {
		if l == lang {
			return i
		}
	}
	if lang == frameworkStatic {
		return len(toolchain.Languages)
	}
	return len(toolchain.Languages) + 1
}

// estateSeriesOf is the release line a recorded version belongs to: 3.13.4
// counts as 3.13, and 22.11.0 as 22.
//
// Patch never matters, and a census that split one series into three rows would
// put a distinction on screen the product refuses to let anybody act on.
func estateSeriesOf(language, version string) string {
	return toolchain.Version{Language: language, Version: version}.Series()
}

func isEstateIsolated(mode string) bool {
	m, err := runtime.ParseMode(mode)
	if err != nil {
		// The reading the rest of the platform does: an empty running_mode is a
		// row written before modes were per-deployment, and every one of those
		// is a process.
		return false
	}
	return m == runtime.ModeIsolated
}

// estateSeriesBefore orders series newest first, with the two wordless states
// at the bottom — an unresolved row is not a version and must not sort among
// them.
func estateSeriesBefore(a, b estateSeries) bool {
	if (a.State == estateResolved) != (b.State == estateResolved) {
		return a.State == estateResolved
	}
	if a.State != estateResolved {
		return a.State < b.State
	}
	av, bv := estateSeriesParts(a.Series), estateSeriesParts(b.Series)
	for i := 0; i < len(av) && i < len(bv); i++ {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	if len(av) != len(bv) {
		return len(av) > len(bv)
	}
	return a.Series < b.Series
}

func estateSeriesParts(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// appRuntime is one app's release line, as it appears on the apps list.
//
// The same four facts the census carries, folded by the same functions, so the
// facet on the dashboard and the rows on /admin/estate cannot disagree about
// which series an app is on or what to call it.
type appRuntime struct {
	Language string `json:"language"`
	Label    string `json:"label"`
	Series   string `json:"series"`
	State    string `json:"state"`
	// Version is the recorded string in full — `3.13.7`, not `3.13` — and is
	// empty whenever Series is.
	//
	// The two are not a duplicate. A count has to fold patch away or the census
	// fragments across 3.13.2 and 3.13.4, which is what Series is for; an app's
	// own page is answering a different question — *which* interpreter is
	// running my code — and rounding the answer there would hide the difference
	// between two of its own releases. So both travel, and each surface renders
	// the one its question needs.
	Version string `json:"version,omitempty"`
}

// appRuntimeOf folds one app's framework and recorded runtime into the words a
// filter chip needs.
func appRuntimeOf(framework string, r database.AppRuntime) *appRuntime {
	g := database.EstateGroup{Language: r.Language, Version: r.Version, Framework: framework}
	lang := estateLanguageOf(g)
	out := &appRuntime{Language: lang, Label: estateLanguageLabel(lang), State: estateUnresolved}
	if lang == frameworkStatic {
		out.State = estateNotApplicable
	}
	if r.Version != "" {
		out.State, out.Series = estateResolved, estateSeriesOf(lang, r.Version)
		out.Version = r.Version
	}
	return out
}

// ---------------------------------------------------------------- the targets

// The estate's scan targets — every app as an address, for the team whose job
// is to point a scanner at this install.
//
// A third route beside the census and /admin/packages rather than a flag on
// either, for the reason /admin/packages is a third route beside
// /admin/dependencies: a row here is *an app to visit*, a row there is an app
// with findings, and a row on the census is a language and a count. A reader
// that answered all three would answer none of them well.
//
// It is not a scanner and it schedules nothing. What the platform owes an
// AppSec team is what to point at, how to be counted (X-Toft-Scan) and
// how to be contacted (/.well-known/security.txt); running the scan, storing
// findings and rendering a verdict are somebody else's product.

// estateTarget is one app as a scanner needs it: the database's row, plus the
// two things only this layer can answer.
type estateTarget struct {
	database.EstateTarget
	// URL is the address a visitor would be sent to, or empty for an app that
	// has never been deployed. Built by builder.PublicURL from the origin the
	// install actually serves apps on — never composed here, because the
	// answer differs between single- and split-origin installs and a scanner
	// pointed at a URL this handler guessed would scan a 404 and report it
	// clean.
	URL string `json:"url,omitempty"`
	// CallerCanReach is whether the caller who asked could open this app at
	// the gateway, answered by the authorizer the gateway uses. A deploy key
	// reaches no app there, so it is false on every row for one. An app the
	// scanner cannot reach is a gap it can count, never a row it cannot see.
	CallerCanReach bool `json:"caller_can_reach"`
	// VisibilityWithheld says the word above is not the app's own: it is
	// `public` in the database and `authenticated` in effect, because this
	// install's anonymous-access switch is off. Carried rather than left implicit so the difference between "the
	// owner never published it" and "the install withdrew it" survives into
	// whatever reads this.
	VisibilityWithheld bool `json:"visibility_withheld,omitempty"`
}

// estateTargetsResponse is the list and its coverage, together for the reason
// the census's are: a list of addresses feels exhaustive.
type estateTargetsResponse struct {
	Targets []estateTarget `json:"targets"`

	AppsTotal int `json:"apps_total"`
	// AppsAddressable is how many carry a URL. **Nothing is ever omitted** —
	// an app with no successful deployment is a row with a status and no
	// address — so this is the coverage figure rather than a skipped count:
	// what is missing from this answer is an address, never an app.
	AppsAddressable int `json:"apps_addressable"`
	AppsLocked      int `json:"apps_locked"`
	// AppsReachableByCaller counts the rows with caller_can_reach — the
	// coverage figure for whoever is scanning, beside the estate's own.
	AppsReachableByCaller int `json:"apps_reachable_by_caller"`
	// AppsOrigin is where deployed apps are served from, said once rather than
	// inferred from the rows — an install with no apps deployed yet would
	// otherwise answer a list of nothing with no way to tell what it would
	// have looked like.
	AppsOrigin string `json:"apps_origin"`
}

// canReadEstateTargets is the gate, and it is deliberately wider than
// p.IsAdmin() by exactly one principal.
//
// **A deploy key may read this, and it is the only class that may.** The
// argument is that it gains nothing it did not have: `toftd_` already acts as
// owner on every app (auth.Authorize step 1a), `scopeFor` already resolves it
// to every app on the install, and `GET /apps` is already on both of its
// scopes' lists — so a deploy key can already enumerate every app's id, slug,
// name, owner, framework, status and visibility. What this route adds for it
// is the URL column, which anything holding /api/v1/config and a slug could
// already compose.
//
// The reason it is worth the widening at all is that an estate read is a
// pipeline's request and a pipeline holds an `toftd_`: a scan job that had to be
// given an administrator's personal key in order to find out what to scan is a
// scan job holding a credential that can do far more than scan.
//
// **And the scanner account** — an account an administrator marked — on its
// session or its personal key. It is what an AppSec tool signs in as, and a
// scanner that has to be an administrator to learn what to scan is a scanner
// holding a credential that can do far more than scan. The read discloses what
// a deploy key's already does; reach is not part of it (caller_can_reach).
//
// Every other signed-in non-admin gets the same 404 every other admin read
// here answers with.
func (s *Server) canReadEstateTargets(p *auth.Principal) bool {
	if p == nil {
		return false
	}
	if p.IsAdmin() || p.IsDeployKey() {
		return true
	}
	return p.IsUser() && s.isScannerAccount(p.ID)
}

// handleEstateTargets answers "what is there to scan, and where is it".
func (s *Server) handleEstateTargets(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if !s.canReadEstateTargets(p) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	// The whole estate for every reader admitted. A scanner account that is
	// not an administrator still gets every row, because the reader never
	// omits an app (ADR §77): an app it cannot reach is a row that says so in
	// caller_can_reach, not a row that is missing.
	set, err := s.db.EstateTargets(database.AppScope{All: true})
	if err != nil {
		// Rows and coverage come from one query, so a failure takes both or
		// neither — never a list of addresses with no count over it.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := foldEstateTargets(set, s.cfg.AppsBaseURL)
	s.markReach(p, &resp)
	writeJSON(w, http.StatusOK, resp)
}

// foldEstateTargets turns the recorded rows into the list a scanner reads.
//
// Pure and separate from the handler, so the URL rule and the visibility
// substitution can both be exercised without a database or an HTTP request.
// markReach answers caller_can_reach on every row, for the caller who asked.
func (s *Server) markReach(p *auth.Principal, resp *estateTargetsResponse) {
	if p == nil || p.IsDeployKey() {
		return
	}
	for i := range resp.Targets {
		row := &resp.Targets[i]
		obj := database.AuthObjectOf(row.AppID, row.OwnerID, row.Visibility, false, false)
		if auth.CanRead(p, obj, s.db) {
			row.CallerCanReach = true
			resp.AppsReachableByCaller++
		}
	}
}

func foldEstateTargets(set database.EstateTargetSet, appsBaseURL string) estateTargetsResponse {
	resp := estateTargetsResponse{
		Targets:    make([]estateTarget, 0, len(set.Targets)),
		AppsTotal:  set.AppsTotal,
		AppsLocked: set.AppsLocked,
		AppsOrigin: strings.TrimSuffix(appsBaseURL, "/"),
	}
	for _, row := range set.Targets {
		t := estateTarget{EstateTarget: row}
		// An app that has never deployed has no address, and neither has a
		// batch app (ADR §107), which runs jobs and serves no pages. The row
		// stays — a scanner needs to know the slug exists and is not
		// scannable, and omitting it would be the reader answering less than
		// it knows.
		if row.LastDeployedAt != nil && !builder.IsBatchFramework(row.Framework) {
			t.URL = builder.PublicURL(appsBaseURL, &database.App{Slug: row.Slug, Hostname: row.Hostname, ReleaseBasePath: row.ReleaseBasePath})
			resp.AppsAddressable++
		}
		// One substitution, through the one function that owns it. Nothing
		// here re-derives "public means public": AuthObjectOf is what every
		// authorizer already reads, and a report that disagreed with it would
		// send somebody to test access the platform refuses.
		t.Visibility = database.AuthObjectOf(row.AppID, "", row.Visibility, row.AnonymityWithheld, false).Visibility
		t.VisibilityWithheld = t.Visibility != row.Visibility
		resp.Targets = append(resp.Targets, t)
	}
	return resp
}
