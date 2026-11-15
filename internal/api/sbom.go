package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/deps"
)

// The software bill of materials of one release: CycloneDX 1.6 JSON, rendered
// from the inventory the build recorded, on request, and filed nowhere.
//
// **CycloneDX over SPDX** because `compositions` can say an aggregate is
// incomplete or unknown, and most of what makes this honest is saying exactly
// that: a release whose Python needed a source build, a pnpm lock this
// platform does not read, an R app with no renv.lock. SPDX 2.3 can say
// NOASSERTION about a field and nothing about a missing ecosystem.
//
// **What it claims is decided by sbomComposition alone, from what was
// recorded** — the verdict's `unread` gaps and the precedence the Dependencies
// tab already uses — never from `scanned`, which is a fact about the advisory
// feed's coverage and not about whether the inventory is complete.
//
// **Same release, same bytes.** The serial is a UUIDv5 of the deployment, the
// timestamp the build's finish, components sorted, and only facts the release
// cannot change are rendered: no app name or slug, both of which are
// writable. The generator is named by sbomFormat, not by the binary's
// version, so an upgrade changes an old release's bytes only when the format
// does. Findings are not in it: they are derived from a feed, and an SBOM is
// a fact about a release.

// sbomFormat is this renderer's format, bumped when the bytes it produces for
// the same record would change.
const sbomFormat = "1"

// sbomNamespace roots the serial numbers.
var sbomNamespace = uuid.MustParse("6f1c0a7e-1b8e-4e7a-9e57-7a0f7d1b6a01")

type cdxBOM struct {
	BOMFormat    string           `json:"bomFormat"`
	SpecVersion  string           `json:"specVersion"`
	SerialNumber string           `json:"serialNumber"`
	Version      int              `json:"version"`
	Metadata     cdxMetadata      `json:"metadata"`
	Components   []cdxComponent   `json:"components"`
	Dependencies []cdxDependency  `json:"dependencies"`
	Compositions []cdxComposition `json:"compositions"`
}

type cdxMetadata struct {
	Timestamp  string        `json:"timestamp,omitempty"`
	Tools      cdxTools      `json:"tools"`
	Component  cdxComponent  `json:"component"`
	Properties []cdxProperty `json:"properties"`
}

type cdxTools struct {
	Components []cdxComponent `json:"components"`
}

type cdxComponent struct {
	Type       string        `json:"type"`
	BOMRef     string        `json:"bom-ref,omitempty"`
	Name       string        `json:"name"`
	Version    string        `json:"version,omitempty"`
	Purl       string        `json:"purl,omitempty"`
	Scope      string        `json:"scope,omitempty"`
	Properties []cdxProperty `json:"properties,omitempty"`
}

type cdxProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cdxDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

