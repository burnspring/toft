// Package compliance holds what this install can tell an assessor.
//
// Two documents come out of one registry, and that is the point of the package
// rather than an implementation detail. The **control document** is a property
// of the build: what Toft does about a requirement is the same sentence on
// every install of a version, so it reads no database and needs no
// configuration. The **audit report** is a property of the install: the same
// requirements, with what this machine is actually set to, read now.
//
// They render from one registry so they cannot disagree about what the controls
// are. A narrative that named a control the evidence did not collect — or an
// evidence item substantiating a requirement the narrative never made a claim
// about — is the failure mode this package is shaped to prevent, and the
// completeness test is where that shape is enforced.
//
// Nothing here is a verdict. There is no score, no percentage and no overall
// state: 19 of the 97 requirements have nothing to do with Toft, so any
// average mixes a platform control with personnel screening and produces a
// figure that exists only to be quoted by somebody who will not read the rest.
// The registry reports what it holds; the assessor decides.
package compliance

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// The three registries, compiled in.
//
// The catalog is **generated** by tools/extract_catalog.py from NIST's OSCAL
// release and never hand-edited, on the same rule the schema snapshot holds:
// requirement ids, titles and statement text are somebody else's document, and
// a transcription is a copy that drifts. The other two are this project's own
// assertions and are authored here.
//
//go:embed data/nist-800-171r3.json
var catalogJSON []byte

//go:embed data/mapping.yaml
var mappingYAML []byte

//go:embed data/evidence.yaml
var evidenceYAML []byte

// The four values of Responsibility, in the order a reader should meet them:
// what the platform does, what it shares, what sits below it, and what it has
// no part in.
//
// The last two are load-bearing. A document claiming Toft satisfied all 97
// requirements would be worthless to an assessor — it is software, and it has
// nothing to say about personnel screening or physical access — so the entries
// that claim nothing are what make the ones that claim something credible.
const (
	// RespPlatform: Toft implements it, and the install's own
	// configuration is the evidence.
	RespPlatform = "platform"
	// RespShared: Toft provides the mechanism; the organization operates
	// it. Both halves are stated.
	RespShared = "shared"
	// RespInherited: satisfied below Toft — the host, the cluster, the
	// facility, the identity provider.
	RespInherited = "inherited"
	// RespCustomer: Toft has no part in it. Said plainly rather than
	// padded.
	RespCustomer = "customer"
)

// Responsibilities is the closed list, in rendering order. A value outside it
// fails the registry's own validation rather than rendering as an unlabelled
// badge.
var Responsibilities = []string{RespPlatform, RespShared, RespInherited, RespCustomer}

// Credential is how an evidence item can be read. It is a property of the
// ITEM rather than of any particular reader, and it survives into the platform
// from the gallery extension that came first, where it decided whether a thing
// could be read at all.
//
// Inside the platform every item is reachable — the reader and the database are
// the same process — so this no longer gates collection. It is kept because it
// still says something true and useful to an assessor: an item marked
// CredSession is one the platform refuses to every API key by name, and that
// refusal is itself part of the access-control story the document tells.
const (
	CredNone    = "none"
	CredKey     = "key"
	CredSession = "session"
)

// Catalog is NIST's document, as extracted. Nothing in this struct is this
// project's writing.
type Catalog struct {
	Name         string   `json:"catalog"`
	Source       string   `json:"source"`
	OSCALVersion string   `json:"oscal_version"`
	Modified     string   `json:"oscal_last_modified"`
	Families     []Family `json:"families"`
	Requirements []Req    `json:"requirements"`
}

type Family struct {
	Code  string `json:"code"`
	Title string `json:"title"`
}

// Req is one requirement as NIST states it, before this project says anything
// about it. Requirement joins it with what this project says.
type Req struct {
	ID          string      `json:"id"`
	Family      string      `json:"family"`
	FamilyTitle string      `json:"family_title"`
	Title       string      `json:"title"`
	Statement   []Statement `json:"statement"`
	ODPs        []ODP       `json:"odps"`
	Guidance    string      `json:"guidance"`
	Objectives  []string    `json:"objectives"`
}

type Statement struct {
	Label string `json:"label"`
	Prose string `json:"prose"`
	Depth int    `json:"depth"`
}

// ODP is an organization-defined parameter: a period, a frequency or a set that
// NIST asks the organization to fill in.
//
// Binding these to live readings is the sharpest thing this package does. An
// install already answers a number of them — the account-inactivity period IS
// dormant_lock_days — and a document that made the organization write the
// number down a second time would be creating a second place for it to be
// wrong.
type ODP struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Usage     string `json:"usage"`
	Guideline string `json:"guideline"`
}

