package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// The banner is readable by anyone, and who set it is told only to an
// administrator: the anonymous internet and a signed-in viewer get the message
// and not an administrator's account name.
func TestTheBannerNamesItsAuthorOnlyToAnAdministrator(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "viewer", database.SystemRoleViewer)

	if rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/announcement",
		`{"message":"Maintenance at noon","level":"info"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	for who, wantBy := range map[string]bool{"": false, "viewer": false, "root": true} {
		rec := sendJSONAs(t, srv, router, who, http.MethodGet, "/api/v1/announcement", "")
		var got announcementResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET as %q = %d %s", who, rec.Code, rec.Body.String())
		}
		if got.Message != "Maintenance at noon" {
			t.Errorf("GET as %q: message = %q", who, got.Message)
		}
		if (got.UpdatedBy != "") != wantBy {
			t.Errorf("GET as %q: updated_by = %q, want it only for an administrator", who, got.UpdatedBy)
		}
	}
}
