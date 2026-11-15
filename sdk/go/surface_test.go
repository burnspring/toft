package toft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A retried request reuses its idempotency key, which is the whole reason the
// key is chosen per call rather than per attempt. Without it, this retry is
// precisely the thing that sends the weekly report twice.
func TestARetryReusesTheIdempotencyKey(t *testing.T) {
	var keys []string
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if len(keys) == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 503, map[string]any{"error": "the queue is restarting"})
			return
		}
		writeJSON(t, w, 202, map[string]any{"send_id": "snd_1", "status": "queued"})
	})

	send, err := Notify.Email(context.Background(), EmailMessage{
		To: []string{"finance@example.com"}, Subject: "Weekly report", Text: "1,204 rows",
	})
	if err != nil {
		t.Fatal(err)
	}
	if send.ID != "snd_1" || send.Status != "queued" {
		t.Errorf("Send = %+v", send)
	}
	if len(keys) != 2 {
		t.Fatalf("attempts = %d, want exactly one retry", len(keys))
	}
	if keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("keys = %v, want one key presented twice", keys)
	}
}

// The retry is exactly one, and a refusal the platform means is not retried at
// all: anything more here would be a second source of truth about how a send
// behaves.
func TestARefusalIsNotRetriedAndKeepsThePlatformsWords(t *testing.T) {
	var attempts int
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		writeJSON(t, w, 403, map[string]any{
			"code":  "capability_absent",
			"error": "email is not enabled for this app; an administrator can enable it on the app's Integrations tab",
		})
	})

	_, err := Notify.Email(context.Background(), EmailMessage{
		To: []string{"a@example.com"}, Subject: "s", Text: "t",
	})
	if attempts != 1 {
		t.Errorf("attempts = %d, want one — a 403 is not a blip", attempts)
	}
	platformErr, ok := AsError(err)
	if !ok {
		t.Fatalf("err = %v, want a platform error", err)
	}
	if !strings.Contains(platformErr.Error(), "Integrations tab") {
		t.Errorf("err = %q — the platform's own words name the remedy and were flattened away", err)
	}
	if platformErr.Code() != "capability_absent" {
		t.Errorf("Code = %q, want the stable name app code branches on", platformErr.Code())
	}
}

// A file post travels as multipart with the file under the field the route
// wants, and the filename is reduced to a basename on this side too.
func TestASlackFileIsMultipartAndCarriesNoDirectoryLayout(t *testing.T) {
	var gotName, gotTitle, gotField string
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatalf("Content-Type = %q, want multipart", r.Header.Get("Content-Type"))
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			switch part.FormName() {
			case "title":
				body, _ := io.ReadAll(part)
				gotTitle = string(body)
			default:
				gotField, gotName = part.FormName(), part.FileName()
			}
		}
		writeJSON(t, w, 202, map[string]any{"send_id": "snd_2", "status": "queued"})
	})

	_, err := Notify.SlackFile(context.Background(),
		Attachment{Name: "/home/someone/private/recon.xlsx", Content: []byte("x")},
		SlackFileOptions{Title: "Weekly reconciliation"})
	if err != nil {
		t.Fatal(err)
	}
	if gotField != "file" {
		t.Errorf("field = %q, want file", gotField)
	}
	if gotName != "recon.xlsx" {
		t.Errorf("filename = %q — a directory layout was about to be posted to a channel", gotName)
	}
	if gotTitle != "Weekly reconciliation" {
		t.Errorf("title = %q", gotTitle)
	}
}

// Which channel is not an argument, and mail has no from: an app supplies the
// message and the platform supplies the identity.
func TestASendCarriesNoRoutingFields(t *testing.T) {
	var body map[string]any
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		writeJSON(t, w, 202, map[string]any{"send_id": "snd_3", "status": "queued"})
	})

	if _, err := Notify.Email(context.Background(), EmailMessage{
		To: []string{" finance@example.com ", "  "}, Subject: "s", Text: "t",
	}); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"from", "reply_to", "channel"} {
		if _, present := body[forbidden]; present {
			t.Errorf("the request carried %q, which is the platform's to decide", forbidden)
		}
	}
	to, _ := body["to"].([]any)
	if len(to) != 1 || to[0] != "finance@example.com" {
		t.Errorf("to = %v, want the blank dropped and the address trimmed", body["to"])
	}
}

