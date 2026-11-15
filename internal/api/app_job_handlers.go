package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/jobs"
)

// An app starting a different entrypoint of *itself*.
//
// Same repo, same release ref, same storage, different command. What makes this
// worth having is the case it replaces: a shared-mode app whose author wants to
// offer a twelve-minute, six-gigabyte report has three options today and all of
// them are bad — block the request, spawn a thread the framework's next rerun
// orphans, or refuse to build the feature. The first two put the work in
// Toft's shared process space beside four other people's apps. A job moves
// it into a sandbox with a memory limit, a deadline and a durable record.
//
// Nothing here is cross-app. There is no route by which app A starts work in
// app B: that is an authorization feature wearing a jobs costume, and routing it
// through B's job system would put the quota on B, the failure mail on B's
// owner, and a confused deputy in the middle. The composition already exists —
// B publishes a route, grants an account on itself, and calls start() on itself.
//
// Two rules govern every handler in this file.
//
//   - **The token is the app.** No route takes an app id, so none of these
//     calls auth.Authorize on the app. But they take a *run* id, which puts the
//     standing IDOR rule back in force: every query scopes by
//     app_id = <the token's app> in the query itself, never as a check after
//     the read.
//   - **A viewer is resolved, never asserted.** Where an act-for token is
//     present the platform verifies its own signature and looks the person up
//     itself. An app's claim about who is looking at it is not an input.

