package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The device flow, end to end and at its edges.
//
// The properties under test are the ones the design rests on rather than the
// happy path alone: a credential cannot mint a credential, a code is single-use,
// and a decision is a decision.

type deviceEnv struct {
	srv    *Server
	router http.Handler
}

func newDeviceEnv(t *testing.T) *deviceEnv {
	t.Helper()
	srv, router := keyEnv(t)
	return &deviceEnv{srv: srv, router: router}
}

// start runs the unauthenticated first hop and returns the decoded answer.
func (e *deviceEnv) start(t *testing.T, hostname string) map[string]any {
	t.Helper()
	rec := e.post(t, "", "/api/v1/auth/cli/device",
		map[string]string{"hostname": hostname, "version": "0.1.0"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func (e *deviceEnv) post(t *testing.T, who, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// poll goes straight to the handler's state, stepping over the per-code
// back-off: these tests are about what the flow answers, not how fast it may be
// asked, and TestPollingTooFastIsToldToSlowDown covers the floor on its own.
func (e *deviceEnv) poll(t *testing.T, deviceCode string) (int, map[string]any) {
	t.Helper()
	e.unthrottle(t, deviceCode)
	rec := e.post(t, "", "/api/v1/auth/cli/device/token",
		map[string]string{"device_code": deviceCode})
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// unthrottle backdates the last poll so the next one is not refused for being
// too soon.
func (e *deviceEnv) unthrottle(t *testing.T, deviceCode string) {
	t.Helper()
	_, err := e.srv.db.Conn().Exec(
		`UPDATE cli_device_codes SET last_polled_at = ? WHERE device_code_hash = ?`,
		time.Now().UTC().Add(-time.Minute), hashAPIKey(deviceCode))
	if err != nil {
		t.Fatalf("backdating the poll: %v", err)
	}
}

func TestDeviceFlowMintsAnOrdinaryPersonalKey(t *testing.T) {
	e := newDeviceEnv(t)
	started := e.start(t, "ada-mbp")
	deviceCode := started["device_code"].(string)
	userCode := started["user_code"].(string)

	// Before anyone answers, the terminal is told to keep waiting — and that is
	// a 200, not an error: waiting for a human is what this flow does.
	code, out := e.poll(t, deviceCode)
	if code != http.StatusOK || out["status"] != deviceStatusPending {
		t.Fatalf("poll before approval = %d %v, want 200 pending", code, out)
	}

	// The approval screen names the machine that asked.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/cli/device?user_code="+userCode, nil)
	req.AddCookie(sessionCookieFor(t, e.srv, "alice"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup: %d body=%s", rec.Code, rec.Body.String())
	}
	var view deviceRequestView
	json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Hostname != "ada-mbp" {
		t.Errorf("hostname = %q, want ada-mbp — the screen has to say what it is approving", view.Hostname)
	}

	if rec := e.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": userCode, "approve": true}); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}

	code, out = e.poll(t, deviceCode)
	if code != http.StatusOK || out["status"] != deviceStatusApproved {
		t.Fatalf("poll after approval = %d %v, want 200 approved", code, out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatal("the poll that collects the approval returns the secret; it returned nothing")
	}

	// It is an toftu_ key and nothing else: the same rows the keys page lists,
	// owned by the approver, and it authenticates.
	keys, err := e.srv.db.ListUserAPIKeys("alice", 1)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("alice has %d keys, want 1", len(keys))
	}
	if keys[0].Name != "toft on ada-mbp" {
		t.Errorf("key name = %q, want it to name the machine", keys[0].Name)
	}
	if rec := getWithKey(t, e.router, token, "/api/v1/me/capabilities"); rec.Code != http.StatusOK {
		t.Errorf("the minted key does not authenticate: %d %s", rec.Code, rec.Body.String())
	}

	// Single use. The request is gone, so a replay of the device code — by
	// whoever else might have it — collects nothing.
	code, _ = e.poll(t, deviceCode)
	if code != http.StatusNotFound {
		t.Errorf("replayed poll = %d, want 404; the request must be single-use", code)
	}
}

// The rule the whole design rests on: **a key never mints a key.** These routes
// live where a presented credential is ignored, so an toftu_ holder approving its
// own code is simply an anonymous caller, and anonymous cannot approve.
func TestAKeyCannotApproveItsOwnLogin(t *testing.T) {
	e := newDeviceEnv(t)
	stolen := mintKey(t, e.srv, "alice", "leaked", nil, false)
	started := e.start(t, "attacker-box")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/cli/device/approve",
		bytes.NewReader([]byte(`{"user_code":"`+started["user_code"].(string)+`","approve":true}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", stolen)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("approve with a key = %d, want 401 — a stolen key must not mint a fresh one (body: %s)",
			rec.Code, rec.Body.String())
	}
	// And the lookup is closed to it too, so a key cannot even enumerate what
	// is waiting.
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/auth/cli/device?user_code="+started["user_code"].(string), nil)
	req.Header.Set("X-API-Key", stolen)
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("lookup with a key = %d, want 401", rec.Code)
	}
}

func TestDeniedLoginStaysDenied(t *testing.T) {
	e := newDeviceEnv(t)
	started := e.start(t, "ada-mbp")
	userCode := started["user_code"].(string)

	if rec := e.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": userCode, "approve": false}); rec.Code != http.StatusOK {
		t.Fatalf("deny: %d body=%s", rec.Code, rec.Body.String())
	}

	code, out := e.poll(t, started["device_code"].(string))
	if code != http.StatusOK || out["status"] != deviceStatusDenied {
		t.Fatalf("poll after denial = %d %v, want 200 denied", code, out)
	}
	if _, err := e.srv.db.GetDeviceCodeByUserCode(userCode); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// A second answer is not accepted: one approval, and a decision somebody
	// already made is not one a second tab can revisit.
	if rec := e.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": userCode, "approve": true}); rec.Code == http.StatusOK {
		t.Errorf("a denied request was approved on a second attempt (%d)", rec.Code)
	}
	if n, _ := e.srv.db.CountUserAPIKeys("alice"); n != 0 {
		t.Errorf("a denied login minted %d keys, want 0", n)
	}
}

// Approving is a decision the approver made about their own account, and it is
// audited as one — including who, and which machine they let in.
func TestApprovalIsAudited(t *testing.T) {
	e := newDeviceEnv(t)
	started := e.start(t, "ada-mbp")
	e.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": started["user_code"].(string), "approve": true})

	var after string
	err := e.srv.db.Conn().QueryRow(
		`SELECT after FROM audit_log WHERE action='user.cli_login_approve' AND actor_id='alice'`).
		Scan(&after)
	if err != nil {
		t.Fatalf("approving a CLI sign-in wrote no audit row: %v", err)
	}
	if !strings.Contains(after, "ada-mbp") {
		t.Errorf("the audit row does not name the machine: %s", after)
	}

	// And the mint is audited too, as the credential creation it is.
	var mint string
	if err := e.srv.db.Conn().QueryRow(
		`SELECT after FROM audit_log WHERE action='user.api_key_create' AND actor_id='alice'`).
		Scan(&mint); err == nil {
		t.Fatal("the key was minted at approval time; it must be minted by the poll that collects it")
	}
	e.poll(t, started["device_code"].(string))
	if err := e.srv.db.Conn().QueryRow(
		`SELECT after FROM audit_log WHERE action='user.api_key_create' AND actor_id='alice'`).
		Scan(&mint); err != nil {
		t.Fatalf("minting the key wrote no audit row: %v", err)
	}
	if !strings.Contains(mint, "cli_login") {
		t.Errorf("the mint row does not say where the key came from: %s", mint)
	}
}

func TestPollingTooFastIsToldToSlowDown(t *testing.T) {
	e := newDeviceEnv(t)
	started := e.start(t, "ada-mbp")
	deviceCode := started["device_code"].(string)

	// The first poll stamps the row; the second arrives immediately.
	e.post(t, "", "/api/v1/auth/cli/device/token", map[string]string{"device_code": deviceCode})
	rec := e.post(t, "", "/api/v1/auth/cli/device/token", map[string]string{"device_code": deviceCode})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second poll = %d, want 429 (body: %s)", rec.Code, rec.Body.String())
	}
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["code"] != "slow_down" {
		t.Errorf("code = %q, want slow_down — the client needs to know which refusal this is", out["code"])
	}

	// **Back-off bounds the rate; it does not lock the door.** A refused poll
	// is not stamped, so a client that polled too fast and then waited properly
	// is served — rather than being told to slow down forever because every
	// attempt it made moved the window forward.
	e.post(t, "", "/api/v1/auth/cli/device/token", map[string]string{"device_code": deviceCode})
	e.unthrottle(t, deviceCode)
	rec = e.post(t, "", "/api/v1/auth/cli/device/token", map[string]string{"device_code": deviceCode})
	if rec.Code != http.StatusOK {
		t.Errorf("a client that backed off was still refused: %d %s", rec.Code, rec.Body.String())
	}
}

// An expired request is over, and says so rather than waiting forever.
func TestAnExpiredCodeIsToldItExpired(t *testing.T) {
	e := newDeviceEnv(t)
	started := e.start(t, "ada-mbp")
	deviceCode := started["device_code"].(string)

	if _, err := e.srv.db.Conn().Exec(
		`UPDATE cli_device_codes SET expires_at = ? WHERE device_code_hash = ?`,
		time.Now().UTC().Add(-time.Second), hashAPIKey(deviceCode)); err != nil {
		t.Fatalf("expiring the row: %v", err)
	}

	code, out := e.poll(t, deviceCode)
	if code != http.StatusOK || out["status"] != deviceStatusExpired {
		t.Fatalf("poll of an expired request = %d %v, want 200 expired", code, out)
	}
	// And it cannot be approved after the fact: a code somebody can be talked
	// into approving an hour later is the risk the ten minutes exists to bound.
	if rec := e.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": started["user_code"].(string), "approve": true}); rec.Code == http.StatusOK {
		t.Errorf("an expired request was approved (%d)", rec.Code)
	}
}

// Off means absent: 404 on the way in, so the flow looks like something this
// install does not offer rather than something it offers and denies.
func TestDeviceFlowIsAbsentWhenPersonalKeysAreOff(t *testing.T) {
	e := newDeviceEnv(t)
	enablePersonalKeys(t, e.srv, false)

	for _, tc := range []struct {
		method, path string
		who          string
	}{
		{http.MethodPost, "/api/v1/auth/cli/device", ""},
		{http.MethodPost, "/api/v1/auth/cli/device/token", ""},
		{http.MethodGet, "/api/v1/auth/cli/device?user_code=BCDF-2345", "alice"},
		{http.MethodPost, "/api/v1/auth/cli/device/approve", "alice"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		if tc.who != "" {
			req.AddCookie(sessionCookieFor(t, e.srv, tc.who))
		}
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// A person types what they see, and what they see has a hyphen in it. Every
// spelling of the same code resolves to the stored one, in one place, so four
// call sites cannot disagree about whether case matters.
func TestUserCodeNormalization(t *testing.T) {
	for _, in := range []string{"BCDF-2345", "bcdf-2345", "bcdf2345", " BCDF2345 ", "BCDF 2345"} {
		if got := normalizeUserCode(in); got != "BCDF-2345" {
			t.Errorf("normalizeUserCode(%q) = %q, want BCDF-2345", in, got)
		}
	}
}

// Whoever ran the client picked these strings, and a person reads them to
// decide whether to hand over a credential.
func TestApprovalScreenDisplayIsBounded(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	if got := sanitizeDeviceDisplay(long); len(got) > maxDeviceDisplayLen {
		t.Errorf("a %d-character hostname survived as %d characters", len(long), len(got))
	}
	if got := sanitizeDeviceDisplay("ada\nmbp\x00"); got != "adambp" {
		t.Errorf("sanitizeDeviceDisplay = %q, want the control characters gone", got)
	}
}

// Ten minutes, one approval. An approval that nobody collected inside the ten
// minutes is expired, not approved: the poll says so, and the claim underneath
// it mints nothing. Before this the status tested approval before expiry and
// the claim had no expiry clause, so a code approved at 09:59 and collected a
// day later minted a key for the approver.
func TestAnApprovedCodeCannotBeCollectedAfterItExpires(t *testing.T) {
	e := newDeviceEnv(t)
	started := e.start(t, "ada-mbp")
	deviceCode := started["device_code"].(string)

	if rec := e.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": started["user_code"].(string), "approve": true}); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := e.srv.db.Conn().Exec(
		`UPDATE cli_device_codes SET expires_at = ? WHERE device_code_hash = ?`,
		time.Now().UTC().Add(-time.Second), hashAPIKey(deviceCode)); err != nil {
		t.Fatalf("expiring the row: %v", err)
	}

	code, out := e.poll(t, deviceCode)
	if code != http.StatusOK || out["status"] != deviceStatusExpired {
		t.Fatalf("poll of an approved-then-expired request = %d %v, want 200 expired", code, out)
	}
	if _, ok := out["api_key"]; ok {
		t.Fatal("a key was minted from an expired approval")
	}
	var minted int
	if err := e.srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action='user.api_key_create' AND actor_id='alice'`).
		Scan(&minted); err != nil {
		t.Fatal(err)
	}
	if minted != 0 {
		t.Fatal("the claim minted a key for an expired approval")
	}
}
