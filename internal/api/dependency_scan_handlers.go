package api

import (
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// The dependency scan history — the governance read, and the administrator's.
//
// **Two readers, and the split is the point.** The owner is being asked to fix
// something and needs the latest reading, which rides on their own
// `GET /apps/{appID}/dependencies` as one `last_scan` object. This is the
// longitudinal question — *how long has this been true, and did anyone act* —
// and a longitudinal reading of a finding is a statement about **response**,
// which is the class `audit_log`'s two-reader rule and `/admin/packages`'s
// admin-only rule already govern.
//
// It is not squeamishness about the data: the owner can already see every
// finding in it. It is that the same rows answer a different question when they
// are stacked, and that question is an administrator's.

// dependencyScanHistoryResponse is one app's readings, newest first.
type dependencyScanHistoryResponse struct {
	AppID   string `json:"app_id"`
	AppSlug string `json:"app_slug"`
	// Kept is the install's retention count, so the panel can say "the last 30"
	// rather than implying the list is everything that ever happened.
	Kept  int                       `json:"kept"`
	Total int                       `json:"total"`
	Scans []database.DependencyScan `json:"scans"`
	// Lock is what the install would do about a finding here, on the page where
	// somebody is deciding whether the history justifies the switch.
	LockOn         string `json:"lock_on"`
	LockedByPolicy bool   `json:"locked_by_policy"`
}

// handleAppDependencyScans — GET /admin/apps/{appID}/dependency-scans.
//
// Administrator-only, and 404 to everyone else rather than 403: *404 over 403*,
// and the existence of a governance history is not a fact an owner needs to
// discover by probing.
//
// It takes an id from the URL, so it scopes by that id — the app is loaded by
// it and every query below reads it back rather than trusting the parameter.
func (s *Server) handleAppDependencyScans(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return
	}

	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	limit := settings.DependencyScanHistoryKept
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= database.MaxDependencyScanHistoryKept {
			limit = n
		}
	}

	resp := dependencyScanHistoryResponse{
		AppID: app.ID, AppSlug: app.Slug,
		Kept:           settings.DependencyScanHistoryKept,
		Scans:          []database.DependencyScan{},
		LockOn:         settings.DependencyLockOn,
		LockedByPolicy: app.LockedByPolicy(),
	}
	if scans, err := s.db.ListDependencyScans(app.ID, limit); err == nil {
		resp.Scans = scans
	} else {
		log.Printf("deps: reading %s's scan history: %v", app.Slug, err)
	}
	if n, err := s.db.CountDependencyScans(app.ID); err == nil {
		resp.Total = n
	}
	writeJSON(w, http.StatusOK, resp)
}
