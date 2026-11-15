package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"toft/internal/appint"
	"toft/internal/database"
)

// One app token must not be able to stream an unbounded body onto TMPDIR —
// the disk every concurrent build writes to. The cap applies before
// ParseMultipartForm spills anything, and answers 413 rather than the shape
// error a malformed body gets.
func TestAppMultipartIsCappedBeforeItSpills(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	kind := &appint.Kind{FileLimit: 1 << 20, TextLimit: 1 << 10}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "big.bin")
	part.Write(bytes.Repeat([]byte("x"), 4<<20)) // over the 1 MiB file + 1 MiB framing ceiling
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/app/notify/slack/file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	err := parseAppMultipart(rec, req, kind)
	if err == nil {
		t.Fatal("a body over the ceiling parsed")
	}
	if !tooLarge(rec, err) {
		t.Fatalf("err = %v, want the body cap", err)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if left, _ := os.ReadDir(tmp); len(left) > 0 {
		t.Errorf("%d spill files left in TMPDIR", len(left))
	}

	// Within the ceiling parses as before.
	body.Reset()
	mw = multipart.NewWriter(&body)
	part, _ = mw.CreateFormFile("file", "ok.bin")
	part.Write(bytes.Repeat([]byte("x"), 512<<10))
	mw.Close()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/app/notify/slack/file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if err := parseAppMultipart(httptest.NewRecorder(), req, kind); err != nil {
		t.Fatalf("a body within the ceiling: %v", err)
	}
}

// Every size limit on a send is 413 too_large with a hint — the published code
// — and a body over the JSON cap is the same answer, not "invalid JSON body"
// for JSON that was valid and merely long (BB-028, fourth 2026-11-03
// black-box list). Every other refusal on these routes carries a hint too
// (BB-042).
func TestNotifyOverLimitsAreTooLargeWithAHint(t *testing.T) {
	s := &Server{}
	token := &database.ResolvedAppToken{}
	slack := appint.Lookup(appint.IntegrationSlack)
	email := appint.Lookup(appint.IntegrationEmail)
	mail := func(extra string) string {
		return `{"to":["a@example.com"],"subject":"s",` + extra + `}`
	}
	for _, c := range []struct {
		name, kind, body string
		status           int
		code             string
	}{
		{"slack text over its limit", "slack",
			`{"text":"` + strings.Repeat("x", slack.TextLimit+1) + `"}`, 413, "too_large"},
		{"slack body over the JSON cap", "slack",
			`{"text":"` + strings.Repeat("x", slack.TextLimit+70000) + `"}`, 413, "too_large"},
		{"email text over its limit", "email",
			mail(`"text":"` + strings.Repeat("x", email.TextLimit+1) + `"`), 413, "too_large"},
		{"email html over its limit", "email",
			mail(`"text":"t","html":"` + strings.Repeat("x", emailHTMLLimit+1) + `"`), 413, "too_large"},
		{"email body over the JSON cap", "email",
			mail(`"text":"` + strings.Repeat("x", email.TextLimit+emailHTMLLimit+70000) + `"`), 413, "too_large"},
		{"email subject over its limit", "email",
			`{"to":["a@example.com"],"subject":"` + strings.Repeat("s", emailSubjectMax+1) + `","text":"t"}`, 413, "too_large"},
		{"too many recipients", "email",
			`{"to":[` + strings.TrimSuffix(strings.Repeat(`"a@example.com",`, emailRecipients+1), ",") + `],"subject":"s","text":"t"}`,
			400, "too_many_recipients"},
		{"blank slack text", "slack", `{"text":"  "}`, 400, "bad_request"},
		{"blank email subject", "email", `{"to":["a@example.com"],"subject":"","text":"t"}`, 400, "bad_request"},
		{"broken JSON", "slack", `{"text":`, 400, "bad_request"},
		{"blocks", "slack", `{"text":"t","blocks":[]}`, 400, "blocks_not_allowed"},
		{"unknown action", "slack", `{"text":"t","action":"nope"}`, 400, "unknown_action"},
		{"unknown kind", "nope", `{"text":"t"}`, 400, "unknown_kind"},
		{"pagerduty with no dedup_key", "pagerduty", `{"text":"t"}`, 400, "dedup_key_required"},
		{"pagerduty resolve with no dedup_key", "pagerduty", `{"text":"t","action":"resolve"}`, 400, "dedup_key_required"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/app/notify/"+c.kind, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("kind", c.kind)
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		req = req.WithContext(context.WithValue(ctx, appTokenCtxKey{}, token))
		rec := httptest.NewRecorder()
		s.handleAppNotify(rec, req)
		var body struct{ Code, Hint string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || body.Code != c.code || body.Hint == "" {
			out := rec.Body.String()
			if len(out) > 300 {
				out = out[:300]
			}
			t.Errorf("%s: %d %q hint=%q, want %d %s with a hint: %s",
				c.name, rec.Code, body.Code, body.Hint, c.status, c.code, out)
		}
	}
}
