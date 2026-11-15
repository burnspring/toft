package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
)

// meEnv is the shared fixture: a router, an administrator, and two ordinary
// users so that "the caller gets their own record" is a claim with something
// to be wrong about.
func meEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "alice", database.SystemRolePublisher)
	seedUser(t, srv.db, "bob", database.SystemRoleViewer)
	return srv, router
}

func decodeAccount(t *testing.T, body []byte) userDetailResponse {
	t.Helper()
	var got userDetailResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode /me/account: %v — body=%s", err, body)
	}
	return got
}

// The ordinary case: a non-administrator reads the record the administrator's
// /users/{id} would return for them, which today they cannot see at all.
func TestMyAccount_ReturnsTheCallersOwnRecord(t *testing.T) {
	srv, router := meEnv(t)

	rec := getAs(t, srv, router, "alice", "/api/v1/me/account")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/account as alice = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}

	got := decodeAccount(t, rec.Body.Bytes())
	if got.User == nil {
		t.Fatal("no user in the response")
	}
	if got.User.ID != "alice" {
		t.Fatalf("user.id = %q, want alice", got.User.ID)
	}
	if got.User.SystemRole != database.SystemRolePublisher {
		t.Fatalf("system_role = %q, want publisher", got.User.SystemRole)
	}
	// The activity row rather than the bare user — this is the pair the
	// dormancy sweep reads and the reason the endpoint returns UserActivity.
	if got.Groups == nil {
		t.Error("groups was null; want an empty array so the client need not special-case it")
	}
	if got.Identities == nil {
		t.Error("identities was null; want an empty array")
	}
}

// The point of D2, stated as a test. There is no id to supply, so there is no
// input that makes this handler answer about somebody else — bob's record is
// unreachable through this route by construction rather than by a check.
func TestMyAccount_TakesNoIDAndCannotBeAimedAtAnother(t *testing.T) {
	srv, router := meEnv(t)

	// Each caller gets themselves, from the same path.
	for _, who := range []string{"alice", "bob", "root"} {
		rec := getAs(t, srv, router, who, "/api/v1/me/account")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /me/account as %s = %d, want 200", who, rec.Code)
		}
		if got := decodeAccount(t, rec.Body.Bytes()); got.User.ID != who {
			t.Fatalf("as %s the record returned was %q", who, got.User.ID)
		}
	}

	// And no id-bearing variant of the path exists to be found.
	for _, path := range []string{
		"/api/v1/me/account/bob",
		"/api/v1/me/account?user_id=bob",
		"/api/v1/me/account?id=bob",
	} {
		rec := getAs(t, srv, router, "alice", path)
		if rec.Code == http.StatusOK {
			got := decodeAccount(t, rec.Body.Bytes())
			if got.User != nil && got.User.ID != "alice" {
				t.Fatalf("%s answered about %q — the route accepted an id", path, got.User.ID)
			}
			continue
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 or alice's own record", path, rec.Code)
		}
	}
}

// auth.Middleware lets anonymous callers through with a nil principal, so the
// handler has to refuse for itself. This is that refusal.
func TestMyAccount_AnonymousIsRefused(t *testing.T) {
	srv, router := meEnv(t)

	rec := getAs(t, srv, router, "", "/api/v1/me/account")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /me/account = %d, want 401 — body=%s", rec.Code, rec.Body.String())
	}
}

// The keyroutes classification, proved as behaviour rather than as a map
// entry: a personal key may read the record of the identity it acts as.
func TestMyAccount_AnswersAPersonalAPIKey(t *testing.T) {
	srv, router := meEnv(t)
	enablePersonalKeys(t, srv, true)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	rec := getWithKey(t, router, secret, "/api/v1/me/account")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/account with an toftu_ key = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeAccount(t, rec.Body.Bytes()); got.User.ID != "alice" {
		t.Fatalf("the key's record was %q, want alice — a key acts as its owner", got.User.ID)
	}
}

