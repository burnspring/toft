package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/entitlements"
)

// refusingLicense is an install that bought nothing. It exists so gate 1 can be
// closed on its own, which is the property the four-gate design turns on: each
// gate has to be able to refuse without the others, or they collapse into one
// boolean during implementation and nobody notices until a refusal says the
// wrong thing.
type refusingLicense struct{}

func (refusingLicense) Entitled(feature string) entitlements.Status {
	return entitlements.Status{Reason: "the term for this feature ended on 1 January"}
}

// Tier is the demo tier, which is what an install that bought nothing is on.
func (refusingLicense) Tier() entitlements.Tier { return entitlements.TierDemo }

func newFeatureEnv(t *testing.T, ents entitlements.Source) (*Server, http.Handler) {
	t.Helper()
	db := dbtest.New(t)
	cfg := &config.Config{BaseURL: "http://localhost:8080", AppsDir: t.TempDir()}
	opts := []ServerOption{}
	if ents != nil {
		opts = append(opts, WithEntitlements(ents))
	}
	srv := newTestServerWithConfig(t, db, cfg, opts...)
	seedUser(t, db, "admin-user", database.SystemRoleAdmin)
	seedUser(t, db, "publisher", database.SystemRolePublisher)
	return srv, srv.Router(http.NotFoundHandler())
}

func featureCall(t *testing.T, srv *Server, router http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	var req *http.Request
	if r != nil {
		req = httptest.NewRequest(method, path, r)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:34567"
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// switchable is a feature this build offers *and* an administrator can throw,
// asked of the catalogue rather than written down here.
//
// It was a literal — `faq` — and the catalogue outlived it: the documentation
// page took the FAQ with it (ADR §81), and four cases went on PUTting a name
// this build no longer offers, getting the 404 that rule is supposed to produce
// and asserting nothing about the gate they were named for. A test that names a
// row of a list the product owns is a hostage to that list.
func switchable(t *testing.T) string {
	t.Helper()
	for _, d := range entitlements.Current() {
		if d.AdminSwitch {
			return d.Name
		}
	}
	t.Skip("this build offers no feature with an administrator's switch")
	return ""
}

// Which features an install has bought is not an app owner's business — the
// same line /system/settings draws, and it 404s rather than 403ing so the
// surface reads as absent rather than as something to ask for.
func TestFeatures_NonAdminGets404(t *testing.T) {
	srv, router := newFeatureEnv(t, nil)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/features", ""},
		{http.MethodPut, "/api/v1/admin/features/" + switchable(t), `{"enabled":true}`},
	} {
		rr := featureCall(t, srv, router, "publisher", tc.method, tc.path, tc.body)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s as a publisher = %d, want 404", tc.method, tc.path, rr.Code)
		}
	}
}

// Gate 2, both ways, and the row surviving the way back down.
//
// Off is not absent: the row stays, so an administrator whose app stopped
// answering can see that somebody switched the feature off rather than find
// nothing on the page at all.
func TestFeatures_EnableThenDisableKeepsTheRow(t *testing.T) {
	srv, router := newFeatureEnv(t, nil)
	feature := switchable(t)

	rr := featureCall(t, srv, router, "admin-user", http.MethodPut,
		"/api/v1/admin/features/"+feature, `{"enabled":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable = %d (%s)", rr.Code, rr.Body.String())
	}
	var on featureDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &on)
	if !on.Enabled || !on.EverEnabled || on.EnabledAt == "" || on.EnabledBy != "admin-user" {
		t.Fatalf("enable returned %+v — want enabled, with who and when", on)
	}

	rr = featureCall(t, srv, router, "admin-user", http.MethodPut,
		"/api/v1/admin/features/"+feature, `{"enabled":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable = %d (%s)", rr.Code, rr.Body.String())
	}
	var off featureDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &off)
	if off.Enabled {
		t.Fatal("still enabled after being switched off")
	}
	if !off.EverEnabled {
		t.Fatal("the row vanished on the way off — absent and off are different states")
	}
	if off.EnabledAt != on.EnabledAt {
		t.Fatal("switching off rewrote when it was switched on, which is the question " +
			"the log exists to answer")
	}
}

// A name this build does not offer is a 404, not a row somebody can enable. The
// catalogue is the binary's, so a `features` row that survived a downgrade must
// not resurrect a capability the code no longer has.
func TestFeatures_UnknownNameIs404(t *testing.T) {
	srv, router := newFeatureEnv(t, nil)
	rr := featureCall(t, srv, router, "admin-user", http.MethodPut,
		"/api/v1/admin/features/wingdings", `{"enabled":true}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown feature = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
}

// Every change to gate 2 is on the audit trail, because "who turned on the thing
// that started sending our documentation to a model vendor, and when" is exactly
// what that log exists to answer.
func TestFeatures_EnableIsAudited(t *testing.T) {
	srv, router := newFeatureEnv(t, nil)
	feature := switchable(t)

	before, _ := srv.db.CountAuditEntries()
	featureCall(t, srv, router, "admin-user", http.MethodPut,
		"/api/v1/admin/features/"+feature, `{"enabled":true}`)
	after, _ := srv.db.CountAuditEntries()
	if after <= before {
		t.Fatal("switching a feature on wrote no audit row")
	}
}