// appJobRunResponse is what an app sees of a run — the run fields every
// surface shares, under the same snake_case names.
//
// Deliberately not database.JobRun: an app is not owed the actor (it must
// never learn who its viewer is), the release ref, or the idempotency key —
// which for a derived key is a hash of the actor. started_by says *how* a run
// started, and `viewer` names nobody.
type appJobRunResponse struct {
	ID        string `json:"id"`
	Job       string `json:"job"`
	Status    string `json:"status"`
	StartedBy string `json:"started_by"`
	// RerunOf, ScheduledFor and FailureReason are null when empty — present,
	// as the SDKs' Run objects document them (BB-030, fourth 2026-11-03
	// black-box list).
	RerunOf nullableText    `json:"rerun_of"`
	Params  json.RawMessage `json:"params"`

	// ScheduledFor is the instant the run is for — a scheduled run's tick, or
	// one a person named (ADR §104). Absent when nobody gave one.
	ScheduledFor *time.Time `json:"scheduled_for"`
	QueuedAt     time.Time  `json:"queued_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	// Deadline is when the run will be stopped if it is still going.
	Deadline *time.Time `json:"deadline,omitempty"`

	Progress        int    `json:"progress"`
	ProgressMessage string `json:"progress_message,omitempty"`
	// QueuePosition is 1-based while queued and absent once the run starts.
	QueuePosition int `json:"queue_position,omitempty"`

	// HasOutput says an output object exists to fetch. False both for a run
	// that produced none and for one whose artifact has expired — /output tells
	// those apart, because only it needs to.
	HasOutput     bool         `json:"has_output"`
	ExitCode      *int         `json:"exit_code,omitempty"`
	FailureStage  string       `json:"failure_stage,omitempty"`
	FailureReason nullableText `json:"failure_reason"`
	PeakMemoryMB  *int         `json:"peak_memory_mb,omitempty"`
	// CPUThrottledSeconds is absent where the run's cgroup kept no counter.
	// ExecutionMode and DefinitionID are what the person-facing read carries
	// too; only started_by_user is withheld from an app (BB-076).
	CPUThrottledSeconds *float64 `json:"cpu_throttled_seconds,omitempty"`
	ExecutionMode       string   `json:"execution_mode,omitempty"`
	DefinitionID        string   `json:"definition_id,omitempty"`

	// StatusURL is the run's live status page on the app's own origin — what
	// respond(run) points a browser at. LogsURL is the run's log on the app
	// surface.
	StatusURL string `json:"status_url"`
	LogsURL   string `json:"logs_url"`

	// Deduplicated says a start returned a run that already existed. Reported
	// rather than silent: an app that appeared to ignore a click is
	// indistinguishable from a broken one. Always present on a start's answer,
	// false included, so a caller never has to read absence as an answer.
	Deduplicated bool `json:"deduplicated"`
}

// nullableText is a string the wire carries as null when it is empty.
type nullableText string

func (t nullableText) MarshalJSON() ([]byte, error) {
	if t == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(t))
}

func (t *nullableText) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*t = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*t = nullableText(s)
	return nil
}

// runStatusPath is a run's live status page, served by the gateway on the
// app's own origin: under the app's base path, so it carries the viewer's
// session exactly as the app's own pages do.
//
// **The serving release's base path, not one built from the slug.** An app at
// the root of a hostname of its own is served with an empty prefix, and the
// gateway matches the segment against that prefix — so `/apps/{slug}/__toft/…`
// on that hostname was proxied to the app and 404ed. The SDKs' `url()` joins
// this onto the app's public address, and the two must describe one origin.
func runStatusPath(app *database.App, runID string) string {
	return builder.ServingBasePath(app) + runStatusSegment + runID
}

// runStatusSegment is the gateway's own path inside an app's base path. A
// segment no framework routes, intercepted before the request is proxied.
const runStatusSegment = "/__toft/runs/"

func (s *Server) appRunResponse(run *database.JobRun, deduplicated bool) appJobRunResponse {
	// In the schedule's zone, as a person's read and TOFT_SCHEDULED_FOR (D-08).
	s.runInScheduleZone(run)
	out := appJobRunResponse{
		ID:                  run.ID,
		Job:                 run.JobName,
		Status:              run.ShownStatus(),
		StartedBy:           run.StartedBy,
		RerunOf:             nullableText(run.RerunOf),
		Params:              run.Params,
		ScheduledFor:        run.ScheduledFor,
		QueuedAt:            run.QueuedAt,
		StartedAt:           run.StartedAt,
		FinishedAt:          run.FinishedAt,
		Deadline:            run.DeadlineAt,
		Progress:            run.Progress,
		ProgressMessage:     run.ProgressMessage,
		HasOutput:           run.OutputKey != "",
		ExitCode:            run.ExitCode,
		FailureStage:        run.FailureStage,
		FailureReason:       nullableText(run.FailureReason),
		PeakMemoryMB:        run.PeakMemoryMB,
		CPUThrottledSeconds: run.CPUThrottledSeconds,
		ExecutionMode:       run.ExecutionMode,
		DefinitionID:        run.DefinitionID,
		LogsURL:             "/api/v1/app/job-runs/" + run.ID + "/logs",
		Deduplicated:        deduplicated,
	}
	if app, err := s.db.GetApp(run.AppID); err == nil && app != nil {
		out.StatusURL = runStatusPath(app, run.ID)
	}
	if run.ShownStatus() == database.JobRunQueued {
		if pos, err := s.db.QueuePosition(run); err == nil {
			out.QueuePosition = pos
		}
	}
	return out
}

// --- The act-for token, on the receiving side ---

// appViewer resolves the act-for header, if one was relayed.
//
// Three returns, and the middle one carries the whole design: (viewer, present,
// ok). *Absent* is not an error — a call with no act-for header is the app
// acting as itself, which is a first-class case. A header that is present and
// bad is refused, and the handler stops: an app that relayed something it could
// not have minted must not silently fall through to acting as itself, because
// that would move a run from a person's name to nobody's and nobody would
// notice until an audit.
// It refuses an anonymous blob, and every caller here wants that. See
// [Server.appViewerAny] for the one that does not.
func (s *Server) appViewer(w http.ResponseWriter, r *http.Request, appID string) (*auth.ActFor, bool, bool) {
	viewer, present, ok := s.appViewerAny(w, r, appID)
	if !ok || !present || !viewer.Anonymous {
		return viewer, present, ok
	}
	// An anonymous act-for names nobody, so there is nobody to act as. This is
	// the visible half of the change that introduced it: a page that started a
	// job "as the viewer where possible" used to relay nothing for a stranger,
	// and relaying nothing meant the app acting as itself. Now it relays a blob
	// that says *nobody in particular* and the start is refused — which is the
	// correct behaviour, and it says so rather than failing as a generic
	// authorization error nobody could trace back to this.
	//
	// The hint names the SDK call that does it as the app, because "the app's
	// own credential" is not something app code holds in its hand: it is the
	// job surface reached without the viewer (drills D5, 2026-10-28).
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "this request came from a visitor who is not signed in, so there is no " +
			"person to act for. Use the app's own credential to do this as the app, or " +
			"ask the visitor to sign in.",
		"code":      "viewer_anonymous",
		"retryable": false,
		"hint":      viewerAnonymousHint(r)})
	return nil, true, false
}

// viewerAnonymousHint is the fix for the route that was asked: one hint for
// every job route told a visitor's run listing to "start it as the app"
// (BB-022, second 2026-11-03 bash).
func viewerAnonymousHint(r *http.Request) string {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/job-runs"):
		return "a visitor who is not signed in has no runs of their own: list the app's runs " +
			"without the viewer (toft.jobs.list, toft.Jobs.List, job_list without a viewer) — " +
			"or ask the visitor to sign in"
	case r.Method == http.MethodGet:
		return "read the run as the app: call the job surface without the viewer (toft.jobs.get, " +
			"toft.Jobs.Get, job_get without a viewer) — or ask the visitor to sign in"
	case strings.HasSuffix(r.URL.Path, "/cancel"):
		return "cancel it as the app: call the job surface without the viewer — or ask the " +
			"visitor to sign in"
	}
	return "start it as the app: call the job surface without the viewer (toft.jobs.start, " +
		"toft.Jobs.Start, job_start without a viewer) — or ask the visitor to sign in"
}

// appViewerAny is [Server.appViewer] without the anonymous refusal, for the
// route where *nobody in particular* is an answer rather than a failure: the
// viewer identity route, which says 204. It had a second caller — the AI route
// attributed spend to a person when there was one and did not otherwise — and
// the split survives that route rather than collapsing back into one function.
//
// The split is deliberately this way round — the refusing one is the default
// spelling — so that a route added later fails closed by forgetting rather than
// by remembering. That argument is about the *next* caller, so it does not stop
// being worth the seam when there is only one.
func (s *Server) appViewerAny(w http.ResponseWriter, r *http.Request, appID string) (*auth.ActFor, bool, bool) {
	blob := r.Header.Get(auth.ActForHeader)
	if blob == "" {
		return nil, false, true
	}

	viewer, err := s.jwt.VerifyActFor(blob, appID)
	if err != nil {
		// Each with its next step (D-105: act_for_invalid reached all four
		// SDKs with hint null, the one refusal of the three an app author can
		// only fix in their own code).
		code, status := "act_for_invalid", http.StatusUnauthorized
		hint := "pass the " + auth.ActForHeader + " header of the viewer's request to this app through " +
			"unchanged (give the SDK the request's headers) — or call without the viewer to act as the app"
		switch {
		case errors.Is(err, auth.ErrActForExpired):
			// Named, because it is the one failure the app can render usefully:
			// nothing is broken, the person's session ended and they sign in
			// again.
			code = "act_for_expired"
			hint = "ask the viewer to open the app again, and act for them with the token their next request carries"
		case errors.Is(err, auth.ErrActForWrongApp):
			code = "act_for_wrong_app"
			status = http.StatusForbidden
			hint = "act for a viewer only with the " + auth.ActForHeader + " header of a request to this app — " +
				"a token another app received is that app's"
		}
		writeJSON(w, status, map[string]string{"error": err.Error(), "code": code, "hint": hint})
		return nil, true, false
	}

	// The signature says this blob is ours and names a session. Whether that
	// session is still alive is a separate question with a separate answer, and
	// it is the one that makes the long expiry safe.
	if !auth.CheckActForSession(s.db, viewer) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": auth.ErrActForExpired.Error(), "code": "act_for_expired", "hint": "ask the viewer to open the app again, and act for them with the token their next request carries"})
		return nil, true, false
	}
	return viewer, true, true
}

// principalForViewer builds the principal authorization actually runs against.
//
// This is the point of the whole act-for mechanism: the platform holds the real
// principal at the moment it decides. Role and groups are read live from the
// database rather than carried in the blob, so a demotion between the button
// appearing and the button being pressed applies.
func (s *Server) principalForViewer(viewer *auth.ActFor) (*auth.Principal, error) {
	p, err := s.db.PrincipalForUser(viewer.UserID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("user not found")
	}
	return p, nil
}

// --- Start ---

// maxStartBodyBytes bounds a start's body: parameters, not data. Large input
// goes into the app's storage first and its name is passed as a parameter.
const maxStartBodyBytes = 64 << 10

// writeStartBodyTooLarge answers a start body over maxStartBodyBytes with a
// refusal that names the cap (BB-022) — 413 params_too_large rather than the
// "must be a JSON object" a truncated read otherwise decodes into. False when
// err is not the cap, so the caller reports its own malformed-body error.
func writeStartBodyTooLarge(w http.ResponseWriter, err error, fromApp bool) bool {
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		return false
	}
	hint := "put the input in the app's storage and pass its name as a parameter"
	if fromApp {
		hint = "write the input to the app's storage first and pass its name in the start's params"
	}
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
		"error": fmt.Sprintf("a start's params are capped at %d KB — params are a run's "+
			"settings, not its data", maxStartBodyBytes>>10),
		"code": "params_too_large", "retryable": false, "limit_bytes": maxStartBodyBytes,
		"hint": hint})
	return true
}

// handleAppStartJobRun is POST /api/v1/app/jobs/{name}/runs: the app starting
// one of its own jobs, as itself or — with the act-for header relayed — for the
// signed-in person in front of it.
//
// **The platform does not gate a viewer's start by role.** A viewer cannot call
// this route: the act-for token exists only on a request the gateway passed to
// the app, so every viewer start goes through the app's own code, and the app's
// own button is the gate (it checks toft.who / toft.caller first). A second,
// platform-side gate duplicated that check and was the one people got wrong.
// What the platform still does is resolve the person itself, attribute the run
// to them, and refuse a blob it did not mint.
func (s *Server) handleAppStartJobRun(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	appID := token.Token.AppID

	app, err := s.db.GetApp(appID)
	if err != nil || app == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	jobName := chi.URLParam(r, "name")
	var body struct {
		Params json.RawMessage `json:"params,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStartBodyBytes)).Decode(&body); err != nil {
			if writeStartBodyTooLarge(w, err, true) {
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "the body must be a JSON object like {\"params\": {...}}; large input goes " +
					"into the app's storage and its name is passed as a parameter",
				"code": "bad_request", "hint": `send {"params": {"name": "value"}}, and put large input in the app's storage`})
			return
		}
	}

	viewer, present, ok := s.appViewer(w, r, appID)
	if !ok {
		return
	}
	startedBy, actorID := database.JobStartedByApp, ""
	if present {
		p, err := s.principalForViewer(viewer)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		startedBy, actorID = database.JobStartedByViewer, p.ID
	}

	if s.jobs == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": s.capabilities().ExplainJobsUnavailable(modeOf(app)),
			"code":  "jobs_unavailable"})
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	explicit := key != ""
	if !explicit {
		key = derivedIdempotencyKey(appID, jobName, body.Params, actorID)
	}

	run, deduplicated, err := s.jobs.StartRun(jobs.StartRequest{
		AppID: appID, Job: jobName, StartedBy: startedBy, ActorID: actorID,
		Params: body.Params, IdempotencyKey: key, ExplicitKey: explicit,
	})
	if err != nil {
		s.writeStartFailure(w, err, app, jobName, true)
		return
	}

	status := http.StatusAccepted
	if deduplicated {
		// 200 rather than 202: nothing was accepted for processing, and the
		// distinction is the honest half of reporting the dedup at all.
		status = http.StatusOK
	}
	writeJSON(w, status, s.appRunResponse(run, deduplicated))
}

