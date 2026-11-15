package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"toft/internal/database"
)

// Test 10. The webhook buys a minute and nothing else.
//
// **It marks, and the poll is what builds.** A verified delivery enqueues
// nothing — every gate the poller applies on a tick still applies — so the
// worst a delivery can do is cost the install exactly the minute it was buying.

// recordingNudger stands in for the poller: it records what it was asked to
// poll and does nothing, which is all this route's contract says about it.
type recordingNudger struct {
	mu    sync.Mutex
	calls [][]string
}

func (n *recordingNudger) PollNow(appIDs []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, append([]string(nil), appIDs...))
}

func (n *recordingNudger) marked() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, c := range n.calls {
		out = append(out, c...)
	}
	return out
}

// hookEnv is a connection with a known webhook secret, an app on it, and a
// recording poller.
type hookEnv struct {
	*sourceEnv
	conn   *database.GitConnection
	secret string
	nudger *recordingNudger
	appID  string
}

func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	e := newSourceEnv(t)
	dto := createConnection(t, e, tokenConnectionBody)

	conn, err := e.db.GetGitConnection(dto.ID)
	if err != nil {
		t.Fatalf("GetGitConnection: %v", err)
	}
	// The webhook is opt-in, so a created connection has no secret: set one up
	// the way the set-up route does, and keep the plaintext the host would
	// have been shown. (Reading back a create's secret failed with a keyring
	// error on every run: there was none to read.)
	secret := "hook-secret-for-tests"
	ciphertext, err := e.srv.encryptor.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := e.db.RotateGitConnectionWebhookSecret(conn.ID, ciphertext, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("RotateGitConnectionWebhookSecret: %v", err)
	}
	if conn, err = e.db.GetGitConnection(dto.ID); err != nil {
		t.Fatalf("GetGitConnection: %v", err)
	}

	app := &database.App{
		ID: "app-reports", Slug: "reports", Name: "reports", OwnerID: "admin-user",
		RepoURL: "https://github.com/acme/reports", Branch: "main",
		Status: database.AppStatusRunning, AutoDeploy: true,
	}
	if err := e.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if err := e.db.SetAppGitConnection(app.ID, conn.ID); err != nil {
		t.Fatalf("SetAppGitConnection: %v", err)
	}

	nudger := &recordingNudger{}
	e.srv.SetAutodeploy(nudger)
	return &hookEnv{sourceEnv: e, conn: conn, secret: secret, nudger: nudger, appID: app.ID}
}

func (h *hookEnv) deliver(t *testing.T, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/git/hooks/"+h.conn.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const pushBody = `{"repository":{"full_name":"acme/reports"}}`

func TestAVerifiedDeliveryMarksAndEnqueuesNothing(t *testing.T) {
	h := newHookEnv(t)

	rec := h.deliver(t, pushBody, sign(h.secret, pushBody))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delivery = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if marked := h.nudger.marked(); len(marked) != 1 || marked[0] != h.appID {
		t.Fatalf("marked = %v, want the one app on that repository", marked)
	}
	// Nothing was enqueued: the poller is what builds, and it does what it does
	// on a tick.
	deployments, err := h.db.ListDeployments(h.appID)
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(deployments) != 0 {
		t.Errorf("a delivery enqueued %d deployment(s) itself", len(deployments))
	}

	// **Delivered twice is two markers and one build**, because the guarded
	// claim was already the mechanism that made several nodes seeing one push
	// build it once. This route adds no second mechanism.
	rec = h.deliver(t, pushBody, sign(h.secret, pushBody))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second delivery = %d, want 204", rec.Code)
	}
	if marked := h.nudger.marked(); len(marked) != 2 {
		t.Errorf("two deliveries marked %d times, want 2", len(marked))
	}
}

// An HMAC-failing delivery writes nothing and polls nothing. It is counted, not
// recorded per delivery: an unauthenticated endpoint that writes a row per
// request is a way to fill a table from the internet.
func TestAnUnverifiedDeliveryWritesNothingAndPollsNothing(t *testing.T) {
	h := newHookEnv(t)
	auditBefore := len(auditRows(t, h.db, "git_connection.updated"))

	for _, c := range []struct{ name, signature string }{
		{"wrong signature", sign("not-the-secret", pushBody)},
		{"no signature", ""},
		{"signature over a different body", sign(h.secret, `{"repository":{"full_name":"acme/other"}}`)},
	} {
		rec := h.deliver(t, pushBody, c.signature)
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s = %d, want 204", c.name, rec.Code)
		}
	}
	if marked := h.nudger.marked(); len(marked) != 0 {
		t.Errorf("an unverified delivery polled %v", marked)
	}
	if after := len(auditRows(t, h.db, "git_connection.updated")); after != auditBefore {
		t.Error("an unverified delivery wrote an audit row")
	}
	// The connection's health is untouched: an unverified delivery is not
	// evidence about the host.
	conn, err := h.db.GetGitConnection(h.conn.ID)
	if err != nil {
		t.Fatalf("GetGitConnection: %v", err)
	}
	if conn.LastOKAt != nil {
		t.Error("an unverified delivery recorded the connection as healthy")
	}
}

