package api

import (
	"encoding/json"
	"log"
	"net/http"

	"toft/internal/auth"

	"toft/internal/database"
	"toft/internal/schedule"
)

// The re-render schedule: one row per app, declared as `[render]` in toft.toml.
//
// A notebook is executed once, at build time, and served as a document — so a
// report nobody re-runs is a screenshot. Re-execution is what makes this half a
// product rather than a file format, and it is a *deploy*: the same BuildJob the
// Redeploy button enqueues, producing a new deployment row, a new release and a
// new artifact. See internal/builder/renderschedule.go for why it cannot be
// either kind of automation.
//
// **The file is the only writer of the schedule** (ADR §144): it
// versions and rolls back with the report, is reviewed in the same diff, and is
// judged by `toft check`. So the routes here are a read, a pause that lasts
// until the next deploy, and the recipient list — which stays in the UI because
// it is people, and accounts in a repository would be user ids in a commit
// (D14). PUT and DELETE answer 405 and say where the schedule is written.

// handleGetRenderSchedule answers with the app's schedule, or `null`.
//
// null rather than 404 for a notebook with no schedule: "this app has none" is
// the ordinary state of the majority of them, and a 404 is the answer to "there
// is no such app", which the page has to be able to tell apart.
func (s *Server) handleGetRenderSchedule(w http.ResponseWriter, r *http.Request) {
	app := s.editableApp(w, r)
	if app == nil {
		return
	}
	sch, err := s.db.GetRenderSchedule(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.writeScheduleWithRecipients(w, sch)
}

// handleRenderScheduleDeclared answers the PUT and DELETE an older client
// sends: the schedule is declared in toft.toml now, and the refusal says so
// rather than leaving a generic 405 to be puzzled over.
func (s *Server) handleRenderScheduleDeclared(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "GET, PATCH")
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error": "a re-render schedule is declared as [render] in the app's toft.toml, not set through the API",
		"code":  "method_not_allowed",
		"hint":  "declare [render] in toft.toml and deploy; PATCH {\"enabled\": false} pauses it",
	})
}

// handlePatchRenderSchedule pauses or resumes the app's re-render — the one
// field the file does not own, until the next deploy resumes it.
func (s *Server) handlePatchRenderSchedule(w http.ResponseWriter, r *http.Request) {
	app, ok := s.pauseAppSchedule(w, r, "render")
	if !ok {
		return
	}
	sch, err := s.db.GetRenderSchedule(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.writeScheduleWithRecipients(w, sch)
}

// handlePutRenderScheduleRecipients replaces the list of people told when a
// render lands. Refused while the file declares no `[render]`: a list with no
// schedule would be a list nothing sends, and would reappear on a later one.
func (s *Server) handlePutRenderScheduleRecipients(w http.ResponseWriter, r *http.Request) {
	app := s.editableApp(w, r)
	if app == nil {
		return
	}
	var req struct {
		// A pointer, so an omitted field is a refusal rather than an empty
		// list: a client that forgot it would otherwise silently clear a list
		// somebody spent an afternoon building.
		Recipients *[]string `json:"recipients"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Recipients == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "send {\"recipients\": [user ids]}; an empty array clears the list", "hint": `send {"recipients": ["<user id>"]}; GET /api/v1/directory/users lists the ids`})
		return
	}
	sch, err := s.db.GetRenderSchedule(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if sch == nil {
		writeJSON(w, http.StatusConflict, noScheduleRefusal("render"))
		return
	}
	p := auth.GetPrincipal(r.Context())
	if err := s.db.ReplaceScheduleRecipients(r.Context(), sch.ID, *req.Recipients, p.ID); err != nil {
		log.Printf("saving report recipients for %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.writeScheduleWithRecipients(w, sch)
}

// noScheduleRefusal is the 409 for an app whose toft.toml declares no such
// table, with the block that would.
func noScheduleRefusal(table string) map[string]string {
	what := map[string]string{"render": "re-render", "restart": "restart"}[table]
	return map[string]string{
		"error": "this app's toft.toml declares no [" + table + "], so it has no " + what + " schedule",
		"code":  "no_" + table + "_schedule",
		"hint":  "add [" + table + "] with schedule = \"0 4 * * *\" to toft.toml and deploy",
	}
}

// pauseAppSchedule is the PATCH both one-per-app schedules take: `enabled`
// and nothing else, audited as the job pause is. It answers every refusal
// itself and reports whether the caller should write the schedule back.
func (s *Server) pauseAppSchedule(w http.ResponseWriter, r *http.Request, table string) (*database.App, bool) {
	app := s.editableApp(w, r)
	if app == nil {
		return nil, false
	}
	// Resuming a locked app's schedule would hand the lock's decision back to
	// a timetable; pausing is refused with it, so the lock is one answer.
	if s.refuseIfLocked(w, app) {
		return nil, false
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "send {\"enabled\": false} to pause the schedule or true to resume it; the schedule " +
				"itself is declared in toft.toml",
			"code": "bad_request",
			"hint": "edit [" + table + "] in toft.toml and deploy"})
		return nil, false
	}
	set := s.db.SetRenderScheduleEnabled
	if table == "restart" {
		set = s.db.SetRestartScheduleEnabled
	}
	found, err := set(app.ID, *req.Enabled, schedule.Next)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	if !found {
		writeJSON(w, http.StatusConflict, noScheduleRefusal(table))
		return nil, false
	}
	// Spelled out rather than composed, so a grep for the action finds it.
	actions := map[string][2]string{
		"render":  {"app.render_schedule.pause", "app.render_schedule.resume"},
		"restart": {"app.restart_schedule.pause", "app.restart_schedule.resume"},
	}[table]
	action := actions[0]
	if *req.Enabled {
		action = actions[1]
	}
	s.auditJob(r, action, app.ID, nil, map[string]any{"enabled": *req.Enabled})
	return app, true
}

// writeScheduleWithRecipients answers with the schedule, carrying its list.
//
// **On the schedule rather than in an envelope beside it**, because `null` for
// an app with no schedule is a contract this handler already argues for: "this
// app has none" and "there is no such app" are different facts, and wrapping
// the answer would make the first one an object that has to be inspected. The
// recipients belong to the schedule, so they ride on it — the same way an app
// carries its summary and an announcement carries its counts.
func (s *Server) writeScheduleWithRecipients(w http.ResponseWriter, sch *database.RenderSchedule) {
	if sch == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	people, err := s.db.ListScheduleRecipients(sch.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	sch.Recipients = people
	writeJSON(w, http.StatusOK, sch)
}

// handleListReportDeliveries answers with the recent runs and what happened to
// each person on them.
//
// The app's editors, the same authorization the schedule itself has: "did
// Monday's report go out" is a question its owner should not need an
// administrator for, and without this the feature's whole record lives in a
// table nobody but an administrator can read.
func (s *Server) handleListReportDeliveries(w http.ResponseWriter, r *http.Request) {
	app := s.editableApp(w, r)
	if app == nil {
		return
	}
	deliveries, err := s.db.ListReportDeliveries(app.ID, 20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
}
