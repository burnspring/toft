package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"toft/internal/database"
	"toft/internal/password"
	"toft/internal/telemetry"
)

// The session ledger — `session.*` in audit_log.
//
// These tests are about the two things that make it worth having and the two
// that make it safe: that a sign-in and a refusal are both recorded, that the
// row does not repeat the response's discretion (the API stays a non-oracle
// while the admin-only table says what was typed), and that the family stays
// separable from the governance feed.

// signinRows reads the ledger the way the admin UI does, so a test that passes
// proves the row is reachable rather than merely inserted.
func signinRows(t *testing.T, srv *Server) []database.ActivityEntry {
	t.Helper()
	rows, err := srv.db.ListActivity(database.ActivityFilter{Actions: []string{"session.*"}})
	if err != nil {
		t.Fatalf("listing the session ledger: %v", err)
	}
	return rows
}

// detailOf decodes the JSON blob the row carries, which is where the reason and
// the attempted name live.
func detailOf(t *testing.T, e database.ActivityEntry) map[string]string {
	t.Helper()
	out := map[string]string{}
	if e.Detail == "" {
		return out
	}
	if err := json.Unmarshal([]byte(e.Detail), &out); err != nil {
		t.Fatalf("decoding detail %q: %v", e.Detail, err)
	}
	return out
}

func onlyRow(t *testing.T, rows []database.ActivityEntry) database.ActivityEntry {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("session ledger has %d rows, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// The gap this closes: an install could not say whether an administrator had
// signed in.
func TestSuccessfulSignInIsRecorded(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	const chosen = "correct-horse-battery-staple"
	if rec := postJSON(t, router, "/api/v1/auth/local/password", map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default, "new_password": chosen,
	}); rec.Code != http.StatusOK {
		t.Fatalf("password change = %d body=%s", rec.Code, rec.Body.String())
	}

	// The change minted a session of its own, so the ledger already has one
	// row. Read from here on.
	before := len(signinRows(t, srv))

	if rec := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin, "password": chosen,
	}); rec.Code != http.StatusOK || sessionCookieIn(rec) == nil {
		t.Fatalf("login = %d, cookie=%v", rec.Code, sessionCookieIn(rec) != nil)
	}

	rows := signinRows(t, srv)
	if len(rows) != before+1 {
		t.Fatalf("ledger went from %d to %d rows, want one more", before, len(rows))
	}
	got := rows[0] // newest first
	if got.Action != ActionSignin {
		t.Errorf("action = %q, want %q", got.Action, ActionSignin)
	}
	if got.TargetID != database.BuiltinAdminID {
		t.Errorf("target = %q, want the built-in administrator", got.TargetID)
	}
	if d := detailOf(t, got); d["provider"] != database.ProviderKindPassword {
		t.Errorf("provider = %q, want %q", d["provider"], database.ProviderKindPassword)
	}
}

// A wrong password against a real account: the person is known, so the row says
// who, and the reason matches the metric's.
func TestRefusedSignInIsRecordedWithItsReason(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	// Off the must-change path first, so the refusal under test is a bad
	// credential rather than the published default.
	const chosen = "correct-horse-battery-staple"
	postJSON(t, router, "/api/v1/auth/local/password", map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default, "new_password": chosen,
	})
	before := len(signinRows(t, srv))

	if rec := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin, "password": "not-the-password",
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad password = %d, want 401", rec.Code)
	}

	rows := signinRows(t, srv)
	if len(rows) != before+1 {
		t.Fatalf("ledger went from %d to %d rows, want one more", before, len(rows))
	}
	got := rows[0]
	if got.Action != ActionSigninFailed {
		t.Errorf("action = %q, want %q", got.Action, ActionSigninFailed)
	}
	if got.TargetID != database.BuiltinAdminID {
		t.Errorf("target = %q, want the account that was attacked", got.TargetID)
	}
	if d := detailOf(t, got); d["reason"] != telemetry.SigninBadCredential {
		t.Errorf("reason = %q, want %q", d["reason"], telemetry.SigninBadCredential)
	}
}

// The row and the response answer to different audiences.
//
// The API stays a non-oracle — `TestFailedLoginsAreIndistinguishable` is the
// test for that and this must not weaken it — while the admin-only table gets
// to say "four hundred attempts as `root`", which is the whole point of having
// it. The attempted name must never reach actor_id, though: the feed and the
// user page read that column as an id, so a caller who could choose it could
// write rows into somebody else's history.
func TestAnUnknownAccountIsRecordedAsAnonymousWithWhatWasTyped(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	if rec := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": "nobody-here", "password": "not-the-password",
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown account = %d, want 401", rec.Code)
	}

	got := onlyRow(t, signinRows(t, srv))
	if got.Action != ActionSigninFailed {
		t.Errorf("action = %q, want %q", got.Action, ActionSigninFailed)
	}
	if got.ActorLabel != auditActorAnonymous {
		t.Errorf("actor = %q, want %q — an unattributable attempt is not the platform acting",
			got.ActorLabel, auditActorAnonymous)
	}
	if got.TargetID != "" {
		t.Errorf("target = %q, want empty: no account was identified", got.TargetID)
	}
	d := detailOf(t, got)
	if d["attempted"] != "nobody-here" {
		t.Errorf("attempted = %q, want the typed name", d["attempted"])
	}
	if d["reason"] != telemetry.SigninNoAccount {
		t.Errorf("reason = %q, want %q", d["reason"], telemetry.SigninNoAccount)
	}
}

