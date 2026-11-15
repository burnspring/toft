package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// The account record's key list. What it has to get right is not "does it
// return rows" but "whose rows" — it takes a user id from the URL, which puts
// it under the IDOR rule, and it is admin-only, which puts it under the 404
// rule.

func decodeKeyList(t *testing.T, body []byte) struct {
	Keys []struct {
		ID     string `json:"id"`
		UserID string `json:"user_id"`
		Name   string `json:"name"`
	} `json:"keys"`
	Days int `json:"days"`
} {
	t.Helper()
	var out struct {
		Keys []struct {
			ID     string `json:"id"`
			UserID string `json:"user_id"`
			Name   string `json:"name"`
		} `json:"keys"`
		Days int `json:"days"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	return out
}

// The whole point of the section: an administrator looking at an account sees
// that account's keys, and only that account's.
func TestUserKeyListIsScopedToTheAccountInTheURL(t *testing.T) {
	srv, router := keyEnv(t)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)
	mintKey(t, srv, "alice", "nightly", nil, false)
	mintKey(t, srv, "bob", "bobs-key", nil, false)

	rec := doJSONAs(t, srv, router, "root", http.MethodGet, "/api/v1/users/alice/api-keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeKeyList(t, rec.Body.Bytes())
	if len(got.Keys) != 1 || got.Keys[0].Name != "nightly" {
		t.Fatalf("expected only alice's key, got %+v", got.Keys)
	}
	if got.Days <= 0 {
		t.Errorf("days must qualify the request counts, got %d", got.Days)
	}
}

// 404, not 403 and not an empty list: the route must not confirm which user
// ids exist, and it must not let "no such person" read as "no credentials".
func TestUserKeyListRefusesNonAdminsAndUnknownIDs(t *testing.T) {
	srv, router := keyEnv(t)
	mintKey(t, srv, "alice", "nightly", nil, false)

	if rec := doJSONAs(t, srv, router, "alice", http.MethodGet,
		"/api/v1/users/alice/api-keys", nil); rec.Code != http.StatusNotFound {
		t.Errorf("non-admin reading their own account record: got %d, want 404", rec.Code)
	}
	if rec := doJSONAs(t, srv, router, "root", http.MethodGet,
		"/api/v1/users/nobody/api-keys", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user: got %d, want 404", rec.Code)
	}
}

// Off is when an administrator most needs to see what would stop working, so
// this route answers while personal keys are switched off — matching
// /admin/api-keys and deliberately unlike /me/api-keys.
func TestUserKeyListSurvivesTheCapabilityBeingOff(t *testing.T) {
	srv, router := keyEnv(t)
	mintKey(t, srv, "alice", "nightly", nil, false)
	enablePersonalKeys(t, srv, false)

	if rec := doJSONAs(t, srv, router, "alice", http.MethodGet,
		"/api/v1/me/api-keys", nil); rec.Code != http.StatusNotFound {
		t.Errorf("own keys while off: got %d, want 404", rec.Code)
	}
	rec := doJSONAs(t, srv, router, "root", http.MethodGet, "/api/v1/users/alice/api-keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("account record while off: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeKeyList(t, rec.Body.Bytes()); len(got.Keys) != 1 {
		t.Errorf("a switched-off key is still a credential that exists, got %+v", got.Keys)
	}
}

// The usage route resolves the key through the owner in the URL, so a real key
// id under the wrong account is a 404 rather than someone else's traffic drawn
// on this person's record.
func TestUserKeyUsageIsScopedByOwnerNotJustKeyID(t *testing.T) {
	srv, router := keyEnv(t)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)
	mintKey(t, srv, "bob", "bobs-key", nil, false)

	// mintKey names the row "<name>-<owner>", so this is bob's key id asked
	// for under alice.
	if rec := doJSONAs(t, srv, router, "root", http.MethodGet,
		"/api/v1/users/alice/api-keys/bobs-key-bob/usage", nil); rec.Code != http.StatusNotFound {
		t.Errorf("bob's key under alice: got %d, want 404", rec.Code)
	}
	if rec := doJSONAs(t, srv, router, "root", http.MethodGet,
		"/api/v1/users/bob/api-keys/bobs-key-bob/usage", nil); rec.Code != http.StatusOK {
		t.Errorf("bob's key under bob: got %d, want 200", rec.Code)
	}
}

// A token may learn from GET /users/{id} that an account is in use; it may not
// learn which credentials are the ones using it. Both new routes are on the
// denied list, and this walks the real router to prove it.
func TestUserKeyRoutesAreDeniedToKeys(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "nightly", nil, false)

	for _, path := range []string{
		"/api/v1/users/alice/api-keys",
		"/api/v1/users/alice/api-keys/nightly-alice/usage",
	} {
		if rec := getWithKey(t, router, secret, path); rec.Code != http.StatusForbidden {
			t.Errorf("%s with a key: got %d, want 403", path, rec.Code)
		}
	}
}
