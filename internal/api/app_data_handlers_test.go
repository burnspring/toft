package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The app's one JSON document, driven through the real router.
//
// Three claims are what these tests are about. The document is the app's own —
// nobody else's app can see it and no other credential can reach it. It is
// *durable* in the one sense that matters: the credential that wrote it is gone
// by the next deploy and the document is not. And the two states an app branches
// on — never written, versus written and cleared — stay tellable apart, which
// every convenience in this design (a null read instead of a 404, an object
// instead of any JSON value) exists to preserve.

type appDataFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
	other  *database.App
	token  string // the app's tofta_ secret
	otherT string // the other app's
}

func newAppDataFixture(t *testing.T) *appDataFixture {
	t.Helper()

	srv := newTestServer(t)
	f := &appDataFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "priya", database.SystemRoleViewer)

	f.app = &database.App{
		Slug: "recon", Name: "Reconciliation", OwnerID: "owner",
		Framework: "streamlit", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := srv.db.CreateApp(f.app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	f.other = &database.App{
		Slug: "other", Name: "Other", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := srv.db.CreateApp(f.other); err != nil {
		t.Fatalf("create other app: %v", err)
	}

	f.token = f.mintToken(t, f.app.ID, "tok-1")
	f.otherT = f.mintToken(t, f.other.ID, "tok-2")
	return f
}

func (f *appDataFixture) mintToken(t *testing.T, appID, id string) string {
	t.Helper()
	secret := appTokenSecretPrefix + id + "-secret"
	if err := f.srv.db.CreateAppToken(&database.AppToken{
		ID: id, AppID: appID, TokenHash: hashAPIKey(secret), Prefix: secret[:8],
	}); err != nil {
		t.Fatalf("create app token: %v", err)
	}
	return secret
}

// call drives one request as an app.
func (f *appDataFixture) call(t *testing.T, token, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.ContentLength = 0
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *appDataFixture) get(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, f.token, http.MethodGet, "/api/v1/app/data", "", nil)
}

func (f *appDataFixture) put(t *testing.T, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, f.token, http.MethodPut, "/api/v1/app/data", body, headers)
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return out
}

// --- The two empty states ---