// writeStartFailure turns an enqueue error into the refusal an app, a person or
// a script can act on — every one with a hint that is a command to paste.
//
// fromApp is the app's own route, reached through an SDK: a param hint there is
// about the start's params, never `toft job run …`, which app code cannot run
// and which read as the fix to code calling jobs.start (drills, 2026-10-23).
//
// app chooses the no_such_job hint: a static app is told it runs no jobs, and
// every other app is given the [jobs] advice alone (drills D14, 2026-10-28).
func (s *Server) writeStartFailure(w http.ResponseWriter, err error, app *database.App, job string, fromApp bool) {
	var paramErr *jobs.ParamError
	switch {
	case errors.Is(err, jobs.ErrNoSuchJob):
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": err.Error(), "code": "no_such_job", "retryable": false,
			"hint": noSuchJobHint(app, job)})
	case errors.Is(err, jobs.ErrJobsUnavailable):
		// Well-formed request, and what is wrong is a decision somebody has to
		// make. The explanation is install-level and names the remedy.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": err.Error(), "code": "jobs_unavailable"})
	case errors.Is(err, jobs.ErrAppLocked):
		// Normally refused earlier — a locked app is stopped, and a stopped
		// app's token is refused — but the stop is best-effort, and this is the
		// answer when it did not happen.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": err.Error(), "code": "app_locked"})
	case errors.Is(err, database.ErrQueueLimit):
		writeQueueFull(w, err, app)
	case errors.Is(err, jobs.ErrIdempotencyKeyReused):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "code": "idempotency_key_reused", "retryable": false,
			"hint": "a key is matched for 24 hours; send a new Idempotency-Key for different work"})
	case errors.As(err, &paramErr):
		hint := paramErr.Hint
		switch {
		// A refusal about the whole params value names no parameter, so its
		// own hint stands — never "--param =" (BB-082).
		case paramErr.Param == "" && hint != "":
		case fromApp && paramErr.Code == "param_unknown":
			hint = "[jobs." + job + ".params] in toft.toml declares the parameters this job takes"
		// A value of the wrong type is told the type, not offered a choice
		// as if it had picked the wrong one (docs/bugs 2026-10-28 #7).
		case paramErr.Code == "param_type" && paramErr.Type != "" && fromApp:
			hint = "the parameter " + paramErr.Param + " is declared as " + paramErr.Type + " — pass one, e.g. " +
				paramErr.Param + "=" + paramErr.Example
		case paramErr.Code == "param_type" && paramErr.Type != "":
			hint = "the parameter " + paramErr.Param + " is declared as " + paramErr.Type + ": toft job run " + job +
				" --param " + paramErr.Param + "=" + paramErr.Example
		// A value outside the choices is told to pick one, not read as a
		// param left out (drills 2026-10-30) — the sentence lists them.
		case fromApp && paramErr.Code == "param_choice":
			hint = "pass one of the choices " + paramErr.Param + " declares, e.g. " + paramErr.Param + "=" + paramErr.Example
		case fromApp:
			hint = "pass " + paramErr.Param + " in the start's params, e.g. " + paramErr.Param + "=" + paramErr.Example
		case hint == "":
			hint = "toft job run " + job + " --param " + paramErr.Param + "=" + paramErr.Example
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": paramErr.Msg, "code": paramErr.Code, "retryable": false, "hint": hint})
	default:
		log.Printf("app jobs: starting: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

// noSuchJobHint is the remedy for a job the app does not declare. A static app
// is served as files and runs no jobs, so declaring one would not help it.
func noSuchJobHint(app *database.App, job string) string {
	if app != nil && app.Framework == frameworkStatic {
		return "this is a static app, which runs no jobs — a job needs an app with a server or a script"
	}
	return "declare [jobs." + job + "] in toft.toml and deploy; toft job list shows the jobs this app has"
}

// writeQueueFull is the one answer to a full queue, on the app's route and a
// person's alike. Runs beyond the app's runs-at-once limit queue rather than
// being refused, so this is reached only when the queue itself is at the
// install's cap. The hint names the app by its slug — a literal <app> was
// one more thing to fill in (D-36).
func writeQueueFull(w http.ResponseWriter, err error, app *database.App) {
	slug := "<app>"
	if app != nil && app.Slug != "" {
		slug = app.Slug
	}
	w.Header().Set("Retry-After", "30")
	body := map[string]any{
		"error":       "this app's queue is full: " + strings.TrimPrefix(err.Error(), database.ErrQueueLimit.Error()+": "),
		"code":        "queue_full",
		"retryable":   true,
		"retry_after": 30,
		"hint":        "toft job runs --app " + slug + " shows what is queued",
	}
	// The numbers the SDKs' QueueFull carries (in_flight, limit).
	var ql *database.QueueLimitError
	if errors.As(err, &ql) {
		body["in_flight"], body["limit"] = ql.Queued, ql.Limit
	}
	writeJSON(w, http.StatusTooManyRequests, body)
}

// derivedIdempotencyKey is D6's default: the key a caller does not have to
// think about.
//
// Streamlit *guarantees* the double-fire — a rerun re-executes the script top
// to bottom, so a start() in a button branch fires again on the next widget
// change — so the naive call has to be correct without a key. Both other
// platforms in this space make the caller remember one; here that is the
// override, not the requirement.
//
// The resolved **user** is in the key, not the session id. A session is
// per-login, so keying on it would fail to deduplicate across two tabs, and
// would give one run more than one owner if it ever coalesced across people.
// Including the actor also means cross-user coalescing, if it is ever built, is
// a widening of this key rather than a change in what it means.
func derivedIdempotencyKey(appID, jobName string, params json.RawMessage, actorID string) string {
	h := sha256.New()
	h.Write([]byte(appID))
	h.Write([]byte{0})
	h.Write([]byte(jobName))
	h.Write([]byte{0})
	h.Write([]byte(canonicalJSON(params)))
	h.Write([]byte{0})
	h.Write([]byte(actorID))
	return "d:" + hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON renders params so that two equal parameter sets hash equal.
//
// Key order is not meaningful in JSON, and a Python dict serialized twice can
// come out either way round; without this, the double-fire the key exists to
// absorb would slip through half the time. Anything that will not parse is
// hashed verbatim — a caller sending malformed JSON has a different problem,
// and it is not this function's to report.
func canonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	var b strings.Builder
	writeCanonical(&b, v)
	return b.String()
}

func writeCanonical(b *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			enc, _ := json.Marshal(k)
			b.Write(enc)
			b.WriteByte(':')
			writeCanonical(b, t[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, e)
		}
		b.WriteByte(']')
	default:
		enc, _ := json.Marshal(t)
		b.Write(enc)
	}
}

// --- Reads ---

// handleAppListJobRuns answers either "what has this app run" or "what has the
// person looking at it run".
//
// scope=viewer is what makes coming back tomorrow work without the app storing
// anything per-user. The app relays the act-for token, the platform resolves it
// and filters on the resolved user — so **the app gets that person's runs and
// still never learns who they are**. Identity as a scoping capability rather
// than a fact the app holds.
// listableRunStatuses are the statuses a run list filters by: every status a
// run is shown with. `claimed` is not among them — it reads as queued.
var listableRunStatuses = []string{
	database.JobRunQueued, database.JobRunRunning, database.JobRunSucceeded,
	database.JobRunFailed, database.JobRunTimedOut, database.JobRunOutOfMemory,
	database.JobRunCancelled, database.JobRunSkipped,
}

// writeBadRunStatus refuses a status filter no run can have — a typo answered
// with 200 and an empty list reads as "no such runs", which is a lie. True
// when the filter is empty or valid and the caller should go on.
func writeBadRunStatus(w http.ResponseWriter, status string) bool {
	if status == "" {
		return true
	}
	for _, ok := range listableRunStatuses {
		if status == ok {
			return true
		}
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": fmt.Sprintf("no run has the status %q", status), "code": "bad_request",
		"hint": "status is one of " + strings.Join(listableRunStatuses, ", ")})
	return false
}

