package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/notify"
)

// The publisher-request workflow, end to end through the real router: a viewer
// asks, administrators are told, one of them decides, and the requester is told
// what was decided.

func newPromotionEnv(t *testing.T) (*Server, http.Handler, *fakeMailer) {
	t.Helper()
	srv, router, fake := newMailTestServer(t)
	seedUser(t, srv.db, "vic", database.SystemRoleViewer)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	return srv, router, fake
}

// createRequest asks for promotion as vic and returns the new request id.
func createRequest(t *testing.T, srv *Server, router http.Handler) string {
	t.Helper()
	rec := sendJSONAs(t, srv, router, "vic", http.MethodPost, "/api/v1/me/promotion-requests", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create promotion request = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var req database.AccessRequest
	if err := json.Unmarshal(rec.Body.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	if req.ID == "" {
		t.Fatal("created request has no id")
	}
	return req.ID
}

func TestPromotionRequest_NotifiesEveryEligibleAdmin(t *testing.T) {
	srv, router, fake := newPromotionEnv(t)
	// A second admin, one locked admin, and one admin with no address: only
	// the first should be written to.
	seedUser(t, srv.db, "root2", database.SystemRoleAdmin)
	locked := seedUser(t, srv.db, "lockedadmin", database.SystemRoleAdmin)
	if _, err := srv.db.Conn().Exec(`UPDATE users SET locked_at = NOW() WHERE id = $1`, locked.ID); err != nil {
		t.Fatalf("lock admin: %v", err)
	}
	noEmail := seedUser(t, srv.db, "quietadmin", database.SystemRoleAdmin)
	noEmail.Email = ""
	if err := srv.db.UpsertUser(noEmail); err != nil {
		t.Fatal(err)
	}

	reqID := createRequest(t, srv, router)

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1 addressed to the admins", len(msgs))
	}
	got := map[string]bool{}
	for _, to := range msgs[0].To {
		got[to] = true
	}
	for _, want := range []string{"root@example.com", "root2@example.com"} {
		if !got[want] {
			t.Errorf("%s was not notified", want)
		}
	}
	for _, unwanted := range []string{"lockedadmin@example.com", "quietadmin@example.com", "vic@example.com"} {
		if got[unwanted] {
			t.Errorf("%s should not have been notified", unwanted)
		}
	}
	if !strings.Contains(msgs[0].HTML, reqID) {
		t.Error("the notification does not deep-link the request it is about")
	}
	if !strings.Contains(msgs[0].HTML, "/admin/publisher-requests") {
		t.Error("the notification does not link to the admin queue")
	}
}

func TestPromotionRequest_DuplicateIsRejected(t *testing.T) {
	srv, router, _ := newPromotionEnv(t)
	createRequest(t, srv, router)

	rec := sendJSONAs(t, srv, router, "vic", http.MethodPost, "/api/v1/me/promotion-requests", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second request = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

// The partial unique index, not the in-transaction count, is what has to hold:
// the count loses a race and the index does not.
func TestPromotionRequest_UniqueIndexRejectsDuplicate(t *testing.T) {
	srv, router, _ := newPromotionEnv(t)
	createRequest(t, srv, router)

	// Insert straight past the handler's check, the way a racing request does.
	_, err := srv.db.Conn().Exec(`
		INSERT INTO access_requests (id, requester_id, request_type, requested_role, status, created_at)
		VALUES ('racing-row', 'vic', 'role_promotion', 'publisher', 'pending', NOW())`)
	if err == nil {
		t.Fatal("a second pending promotion row was accepted; the unique index is missing")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("insert failed for the wrong reason: %v", err)
	}
}

func TestPromotionRequest_RequesterSeesAndWithdraws(t *testing.T) {
	srv, router, _ := newPromotionEnv(t)
	reqID := createRequest(t, srv, router)

	rec := getAs(t, srv, router, "vic", "/api/v1/me/promotion-requests")
	if rec.Code != http.StatusOK {
		t.Fatalf("list own requests = %d (%s)", rec.Code, rec.Body.String())
	}
	var mine []database.PromotionRequestDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].ID != reqID || mine[0].Status != database.AccessReqStatusPending {
		t.Fatalf("own requests = %+v, want one pending %s", mine, reqID)
	}
	if mine[0].RequesterLogin != "vic" {
		t.Errorf("requester_login = %q, want vic", mine[0].RequesterLogin)
	}

	// Someone else's request is not theirs to withdraw.
	seedUser(t, srv.db, "mallory", database.SystemRoleViewer)
	rec = sendJSONAs(t, srv, router, "mallory", http.MethodDelete, "/api/v1/me/promotion-requests/"+reqID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user withdraw = %d, want 404 — the request is still withdrawable by a stranger", rec.Code)
	}
	if got, _ := srv.db.GetAccessRequest(reqID); got == nil || got.Status != database.AccessReqStatusPending {
		t.Fatal("a stranger's DELETE changed the request")
	}

	rec = sendJSONAs(t, srv, router, "vic", http.MethodDelete, "/api/v1/me/promotion-requests/"+reqID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("withdraw = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	got, _ := srv.db.GetAccessRequest(reqID)
	if got == nil || got.Status != database.AccessReqStatusWithdrawn {
		t.Fatalf("status after withdraw = %+v, want withdrawn", got)
	}

	// Withdrawing frees the requester to ask again.
	createRequest(t, srv, router)
}

func TestPromotionApprove_BumpsTokenVersionAndNotifies(t *testing.T) {
	srv, router, fake := newPromotionEnv(t)
	reqID := createRequest(t, srv, router)

	before, err := srv.db.GetTokenVersion("vic")
	if err != nil {
		t.Fatal(err)
	}

	rec := sendJSONAs(t, srv, router, "root", http.MethodPost,
		"/api/v1/admin/promotion-requests/"+reqID+"/approve", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("approve = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}

	u, _ := srv.db.GetUser("vic")
	if u.SystemRole != database.SystemRolePublisher {
		t.Fatalf("role after approval = %q, want publisher", u.SystemRole)
	}
	after, err := srv.db.GetTokenVersion("vic")
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("token_version = %d, was %d — the promoted user keeps a stale-role session", after, before)
	}

	// The requester is told, and told that they have to sign in again.
	msgs := fake.messages()
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want the admin notice and the approval", len(msgs))
	}
	approval := msgs[1]
	if len(approval.To) != 1 || approval.To[0] != "vic@example.com" {
		t.Fatalf("approval went to %v, want the requester", approval.To)
	}
	if !strings.Contains(approval.Text, "Sign in again") {
		t.Error("the approval email does not tell the requester their session was invalidated")
	}
	if !strings.Contains(approval.Text, "root") {
		t.Error("the approval email does not say who approved it")
	}
}

func TestPromotionDeny_EmailsTheReason(t *testing.T) {
	srv, router, fake := newPromotionEnv(t)
	reqID := createRequest(t, srv, router)

	rec := sendJSONAs(t, srv, router, "root", http.MethodPost,
		"/api/v1/admin/promotion-requests/"+reqID+"/deny",
		`{"reason":"Complete the security training first."}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deny = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}

	msgs := fake.messages()
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want the admin notice and the denial", len(msgs))
	}
	denial := msgs[1]
	if len(denial.To) != 1 || denial.To[0] != "vic@example.com" {
		t.Fatalf("denial went to %v, want the requester", denial.To)
	}
	if !strings.Contains(denial.Text, "Complete the security training first.") {
		t.Error("the denial email does not carry the administrator's reason")
	}
	if u, _ := srv.db.GetUser("vic"); u.SystemRole != database.SystemRoleViewer {
		t.Error("a denial changed the requester's role")
	}
}

// Every notification is optional. With no mailer at all the workflow still
// works — which is the whole promise about email on this platform.
func TestPromotionFlow_WorksWithoutAnyMailer(t *testing.T) {
	db := dbtest.New(t)
	srv := newTestServerWithConfig(t, db, &config.Config{BaseURL: "http://localhost:8080"})
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "vic", database.SystemRoleViewer)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	reqID := createRequest(t, srv, router)
	rec := sendJSONAs(t, srv, router, "root", http.MethodPost,
		"/api/v1/admin/promotion-requests/"+reqID+"/approve", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("approve without a mailer = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if u, _ := srv.db.GetUser("vic"); u.SystemRole != database.SystemRolePublisher {
		t.Fatal("the promotion did not happen without a mailer")
	}
}

func TestPromotionList_FiltersAndJoins(t *testing.T) {
	srv, router, _ := newPromotionEnv(t)
	seedUser(t, srv.db, "vic2", database.SystemRoleViewer)

	pending := createRequest(t, srv, router)
	// A second requester whose request gets denied, so history has something
	// in it and the two filters are distinguishable.
	rec := sendJSONAs(t, srv, router, "vic2", http.MethodPost, "/api/v1/me/promotion-requests", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second requester = %d (%s)", rec.Code, rec.Body.String())
	}
	var second database.AccessRequest
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if rec := sendJSONAs(t, srv, router, "root", http.MethodPost,
		"/api/v1/admin/promotion-requests/"+second.ID+"/deny", `{"reason":"not yet"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("deny = %d (%s)", rec.Code, rec.Body.String())
	}

	list := func(t *testing.T, query string) []database.PromotionRequestDetail {
		t.Helper()
		rec := getAs(t, srv, router, "root", "/api/v1/admin/promotion-requests"+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s = %d (%s)", query, rec.Code, rec.Body.String())
		}
		var out []database.PromotionRequestDetail
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Default is the working queue.
	def := list(t, "")
	if len(def) != 1 || def[0].ID != pending {
		t.Fatalf("default listing = %+v, want only the pending request", def)
	}
	if def[0].RequesterLogin != "vic" || def[0].RequesterEmail != "vic@example.com" {
		t.Errorf("requester identity not joined: %+v", def[0])
	}

	denied := list(t, "?status=denied")
	if len(denied) != 1 || denied[0].ID != second.ID {
		t.Fatalf("denied listing = %+v", denied)
	}
	if denied[0].ApproverLogin != "root" {
		t.Errorf("approver_login = %q, want root", denied[0].ApproverLogin)
	}
	if denied[0].Reason != "not yet" {
		t.Errorf("reason = %q", denied[0].Reason)
	}

	if all := list(t, "?status=all"); len(all) != 2 {
		t.Fatalf("all listing = %d rows, want 2", len(all))
	}

	rec = getAs(t, srv, router, "root", "/api/v1/admin/promotion-requests?status=sideways")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown status = %d, want 400", rec.Code)
	}

	// The queue is admin-only.
	if rec := getAs(t, srv, router, "vic", "/api/v1/admin/promotion-requests"); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer listing the admin queue = %d, want 403", rec.Code)
	}
}

// A viewer refused at app creation must be told *why* in a form the UI can act
// on, or the point-of-need gate has nothing to key off.
func TestCreateApp_ForbiddenCarriesReasonCode(t *testing.T) {
	srv, router, _ := newPromotionEnv(t)

	rec := sendJSONAs(t, srv, router, "vic", http.MethodPost, "/api/v1/apps",
		`{"name":"thing","repo_url":"https://github.com/example/thing"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer creating an app = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	var body refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "deny_system_role" {
		t.Fatalf("403 body = %v, want code deny_system_role", body)
	}
}

// recordingPoster is a non-Slack channel: it records what it was asked to post.
type recordingPoster struct {
	mu    sync.Mutex
	base  string
	posts []recordedPost
}

type recordedPost struct {
	template, appID string
	data            notify.TemplateData
}

func (p *recordingPoster) PostAsync(template, appID string, d notify.TemplateData) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.posts = append(p.posts, recordedPost{template, appID, d})
	return true
}

func (p *recordingPoster) BaseURL() string { return p.base }

// VN-4 (2026-11-15): a publisher request is posted to every channel that
// carries the admin audience, not to Slack alone — a Teams bot connection with
// the admin audience ticked received nothing.
func TestPromotionRequest_PostsToEveryChannel(t *testing.T) {
	srv, router, _ := newPromotionEnv(t)
	poster := &recordingPoster{base: "https://teams-base.example"}
	WithPosters(poster, nil)(srv)

	createRequest(t, srv, router)

	poster.mu.Lock()
	defer poster.mu.Unlock()
	if len(poster.posts) != 1 {
		t.Fatalf("posted %d times, want 1", len(poster.posts))
	}
	got := poster.posts[0]
	if got.template != "promotion_requested" || got.appID != "" {
		t.Errorf("posted %q for app %q, want promotion_requested with no app", got.template, got.appID)
	}
	if !strings.HasPrefix(got.data.Link, "https://teams-base.example/") {
		t.Errorf("link %q is not under the poster's own base URL", got.data.Link)
	}
}