// Never written is a 200 with a null document, not a 404. An app reads this on
// its first line; making it an error means every app's first call is inside a
// try/except and the one that forgets crashes on its first boot.
func TestAppData_NeverWritten_ReadsAsNull(t *testing.T) {
	f := newAppDataFixture(t)

	rec := f.get(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeData(t, rec)
	if body["data"] != nil {
		t.Fatalf("expected a null document, got %v", body["data"])
	}
	if body["version"].(float64) != 0 {
		t.Fatalf("expected version 0, got %v", body["version"])
	}
	if body["updated_at"] != nil {
		t.Fatalf("expected no updated_at, got %v", body["updated_at"])
	}
}

// Cleared is the same state as never written — deliberately, because an app
// that saved `{}` meant something different and is entitled to say so.
func TestAppData_Delete_ReturnsToNeverWritten(t *testing.T) {
	f := newAppDataFixture(t)

	if rec := f.put(t, `{"period":"2026-03"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.call(t, f.token, http.MethodDelete, "/api/v1/app/data", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if deleted, _ := decodeData(t, rec)["deleted"].(bool); !deleted {
		t.Fatal("expected the delete to report having removed something")
	}

	after := decodeData(t, f.get(t))
	if after["data"] != nil || after["version"].(float64) != 0 {
		t.Fatalf("expected the app back at never-written, got %v", after)
	}

	// And an empty object is a document, not a clear.
	if rec := f.put(t, `{}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("put empty object: %d %s", rec.Code, rec.Body.String())
	}
	stored := decodeData(t, f.get(t))
	if stored["data"] == nil {
		t.Fatal("an empty object must read back as a document, not as nothing")
	}
}

// --- Round trip ---

// The bytes come back as they went in. Key order, a large integer and a
// trailing zero all survive, which is what storing the document as text rather
// than as JSONB buys — and what the byte count in the response is counting.
func TestAppData_RoundTripsBytesUnchanged(t *testing.T) {
	f := newAppDataFixture(t)

	doc := `{"zebra":1,"alpha":{"nested":[1,2,3]},"big":12345678901234567890,"exact":1.0}`
	rec := f.put(t, doc, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got != `"1"` {
		t.Fatalf("expected ETag \"1\", got %q", got)
	}

	rec = f.get(t)
	var envelope struct {
		Data    json.RawMessage `json:"data"`
		Version int64           `json:"version"`
		Bytes   int             `json:"bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	if string(envelope.Data) != doc {
		t.Fatalf("document came back changed:\n want %s\n  got %s", doc, envelope.Data)
	}
	if envelope.Version != 1 || envelope.Bytes != len(doc) {
		t.Fatalf("expected version 1 and %d bytes, got %d and %d", len(doc), envelope.Version, envelope.Bytes)
	}

	// A second write replaces rather than merges, and advances the version.
	if rec := f.put(t, `{"alpha":2}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("second put: %d %s", rec.Code, rec.Body.String())
	}
	second := decodeData(t, f.get(t))
	if second["version"].(float64) != 2 {
		t.Fatalf("expected version 2, got %v", second["version"])
	}
	if doc, ok := second["data"].(map[string]any); !ok || len(doc) != 1 {
		t.Fatalf("a write replaces the document; got %v", second["data"])
	}
}

// The point of the feature: the credential that wrote the document is superseded
// on the next deploy, and the document is not. Nothing here is keyed by
// deployment, release or token, and this is the test that says so.
func TestAppData_SurvivesTheTokenThatWroteIt(t *testing.T) {
	f := newAppDataFixture(t)

	if rec := f.put(t, `{"cursor":4821}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	// What a redeploy does: the old workload's token is revoked and the new one
	// mints its own.
	if err := f.srv.db.RevokeAppTokens(f.app.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	fresh := f.mintToken(t, f.app.ID, "tok-redeployed")

	rec := f.call(t, fresh, http.MethodGet, "/api/v1/app/data", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read after redeploy: %d %s", rec.Code, rec.Body.String())
	}
	doc, _ := decodeData(t, rec)["data"].(map[string]any)
	if doc == nil || doc["cursor"].(float64) != 4821 {
		t.Fatalf("expected the document to outlive the token, got %s", rec.Body.String())
	}

	// The revoked one is refused, which is the other half of the same fact.
	if rec := f.get(t); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected the superseded token to be refused, got %d", rec.Code)
	}
}

// --- One app, one document ---

func TestAppData_IsScopedToTheCallingApp(t *testing.T) {
	f := newAppDataFixture(t)

	if rec := f.put(t, `{"who":"recon"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.call(t, f.otherT, http.MethodGet, "/api/v1/app/data", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("other app read: %d %s", rec.Code, rec.Body.String())
	}
	if body := decodeData(t, rec); body["data"] != nil {
		t.Fatalf("another app's token must not see this document, got %v", body["data"])
	}

	// And its own writes land somewhere else entirely.
	if rec := f.call(t, f.otherT, http.MethodPut, "/api/v1/app/data", `{"who":"other"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("other app put: %d %s", rec.Code, rec.Body.String())
	}
	mine, _ := decodeData(t, f.get(t))["data"].(map[string]any)
	if mine["who"] != "recon" {
		t.Fatalf("this app's document was overwritten by another app's: %v", mine)
	}
}

// The subtree admits nothing but an app token; this route is not the hole.
func TestAppDataRoutesRefuseEveryCredentialButTheAppToken(t *testing.T) {
	f := newAppDataFixture(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/app/data", strings.NewReader(`{"a":1}`))
		req.AddCookie(sessionCookieFor(t, f.srv, "owner"))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with a session cookie: expected 401, got %d", method, rec.Code)
		}
	}
}

// --- What the platform will not store ---

func TestAppData_RefusesAnythingButAnObject(t *testing.T) {
	cases := []struct{ name, body string }{
		{"an array", `[1,2,3]`},
		{"a bare string", `"hello"`},
		{"a number", `42`},
		// The one that matters: a stored null would read back exactly like
		// never having written anything.
		{"null", `null`},
		{"an empty body", ``},
		{"malformed JSON", `{"a":}`},
		{"invalid UTF-8", "{\"a\":\"\xff\xfe\"}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fixture each: an app token is rate limited to 60 calls a
			// minute, and fourteen requests in a millisecond is not what this
			// test is about.
			f := newAppDataFixture(t)
			rec := f.put(t, tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if body := decodeData(t, rec); body["error"] == "" {
				t.Fatal("a refusal must say what would work")
			}
			// Nothing was stored on the way to the refusal.
			if got := decodeData(t, f.get(t)); got["data"] != nil {
				t.Fatalf("a refused write must store nothing, got %v", got["data"])
			}
		})
	}
}

func TestAppData_RefusesADocumentOverTheCap(t *testing.T) {
	f := newAppDataFixture(t)

	oversized := `{"blob":"` + strings.Repeat("x", maxAppDataBytes) + `"}`
	rec := f.put(t, oversized, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "storage") {
		t.Fatalf("the refusal must name what to use instead: %s", rec.Body.String())
	}

	// A document exactly at the cap is accepted — the boundary is inclusive,
	// and an off-by-one here is the kind that only shows up in production.
	padding := maxAppDataBytes - len(`{"blob":""}`)
	exact := `{"blob":"` + strings.Repeat("x", padding) + `"}`
	if len(exact) != maxAppDataBytes {
		t.Fatalf("test builds a %d byte document, wanted %d", len(exact), maxAppDataBytes)
	}
	if rec := f.put(t, exact, nil); rec.Code != http.StatusOK {
		t.Fatalf("a document exactly at the cap must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- Preconditions ---

func TestAppData_IfMatch(t *testing.T) {
	f := newAppDataFixture(t)

	// "only if nothing is stored" — absent is version 0, which is why
	// create-only needs no second route.
	if rec := f.put(t, `{"n":1}`, map[string]string{"If-Match": `"0"`}); rec.Code != http.StatusOK {
		t.Fatalf("create-only against an empty app: %d %s", rec.Code, rec.Body.String())
	}
	// The same precondition now fails, and the refusal carries the version that
	// is actually stored so the app recovers in one round trip.
	rec := f.put(t, `{"n":2}`, map[string]string{"If-Match": `"0"`})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeData(t, rec)
	if body["code"] != "version_conflict" || body["version"].(float64) != 1 {
		t.Fatalf("expected a version_conflict naming version 1, got %v", body)
	}

	// A stale version is refused and writes nothing.
	if rec := f.put(t, `{"n":3}`, map[string]string{"If-Match": `"7"`}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 for a stale version, got %d: %s", rec.Code, rec.Body.String())
	}
	current, _ := decodeData(t, f.get(t))["data"].(map[string]any)
	if current["n"].(float64) != 1 {
		t.Fatalf("a refused write must leave the document alone, got %v", current)
	}

	// The current version is accepted, quoted or not.
	if rec := f.put(t, `{"n":4}`, map[string]string{"If-Match": "1"}); rec.Code != http.StatusOK {
		t.Fatalf("unquoted current version: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.put(t, `{"n":5}`, map[string]string{"If-Match": `"2"`}); rec.Code != http.StatusOK {
		t.Fatalf("quoted current version: %d %s", rec.Code, rec.Body.String())
	}

	// A delete carries the same precondition.
	if rec := f.call(t, f.token, http.MethodDelete, "/api/v1/app/data", "", map[string]string{"If-Match": `"1"`}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected a stale delete to be refused, got %d", rec.Code)
	}
	if rec := f.call(t, f.token, http.MethodDelete, "/api/v1/app/data", "", map[string]string{"If-Match": `"3"`}); rec.Code != http.StatusOK {
		t.Fatalf("delete at the current version: %d %s", rec.Code, rec.Body.String())
	}
}

// A precondition the platform cannot honour is a named refusal, not a silently
// ignored header — the whole value of a precondition is that it was applied.
func TestAppData_RefusesPreconditionsItCannotHonour(t *testing.T) {
	f := newAppDataFixture(t)

	for _, value := range []string{"*", `W/"1"`, "later", "-1"} {
		rec := f.put(t, `{"a":1}`, map[string]string{"If-Match": value})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("If-Match %q: expected 400, got %d: %s", value, rec.Code, rec.Body.String())
		}
		if decodeData(t, rec)["code"] != "bad_precondition" {
			t.Fatalf("If-Match %q: expected a bad_precondition code, got %s", value, rec.Body.String())
		}
	}
}

// --- The platform-side pair ---

// Metadata, never the document. The app is the only reader and the only writer;
// what the platform side answers is whether anything is there and when it
// changed.
func TestAppDataMeta_ReportsSizeButNotContents(t *testing.T) {
	f := newAppDataFixture(t)
	doc := `{"secret-ish":"whatever the app put here"}`
	if rec := f.put(t, doc, nil); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/data")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner read: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "whatever the app put here") {
		t.Fatalf("the platform side must not serve the document: %s", rec.Body.String())
	}
	body := decodeData(t, rec)
	if body["bytes"].(float64) != float64(len(doc)) || body["version"].(float64) != 1 {
		t.Fatalf("expected the size and version, got %v", body)
	}
}

// The escape hatch: an app that has written state its own startup code cannot
// read is wedged in a way no redeploy fixes, because a redeploy is exactly what
// this store survives.
func TestAppDataReset_ClearsAndIsAudited(t *testing.T) {
	f := newAppDataFixture(t)
	if rec := f.put(t, `{"wedged":true}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	before, err := f.srv.db.CountAuditEntries()
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/apps/"+f.app.ID+"/data", nil)
	req.AddCookie(sessionCookieFor(t, f.srv, "owner"))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}

	if got := decodeData(t, f.get(t)); got["data"] != nil {
		t.Fatalf("expected the document gone, got %v", got["data"])
	}
	after, err := f.srv.db.CountAuditEntries()
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if after != before+1 {
		t.Fatalf("a person erasing an app's state is an audited event: %d → %d", before, after)
	}
}

// Reading is a read; erasing is an edit. A viewer on the app may see that
// something is stored and may not throw it away.
func TestAppDataReset_RequiresEditRights(t *testing.T) {
	f := newAppDataFixture(t)
	if rec := f.put(t, `{"a":1}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	if rec := getAs(t, f.srv, f.router, "priya", "/api/v1/apps/"+f.app.ID+"/data"); rec.Code != http.StatusOK {
		t.Fatalf("a viewer may read the metadata: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/apps/"+f.app.ID+"/data", nil)
	req.AddCookie(sessionCookieFor(t, f.srv, "priya"))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("a viewer must not be able to erase an app's state, got %d", rec.Code)
	}
	if got := decodeData(t, f.get(t)); got["data"] == nil {
		t.Fatal("the document was erased by someone who may not")
	}
}

// MaxBytesReader(n) reads exactly n bytes without complaint, so the reader's
// +1 admitted a document one byte over the cap. The boundary is inclusive at
// the cap and refused at cap+1 — the case the earlier test skipped, checking
// the cap and cap+11 and never the byte between.
func TestAppData_RefusesOneBytePastTheCap(t *testing.T) {
	f := newAppDataFixture(t)

	padding := maxAppDataBytes + 1 - len(`{"blob":""}`)
	overByOne := `{"blob":"` + strings.Repeat("x", padding) + `"}`
	if len(overByOne) != maxAppDataBytes+1 {
		t.Fatalf("test builds a %d byte document, wanted %d", len(overByOne), maxAppDataBytes+1)
	}
	rec := f.put(t, overByOne, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("one byte over the cap = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if got := decodeData(t, f.get(t)); got["data"] != nil {
		t.Fatalf("a refused write must store nothing, got %v", got["data"])
	}
}

// A clear never resets the version (BB-005). A writer holding version 1 from
// before a clear must not be able to overwrite a different document that has
// reached version 1 again after it — the ABA a restarting counter allows.
func TestAppData_VersionsAreMonotonicAcrossAClear(t *testing.T) {
	f := newAppDataFixture(t)
	del := func(h map[string]string) *httptest.ResponseRecorder {
		return f.call(t, f.token, http.MethodDelete, "/api/v1/app/data", "", h)
	}

	if rec := del(nil); rec.Code != http.StatusOK {
		t.Fatalf("clear on nothing: %d %s", rec.Code, rec.Body.String())
	}
	rec := f.put(t, `{"doc":"A"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("put A: %d %s", rec.Code, rec.Body.String())
	}
	staleVersion := decodeData(t, rec)["version"].(float64)

	if rec := del(nil); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	// Cleared reads exactly as never-written: version 0, data null, and the
	// meta route says nothing is stored.
	after := decodeData(t, f.get(t))
	if after["data"] != nil || after["version"].(float64) != 0 {
		t.Fatalf("expected a cleared document to read as never-written, got %v", after)
	}
	if rec := del(map[string]string{"If-Match": `"0"`}); rec.Code != http.StatusOK {
		t.Fatalf("a delete at version 0 against a cleared document: %d %s", rec.Code, rec.Body.String())
	}

	// Create-only still works against a cleared document, and continues the
	// version rather than restarting it.
	rec = f.put(t, `{"doc":"B"}`, map[string]string{"If-Match": `"0"`})
	if rec.Code != http.StatusOK {
		t.Fatalf("put B create-only: %d %s", rec.Code, rec.Body.String())
	}
	if v := decodeData(t, rec)["version"].(float64); v <= staleVersion {
		t.Fatalf("the version after a clear must exceed every earlier one: %v <= %v", v, staleVersion)
	}

	rec = f.put(t, `{"doc":"A2"}`, map[string]string{"If-Match": `"1"`})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("a version from before the clear must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if doc, _ := decodeData(t, f.get(t))["data"].(map[string]any); doc["doc"] != "B" {
		t.Fatalf("the refused write must leave B in place, got %v", doc)
	}
}