// Group membership carries the date it was granted, because "since when" is
// the half of the fact that makes it reviewable.
func TestMyAccount_ReportsGroupsWithTheDateGranted(t *testing.T) {
	srv, router := meEnv(t)

	ctx := context.Background()
	g, err := srv.db.CreateGroup(ctx, "platform", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := srv.db.AddGroupMember(ctx, g.ID, "alice", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	rec := getAs(t, srv, router, "alice", "/api/v1/me/account")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/account = %d, want 200", rec.Code)
	}
	got := decodeAccount(t, rec.Body.Bytes())
	if len(got.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(got.Groups))
	}
	if got.Groups[0].Name != "platform" {
		t.Fatalf("group name = %q, want platform", got.Groups[0].Name)
	}
	if got.Groups[0].AddedAt.IsZero() {
		t.Error("added_at is zero — the date a membership was granted is part of the fact")
	}

	// bob is in no group, and reads his own emptiness rather than alice's.
	rec = getAs(t, srv, router, "bob", "/api/v1/me/account")
	if got := decodeAccount(t, rec.Body.Bytes()); len(got.Groups) != 0 {
		t.Fatalf("bob sees %d groups, want 0", len(got.Groups))
	}
}

// setDormancy configures the idle sweep the way an administrator would.
func setDormancy(t *testing.T, srv *Server, enabled bool, days int) {
	t.Helper()
	s, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	s.DormantLockEnabled = enabled
	s.DormantLockDays = days
	if err := srv.db.SetPlatformSettings(&s, database.BuiltinAdminID, "req", "ip", "ua"); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// dormancyOf pulls the optional block out of the raw body — decoding into
// userDetailResponse would silently drop it, which is the mistake this guards.
func dormancyOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d, ok := raw["dormancy"]
	if !ok || d == nil {
		return nil
	}
	m, ok := d.(map[string]any)
	if !ok {
		t.Fatalf("dormancy is %T, want an object", d)
	}
	return m
}

// Absent, not dormant: with the sweep off there is no section to render, so
// there is no field either. A "not configured" placeholder would be the bug.
func TestMyAccount_NoDormancyNoticeWhenTheSweepIsOff(t *testing.T) {
	srv, router := meEnv(t)
	setDormancy(t, srv, false, 90)

	rec := getAs(t, srv, router, "alice", "/api/v1/me/account")
	if d := dormancyOf(t, rec.Body.Bytes()); d != nil {
		t.Fatalf("dormancy reported while the sweep is off: %v", d)
	}
}

// With the sweep on, the subject is told the policy and the date their own
// clock puts them at — before it happens, which is the whole point.
func TestMyAccount_ReportsDormancyWhenTheSweepIsOn(t *testing.T) {
	srv, router := meEnv(t)
	setDormancy(t, srv, true, 90)

	rec := getAs(t, srv, router, "alice", "/api/v1/me/account")
	d := dormancyOf(t, rec.Body.Bytes())
	if d == nil {
		t.Fatal("no dormancy block while the sweep is on")
	}
	if got := d["threshold_days"]; got != float64(90) {
		t.Errorf("threshold_days = %v, want 90", got)
	}
	locksOn, ok := d["locks_on"].(string)
	if !ok || locksOn == "" {
		t.Fatalf("locks_on = %v, want a date", d["locks_on"])
	}
	when, err := time.Parse(time.RFC3339, locksOn)
	if err != nil {
		t.Fatalf("locks_on %q is not a timestamp: %v", locksOn, err)
	}
	// Seeded moments ago, so the lock date is ~90 days out.
	days := time.Until(when).Hours() / 24
	if days < 88 || days > 91 {
		t.Errorf("locks_on is %.1f days away, want ~90", days)
	}
}

// The sweep never locks an administrator, so an administrator is never warned
// about it. A warning that cannot come true is a lie with a date on it.
func TestMyAccount_NeverWarnsAnAdministratorAboutDormancy(t *testing.T) {
	srv, router := meEnv(t)
	setDormancy(t, srv, true, 90)

	rec := getAs(t, srv, router, "root", "/api/v1/me/account")
	if d := dormancyOf(t, rec.Body.Bytes()); d != nil {
		t.Fatalf("an administrator was warned about a sweep that skips them: %v", d)
	}

	// And the ordinary user on the same install still is — proving the absence
	// above is about the role, not about the settings read failing.
	rec = getAs(t, srv, router, "bob", "/api/v1/me/account")
	if d := dormancyOf(t, rec.Body.Bytes()); d == nil {
		t.Fatal("a viewer got no dormancy notice on an install where the sweep is on")
	}
}

