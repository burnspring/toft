package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/integrations"
)

// fakeIssuer stands in for the SharePoint transport: it hands out a fixed token
// or a fixed refusal, and never dials anything.
type fakeIssuer struct {
	placingTransport
	err error
}

func (f fakeIssuer) CheckToken(context.Context, integrations.Target) error { return f.err }

const fakeAccessToken = "eyJ0eXAiOiJKV1QifQ.e30.secret-token-value"

func (f fakeIssuer) Token(_ context.Context, t integrations.Target) (integrations.IssuedToken, error) {
	if f.err != nil {
		return integrations.IssuedToken{}, f.err
	}
	return integrations.IssuedToken{
		AccessToken: integrations.AccessToken{Value: fakeAccessToken, ExpiresAt: time.Now().Add(time.Hour)},
		Resource:    "https://graph.microsoft.com",
		Folder: integrations.TokenFolder{DriveID: "b!drive", ItemID: t.Destination.ID,
			Label: t.Destination.Label},
	}, nil
}

// newTokenFixture is an app attached to a SharePoint connection whose switch is
// as given, with a fake issuer in the registry.
func newTokenFixture(t *testing.T, issueTokens bool, issuer fakeIssuer) (*appDataFixture, *database.IntegrationConnection) {
	t.Helper()
	f := newAppDataFixture(t)
	conn := &database.IntegrationConnection{
		ID: "iconn-sp", Kind: "sharepoint", Name: "Finance", ForApps: true,
		ScopeID: "contoso.sharepoint.com/sites/finance", ScopeLabel: "Finance",
		DefaultDestination: database.Destination{ID: "01REPORTS", Label: "reports"},
		IssueTokens:        issueTokens,
	}
	if err := f.srv.db.SaveConnection(conn, "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.db.AttachAppIntegration(&database.AppIntegration{
		AppID: f.app.ID, Kind: "sharepoint", ConnectionID: conn.ID, Enabled: true,
	}, "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	f.srv.transports = transportRegistry{"sharepoint": issuer}
	return f, conn
}

