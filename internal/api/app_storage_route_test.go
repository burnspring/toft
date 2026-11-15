package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// `GET /api/v1/app/storage` is TOFT_STORAGE over HTTP, and an app that reads
// one must not be told something the other contradicts. A hand-built copy of the
// descriptor here dropped the folder — so an app with one resource mapped twice
// got two entries it could not tell apart.
func TestAppStorageRouteCarriesTheFolder(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	f := &appDataFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{Slug: "recon", Name: "Recon", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	res := seedStore(t, srv.db, "drop")
	for _, m := range []database.StorageMapping{
		{ResourceID: res.ID, AppID: app.ID, Path: "/data/all", Access: runtime.AccessRead},
		{ResourceID: res.ID, AppID: app.ID, Path: "/data/in", Access: runtime.AccessWrite, Folder: "in"},
	} {
		m := m
		if err := srv.db.PutStorageMapping(&m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
			t.Fatalf("PutStorageMapping: %v", err)
		}
	}
	token := f.mintToken(t, app.ID, "tok-storage")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/app/storage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /app/storage = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Storage []runtime.MountDescriptor `json:"storage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	folders := map[string]string{}
	for _, d := range body.Storage {
		folders[d.Path] = d.Folder
	}
	if len(folders) != 2 || folders["/data/in"] != "in" || folders["/data/all"] != "" {
		t.Errorf("storage = %+v, want the folder on the folder mapping", body.Storage)
	}
}
