package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/jobs"
	"toft/internal/runtime"
	"toft/internal/schedule"
)

// Job definitions and runs, scoped to their app.
//
// Definitions come from toft.toml and nowhere else: these routes list them,
// pause and resume a schedule, and start, re-run, cancel and read runs. Nothing
// here creates, edits or deletes a job.
//
// Authorization follows the precedent the log endpoints set rather than
// minting new actions: **edit-content** to start, pause, re-run or cancel;
// **edit-content** (logsAction) to see definitions, history and logs — a run's
// params, actor and output are workload output, and a viewer or an anonymous
// visitor of a public app must not see it any more than on /logs. A viewer
// starts a job through the app's own button (the in-app route), never here.
//
// Every lookup is scoped by app as well as by ID. Authorizing against the app
// in the path and then fetching a row by its own ID alone is the IDOR shape,
// and it is the one that keeps recurring.

// jobAuth resolves the app in the path and authorizes an action against it.
// Returns nil having already written the response when the caller may not
// proceed — 404 over 403 for an app they cannot read.
func (s *Server) jobAuth(w http.ResponseWriter, r *http.Request, action auth.Action) *database.App {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return nil
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	d := auth.Authorize(p, action, obj, s.db)
	if !d.Allow {
		writeAuthzError(w, r, d, readable)
		return nil
	}
	return app
}

// --- Definitions ---

// jobListEntry is one declared job as the Automation tab and `toft job list`
// see it: the definition, its effective limits with where each came from, and
// whether its schedule is paused.
type jobListEntry struct {
	database.JobDefinition
	Limits jobs.Limits `json:"limits"`
	// Paused is the schedule switched off from the Automation tab — until the
	// next deploy, which re-applies toft.toml.
	Paused bool `json:"paused"`
	// HeldByLock is a schedule the app's lock is holding: the scheduler skips
	// every firing of a locked app, so next_run_at is left out rather than
	// promised (drills D15, 2026-10-28). A paused schedule is paused, not held.
	HeldByLock bool `json:"held_by_lock"`
}

func (s *Server) handleListJobDefinitions(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, logsAction)
	if app == nil {
		return
	}

	defs, err := s.db.ListJobDefinitions(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	settings, _ := s.db.GetPlatformSettings()
	env := s.envelope()
	caps := s.capabilities()
	placement, held := caps.JobPlacement(), s.containment(r.Context())
	out := make([]jobListEntry, 0, len(defs))
	locked := app.Locked()
	for i := range defs {
		e := jobListEntry{
			JobDefinition: defs[i],
			Limits:        withEnforcement(jobs.EffectiveLimits(&defs[i], settings, env), placement, held),
			Paused:        !defs[i].Enabled && defs[i].Schedule != "",
		}
		if locked && e.Schedule != "" && !e.Paused {
			e.HeldByLock, e.NextRunAt = true, nil
		}
		out = append(out, e)
	}

	// The capability state travels with the list so the UI can explain why the
	// tab is inert rather than showing an empty page that looks broken, and
	// says where runs go — on the cluster or on this machine.
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":              out,
		"jobs_available":    caps.BackgroundJobsAvailable(),
		"unavailable_bec":   caps.ExplainJobsUnavailable(modeOf(app)),
		"placement":         jobPlacementWord(caps),
		"execution_mode":    app.ExecutionMode,
		"concurrency_limit": settings.JobConcurrency,
		"max_queued":        settings.MaxQueuedRunsPerApp,
	})
}

// withEnforcement adds how a job's memory and CPU are held where its runs go.
//
// The timeout is the platform's own clock in every mode, so it is always
// enforced — said rather than left absent, which read as unknown beside the
// other two (BB-010, second 2026-11-03 bash).
func withEnforcement(l jobs.Limits, mode runtime.Mode, held runtime.Containment) jobs.Limits {
	l.TimeoutSec.Enforcement = runtime.Enforced
	l.MemoryMB.Enforcement = runtime.Enforcement(mode, held, runtime.DialMemory)
	l.CPUMillicores.Enforcement = cpuEnforcement(mode, held, l.CPUMillicores.Value)
	return l
}