// seedAccessMatrix gives alice an owned app, a direct grant, a group grant,
// and one app she has no path to — enough that a report which quietly returned
// everything, or nothing, would fail.
func seedAccessMatrix(t *testing.T, srv *Server) {
	t.Helper()

	mk := func(slug, owner, visibility string) *database.App {
		a := &database.App{
			Slug: slug, Name: slug, OwnerID: owner,
			Framework: "flask", Status: database.AppStatusRunning, Visibility: visibility,
		}
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
		return a
	}

	mk("a-mine", "alice", database.VisibilityPrivate)
	direct := mk("b-direct", "bob", database.VisibilityPrivate)
	viaGroup := mk("c-group", "bob", database.VisibilityPrivate)
	mk("d-unrelated", "bob", database.VisibilityPrivate)

	ctx := t.Context()
	g, err := srv.db.CreateGroup(ctx, "data-team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := srv.db.AddGroupMember(ctx, g.ID, "alice", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	meta := database.RequestMeta{}
	if err := srv.db.UpsertGrant(ctx, direct.ID, "user", "alice", database.ObjectRoleViewer, "bob", meta); err != nil {
		t.Fatalf("direct grant: %v", err)
	}
	if err := srv.db.UpsertGrant(ctx, viaGroup.ID, "group", g.ID, database.ObjectRoleEditor, "bob", meta); err != nil {
		t.Fatalf("group grant: %v", err)
	}
}

func decodeAccess(t *testing.T, body []byte) accessReport {
	t.Helper()
	var got accessReport
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode /me/access: %v — body=%s", err, body)
	}
	return got
}

// The report a person reads about themselves: every app they can reach, and
// the reason for each.
func TestMyAccess_ReportsTheCallersOwnReach(t *testing.T) {
	srv, router := meEnv(t)
	seedAccessMatrix(t, srv)

	rec := getAs(t, srv, router, "alice", "/api/v1/me/access")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/access as alice = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	got := decodeAccess(t, rec.Body.Bytes())

	if got.User.ID != "alice" {
		t.Fatalf("report is about %q, want alice", got.User.ID)
	}

	kinds := map[string]string{} // slug -> source kind
	for _, e := range got.Explicit {
		if len(e.Sources) == 0 {
			t.Fatalf("%s appears with no source — a row with no reason is the bug this report exists to prevent", e.App.Slug)
		}
		kinds[e.App.Slug] = e.Sources[0].Kind
	}

	if len(got.Explicit) != 3 {
		t.Fatalf("got %d apps %v, want 3 — d-unrelated must not appear", len(got.Explicit), kinds)
	}
	if kinds["a-mine"] != "owner" {
		t.Errorf("a-mine source = %q, want owner", kinds["a-mine"])
	}
	if kinds["b-direct"] != "direct" {
		t.Errorf("b-direct source = %q, want direct", kinds["b-direct"])
	}
	if kinds["c-group"] != "group" {
		t.Errorf("c-group source = %q, want group", kinds["c-group"])
	}
	if _, leaked := kinds["d-unrelated"]; leaked {
		t.Error("d-unrelated is in the report; alice has no path to it")
	}
}

// The reason the builder was extracted rather than copied: the answer a person
// gets about themselves and the answer an auditor gets about them are the same
// answer. If these two ever disagree, the access review is the one that is
// wrong, and nobody would find out from the admin route's own tests.
func TestMyAccess_AgreesWithTheAdministratorsReview(t *testing.T) {
	srv, router := meEnv(t)
	seedAccessMatrix(t, srv)

	mine := getAs(t, srv, router, "alice", "/api/v1/me/access")
	if mine.Code != http.StatusOK {
		t.Fatalf("/me/access = %d", mine.Code)
	}
	theirs := getAs(t, srv, router, "root", "/api/v1/users/alice/access")
	if theirs.Code != http.StatusOK {
		t.Fatalf("/users/alice/access = %d", theirs.Code)
	}

	if mine.Body.String() != theirs.Body.String() {
		t.Fatalf("the two views of one person's access differ:\n self  = %s\n admin = %s",
			mine.Body.String(), theirs.Body.String())
	}
}