// Binding attaches an ODP to something that answers it. Exactly one of Evidence
// (a live reading) or Value (a fact about the build) is set; neither means the
// parameter is the organization's and Note says so.
type Binding struct {
	Evidence string `yaml:"evidence"`
	Field    string `yaml:"field"`
	Value    string `yaml:"value"`
	Note     string `yaml:"note"`
}

// Requirement is a NIST requirement joined with this project's answer to it.
type Requirement struct {
	Req

	Responsibility string   `json:"responsibility"`
	Summary        string   `json:"summary"`
	Mechanism      string   `json:"mechanism,omitempty"`
	Customer       string   `json:"customer,omitempty"`
	Refs           []string `json:"refs,omitempty"`
	EvidenceIDs    []string `json:"evidence,omitempty"`

	// Bindings is keyed by ODP id. A parameter with no entry is unaddressed and
	// renders as such — shown rather than hidden, because a coverage figure you
	// cannot see is a coverage figure you cannot trust.
	Bindings map[string]Binding `json:"-"`
}

// Evidence is one thing the platform can read to substantiate a claim.
type Evidence struct {
	ID         string `yaml:"id" json:"id"`
	Label      string `yaml:"label" json:"label"`
	Credential string `yaml:"credential" json:"credential"`
	Source     struct {
		Kind   string            `yaml:"kind" json:"kind,omitempty"`
		Route  string            `yaml:"route" json:"route,omitempty"`
		Fields []string          `yaml:"fields" json:"fields,omitempty"`
		Params map[string]string `yaml:"params" json:"-"`
	} `yaml:"source" json:"source"`
	Screen struct {
		Path        string `yaml:"path" json:"path,omitempty"`
		Anchor      string `yaml:"anchor" json:"anchor,omitempty"`
		Label       string `yaml:"label" json:"label,omitempty"`
		Anonymous   bool   `yaml:"anonymous" json:"-"`
		Conditional bool   `yaml:"conditional" json:"-"`
	} `yaml:"screen" json:"screen"`
	Reading string `yaml:"reading" json:"reading,omitempty"`
}

// Registry is the loaded, joined and validated whole. Build it once.
type Registry struct {
	Catalog      Catalog
	Requirements []Requirement
	Evidence     []Evidence

	byReq map[string]*Requirement
	byEv  map[string]*Evidence
}

type mappingDoc struct {
	Meta struct {
		Version int    `yaml:"version"`
		Catalog string `yaml:"catalog"`
		Note    string `yaml:"note"`
	} `yaml:"meta"`
	Requirements map[string]struct {
		Responsibility string             `yaml:"responsibility"`
		Summary        string             `yaml:"summary"`
		Mechanism      string             `yaml:"mechanism"`
		Customer       string             `yaml:"customer"`
		Evidence       []string           `yaml:"evidence"`
		ODP            map[string]Binding `yaml:"odp"`
		Refs           []string           `yaml:"refs"`
	} `yaml:"requirements"`
}

type evidenceDoc struct {
	Items []Evidence `yaml:"items"`
}

// Load parses, joins and validates the three registries.
//
// It returns an error rather than panicking, and every caller in this repository
// treats that error as fatal at startup — but the error is what lets the test
// assert the failure rather than crash the run, and a package that can only fail
// by dying cannot be tested for failing correctly.
func Load() (*Registry, error) {
	var cat Catalog
	if err := json.Unmarshal(catalogJSON, &cat); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var m mappingDoc
	if err := yaml.Unmarshal(mappingYAML, &m); err != nil {
		return nil, fmt.Errorf("mapping: %w", err)
	}
	var e evidenceDoc
	if err := yaml.Unmarshal(evidenceYAML, &e); err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}

	r := &Registry{
		Catalog:  cat,
		Evidence: e.Items,
		byReq:    make(map[string]*Requirement, len(cat.Requirements)),
		byEv:     make(map[string]*Evidence, len(e.Items)),
	}

	for i := range r.Evidence {
		it := &r.Evidence[i]
		if _, dup := r.byEv[it.ID]; dup {
			return nil, fmt.Errorf("evidence %q is defined twice", it.ID)
		}
		r.byEv[it.ID] = it
	}

	for _, req := range cat.Requirements {
		entry, ok := m.Requirements[req.ID]
		if !ok {
			// A requirement with no mapping would render as a blank section in
			// an assessment document — which is worse than a missing one,
			// because a blank reads as "nothing to say" rather than as an
			// omission. Refuse to build the registry at all.
			return nil, fmt.Errorf("requirement %s has no mapping entry", req.ID)
		}
		if !validResponsibility(entry.Responsibility) {
			return nil, fmt.Errorf("requirement %s: responsibility %q is not one of %v",
				req.ID, entry.Responsibility, Responsibilities)
		}
		for _, id := range entry.Evidence {
			if _, ok := r.byEv[id]; !ok {
				return nil, fmt.Errorf("requirement %s cites unknown evidence %q", req.ID, id)
			}
		}
		known := make(map[string]bool, len(req.ODPs))
		for _, o := range req.ODPs {
			known[o.ID] = true
		}
		for id, b := range entry.ODP {
			if !known[id] {
				return nil, fmt.Errorf("requirement %s binds unknown parameter %q", req.ID, id)
			}
			if b.Evidence != "" {
				if _, ok := r.byEv[b.Evidence]; !ok {
					return nil, fmt.Errorf("requirement %s parameter %s cites unknown evidence %q",
						req.ID, id, b.Evidence)
				}
			}
		}
		r.Requirements = append(r.Requirements, Requirement{
			Req:            req,
			Responsibility: entry.Responsibility,
			Summary:        entry.Summary,
			Mechanism:      entry.Mechanism,
			Customer:       entry.Customer,
			Refs:           entry.Refs,
			EvidenceIDs:    entry.Evidence,
			Bindings:       entry.ODP,
		})
	}

	// A mapping entry for a requirement the catalog does not have is the
	// orphan case, and it is the one a catalog refresh produces: NIST withdrew
	// 33 requirements between revisions, and a mapping still answering one of
	// them is prose describing a document nobody is assessed against.
	for id := range m.Requirements {
		if _, ok := findReq(cat.Requirements, id); !ok {
			return nil, fmt.Errorf("mapping answers %s, which the catalog does not contain", id)
		}
	}

	for i := range r.Requirements {
		r.byReq[r.Requirements[i].ID] = &r.Requirements[i]
	}
	return r, nil
}

