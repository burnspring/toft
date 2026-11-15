package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	mailer "toft/internal/mail"
)

func putGrantAs(t *testing.T, srv *Server, router http.Handler, who, appID, principalType, principalID, role string) *httptest.ResponseRecorder {
	t.Helper()

	body := fmt.Sprintf(`{"role":%q}`, role)
	req := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/v1/apps/%s/grants/%s/%s", appID, principalType, principalID),
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// A group may be granted editor as well as viewer, and owner through this
// API by nobody at all.
//
// The inverse of what this test used to assert. The group clamp is gone with
// ADR §51 — a group that can own an app can certainly hold the strictly weaker
// editor — but the rule it sat beside has not moved: owner is refused for
// every principal type, because ownership moves by transfer only and that is
// what keeps apps.owner_* and the owner grant row from desyncing.
func TestGroupGrantsMayBeWriteBearingButNeverOwner(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "shared", Name: "Shared", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	g, err := srv.db.CreateGroup(t.Context(), "team", "owner", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	// Owner: refused, and the refusal is about transfer rather than about
	// groups.
	rec := putGrantAs(t, srv, router, "owner", app.ID, "group", g.ID, database.ObjectRoleOwner)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("group owner grant: status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	// And a *user* owner grant is refused the same way, which is the evidence
	// that the rule is about the role and not about the principal type.
	seedUser(t, srv.db, "other", database.SystemRolePublisher)
	rec = putGrantAs(t, srv, router, "owner", app.ID, "user", "other", database.ObjectRoleOwner)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("user owner grant: status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}

	for _, role := range []string{database.ObjectRoleEditor, database.ObjectRoleViewer} {
		rec = putGrantAs(t, srv, router, "owner", app.ID, "group", g.ID, role)
		if rec.Code != http.StatusOK {
			t.Fatalf("group %s grant: status = %d, want 200 (body=%s)", role, rec.Code, rec.Body.String())
		}
		grants, err := srv.db.ListGrants(app.ID)
		if err != nil {
			t.Fatalf("list grants: %v", err)
		}
		found := false
		for _, gr := range grants {
			if gr.PrincipalType == database.PrincipalTypeGroup && gr.PrincipalID == g.ID {
				found = true
				if gr.Role != role {
					t.Fatalf("group grant role = %q, want %q", gr.Role, role)
				}
			}
		}
		if !found {
			t.Fatalf("group %s grant not persisted", role)
		}
	}
}

// The owner's grant row moves only via transfer: overwriting or deleting it
// through the grants API would desync it from apps.owner_id.
func TestOwnerGrantImmutableViaGrantsAPI(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "keep", Name: "Keep", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	rec := putGrantAs(t, srv, router, "owner", app.ID, "user", "owner", database.ObjectRoleViewer)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("demoting owner via PUT grant: %d, want 422", rec.Code)
	}

	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/v1/apps/%s/grants/user/owner", app.ID), nil)
	req.AddCookie(sessionCookieFor(t, srv, "owner"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("deleting owner grant: %d, want 422", rec.Code)
	}
}

// Directory searches are for authenticated principals only.
func TestDirectorySearchRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "someone", database.SystemRolePublisher)

	for _, path := range []string{"/api/v1/directory/users?q=ali", "/api/v1/directory/groups?q=team"} {
		if rec := getAs(t, srv, router, "", path); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d, want 401", path, rec.Code)
		}
		if rec := getAs(t, srv, router, "someone", path); rec.Code != http.StatusOK {
			t.Errorf("authenticated %s: %d, want 200", path, rec.Code)
		}
	}
}

// --- Telling somebody an app has been shared with them ---
//
// The notification is **opt-in per share** and never load-bearing: every case
// below asserts that the grant landed regardless, because a share that failed
// because a relay was down would be the notification invariant broken at the
// one place it matters most.

// shareEnv is an app somebody may share, a person to share it with, and a fake
// relay to watch.
type shareEnv struct {
	srv    *Server
	router http.Handler
	mail   *fakeMailer
	app    *database.App
}

func newShareEnv(t *testing.T) *shareEnv {
	t.Helper()
	db := dbtest.New(t)
	fake := &fakeMailer{enabled: true, db: db}
	srv := newTestServerWithConfig(t, db, &config.Config{BaseURL: "http://localhost:8080"}, WithMailer(fake))

	seedUser(t, db, "sharer", database.SystemRolePublisher)
	seedUser(t, db, "recipient", database.SystemRolePublisher)
	app := &database.App{
		Slug: "payroll", Name: "Payroll", OwnerID: "sharer",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return &shareEnv{srv: srv, router: srv.Router(http.NotFoundHandler()), mail: fake, app: app}
}

// share puts a grant with whatever body it is given, and returns the decoded
// answer.
func (e *shareEnv) share(t *testing.T, principalType, principalID, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/v1/apps/%s/grants/%s/%s", e.app.ID, principalType, principalID),
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookieFor(t, e.srv, "sharer"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("share = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the share answer: %v", err)
	}
	return out
}

func (e *shareEnv) sent(t *testing.T) []mailer.Message {
	t.Helper()
	e.mail.mu.Lock()
	defer e.mail.mu.Unlock()
	return append([]mailer.Message(nil), e.mail.sent...)
}

// The ordinary case: a person is added, asked to be told, and told — with the
// sharer's name and their own words, addressed to the person and nobody else.
func TestSharingWithAPersonNotifiesThemWithTheSharersWords(t *testing.T) {
	e := newShareEnv(t)
	out := e.share(t, database.PrincipalTypeUser, "recipient",
		`{"role":"viewer","notify":true,"note":"This is the payroll dashboard we discussed."}`)

	if out["notified"] != true {
		t.Fatalf("notified = %v, want true (%v)", out["notified"], out)
	}
	sent := e.sent(t)
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent))
	}
	msg := sent[0]
	if len(msg.To) != 1 || msg.To[0] != "recipient@example.com" {
		t.Errorf("addressed to %v, want the recipient alone", msg.To)
	}
	// AudienceApp, so an administrator's "stop telling my users things" switch
	// governs it — asserted here as well as in the mail package's table,
	// because the send site is what would quietly pick a different one.
	if msg.Audience != mailer.AudienceApp {
		t.Errorf("audience = %q, want %q", msg.Audience, mailer.AudienceApp)
	}
	if !strings.Contains(msg.Subject, "Payroll") || !strings.Contains(msg.Subject, "sharer") {
		t.Errorf("subject names neither the app nor the sharer: %q", msg.Subject)
	}
	if !strings.Contains(msg.Text, "payroll dashboard we discussed") {
		t.Errorf("the note did not reach the message: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "a viewer") {
		t.Errorf("the message does not say what access was given: %q", msg.Text)
	}
}

