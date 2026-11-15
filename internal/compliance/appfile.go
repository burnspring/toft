package compliance

import (
	"context"
	"fmt"
	"time"
)

// The app's audit file: one app, described to an assessor.
//
// **An evidence inventory, bound to no framework.** An app on Toft is a
// component inside the customer's boundary, and the controls about the app
// itself — its input validation, its own sessions, its developers' testing —
// are about code Toft did not write; a per-app control document would read
// "customer" on nearly every row and repeat the install's on the rest. Where a
// section substantiates an install evidence item it cites the id, and the
// cited ids are checked against the registry, so a renamed item fails the
// build rather than leaving a dangling citation.
//
// **The sections are a closed list, and the list is the specification.** It is
// declared here, without a database; internal/api collects each by key, and a
// test fails on a section with no collector or a collector with no section.
// Each section names the route it mirrors, and its gate is that route's — for
// the caller and for the credential — computed, never restated, so a later
// change that narrows a route narrows the pack with it.

// The gates a section can carry: the check its mirrored route's handler
// makes. GateNone is a section with nothing behind it to refuse.
const (
	GateEdit         = "edit"          // appForEdit
	GateManageGrants = "manage_grants" // ActionManageGrants
	GateAdmin        = "admin"         // system_admin
	GateRead         = "read"          // ActionRead
	GateNone         = "none"
)

// AppFileSection is one section of the pack.
type AppFileSection struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Cites is the install evidence ids this section substantiates.
	Cites []string `json:"cites,omitempty"`
	// Gate is the mirrored route's check; Mirrors its pattern, as the key
	// lists spell it, or "" for GateNone.
	Gate    string `json:"-"`
	Mirrors string `json:"-"`
}

// AppFileSections is the pack, in the order it renders.
var AppFileSections = []AppFileSection{
	{Key: "identity", Label: "What the app is", Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "source", Label: "Where its code comes from", Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "access", Label: "Who may open it", Cites: []string{"user_access_review", "apps_and_visibility"},
		Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "security_headers", Label: "What its responses carry", Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "embedding_and_identity", Label: "Who may frame it, and whether it is told who is looking",
		Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "releases", Label: "What was deployed, and by whom", Cites: []string{"dependency_findings"},
		Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/deployments"},
	{Key: "dependencies", Label: "The live release's packages", Cites: []string{"dependency_findings", "dependency_inventory"},
		Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/dependencies"},
	{Key: "reach", Label: "What it can reach", Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "credential_age", Label: "How old its credentials are", Gate: GateRead, Mirrors: "GET /api/v1/apps/{appID}"},
	{Key: "logs", Label: "Where its output goes", Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "viewers", Label: "Who opened it", Gate: GateManageGrants, Mirrors: "GET /api/v1/apps/{appID}/usage/viewers"},
	{Key: "activity", Label: "What was done to it", Gate: GateAdmin, Mirrors: "GET /api/v1/admin/audit"},
	{Key: "deletion", Label: "What deleting it removes, and what it leaves", Gate: GateEdit, Mirrors: "GET /api/v1/apps/{appID}/reach"},
	{Key: "cannot_see", Label: "What this install cannot see", Gate: GateNone},
}

// ValidateAppFile is the list's own check: every gate known, every gated
// section mirroring a route, every citation an evidence item that exists.
func ValidateAppFile(reg *Registry) error {
	seen := map[string]bool{}
	for _, sec := range AppFileSections {
		if seen[sec.Key] {
			return fmt.Errorf("app file: section %q twice", sec.Key)
		}
		seen[sec.Key] = true
		switch sec.Gate {
		case GateEdit, GateManageGrants, GateAdmin, GateRead:
			if sec.Mirrors == "" {
				return fmt.Errorf("app file: section %q has a gate and mirrors no route", sec.Key)
			}
		case GateNone:
		default:
			return fmt.Errorf("app file: section %q has no known gate (%q)", sec.Key, sec.Gate)
		}
		for _, id := range sec.Cites {
			if reg.EvidenceItem(id) == nil {
				return fmt.Errorf("app file: section %q cites %q, which is no evidence item", sec.Key, id)
			}
		}
	}
	return nil
}

// AppFileCollector reads one section.
type AppFileCollector interface {
	CollectSection(ctx context.Context, sec AppFileSection) Result
}

// AppFile is one app's pack, read.
type AppFile struct {
	CollectedAt time.Time       `json:"collected_at"`
	App         AppFileSubject  `json:"app"`
	Install     AppFileInstall  `json:"install"`
	Producer    AppFileProducer `json:"producer"`
	Sections    []Result        `json:"sections"`
	ByOutcome   map[Outcome]int `json:"by_outcome"`
}

// AppFileSubject is the app the pack describes.
type AppFileSubject struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// AppFileInstall is the machine it was read on.
type AppFileInstall struct {
	ID       string `json:"id,omitempty"`
	Version  string `json:"version,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

// AppFileProducer is who read it, and with what — so two honestly different
// packs of one app each say why.
type AppFileProducer struct {
	PrincipalID string `json:"principal_id"`
	Credential  string `json:"credential"` // session | personal_key
	KeyID       string `json:"key_id,omitempty"`
}

// AssembleAppFile walks every section and collects it. **Nothing is ever
// skipped**: a panicking or empty collector is that section's error, as in
// the install report.
func AssembleAppFile(ctx context.Context, c AppFileCollector) *AppFile {
	f := &AppFile{CollectedAt: time.Now().UTC(), ByOutcome: map[Outcome]int{}}
	for _, sec := range AppFileSections {
		res := contain(func() Result { return c.CollectSection(ctx, sec) })
		res.ID, res.Label = sec.Key, sec.Label
		if res.TakenAt.IsZero() {
			res.TakenAt = time.Now().UTC()
		}
		f.Sections = append(f.Sections, res)
		f.ByOutcome[res.Outcome]++
	}
	return f
}

// FreezeAppFile is the export: the same digest rule as the install report's.
func FreezeAppFile(f *AppFile) ([]byte, error) { return freeze("toft-app-audit-file", f) }

// AppFileFilename names the file the browser saves.
func AppFileFilename(f *AppFile) string {
	return SafeFilenamePart(f.App.Slug) + "-audit-" + f.CollectedAt.Format("2006-01-02") + ".json"
}