// Same structural argument as /me/account: no id is accepted, so no input aims
// this at somebody else.
func TestMyAccess_TakesNoIDAndRefusesAnonymous(t *testing.T) {
	srv, router := meEnv(t)
	seedAccessMatrix(t, srv)

	if rec := getAs(t, srv, router, "", "/api/v1/me/access"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /me/access = %d, want 401", rec.Code)
	}

	// bob owns three of the four apps; alice's report must not be reachable
	// from his session, and his own report is his own.
	rec := getAs(t, srv, router, "bob", "/api/v1/me/access")
	if rec.Code != http.StatusOK {
		t.Fatalf("/me/access as bob = %d, want 200", rec.Code)
	}
	if got := decodeAccess(t, rec.Body.Bytes()); got.User.ID != "bob" {
		t.Fatalf("bob's report is about %q", got.User.ID)
	}

	if rec := getAs(t, srv, router, "alice", "/api/v1/me/access/bob"); rec.Code != http.StatusNotFound {
		t.Fatalf("/me/access/bob = %d, want 404 — no id-bearing variant may exist", rec.Code)
	}
}

// Allowed to a key, per the keyroutes classification.
func TestMyAccess_AnswersAPersonalAPIKey(t *testing.T) {
	srv, router := meEnv(t)
	seedAccessMatrix(t, srv)
	enablePersonalKeys(t, srv, true)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	rec := getWithKey(t, router, secret, "/api/v1/me/access")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/access with an toftu_ key = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeAccess(t, rec.Body.Bytes()); got.User.ID != "alice" {
		t.Fatalf("the key's report is about %q, want alice", got.User.ID)
	}
}

// --- D8: sign out everywhere -------------------------------------------------

// The mechanism, end to end: the bump lands, the audit row lands with it, the
// caller's cookies are cleared, and the token that made the request stops
// working.
func TestRevokeMySessions_EndsEverySessionAndAudits(t *testing.T) {
	srv, router := meEnv(t)

	before, err := srv.db.GetTokenVersion("alice")
	if err != nil {
		t.Fatalf("token version: %v", err)
	}
	cookie := sessionCookieFor(t, srv, "alice")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/sessions/revoke", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /me/sessions/revoke = %d, want 204 — body=%s", rec.Code, rec.Body.String())
	}

	after, err := srv.db.GetTokenVersion("alice")
	if err != nil {
		t.Fatalf("token version: %v", err)
	}
	if after != before+1 {
		t.Fatalf("token_version went %d → %d, want +1", before, after)
	}

	// The caller's own cookies are cleared: the session pressing the button is
	// one of the sessions it ends.
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the caller's session cookie was not cleared")
	}

	// Audited in the same transaction as the bump.
	rows, err := srv.db.ListActivity(database.ActivityFilter{Actions: []string{"session.*"}, Limit: 20})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.Action == "session.revoke_all" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session.revoke_all row after revoking; got %d session rows", len(rows))
	}

	// And the token that made the request is now refused.
	if rec := getAs(t, srv, router, "", "/api/v1/me/account"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous follow-up = %d, want 401", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me/account", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("the revoked token still reads /me/account")
	}
}

// One person's revoke is their own. Nothing about it reaches another account.
func TestRevokeMySessions_TouchesNobodyElse(t *testing.T) {
	srv, router := meEnv(t)

	bobBefore, err := srv.db.GetTokenVersion("bob")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/sessions/revoke", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	req.AddCookie(sessionCookieFor(t, srv, "alice"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", rec.Code)
	}

	bobAfter, err := srv.db.GetTokenVersion("bob")
	if err != nil {
		t.Fatal(err)
	}
	if bobAfter != bobBefore {
		t.Fatalf("bob's token_version moved %d → %d when alice revoked hers", bobBefore, bobAfter)
	}
}

