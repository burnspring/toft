package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// GET /api/v1/app/viewer — who is looking at this app.
//
// Two claims run through everything here. An app is told about the person in
// front of it and nothing else, and the three answers it can get — here they
// are, nobody is there, you may not be told — never collapse into each other.

// --- Harness ---

type viewerFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App // private, viewer identity on
	public *database.App // public, viewer identity on
	off    *database.App // private, viewer identity off — a new app's ordinary state
	token  string        // the app's tofta_ secret
}

func newViewerFixture(t *testing.T) *viewerFixture {
	t.Helper()

	srv := newTestServer(t)
	f := &viewerFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "edith", database.SystemRolePublisher)
	seedUser(t, srv.db, "priya", database.SystemRoleViewer)
	seedUser(t, srv.db, "sam", database.SystemRoleViewer)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	f.app = f.createApp(t, "recon", database.VisibilityAuthenticated, true)
	f.public = f.createApp(t, "status", database.VisibilityPublic, true)
	f.off = f.createApp(t, "fresh", database.VisibilityAuthenticated, false)

	f.token = f.mintToken(t, f.app.ID)
	return f
}

func (f *viewerFixture) createApp(t *testing.T, slug, visibility string, identity bool) *database.App {
	t.Helper()
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: "owner",
		Framework: "streamlit", Status: database.AppStatusRunning,
		Visibility: visibility,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app %s: %v", slug, err)
	}
	if identity {
		if err := f.srv.db.SetAppViewerIdentity(app.ID, true, "root", "", "", ""); err != nil {
			t.Fatalf("enable viewer identity on %s: %v", slug, err)
		}
		app.ViewerIdentity = true
	}
	return app
}

func (f *viewerFixture) mintToken(t *testing.T, appID string) string {
	t.Helper()
	secret := appTokenSecretPrefix + appID + "-secret"
	if err := f.srv.db.CreateAppToken(&database.AppToken{
		ID: "tok-" + appID, AppID: appID, TokenHash: hashAPIKey(secret), Prefix: secret[:8],
	}); err != nil {
		t.Fatalf("create app token: %v", err)
	}
	return secret
}

// actFor mints the blob the proxy would have set, for a live session of `who`.
func (f *viewerFixture) actFor(t *testing.T, who, appID string) string {
	t.Helper()
	version, err := f.srv.db.GetTokenVersion(who)
	if err != nil {
		t.Fatalf("token version for %s: %v", who, err)
	}
	return f.srv.jwt.MintActFor(who, appID, version, time.Now().Add(time.Hour))
}