func tokenRows(t *testing.T, f *appDataFixture) []database.IntegrationSend {
	t.Helper()
	rows, err := f.srv.db.ListAppSends(f.app.ID, "token", 50)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Off is refused by name, writes nothing, and /app/self does not list it.
func TestATokenIsRefusedWhileTheConnectionSwitchIsOff(t *testing.T) {
	f, _ := newTokenFixture(t, false, fakeIssuer{})
	rec := f.call(t, f.token, http.MethodPost, "/api/v1/app/tokens/sharepoint", "", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "sharepoint_tokens_off") {
		t.Fatalf("switch off = %d %s, want 403 sharepoint_tokens_off", rec.Code, rec.Body.String())
	}
	if rows := tokenRows(t, f); len(rows) != 0 {
		t.Errorf("a refused gate wrote %d ledger rows", len(rows))
	}
	self := f.call(t, f.token, http.MethodGet, "/api/v1/app/self", "", nil)
	if strings.Contains(self.Body.String(), "sharepoint.token") {
		t.Errorf("/app/self lists the token while the switch is off: %s", self.Body.String())
	}
}

// On, the app gets the token, the folder hint and the expiry; the ledger row
// records the issuance and carries nothing token-shaped.
func TestATokenIsHandedOutAndNeverRecorded(t *testing.T) {
	f, _ := newTokenFixture(t, true, fakeIssuer{})
	rec := f.call(t, f.token, http.MethodPost, "/api/v1/app/tokens/sharepoint", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch on = %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		AccessToken string    `json:"access_token"`
		TokenType   string    `json:"token_type"`
		ExpiresAt   time.Time `json:"expires_at"`
		Resource    string    `json:"resource"`
		Folder      struct {
			DriveID string `json:"drive_id"`
			ItemID  string `json:"item_id"`
			Label   string `json:"label"`
		} `json:"folder"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != fakeAccessToken || got.TokenType != "Bearer" ||
		got.Resource != "https://graph.microsoft.com" || time.Until(got.ExpiresAt) < 50*time.Minute {
		t.Errorf("answer = %+v", got)
	}
	if got.Folder.ItemID != "01REPORTS" || got.Folder.Label != "reports" || got.Folder.DriveID != "b!drive" {
		t.Errorf("folder = %+v, want the connection's default", got.Folder)
	}

	rows := tokenRows(t, f)
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Status != database.SendSent || row.Kind != "sharepoint" || row.Destination != "Finance" ||
		!strings.HasPrefix(row.Subject, "expires ") {
		t.Errorf("row = %+v", row)
	}
	blob, _ := json.Marshal(row)
	if strings.Contains(string(blob), "secret-token-value") || strings.Contains(string(blob), "eyJ") {
		t.Errorf("the ledger row carries the token: %s", blob)
	}
	self := f.call(t, f.token, http.MethodGet, "/api/v1/app/self", "", nil)
	if !strings.Contains(self.Body.String(), "sharepoint.token") {
		t.Errorf("/app/self omits the token while the switch is on: %s", self.Body.String())
	}
}

// A too-wide Graph token is a 409 naming the roles, and a blocked row.
func TestATooWideTokenIsRefusedAndRecorded(t *testing.T) {
	f, _ := newTokenFixture(t, true, fakeIssuer{
		err: &integrations.TokenTooWideError{Roles: []string{"Mail.Send"}}})
	rec := f.call(t, f.token, http.MethodPost, "/api/v1/app/tokens/sharepoint", "", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "token_too_wide") ||
		!strings.Contains(rec.Body.String(), "Mail.Send") {
		t.Fatalf("too wide = %d %s, want 409 token_too_wide naming Mail.Send", rec.Code, rec.Body.String())
	}
	if rows := tokenRows(t, f); len(rows) != 1 || rows[0].Status != database.SendBlocked {
		t.Errorf("rows = %+v, want one blocked", rows)
	}
}

// A kind that issues no tokens is an unknown capability, not a 403.
func TestATextKindIssuesNoToken(t *testing.T) {
	f, _ := newTokenFixture(t, true, fakeIssuer{})
	rec := f.call(t, f.token, http.MethodPost, "/api/v1/app/tokens/slack", "", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown_capability") {
		t.Errorf("slack token = %d %s, want 400 unknown_capability", rec.Code, rec.Body.String())
	}
}

// The token route takes a kind, so its unknown_capability hint names kinds —
// "sharepoint", not "sharepoint.token", which the route would read as
// "sharepoint.token.token" (TOFT-BB-008).
func TestAnUnknownTokenKindHintNamesKinds(t *testing.T) {
	f, _ := newTokenFixture(t, true, fakeIssuer{})
	rec := f.call(t, f.token, http.MethodPost, "/api/v1/app/tokens/bogus", "", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || !strings.Contains(body, "unknown_capability") {
		t.Fatalf("bogus token = %d %s, want 400 unknown_capability", rec.Code, body)
	}
	if strings.Contains(body, "sharepoint.token") || !strings.Contains(body, "kind alone") {
		t.Errorf("hint names capabilities, not kinds: %s", body)
	}
}

// The bucket is the token route's own: ten at once, then 429, and the app's
// other calls are untouched.
func TestTokenRequestsHaveABucketOfTheirOwn(t *testing.T) {
	f, _ := newTokenFixture(t, false, fakeIssuer{})
	for i := 0; i < appTokensBurst; i++ {
		f.call(t, f.token, http.MethodPost, "/api/v1/app/tokens/sharepoint", "", nil)
	}
	if rec := f.call(t, f.token, http.MethodPost, "/api/v1/app/tokens/sharepoint", "", nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("past the burst = %d, want 429", rec.Code)
	}
	if rec := f.call(t, f.token, http.MethodGet, "/api/v1/app/data", "", nil); rec.Code != http.StatusOK {
		t.Errorf("app data after the token bucket emptied = %d", rec.Code)
	}
}

// A job's run token reaches the route while the run is in flight.
func TestAJobMayGetAToken(t *testing.T) {
	f, _ := newTokenFixture(t, true, fakeIssuer{})
	run := &database.JobRun{AppID: f.app.ID, JobName: "nightly"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='running' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	tok, err := f.srv.jwt.MintRunToken(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.call(t, tok, http.MethodPost, "/api/v1/app/tokens/sharepoint", "", nil); rec.Code != http.StatusOK {
		t.Errorf("a running job = %d %s, want 200", rec.Code, rec.Body.String())
	}
}
