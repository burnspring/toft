package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// What an app can reach, over HTTP.
//
// The test that leads is the silent one: a credential value in a field nobody
// expected, which nobody notices until it is in a screenshot in a ticket. The
// rest are the gates and the substitution, and they fail loudly.

type reachFixture struct {
	srv    *Server
	router http.Handler
}

func newReachFixture(t *testing.T) *reachFixture {
	t.Helper()
	srv := newTestServerWithConfig(t, dbtest.New(t), &config.Config{
		BaseURL: "https://toft.example", AppsBaseURL: "https://apps.example",
		Port: 8080, AppsPort: 8081,
	})
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "onlooker", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	return &reachFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}
}

func (f *reachFixture) app(t *testing.T, slug, visibility string) *database.App {
	t.Helper()
	return mkTestApp(t, f.srv, slug, "owner", visibility, "flask")
}

// TestReachCarriesNoCredentialValue is the silent one.
//
// The app is given every kind of thing that has a value behind it — a
// credential with two secrets in it, a sensitive environment variable, a
// non-sensitive one — and the assertion is on the *bytes*: no value appears
// anywhere in the response, under any key, at any depth.
//
// Names, never values. A governance reader must not become the second route
// that reads a credential, and it does not need to be: which credential an app
// holds is the whole governance fact.
func TestReachCarriesNoCredentialValue(t *testing.T) {
	f := newReachFixture(t)
	app := f.app(t, "payroll", database.VisibilityAuthenticated)

	const dbPassword = "hunter2-the-production-database"
	const apiToken = "sk-live-do-not-print-me"
	cred, err := f.srv.db.CreateCredential(context.Background(), "production-db", "",
		map[string]string{"PASSWORD": dbPassword, "TOKEN": apiToken},
		func(v string) (string, error) { return "enc:" + v, nil },
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "root"})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := f.srv.db.AttachCredential(context.Background(), app.ID, cred.ID,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "root"}); err != nil {
		t.Fatalf("AttachCredential: %v", err)
	}

	const secretEnv = "s3cret-env-value"
	const plainEnv = "eu-west-1"
	// Stored as the platform stores them — a sensitive value is encrypted at
	// rest and a plain one is not, and neither may appear below.
	if err := f.srv.db.SetEnvVar(app.ID, "STRIPE_KEY", secretEnv, true); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}
	if err := f.srv.db.SetEnvVar(app.ID, "AWS_REGION", plainEnv, false); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}

	rec := sendAs(t, f.srv, f.router, "owner", http.MethodGet, "/api/v1/apps/"+app.ID+"/reach", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, value := range []string{dbPassword, apiToken, secretEnv, plainEnv} {
		if strings.Contains(body, value) {
			t.Errorf("a value appears in the reach response: %q\n"+
				"Names, never values — this reader must not become the second route that reads one.", value)
		}
	}

	// And the names are there, because a reader that hid them would be safe
	// and useless.
	var out appReachResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Credentials) != 1 || out.Credentials[0].Name != "production-db" {
		t.Fatalf("credentials = %+v, want one named production-db", out.Credentials)
	}
	if got := out.Credentials[0].Keys; len(got) != 2 {
		t.Errorf("credential key names = %v, want both — which values a rotation touches is the point", got)
	}
	if len(out.EnvKeys) != 2 {
		t.Errorf("env keys = %+v, want both listed by name", out.EnvKeys)
	}
	for _, e := range out.EnvKeys {
		if e.Key == "STRIPE_KEY" && !e.Sensitive {
			t.Error("a sensitive env var is not marked sensitive")
		}
	}
}