func (s *Server) handleAppListJobRuns(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	appID := token.Token.AppID

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobName := r.URL.Query().Get("job")
	status := r.URL.Query().Get("status")
	if !writeBadRunStatus(w, status) {
		return
	}

	var runs []database.JobRun
	var err error

	// A relayed act-for header is checked on every listing, not only on
	// scope=viewer: a viewer's list is a viewer call, so a blob that names
	// nobody is refused viewer_anonymous like the get, the start and the
	// cancel. It answered the app's runs to an anonymous visitor while every
	// other call on the same viewer was refused (drills 2026-11-03).
	viewer, present, ok := s.appViewer(w, r, appID)
	if !ok {
		return
	}

	if r.URL.Query().Get("scope") == "viewer" {
		if !present {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "scope=viewer needs the " + auth.ActForHeader + " header this app " +
					"received on the request it is serving — without it there is no viewer " +
					"to scope to",
				"code": "act_for_required",
				"hint": "relay the " + auth.ActForHeader + " header from the viewer's request — the SDK's viewer-scoped call does — or drop scope=viewer"})
			return
		}
		// Authorization stays live and is not skipped for a read: if she lost
		// read on this app overnight the list is empty, because the filter runs
		// against a principal the platform resolved itself.
		p, perr := s.principalForViewer(viewer)
		if perr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		app, aerr := s.db.GetApp(appID)
		if aerr != nil || app == nil || !auth.CanRead(p, objectFromApp(app), s.db) {
			writeJSON(w, http.StatusOK, map[string]any{"runs": []appJobRunResponse{}})
			return
		}
		runs, err = s.db.ListActorJobRuns(appID, viewer.UserID, jobName, status, limit)
	} else {
		runs, err = s.db.ListJobRuns(appID, jobName, status, limit)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := make([]appJobRunResponse, 0, len(runs))
	for i := range runs {
		out = append(out, s.appRunResponse(&runs[i], false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// appRun fetches one run, scoped by the token's app and — where a viewer is
// relayed — by that viewer.
//
// The app scope is the IDOR rule: this route takes an id from the URL, so the
// query scopes by the app rather than checking after the read. The viewer scope
// on top of it is what stops one person reading another's run through an app
// that never knew them apart.
func (s *Server) appRun(w http.ResponseWriter, r *http.Request) (*database.JobRun, *auth.ActFor, bool) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return nil, nil, false
	}

	viewer, _, ok := s.appViewer(w, r, token.Token.AppID)
	if !ok {
		return nil, nil, false
	}

	// The viewer scope is in the query: a run another viewer started does not
	// come back, so no reader has to remember to compare.
	actorID := ""
	if viewer != nil {
		actorID = viewer.UserID
	}
	run, err := s.db.GetAppJobRun(token.Token.AppID, chi.URLParam(r, "runID"), actorID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, nil, false
	}
	if run == nil {
		// 404 over 403, the standing rule for an object this caller may not
		// read — and for a run somebody else started, whose existence is
		// theirs.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no such run", "code": "not_found",
			"hint": "pass an id that starting or listing this app's runs returned (jobs.start/jobs.list; Jobs.Start/Jobs.List in Go, job_start/job_list in R); a run another person started is theirs to read"})
		return nil, nil, false
	}
	return run, viewer, true
}