// A key never manages credentials. This is that rule, as behaviour.
func TestRevokeMySessions_RefusesAPersonalAPIKey(t *testing.T) {
	srv, router := meEnv(t)
	enablePersonalKeys(t, srv, true)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	before, err := srv.db.GetTokenVersion("alice")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/me/sessions/revoke", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusNoContent {
		t.Fatal("a personal API key revoked its owner's sessions")
	}
	after, err := srv.db.GetTokenVersion("alice")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("token_version moved %d → %d on a refused request", before, after)
	}
}

// --- D4: the sign-in keyhole -------------------------------------------------

func decodeSignins(t *testing.T, body []byte) []database.MySignin {
	t.Helper()
	var got struct {
		SignIns []database.MySignin `json:"sign_ins"`
		Days    int                 `json:"days"`
		Limit   int                 `json:"limit"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode /me/sign-ins: %v — body=%s", err, body)
	}
	if got.Days == 0 || got.Limit == 0 {
		t.Error("the window and cap should travel with the answer")
	}
	return got.SignIns
}

// The keyhole: your rows and only your rows, whatever else is in the ledger.
func TestMySignins_ReturnsOnlyTheCallersOwnRows(t *testing.T) {
	srv, router := meEnv(t)

	// Two accounts with session history, written the way the sign-in path does.
	for _, who := range []string{"alice", "bob"} {
		srv.db.BumpTokenVersion(who) // touch, so the account is real to the ledger
		if err := srv.db.RevokeAllSessions(who, database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: who,
			IP: "10.0.0.9", UserAgent: "test-agent",
		}); err != nil {
			t.Fatalf("seed session row for %s: %v", who, err)
		}
	}

	rec := getAs(t, srv, router, "alice", "/api/v1/me/sign-ins")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/sign-ins = %d, want 200 — body=%s", rec.Code, rec.Body.String())
	}
	rows := decodeSignins(t, rec.Body.Bytes())
	if len(rows) == 0 {
		t.Fatal("no rows; the seeded session event should be here")
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.Action, "session.") {
			t.Errorf("row outside the session family: %q", r.Action)
		}
	}

	// bob's identical event is not in alice's answer: the count is alice's own.
	aliceCount := len(rows)
	rec = getAs(t, srv, router, "bob", "/api/v1/me/sign-ins")
	bobRows := decodeSignins(t, rec.Body.Bytes())
	if len(bobRows) != 1 || aliceCount != 1 {
		t.Fatalf("alice saw %d rows and bob %d; each seeded exactly one", aliceCount, len(bobRows))
	}
}

// The payload is four columns. `before` and `after` are where a widened query
// would leak the rest of the ledger, so they must never appear.
func TestMySignins_NeverReturnsTheAuditDocuments(t *testing.T) {
	srv, router := meEnv(t)
	if err := srv.db.RevokeAllSessions("alice", database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "alice",
		Before: `{"secret":"before-doc"}`, After: `{"secret":"after-doc"}`,
	}); err != nil {
		t.Fatal(err)
	}

	rec := getAs(t, srv, router, "alice", "/api/v1/me/sign-ins")
	body := rec.Body.String()
	for _, leaked := range []string{"before-doc", "after-doc", `"before"`, `"after"`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("the audit documents reached the client: %q in %s", leaked, body)
		}
	}
}

// A governance read about the human, so a key is refused.
func TestMySignins_RefusesAPersonalAPIKey(t *testing.T) {
	srv, router := meEnv(t)
	enablePersonalKeys(t, srv, true)
	secret := mintKey(t, srv, "alice", "ci", nil, false)

	rec := getWithKey(t, router, secret, "/api/v1/me/sign-ins")
	if rec.Code == http.StatusOK {
		t.Fatalf("a personal API key read its owner's sign-in history: %s", rec.Body.String())
	}
}

func TestMySignins_AnonymousIsRefused(t *testing.T) {
	srv, router := meEnv(t)
	if rec := getAs(t, srv, router, "", "/api/v1/me/sign-ins"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", rec.Code)
	}
}