// A delivery for a repository no app deploys from: no marker, nothing.
func TestADeliveryForAnUnusedRepositoryDoesNothing(t *testing.T) {
	h := newHookEnv(t)
	body := `{"repository":{"full_name":"acme/nobody-deploys-this"}}`

	rec := h.deliver(t, body, sign(h.secret, body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delivery = %d, want 204", rec.Code)
	}
	if marked := h.nudger.marked(); len(marked) != 0 {
		t.Errorf("a delivery for an unused repository marked %v", marked)
	}
}

// An unknown connection answers 204 rather than 404: an unauthenticated
// endpoint that tells the internet which ids exist is an oracle, and there is
// nothing a real host would do differently with the answer.
func TestAnUnknownConnectionIsNotAnOracle(t *testing.T) {
	h := newHookEnv(t)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/git/hooks/00000000-0000-0000-0000-000000000000", strings.NewReader(pushBody))
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("unknown connection = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("the answer carries a body: %s", rec.Body.String())
	}
}

// The route requires no credential of ours and takes none: a session cookie
// changes nothing, and neither does a key.
func TestTheHookTakesNoCredentialOfOurs(t *testing.T) {
	h := newHookEnv(t)

	// Correct HMAC and no session: accepted.
	if rec := h.deliver(t, pushBody, sign(h.secret, pushBody)); rec.Code != http.StatusNoContent {
		t.Fatalf("anonymous verified delivery = %d", rec.Code)
	}
	before := len(h.nudger.marked())

	// A session with a wrong HMAC: still refused. Being signed in is not a way
	// past the signature.
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/git/hooks/"+h.conn.ID, strings.NewReader(pushBody))
	req.AddCookie(sessionCookieFor(t, h.srv, "admin-user"))
	req.Header.Set("X-Hub-Signature-256", sign("not-the-secret", pushBody))
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("signed-in unverified delivery = %d, want 204", rec.Code)
	}
	if after := len(h.nudger.marked()); after != before {
		t.Error("a session got a delivery past the signature check")
	}
}

// Both hosts, one route. GitLab compares a shared token whole; GitHub signs the
// body. A payload spelled either way names the same repository.
func TestBothHostsSpellingsAreRead(t *testing.T) {
	h := newHookEnv(t)

	gitlabBody := `{"project":{"path_with_namespace":"acme/reports"}}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/git/hooks/"+h.conn.ID, strings.NewReader(gitlabBody))
	req.Header.Set("X-Gitlab-Token", h.secret)
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("gitlab delivery = %d, want 204", rec.Code)
	}
	if marked := h.nudger.marked(); len(marked) != 1 || marked[0] != h.appID {
		t.Errorf("marked = %v, want the one app on that repository", marked)
	}

	// A wrong GitLab token is refused whole rather than compared by prefix.
	req = httptest.NewRequest(http.MethodPost,
		"/api/v1/git/hooks/"+h.conn.ID, strings.NewReader(gitlabBody))
	req.Header.Set("X-Gitlab-Token", h.secret[:len(h.secret)-1])
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	rec = httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if len(h.nudger.marked()) != 1 {
		t.Error("a truncated token was accepted")
	}
}

func TestDeliveredRepositoryReadsBothSpellingsAndGuessesAtNothing(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"repository":{"full_name":"acme/reports"}}`, "acme/reports"},
		{`{"repository":{"path_with_namespace":"acme/team/payroll"}}`, "acme/team/payroll"},
		{`{"project":{"path_with_namespace":"acme/reports"}}`, "acme/reports"},
		{`{"repository":{"full_name":"reports"}}`, ""}, // no owner: not a repository
		{`{"repository":{}}`, ""},
		{`{}`, ""},
		{`not json at all`, ""},
	} {
		if got := deliveredRepository([]byte(c.body)); got != c.want {
			t.Errorf("deliveredRepository(%s) = %q, want %q", c.body, got, c.want)
		}
	}
}

// DG-7 (security review 2026-11-14): a verified delivery proves the webhook
// secret, not the clone credential, so it leaves the connection's recorded
// credential failure where Test put it.
func TestADeliveryLeavesTheCredentialsHealthAlone(t *testing.T) {
	h := newHookEnv(t)
	if _, err := h.db.RecordGitConnectionResult(h.conn.ID, "refused this install's credential"); err != nil {
		t.Fatalf("RecordGitConnectionResult: %v", err)
	}
	if rec := h.deliver(t, pushBody, sign(h.secret, pushBody)); rec.Code != http.StatusNoContent {
		t.Fatalf("delivery = %d", rec.Code)
	}
	conn, err := h.db.GetGitConnection(h.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.LastError != "refused this install's credential" || conn.LastOKAt != nil {
		t.Errorf("after a delivery: last_error=%q last_ok_at=%v, want the failure untouched",
			conn.LastError, conn.LastOKAt)
	}
}

// DG-8 (security review 2026-11-14): a signed delivery that is not a push —
// an issue, a pull request — for a deployed repository marks nothing.
func TestANonPushDeliveryIsIgnored(t *testing.T) {
	h := newHookEnv(t)
	for _, event := range []string{"issues", "pull_request", "ping", ""} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/git/hooks/"+h.conn.ID, strings.NewReader(pushBody))
		req.Header.Set("X-Hub-Signature-256", sign(h.secret, pushBody))
		if event != "" {
			req.Header.Set("X-GitHub-Event", event)
		}
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%q delivery = %d, want 204", event, rec.Code)
		}
	}
	if marked := h.nudger.marked(); len(marked) != 0 {
		t.Errorf("non-push deliveries marked %v, want nothing", marked)
	}
}
