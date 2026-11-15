package api

import (
	"net/http"
)

// The scheduled restart: one row per app, declared as `[restart]` in its
// toft.toml and fired by RestartScheduler (lifecycle.go). The routes are a
// read and a pause, as the re-render's are: the file is the only writer
// (ADR §144).

// handleGetRestartSchedule answers with the app's schedule and its last
// outcome, or `null` when its toft.toml declares none.
func (s *Server) handleGetRestartSchedule(w http.ResponseWriter, r *http.Request) {
	app := s.editableApp(w, r)
	if app == nil {
		return
	}
	sch, err := s.db.GetRestartSchedule(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if sch == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

// handlePatchRestartSchedule pauses or resumes it until the next deploy.
func (s *Server) handlePatchRestartSchedule(w http.ResponseWriter, r *http.Request) {
	app, ok := s.pauseAppSchedule(w, r, "restart")
	if !ok {
		return
	}
	sch, err := s.db.GetRestartSchedule(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, sch)
}
