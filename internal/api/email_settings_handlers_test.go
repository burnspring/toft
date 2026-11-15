package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	mailer "toft/internal/mail"
)

// fakeMailer records what would have been sent. Synchronous by construction —
// the real service's fire-and-forget goroutine is exactly the thing a test must
// not inherit, or assertions race the send.
type fakeMailer struct {
	mu       sync.Mutex
	sent     []mailer.Message
	enabled  bool
	testErr  error
	testSent []string
	// db backs AdminRecipients, so the fake answers "who would be told?" from
	// the same two sources the real service does — the stored group address, then
	// the administrators. A fake that returned a canned list would let a test
	// pass while the group address was ignored.
	db *database.DB
	// appAudienceOff mirrors the administrator's "stop telling my users
	// things" switch, so a test can have a configured relay that still offers
	// no share notification. Separate from `enabled` because they are separate
	// switches and the share checkbox has to be absent for either.
	appAudienceOff bool
}

func (f *fakeMailer) Enabled() bool { return f.enabled }

func (f *fakeMailer) AppNotificationsEnabled() bool { return f.enabled && !f.appAudienceOff }
func (f *fakeMailer) BaseURL() string               { return "http://localhost:8080" }

func (f *fakeMailer) AdminRecipients() ([]string, error) { return f.db.AdminRecipients() }

func (f *fakeMailer) SendAsync(m mailer.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
}

func (f *fakeMailer) SendTest(_ context.Context, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.testErr != nil {
		return f.testErr
	}
	f.testSent = append(f.testSent, to)
	return nil
}

func (f *fakeMailer) messages() []mailer.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mailer.Message(nil), f.sent...)
}

// recipients flattens every address the mailer was handed.
func (f *fakeMailer) recipients() []string {
	var out []string
	for _, m := range f.messages() {
		out = append(out, m.To...)
	}
	return out
}

func newMailTestServer(t *testing.T) (*Server, http.Handler, *fakeMailer) {
	t.Helper()
	db := dbtest.New(t)
	fake := &fakeMailer{enabled: true, db: db}
	srv := newTestServerWithConfig(t, db, &config.Config{BaseURL: "http://localhost:8080"}, WithMailer(fake))
	return srv, srv.Router(http.NotFoundHandler()), fake
}