// A caller-supplied string reaches this table, so it is bounded. Otherwise the
// login form is a way to write arbitrarily large rows into the audit log.
func TestTheAttemptedNameIsBounded(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	long := ""
	for len(long) < 4096 {
		long += "abcdefghij"
	}
	postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": long, "password": "not-the-password",
	})

	attempted := detailOf(t, onlyRow(t, signinRows(t, srv)))["attempted"]
	if len([]rune(attempted)) > maxAttemptedLogin+1 { // +1 for the ellipsis
		t.Errorf("attempted is %d runes, want at most %d", len([]rune(attempted)), maxAttemptedLogin+1)
	}
}

// The published-default rule, in the ledger.
//
// The must-change path verifies a password and mints nothing. Recording that as
// a sign-in would put "the administrator signed in" in the record for an install
// still holding the password that ships in the documentation.
func TestTheDefaultPasswordIsNotRecordedAsASignIn(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	if rec := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default,
	}); rec.Code != http.StatusOK {
		t.Fatalf("default password = %d", rec.Code)
	}

	got := onlyRow(t, signinRows(t, srv))
	if got.Action != ActionSigninFailed {
		t.Fatalf("action = %q, want %q — nothing was minted", got.Action, ActionSigninFailed)
	}
	if d := detailOf(t, got); d["reason"] != telemetry.SigninMustChange {
		t.Errorf("reason = %q, want %q", d["reason"], telemetry.SigninMustChange)
	}
}

// Sign-out is an act somebody performs — it revokes every session they have —
// so it is in the ledger. Expiry is not, because nobody performed it.
func TestSignOutIsRecorded(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	const chosen = "correct-horse-battery-staple"
	rec := postJSON(t, router, "/api/v1/auth/local/password", map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default, "new_password": chosen,
	})
	cookie := sessionCookieIn(rec)
	if cookie == nil {
		t.Fatal("no session to sign out of")
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	req.AddCookie(cookie)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", out.Code)
	}

	rows := signinRows(t, srv)
	if len(rows) == 0 || rows[0].Action != ActionSignout {
		t.Fatalf("newest row = %+v, want a %s", rows, ActionSignout)
	}
	if rows[0].TargetID != database.BuiltinAdminID {
		t.Errorf("target = %q, want the account that signed out", rows[0].TargetID)
	}
}

// Clearing cookies that were never valid is not somebody signing out. A row for
// it would make the ledger's sign-outs outnumber its sign-ins, which is the
// shape an operator would read as sessions leaking.
func TestSignOutWithoutASessionRecordsNothing(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)

	if rows := signinRows(t, srv); len(rows) != 0 {
		t.Fatalf("ledger has %d rows after a sign-out with no session: %+v", len(rows), rows)
	}
}

// The condition the whole design rests on: the noisy family is separable, so
// adding it did not cost anybody the governance view.
//
// `access` is what an administrator reads to answer "who was given what". If
// sign-ins landed in it, a morning of logins would push a grant change off the
// first page — and the reason this table skipped routine sign-ins for so long
// was exactly that.
func TestSignInsAreTheirOwnCategoryAndNotPartOfAccess(t *testing.T) {
	if _, ok := activityCategories["signins"]; !ok {
		t.Fatal("no `signins` category — the ledger has no chip of its own")
	}
	for _, pattern := range activityCategories["access"] {
		if pattern == "session.*" {
			t.Error("`session.*` is in the access category: sign-ins would bury grant changes")
		}
	}
	if got := activityCategories["signins"]; len(got) != 1 || got[0] != "session.*" {
		t.Errorf("signins = %v, want exactly [session.*]", got)
	}
}

// LB-7 (security review 2026-11-15): a User-Agent that is not valid UTF-8 made
// the audit INSERT fail, so a refused sign-in — and every other best-effort
// row — left no record. The row is written, with the bad bytes replaced.
func TestAFailedSignInWithANonUTF8UserAgentIsStillRecorded(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	raw, _ := json.Marshal(map[string]string{"username": "nobody-here", "password": "not-the-password"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/local/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "\xff\xfe")
	req.RemoteAddr = "192.0.2.10:44444"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in = %d, want 401", rec.Code)
	}
	got := onlyRow(t, signinRows(t, srv))
	if got.Action != ActionSigninFailed {
		t.Fatalf("action = %q, want %q", got.Action, ActionSigninFailed)
	}
	var ua string
	if err := srv.db.Conn().QueryRow(`SELECT user_agent FROM audit_log WHERE action=?`,
		ActionSigninFailed).Scan(&ua); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ua, "�") || !utf8.ValidString(ua) {
		t.Errorf("user_agent = %q, want the bad bytes replaced by U+FFFD", ua)
	}
}
