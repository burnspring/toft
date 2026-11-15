package mail

import (
	"strings"
	"testing"

	"toft/internal/database"
)

func TestConfigValidate(t *testing.T) {
	valid := Config{
		Host: "smtp.example.com", Port: 587, Security: SecurityStartTLS,
		FromAddress: "toft@example.com",
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid"},
		{name: "no host", mutate: func(c *Config) { c.Host = "  " }, wantErr: "host is required"},
		{name: "port zero", mutate: func(c *Config) { c.Port = 0 }, wantErr: "out of range"},
		{name: "port too high", mutate: func(c *Config) { c.Port = 70000 }, wantErr: "out of range"},
		{name: "unknown security", mutate: func(c *Config) { c.Security = "tls-ish" }, wantErr: "unknown transport security"},
		{name: "implicit tls ok", mutate: func(c *Config) { c.Security = SecurityImplicitTLS }},
		{name: "no security ok", mutate: func(c *Config) { c.Security = SecurityNone }},
		{name: "no from", mutate: func(c *Config) { c.FromAddress = "" }, wantErr: "From address is required"},
		{name: "bad from", mutate: func(c *Config) { c.FromAddress = "not-an-address" }, wantErr: "not a valid email address"},
		{name: "bad reply-to", mutate: func(c *Config) { c.ReplyTo = "@@" }, wantErr: "not a valid email address"},
		{name: "good reply-to", mutate: func(c *Config) { c.ReplyTo = "ops@example.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestRenderTemplates(t *testing.T) {
	data := TemplateData{
		BaseURL:        "https://toft.internal",
		RequesterName:  "Ada Lovelace",
		RequesterLogin: "ada",
		RequesterEmail: "ada@example.com",
		ApproverName:   "root",
		Reason:         "Ask again after the security training.",
		Link:           "https://toft.internal/admin/publisher-requests?request=abc",
		AppName:        "Quarterly Report",
		EventTitle:     "The access token for example/reports is no longer accepted",
		EventDetail:    "remote: Invalid username or password.\nfatal: Authentication failed",
		EventWhen:      "10 Aug 2026 at 04:51 UTC",
		Severity:       "error",
	}

	for _, name := range []string{"promotion_requested", "promotion_approved", "promotion_denied", "test_email", "app_event", "app_event_recovered", "app_shared"} {
		t.Run(name, func(t *testing.T) {
			msg, err := Render(name, data)
			if err != nil {
				t.Fatalf("Render(%q): %v", name, err)
			}
			if strings.TrimSpace(msg.Subject) == "" {
				t.Error("subject is empty")
			}
			if strings.Contains(msg.HTML, "{{") || strings.Contains(msg.Text, "{{") {
				t.Error("template markers survived rendering")
			}
			// Every message has to tell the reader where it came from, or it
			// reads as phishing.
			if !strings.Contains(msg.HTML, data.BaseURL) {
				t.Errorf("HTML body does not mention the base URL")
			}
			if strings.TrimSpace(msg.Text) == "" {
				t.Error("text body is empty — a client that cannot render HTML would see nothing")
			}
		})
	}
}

func TestRenderRequestedNamesTheRequester(t *testing.T) {
	msg, err := Render("promotion_requested", TemplateData{
		BaseURL: "https://toft.internal", RequesterName: "Ada Lovelace",
		RequesterLogin: "ada", RequesterEmail: "ada@example.com",
		Link: "https://toft.internal/admin/publisher-requests?request=abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Ada Lovelace", "ada", "ada@example.com"} {
		if !strings.Contains(msg.HTML, want) {
			t.Errorf("HTML body missing %q", want)
		}
		if !strings.Contains(msg.Text, want) {
			t.Errorf("text body missing %q", want)
		}
	}
	if !strings.Contains(msg.Subject, "Ada Lovelace") {
		t.Errorf("subject %q does not name the requester", msg.Subject)
	}
	if !strings.Contains(msg.HTML, "publisher-requests?request=abc") {
		t.Error("HTML body does not link to the request")
	}
}

// A display name is provider-supplied and a deny reason is admin-typed. Neither
// is trusted markup, and an HTML email is exactly where that matters.
func TestRenderEscapesUserSuppliedText(t *testing.T) {
	msg, err := Render("promotion_denied", TemplateData{
		BaseURL:       "https://toft.internal",
		RequesterName: `<script>alert(1)</script>`,
		Reason:        `no & <b>absolutely not</b>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.HTML, "<script>") {
		t.Error("a script tag survived into the HTML body")
	}
	if strings.Contains(msg.HTML, "<b>absolutely not</b>") {
		t.Error("admin-typed markup was rendered as markup rather than text")
	}
	if !strings.Contains(msg.HTML, "&amp;") {
		t.Error("ampersand was not escaped")
	}
	// The text body is not markup, so it carries the reason verbatim.
	if !strings.Contains(msg.Text, `no & <b>absolutely not</b>`) {
		t.Error("text body should carry the reason as typed")
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	if _, err := Render("no_such_template", TemplateData{}); err == nil {
		t.Fatal("Render of an unknown template succeeded; it must not send a blank email")
	}
}

func TestRenderDeniedWithoutReason(t *testing.T) {
	msg, err := Render("promotion_denied", TemplateData{BaseURL: "https://toft.internal"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.Text, "Reason:") {
		t.Error("a denial with no reason should not print an empty Reason block")
	}
	if !strings.Contains(msg.Text, "An administrator declined") {
		t.Error("a denial with no named approver should still say who declined, generically")
	}
}

// The audience switches are enforced in one place, so this is where the mapping
// is pinned. The empty audience is the one that matters most: a notification
// written without declaring who it is for must not slip past both switches.
func TestAudienceEnabled(t *testing.T) {
	both := database.EmailSettings{NotifyAdmin: true, NotifyApp: true}
	for _, tc := range []struct {
		name     string
		settings database.EmailSettings
		audience string
		want     bool
	}{
		{name: "admin on", settings: both, audience: AudienceAdmin, want: true},
		{name: "app on", settings: both, audience: AudienceApp, want: true},
		{name: "admin off", settings: database.EmailSettings{NotifyApp: true}, audience: AudienceAdmin},
		{name: "app off", settings: database.EmailSettings{NotifyAdmin: true}, audience: AudienceApp},
		{name: "app off does not silence admin", settings: database.EmailSettings{NotifyAdmin: true}, audience: AudienceAdmin, want: true},
		{name: "no audience is refused", settings: both, audience: ""},
		{name: "unknown audience is refused", settings: both, audience: "marketing"},
		{name: "test is not a notification", settings: both, audience: AudienceTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := audienceEnabled(tc.settings, tc.audience); got != tc.want {
				t.Fatalf("audienceEnabled(%q) = %v, want %v", tc.audience, got, tc.want)
			}
		})
	}
}

// Every template declares its audience, and Render carries it onto the Message —
// the send site never sets it, so a missing one here is a message that would be
// silently dropped forever.
func TestRenderSetsAudience(t *testing.T) {
	want := map[string]string{
		"promotion_requested": AudienceAdmin,
		"promotion_approved":  AudienceApp,
		"promotion_denied":    AudienceApp,
		"test_email":          AudienceTest,
		// Failure mail belongs to the app's people, so an administrator who has
		// switched the "app" audience off has switched these off with it.
		"app_event":           AudienceApp,
		"app_event_recovered": AudienceApp,
		"app_shared":          AudienceApp,
		// The platform locking an app for a dependency finding. AudienceApp
		// like the two above it: the owner is who has to act, and an
		// administrator's own record of it is the audit row and Admin →
		// Dependencies, which are immediate and are not mail.
		"app_locked_dependency": AudienceApp,
		// Where this install may reach for code widening is an install-level
		// fact with no app behind it, and it is addressed to the people who
		// would care that it happened without them. An install with the admin
		// audience off has switched this off too, which is correct: it
		// withholds a notice, and this was never a control.
		"source_reach_widened": AudienceAdmin,
		// Somebody is waiting in the activation queue. AudienceAdmin: it asks
		// an administrator to do something, and an install that has silenced
		// admin mail has silenced the nudge and not the queue.
		"account_pending_activation": AudienceAdmin,
		// And the answer, to the person. AudienceApp like every other
		// person-facing message here — it is about their standing, not about
		// the install.
		"account_activated": AudienceApp,
		// Somebody has asked for access to one app. AudienceApp rather than
		// AudienceAdmin: it is addressed to that app's owners, and an install
		// that silenced admin mail did not ask to stop app owners hearing
		// about their own apps.
		"access_requested": AudienceApp,
		// An administrator moved one app's address; its owners are told.
		"app_hostname_changed": AudienceApp,
		// And its two answers, to the person who asked.
		"access_approved": AudienceApp,
		"access_denied":   AudienceApp,
		// The administrator's own announcement, and the one audience no switch
		// silences. Its enforcement point is the Send button rather than a
		// setting — see audienceEnabled, where the reason is written down.
		"announcement": AudienceAnnouncement,
		// The second send whose arrival is the point, and the second whose
		// enforcement point is not a setting: a report exists only where
		// somebody wrote a cron and named people.
		"report_rendered": AudienceReport,
	}
	for name := range templates {
		msg, err := Render(name, TemplateData{BaseURL: "https://toft.internal"})
		if err != nil {
			t.Fatalf("Render(%q): %v", name, err)
		}
		if msg.Audience == "" {
			t.Errorf("template %q declares no audience, so nothing it renders would ever be sent", name)
		}
		if w, ok := want[name]; ok && msg.Audience != w {
			t.Errorf("template %q audience = %q, want %q", name, msg.Audience, w)
		} else if !ok {
			t.Errorf("template %q is new — add its audience to this test", name)
		}
	}
}

// Where a message is delivered, as opposed to whether.
//
// The rule lives on the send path so no call site can escape it, which means
// this is the only place the property can be pinned: that admin mail is
// re-addressed, that app mail is not, and that "silence this audience" is not
// something a group address can override.
func TestAddressed(t *testing.T) {
	const group = "toft-admins@corp.example"
	withGroup := database.EmailSettings{AdminGroupAddress: group}
	without := database.EmailSettings{}

	for _, tc := range []struct {
		name     string
		settings database.EmailSettings
		msg      Message
		want     []string
	}{
		{
			name:     "admin mail goes to the group instead of the administrators",
			settings: withGroup,
			msg:      Message{Audience: AudienceAdmin, To: []string{"a@x", "b@x"}},
			want:     []string{group},
		},
		{
			// The case the individual addresses cannot cover: no administrator
			// has an address on file, so before the group address this message
			// had nowhere to go.
			name:     "admin mail with no recipients still reaches the group",
			settings: withGroup,
			msg:      Message{Audience: AudienceAdmin},
			want:     []string{group},
		},
		{
			name:     "no group address leaves the administrators alone",
			settings: without,
			msg:      Message{Audience: AudienceAdmin, To: []string{"a@x", "b@x"}},
			want:     []string{"a@x", "b@x"},
		},
		{
			// An admin alias is not one of an app's people. Copying app mail
			// there would turn a setting about administrator noise into an
			// install-wide interception of everyone's mail about their own apps.
			name:     "app mail is never redirected",
			settings: withGroup,
			msg:      Message{Audience: AudienceApp, To: []string{"owner@x"}},
			want:     []string{"owner@x"},
		},
		{
			name:     "test sends are never redirected",
			settings: withGroup,
			msg:      Message{Audience: AudienceTest, To: []string{"admin@x"}},
			want:     []string{"admin@x"},
		},
		{
			// See TestAnnouncementNeverUsesTheAdminGroupAddress below for why
			// this one is worth its own name as well as a row here.
			name:     "an announcement is never redirected",
			settings: withGroup,
			msg:      Message{Audience: AudienceAnnouncement, To: []string{"ada@x"}},
			want:     []string{"ada@x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := addressed(tc.settings, tc.msg)
			if len(got) != len(tc.want) {
				t.Fatalf("addressed = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("addressed = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A group address changes where administrator mail goes, never whether it is
// sent. An administrator who switched the audience off and *then* named a group
// must stay silenced — otherwise "stop filling my inbox" would have a setting
// that quietly undoes it.
func TestAdminGroupAddressDoesNotDefeatTheAudienceSwitch(t *testing.T) {
	settings := database.EmailSettings{
		NotifyAdmin:       false,
		NotifyApp:         true,
		AdminGroupAddress: "toft-admins@corp.example",
	}
	if audienceEnabled(settings, AudienceAdmin) {
		t.Error("administrator mail is enabled despite the audience being switched off")
	}
}

// The group-address rule is scoped to AudienceAdmin, and nothing stops somebody
// widening that. If it were widened, an announcement to two hundred publishers
// would deliver to one distribution list and the frozen row would still read
// *delivered=200* — a record that is wrong in the one direction nobody could
// reconstruct, because every send returned success.
//
// So this asserts the property by name rather than leaving it as a thing that
// happens to hold: an announcement addresses the person it names, whatever the
// administrator has set the group address to.
func TestAnnouncementNeverUsesTheAdminGroupAddress(t *testing.T) {
	settings := database.EmailSettings{
		Enabled:           true,
		NotifyAdmin:       true,
		NotifyApp:         true,
		AdminGroupAddress: "toft-admins@corp.example",
	}
	recipients := []string{"ada@x", "bob@x", "cleo@x"}
	seen := map[string]bool{}
	for _, to := range recipients {
		// One mail.Message per recipient, never one message with N addresses:
		// a To: carrying the whole directory is a disclosure nobody declared,
		// and a BCC: carrying it is the same header one hop away.
		got := addressed(settings, Message{Audience: AudienceAnnouncement, To: []string{to}})
		if len(got) != 1 || got[0] != to {
			t.Fatalf("an announcement to %s was addressed to %v", to, got)
		}
		seen[got[0]] = true
	}
	if len(seen) != len(recipients) {
		t.Fatalf("%d distinct recipients for %d people; the group address collapsed them",
			len(seen), len(recipients))
	}
}

// The body is an administrator's free text and the platform owns the frame.
// Escaped in the HTML part, literal in the text part, and its line breaks
// survive without any markup being generated from what somebody typed.
func TestBodyIsEscaped(t *testing.T) {
	const body = "Line one.\n<script>alert(1)</script>\nLine three."
	msg, err := Render("announcement", TemplateData{
		BaseURL:             "https://toft.internal",
		AnnouncementSubject: "The build queue is down",
		AnnouncementBody:    body,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if msg.Subject != "The build queue is down" {
		t.Errorf("subject = %q", msg.Subject)
	}
	if strings.Contains(msg.HTML, "<script>") {
		t.Error("the HTML part carries an unescaped script tag from the composer")
	}
	if !strings.Contains(msg.HTML, "&lt;script&gt;") {
		t.Error("the HTML part did not escape the body")
	}
	if !strings.Contains(msg.HTML, "white-space:pre-wrap") {
		t.Error("the HTML part does not preserve line breaks, so a typed paragraph arrives as one line")
	}
	if !strings.Contains(msg.Text, "<script>alert(1)</script>") {
		t.Error("the text part escaped the body, where escaping is wrong")
	}
	if strings.Contains(msg.Text, "&lt;") {
		t.Error("the text part carries HTML entities")
	}
}