// A full queue is a refusal an app can render, not a status code.
func TestAFullQueueCarriesTheNumbers(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		writeJSON(t, w, 429, map[string]any{
			"code": "queue_full", "in_flight": 3, "limit": 3,
			"error": "3 runs are already in flight and 3 is the limit",
		})
	})

	_, err := Jobs.Start(context.Background(), "reconcile", StartOptions{})
	var full *QueueFullError
	if !errors.As(err, &full) {
		t.Fatalf("Start err = %v, want a QueueFullError", err)
	}
	if full.InFlight != 3 || full.Limit != 3 {
		t.Errorf("QueueFullError = %+v, want the counts an app puts on a page", full)
	}
}

// A run ends exactly once: Wait returns the run in its final state, and there
// is no chain to follow — the failure is the answer, with its reason.
func TestAWaitReturnsTheRunInItsFinalState(t *testing.T) {
	reads := 0
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/runs"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["payload"]; ok {
				t.Error("a start sent a payload; params are the only input")
			}
			writeJSON(t, w, 202, map[string]any{"id": "run_1", "job": "reconcile", "status": "queued",
				"queue_position": 2, "status_url": "/apps/books/__toft/runs/run_1"})
		case strings.HasSuffix(r.URL.Path, "/run_1"):
			reads++
			if reads < 2 {
				writeJSON(t, w, 200, map[string]any{"id": "run_1", "status": "running", "progress": 40,
					"progress_message": "loading rows"})
				return
			}
			writeJSON(t, w, 200, map[string]any{"id": "run_1", "job": "reconcile", "status": "failed",
				"exit_code": 3, "failure_reason": "exit 3: connection refused",
				"finished_at": "2026-10-22T03:04:05Z"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})

	ctx := context.Background()
	run, err := Jobs.Start(ctx, "reconcile", StartOptions{Params: map[string]any{"month": "2026-03"}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Done() || run.Job != "reconcile" || run.QueuePosition != 2 {
		t.Errorf("a queued run read as %+v", run)
	}
	final, err := run.Wait(ctx, WaitOptions{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !final.Done() || final.Succeeded() || final.FailureReason != "exit 3: connection refused" ||
		final.ExitCode == nil || *final.ExitCode != 3 || final.FinishedAt == nil {
		t.Errorf("Wait = %+v, want the failure with its reason", final)
	}
}

// A poll the app's rate limit refused is waited out, not returned: a busy
// app's other calls spend the same limiter (jobs-regression #4).
func TestAWaitWaitsOutTheRateLimit(t *testing.T) {
	reads := 0
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		reads++
		if reads < 5 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 429, map[string]any{"code": "rate_limited", "retryable": true, "error": "slow down"})
			return
		}
		writeJSON(t, w, 200, map[string]any{"id": "run_1", "status": "succeeded"})
	})
	final, err := (&JobRun{ID: "run_1"}).Wait(context.Background(), WaitOptions{PollInterval: time.Millisecond})
	if err != nil || !final.Succeeded() {
		t.Errorf("Wait under the rate limit = %+v, %v; want it waited out", final, err)
	}
}

// A wait that ran out is named, so a caller can branch on it — and it is
// retryable, because the run is still going and waiting again is fair.
func TestAWaitThatRanOutIsNamed(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{"id": "run_1", "status": "running"})
	})
	_, err := (&JobRun{ID: "run_1"}).Wait(context.Background(),
		WaitOptions{Timeout: 20 * time.Millisecond, PollInterval: time.Millisecond})
	e, ok := AsError(err)
	if !ok || e.Code() != "wait_timeout" || !e.Retryable() {
		t.Errorf("Wait past its timeout = %#v; want code wait_timeout, retryable", err)
	}
}

