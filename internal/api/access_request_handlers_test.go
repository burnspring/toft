package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

type accessReqEnv struct {
	srv       *Server
	router    http.Handler
	victimApp *database.App
	ownApp    *database.App
}

// newAccessReqEnv sets up the two publishers the escalation needs: "mallory",
// who owns nothing of interest, and "victim", who owns a private app.
func newAccessReqEnv(t *testing.T) *accessReqEnv {
	t.Helper()

	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	seedUser(t, srv.db, "mallory", database.SystemRolePublisher)
	seedUser(t, srv.db, "victim", database.SystemRolePublisher)

	victimApp := &database.App{
		Slug: "finance", Name: "Finance", OwnerID: "victim",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	ownApp := &database.App{
		Slug: "mallory-toy", Name: "Toy", OwnerID: "mallory",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}
	for _, a := range []*database.App{victimApp, ownApp} {
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
	}

	return &accessReqEnv{srv: srv, router: srv.Router(http.NotFoundHandler()), victimApp: victimApp, ownApp: ownApp}
}

func (e *accessReqEnv) post(t *testing.T, who, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *accessReqEnv) grantsOn(t *testing.T, appID string) []database.ObjectGrant {
	t.Helper()

	grants, err := e.srv.db.ListGrants(appID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	return grants
}

// The full escalation from finding 0.3: any publisher could grant themselves
// editor on any app by raising a request against the victim's app and
// approving it through an app they own themselves. The handler authorized
// against the URL's app; the query granted against the request row's object_id.
func TestApprovalCannotGrantAgainstAnotherApp(t *testing.T) {
	e := newAccessReqEnv(t)
	rungOff(t, e.srv)

	// Step 1: raise a request against the victim's app. This must now fail on
	// its own — mallory cannot read a private app they have no grant on.
	rec := e.post(t, "mallory",
		fmt.Sprintf("/api/v1/apps/%s/access-requests", e.victimApp.ID),
		`{"role":"editor"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("request against an unreadable app: status = %d, want 404", rec.Code)
	}

	// Even with a request row in hand — say the app had been readable when the
	// request was raised — approving it through mallory's own app must fail.
	req := &database.AccessRequest{
		RequesterID:   "mallory",
		RequestType:   database.AccessReqTypeObject,
		ObjectID:      e.victimApp.ID,
		RequestedRole: database.ObjectRoleEditor,
		ApproverID:    "victim",
	}
	if err := e.srv.db.CreateAccessRequest(t.Context(), req, false, "", database.RequestMeta{}); err != nil {
		t.Fatalf("seed access request: %v", err)
	}

	// Step 2: approve it via the app mallory owns, where Authorize passes.
	rec = e.post(t, "mallory",
		fmt.Sprintf("/api/v1/apps/%s/access-requests/%s/approve", e.ownApp.ID, req.ID),
		``)
	if rec.Code == http.StatusNoContent {
		t.Error("cross-app approval succeeded — the escalation is still open")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	// Step 3: and no grant may exist on the victim's app.
	for _, g := range e.grantsOn(t, e.victimApp.ID) {
		if g.PrincipalID == "mallory" {
			t.Fatalf("a grant was written on the victim app: %+v", g)
		}
	}
}

// Denial had the same shape: any app owner could deny any pending request on
// any app.
func TestDenialCannotReachAnotherAppsRequest(t *testing.T) {
	e := newAccessReqEnv(t)

	req := &database.AccessRequest{
		RequesterID:   "mallory",
		RequestType:   database.AccessReqTypeObject,
		ObjectID:      e.victimApp.ID,
		RequestedRole: database.ObjectRoleViewer,
		ApproverID:    "victim",
	}
	if err := e.srv.db.CreateAccessRequest(t.Context(), req, false, "", database.RequestMeta{}); err != nil {
		t.Fatalf("seed access request: %v", err)
	}

	rec := e.post(t, "mallory",
		fmt.Sprintf("/api/v1/apps/%s/access-requests/%s/deny", e.ownApp.ID, req.ID),
		`{"reason":"no"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	got, err := e.srv.db.GetAccessRequest(req.ID)
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if got.Status != database.AccessReqStatusPending {
		t.Errorf("request status = %q, want it left pending", got.Status)
	}
}

// A withdrawal is scoped by the URL's app as well as by the requester: the
// requester's own request, named under some other app's id, is not found.
func TestWithdrawalIsScopedByTheURLsApp(t *testing.T) {
	e := newAccessReqEnv(t)
	req := &database.AccessRequest{
		RequesterID:   "mallory",
		RequestType:   database.AccessReqTypeObject,
		ObjectID:      e.victimApp.ID,
		RequestedRole: database.ObjectRoleViewer,
		ApproverID:    "victim",
	}
	if err := e.srv.db.CreateAccessRequest(t.Context(), req, false, "", database.RequestMeta{}); err != nil {
		t.Fatalf("seed access request: %v", err)
	}

	withdraw := func(appID string) int {
		r := httptest.NewRequest(http.MethodDelete,
			fmt.Sprintf("/api/v1/apps/%s/access-requests/%s", appID, req.ID), nil)
		r.AddCookie(sessionCookieFor(t, e.srv, "mallory"))
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := withdraw(e.ownApp.ID); code != http.StatusNotFound {
		t.Errorf("withdraw under another app = %d, want 404", code)
	}
	if got, _ := e.srv.db.GetAccessRequest(req.ID); got == nil || got.Status != database.AccessReqStatusPending {
		t.Fatalf("the request was withdrawn through another app's URL")
	}
	if code := withdraw(e.victimApp.ID); code != http.StatusNoContent {
		t.Errorf("withdraw under its own app = %d, want 204", code)
	}
}

// The owner's own approval still has to work, or the fix has broken the feature.
func TestOwnerCanStillApproveARequestOnTheirOwnApp(t *testing.T) {
	e := newAccessReqEnv(t)

	req := &database.AccessRequest{
		RequesterID:   "mallory",
		RequestType:   database.AccessReqTypeObject,
		ObjectID:      e.victimApp.ID,
		RequestedRole: database.ObjectRoleEditor,
		ApproverID:    "victim",
	}
	if err := e.srv.db.CreateAccessRequest(t.Context(), req, false, "", database.RequestMeta{}); err != nil {
		t.Fatalf("seed access request: %v", err)
	}

	rec := e.post(t, "victim",
		fmt.Sprintf("/api/v1/apps/%s/access-requests/%s/approve", e.victimApp.ID, req.ID),
		``)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner approval: status = %d, want 204: %s", rec.Code, rec.Body.String())
	}

	var found bool
	for _, g := range e.grantsOn(t, e.victimApp.ID) {
		if g.PrincipalID == "mallory" && g.Role == database.ObjectRoleEditor {
			found = true
		}
	}
	if !found {
		t.Error("the owner's approval did not write the grant")
	}
}
