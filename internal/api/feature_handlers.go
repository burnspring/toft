package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
)

// Admin → Features: what this build offers, what it is licensed for, and what an
// administrator has switched on.
//
// Admin-only and 404 to everyone else, like /system/settings: which features an
// install has bought is not something an app owner is owed. That is the same
// line ADR §20 draws from the other side — an owner is owed the rules *their
// app* is judged by, not the shape of the install.
//
// # The four gates, and why two of them live here
//
//	1. licensed  — internal/entitlements, from the license key
//	2. enabled   — the `features` table, audited
//	3. granted   — a per-capability grant table, per app
//	4. now       — appint.Check, immediately before the transport
//
// This surface owns 1 and 2 and shows them separately, because they are
// separate: an administrator must be able to switch off something they have
// paid for — during an incident, while a data-egress policy is reviewed, or
// because a budget ran out — without surrendering the entitlement.

// featureDTO is one row of the page. It merges three sources that are
// deliberately not merged in storage: the catalog of what this binary offers
// (a property of the binary), the license answer (a property of the key), and
// the administrator's switch (a row).
type featureDTO struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`

	// Licensed is gate 1. Unlicensed reports that the install has no key at
	// all, so the page can say "no license key — every feature this build
	// offers is available" rather than render a tick that means nothing.
	Licensed    bool   `json:"licensed"`
	Unlicensed  bool   `json:"unlicensed"`
	InGrace     bool   `json:"in_grace"`
	LicenseNote string `json:"license_note,omitempty"`

	// Tier is what this install is on and MinTier is what the feature asks
	// for, so the page can render **locked** — visible, labelled, one line of
	// what it does and one button. That is the opposite of *absent, not
	// dormant*, and it is deliberate: a feature nobody can see sells nothing,
	// and the two states are opposite to a reader of the screen while being
	// identical to a reader of the code.
	Tier    string `json:"tier"`
	MinTier string `json:"min_tier"`

	// AdminSwitch is whether gate 2 exists here at all. False renders the row
	// with its license state and no toggle: switching off the providers your
	// colleagues sign in with is not a thing this page offers, because there is
	// nothing to switch — a license decides whether *new* configuration may be
	// written and never whether existing configuration is honoured.
	AdminSwitch bool `json:"admin_switch"`

	// Enabled is gate 2. EverEnabled is what keeps a switched-off feature
	// visible: absent and off are different states, and an administrator whose
	// FAQ app stopped answering needs to see *why* rather than find the row
	// gone.
	Enabled     bool   `json:"enabled"`
	EverEnabled bool   `json:"ever_enabled"`
	EnabledAt   string `json:"enabled_at,omitempty"`
	EnabledBy   string `json:"enabled_by,omitempty"`
}

func (s *Server) handleListFeatures(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureAdmin(w, r) {
		return
	}

	rows, err := s.db.ListFeatures()
	if err != nil {
		log.Printf("listing features: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	byName := map[string]database.Feature{}
	for _, f := range rows {
		byName[f.Name] = f
	}

	// entitlements.Known is the catalog, and it leads: a feature this build does
	// not offer must not appear because a row for it survived a downgrade.
	out := []featureDTO{}
	for _, d := range entitlements.Current() {
		out = append(out, s.featureDTO(d, byName[d.Name]))
	}
	writeJSON(w, http.StatusOK, out)
}

// featureDTO assembles one row. Split out because the install action returns the
// same shape, and two places that could disagree about whether a feature is on
// is exactly the bug this page exists to prevent.
func (s *Server) featureDTO(d entitlements.Descriptor, row database.Feature) featureDTO {
	lic := s.entitlements().Entitled(d.Name)

	dto := featureDTO{
		Name:        d.Name,
		Title:       d.Title,
		Summary:     d.Summary,
		Licensed:    lic.Entitled,
		Unlicensed:  lic.Unlicensed,
		InGrace:     lic.InGrace,
		LicenseNote: lic.Reason,
		Tier:        string(lic.Tier),
		MinTier:     string(d.MinTier),
		AdminSwitch: d.AdminSwitch,
		Enabled:     row.Enabled,
		// A row exists only because somebody enabled the feature at least once —
		// SetFeatureEnabled is the only writer, and the install action goes
		// through it.
		EverEnabled: row.Name != "",
		EnabledBy:   row.EnabledBy,
	}
	if row.EnabledAt != nil {
		dto.EnabledAt = row.EnabledAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return dto
}

type featureUpdateRequest struct {
	Enabled bool `json:"enabled"`
}

// handleUpdateFeature is gate 2, and only gate 2.
//
// It never touches the license and never touches an app's grant. Switching a
// feature off leaves the app installed and serving — a license or a switch never
// takes down running code — and leaves the grant rows alone, so switching it
// back on restores what was there rather than requiring somebody to remember
// what had been granted.
func (s *Server) handleUpdateFeature(w http.ResponseWriter, r *http.Request) {
	if !s.requireFeatureAdmin(w, r) {
		return
	}
	name := chi.URLParam(r, "name")
	d := entitlements.Describe(name)
	if d == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this build does not offer a feature called " + name,
			"hint":  "GET /api/v1/admin/features lists the features by name"})
		return
	}

	// **A feature with no administrator's switch is refused by name**, and 400
	// rather than 404: the feature exists — it is in the catalog, the page
	// renders it, and this endpoint just described it — what does not exist is
	// the switch. 404 would say the name was wrong and send somebody looking
	// for a typo.
	//
	// This is the cost of a licensed surface joining the catalog, paid here
	// rather than designed around. `featureDTO` merges `Known` with the
	// `features` row, so without this an administrator would acquire a way to
	// switch off the identity providers their colleagues sign in with.
	if !d.AdminSwitch {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "There is no on/off switch for " + d.Title + ". A license decides " +
				"whether new configuration may be written here; there is nothing to switch off.",
			"code": "no_admin_switch",
			"hint": "leave it as it is — GET /api/v1/admin/features shows which features have a switch"})
		return
	}

	var req featureUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "hint": badBodyHint})
		return
	}

	// Enabling something this install is not entitled to is refused rather than
	// stored, because a switch that is on over a license that is off would
	// render as working and refuse at the transport — which is the shape of
	// failure this whole four-gate design exists to avoid. Switching *off* is
	// always allowed, whatever the license says.
	if req.Enabled {
		if lic := s.entitlements().Entitled(name); !lic.Entitled {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this install is not licensed for " + d.Title + " — " + lic.Reason,
				"code":  "not_licensed"})
			return
		}
	}

	p := auth.GetPrincipal(r.Context())
	meta := database.GetRequestMeta(r.Context())
	if err := s.db.SetFeatureEnabled(name, req.Enabled, p.ID, meta); err != nil {
		log.Printf("setting feature %s: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	row, err := s.db.GetFeature(name)
	if err != nil || row == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.featureDTO(*d, *row))
}

// entitlements returns the license source. Never nil: an install that was
// started without one is permissively licensed, which is the state every install
// is in until a licensing system exists, and a nil check at every call site
// would be four chances to get the default wrong.
func (s *Server) entitlements() entitlements.Source {
	if s.ents == nil {
		return entitlements.Default()
	}
	return s.ents
}

func (s *Server) requireFeatureAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return false
	}
	return true
}