// Inside a request, a wait the gateway would cut short is refused before a
// single read: no timeout, or one past the request's deadline. The hint is the
// line to paste. A wait that fits is allowed, and so is any wait outside one.
func TestAWaitThatWouldOutliveTheRequestIsRefused(t *testing.T) {
	reads := 0
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		reads++
		writeJSON(t, w, 200, map[string]any{"id": "run_1", "status": "succeeded"})
	})
	ctx := context.Background()
	req := httptest.NewRequest(http.MethodGet, "/export", nil)
	req.Header.Set(RequestDeadlineHeader, time.Now().Add(30*time.Second).UTC().Format(time.RFC3339))
	run := JobRun{ID: "run_1"}

	for name, opts := range map[string]WaitOptions{
		"no timeout":        {Request: req},
		"past the deadline": {Request: req, Timeout: time.Minute},
		"by header":         {Header: req.Header, Timeout: time.Hour},
	} {
		_, err := run.Wait(ctx, opts)
		e, ok := AsError(err)
		if !ok || e.Code() != "would_outlive_request" || e.Retryable() ||
			e.Hint() != "return toft.Jobs.Respond(w, r, run)" {
			t.Errorf("%s: Wait = %#v; want would_outlive_request, not retryable, with the Respond hint", name, err)
		}
	}
	if reads != 0 {
		t.Errorf("a refused wait read the run %d times; it must refuse at once", reads)
	}

	if final, err := run.Wait(ctx, WaitOptions{Request: req, Timeout: 5 * time.Second}); err != nil || !final.Succeeded() {
		t.Errorf("a wait that fits = %+v, %v", final, err)
	}
	if final, err := run.Wait(ctx, WaitOptions{}); err != nil || !final.Succeeded() {
		t.Errorf("a wait outside a request = %+v, %v", final, err)
	}
}

// A run started for a viewer carries the request it came from, so the guard
// holds without the handler passing the request twice.
func TestARunStartedForAViewerCarriesTheRequestsDeadline(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ActForHeader) != "signed-blob" {
			t.Errorf("act-for = %q", r.Header.Get(ActForHeader))
		}
		writeJSON(t, w, 202, map[string]any{"id": "run_2", "status": "queued", "started_by": "viewer"})
	})
	headers := http.Header{}
	headers.Set(ActForHeader, "signed-blob")
	headers.Set(RequestDeadlineHeader, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
	me, err := ViewerFor(headers)
	if err != nil {
		t.Fatal(err)
	}
	run, err := me.Jobs.Start(context.Background(), "reconcile", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.StartedBy != "viewer" {
		t.Errorf("StartedBy = %q", run.StartedBy)
	}
	_, err = run.Wait(context.Background(), WaitOptions{})
	if e, ok := AsError(err); !ok || e.Code() != "would_outlive_request" {
		t.Errorf("a viewer's run waited inside its request: %v", err)
	}
}

// Respond is what a handler returns instead of waiting: 202 with Location and
// the run for a script, 303 to the live status page for a browser.
func TestRespondPointsTheCallerAtTheRun(t *testing.T) {
	run := JobRun{ID: "run_3", Job: "reconcile", Status: RunQueued, StartedBy: "app",
		StatusURL: "/apps/books/__toft/runs/run_3", LogsURL: "/api/v1/app/job-runs/run_3/logs"}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/export", nil)
	req.Header.Set("Accept", "application/json")
	if err := Jobs.Respond(w, req, run); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusAccepted || w.Header().Get("Location") != run.StatusURL {
		t.Errorf("JSON caller got %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != "run_3" || body["job"] != "reconcile" || body["status_url"] != run.StatusURL {
		t.Errorf("body = %v, want the run in the platform's own names", body)
	}

	w = httptest.NewRecorder()
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9")
	if err := Jobs.Respond(w, req, run); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != run.StatusURL {
		t.Errorf("browser got %d, Location %q; want 303 to the status page", w.Code, w.Header().Get("Location"))
	}
}

// List narrows by job and status in the query the platform reads.
func TestListSendsItsOptions(t *testing.T) {
	var query map[string]string
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		query = map[string]string{}
		for k := range r.URL.Query() {
			query[k] = r.URL.Query().Get(k)
		}
		writeJSON(t, w, 200, map[string]any{"runs": []any{
			map[string]any{"id": "run_4", "job": "nightly", "status": "failed"},
		}})
	})
	runs, err := Jobs.List(context.Background(), ListOptions{Job: "nightly", Status: RunFailed, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if query["job"] != "nightly" || query["status"] != "failed" || query["limit"] != "5" || query["scope"] != "" {
		t.Errorf("query = %v", query)
	}
	if len(runs) != 1 || runs[0].Status != RunFailed {
		t.Errorf("runs = %+v", runs)
	}
}

// Mine needs a viewer. Answering with every run would be a silent widening
// rather than an answer.
func TestMineRefusesTheAppActingAsItself(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("Mine reached the platform without a viewer")
	})
	_, err := Jobs.Mine(context.Background(), ListOptions{Job: "reconcile"})
	if err == nil {
		t.Fatal("Mine on the app itself was accepted")
	}
	// A refusal no retry can change says so, with a code to branch on.
	e, ok := AsError(err)
	if !ok || e.Code() != "no_viewer" || e.Retryable() {
		t.Errorf("Mine's refusal = %#v; want code no_viewer, not retryable", err)
	}
}