type cdxComposition struct {
	BOMRef       string   `json:"bom-ref,omitempty"`
	Aggregate    string   `json:"aggregate"`
	Assemblies   []string `json:"assemblies,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// sbomRecord is what the release recorded, for the renderer.
type sbomRecord struct {
	AppID      string
	Deployment *database.Deployment
	Inventory  []deps.Dependency
	Verdict    deps.Verdict
	HasVerdict bool
	// Static is a release that serves files and installs nothing.
	Static bool
	// NotEnumerable is an R family whose release recorded no CRAN packages:
	// a repository with no renv.lock, which has dependencies nothing can name.
	NotEnumerable bool
}

// ecosystemClaim is one ecosystem's composition and why.
type ecosystemClaim struct {
	Ecosystem, Aggregate, Reason string
}

const rootRef = "release"

// sbomComposition is the one function deciding what the SBOM claims.
func sbomComposition(rec sbomRecord) (claims []ecosystemClaim, overall string, overallReason string) {
	if rec.Static {
		return nil, "complete", "serves files, installs nothing"
	}
	if !rec.HasVerdict {
		return nil, "unknown", "dependencies were not resolved for this release"
	}
	present := map[string]bool{}
	for _, d := range rec.Inventory {
		present[d.Ecosystem] = true
	}
	for _, g := range rec.Verdict.Unread {
		if g.Ecosystem != "" {
			present[g.Ecosystem] = true
		}
	}
	if rec.NotEnumerable && !present[advisory.EcosystemCRAN] {
		present[advisory.EcosystemCRAN] = true
	}
	ecos := make([]string, 0, len(present))
	for e := range present {
		ecos = append(ecos, e)
	}
	sort.Strings(ecos)

	if rec.Verdict.Unread == nil {
		for _, e := range ecos {
			claims = append(claims, ecosystemClaim{e, "unknown", "built before this install recorded what resolution missed"})
		}
		return claims, "unknown", "built before this install recorded what resolution missed"
	}
	worst := "complete"
	for _, e := range ecos {
		c := ecosystemClaim{Ecosystem: e, Aggregate: "complete", Reason: "the lockfile was read"}
		if e == advisory.EcosystemPyPI && rec.Verdict.HashedRequirements {
			// Python's lock is the hashed file the build installed with
			// --require-hashes; a reader should not go looking for a uv.lock.
			c.Reason = "requirements.txt pins and hashes every package"
		}
		if e == advisory.EcosystemCRAN && rec.NotEnumerable && !hasEcosystem(rec.Inventory, e) {
			c.Aggregate, c.Reason = "unknown", "no renv.lock, so its packages cannot be named"
		}
		for _, g := range rec.Verdict.Unread {
			if g.Ecosystem != e && !(g.Kind == deps.GapVendored && vendoredEcosystem(g.File) == e) {
				continue
			}
			switch g.Kind {
			case deps.GapUnresolved:
				c.Aggregate, c.Reason = "unknown", "could not be resolved"
				if g.Reason != "" {
					c.Reason += ": " + g.Reason
				}
			case deps.GapForeignLock:
				c.Aggregate, c.Reason = "unknown", g.File+" was not read; versions were resolved from package.json and may differ from those installed"
			case deps.GapUnlocked:
				if c.Aggregate != "unknown" {
					c.Aggregate, c.Reason = "unknown", "no lockfile; versions were resolved at build time and may differ from those installed"
				}
			case deps.GapDirectOnly, deps.GapVendored:
				if c.Aggregate == "complete" {
					c.Aggregate = "incomplete"
					if g.Kind == deps.GapVendored {
						c.Reason = "a committed " + g.File + " directory was not read"
					} else {
						c.Reason = "only the packages " + g.File + " names; the packages they depend on were installed but not listed"
					}
				}
			}
		}
		claims = append(claims, c)
		worst = worse(worst, c.Aggregate)
	}
	return claims, worst, ""
}

func hasEcosystem(inv []deps.Dependency, eco string) bool {
	for _, d := range inv {
		if d.Ecosystem == eco {
			return true
		}
	}
	return false
}

// vendoredEcosystem is the ecosystem a committed tree belongs to.
func vendoredEcosystem(dir string) string {
	switch dir {
	case "node_modules":
		return advisory.EcosystemNPM
	case "vendor":
		return advisory.EcosystemGo
	}
	return ""
}

func worse(a, b string) string {
	rank := map[string]int{"complete": 0, "incomplete": 1, "unknown": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// purlOf is a package's purl, or "" for a version that does not parse as one
// — a git URL, a file: reference, a local tag — which is kept, never dropped.
func purlOf(d deps.Dependency) string {
	if d.Version == "" || strings.ContainsAny(d.Version, ":/ ") {
		return ""
	}
	typ := map[string]string{
		advisory.EcosystemNPM: "npm", advisory.EcosystemPyPI: "pypi",
		advisory.EcosystemGo: "golang", advisory.EcosystemCRAN: "cran",
	}[d.Ecosystem]
	if typ == "" {
		return ""
	}
	name := d.Package
	if typ == "pypi" {
		name = strings.ToLower(name)
	}
	var ns string
	if typ == "npm" && strings.HasPrefix(name, "@") {
		if i := strings.IndexByte(name, '/'); i > 0 {
			ns, name = "%40"+url.PathEscape(name[1:i])+"/", name[i+1:]
		}
	}
	if typ == "golang" {
		if i := strings.LastIndexByte(name, '/'); i > 0 {
			ns, name = name[:i]+"/", name[i+1:]
		}
	}
	return "pkg:" + typ + "/" + ns + url.PathEscape(name) + "@" + url.PathEscape(d.Version)
}

// renderSBOM is the release's SBOM.
func renderSBOM(rec sbomRecord) cdxBOM {
	d := rec.Deployment
	claims, overall, overallReason := sbomComposition(rec)

	bom := cdxBOM{
		BOMFormat: "CycloneDX", SpecVersion: "1.6", Version: 1,
		SerialNumber: "urn:uuid:" + uuid.NewSHA1(sbomNamespace, []byte(d.ID)).String(),
		Components:   []cdxComponent{},
		Dependencies: []cdxDependency{},
		Compositions: []cdxComposition{},
	}
	if d.FinishedAt != nil {
		bom.Metadata.Timestamp = d.FinishedAt.UTC().Format(time.RFC3339)
	}
	bom.Metadata.Tools.Components = []cdxComponent{{Type: "application", Name: "toft"}}
	root := cdxComponent{Type: "application", BOMRef: rootRef, Name: rec.AppID, Version: d.ID}
	bom.Metadata.Component = root
	props := []cdxProperty{
		{"toft:sbom_format", sbomFormat},
		{"toft:app_id", rec.AppID},
		{"toft:deployment_id", d.ID},
	}
	if d.CommitSHA != "" {
		props = append(props, cdxProperty{"toft:commit", d.CommitSHA})
	}
	if d.Branch != "" {
		props = append(props, cdxProperty{"toft:branch", d.Branch})
	}
	props = append(props,
		cdxProperty{"toft:not_included", "no hashes or licenses are recorded; the operating system and runner image beneath the interpreter are outside this document"})
	if overallReason != "" {
		props = append(props, cdxProperty{"toft:composition", overall + ": " + overallReason})
	}
	for _, c := range claims {
		props = append(props, cdxProperty{"toft:ecosystem:" + c.Ecosystem, c.Aggregate + ": " + c.Reason})
	}
	bom.Metadata.Properties = props

	inv := append([]deps.Dependency(nil), rec.Inventory...)
	sort.SliceStable(inv, func(i, j int) bool {
		if inv[i].Ecosystem != inv[j].Ecosystem {
			return inv[i].Ecosystem < inv[j].Ecosystem
		}
		if inv[i].Package != inv[j].Package {
			return inv[i].Package < inv[j].Package
		}
		return inv[i].Version < inv[j].Version
	})
	byEco := map[string][]string{}
	var direct []string
	seen := map[string]bool{}
	for _, dep := range inv {
		ref := dep.Ecosystem + ":" + dep.Package + "@" + dep.Version
		if seen[ref] {
			continue
		}
		seen[ref] = true
		c := cdxComponent{Type: "library", BOMRef: ref, Name: dep.Package, Version: dep.Version, Purl: purlOf(dep)}
		if dep.Dev {
			c.Scope = "optional"
		}
		// Where the row came from, so a consumer can tell the platform's own
		// packages — renv, the SDK — from the app's. Additive: a consumer
		// that ignores properties reads the same document, so sbom_format
		// stays where it is.
		c.Properties = []cdxProperty{{"toft:source", deps.SourceOf(dep)}}
		if dep.Path != "" {
			c.Properties = append(c.Properties, cdxProperty{"toft:path", dep.Path})
		}
		bom.Components = append(bom.Components, c)
		byEco[dep.Ecosystem] = append(byEco[dep.Ecosystem], ref)
		if dep.Direct {
			direct = append(direct, ref)
		}
	}
	if d.RuntimeLanguage != "" {
		ref := "runtime:" + d.RuntimeLanguage
		bom.Components = append(bom.Components, cdxComponent{Type: "platform", BOMRef: ref,
			Name: d.RuntimeLanguage, Version: d.RuntimeVersion})
		direct = append(direct, ref)
	}
	if direct == nil {
		direct = []string{}
	}
	bom.Dependencies = append(bom.Dependencies, cdxDependency{Ref: rootRef, DependsOn: direct})

	if len(claims) == 0 {
		bom.Compositions = append(bom.Compositions, cdxComposition{Aggregate: overall, Assemblies: []string{rootRef}})
	}
	for _, c := range claims {
		bom.Compositions = append(bom.Compositions, cdxComposition{
			BOMRef: "composition:" + c.Ecosystem, Aggregate: c.Aggregate, Assemblies: byEco[c.Ecosystem],
		})
	}
	// One representative path per package is not a graph.
	bom.Compositions = append(bom.Compositions, cdxComposition{
		BOMRef: "composition:dependencies", Aggregate: "incomplete", Dependencies: []string{rootRef},
	})
	return bom
}

// handleDeploymentSBOM — GET /apps/{appID}/deployments/{deployID}/sbom.
//
// Gated as /dependencies: whoever may read the inventory may read its SBOM,
// and the promotion pipeline that reads the verdict is a consumer, which is
// why it is on both deploy-key lists too. Not gated on today's policy or the
// license: the inventory is the record, and a lapsed license does not unsay
// what a release installed.
func (s *Server) handleDeploymentSBOM(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.appForEdit(w, r)
	if !ok {
		return
	}
	d, err := s.db.GetDeployment(chi.URLParam(r, "deployID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Scoped by the app in the URL, as every id taken from a URL is.
	if d == nil || d.AppID != app.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found", "hint": "toft deployments lists this app's deployments with their ids"})
		return
	}
	inv, err := s.db.ListDeploymentDependencies(d.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	v, has, err := s.db.GetDeploymentVerdict(d.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	rec := sbomRecord{
		AppID: app.ID, Deployment: d, Inventory: inv, Verdict: v, HasVerdict: has,
		Static:        noDependenciesReason(app) == "no_dependencies",
		NotEnumerable: emptyInventoryReason(app) == "not_enumerable",
	}
	body, err := json.MarshalIndent(renderSBOM(rec), "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.Header().Set("Content-Type", "application/vnd.cyclonedx+json; version=1.6")
	w.Header().Set("Content-Disposition", `attachment; filename="`+app.Slug+"-"+shortID(strings.ReplaceAll(d.ID, "-", ""))+`.cdx.json"`)
	_, _ = w.Write(body)
}