// ask drives one /app/viewer request as the app holding token.
func (f *viewerFixture) ask(t *testing.T, token, blob string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/app/viewer", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.ContentLength = 0
	if blob != "" {
		req.Header.Set(auth.ActForHeader, blob)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

type viewerIdentity struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	IsAdmin bool   `json:"is_admin"`
	Groups  []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"groups"`
}

func decodeIdentity(t *testing.T, rec *httptest.ResponseRecorder) viewerIdentity {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out viewerIdentity
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return out
}

func (f *viewerFixture) grant(t *testing.T, appID, who, role string) {
	t.Helper()
	if err := f.srv.db.UpsertGrant(context.Background(), appID,
		database.PrincipalTypeUser, who, role, "root", database.RequestMeta{}); err != nil {
		t.Fatalf("granting %s %s: %v", who, role, err)
	}
}

// --- The three answers, and that they never collapse ---

// The one the changed default makes essential. A new app's ordinary state is
// off, and an app told "nobody is there" when somebody is would render its
// signed-out state to a signed-in person — with no reason for its author to
// suspect a setting rather than a bug in their own code.
func TestViewerIdentityOffIs403AndNever204(t *testing.T) {
	f := newViewerFixture(t)
	token := f.mintToken(t, f.off.ID)

	rec := f.ask(t, token, f.actFor(t, "priya", f.off.ID))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an app that was never enabled (%s)",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "viewer_identity_disabled") {
		t.Errorf("body = %s, want the code an SDK raises on", rec.Body.String())
	}
	// A refusal names the remedy, or it is a morning of guessing.
	if !strings.Contains(rec.Body.String(), "administrator") {
		t.Errorf("body = %s, want the refusal to say who can fix it", rec.Body.String())
	}
}

// The gateway mints an anonymous blob for a visitor to a public app with no
// session, so an app asking who is looking reaches this route for the whole
// public. Off says *there is somebody and you may not be told who*; an
// anonymous visitor is nobody, whatever the setting, and the 403 turned every
// public page that asks into a 500 for everybody who was not signed in.
func TestAnAnonymousVisitorIsNobodyEvenWithIdentityOff(t *testing.T) {
	f := newViewerFixture(t)
	public := f.createApp(t, "billboard", database.VisibilityPublic, false)
	token := f.mintToken(t, public.ID)

	rec := f.ask(t, token, f.srv.jwt.MintAnonymousActFor(public.ID, time.Now().Add(time.Hour)))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for an anonymous visitor with identity off (%s)",
			rec.Code, rec.Body.String())
	}
}

// No blob is the anonymous visitor of a public app, and the viewer holding a
// personal toftu_ key — a person, but not a session, so the proxy mints nothing
// for them. Neither is an error and neither is somebody to describe.
func TestViewerIsAbsentWithoutABlob(t *testing.T) {
	f := newViewerFixture(t)

	rec := f.ask(t, f.token, "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 with no act-for header (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing", rec.Body.String())
	}
}

// The case that justifies reading the role live at all: a Streamlit socket is
// held open for hours, so a viewer whose grant is revoked keeps a page that
// still works. The app asks who is there and is told nobody.
func TestARevokedViewerBecomesNobodyWhileTheirBlobIsStillValid(t *testing.T) {
	f := newViewerFixture(t)
	private := f.createApp(t, "ledger", database.VisibilityPrivate, true)
	token := f.mintToken(t, private.ID)
	f.grant(t, private.ID, "priya", database.ObjectRoleViewer)

	blob := f.actFor(t, "priya", private.ID)
	if got := decodeIdentity(t, f.ask(t, token, blob)); got.ID != "priya" {
		t.Fatalf("id = %q, want priya before the grant is revoked", got.ID)
	}

	if _, err := f.srv.db.DeleteGrant(context.Background(), private.ID,
		database.PrincipalTypeUser, "priya", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("revoking priya's grant: %v", err)
	}

	// The same blob, unexpired and correctly signed.
	rec := f.ask(t, token, blob)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for a viewer who may no longer read (%s)",
			rec.Code, rec.Body.String())
	}
}

// --- What the answer contains ---

func TestViewerRoleMirrorsTheGrant(t *testing.T) {
	f := newViewerFixture(t)
	f.grant(t, f.app.ID, "edith", database.ObjectRoleEditor)

	for _, tc := range []struct {
		who, want string
	}{
		{"owner", database.ObjectRoleOwner},
		{"edith", database.ObjectRoleEditor},
		// No grant, and readable only because the app is visible to anyone
		// signed in. `authenticated` is the platform's bookkeeping about *how*
		// read access was obtained, and an app has no use for it.
		{"priya", database.ObjectRoleViewer},
	} {
		t.Run(tc.who, func(t *testing.T) {
			got := decodeIdentity(t, f.ask(t, f.token, f.actFor(t, tc.who, f.app.ID)))
			if got.Role != tc.want {
				t.Errorf("role = %q, want %q", got.Role, tc.want)
			}
			if got.IsAdmin {
				t.Errorf("is_admin = true for %s, who administers nothing", tc.who)
			}
			if got.ID != tc.who {
				t.Errorf("id = %q, want %q", got.ID, tc.who)
			}
		})
	}
}

// The case that separates the gate from the ladder. EffectiveObjectRole has no
// system_admin bypass by design, so an administrator with no grant on a private
// app resolves to "none" there — while CanRead says yes, because Authorize
// applied the bypass first. Testing the ladder would answer *no viewer* to the
// one person with full authority over the app.
func TestAnAdministratorWithNoGrantIsAViewerAndNever204(t *testing.T) {
	f := newViewerFixture(t)
	private := f.createApp(t, "payroll", database.VisibilityPrivate, true)
	token := f.mintToken(t, private.ID)

	rec := f.ask(t, token, f.actFor(t, "root", private.ID))
	if rec.Code == http.StatusNoContent {
		t.Fatal("an administrator reading a private app answered 204: the handler is " +
			"testing the role ladder instead of the read gate")
	}
	got := decodeIdentity(t, rec)
	if got.Role != database.ObjectRoleViewer {
		t.Errorf("role = %q, want viewer — no grant is the literal truth", got.Role)
	}
	if !got.IsAdmin {
		t.Error("is_admin = false for an administrator, so an app gating on role " +
			"alone cannot tell them from an ordinary reader")
	}
}

// An administrator who *is* the owner is an ordinary case, and it is why is_admin
// is a boolean beside the role rather than a fourth value of it: a single enum
// would force a precedence rule that loses one of the two facts.
func TestAnAdministratorWhoOwnsTheAppIsBoth(t *testing.T) {
	f := newViewerFixture(t)
	f.grant(t, f.app.ID, "root", database.ObjectRoleOwner)

	got := decodeIdentity(t, f.ask(t, f.token, f.actFor(t, "root", f.app.ID)))
	if got.Role != database.ObjectRoleOwner || !got.IsAdmin {
		t.Errorf("role = %q is_admin = %v, want owner + true", got.Role, got.IsAdmin)
	}
}

// Every group the person is in, not only the ones holding a grant here. A domain
// rule belongs on `finance` or `approvers`, and those are usually not the groups
// that explain the app's access — a filtered list would answer "not in finance"
// about somebody who is.
func TestGroupsAreAllOfThemAndCarryNames(t *testing.T) {
	f := newViewerFixture(t)

	ctx := context.Background()
	finance, err := f.srv.db.CreateGroup(ctx, "finance", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := f.srv.db.AddGroupMember(ctx, finance.ID, "priya", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	got := decodeIdentity(t, f.ask(t, f.token, f.actFor(t, "priya", f.app.ID)))
	if len(got.Groups) != 1 {
		t.Fatalf("groups = %+v, want the one group priya is in even though it holds "+
			"no grant on this app", got.Groups)
	}
	if got.Groups[0].Name != "finance" {
		t.Errorf("group name = %q, want finance — an id an app cannot map is no use",
			got.Groups[0].Name)
	}
	if got.Groups[0].ID == "" {
		t.Error("group id is empty")
	}
}

// The address is the one field of the seven that is useful after the person has
// closed the tab, and a public app's visitors did not consent to a mailing list
// by visiting. Everything else still answers in full.
func TestEmailIsWithheldOnAPublicAppAndPresentOtherwise(t *testing.T) {
	f := newViewerFixture(t)
	publicToken := f.mintToken(t, f.public.ID)

	pub := decodeIdentity(t, f.ask(t, publicToken, f.actFor(t, "priya", f.public.ID)))
	if pub.Email != "" {
		t.Errorf("email = %q on a public app, want it withheld", pub.Email)
	}
	if pub.ID != "priya" || pub.Role == "" {
		t.Errorf("identity = %+v, want the rest of the answer intact", pub)
	}

	priv := decodeIdentity(t, f.ask(t, f.token, f.actFor(t, "priya", f.app.ID)))
	if priv.Email == "" {
		t.Error("email is missing on an app that is not public")
	}
}

// --- The blob is still the blob ---

func TestViewerRefusesABlobMintedForAnotherApp(t *testing.T) {
	f := newViewerFixture(t)
	other := f.createApp(t, "elsewhere", database.VisibilityAuthenticated, true)

	rec := f.ask(t, f.token, f.actFor(t, "priya", other.ID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a blob minted for another app (%s)",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "act_for_wrong_app") {
		t.Errorf("body = %s, want the wrong-app code", rec.Body.String())
	}
}

func TestViewerRefusesAnExpiredBlob(t *testing.T) {
	f := newViewerFixture(t)
	version, err := f.srv.db.GetTokenVersion("priya")
	if err != nil {
		t.Fatalf("token version: %v", err)
	}
	stale := f.srv.jwt.MintActFor("priya", f.app.ID, version, time.Now().Add(-time.Minute))

	rec := f.ask(t, f.token, stale)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an expired blob (%s)", rec.Code, rec.Body.String())
	}
}

func TestViewerRefusesAForgedBlob(t *testing.T) {
	f := newViewerFixture(t)

	rec := f.ask(t, f.token, f.actFor(t, "priya", f.app.ID)+"x")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a tampered signature (%s)", rec.Code, rec.Body.String())
	}
}

// --- The route itself ---

// The app surface is tofta_-only, and a walk of the router is what keeps that
// true as routes are added — a session cookie or a personal key reaching this
// one would hand a person's identity to whoever holds either.
func TestViewerRouteIsAppTokenOnly(t *testing.T) {
	f := newViewerFixture(t)

	// A session cookie for the app's own owner: the most plausible wrong
	// credential, and the one an implementation would most easily let through.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/app/viewer", nil)
	req.AddCookie(sessionCookieFor(t, f.srv, "owner"))
	req.RemoteAddr = "127.0.0.1:34567"
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a session cookie on the app surface (%s)",
			rec.Code, rec.Body.String())
	}

	// And the route is where it is claimed to be, with no id in it.
	found := false
	_ = chi.Walk(f.srv.Router(http.NotFoundHandler()).(chi.Routes),
		func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			if route == "/api/v1/app/viewer" && method == http.MethodGet {
				found = true
			}
			return nil
		})
	if !found {
		t.Error("GET /api/v1/app/viewer is not registered")
	}
}

// --- The switch: an administrator writes it, an owner reads it ---

// The pair most likely to be got right in one place and wrong in the other, so
// it is tested from both sides.
func TestOnlyAnAdministratorMayEnableViewerIdentity(t *testing.T) {
	f := newViewerFixture(t)

	patch := func(who string, on bool) *httptest.ResponseRecorder {
		body := `{"viewer_identity":` + map[bool]string{true: "true", false: "false"}[on] + `}`
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+f.off.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sessionCookieFor(t, f.srv, who))
		req.RemoteAddr = "127.0.0.1:34567"
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}

	// The owner may edit everything else about this app and not this.
	if rec := patch("owner", true); rec.Code != http.StatusForbidden {
		t.Fatalf("owner write = %d, want 403 — an owner asking for their own app to "+
			"learn who its visitors are is the request that wants a second person in it (%s)",
			rec.Code, rec.Body.String())
	}
	if app, _ := f.srv.db.GetApp(f.off.ID); app.ViewerIdentity {
		t.Fatal("the owner's refused write landed anyway")
	}

	if rec := patch("root", true); rec.Code != http.StatusOK {
		t.Fatalf("administrator write = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	app, _ := f.srv.db.GetApp(f.off.ID)
	if !app.ViewerIdentity {
		t.Fatal("the administrator's write did not land")
	}
}

// Reading is the other half: an owner must be able to see *why* their toft.who()
// call is failing without filing a ticket to find out.
func TestTheOwnerSeesTheSwitchAndAViewerDoesNot(t *testing.T) {
	f := newViewerFixture(t)
	f.grant(t, f.app.ID, "priya", database.ObjectRoleViewer)

	read := func(who string) *struct {
		Policy struct {
			ViewerIdentity *bool `json:"viewer_identity"`
		} `json:"policy"`
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+f.app.ID, nil)
		req.AddCookie(sessionCookieFor(t, f.srv, who))
		req.RemoteAddr = "127.0.0.1:34567"
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s reading the app = %d (%s)", who, rec.Code, rec.Body.String())
		}
		out := &struct {
			Policy struct {
				ViewerIdentity *bool `json:"viewer_identity"`
			} `json:"policy"`
		}{}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return out
	}

	if got := read("owner").Policy.ViewerIdentity; got == nil || !*got {
		t.Errorf("the owner cannot see that viewer identity is on: %v", got)
	}
	if got := read("priya").Policy.ViewerIdentity; got != nil {
		t.Errorf("a viewer was shown the switch: %v", got)
	}
}

// Flipping it is an authorization-relevant mutation, so it is on a durable row
// in the same transaction as the change.
func TestEnablingViewerIdentityIsAudited(t *testing.T) {
	f := newViewerFixture(t)

	if err := f.srv.db.SetAppViewerIdentity(f.off.ID, true, "root", "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("SetAppViewerIdentity: %v", err)
	}

	var before, after, actor string
	err := f.srv.db.Conn().QueryRow(
		`SELECT before, after, COALESCE(actor_id,'') FROM audit_log
		 WHERE action = 'app.viewer_identity.change' AND target_id = ?`, f.off.ID).
		Scan(&before, &after, &actor)
	if err != nil {
		t.Fatalf("no app.viewer_identity.change row — the change and its record can "+
			"come apart: %v", err)
	}
	if !strings.Contains(after, "true") || !strings.Contains(before, "false") {
		t.Errorf("before/after = %q/%q, want the flip in both directions", before, after)
	}
	if actor != "root" {
		t.Errorf("actor = %q, want the administrator who decided", actor)
	}
}

// A static app has no process to ask who is looking: turning viewer identity
// on is refused 409 static_app, as storage and integrations are, and turning
// it off still works (drills 2026-11-03).
func TestViewerIdentityIsRefusedOnAStaticApp(t *testing.T) {
	f := newViewerFixture(t)
	app := &database.App{Slug: "files", Name: "files", OwnerID: "owner",
		Framework: frameworkStatic, Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+app.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sessionCookieFor(t, f.srv, "root"))
		req.RemoteAddr = "127.0.0.1:34567"
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}
	if rec := patch(`{"viewer_identity":true}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "static_app") {
		t.Fatalf("on = %d %s, want 409 static_app", rec.Code, rec.Body.String())
	}
	if got, _ := f.srv.db.GetApp(app.ID); got.ViewerIdentity {
		t.Fatal("the refused write landed")
	}
	if rec := patch(`{"viewer_identity":false}`); rec.Code != http.StatusOK {
		t.Fatalf("off = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// A rendered document is served as files too, so viewer identity is refused
// on it, 409 document_app with a hint; turning it off still works (2026-11-04).
func TestViewerIdentityIsRefusedOnADocumentApp(t *testing.T) {
	f := newViewerFixture(t)
	for _, fw := range []string{"notebook", "rmarkdown", "quarto"} {
		app := &database.App{Slug: "doc-" + fw, Name: "doc " + fw, OwnerID: "owner",
			Framework: fw, Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
		if err := f.srv.db.CreateApp(app); err != nil {
			t.Fatal(err)
		}
		patch := func(body string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+app.ID, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(sessionCookieFor(t, f.srv, "root"))
			req.RemoteAddr = "127.0.0.1:34567"
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)
			return rec
		}
		rec := patch(`{"viewer_identity":true}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"document_app"`) ||
			!strings.Contains(rec.Body.String(), `"hint"`) {
			t.Fatalf("%s on = %d %s, want 409 document_app with a hint", fw, rec.Code, rec.Body.String())
		}
		if got, _ := f.srv.db.GetApp(app.ID); got.ViewerIdentity {
			t.Fatalf("%s: the refused write landed", fw)
		}
		if rec := patch(`{"viewer_identity":false}`); rec.Code != http.StatusOK {
			t.Fatalf("%s off = %d %s, want 200", fw, rec.Code, rec.Body.String())
		}
	}
}

// A batch app serves no pages, so viewer identity is refused on it, 409
// batch_app with a hint, as on a static or a document app (BB-007).
func TestViewerIdentityIsRefusedOnABatchApp(t *testing.T) {
	f := newViewerFixture(t)
	app := &database.App{Slug: "batch-vi", Name: "batch vi", OwnerID: "owner",
		Framework: "batch-r", Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+app.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sessionCookieFor(t, f.srv, "root"))
		req.RemoteAddr = "127.0.0.1:34567"
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}
	rec := patch(`{"viewer_identity":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"batch_app"`) ||
		!strings.Contains(rec.Body.String(), `"hint"`) {
		t.Fatalf("on = %d %s, want 409 batch_app with a hint", rec.Code, rec.Body.String())
	}
	if got, _ := f.srv.db.GetApp(app.ID); got.ViewerIdentity {
		t.Fatal("the refused write landed")
	}
	if rec := patch(`{"viewer_identity":false}`); rec.Code != http.StatusOK {
		t.Fatalf("off = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// D-105: every refusal of the act-for token carries a hint — act_for_invalid
// reached all four SDKs with hint null, and it is the one an app author can
// only fix in their own code.
func TestEveryActForRefusalCarriesAHint(t *testing.T) {
	f := newViewerFixture(t)
	other := f.createApp(t, "elsewhere-hint", database.VisibilityAuthenticated, true)
	for _, c := range []struct {
		name, blob, code string
		status           int
	}{
		{"forged", "a1.not-a-real-token", "act_for_invalid", http.StatusUnauthorized},
		{"another app's", f.actFor(t, "priya", other.ID), "act_for_wrong_app", http.StatusForbidden},
	} {
		rec := f.ask(t, f.token, c.blob)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		hint, _ := body["hint"].(string)
		if rec.Code != c.status || body["code"] != c.code || hint == "" {
			t.Errorf("%s blob: %d %s, want %d %s with a hint", c.name, rec.Code, rec.Body.String(), c.status, c.code)
		}
		if !strings.Contains(hint, auth.ActForHeader) {
			t.Errorf("%s blob: hint %q does not name the %s header", c.name, hint, auth.ActForHeader)
		}
	}
}