// **The default is silence.** A share with no `notify` sends nothing, which is
// what every client written before this existed does.
func TestSharingWithoutAskingToNotifySendsNothing(t *testing.T) {
	e := newShareEnv(t)
	out := e.share(t, database.PrincipalTypeUser, "recipient", `{"role":"viewer"}`)

	if out["notified"] != false {
		t.Errorf("notified = %v, want false", out["notified"])
	}
	if _, ok := out["notify_skipped"]; ok {
		// Nobody asked, which is not the same fact as asked-and-skipped.
		t.Errorf("an unasked-for notification reported a skip reason: %v", out)
	}
	if n := len(e.sent(t)); n != 0 {
		t.Errorf("sent %d messages without being asked", n)
	}
}

// Every refusal, and each one **names itself** — because the person who ticked
// the box would otherwise assume a message went out.
func TestEveryRefusalToNotifyIsNamedAndTheGrantStillLands(t *testing.T) {
	t.Run("a group is not a person", func(t *testing.T) {
		e := newShareEnv(t)
		g, err := e.srv.db.CreateGroup(context.Background(), "Finance", "sharer", database.RequestMeta{})
		if err != nil {
			t.Fatalf("create group: %v", err)
		}
		out := e.share(t, database.PrincipalTypeGroup, g.ID, `{"role":"viewer","notify":true}`)
		if out["notify_skipped"] != notifySkipNotAPerson {
			t.Errorf("skip = %v, want %q", out["notify_skipped"], notifySkipNotAPerson)
		}
		if n := len(e.sent(t)); n != 0 {
			t.Errorf("sent %d messages to a group", n)
		}
	})

	t.Run("a role change is not a new share", func(t *testing.T) {
		e := newShareEnv(t)
		e.share(t, database.PrincipalTypeUser, "recipient", `{"role":"viewer"}`)
		out := e.share(t, database.PrincipalTypeUser, "recipient", `{"role":"editor","notify":true}`)
		if out["notify_skipped"] != notifySkipNotNew {
			t.Errorf("skip = %v, want %q", out["notify_skipped"], notifySkipNotNew)
		}
		if n := len(e.sent(t)); n != 0 {
			t.Errorf("a role change sent %d messages", n)
		}
		// And the change itself happened.
		g, err := e.srv.db.GetGrant(e.app.ID, database.PrincipalTypeUser, "recipient")
		if err != nil || g == nil || g.Role != database.ObjectRoleEditor {
			t.Errorf("the role change did not land: %v (%v)", g, err)
		}
	})

	t.Run("no address on file", func(t *testing.T) {
		e := newShareEnv(t)
		if err := e.srv.db.UpsertUser(&database.User{ID: "recipient", Login: "recipient", Email: ""}); err != nil {
			t.Fatalf("clearing the address: %v", err)
		}
		out := e.share(t, database.PrincipalTypeUser, "recipient", `{"role":"viewer","notify":true}`)
		if out["notify_skipped"] != notifySkipNoAddress {
			t.Errorf("skip = %v, want %q", out["notify_skipped"], notifySkipNoAddress)
		}
		// The grant is what was asked for and it landed anyway.
		g, err := e.srv.db.GetGrant(e.app.ID, database.PrincipalTypeUser, "recipient")
		if err != nil || g == nil {
			t.Errorf("the share was lost with the notification: %v", err)
		}
	})

	// The administrator's app-audience switch. The UI does not offer the box in
	// this state; a caller that asks anyway is told rather than quietly ignored.
	t.Run("the app audience is switched off", func(t *testing.T) {
		e := newShareEnv(t)
		e.mail.appAudienceOff = true
		out := e.share(t, database.PrincipalTypeUser, "recipient", `{"role":"viewer","notify":true}`)
		if out["notify_skipped"] != notifySkipEmailOff {
			t.Errorf("skip = %v, want %q", out["notify_skipped"], notifySkipEmailOff)
		}
		if n := len(e.sent(t)); n != 0 {
			t.Errorf("sent %d messages with the audience off", n)
		}
	})
}