func (s *Server) handleAppGetJobRun(w http.ResponseWriter, r *http.Request) {
	run, _, ok := s.appRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.appRunResponse(run, false))
}

func (s *Server) handleAppJobRunLogs(w http.ResponseWriter, r *http.Request) {
	run, _, ok := s.appRun(w, r)
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

// handleAppJobRunOutput redirects to a presigned GET of the run's output.
//
// The platform's own object store, not an app storage mount — reading an
// output back costs no additional configuration. What was
// missing was only the GET: the PUT half has been presigned at bootstrap all
// along.
//
// The two failure modes are told apart deliberately. A run that produced no
// artifact is a 404. A run that produced one which has since expired is a 410
// saying so, because rendering a download that 404s is worse than saying the
// artifact is gone — status outliving its payload is the failure shape this
// whole channel is arranged to avoid.
func (s *Server) handleAppJobRunOutput(w http.ResponseWriter, r *http.Request) {
	run, _, ok := s.appRun(w, r)
	if !ok {
		return
	}
	s.serveRunOutput(w, r, run)
}

// serveRunOutput answers for one run's output: a redirect to a presigned GET,
// 404 when the run wrote none, 410 when it wrote one that has expired.
func (s *Server) serveRunOutput(w http.ResponseWriter, r *http.Request, run *database.JobRun) {

	presigner, canPresign := artifact.PresignerOf(s.artifacts)
	if s.artifacts == nil || !canPresign {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "this install's artifact store cannot presign URLs, so a run's output " +
				"cannot be read back",
			"code": "no_object_store", "hint": "ask the operator who runs this install to configure an object store (ARTIFACT_STORE=s3); the run's logs are still readable"})
		return
	}

	if run.OutputKey == "" {
		// Nothing was ever recorded. Distinguishable from an expired artifact
		// precisely because output_key is only set when a run actually wrote
		// one.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this run produced no output — an output exists only when the job " +
				"wrote one with run.write_output(...) or run.write_output_json(...)",
			"code": "no_output",
			"hint": "toft job logs shows what the run printed instead; to keep a result, have the job write it with write_output (writeOutput in Node, WriteOutput in Go, run_write_output in R)"})
		return
	}

	url, err := presigner.PresignGet(r.Context(), run.OutputKey, artifact.PresignTTL)
	if err != nil {
		// A store that cannot presign at all says so in its own words, which
		// name the remedy (ARTIFACT_STORE=s3). Reporting that as a 500 would
		// send an operator to look for a fault in a working install.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": err.Error(), "code": "no_object_store", "hint": "ask the operator who runs this install to configure an object store (ARTIFACT_STORE=s3); the run's logs are still readable"})
		return
	}

	// Asked *after* signing, because signing is local and cheap while this is a
	// round trip to the store. The two answers are deliberately different: a
	// run that produced nothing is a 404, and a run whose artifact has since
	// gone is a 410 that says *expired*. Rendering a download that then 404s is
	// the failure this whole channel is arranged to avoid — status outliving
	// its payload.
	if _, err := s.artifacts.Stat(r.Context(), run.OutputKey); err != nil {
		// Gone and unreachable are two answers: a store blip read as
		// "expired, run it again" sent people re-running jobs whose output
		// was there (code review 2026-11-11, H-7).
		if !errors.Is(err, artifact.ErrNotFound) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "the object store did not answer, so this run's output cannot be fetched right now",
				"code":  "store_unavailable", "hint": "try again shortly; the output has not expired"})
			return
		}
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "this run's output has expired — an output object lives as long as the " +
				"run row that explains it, and this one's has been trimmed",
			"code": "output_expired", "hint": "start the run again to produce new output"})
		return
	}
	// 302 rather than proxying the bytes: the platform is not in the data path
	// for a release either, and a report is exactly the thing that would make
	// it one.
	http.Redirect(w, r, url, http.StatusFound)
}

