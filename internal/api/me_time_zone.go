package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"toft/internal/auth"
	"toft/internal/database"
)

// The zone the SPA shows exact times in (ADR §154). Display only: the API
// speaks UTC ISO-8601 whatever is chosen here, and nothing on the server is
// computed in it.
//
// Two layers and a fallback. A person's own choice wins; with none, the
// install's default; with no default either, the browser's own zone — which is
// what the product did before either existed. The server never resolves
// "browser": only the page knows what that is, so it hands both layers over
// and the SPA picks.

// myTimeZone is GET /auth/me's `time_zone`.
type myTimeZone struct {
	// "" follows Install, "browser" is the person's browser, otherwise a zone.
	User string `json:"user"`
	// "" is each viewer's browser, otherwise "UTC" or an IANA name.
	Install string `json:"install"`
}

// timeZoneFor reads both layers. A failed read is the browser's zone — a page
// that formats every date in it is the product before this feature, not an
// error worth refusing a sign-in check over.
func (s *Server) timeZoneFor(userID string) myTimeZone {
	tz := myTimeZone{}
	if v, err := s.db.GetUserTimeZone(userID); err != nil {
		log.Printf("reading time zone for %s: %v", userID, err)
	} else {
		tz.User = v
	}
	if settings, err := s.db.GetPlatformSettings(); err != nil {
		log.Printf("reading display time zone: %v", err)
	} else {
		tz.Install = settings.DisplayTimeZone
	}
	return tz
}

// handleSetMyTimeZone — PUT /me/time-zone {"time_zone": ""|"browser"|zone}.
// Not audited: it changes how one person's pages print a date and nothing
// anybody else reads, the same class as a remembered tab.
func (s *Server) handleSetMyTimeZone(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req struct {
		TimeZone *string `json:"time_zone"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.TimeZone == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "send the zone to show times in",
			"hint":  `{"time_zone": "Europe/London"}, "UTC", "browser", or "" to follow the install's default`})
		return
	}
	zone, err := database.NormalizeUserTimeZone(*req.TimeZone)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "time_zone " + err.Error(),
			"hint":  `{"time_zone": "Europe/London"}, "UTC", "browser", or "" to follow the install's default`})
		return
	}
	if err := s.db.SetUserTimeZone(p.ID, zone); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "user not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.timeZoneFor(p.ID))
}
