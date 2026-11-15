package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/integrations/teamsbot"
)

// A Teams bot connection, saved straight through the database so no token is
// fetched: the far side's answers are teamsbot's own tests, against a recorded
// Bot Connector. What is held here is the platform's half.
func seedBotConnection(t *testing.T, e *connEnv) string {
	t.Helper()
	if err := e.db.SaveConnection(&database.IntegrationConnection{
		ID: "iconn_bot", Kind: "teamsbot", Name: "Build alerts",
		ScopeID: "fb149f69-7608-41ef-8dc4-41992f47b59e", ScopeLabel: "bot fb149f69",
		Config:  map[string]string{"tenant_id": "8115b923-66c5-4aba-90e1-3f2981afc76a"},
		ForApps: true, ForPlatform: true, PlatformAudiences: []string{"app"},
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	return "iconn_bot"
}

// Places belong to the bot kind alone, and a link is refused before anything is
// asked of Teams when it cannot name a team or chat of this tenant.
func TestPlacesRefuseOtherKindsAndUnreadableLinks(t *testing.T) {
	e := newConnEnv(t)
	bot := seedBotConnection(t, e)
	_ = e.db.SaveConnection(&database.IntegrationConnection{ID: "iconn_hook", Kind: "webhook",
		Name: "Hook", ScopeID: "https://203.0.113.9/h", ForApps: true}, "admin-user", database.RequestMeta{})

	if rec, body := e.do(t, http.MethodGet, "/api/v1/admin/integrations/connections/iconn_hook/places", nil); rec.Code != 404 ||
		body["code"] != "no_places" {
		t.Errorf("a webhook's places: %d %v", rec.Code, body)
	}
	for link, want := range map[string]string{
		"https://teams.microsoft.com/l/channel/19%3Ac%40thread.tacv2/General?groupId=g": "Get link to team",
		"https://teams.microsoft.com/l/team/19%3Ac%40thread.tacv2/x?tenantId=other":     "another Microsoft 365 tenant",
		"https://example.com/not-teams":                                                 "paste a link from Teams",
	} {
		rec, body := e.do(t, http.MethodPost, "/api/v1/admin/integrations/connections/"+bot+"/places",
			map[string]string{"link": link})
		if rec.Code != 400 || !strings.Contains(body["error"].(string), want) {
			t.Errorf("%s: %d %v, want 400 naming %q", link, rec.Code, body, want)
		}
	}
}

// A place an attachment or the default destination points into cannot be
// removed, and the refusal names who holds it.
func TestAPlaceInUseCannotBeRemoved(t *testing.T) {
	e := newConnEnv(t)
	bot := seedBotConnection(t, e)
	place := &database.IntegrationPlace{ID: "ipl_1", ConnectionID: bot, Kind: database.PlaceTeam,
		ConversationID: "19:team@thread.tacv2", Name: "Toft Test"}
	if err := e.db.AddPlace(place, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	app := &database.App{ID: "app-1", Slug: "poster", Name: "Poster",
		OwnerID: "admin-user", Status: database.AppStatusRunning}
	if err := e.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AttachAppIntegration(&database.AppIntegration{AppID: app.ID, Kind: "teamsbot",
		ConnectionID: bot, Enabled: true, Destination: database.Destination{
			ID: teamsbot.ChannelDestination(place.ConversationID, "19:general"), Label: "Toft Test › General"},
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/integrations/connections/" + bot + "/places/ipl_1"
	rec, body := e.do(t, http.MethodDelete, path, nil)
	if rec.Code != 409 || !strings.Contains(body["error"].(string), "poster") {
		t.Fatalf("removing a place an app uses: %d %v", rec.Code, body)
	}
	if err := e.db.DetachAppIntegration(app.ID, "teamsbot", "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.do(t, http.MethodDelete, path, nil); rec.Code != 200 {
		t.Errorf("removing a place nothing uses: %d", rec.Code)
	}
	if rec, _ := e.do(t, http.MethodDelete, path, nil); rec.Code != 404 {
		t.Errorf("removing it twice: %d", rec.Code)
	}
}

// The admin audience needs a default destination: dropped quietly on
// create, refused on an update that asks for it.
func TestTheBotsAdminAudienceNeedsADefaultChannel(t *testing.T) {
	e := newConnEnv(t)
	bot := seedBotConnection(t, e)
	rec, body := e.do(t, http.MethodPatch, "/api/v1/admin/integrations/connections/"+bot,
		map[string]any{"for_platform": true, "platform_audiences": []string{"admin", "app"}})
	if rec.Code != 400 || body["code"] != "no_default_destination" {
		t.Errorf("admin audience with no default: %d %v", rec.Code, body)
	}
	rec, body = e.do(t, http.MethodPatch, "/api/v1/admin/integrations/connections/"+bot,
		map[string]any{"for_platform": true, "platform_audiences": []string{"admin", "app"},
			"default_destination_id":    teamsbot.ChannelDestination("19:t@thread.tacv2", "19:t@thread.tacv2"),
			"default_destination_label": "Toft Test › General"})
	if rec.Code != 200 {
		t.Errorf("admin audience with a default: %d %v", rec.Code, body)
	}
}

// The package downloads as a zip whose manifest names the connection's bot and
// is the same app on every download.
func TestTheTeamsAppPackageIsTheConnectionsApp(t *testing.T) {
	e := newConnEnv(t)
	bot := seedBotConnection(t, e)
	get := func() []byte {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/integrations/connections/"+bot+"/teams-app", nil)
		req.AddCookie(sessionCookieFor(t, e.srv, "admin-user"))
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" {
			t.Fatalf("package: %d %s", rec.Code, rec.Header().Get("Content-Type"))
		}
		return rec.Body.Bytes()
	}
	manifest := func(pkg []byte) map[string]any {
		z, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range z.File {
			if f.Name == "manifest.json" {
				rc, _ := f.Open()
				m := map[string]any{}
				_ = json.NewDecoder(rc).Decode(&m)
				return m
			}
		}
		t.Fatal("no manifest.json")
		return nil
	}
	a, b := manifest(get()), manifest(get())
	if a["id"] != b["id"] || a["id"] != teamsbot.ManifestID(bot) {
		t.Errorf("manifest ids %v and %v, want %s both times", a["id"], b["id"], teamsbot.ManifestID(bot))
	}
	bots, _ := a["bots"].([]any)
	if len(bots) != 1 || bots[0].(map[string]any)["botId"] != "fb149f69-7608-41ef-8dc4-41992f47b59e" {
		t.Errorf("bots = %v", a["bots"])
	}
}
