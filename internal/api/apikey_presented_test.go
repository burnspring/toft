package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A credential nothing reads is a 401, never anonymous (BB-033, fourth
// 2026-11-03 black-box list): `Bearer garbage` answered GET /apps with the
// public list, and a broken TOFT_TOKEN read as an empty install.
func TestAPresentedCredentialNothingReadsIsRefused(t *testing.T) {
	s := &Server{}
	for _, c := range []struct {
		name, header, value string
		refused             bool
	}{
		{"garbage bearer", "Authorization", "Bearer garbage", true},
		{"dotted bearer", "Authorization", "Bearer abc.def", true},
		{"lower-case scheme", "Authorization", "bearer garbage", true},
		{"an app token at the platform API", "Authorization", "Bearer tofta_x", true},
		{"garbage X-API-Key", "X-API-Key", "garbage", true},
		// The deploy-key middleware's, which runs next.
		{"a deploy key", "Authorization", "Bearer toftd_x", false},
		{"a deploy key as X-API-Key", "X-API-Key", "toftd_x", false},
		// Not a credential of ours: a proxy's Basic must not sign anyone out.
		{"basic", "Authorization", "Basic dXNlcjpwYXNz", false},
		{"empty bearer", "Authorization", "Bearer ", false},
		{"nothing", "", "", false},
	} {
		reached := false
		h := s.apiKeyAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
		r := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
		if c.header != "" {
			r.Header.Set(c.header, c.value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if reached == c.refused {
			t.Errorf("%s: reached=%v, want refused=%v (%d %s)", c.name, reached, c.refused, w.Code, w.Body.String())
			continue
		}
		if !c.refused {
			continue
		}
		var body struct{ Code, Hint string }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusUnauthorized || body.Code != "invalid_key" || body.Hint == "" {
			t.Errorf("%s: %d %s, want 401 invalid_key with a hint", c.name, w.Code, w.Body.String())
		}
	}
}