// --- Cancel ---

// handleAppCancelJobRun stops a run.
//
// The rule, in full: a viewer may cancel a run they started, a viewer may not
// cancel someone else's, and the app token alone may cancel a run with no
// actor. Anything looser would let one person kill another's work through an
// app that never knew either of them apart.
func (s *Server) handleAppCancelJobRun(w http.ResponseWriter, r *http.Request) {
	run, viewer, ok := s.appRun(w, r)
	if !ok {
		return
	}
	token := appTokenFromContext(r.Context())

	// A relayed viewer was scoped by appRun's query: another person's run was
	// 404 before this line. What is left to refuse is the app token alone on
	// a run somebody started.
	// A finished run is answered as every finished run is — nothing to stop,
	// 204 — whoever started it: refusing the app a no-op on a person's run
	// told it act_for_required where an app-started run said "already
	// finished" (BB-021, second 2026-11-03 bash).
	if viewer == nil && run.ActorID != "" && !database.JobRunTerminal(run.Status) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this run was started on a person's behalf, so only that person can " +
				"cancel it — relay the " + auth.ActForHeader + " header from their request",
			"code": "act_for_required",
			"hint": "cancel it while serving that person's request, relaying their " + auth.ActForHeader + " header — the SDK's viewer-scoped cancel does — or let it finish"})
		return
	}

	_, wasRunning, externalID, err := s.db.CancelJobRun(token.Token.AppID, run.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// The row is marked first and the workload stopped second. The other order
	// leaves a window where the workload is gone and the row still says
	// running, which the reconciler would then "recover" from.
	s.stopCancelledRun(run.ID, wasRunning, externalID)
	w.WriteHeader(http.StatusNoContent)
}