// The app relays the act-for blob; it never mints one and never learns who is
// behind it.
func TestAViewerRelaysTheBlobAndScopesTheList(t *testing.T) {
	var relayed, scope string
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		relayed = r.Header.Get(ActForHeader)
		scope = r.URL.Query().Get("scope")
		writeJSON(t, w, 200, map[string]any{"runs": []any{
			map[string]any{"id": "run_9", "status": "succeeded", "started_by": "viewer"},
		}})
	})

	headers := http.Header{}
	headers.Set(ActForHeader, "signed-blob")
	me, err := ViewerFor(headers)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := me.Jobs.Mine(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if relayed != "signed-blob" || scope != "viewer" {
		t.Errorf("act-for = %q, scope = %q", relayed, scope)
	}
	if len(runs) != 1 || runs[0].StartedBy != "viewer" {
		t.Errorf("runs = %+v", runs)
	}
	if strings.Contains(me.String(), "signed-blob") {
		t.Error("the viewer printed its own bearer blob")
	}
}

// ViewerFor fails when there is nobody to act for; Who answers nobody. A silent
// downgrade would move a run out of a person's name and into nobody's.
func TestActingForNobodyFailsWhileAskingWhoDoesNot(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an absent viewer reached the platform")
	})

	if _, err := ViewerFor(http.Header{}); err == nil {
		t.Error("ViewerFor with no header was accepted")
	} else if !strings.Contains(err.Error(), ActForHeader) {
		t.Errorf("err = %v, want it to name the header", err)
	} else if e, _ := AsError(err); e.Code() != "no_viewer" || e.Retryable() ||
		e.Hint() != "use toft.Jobs.Start(...) to start it as the app, or toft.Jobs.List(...) for its runs" {
		t.Errorf("ViewerFor's refusal = %#v; want no_viewer with its hint", err)
	} else if msg := err.Error(); !strings.Contains(msg, "anonymous visitor") ||
		!strings.Contains(msg, "viewer_anonymous") || strings.Contains(msg, "only for a signed-in") {
		// An anonymous visitor to a public app carries the header too (D5).
		t.Errorf("ViewerFor's sentence = %q; want it to say an anonymous visitor carries the header", msg)
	}

	who, err := Who(context.Background(), http.Header{})
	if err != nil || who != nil {
		t.Errorf("Who = %v, %v — an anonymous visitor is an answer, not a failure", who, err)
	}
}

// 204 is "there is nobody", and it must not collapse with "not allowed to ask".
func TestNoViewerAndNotAllowedToAskAreDifferent(t *testing.T) {
	answer := http.StatusNoContent
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if answer == http.StatusNoContent {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(t, w, 403, map[string]any{"error": "viewer identity is not enabled for this app"})
	})

	headers := http.Header{}
	headers.Set(ActForHeader, "blob-a")
	ctx := context.Background()

	who, err := Who(ctx, headers)
	if err != nil || who != nil {
		t.Errorf("a 204 gave %v, %v — want nobody and no error", who, err)
	}

	answer = http.StatusForbidden
	headers.Set(ActForHeader, "blob-b")
	if _, err := Who(ctx, headers); err == nil {
		t.Error("a 403 read as an anonymous visitor")
	}
}

// The identity is cached per blob, and WhoNow is the way past it.
func TestAnIdentityIsCachedPerBlob(t *testing.T) {
	var reads int
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		reads++
		writeJSON(t, w, 200, map[string]any{
			"id": "usr_1", "name": "Ada", "role": "owner", "is_admin": false,
			"groups": []any{map[string]any{"id": "grp_1", "name": "finance"}},
		})
	})

	headers := http.Header{}
	headers.Set(ActForHeader, "blob-c")
	ctx := context.Background()

	first, err := Who(ctx, headers)
	if err != nil {
		t.Fatal(err)
	}
	if !first.InGroup("finance") || first.GroupNames()[0] != "finance" {
		t.Errorf("groups = %+v", first.Groups)
	}
	if _, err := Who(ctx, headers); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("reads = %d, want the second answered from the cache", reads)
	}
	if _, err := WhoNow(ctx, headers); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Errorf("reads = %d, want WhoNow to have gone to the platform", reads)
	}
}

