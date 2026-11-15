package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// DB1-3: a rotation that adds a key an attached app already sets is refused
// with the attach's 409 key_collision, naming the app — not a 500.
func TestRotatingOntoAnAppsOwnKeyIsAKeyCollision(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "alice", database.SystemRolePublisher)
	app := &database.App{Slug: "reports", Name: "Reports", OwnerID: "alice",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.SetEnvVar(app.ID, "PW", "mine", false); err != nil {
		t.Fatal(err)
	}
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "root"}
	c, err := srv.db.CreateCredential(context.Background(), "warehouse", "",
		map[string]string{"USER": "svc"}, srv.encryptor.Encrypt, audit)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}

	rec := sendJSONAs(t, srv, router, "root", http.MethodPatch, "/api/v1/admin/credentials/"+c.ID,
		`{"values":{"PW":"theirs"},"keep":["USER"]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rotation adding a colliding key = %d %s, want 409", rec.Code, rec.Body.String())
	}
	var body struct {
		Code       string                  `json:"code"`
		App        string                  `json:"app"`
		Collisions []database.KeyCollision `json:"collisions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "key_collision" || body.App != "Reports" ||
		len(body.Collisions) != 1 || body.Collisions[0].Key != "PW" {
		t.Errorf("refusal = %+v, want key_collision on Reports for PW", body)
	}

	// A rotation of the keys it already has is not a collision with itself.
	rec = sendJSONAs(t, srv, router, "root", http.MethodPatch, "/api/v1/admin/credentials/"+c.ID,
		`{"values":{"USER":"svc2"}}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("plain rotation = %d %s, want 204", rec.Code, rec.Body.String())
	}
}