func sendJSONAs(t *testing.T, srv *Server, router http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		r.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

func TestEmailSettings_NonAdminGets404(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for _, tc := range []struct {
		who    string
		method string
		path   string
		body   string
		want   int
	}{
		// Anonymous gets the same 404 as a signed-in non-admin: the route's
		// existence is itself the infrastructure detail being withheld.
		{who: "", method: http.MethodGet, path: "/api/v1/admin/email-settings", want: http.StatusNotFound},
		{who: "pub", method: http.MethodGet, path: "/api/v1/admin/email-settings", want: http.StatusNotFound},
		{who: "pub", method: http.MethodPut, path: "/api/v1/admin/email-settings", body: `{"enabled":false}`, want: http.StatusNotFound},
		{who: "pub", method: http.MethodPost, path: "/api/v1/admin/email-settings/test", want: http.StatusNotFound},
		{who: "root", method: http.MethodGet, path: "/api/v1/admin/email-settings", want: http.StatusOK},
	} {
		t.Run(tc.who+" "+tc.method, func(t *testing.T) {
			rec := sendJSONAs(t, srv, router, tc.who, tc.method, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("%s %s as %q = %d, want %d (%s)", tc.method, tc.path, tc.who, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestEmailSettings_PasswordNeverComesBack(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings", `{
		"enabled": true, "host": "smtp.example.com", "port": 587, "security": "starttls",
		"username": "toft", "password": "hunter2", "from_address": "toft@example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("the PUT response echoed the SMTP password back")
	}

	rec = sendJSONAs(t, srv, router, "root", http.MethodGet, "/api/v1/admin/email-settings", "")
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatal("GET returned the SMTP password")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["password_set"] != true {
		t.Errorf("password_set = %v, want true", got["password_set"])
	}
	if got["host"] != "smtp.example.com" {
		t.Errorf("host = %v", got["host"])
	}

	// Stored encrypted, not in the clear.
	stored, err := srv.db.GetEmailSettings()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Password == "hunter2" || stored.Password == "" {
		t.Fatalf("stored password = %q, want ciphertext", stored.Password)
	}
	plain, err := srv.encryptor.Decrypt(stored.Password)
	if err != nil || plain != "hunter2" {
		t.Fatalf("decrypt stored password = %q, %v", plain, err)
	}
}

func TestEmailSettings_EmptyPasswordKeepsStored(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	base := `{"enabled":true,"host":"smtp.example.com","port":587,"security":"starttls",
		"username":"toft","password":"hunter2","from_address":"toft@example.com"}`
	if rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings", base); rec.Code != http.StatusOK {
		t.Fatalf("first PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	before, _ := srv.db.GetEmailSettings()

	// A re-save that never touched the password field: the form omits it.
	rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings",
		`{"host":"smtp2.example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	after, _ := srv.db.GetEmailSettings()
	if after.Password != before.Password {
		t.Error("omitting the password changed the stored one")
	}
	if after.Host != "smtp2.example.com" {
		t.Errorf("host = %q, want the updated one", after.Host)
	}

	// An explicitly empty string means the same thing.
	rec = sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings",
		`{"password":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("third PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	after, _ = srv.db.GetEmailSettings()
	if after.Password != before.Password {
		t.Error("an empty password string cleared the stored one")
	}
}

func TestEmailSettings_ValidationOnlyWhenEnabled(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "half-filled but off saves", body: `{"enabled":false,"host":"smtp.example.com"}`, want: http.StatusOK},
		{name: "enabled without host", body: `{"enabled":true,"from_address":"a@example.com","host":""}`, want: http.StatusBadRequest},
		{name: "enabled without from", body: `{"enabled":true,"host":"smtp.example.com","from_address":""}`, want: http.StatusBadRequest},
		{name: "bad from address", body: `{"enabled":true,"host":"smtp.example.com","from_address":"nope"}`, want: http.StatusBadRequest},
		{name: "bad reply-to", body: `{"enabled":true,"host":"smtp.example.com","from_address":"a@example.com","reply_to":"@@"}`, want: http.StatusBadRequest},
		{name: "unknown security", body: `{"security":"quantum"}`, want: http.StatusBadRequest},
		{name: "port out of range", body: `{"enabled":true,"host":"smtp.example.com","from_address":"a@example.com","port":99999}`, want: http.StatusBadRequest},
		{name: "complete and enabled", body: `{"enabled":true,"host":"smtp.example.com","port":25,"security":"none","from_address":"a@example.com"}`, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("PUT %s = %d, want %d (%s)", tc.body, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The API key is the second credential in this document and gets the same
// contract as the first: write-only through the API, ciphertext at rest, two
// booleans in the audit row.
func TestEmailSettings_APIKeyNeverComesBack(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings", `{
		"enabled": true, "transport": "api", "api_provider": "resend",
		"api_key": "re_live_secret", "from_address": "toft@example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "re_live_secret") {
		t.Fatal("the PUT response echoed the API key back")
	}

	rec = sendJSONAs(t, srv, router, "root", http.MethodGet, "/api/v1/admin/email-settings", "")
	if strings.Contains(rec.Body.String(), "re_live_secret") {
		t.Fatal("GET returned the API key")
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["api_key_set"] != true || got["transport"] != "api" || got["api_provider"] != "resend" {
		t.Errorf("GET = %v, want the API transport with a key set", got)
	}

	stored, err := srv.db.GetEmailSettings()
	if err != nil {
		t.Fatal(err)
	}
	if stored.APIKey == "re_live_secret" || stored.APIKey == "" {
		t.Fatalf("stored API key = %q, want ciphertext", stored.APIKey)
	}
	plain, err := srv.encryptor.Decrypt(stored.APIKey)
	if err != nil || plain != "re_live_secret" {
		t.Fatalf("decrypt stored API key = %q, %v", plain, err)
	}

	// Same omit-to-keep rule as the password, and a pasted trailing newline is
	// not a key change.
	if rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings",
		`{"from_name":"Toft"}`); rec.Code != http.StatusOK {
		t.Fatalf("re-save = %d (%s)", rec.Code, rec.Body.String())
	}
	after, _ := srv.db.GetEmailSettings()
	if after.APIKey != stored.APIKey {
		t.Error("omitting the API key changed the stored one")
	}
	if rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings",
		`{"api_key":"  "}`); rec.Code != http.StatusOK {
		t.Fatalf("blank-key save = %d (%s)", rec.Code, rec.Body.String())
	}
	after, _ = srv.db.GetEmailSettings()
	if after.APIKey != stored.APIKey {
		t.Error("a blank API key cleared the stored one")
	}

	var before, auditAfter string
	if err := srv.db.Conn().QueryRow(
		`SELECT before, after FROM audit_log WHERE action='email_settings.update' ORDER BY occurred_at LIMIT 1`).Scan(&before, &auditAfter); err != nil {
		t.Fatal(err)
	}
	for _, blob := range []string{before, auditAfter} {
		if strings.Contains(blob, "re_live_secret") || strings.Contains(blob, stored.APIKey) {
			t.Fatal("the audit row contains the API key")
		}
	}
	flags := auditFlags(t, auditAfter)
	if flags["api_key_set"] != true || flags["api_key_changed"] != true {
		t.Errorf("audit after-blob does not record the key change: %s", auditAfter)
	}
	if flags["transport"] != "api" {
		t.Errorf("audit after-blob does not record the transport: %s", auditAfter)
	}
}

// auditFlags parses an audit before/after payload.
//
// Parsed rather than substring-matched because audit_log.before/after are
// JSONB: Postgres decides the key order and the spacing, so `"api_key_set":true`
// is not a string that appears in the column. The redaction checks above stay
// substring-based on purpose — they assert a secret is *absent*, and absence
// survives any formatting.
func auditFlags(t *testing.T, blob string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("audit payload is not JSON: %v (%s)", err, blob)
	}
	return m
}

// Each transport is validated on its own terms. The relay fields an install has
// stopped using must not block a save, and neither must an unused API key.
func TestEmailSettings_TransportValidation(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "unknown transport", body: `{"transport":"pigeon"}`, want: http.StatusBadRequest},
		{name: "unknown provider", body: `{"api_provider":"sesame"}`, want: http.StatusBadRequest},
		{
			name: "api on with no key",
			body: `{"enabled":true,"transport":"api","from_address":"a@example.com"}`,
			want: http.StatusBadRequest,
		},
		{
			// No host, no port, no security — none of it is used.
			name: "api on with a key and no relay",
			body: `{"enabled":true,"transport":"api","api_key":"re_x","from_address":"a@example.com"}`,
			want: http.StatusOK,
		},
		{
			name: "api still needs a from address",
			body: `{"enabled":true,"transport":"api","api_key":"re_x","from_address":""}`,
			want: http.StatusBadRequest,
		},
		{
			// Back to SMTP with a key still stored: the relay is now what counts.
			name: "smtp on with no host",
			body: `{"enabled":true,"transport":"smtp","host":"","from_address":"a@example.com"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "smtp on with a host",
			body: `{"enabled":true,"transport":"smtp","host":"smtp.example.com","port":587,"security":"starttls","from_address":"a@example.com"}`,
			want: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("PUT %s = %d, want %d (%s)", tc.body, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Both audiences default on, so an install that predates them keeps sending what
// it always sent; either can be switched off without turning email off.
func TestEmailSettings_NotificationSwitches(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := sendJSONAs(t, srv, router, "root", http.MethodGet, "/api/v1/admin/email-settings", "")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["notify_admin"] != true || got["notify_app"] != true {
		t.Fatalf("fresh install = %v, want both audiences on", got)
	}

	rec = sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings",
		`{"notify_app":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, err := srv.db.GetEmailSettings()
	if err != nil {
		t.Fatal(err)
	}
	if stored.NotifyApp {
		t.Error("notify_app stayed on")
	}
	if !stored.NotifyAdmin {
		t.Error("switching app notifications off also silenced administrators")
	}

	// Off is a stored false, not an absent key that would read as the default.
	var value string
	if err := srv.db.Conn().QueryRow(
		`SELECT value FROM settings WHERE key=$1`, database.SettingEmailNotifyApp).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "false" {
		t.Errorf("stored %s = %q, want \"false\"", database.SettingEmailNotifyApp, value)
	}
}

func TestEmailSettings_AuditCarriesNoPassword(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	if rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings", `{
		"enabled": true, "host": "smtp.example.com", "port": 587, "security": "starttls",
		"username": "toft", "password": "hunter2", "from_address": "toft@example.com"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}

	var n int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action='email_settings.update' AND actor_id='root'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("email_settings.update audit rows = %d, want 1", n)
	}

	var before, after string
	if err := srv.db.Conn().QueryRow(
		`SELECT before, after FROM audit_log WHERE action='email_settings.update'`).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	stored, _ := srv.db.GetEmailSettings()
	for _, blob := range []string{before, after} {
		if strings.Contains(blob, "hunter2") {
			t.Fatal("the audit row contains the SMTP password in plaintext")
		}
		if stored.Password != "" && strings.Contains(blob, stored.Password) {
			t.Fatal("the audit row contains the SMTP password ciphertext")
		}
	}
	flags := auditFlags(t, after)
	if flags["password_set"] != true || flags["password_changed"] != true {
		t.Errorf("audit after-blob does not record that a password was set and changed: %s", after)
	}
	if !strings.Contains(after, "smtp.example.com") {
		t.Errorf("audit after-blob does not record the host: %s", after)
	}
}

func TestTestEmail(t *testing.T) {
	srv, router, fake := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := sendJSONAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/email-settings/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test send = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(fake.testSent) != 1 || fake.testSent[0] != "root@example.com" {
		t.Fatalf("test email went to %v, want the caller's own address", fake.testSent)
	}
}

func TestTestEmail_NoCallerAddress(t *testing.T) {
	srv, router, fake := newMailTestServer(t)
	u := seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	u.Email = ""
	if err := srv.db.UpsertUser(u); err != nil {
		t.Fatal(err)
	}

	rec := sendJSONAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/email-settings/test", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("test send with no address = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if len(fake.testSent) != 0 {
		t.Fatalf("a test email was sent anyway: %v", fake.testSent)
	}
}

// Nothing was dialled, so this is not a relay failure and must not read as one.
func TestTestEmail_DisabledIsNotAGatewayError(t *testing.T) {
	srv, router, fake := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	fake.enabled = false

	rec := sendJSONAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/email-settings/test", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("test send with email off = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if len(fake.testSent) != 0 {
		t.Fatalf("a test email was attempted anyway: %v", fake.testSent)
	}
}

func TestTestEmail_SurfacesRelayError(t *testing.T) {
	srv, router, fake := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	fake.testErr = errRelay{}

	rec := sendJSONAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/email-settings/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failing test send = %d, want 502 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "535 authentication failed") {
		t.Fatalf("the relay's own error was not surfaced: %s", rec.Body.String())
	}
}

type errRelay struct{}

func (errRelay) Error() string { return "535 authentication failed" }

// The admin group address, through the API.
//
// It is the one field in this document that is neither a credential nor a
// switch: it is read back in the clear, it can be cleared by sending "", and it
// is checked whichever way the enabled switch is set — a string that is not an
// email address is never going to become one, and this one silently misroutes
// every administrator notification rather than failing loudly.
func TestEmailSettings_AdminGroupAddress(t *testing.T) {
	srv, router, _ := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "valid, email off", body: `{"enabled":false,"admin_group_address":"admins@corp.example"}`, want: http.StatusOK},
		{name: "invalid, email off", body: `{"enabled":false,"admin_group_address":"not an address"}`, want: http.StatusBadRequest},
		{name: "whitespace is trimmed away to empty", body: `{"admin_group_address":"   "}`, want: http.StatusOK},
		{name: "cleared", body: `{"admin_group_address":""}`, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("PUT %s = %d, want %d (%s)", tc.body, rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// Unlike the two credentials, it comes back: an administrator cannot reason
	// about where their install's alerts go if the field is blanked on read.
	if rec := sendJSONAs(t, srv, router, "root", http.MethodPut, "/api/v1/admin/email-settings",
		`{"admin_group_address":"admins@corp.example"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := sendJSONAs(t, srv, router, "root", http.MethodGet, "/api/v1/admin/email-settings", "")
	if !strings.Contains(rec.Body.String(), "admins@corp.example") {
		t.Errorf("GET did not return the admin group address: %s", rec.Body.String())
	}
}