func findReq(reqs []Req, id string) (Req, bool) {
	for _, q := range reqs {
		if q.ID == id {
			return q, true
		}
	}
	return Req{}, false
}

func validResponsibility(v string) bool {
	for _, r := range Responsibilities {
		if r == v {
			return true
		}
	}
	return false
}

// Requirement returns one requirement, or nil.
func (r *Registry) Requirement(id string) *Requirement { return r.byReq[id] }

// EvidenceItem returns one evidence definition, or nil.
func (r *Registry) EvidenceItem(id string) *Evidence { return r.byEv[id] }

// Coverage is the registry's account of itself.
//
// **Summed from the rows, never written down.** A coverage figure a human
// maintains is a coverage figure that goes stale, and this one is quoted at an
// assessor.
type Coverage struct {
	Requirements     int            `json:"requirements"`
	Families         int            `json:"families"`
	ByResponsibility map[string]int `json:"by_responsibility"`
	Evidence         int            `json:"evidence"`
	EvidenceByCred   map[string]int `json:"evidence_by_credential"`

	// ODPs, and the three states a parameter can be in. Unbound is reported
	// rather than hidden: it is the honest half of the number beside it.
	ODPs        int `json:"odps"`
	ODPsBound   int `json:"odps_bound"`
	ODPsNoted   int `json:"odps_noted"`
	ODPsUnbound int `json:"odps_unbound"`
}

func (r *Registry) Coverage() Coverage {
	c := Coverage{
		Requirements:     len(r.Requirements),
		Families:         len(r.Catalog.Families),
		ByResponsibility: map[string]int{},
		Evidence:         len(r.Evidence),
		EvidenceByCred:   map[string]int{},
	}
	for _, e := range r.Evidence {
		c.EvidenceByCred[e.Credential]++
	}
	for _, q := range r.Requirements {
		c.ByResponsibility[q.Responsibility]++
		for _, o := range q.ODPs {
			c.ODPs++
			b, ok := q.Bindings[o.ID]
			switch {
			case ok && (b.Evidence != "" || b.Value != ""):
				c.ODPsBound++
			case ok:
				c.ODPsNoted++
			default:
				c.ODPsUnbound++
			}
		}
	}
	return c
}

// EvidenceRoutes returns every distinct API route the evidence definitions
// name, sorted. The api package's test walks the router against this, so an
// item pointing at a route that no longer exists fails the build rather than
// citing a source an assessor cannot follow.
func (r *Registry) EvidenceRoutes() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range r.Evidence {
		if e.Source.Route == "" {
			continue
		}
		if !seen[e.Source.Route] {
			seen[e.Source.Route] = true
			out = append(out, e.Source.Route)
		}
	}
	sort.Strings(out)
	return out
}

// Citations returns every distinct reference the mapping makes into this
// repository's own documentation, sorted. The test resolves each against
// docs/invariants.md and docs/architecture-decisions.md.
func (r *Registry) Citations() []string {
	seen := map[string]bool{}
	var out []string
	for _, q := range r.Requirements {
		for _, ref := range q.Refs {
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	sort.Strings(out)
	return out
}