// A note is capped, and the refusal is the whole request — the alternative is a
// share that silently went out with a truncated message its author never wrote.
func TestAnOverlongNoteRefusesTheShareRatherThanTruncatingIt(t *testing.T) {
	e := newShareEnv(t)
	body := fmt.Sprintf(`{"role":"viewer","notify":true,"note":%q}`, strings.Repeat("x", noteLimit+1))
	req := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/api/v1/apps/%s/grants/user/recipient", e.app.ID), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookieFor(t, e.srv, "sharer"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an overlong note = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	g, err := e.srv.db.GetGrant(e.app.ID, database.PrincipalTypeUser, "recipient")
	if err != nil {
		t.Fatalf("GetGrant: %v", err)
	}
	if g != nil {
		t.Error("a refused request wrote the grant anyway")
	}
}

// **The checkbox is gated on the capability, not on a permission**, and the
// capability is the relay *and* the app-audience switch.
//
// It is asserted here rather than left to the UI because it is the mechanism
// that keeps "copy may not promise mail" true of a *control*: where this is
// false the sharing panel renders no checkbox at all, and a control that is
// merely disabled would still be making the claim.
func TestTheShareCheckboxIsOfferedOnlyWhenAMessageWouldGoOut(t *testing.T) {
	e := newShareEnv(t)

	read := func() bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+e.app.ID+"/permissions", nil)
		req.AddCookie(sessionCookieFor(t, e.srv, "sharer"))
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("permissions = %d (%s)", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding permissions: %v", err)
		}
		v, ok := out["can_notify_on_share"]
		if !ok {
			t.Fatal("permissions carry no can_notify_on_share")
		}
		return v == true
	}

	if !read() {
		t.Error("a configured relay with the app audience on offers no share notification")
	}

	// The administrator's "stop telling my users things" switch.
	e.mail.appAudienceOff = true
	if read() {
		t.Error("the checkbox is offered with the app audience switched off")
	}

	// And email off entirely.
	e.mail.appAudienceOff, e.mail.enabled = false, false
	if read() {
		t.Error("the checkbox is offered with email off")
	}
}