// jobPlacementWord says where an install places runs, in the words the UI and
// the CLI print.
func jobPlacementWord(caps runtime.Capabilities) string {
	if !caps.BackgroundJobsAvailable() {
		return ""
	}
	if caps.JobPlacement() == runtime.ModeIsolated {
		return "cluster"
	}
	return "this machine"
}

// handlePauseJob is PATCH /apps/{appID}/jobs/{jobID}: {"enabled": bool}
// pauses or resumes a job's schedule, an owner's or editor's, until the next
// deploy. Beside it, memory_limit_mb|_gb and cpu_limit_millicores|_cores set
// the job's own limits — an administrator's alone, 403 for anybody else on
// those fields only, so an owner can still pause. null or 0 clears one back
// to the install's job default. All or nothing: a body any part of which is
// refused applies none of it.
func (s *Server) handlePauseJob(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, auth.ActionEditContent)
	if app == nil {
		return
	}
	// Resuming a locked app's schedule would hand the lock's decision back to
	// a timetable; pausing is refused with it, so the lock is one answer.
	if s.refuseIfLocked(w, app) {
		return
	}
	var req struct {
		Enabled            *bool             `json:"enabled"`
		MemoryLimitMB      nullable[int]     `json:"memory_limit_mb"`
		MemoryLimitGB      nullable[float64] `json:"memory_limit_gb"`
		CPULimitMillicores nullable[int]     `json:"cpu_limit_millicores"`
		CPULimitCores      nullable[float64] `json:"cpu_limit_cores"`
	}
	resources := func() bool {
		return req.MemoryLimitMB.Set || req.MemoryLimitGB.Set || req.CPULimitMillicores.Set || req.CPULimitCores.Set
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || (req.Enabled == nil && !resources()) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "send {\"enabled\": false} to pause a job's schedule or true to resume it; an " +
				"administrator may also send memory_limit_mb or cpu_limit_millicores. Everything else " +
				"about a job is declared in toft.toml",
			"code": "bad_request", "retryable": false,
			"hint": "edit [jobs.<name>] in toft.toml and deploy"})
		return
	}
	p := auth.GetPrincipal(r.Context())
	if resources() && (p == nil || !p.IsAdmin()) {
		field := "memory_limit_mb"
		switch {
		case req.MemoryLimitGB.Set:
			field = "memory_limit_gb"
		case req.CPULimitMillicores.Set:
			field = "cpu_limit_millicores"
		case req.CPULimitCores.Set:
			field = "cpu_limit_cores"
		}
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "only an administrator can set a job's limits (" + field + "); nothing was changed",
			"code":  "forbidden", "field": field, "hint": "leave the limits out to change anything else, or ask an administrator to set them"})
		return
	}
	var memTo, cpuTo *int
	if req.MemoryLimitMB.Set || req.MemoryLimitGB.Set {
		v := 0
		if !storeQuantity(w, req.MemoryLimitMB.ptr(), req.MemoryLimitGB.ptr(), mebibytesPerGigabyte,
			"memory_limit_mb", "memory_limit_gb", minAppMemoryLimitMB, maxAppMemoryLimitMB, &v) {
			return
		}
		memTo = &v
	}
	if req.CPULimitMillicores.Set || req.CPULimitCores.Set {
		v := 0
		if !storeQuantity(w, req.CPULimitMillicores.ptr(), req.CPULimitCores.ptr(), millicoresPerCore,
			"cpu_limit_millicores", "cpu_limit_cores", minCPULimitMillicores, maxCPUMillicores, &v) {
			return
		}
		if reqm := s.envelope().AppCPURequestMillicores; v > 0 && v < reqm {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("a CPU limit of %s is below this install's CPU request of %s",
					coresPhrase(v), coresPhrase(reqm)), "hint": "pass a CPU limit at least as large as the request the sentence names, or 0 to clear it"})
			return
		}
		cpuTo = &v
	}

	def, err := s.db.GetJobDefinition(app.ID, chi.URLParam(r, "jobID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if def == nil {
		def, _ = s.db.GetJobDefinitionByName(app.ID, chi.URLParam(r, "jobID"))
	}
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job", "code": "no_such_job", "hint": "GET /api/v1/apps/{id}/jobs lists this app's jobs with their ids"})
		return
	}
	if memTo != nil || cpuTo != nil {
		meta := requestMeta(r.Context())
		actorType, actor := auditActor(p)
		if err := s.db.SetJobResources(app.ID, def.ID, memTo, cpuTo, actorType, actor, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
			log.Printf("Error setting job limits: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	if req.Enabled != nil {
		before := def.Enabled
		if err := s.db.SetJobEnabled(app.ID, def.ID, *req.Enabled, nextRunOnEnable(def, *req.Enabled)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		s.auditJob(r, "job.pause", app.ID, map[string]any{"job": def.Name, "enabled": before},
			map[string]any{"job": def.Name, "enabled": *req.Enabled})
	}
	def, _ = s.db.GetJobDefinition(app.ID, def.ID)
	writeJSON(w, http.StatusOK, def)
}

// nullable is a JSON field whose absence and whose null are two answers:
// absent leaves a figure alone, null clears it.
type nullable[T any] struct {
	Set   bool
	Value *T
}

func (n *nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// ptr is the field as storeQuantity takes it: nil when absent, and a null
// sent as 0, which clears.
func (n nullable[T]) ptr() *T {
	if !n.Set {
		return nil
	}
	if n.Value == nil {
		var zero T
		return &zero
	}
	return n.Value
}

// nextRunOnEnable is the schedule's next fire time after a pause or resume:
// none while paused, and the next tick from now on resume — never a backlog of
// the ticks it slept through.
func nextRunOnEnable(def *database.JobDefinition, enabled bool) *time.Time {
	if !enabled || def.Schedule == "" {
		return nil
	}
	at, err := schedule.Next(def.Schedule, time.Now().UTC())
	if err != nil {
		return nil
	}
	return &at
}

// --- Runs ---

type triggerRunRequest struct {
	Job    string          `json:"job"`
	Params json.RawMessage `json:"params,omitempty"`
	// ScheduledFor names the instant this run is for — a re-run of Tuesday's
	// work on Thursday says Tuesday. Absent is a run for nothing in particular,
	// never "now" (ADR §104). The app-facing start does not take it: an app
	// starting itself is acting now.
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`
}

// handleTriggerJobRun is a person or a script starting a run: POST
// /apps/{appID}/jobs/{job}/runs, or POST /apps/{appID}/job-runs with the job in
// the body. Owners and editors only — Run now in Toft's UI, `toft job run`, a
// pipeline's key. A viewer starts a job through the app's own button.
func (s *Server) handleTriggerJobRun(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	var req triggerRunRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStartBodyBytes)).Decode(&req); err != nil {
			if writeStartBodyTooLarge(w, err, false) {
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "code": "bad_request", "hint": badBodyHint})
			return
		}
	}
	if job := chi.URLParam(r, "job"); job != "" {
		req.Job = job
	}
	if req.Job == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "which job?", "code": "bad_request",
			"hint": "toft job list shows the jobs this app declares"})
		return
	}

	// Authorized before the job is looked up, so `no_such_job` against a real
	// name's refusal cannot tell a stranger which jobs the app declares.
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	// A job is a different entrypoint of the same app, so a locked app runs no
	// jobs. Without this the lock would stop the front door and leave a side
	// one open that runs the app's own code with the app's own credentials.
	if s.refuseIfLocked(w, app) {
		return
	}
	if def, err := s.db.GetJobDefinitionByName(app.ID, req.Job); err == nil && def == nil {
		s.writeStartFailure(w, fmt.Errorf("%w: this app's toft.toml declares no job called %q",
			jobs.ErrNoSuchJob, req.Job), app, req.Job, false)
		return
	}
	if s.jobs == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": s.capabilities().ExplainJobsUnavailable(modeOf(app)),
			"code":  "jobs_unavailable",
		})
		return
	}

	startedBy, actor := runStarter(p)
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	run, deduplicated, err := s.jobs.StartRun(jobs.StartRequest{
		AppID: app.ID, Job: req.Job, StartedBy: startedBy, ActorID: actor,
		Params: req.Params, ScheduledFor: req.ScheduledFor,
		IdempotencyKey: key, ExplicitKey: key != "",
	})
	if err != nil {
		s.writeStartFailure(w, err, app, req.Job, false)
		return
	}
	if !deduplicated {
		s.auditJob(r, "job_run.trigger", app.ID, nil, map[string]string{"job": req.Job, "run": run.ID})
	}
	s.decorateRun(app, run)
	status := http.StatusAccepted
	if deduplicated {
		status = http.StatusOK
	}
	// deduplicated is always present, false included (BB-010).
	writeJSON(w, status, struct {
		personRunDoc
		Deduplicated bool `json:"deduplicated"`
	}{personRun(run), deduplicated})
}

// runStarter is how a person's request reads in a run's history. A session is
// Toft's UI; any key — a person's, a service account's, a deploy key — is the
// CLI or a script. A deploy key stands for nobody, so its run names no person:
// `job_runs.actor_id` is a foreign key to users, and the audit row is where the
// key is named.
func runStarter(p *auth.Principal) (startedBy, actor string) {
	if p == nil {
		return database.JobStartedByCLI, ""
	}
	if p.IsDeployKey() {
		return database.JobStartedByCLI, ""
	}
	if p.APIKeyID != "" || p.Type == database.PrincipalTypeServiceAccount {
		return database.JobStartedByCLI, p.ID
	}
	return database.JobStartedByUI, p.ID
}

// handleRerunJobRun is POST /apps/{appID}/job-runs/{runID}/rerun: a new run of
// the same job with the same params and scheduled_for, recorded as a rerun of
// the original. Running something again is a deliberate act, and this is it —
// `toft job rerun`, and Run again in Toft's UI.
func (s *Server) handleRerunJobRun(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, auth.ActionEditContent)
	if app == nil {
		return
	}
	if s.refuseIfLocked(w, app) {
		return
	}
	if s.jobs == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": s.capabilities().ExplainJobsUnavailable(modeOf(app)), "code": "jobs_unavailable"})
		return
	}
	runID := chi.URLParam(r, "runID")
	if full, err := s.db.ResolveJobRunID(app.ID, runID); err == nil && full != "" {
		runID = full
	}
	startedBy, actor := runStarter(auth.GetPrincipal(r.Context()))
	run, err := s.jobs.Rerun(app.ID, runID, startedBy, actor)
	switch {
	case errors.Is(err, jobs.ErrNoSuchRun):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such run", "code": "not_found",
			"hint": "toft job runs lists this app's runs"})
		return
	case errors.Is(err, jobs.ErrRunStillOpen):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "run_still_open",
			"retryable": false, "hint": "toft job wait " + shortID(runID)})
		return
	case err != nil:
		s.writeStartFailure(w, err, app, "", false)
		return
	}
	s.auditJob(r, "job_run.rerun", app.ID, nil, map[string]string{"job": run.JobName, "run": run.ID, "rerun_of": runID})
	s.decorateRun(app, run)
	// deduplicated as a start's answer has it: a rerun is always a new run
	// (L7, 2026-11-04 black-box list).
	writeJSON(w, http.StatusAccepted, struct {
		personRunDoc
		Deduplicated bool `json:"deduplicated"`
	}{personRun(run), false})
}

// personRunDoc is a run as a person's routes answer it: database.JobRun with
// rerun_of, scheduled_for and failure_reason present and null when empty, as
// the app surface and the SDKs' Run objects carry them — absent on one
// surface and null on the other was two shapes of one run (L7).
type personRunDoc struct {
	*database.JobRun
	RerunOf       nullableText `json:"rerun_of"`
	ScheduledFor  *time.Time   `json:"scheduled_for"`
	FailureReason nullableText `json:"failure_reason"`
}

func personRun(run *database.JobRun) personRunDoc {
	return personRunDoc{JobRun: run, RerunOf: nullableText(run.RerunOf), ScheduledFor: run.ScheduledFor,
		FailureReason: nullableText(run.FailureReason)}
}

// shortID is the eight characters `toft job runs` prints, which every command
// that takes a run id accepts.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// decorateRun fills what a run reads as on every surface but is not a column:
// where it stands in the queue, and its two URLs.
func (s *Server) decorateRun(app *database.App, run *database.JobRun) {
	// ShownStatus, not Status: a claimed run reads as queued everywhere, so it
	// carries a position too rather than a queued run with none (BB-023).
	if run.ShownStatus() == database.JobRunQueued {
		if pos, err := s.db.QueuePosition(run); err == nil {
			run.QueuePosition = pos
		}
	}
	s.runInScheduleZone(run)
	run.StatusURL = runStatusPath(app, run.ID)
	run.LogsURL = "/api/v1/apps/" + app.ID + "/job-runs/" + run.ID + "/logs"
	// Last: `claimed` is bookkeeping, and to anyone reading a run it is still
	// queued.
	run.Status = run.ShownStatus()
}

// runInScheduleZone writes a run's scheduled_for in its schedule's zone when
// nobody named it in an offset of their own — the zone TOFT_SCHEDULED_FOR
// already carried (scheduledForIn), so a read and the job agree. Every read
// gave UTC, 15:00:00Z beside the job's 10:00:00-05:00 (D-08, 2026-11-04
// black-box list). An offset a person named is kept as given; a definition
// with no zone, or none any more, stays UTC.
func (s *Server) runInScheduleZone(run *database.JobRun) {
	if run.ScheduledFor == nil || database.ScheduledOffset(run.ScheduledFor) != "" {
		return
	}
	def, err := s.db.GetJobDefinition(run.AppID, run.DefinitionID)
	if err != nil || def == nil {
		return
	}
	zone, _ := schedule.Split(def.Schedule)
	if zone == "" {
		return
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return
	}
	in := run.ScheduledFor.In(loc)
	run.ScheduledFor = &in
}

func (s *Server) handleListJobRuns(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, logsAction)
	if app == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	q := r.URL.Query()
	if !writeBadRunStatus(w, q.Get("status")) {
		return
	}
	runs, err := s.db.ListJobRuns(app.ID, q.Get("job"), q.Get("status"), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if runs == nil {
		runs = []database.JobRun{}
	}
	// Where each queued run stands, and the two URLs. Computed here rather than
	// stored, because the position changes every time anything ahead starts.
	docs := make([]personRunDoc, len(runs))
	for i := range runs {
		s.decorateRun(app, &runs[i])
		docs[i] = personRun(&runs[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": docs})
}

func (s *Server) handleGetJobRun(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, logsAction)
	if app == nil {
		return
	}
	run, ok := s.lookupPersonRun(w, app, chi.URLParam(r, "runID"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, personRun(run))
}

// lookupPersonRun reads one run of the app for a person's route, by its full id or
// the eight-character prefix the CLI prints, and decorates it.
func (s *Server) lookupPersonRun(w http.ResponseWriter, app *database.App, runID string) (*database.JobRun, bool) {
	if full, err := s.db.ResolveJobRunID(app.ID, runID); err == nil && full != "" {
		runID = full
	}
	run, err := s.db.GetAppJobRun(app.ID, runID, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	if run == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found", "code": "not_found",
			"hint": "toft job runs lists this app's runs"})
		return nil, false
	}
	s.decorateRun(app, run)
	return run, true
}

// handlePersonJobRunOutput is GET /apps/{appID}/job-runs/{runID}/output: the
// run's output for a person or a script, the same answer the app surface gives.
func (s *Server) handlePersonJobRunOutput(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, logsAction)
	if app == nil {
		return
	}
	run, ok := s.lookupPersonRun(w, app, chi.URLParam(r, "runID"))
	if !ok {
		return
	}
	s.serveRunOutput(w, r, run)
}

func (s *Server) handleCancelJobRun(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, auth.ActionEditContent)
	if app == nil {
		return
	}
	runID := chi.URLParam(r, "runID")
	if full, err := s.db.ResolveJobRunID(app.ID, runID); err == nil && full != "" {
		runID = full
	}

	// The row is marked first and the workload stopped second. The other order
	// would leave a window where the workload is gone but the row still says
	// running, which the reconciler would then "recover" from. The external id
	// comes back from under the lock, not from a read before it: a pending run
	// can be claimed and started in between.
	changed, wasRunning, externalID, err := s.db.CancelJobRun(app.ID, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found", "code": "not_found",
				"hint": "toft job runs lists this app's runs"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if changed {
		s.stopCancelledRun(runID, wasRunning, externalID)
		s.auditJob(r, "job_run.cancel", app.ID, nil, map[string]string{"run": runID})
	}

	// The run as it now stands, and whether this call changed it. A run that
	// had already finished answers 200 with its own status and changed:false —
	// reporting "cancelled" over a run that succeeded is success claimed for
	// nothing.
	run, err := s.db.GetAppJobRun(app.ID, runID, "")
	if err != nil || run == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.decorateRun(app, run)
	writeJSON(w, http.StatusOK, struct {
		personRunDoc
		Changed bool `json:"changed"`
	}{personRun(run), changed})
}

func (s *Server) handleJobRunLogs(w http.ResponseWriter, r *http.Request) {
	app := s.jobAuth(w, r, logsAction)
	if app == nil {
		return
	}
	// Scoped by app: the logs endpoint must not become the way to read another
	// app's run output.
	run, ok := s.lookupPersonRun(w, app, chi.URLParam(r, "runID"))
	if !ok {
		return
	}

	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	lines, err := s.db.GetJobRunLogs(run.ID, after, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if lines == nil {
		lines = []database.JobLogLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "status": run.ShownStatus()})
}

// --- helpers ---

func modeOf(app *database.App) runtime.Mode { return runtime.Mode(app.ExecutionMode) }

// auditJob records a job mutation. Best-effort rather than transactional
// because the mutation itself already committed; the alternative is refusing
// the change because the audit write failed, which helps nobody.
func (s *Server) auditJob(r *http.Request, action, appID string, before, after any) {
	meta := requestMeta(r.Context())
	p := auth.GetPrincipal(r.Context())
	// Through auditActor rather than p.ID, so a deploy key is recorded as the
	// key it is instead of as an audit row with no actor at all. That is the
	// whole point of the class: a credential that belongs to nobody says so in
	// the log, and `deploy_keys.name` turns the id back into "the nightly
	// promotion pipeline".
	actorType, actor := auditActor(p)
	if err := s.db.WriteAuditRow(database.AuditEntry{
		ActorType: actorType, ActorID: actor,
		Action:     action,
		TargetType: "app", TargetID: appID,
		Before:    jsonString(before),
		After:     jsonString(after),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		log.Printf("jobs: audit %s: %v", action, err)
	}
}

func jsonString(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// stopCancelledRun asks the backend to stop a run whose row is already marked
// cancelled. Under its own context, not the request's: the row is marked, so a
// client that goes away before the backend answers must not leave the pod
// running to completion with a database that says it was stopped.
func (s *Server) stopCancelledRun(runID string, wasRunning bool, externalID string) {
	if !wasRunning || s.jobs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.jobs.Cancel(ctx, externalID); err != nil {
		log.Printf("jobs: stopping cancelled run %s: %v", runID, err)
	}
}

// handleScheduledOverview — GET /system/scheduled. Every schedule on the
// install, whichever app owns it: jobs, and the re-renders and restarts beside
// them. system_admin only, like /system/processes:
// it enumerates apps the caller may hold no grant on.
//
// Read-only, deliberately. A schedule is declared in its app's toft.toml,
// where it is reviewed and rolled back with the code; an admin page that could
// rewrite one from a list of names would be a second place it is defined.
func (s *Server) handleScheduledOverview(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	jobs, err := s.db.ListAllScheduledJobs()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if jobs == nil {
		jobs = []database.ScheduledJob{}
	}
	renders, err := s.db.ListAllRenderSchedules()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	restarts, err := s.db.ListAllRestartSchedules()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "renders": renders, "restarts": restarts})
}