// A viewer is refused, and the storage route beside it still answers.
//
// This asserts the asymmetry D4 chose rather than tidying it away: `/storage`
// is deliberately open to anyone who can read the app — an owner needs to know
// their app has /data/reports — and the assembly is not, because the assembly
// says more than the sum of its parts. Removing either half later should be a
// decision, which is what this test makes it.
//
// **A viewer gets 403, not 404**, and the PRD said 404. appForEdit answers 404
// only to a caller who cannot read the app at all; to one who can, it answers
// 403, because 404 would be a lie the caller can disprove by opening the app in
// the next tab. Dependencies, on the same gate, behaves identically. Both
// statuses are asserted here so the distinction is the thing under test rather
// than an accident of which fixture user was used.
func TestReachIsRefusedToAViewer(t *testing.T) {
	f := newReachFixture(t)
	app := f.app(t, "payroll", database.VisibilityPrivate)
	if err := f.srv.db.UpsertGrant(context.Background(), app.ID,
		database.PrincipalTypeUser, "onlooker", auth.ObjectRoleViewer, "owner",
		database.RequestMeta{}); err != nil {
		t.Fatalf("granting viewer: %v", err)
	}

	rec := sendAs(t, f.srv, f.router, "onlooker", http.MethodGet, "/api/v1/apps/"+app.ID+"/reach", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer got %d from /reach, want 403 — the same refusal Dependencies gives", rec.Code)
	}
	// Somebody with no grant, on an app they can still *list*: 403 with
	// `deny_list_only`, the scoped exception writeAuthzError already makes on
	// every app route. 404 on an id the same API just listed teaches the
	// caller that the list is unreliable.
	rec = sendAs(t, f.srv, f.router, "stranger", http.MethodGet, "/api/v1/apps/"+app.ID+"/reach", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a caller holding the list rung got %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "deny_list_only") {
		t.Errorf("the refusal does not carry the list-rung code: %s", rec.Body.String())
	}

	// And with the rung withdrawn, the app's existence is not theirs to learn:
	// 404, so nothing is enumerable that was not listed.
	if err := f.srv.db.SetAppListable(app.ID, true, false,
		database.PrincipalTypeUser, "owner", "", "", "", ""); err != nil {
		t.Fatalf("SetAppListable: %v", err)
	}
	rec = sendAs(t, f.srv, f.router, "stranger", http.MethodGet, "/api/v1/apps/"+app.ID+"/reach", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger got %d from /reach on an unlisted app, want 404", rec.Code)
	}

	// The half a viewer keeps. It needs storage to be configured at all —
	// without a driver the route 404s to everybody, which is the
	// optional-capabilities rule and not this asymmetry.
	f.srv.storage = &fakeProvisioner{}
	rec = sendAs(t, f.srv, f.router, "onlooker", http.MethodGet, "/api/v1/apps/"+app.ID+"/storage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the same viewer got %d from /storage, want 200 — this work does not narrow that read", rec.Code)
	}
}

// The owner appears once, not twice.
//
// Ownership is a column and a grant row naming the same principal at `owner`
// is written beside it — reporting both puts the owner under a heading that
// reads "everybody who has been given a rung on this app by name", which is
// one fact told twice. Somebody *else* holding an owner grant is a real second
// fact and stays.
func TestReachReportsTheOwnerOnce(t *testing.T) {
	f := newReachFixture(t)
	app := f.app(t, "payroll", database.VisibilityPrivate)
	if err := f.srv.db.UpsertGrant(context.Background(), app.ID,
		database.PrincipalTypeUser, "owner", auth.ObjectRoleOwner, "owner",
		database.RequestMeta{}); err != nil {
		t.Fatalf("granting the owner: %v", err)
	}
	if err := f.srv.db.UpsertGrant(context.Background(), app.ID,
		database.PrincipalTypeUser, "onlooker", auth.ObjectRoleOwner, "owner",
		database.RequestMeta{}); err != nil {
		t.Fatalf("granting a second owner: %v", err)
	}

	rec := sendAs(t, f.srv, f.router, "owner", http.MethodGet, "/api/v1/apps/"+app.ID+"/reach", "")
	var out appReachResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Owner == nil || out.Owner.ID != "owner" {
		t.Fatalf("owner = %+v, want the column's answer", out.Owner)
	}
	for _, g := range out.Grants {
		if g.PrincipalID == "owner" {
			t.Errorf("the owner is also listed as a grant: %+v", g)
		}
	}
	// And the other owner-rung grant survives, because it is somebody else.
	found := false
	for _, g := range out.Grants {
		if g.PrincipalID == "onlooker" {
			found = true
		}
	}
	if !found {
		t.Errorf("a second person's owner grant was dropped with the owner's: %+v", out.Grants)
	}
}

// The estate read is administrator-only and answers 404, never 403: the
// census's status, for the census's reason.
func TestEstateReachIsAdminOnly(t *testing.T) {
	f := newReachFixture(t)
	f.app(t, "payroll", database.VisibilityPrivate)

	for _, who := range []string{"", "owner"} {
		rec := sendAs(t, f.srv, f.router, who, http.MethodGet, "/api/v1/admin/estate/reach", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("caller %q got %d, want 404 — an admin read never says 403 here", who, rec.Code)
		}
	}

	rec := sendAs(t, f.srv, f.router, "root", http.MethodGet, "/api/v1/admin/estate/reach", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("an administrator got %d: %s", rec.Code, rec.Body.String())
	}
}

