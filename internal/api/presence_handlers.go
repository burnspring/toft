package api

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/presence"
)

// trackPresence stamps the caller as online for the platform API. It runs after
// auth.Middleware, so an anonymous request has nothing to stamp and is skipped
// — as is a service account, which is a credential rather than a person.
//
// This is a map write under a mutex, never a database write: the rule that the
// request path pays no DB write is the reason presence lives in memory at all.
func (s *Server) trackPresence(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A key is excluded for the same reason a service account is: presence
		// answers "who is online now", and a cron job holding a token is not a
		// person at a desk. Its traffic is still usage — it is just not
		// attendance.
		if p := auth.GetPrincipal(r.Context()); p.IsUser() && !p.IsAPIKey() {
			s.presence.Touch(p.ID, "")
		}
		next.ServeHTTP(w, r)
	})
}

// presenceWindowSeconds bounds what a caller may ask for. The ceiling is the
// tracker's retention: a longer window would be answered from a shorter memory
// and read as a confident undercount.
const (
	presenceMinWindow = 60
	presenceMaxWindow = int(presence.Retention / time.Second)
)

// presenceUser is one person who is online, as this node saw them.
type presenceUser struct {
	ID         string    `json:"id"`
	Login      string    `json:"login"`
	Name       string    `json:"name,omitempty"`
	AvatarURL  string    `json:"avatar_url,omitempty"`
	SystemRole string    `json:"system_role,omitempty"`
	LastSeen   time.Time `json:"last_seen"`

	// Where they were last seen. AppID is empty for someone who has only used
	// Toft's own UI; InApp distinguishes "is in that app now" from "opened
	// it earlier in this window and has been on the platform since".
	AppID   string `json:"app_id,omitempty"`
	AppName string `json:"app_name,omitempty"`
	AppSlug string `json:"app_slug,omitempty"`
	InApp   bool   `json:"in_app"`
}

// handlePresence — GET /system/presence. system_admin only, for the same reason
// as the active-user counts: a platform-wide view of who is doing what is an
// operator's number. Unlike those counts it names people, and it is answered
// entirely from this node's memory.
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to check who is online"})
		return
	}

	windowSecs := int(presence.DefaultWindow / time.Second)
	if v := r.URL.Query().Get("window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < presenceMinWindow || n > presenceMaxWindow {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "window must be " + strconv.Itoa(presenceMinWindow) + "-" + strconv.Itoa(presenceMaxWindow) + " seconds",
				"hint":  "pass window as a whole number of seconds in the range the sentence names, or leave it out for the default",
			})
			return
		}
		windowSecs = n
	}
	window := time.Duration(windowSecs) * time.Second

	entries := s.presence.Active(window)

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.UserID)
	}
	users, err := s.db.GetUsersByIDs(ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// One read for the app names rather than one per entry; the apps a handful
	// of people are looking at are a subset of a list this process already
	// serves constantly.
	appsByID := map[string]database.App{}
	if anyApp(entries) {
		apps, err := s.db.ListAllApps()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		for _, a := range apps {
			appsByID[a.ID] = a
		}
	}

	cutoff := time.Now().UTC().Add(-window)
	out := make([]presenceUser, 0, len(entries))
	for _, e := range entries {
		u, ok := users[e.UserID]
		if !ok {
			// Deleted between the touch and this read. Nothing to name, and a
			// row of blanks would read as a mystery user rather than as noise.
			continue
		}
		pu := presenceUser{
			ID: u.ID, Login: u.Login, Name: u.Name,
			AvatarURL: u.AvatarURL, SystemRole: u.SystemRole,
			LastSeen: e.LastSeen,
		}
		if e.AppID != "" {
			if app, ok := appsByID[e.AppID]; ok {
				pu.AppID, pu.AppName, pu.AppSlug = app.ID, app.Name, app.Slug
				pu.InApp = e.AppSeen.After(cutoff)
			}
		}
		out = append(out, pu)
	}

	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"window_seconds": windowSecs,
		// Presence is per-process, so the UI has to be able to say whose view
		// this is rather than implying it covers the install.
		"node":  host,
		"count": len(out),
		"users": out,
	})
}

func anyApp(entries []presence.Entry) bool {
	for _, e := range entries {
		if e.AppID != "" {
			return true
		}
	}
	return false
}
