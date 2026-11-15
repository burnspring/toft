package gallery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func setMode(t *testing.T, db *database.DB, mode string) {
	t.Helper()
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	s.GalleryMode = mode
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
}

// serve returns a TLS test server and a counter of requests it received.
func serve(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func serviceFor(t *testing.T, db *database.DB, srv *httptest.Server) *Service {
	t.Helper()
	svc := New(db, srv.URL, "built-in", "1.0.0")
	svc.fetcher = testFetcher(t, srv, false)
	return svc
}

// The whole point of the default. An install nobody has configured must not
// contact a public repository — not a request whose result is discarded, no
// request.
func TestSyncMakesNoRequestWhileTheGalleryIsOff(t *testing.T) {
	db := dbtest.New(t)
	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema_version":1,"items":[]}`)
	})
	svc := serviceFor(t, db, srv)

	settings, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.GalleryMode != database.GalleryOff {
		t.Fatalf("a fresh install has the gallery %q, want off", settings.GalleryMode)
	}

	if err := svc.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the catalog host saw %d requests while the gallery was off", n)
	}

	// And nothing was recorded either: an install that never synced must not
	// look like one whose sync failed.
	cache, err := db.GetGalleryCatalog()
	if err != nil {
		t.Fatalf("GetGalleryCatalog: %v", err)
	}
	if cache.Status != "" || cache.FetchedAt != nil {
		t.Fatalf("cache = %+v, want untouched", cache)
	}
}

func TestSyncCachesTheDocumentAndServesItFromMemory(t *testing.T) {
	db := dbtest.New(t)
	setMode(t, db, database.GalleryAll)

	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, `{"schema_version":1,"items":[`+goodExtension+`,`+goodExample+`]}`)
	})
	svc := serviceFor(t, db, srv)

	if err := svc.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want 1", n)
	}

	cache, err := db.GetGalleryCatalog()
	if err != nil {
		t.Fatalf("GetGalleryCatalog: %v", err)
	}
	if cache.Status != database.GallerySyncOK || cache.ItemCount != 2 || cache.ETag != `"v1"` {
		t.Fatalf("cache = %+v", cache)
	}
	if c := svc.Catalog(); c == nil || len(c.Items) != 2 {
		t.Fatalf("in-memory catalog = %+v", c)
	}

	// A second service on the same database starts from the cached document
	// rather than an empty gallery — an install that boots offline still has
	// its catalog.
	cold := New(db, srv.URL, "built-in", "1.0.0")
	cold.loadCached()
	if c := cold.Catalog(); c == nil || len(c.Items) != 2 {
		t.Fatalf("cold start catalog = %+v", c)
	}
}

func TestAFailedSyncKeepsTheLastGoodCatalog(t *testing.T) {
	db := dbtest.New(t)
	setMode(t, db, database.GalleryAll)

	var fail atomic.Bool
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"schema_version":1,"items":[`+goodExtension+`]}`)
	})
	svc := serviceFor(t, db, srv)

	if err := svc.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	fail.Store(true)
	if err := svc.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync returned an error rather than recording one: %v", err)
	}

	cache, err := db.GetGalleryCatalog()
	if err != nil {
		t.Fatalf("GetGalleryCatalog: %v", err)
	}
	if cache.Status != database.GallerySyncUnreachable || cache.Error == "" {
		t.Fatalf("cache = %+v, want an unreachable status with its reason", cache)
	}
	if cache.Document == "" || cache.ItemCount != 1 {
		t.Fatalf("the last good document must survive a failed refresh: %+v", cache)
	}
	if c := svc.Catalog(); c == nil || len(c.Items) != 1 {
		t.Fatalf("the in-memory catalog must survive too: %+v", c)
	}
}

func TestSyncRecordsACatalogThatNeedsANewerToft(t *testing.T) {
	db := dbtest.New(t)
	setMode(t, db, database.GalleryAll)

	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema_version":99,"items":[]}`)
	})
	svc := serviceFor(t, db, srv)

	if err := svc.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	cache, err := db.GetGalleryCatalog()
	if err != nil {
		t.Fatalf("GetGalleryCatalog: %v", err)
	}
	if cache.Status != database.GallerySyncTooNew {
		t.Fatalf("status = %q, want %q — a newer catalog is a state, not a breakage",
			cache.Status, database.GallerySyncTooNew)
	}
}

func TestSyncNowIsRateLimited(t *testing.T) {
	db := dbtest.New(t)
	setMode(t, db, database.GalleryAll)
	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema_version":1,"items":[]}`)
	})
	svc := serviceFor(t, db, srv)

	if err := svc.SyncNow(context.Background()); err != nil {
		t.Fatalf("first SyncNow: %v", err)
	}
	if err := svc.SyncNow(context.Background()); err != ErrSyncTooSoon {
		t.Fatalf("second SyncNow = %v, want ErrSyncTooSoon", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want 1", n)
	}
}

func TestItemsForFiltersByModeAndRole(t *testing.T) {
	db := dbtest.New(t)
	setMode(t, db, database.GalleryAll)
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema_version":1,"items":[`+goodExtension+`,`+goodExample+`]}`)
	})
	svc := serviceFor(t, db, srv)
	if err := svc.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	cases := []struct {
		mode  string
		admin bool
		want  []string
	}{
		{database.GalleryAll, true, []string{"ext-1", "ex-1"}},
		{database.GalleryAll, false, []string{"ex-1"}},
		{database.GalleryExtensionsOnly, true, []string{"ext-1"}},
		{database.GalleryExtensionsOnly, false, nil},
		{database.GalleryOff, true, nil},
	}
	for _, tc := range cases {
		got := svc.ItemsFor(tc.mode, tc.admin)
		if len(got) != len(tc.want) {
			t.Errorf("mode=%s admin=%v: got %d items, want %d", tc.mode, tc.admin, len(got), len(tc.want))
			continue
		}
		for i, id := range tc.want {
			if got[i].ID != id {
				t.Errorf("mode=%s admin=%v: item %d = %q, want %q", tc.mode, tc.admin, i, got[i].ID, id)
			}
		}
	}
}

func TestDeployableNamesItsRefusal(t *testing.T) {
	svc := New(nil, "https://example.com/c.json", "built-in", "0.3.0")

	ext := Item{Kind: KindExtension, Version: "1.0.0"}
	ex := Item{Kind: KindExample, Version: "1.0.0"}

	if ok, why := svc.Deployable(ext, database.GalleryOff, true); ok || why == "" {
		t.Errorf("off: ok=%v why=%q", ok, why)
	}
	if ok, why := svc.Deployable(ex, database.GalleryExtensionsOnly, true); ok || why == "" {
		t.Errorf("extensions_only must refuse an example: ok=%v why=%q", ok, why)
	}
	if ok, why := svc.Deployable(ext, database.GalleryAll, false); ok || why == "" {
		t.Errorf("a non-admin must be refused an extension: ok=%v why=%q", ok, why)
	}
	if ok, _ := svc.Deployable(ex, database.GalleryAll, false); !ok {
		t.Error("a publisher may deploy an example when the mode is all")
	}

	newer := Item{Kind: KindExample, Version: "1.0.0", MinPlatformVersion: "0.9.0"}
	ok, why := svc.Deployable(newer, database.GalleryAll, true)
	if ok || why == "" {
		t.Errorf("an item needing a newer platform must be refused with the requirement: %v %q", ok, why)
	}
}
