package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// may_attach is an administrator's choice, set at create or toggled, audited,
// and refused on a read key — by the handler and by the CHECK underneath
// (D19).
func TestMayAttachIsADeployKeysAndAuditedByWhoSetIt(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys",
		`{"name":"reader","scope":"read","may_attach":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "deploy-scoped") {
		t.Fatalf("may_attach on a read key at create = %d %s", rec.Code, rec.Body.String())
	}
	rec = sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys", `{"name":"pipeline","may_attach":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key struct {
			ID        string `json:"id"`
			MayAttach bool   `json:"may_attach"`
		} `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if !created.Key.MayAttach {
		t.Fatalf("created key = %+v, want may_attach", created.Key)
	}

	rec = sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/deploy-keys/"+created.Key.ID, `{"may_attach":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle = %d %s", rec.Code, rec.Body.String())
	}
	if k, _ := srv.db.GetDeployKey(created.Key.ID); k.MayAttach {
		t.Error("the toggle did not land")
	}
	var n int
	srv.db.Conn().QueryRow(`SELECT count(*) FROM audit_log WHERE action='deploy_key.may_attach_set' AND actor_id='root' AND target_id=?`,
		created.Key.ID).Scan(&n)
	if n != 1 {
		t.Errorf("may_attach_set audit rows = %d, want 1 naming the administrator", n)
	}

	rec = sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys", `{"name":"reader","scope":"read"}`)
	var reader struct {
		Key struct{ ID string } `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &reader)
	if rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/deploy-keys/"+reader.Key.ID,
		`{"may_attach":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("may_attach toggled on a read key = %d, want 400", rec.Code)
	}
	if _, err := srv.db.Conn().Exec(`UPDATE deploy_keys SET may_attach = true WHERE id = ?`, reader.Key.ID); err == nil {
		t.Error("the CHECK let a read key carry may_attach")
	}
}

// A scope that is neither is 400 invalid_scope, as every invalid_* is (BB-019).
func TestAnUnknownDeployKeyScopeIs400(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys", `{"name":"x","scope":"admin"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_scope") {
		t.Fatalf("scope admin = %d %s, want 400 invalid_scope", rec.Code, rec.Body.String())
	}
}