// The line naming what the platform cannot see is on both surfaces, always, and
// it names outbound network specifically.
//
// A hedge — "this may not be complete" — would be worse than nothing: it makes
// every entry suspect and names no gap. A page called Reach is where a control
// is easiest to oversell, and an oversold control produces confidence rather
// than safety.
func TestReachSaysWhatItCannotSee(t *testing.T) {
	f := newReachFixture(t)
	app := f.app(t, "payroll", database.VisibilityPrivate)

	rec := sendAs(t, f.srv, f.router, "owner", http.MethodGet, "/api/v1/apps/"+app.ID+"/reach", "")
	var one appReachResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	rec = sendAs(t, f.srv, f.router, "root", http.MethodGet, "/api/v1/admin/estate/reach", "")
	var all estateReachResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if one.CannotSee != all.CannotSee || one.CannotSee == "" {
		t.Fatalf("the two surfaces disagree about what the platform cannot see:\n app: %q\n estate: %q",
			one.CannotSee, all.CannotSee)
	}
	if !strings.Contains(strings.ToLower(one.CannotSee), "outbound") {
		t.Errorf("the sentence does not name outbound network: %q", one.CannotSee)
	}
}

// A withheld `public` app reads `authenticated` and carries the marker, on both
// surfaces. Pure, through the folds, no database.
//
// A report that disagreed with the authorizer would send somebody to test
// access the platform refuses, and the refusal would come back as a finding
// about an app behaving exactly as configured.
func TestReachReportsEffectiveVisibility(t *testing.T) {
	one := foldAppReach(database.AppReach{
		AppID: "a1", Slug: "payroll", Visibility: database.VisibilityPublic,
		AnonymityWithheld: true,
	}, nil, false, false)
	if one.Visibility != database.VisibilityAuthenticated {
		t.Errorf("app visibility = %q, want the effective word", one.Visibility)
	}
	if !one.VisibilityWithheld {
		t.Error("the app read does not mark the substitution")
	}

	all := foldEstateReach(database.EstateReachSet{
		Rows: []database.EstateReachRow{
			{AppID: "a1", Slug: "payroll", Visibility: database.VisibilityPublic, AnonymityWithheld: true},
			{AppID: "a2", Slug: "status", Visibility: database.VisibilityPublic},
		},
		AppsTotal: 2,
	}, false, false)
	if all.Rows[0].Visibility != database.VisibilityAuthenticated || !all.Rows[0].VisibilityWithheld {
		t.Errorf("withheld row = %+v, want authenticated and marked", all.Rows[0])
	}
	if all.Rows[1].Visibility != database.VisibilityPublic || all.Rows[1].VisibilityWithheld {
		t.Errorf("an unwithheld public app was substituted: %+v", all.Rows[1])
	}
	// The count comes from the same rows the table renders, so it cannot
	// disagree with them about what `public` means.
	if all.AppsPublic != 1 {
		t.Errorf("apps_public = %d, want 1 — the withheld app is not public in effect", all.AppsPublic)
	}
}

// An install that has provisioned no storage and connected no integration gets
// no section, rather than a section of zeros. Absent, not off: an app was not
// refused something the install never offered.
func TestReachOmitsCapabilitiesTheInstallDoesNotHave(t *testing.T) {
	f := newReachFixture(t)
	app := f.app(t, "payroll", database.VisibilityPrivate)

	rec := sendAs(t, f.srv, f.router, "owner", http.MethodGet, "/api/v1/apps/"+app.ID+"/reach", "")
	var out appReachResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.ShowStorage || out.ShowIntegrations {
		t.Errorf("an install with neither configured offers both sections: %+v", out)
	}
	// And the lists are empty rather than null, so a surface that does render
	// them does not have to guard every one.
	if out.Mounts == nil || out.Integrations == nil || out.Credentials == nil {
		t.Errorf("a nil list reached the wire: %+v", out)
	}
}

// An attachment whose connection was switched off is present and not
// effective, and both surfaces still show the integrations section for it.
// Gated on the install's offer alone, the section vanished the moment the last
// connection stopped offering itself (docs/bugs 2026-10-22-e2e-install, ARCH.10).
func TestAHeldAttachmentKeepsTheIntegrationsSectionWhenNothingIsOffered(t *testing.T) {
	held := []database.ReachIntegration{{Kind: "webhook", Connected: false, Attached: true, Enabled: true}}
	one := foldAppReach(database.AppReach{AppID: "a1", Slug: "hooked"}, held, false, false)
	if !one.ShowIntegrations {
		t.Error("the app's reach hides an attachment because its connection is switched off")
	}
	if none := foldAppReach(database.AppReach{AppID: "a2", Slug: "plain"}, nil, false, false); none.ShowIntegrations {
		t.Error("an app with nothing attached on an install offering nothing shows the section")
	}
	all := foldEstateReach(database.EstateReachSet{
		Rows: []database.EstateReachRow{{AppID: "a1", Slug: "hooked", Integrations: held}},
	}, false, false)
	if !all.ShowIntegrations {
		t.Error("the estate hides an attachment because its connection is switched off")
	}
}
