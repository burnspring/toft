package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// The cap's own refusal says "revoke one first", so revoking has to make room.
//
// It counted `ListDeployKeys()`, which returns revoked rows too — deliberately,
// because the audit trail resolves `deploy_key:{id}` to a name. An install that
// minted and revoked its way to thirty therefore refused every new key while
// telling the operator to do the one thing that changed nothing, and since no
// route removes a revoked row the state could not be left through the API at
// all. e2e KEY.4.
func TestRevokingADeployKeyMakesRoomUnderTheCap(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for i := 0; i < maxDeployKeys; i++ {
		mintDeployKey(t, srv, router, fmt.Sprintf("filler-%02d", i))
	}

	// Full, and the refusal is the one the operator is asked to act on.
	rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys",
		`{"name":"one-too-many"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("at the cap: %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "revoke one first") {
		t.Fatalf("the refusal does not say what to do: %s", rec.Body.String())
	}

	keys, err := srv.db.ListDeployKeys()
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	for _, k := range keys {
		if rec := sendAs(t, srv, router, "root", http.MethodDelete,
			"/api/v1/deploy-keys/"+k.ID, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("revoking %s: %d (%s)", k.ID, rec.Code, rec.Body.String())
		}
	}

	// Doing what the refusal said makes room.
	if rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys",
		`{"name":"after-the-purge"}`); rec.Code != http.StatusCreated {
		t.Fatalf("after revoking every key: %d (%s), want 201",
			rec.Code, rec.Body.String())
	}

	// And the revoked rows are still there, so the audit trail still names them.
	after, err := srv.db.ListDeployKeys()
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(after) != maxDeployKeys+1 {
		t.Fatalf("rows after the purge = %d, want %d — revoking is not deleting",
			len(after), maxDeployKeys+1)
	}
}

// A disabled key still holds its place: disabled is reversible, and the row is
// one an operator can bring back rather than one they have given up.
func TestADisabledDeployKeyStillCountsAgainstTheCap(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for i := 0; i < maxDeployKeys; i++ {
		mintDeployKey(t, srv, router, fmt.Sprintf("filler-%02d", i))
	}
	keys, _ := srv.db.ListDeployKeys()
	if rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/deploy-keys/"+keys[0].ID, `{"disabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("disabling: %d (%s)", rec.Code, rec.Body.String())
	}

	if rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys",
		`{"name":"squeezed-in"}`); rec.Code != http.StatusConflict {
		t.Fatalf("with one key disabled: %d (%s), want 409 — disabled is not revoked",
			rec.Code, rec.Body.String())
	}
}

// A name already taken is an ordinary refusal, and the same class the other two
// credential classes answer. It was a 500: a caller could not tell a name clash
// from a broken install. e2e KEY.4.
func TestADuplicateDeployKeyNameIsRefusedTheWayAPersonalOneIs(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	mintDeployKey(t, srv, router, "pipeline")

	rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys",
		`{"name":"pipeline"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a duplicate name: %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("the refusal does not name the clash: %s", rec.Body.String())
	}
	// D-106: name_taken like every other name collision, not the envelope's
	// generic `conflict` — the CLI reads name_taken as a usage error.
	var clash map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &clash)
	if clash["code"] != "name_taken" || clash["hint"] == nil {
		t.Errorf("a duplicate name: %s, want code name_taken with a hint", rec.Body.String())
	}

	// The personal-key route is the comparison the catalog row makes.
	enablePersonalKeys(t, srv, true)
	personal := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/me/api-keys",
		`{"name":"mine"}`)
	if personal.Code != http.StatusCreated {
		t.Fatalf("minting a personal key: %d (%s)", personal.Code, personal.Body.String())
	}
	dup := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/me/api-keys",
		`{"name":"mine"}`)
	if dup.Code != rec.Code {
		t.Fatalf("a duplicate personal key answers %d and a duplicate deploy key %d; "+
			"they are the same refusal", dup.Code, rec.Code)
	}
}
