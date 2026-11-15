package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
	"toft/internal/entitlements"
)

// Toft is free (ADR §32, *Toft is free*): a Server wired the way main.go wires
// it — the perpetual source, or nothing, which defaults to it — holds every
// feature and caps no creation. The license surface is deleted, not refused:
// its routes are absent and read like any other unknown path.

func perpetualCall(t *testing.T, srv *Server, router http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:34567"
	req.AddCookie(sessionCookieFor(t, srv, "admin-user"))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func newPerpetualEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(t)
	srv.ents = entitlements.Perpetual{}
	seedUser(t, srv.db, "admin-user", database.SystemRoleAdmin)
	return srv, srv.Router(http.NotFoundHandler())
}

func TestPerpetual_TheLicenseSurfaceIsGone(t *testing.T) {
	if _, ok := (&Server{}).entitlements().(entitlements.Perpetual); !ok {
		t.Fatalf("an unwired server's source is %T, not Perpetual", (&Server{}).entitlements())
	}
	srv, router := newPerpetualEnv(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/license"},
		{http.MethodPut, "/api/v1/admin/license"},
		{http.MethodPost, "/api/v1/admin/license/check"},
		{http.MethodPost, "/api/v1/admin/reachability/rearm"},
	} {
		if rr := perpetualCall(t, srv, router, tc.method, tc.path); rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want it absent: %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
	if _, capped := entitlements.AppLimit(srv.tier()); capped {
		t.Fatal("the perpetual install caps app creation")
	}
}

// The feature list reads every feature licensed, with nothing saying "no
// license key" and no grace window.
func TestPerpetual_EveryFeatureIsLicensed(t *testing.T) {
	srv, router := newPerpetualEnv(t)
	rr := perpetualCall(t, srv, router, http.MethodGet, "/api/v1/admin/features")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/features = %d: %s", rr.Code, rr.Body.String())
	}
	var features []featureDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &features); err != nil {
		t.Fatal(err)
	}
	if len(features) == 0 {
		t.Fatal("no features listed")
	}
	for _, f := range features {
		if !f.Licensed || f.Unlicensed || f.InGrace || f.Tier != string(entitlements.TierPaid) {
			t.Errorf("%s: %+v", f.Name, f)
		}
	}
}
