package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// **whoami's create answer is the platform's** (drills rerun 2026-10-22 #4).
// can_create_objects was derived from the system role alone, which a deploy
// key does not have, so a key the create route accepts — given a group — was
// told it could not create. It now reads the route list that bounds the key:
// a deploy-scoped key may, a read-scoped one may not.
func TestADeployKeysCreateAnswerMatchesTheCreateRoute(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for scope, want := range map[string]bool{
		database.DeployKeyScopeDeploy: true,
		database.DeployKeyScopeRead:   false,
	} {
		secret := mintScopedDeployKey(t, srv, router, "whoami-"+scope, scope)
		rec := asDeployKey(t, router, http.MethodGet, "/api/v1/me/capabilities", secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: capabilities %d (%s)", scope, rec.Code, rec.Body.String())
		}
		var caps struct {
			CanCreate bool `json:"can_create_objects"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
			t.Fatal(err)
		}
		if caps.CanCreate != want {
			t.Errorf("a %s-scoped deploy key: can_create_objects = %v, want %v", scope, caps.CanCreate, want)
		}
	}
}
