package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/appint"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/integrations"
	"toft/internal/integrations/teams"
	"toft/internal/notify"
)

// A Teams workflow URL as an administrator pastes it. The host is TEST-NET-3,
// a public literal, so Verify's address check passes without a DNS lookup.
const pastedTeamsURL = "https://203.0.113.9:443/powerautomate/automations/direct/workflows/" +
	"166331419bd741f181eb28bd825992f4/triggers/manual/paths/invoke" +
	"?api-version=1&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=SECRETSIG"

type connEnv struct {
	db     *database.DB
	srv    *Server
	router http.Handler
}

func newConnEnv(t *testing.T) *connEnv {
	t.Helper()
	db := dbtest.New(t)
	srv := newTestServerWithConfig(t, db, &config.Config{BaseURL: "http://localhost:8080"})
	seedUser(t, db, "admin-user", database.SystemRoleAdmin)
	return &connEnv{db: db, srv: srv, router: srv.Router(http.NotFoundHandler())}
}

func (e *connEnv) do(t *testing.T, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookieFor(t, e.srv, "admin-user"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// **The workflow URL's query string is its credential**, so it is sealed at
// save: the scope an administrator, the audit log and every read see is the
// URL without it, the label names the workflow, and the transport alone gets
// the URL back whole.
func TestATeamsSignatureIsSealedAndNeverReadBack(t *testing.T) {
	e := newConnEnv(t)
	rec, made := e.do(t, http.MethodPost, "/api/v1/admin/integrations/connections", map[string]any{
		"kind": "teams", "name": "Build alerts", "scope_id": pastedTeamsURL,
		"description": "Platform team › #builds, workflow owned by Dana",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "SECRETSIG") {
		t.Errorf("the create response carries the signature: %s", rec.Body)
	}
	if got := made["scope_label"]; got != "workflow 1663…92f4" {
		t.Errorf("scope_label = %v, want the workflow id", got)
	}
	if got := made["description"]; got != "Platform team › #builds, workflow owned by Dana" {
		t.Errorf("description = %v", got)
	}
	id, _ := made["id"].(string)

	list, _ := e.do(t, http.MethodGet, "/api/v1/admin/integrations/connections", nil)
	if strings.Contains(list.Body.String(), "SECRETSIG") {
		t.Error("the connections list carries the signature")
	}

	var scope, secret string
	if err := e.db.Conn().QueryRow(`SELECT scope_id, credential_secret FROM integration_connections
		WHERE id = ?`, id).Scan(&scope, &secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scope, "?") || secret == "" || strings.Contains(secret, "SECRETSIG") {
		t.Errorf("stored scope %q, secret set %v — the query belongs sealed", scope, secret != "")
	}
	var audits string
	if err := e.db.Conn().QueryRow(`SELECT COALESCE(string_agg(after::text, ' '), '')
		FROM audit_log WHERE target_id = ?`, id).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audits, "SECRETSIG") {
		t.Error("the audit row carries the signature")
	}

	target, err := e.srv.connectionTarget(id, database.Destination{})
	if err != nil {
		t.Fatalf("connectionTarget: %v", err)
	}
	if got := teams.Endpoint(target); got != pastedTeamsURL {
		t.Errorf("the transport posts to %q, not the URL as pasted", got)
	}
}

// A connection saved before the split holds its whole URL as the scope. It
// keeps working, and it is still never shown with its signature.
func TestAPreSplitTeamsRowIsShownWithoutItsSignature(t *testing.T) {
	e := newConnEnv(t)
	if err := e.db.SaveConnection(&database.IntegrationConnection{
		ID: "iconn_legacy", Kind: "teams", Name: "Old", ScopeID: pastedTeamsURL,
		ScopeLabel: "a Teams workflow", ForApps: true,
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.do(t, http.MethodGet, "/api/v1/admin/integrations/connections", nil)
	if strings.Contains(rec.Body.String(), "SECRETSIG") {
		t.Errorf("a pre-split row is listed with its signature: %s", rec.Body)
	}
	target, err := e.srv.connectionTarget("iconn_legacy", database.Destination{})
	if err != nil {
		t.Fatal(err)
	}
	if got := teams.Endpoint(target); got != pastedTeamsURL {
		t.Errorf("a pre-split row posts to %q", got)
	}
	// Its update and delete are audited without the signature, too.
	if rec, _ := e.do(t, http.MethodPatch, "/api/v1/admin/integrations/connections/iconn_legacy",
		map[string]any{"name": "Older"}); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := e.do(t, http.MethodDelete, "/api/v1/admin/integrations/connections/iconn_legacy",
		nil); rec.Code/100 != 2 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	var rows int
	var audits string
	if err := e.db.Conn().QueryRow(`SELECT count(*),
		COALESCE(string_agg(COALESCE(before::text,'') || ' ' || COALESCE(after::text,''), ' '), '')
		FROM audit_log WHERE target_id = 'iconn_legacy'`).Scan(&rows, &audits); err != nil {
		t.Fatal(err)
	}
	if rows < 2 {
		t.Errorf("expected the update and delete audited, found %d rows", rows)
	}
	if strings.Contains(audits, "SECRETSIG") {
		t.Errorf("a pre-split row's signature reached audit_log: %s", audits)
	}
}

// A workflow URL that points inward is refused at the form, by name.
func TestATeamsURLOnThisNetworkIsRefusedAtSave(t *testing.T) {
	e := newConnEnv(t)
	for _, raw := range []string{
		"https://10.0.0.5/workflows/x/triggers/manual/paths/invoke?sig=x",
		"https://169.254.169.254/latest/meta-data/",
	} {
		rec, body := e.do(t, http.MethodPost, "/api/v1/admin/integrations/connections", map[string]any{
			"kind": "teams", "name": "inward", "scope_id": raw,
		})
		if rec.Code != http.StatusBadRequest || body["code"] != "scope_refused" {
			t.Errorf("%s: %d %s, want 400 scope_refused", raw, rec.Code, rec.Body)
		}
	}
}

// The description is the administrator's own note: optional, editable,
// cleared by "", kept by an update that does not name it, and bounded.
func TestAConnectionDescriptionIsOptionalEditableAndBounded(t *testing.T) {
	e := newConnEnv(t)
	rec, made := e.do(t, http.MethodPost, "/api/v1/admin/integrations/connections", map[string]any{
		"kind": "teams", "name": "No note", "scope_id": pastedTeamsURL,
	})
	if rec.Code != http.StatusCreated || made["description"] != "" {
		t.Fatalf("a connection with no description: %d %s", rec.Code, rec.Body)
	}
	path := "/api/v1/admin/integrations/connections/" + made["id"].(string)

	if rec, got := e.do(t, http.MethodPatch, path, map[string]any{"description": "  Owned by Dana  "}); rec.Code != 200 ||
		got["description"] != "Owned by Dana" {
		t.Errorf("set: %d %v", rec.Code, got["description"])
	}
	if rec, got := e.do(t, http.MethodPatch, path, map[string]any{"name": "Renamed"}); rec.Code != 200 ||
		got["description"] != "Owned by Dana" {
		t.Errorf("an update that does not name it changed it: %d %v", rec.Code, got["description"])
	}
	if rec, got := e.do(t, http.MethodPatch, path, map[string]any{"description": ""}); rec.Code != 200 ||
		got["description"] != "" {
		t.Errorf("clear: %d %v", rec.Code, got["description"])
	}
	if rec, _ := e.do(t, http.MethodPatch, path, map[string]any{"description": strings.Repeat("x", 1001)}); rec.Code != 400 {
		t.Errorf("a 1001-character description: %d, want 400", rec.Code)
	}
}

// placingTransport is a file kind's far side that keeps what it is given.
type placingTransport struct{}

func (placingTransport) Send(_ context.Context, _ integrations.Target,
	s integrations.Send) (integrations.Result, error) {
	return integrations.Result{Placement: &integrations.Placement{
		ItemID: "item-1", ItemURL: "https://contoso.sharepoint.com/sites/x/toft-test.txt",
		Name: s.File.Name, Bytes: int64(len(s.File.Content)), Type: s.File.Type,
	}}, nil
}

// A connection's Test button puts a real file in the library, and a file this
// platform put in a library is a placement (§72) -- one with no app and no send.
func TestAConnectionTestThatLeavesAFileRecordsItsPlacement(t *testing.T) {
	e := newConnEnv(t)
	if err := e.db.SaveConnection(&database.IntegrationConnection{
		ID: "iconn_sp", Kind: "sharepoint", Name: "Finance site",
		ScopeID: "contoso.sharepoint.com/sites/x", ScopeLabel: "x", ForApps: true,
		DefaultDestination: database.Destination{ID: "folder-1", Label: "reports"},
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if e.srv.transports == nil {
		e.srv.transports = transportRegistry{}
	}
	e.srv.transports["sharepoint"] = placingTransport{}

	rec, out := e.do(t, http.MethodPost, "/api/v1/admin/integrations/connections/iconn_sp/test", map[string]any{})
	if rec.Code != http.StatusOK || out["sent"] != true {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	rows, err := e.db.ListPlacements("iconn_sp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the test's file left %d placements, want 1", len(rows))
	}
	p := rows[0]
	if p.Name != testFileName || p.ItemID != "item-1" || p.Destination.ID != "folder-1" ||
		p.ConnectionName != "Finance site" || p.AppID != "" || p.SendID != "" {
		t.Errorf("placement %+v", p)
	}
}

// The token switch is a file kind's alone, audited with the connection, and
// while it is on the connection's card says when a token would be refused.
func TestTheTokenSwitchIsAFileKindsAndReportsAWideToken(t *testing.T) {
	e := newConnEnv(t)
	for _, c := range []*database.IntegrationConnection{
		{ID: "iconn_sp", Kind: "sharepoint", Name: "Finance site",
			ScopeID: "contoso.sharepoint.com/sites/x", ScopeLabel: "x", ForApps: true},
		{ID: "iconn_wh", Kind: "webhook", Name: "Receiver",
			ScopeID: "https://example.com/hook", ScopeLabel: "example.com", ForApps: true},
	} {
		if err := e.db.SaveConnection(c, "admin-user", database.RequestMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.transports = transportRegistry{"sharepoint": fakeIssuer{
		err: &integrations.TokenTooWideError{Roles: []string{"Mail.Send"}}}}

	rec, _ := e.do(t, http.MethodPatch, "/api/v1/admin/integrations/connections/iconn_wh",
		map[string]any{"issue_tokens": true})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tokens_not_offered") {
		t.Errorf("a webhook's token switch = %d %s, want 400 tokens_not_offered", rec.Code, rec.Body)
	}

	rec, out := e.do(t, http.MethodPatch, "/api/v1/admin/integrations/connections/iconn_sp",
		map[string]any{"issue_tokens": true})
	if rec.Code != http.StatusOK || out["issue_tokens"] != true {
		t.Fatalf("switching tokens on = %d %s", rec.Code, rec.Body)
	}
	if problem, _ := out["token_problem"].(string); !strings.Contains(problem, "Mail.Send") {
		t.Errorf("token_problem = %q, want the wide role named", problem)
	}
	var after string
	if err := e.db.Conn().QueryRow(`SELECT after FROM audit_log
		WHERE action = 'integration.connection.update' AND target_id = 'iconn_sp'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(after, " ", ""), `"issue_tokens":true`) {
		t.Errorf("the audit row does not record the switch: %s", after)
	}
}

// A static app has no process and no job to send from, so attaching an
// integration to one is refused static_app, as storage is — before, email
// attached with `email.send` among capabilities nothing could use (docs/bugs
// 2026-10-28 D13). Switching one off stays allowed.
func TestAttachingAnIntegrationToAStaticAppIsRefused(t *testing.T) {
	e := newConnEnv(t)
	if err := e.db.SaveConnection(&database.IntegrationConnection{
		ID: "iconn_static", Kind: "teams", Name: "Teams", ScopeID: "https://example.com/hook",
		ScopeLabel: "example.com", ForApps: true,
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	for _, fw := range []string{frameworkStatic, "flask"} {
		app := &database.App{Slug: "int-" + fw, Name: fw, OwnerID: "admin-user",
			Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
			RepoURL: "https://github.com/example/demo"}
		if err := e.db.CreateAppTx(app, "", "admin-user", "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := e.db.SetAppDetection(app.ID, fw, "", ""); err != nil {
			t.Fatal(err)
		}
		// The tab offers what the attach accepts: nothing, on a static app
		// (docs/bugs 2026-10-30 O1).
		list, _ := e.do(t, http.MethodGet, "/api/v1/apps/"+app.ID+"/integrations", nil)
		offered := strings.Contains(list.Body.String(), `"kind":"teams"`)
		if offered == (fw == frameworkStatic) {
			t.Errorf("a %s app's integrations offer a connected kind = %v: %s", fw, offered, list.Body)
		}
		rec, _ := e.do(t, http.MethodPut, "/api/v1/apps/"+app.ID+"/integrations/email", map[string]any{})
		static := strings.Contains(rec.Body.String(), "static_app")
		if fw == frameworkStatic && (rec.Code != http.StatusConflict || !static) {
			t.Errorf("attaching email to a static app = %d %s, want 409 static_app", rec.Code, rec.Body)
		}
		if fw != frameworkStatic && static {
			t.Errorf("a %s app was refused as static: %s", fw, rec.Body)
		}
		if fw == frameworkStatic {
			off := false
			if rec, _ := e.do(t, http.MethodPut, "/api/v1/apps/"+app.ID+"/integrations/email",
				map[string]any{"enabled": &off}); strings.Contains(rec.Body.String(), "static_app") {
				t.Errorf("switching an attachment off was refused as static: %s", rec.Body)
			}
		}
	}
}

// recordingTransport records every Send it is handed.
type recordingTransport struct{ sent chan integrations.Send }

func (r recordingTransport) Send(_ context.Context, _ integrations.Target,
	s integrations.Send) (integrations.Result, error) {
	r.sent <- s
	return integrations.Result{}, nil
}

// "Apps can send" governs what apps send, not Toft's own notices about them: a
// Teams bot connection with for_apps off still carries an app's crash notice,
// as Slack's poster always did. The attachment is still a gate.
func TestAPlatformNoticeAboutAnAppIgnoresTheAppsCanSendSwitch(t *testing.T) {
	e := newConnEnv(t)
	if err := e.db.SaveConnection(&database.IntegrationConnection{
		ID: "iconn_bot", Kind: appint.IntegrationTeamsBot, Name: "Bot", ScopeID: "tenant-1",
		ScopeLabel: "Contoso", ForApps: false, ForPlatform: true,
		PlatformAudiences:  []string{notify.AudienceApp},
		DefaultDestination: database.Destination{ID: "19:chan", Label: "General"},
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	app := &database.App{Slug: "notice-app", Name: "Notice", OwnerID: "admin-user",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
		RepoURL: "https://github.com/example/demo"}
	if err := e.db.CreateAppTx(app, "", "admin-user", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AttachAppIntegration(&database.AppIntegration{
		AppID: app.ID, Kind: appint.IntegrationTeamsBot, ConnectionID: "iconn_bot", Enabled: true,
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	fake := recordingTransport{sent: make(chan integrations.Send, 1)}
	poster := catalogPoster{db: e.db, enc: e.srv.encryptor,
		transports: transportRegistry{appint.IntegrationTeamsBot: fake}}
	d := notify.TemplateData{AppName: "Notice", EventTitle: "Crashed", Severity: "error"}

	if !poster.PostAsync("app_event", app.ID, d) {
		t.Fatal("a notice about an attached app was not dispatched with for_apps off")
	}
	select {
	case <-fake.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the transport was never called")
	}

	if err := e.db.AttachAppIntegration(&database.AppIntegration{
		AppID: app.ID, Kind: appint.IntegrationTeamsBot, ConnectionID: "iconn_bot", Enabled: false,
	}, "admin-user", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if poster.PostAsync("app_event", app.ID, d) {
		t.Error("a notice was dispatched through a disabled attachment")
	}
}