// The callback contract is a different credential on different routes: sharing
// one client between them would be the first step towards sharing a credential.
func TestARunReportsWithItsOwnTokenAndNotTheAppsToken(t *testing.T) {
	var auth, contract string
	var progress map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		contract = r.Header.Get("X-Toft-Runner-Version")
		_ = json.NewDecoder(r.Body).Decode(&progress)
		writeJSON(t, w, 200, map[string]any{})
	}))
	defer server.Close()

	t.Setenv(EnvAPIURL, server.URL)
	t.Setenv("TOFT_JOB_TOKEN", "run-scoped")
	t.Setenv("TOFT_RUN_ID", "run_11")
	t.Setenv("TOFT_PARAMS", `{"month":"2026-03"}`)
	t.Setenv(EnvJobName, "reconcile")
	t.Setenv(EnvRerunOf, "run_10")
	t.Setenv(EnvDeadline, "2026-10-22T04:00:00Z")

	run := CurrentRun()
	if run == nil {
		t.Fatal("CurrentRun was nil inside a job")
	}
	var params struct {
		Month string `json:"month"`
	}
	if err := run.Params(&params); err != nil || params.Month != "2026-03" {
		t.Fatalf("Params = %+v, %v", params, err)
	}
	if run.Job != "reconcile" || run.RerunOf != "run_10" {
		t.Errorf("Job, RerunOf = %q, %q", run.Job, run.RerunOf)
	}
	if at, ok := run.Deadline(); !ok || !at.Equal(time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("Deadline = %v, %v", at, ok)
	}
	if err := run.Progress(context.Background(), 80, "writing the CSV"); err != nil {
		t.Fatal(err)
	}
	if progress["percent"] != float64(80) || progress["message"] != "writing the CSV" {
		t.Errorf("progress body = %v, want the percent and its message", progress)
	}
	if auth != "Bearer run-scoped" {
		t.Errorf("Authorization = %q, want the run-scoped token", auth)
	}
	if contract != ContractVersion {
		t.Errorf("runner contract = %q, want %q", contract, ContractVersion)
	}
}

// Outside a job it is nil, which is how a package shared between an app's web
// entrypoint and its job entrypoint tells which one it is in.
func TestThereIsNoRunOutsideAJob(t *testing.T) {
	t.Setenv("TOFT_RUN_ID", "")
	t.Setenv("TOFT_JOB_TOKEN", "")
	if run := CurrentRun(); run != nil {
		t.Errorf("CurrentRun outside a job = %+v", run)
	}
}

// An output object is fetched over a presigned URL the platform mints, and the
// app's own token never reaches the host it points at.
func TestAnOutputIsFetchedWithoutTheAppsToken(t *testing.T) {
	var storeAuth string
	var storeSeen bool
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storeSeen = true
		storeAuth = r.Header.Get("Authorization")
		w.Write([]byte("rows,total\n1204,42\n"))
	}))
	defer store.Close()

	platform(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, store.URL+"/object", http.StatusFound)
	})

	run := JobRun{ID: "run_12"}
	body, err := run.Output(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "rows,total") {
		t.Errorf("Output = %q", body)
	}
	if !storeSeen {
		t.Fatal("the redirect was not followed")
	}
	if storeAuth != "" {
		t.Errorf("the object store was handed %q — a presigned URL is the whole authorization", storeAuth)
	}
}

// A run that produced nothing is not an error to the caller.
func TestAnAbsentOutputIsEmptyRatherThanAFailure(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	body, err := JobRun{ID: "run_13"}.Output(context.Background())
	if err != nil || body != nil {
		t.Errorf("Output = %q, %v — want nothing and no error", body, err)
	}
	var v map[string]any
	if ok, err := (JobRun{ID: "run_13"}).OutputJSON(context.Background(), &v); ok || err != nil {
		t.Errorf("OutputJSON = %v, %v — want false and no error", ok, err)
	}
}

// OutputJSON is the reading twin of WriteOutputJSON.
func TestOutputJSONDecodesWhatTheJobWrote(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"rows":1204}`))
	})
	var v struct{ Rows int }
	ok, err := JobRun{ID: "run_14"}.OutputJSON(context.Background(), &v)
	if !ok || err != nil || v.Rows != 1204 {
		t.Errorf("OutputJSON = %v, %v, %+v", ok, err, v)
	}
}
