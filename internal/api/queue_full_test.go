package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/jobs"
)

// One refusal, one code, on both routes that start a run. A person's route sent
// a bare 429 the envelope named rate_limited ("slow down") while the app's said
// queue_full ("wait for a run in flight") — found by the drills on trytoft.dev.
func TestAFullQueueIsQueueFullWithWhatToDo(t *testing.T) {
	rec := httptest.NewRecorder()
	writeQueueFull(rec, &database.QueueLimitError{Queued: 50, Limit: 50}, &database.App{Slug: "books"})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on a full queue")
	}
	var body struct {
		Code       string `json:"code"`
		Error      string `json:"error"`
		Retryable  bool   `json:"retryable"`
		RetryAfter int    `json:"retry_after"`
		Hint       string `json:"hint"`
		InFlight   int    `json:"in_flight"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "queue_full" || !body.Retryable || body.RetryAfter == 0 || body.Hint == "" ||
		!strings.Contains(body.Error, "50 runs are waiting") {
		t.Errorf("body = %+v, want queue_full with the count, retry_after and a hint", body)
	}
	// Every SDK's QueueFull reads these two; without them it said 0 and 0.
	if body.InFlight != 50 || body.Limit != 50 {
		t.Errorf("in_flight = %d, limit = %d, want 50 and 50", body.InFlight, body.Limit)
	}
	// D-36: the hint names the app, not a literal <app>.
	if body.Hint != "toft job runs --app books shows what is queued" {
		t.Errorf("hint = %q", body.Hint)
	}
}

// A param refusal on the app's own route — an SDK's jobs.start — names the
// start's params; the person's route names the command. App code cannot run
// `toft job run …`, and offering it read as the fix (drills, 2026-10-23).
func TestAParamHintSpeaksToItsCaller(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		code    string
		fromApp bool
		want    string
	}{
		{"param_required", true, "pass region in the start's params, e.g. region=emea"},
		// A value outside the choices is told to pick one (drills 2026-10-30).
		{"param_choice", true, "pass one of the choices region declares, e.g. region=emea"},
		{"param_choice", false, "toft job run refresh --param region=emea"},
	} {
		perr := &jobs.ParamError{Code: tc.code, Msg: `parameter "region" …`, Param: "region", Example: "emea"}
		rec := httptest.NewRecorder()
		s.writeStartFailure(rec, perr, nil, "refresh", tc.fromApp)
		var body struct{ Hint, Code string }
		json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusBadRequest || body.Code != tc.code || body.Hint != tc.want {
			t.Errorf("fromApp=%v: %d %+v, want hint %q", tc.fromApp, rec.Code, body, tc.want)
		}
	}
}

// A static app reached with an undeclared job is told it runs no jobs, and any
// other app is given the [jobs] advice without a hedge about static apps
// (drills D14, 2026-10-28).
func TestNoSuchJobHintFollowsTheFramework(t *testing.T) {
	s := &Server{}
	err := fmt.Errorf("%w: no job called %q", jobs.ErrNoSuchJob, "refresh")
	for _, tc := range []struct {
		app            *database.App
		want, wantNone string
	}{
		{&database.App{Framework: frameworkStatic}, "static app, which runs no jobs", "declare [jobs."},
		{&database.App{Framework: "fastapi"}, "declare [jobs.refresh] in toft.toml and deploy", "static"},
		{nil, "declare [jobs.refresh]", "static"},
	} {
		rec := httptest.NewRecorder()
		s.writeStartFailure(rec, err, tc.app, "refresh", false)
		var body struct{ Hint, Code string }
		json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusNotFound || body.Code != "no_such_job" ||
			!strings.Contains(body.Hint, tc.want) || strings.Contains(body.Hint, tc.wantNone) {
			t.Errorf("app=%+v: %d %+v, want a hint with %q and without %q", tc.app, rec.Code, body, tc.want, tc.wantNone)
		}
	}
}
