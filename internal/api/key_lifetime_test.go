package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
)

// How long a personal key lives, and who decides.
//
// A key made by `toft login` or RStudio's Add account never expires unless the
// install caps it (`api_key_max_lifetime_days`); a key made by hand keeps its
// ninety-day default, and under a cap asking for longer — or for never — is
// refused rather than quietly shortened.

func setKeyLifetimeCap(t *testing.T, srv *Server, days int) {
	t.Helper()
	s, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	s.APIKeyMaxLifetimeDays = days
	if err := srv.db.SetPlatformSettings(&s, database.BuiltinAdminID, "req", "ip", "ua"); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// approveLogin runs `toft login` to the point the key exists and returns it.
func approveLogin(t *testing.T, e *deviceEnv) database.UserAPIKey {
	t.Helper()
	started := e.start(t, "ada-mbp")
	if rec := e.post(t, "alice", "/api/v1/auth/cli/device/approve",
		map[string]any{"user_code": started["user_code"], "approve": true}); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d body=%s", rec.Code, rec.Body.String())
	}
	if code, out := e.poll(t, started["device_code"].(string)); code != http.StatusOK || out["status"] != deviceStatusApproved {
		t.Fatalf("collect: %d %v", code, out)
	}
	keys, err := e.srv.db.ListUserAPIKeys("alice", 1)
	if err != nil || len(keys) != 1 {
		t.Fatalf("alice's keys: %v, %d", err, len(keys))
	}
	return keys[0]
}

func TestALoginKeyNeverExpiresByDefault(t *testing.T) {
	key := approveLogin(t, newDeviceEnv(t))
	if key.ExpiresAt != nil {
		t.Errorf("a toft login key expires at %v; with no cap it must not expire", key.ExpiresAt)
	}
}

func TestALoginKeyTakesTheInstallsCap(t *testing.T) {
	e := newDeviceEnv(t)
	setKeyLifetimeCap(t, e.srv, 30)
	key := approveLogin(t, e)
	if key.ExpiresAt == nil {
		t.Fatal("under a 30-day cap a toft login key must expire; it does not")
	}
	want := time.Now().UTC().AddDate(0, 0, 30)
	if d := key.ExpiresAt.Sub(want); d > time.Minute || d < -time.Minute {
		t.Errorf("expires %v, want about %v", key.ExpiresAt, want)
	}
}

func createMyKey(t *testing.T, e *deviceEnv, body map[string]any) (int, map[string]any) {
	t.Helper()
	rec := e.post(t, "alice", "/api/v1/me/api-keys", body)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAHandMadeKeyKeepsItsDefaultWithNoCap(t *testing.T) {
	e := newDeviceEnv(t)
	code, out := createMyKey(t, e, map[string]any{"name": "ci"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create: %d %v", code, out)
	}
	keys, _ := e.srv.db.ListUserAPIKeys("alice", 1)
	if len(keys) != 1 || keys[0].ExpiresAt == nil {
		t.Fatalf("want one key with the ninety-day default, got %+v", keys)
	}
	if d := time.Until(*keys[0].ExpiresAt); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("default expiry is %v away, want ninety days", d)
	}
}

func TestUnderACapNeverAndLongerAreRefused(t *testing.T) {
	e := newDeviceEnv(t)
	setKeyLifetimeCap(t, e.srv, 30)
	for _, days := range []int{0, 31, 365} {
		code, out := createMyKey(t, e, map[string]any{"name": "ci", "expires_in_days": days})
		if code != http.StatusBadRequest {
			t.Errorf("expires_in_days=%d under a 30-day cap: %d, want 400", days, code)
			continue
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "30 days") {
			t.Errorf("refusal %q should name the cap", msg)
		}
	}
	if code, out := createMyKey(t, e, map[string]any{"name": "ok", "expires_in_days": 30}); code != http.StatusCreated && code != http.StatusOK {
		t.Errorf("a key at the cap is refused: %d %v", code, out)
	}
}

func TestUnderASmallCapTheDefaultBecomesTheCap(t *testing.T) {
	e := newDeviceEnv(t)
	setKeyLifetimeCap(t, e.srv, 7)
	if code, out := createMyKey(t, e, map[string]any{"name": "ci"}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("saying nothing must not ask for the one thing refused: %d %v", code, out)
	}
	keys, _ := e.srv.db.ListUserAPIKeys("alice", 1)
	if len(keys) != 1 || keys[0].ExpiresAt == nil || time.Until(*keys[0].ExpiresAt) > 8*24*time.Hour {
		t.Errorf("want a key expiring within the 7-day cap, got %+v", keys)
	}
}

func TestTheCapIsValidatedAndOffered(t *testing.T) {
	e := newDeviceEnv(t)
	for _, days := range []string{"-1", "3651"} {
		rec := putSettings(t, e.srv, e.router, "root", `{"api_key_max_lifetime_days":`+days+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("api_key_max_lifetime_days=%s: %d, want 400", days, rec.Code)
		}
	}
	if rec := putSettings(t, e.srv, e.router, "root", `{"api_key_max_lifetime_days":60}`); rec.Code != http.StatusOK {
		t.Fatalf("set the cap: %d %s", rec.Code, rec.Body.String())
	}
	// The create form reads it from the list, so it can offer only what the
	// server would accept.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/api-keys", nil)
	req.AddCookie(sessionCookieFor(t, e.srv, "alice"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["max_lifetime_days"] != float64(60) {
		t.Errorf("list says max_lifetime_days=%v, want 60", out["max_lifetime_days"])
	}
}
