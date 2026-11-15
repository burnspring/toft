package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
)

// A job's run token reaches the send routes and nothing else on the app surface,
// and only while its run is in flight (ADR §108).
func TestAJobMaySendAndDoNothingElse(t *testing.T) {
	f := newBootstrapFixture(t)
	run := &database.JobRun{AppID: f.app.ID, JobName: "report"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='running' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	tok, err := f.srv.jwt.MintRunToken(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}

	// Past authentication: the answer is the integration gate's — email is not
	// attached to this app — never an authentication refusal.
	send := call(http.MethodPost, "/api/v1/app/notify/email", `{"subject":"s","text":"t","to":["a@example.com"]}`)
	if send.Code == http.StatusUnauthorized || strings.Contains(send.Body.String(), "job_send_only") {
		t.Fatalf("a running job could not reach the send route: %d %s", send.Code, send.Body.String())
	}

	// The stores its app maps are within reach too — on a cluster the run's
	// pod mounts them, and where nothing is mounted this is the only way in.
	// No store is mapped here, so the answer is the store route's own, never
	// the job gate's.
	store := call(http.MethodGet, "/api/v1/app/store/warehouse/objects", "")
	if store.Code == http.StatusUnauthorized || strings.Contains(store.Body.String(), "job_send_only") {
		t.Fatalf("a running job could not reach the store route: %d %s", store.Code, store.Body.String())
	}

	data := call(http.MethodGet, "/api/v1/app/data", "")
	if data.Code != http.StatusForbidden || !strings.Contains(data.Body.String(), "job_send_only") {
		t.Errorf("app data with a run token = %d %s, want 403 job_send_only", data.Code, data.Body.String())
	}
	// The refusal is a rule, not a broken deploy, and says so: it names the job
	// and the run, and its hint carries the two ways round it (drills D9).
	var refusal struct {
		Error, Code, Hint string
		Retryable         *bool
	}
	if err := json.Unmarshal(data.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refusal.Error, `job "report"`) || !strings.Contains(refusal.Error, shortID(run.ID)) {
		t.Errorf("job_send_only does not name the job and the run: %q", refusal.Error)
	}
	if !strings.Contains(refusal.Hint, "service process") || !strings.Contains(refusal.Hint, "write_output_json") {
		t.Errorf("job_send_only hint = %q, want the two ways round it", refusal.Hint)
	}
	if refusal.Retryable == nil || *refusal.Retryable {
		t.Errorf("job_send_only retryable = %v, want false", refusal.Retryable)
	}

	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='succeeded' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	after := call(http.MethodPost, "/api/v1/app/notify/email", `{"subject":"s","text":"t"}`)
	if after.Code != http.StatusUnauthorized || !strings.Contains(after.Body.String(), "run_not_in_flight") {
		t.Errorf("a finished run's token = %d %s, want 401 run_not_in_flight", after.Code, after.Body.String())
	}
}

// A job recording its result mid-run leaves the run in flight; only the
// runner's own report at exit ends it. Before record_only, set_result finished
// the run as succeeded on the spot, so the job's later sends were refused and
// its later failure was lost.
func TestARecordOnlyResultLeavesTheRunInFlight(t *testing.T) {
	f := newBootstrapFixture(t)
	run := &database.JobRun{AppID: f.app.ID, JobName: "report"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='running' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	tok, _ := f.srv.jwt.MintRunToken(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/result", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("result = %d %s", rec.Code, rec.Body.String())
		}
	}

	post(`{"version":"2","result":{"rows":3},"exit_code":0,"record_only":true}`)
	got, _ := f.srv.db.GetJobRun(run.ID)
	if got.Status != database.JobRunRunning {
		t.Fatalf("after a record-only result: status %q; want running", got.Status)
	}

	post(`{"version":"2","exit_code":1,"stage":"app_exec","reason":"exit 1"}`)
	got, _ = f.srv.db.GetJobRun(run.ID)
	if got.Status != database.JobRunFailed {
		t.Errorf("after the runner's report: status %q; want failed", got.Status)
	}
}

// VN-5 (2026-11-15): a job's own code holds the run token and may post its
// result, so what it reports is bounded — the reason clipped and cleaned, the
// stage one the runner sends, the body itself capped.
func TestARunnerResultFromJobCodeIsBounded(t *testing.T) {
	f := newBootstrapFixture(t)
	start := func() (string, string) {
		run := &database.JobRun{AppID: f.app.ID, JobName: "report"}
		if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='running' WHERE id=?`, run.ID); err != nil {
			t.Fatal(err)
		}
		tok, _ := f.srv.jwt.MintRunToken(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
		return run.ID, tok
	}
	post := func(tok, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/result", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}

	id, tok := start()
	reason := strings.Repeat("x", 10000) + "\u0000"
	rec := post(tok, `{"version":"2","exit_code":1,"stage":"zzz","reason":"`+strings.Replace(reason, "\u0000", `\u0000`, 1)+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a long reason: %d %s, want 204", rec.Code, rec.Body.String())
	}
	got, err := f.srv.db.GetJobRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != database.JobRunFailed {
		t.Errorf("status %q, want failed", got.Status)
	}
	if len(got.FailureReason) > 4000 || strings.ContainsRune(got.FailureReason, 0) {
		t.Errorf("stored reason is %d bytes (NUL: %v), want at most 4000 and clean",
			len(got.FailureReason), strings.ContainsRune(got.FailureReason, 0))
	}
	if got.FailureStage != "app_exec" {
		t.Errorf("stored stage %q, want app_exec for a stage the runner never sends", got.FailureStage)
	}

	_, tok2 := start()
	huge := `{"version":"2","exit_code":1,"reason":"` + strings.Repeat("y", 2<<20) + `"}`
	if rec := post(tok2, huge); rec.Code != http.StatusBadRequest {
		t.Errorf("a 2 MiB report: %d, want 400", rec.Code)
	}
}
