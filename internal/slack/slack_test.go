package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"toft/internal/integrations"
	"toft/internal/notify"
)

// testConfig points a Config at a test server. The field is unexported
// precisely so nothing outside this package can do it, which is why the helper
// lives here.
func testConfig(base string) Config {
	return Config{Token: "xoxb-test", DefaultChannel: "C123", baseURL: base + "/"}
}

// The single most important property of this transport: Slack refuses with
// HTTP 200 and "ok": false. A transport that trusted the status code would
// report every rejected token and every kicked-out bot as a delivered message.
func TestRefusalWithHTTP200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":false,"error":"not_in_channel"}`)
	}))
	defer srv.Close()

	err := Post(context.Background(), testConfig(srv.URL), Message{Channel: "C123", Text: "hi"})
	if err == nil {
		t.Fatal("a refusal returned as HTTP 200 was treated as success")
	}
	if !strings.Contains(err.Error(), "/invite") {
		t.Errorf("error should tell an administrator what to do, got %q", err)
	}
}

func TestSuccessfulPostSendsChannelAndBlocks(t *testing.T) {
	var got struct {
		Channel string          `json:"channel"`
		Text    string          `json:"text"`
		Blocks  json.RawMessage `json:"blocks"`
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	msg, err := Render("app_event", "", notify.TemplateData{
		AppName: "Payroll", EventTitle: "Deploy failed", Link: "https://example/apps/payroll",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg.Channel = "C123"
	if err := Post(context.Background(), testConfig(srv.URL), msg); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer xoxb-test" {
		t.Errorf("token should travel in the Authorization header, got %q", auth)
	}
	if got.Channel != "C123" {
		t.Errorf("channel = %q", got.Channel)
	}
	if !strings.Contains(got.Text, "Payroll") {
		t.Errorf("the fallback text has to stand alone in a notification, got %q", got.Text)
	}
	if len(got.Blocks) == 0 {
		t.Error("blocks were not sent")
	}
}

// Every string these templates interpolate is user-supplied. An unescaped
// <https://evil|click me> in an app name or a build log is a link Toft
// appears to have written.
func TestUserSuppliedTextCannotInjectMrkdwnLinks(t *testing.T) {
	msg, err := Render("app_event", "", notify.TemplateData{
		AppName:     "<https://evil.example|Click here>",
		EventTitle:  "build & test <broken>",
		EventDetail: "err: <https://evil.example|totally fine>",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Asserted on the block text rather than the marshalled JSON: encoding/json
	// escapes & as \u0026, which would make a broken escape look fine.
	var all strings.Builder
	for _, b := range msg.Blocks {
		if b.Text != nil {
			all.WriteString(b.Text.Text)
		}
		for _, e := range b.Elements {
			all.WriteString(e.Text)
		}
	}
	text := all.String()
	if strings.Contains(text, "<https://evil.example|") {
		t.Fatalf("a link survived escaping: %s", text)
	}
	if !strings.Contains(text, "&lt;https://evil.example") {
		t.Errorf("expected the angle bracket escaped, got %s", text)
	}
	if !strings.Contains(text, "build &amp; test") {
		t.Errorf("expected the ampersand escaped, got %s", text)
	}

	// The fallback text is parsed as mrkdwn too, and is what a notification
	// shows (RT-3).
	msg, err = Render("app_event", "", notify.TemplateData{AppName: "<!channel>", EventTitle: "crashed"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.Text, "<!") || !strings.Contains(msg.Text, "&lt;!channel&gt;") {
		t.Errorf("fallback text = %q, want the mention escaped", msg.Text)
	}
}

// A fenced block is not a safe harbour: backticks in a log line would end the
// fence and the rest of the message would render as markup.
func TestBacktickInDetailCannotBreakOutOfTheCodeBlock(t *testing.T) {
	out := codeBlock("npm ERR! ```\n*not bold*")
	if strings.Count(out, "```") != 2 {
		t.Fatalf("fence broken: %q", out)
	}
}

func TestDetailIsTrimmedToTheSlackLimit(t *testing.T) {
	out := codeBlock(strings.Repeat("x", SlackDetailLimit*3))
	if len(out) > SlackDetailLimit+64 {
		t.Errorf("excerpt not trimmed: %d bytes", len(out))
	}
	if !strings.Contains(out, "earlier output trimmed") {
		t.Error("a trimmed excerpt must say so, or it reads as the whole story")
	}
}

// The two promotion outcomes are email-only, and Render refuses them here as
// well as in the declaration that says so.
//
// The mechanism moved: it used to be that this package defined no template for
// them, which was enforcement by absence. It is now a `channels` field in
// internal/notify, which is weaker, and `TestPersonFacingNotificationsAreMailOnly`
// is what asserts the rule. This is the second half of it — that the weaker
// mechanism actually stops the post at the point of rendering.
func TestPromotionOutcomesHaveNoSlackRendering(t *testing.T) {
	for _, name := range []string{"promotion_approved", "promotion_denied"} {
		if _, err := Render(name, "", notify.TemplateData{}); err == nil {
			t.Errorf("%s must not render — it would publish a private decision", name)
		}
	}
	// The one that *is* posted, for contrast.
	if _, err := Render("promotion_requested", "", notify.TemplateData{}); err != nil {
		t.Error("promotion_requested should post: it is a queue an administrator acts on")
	}
}

// Audience is declared on the template and never by a caller, which is what
// makes it impossible to add a notification that escapes the switches.
func TestAudienceComesFromTheTemplate(t *testing.T) {
	for name, want := range map[string]string{
		"app_event":           notify.AudienceApp,
		"app_event_recovered": notify.AudienceApp,
		"promotion_requested": notify.AudienceAdmin,
		"test_post":           notify.AudienceTest,
	} {
		msg, err := Render(name, "", notify.TemplateData{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if msg.Audience != want {
			t.Errorf("%s audience = %q, want %q", name, msg.Audience, want)
		}
	}
}

func TestAudienceGate(t *testing.T) {
	cases := []struct {
		name                   string
		notifyAdmin, notifyApp bool
		audience               string
		want                   bool
	}{
		{"admin on", true, true, notify.AudienceAdmin, true},
		{"admin off", false, true, notify.AudienceAdmin, false},
		{"app on", true, true, notify.AudienceApp, true},
		{"app off", true, false, notify.AudienceApp, false},
		// A test message has no switch of its own; PostTest bypasses this gate
		// deliberately, and anything arriving here is not sent.
		{"test", true, true, notify.AudienceTest, false},
		// The one that matters: a message with no audience is refused rather
		// than waved through, so omission cannot become an escape.
		{"none declared", true, true, "", false},
		{"unknown", true, true, "everyone", false},
	}
	for _, c := range cases {
		if got := audienceEnabled(c.notifyAdmin, c.notifyApp, c.audience); got != c.want {
			t.Errorf("%s: audienceEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestValidateRejectsThePastesPeopleActuallyMake(t *testing.T) {
	for _, c := range []struct{ name, token, wantIn string }{
		{"user token", "xoxp-1234", "xoxb-"},
		{"trailing newline", "xoxb-1234\n", "line break"},
		{"blank", "", "required"},
	} {
		err := Config{Token: c.token, DefaultChannel: "C1"}.Validate()
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantIn) {
			t.Errorf("%s: error %q should mention %q", c.name, err, c.wantIn)
		}
	}
	if err := (Config{Token: "xoxb-ok", DefaultChannel: ""}).Validate(); err == nil {
		t.Error("a config with no default channel has nowhere to post")
	}
}

// Channels is the channel menu *and* the validator, and it must page: a
// workspace larger than one page would otherwise silently offer a subset.
func TestChannelsPagesAndExcludesDMs(t *testing.T) {
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// users.conversations takes form arguments only; a JSON body would be
		// ignored and Slack would answer with its defaults.
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q, want form-urlencoded", ct)
		}
		r.ParseForm()
		forms = append(forms, r.PostForm)
		if len(forms) == 1 {
			io.WriteString(w, `{"ok":true,"channels":[{"id":"C1","name":"admin"}],
				"response_metadata":{"next_cursor":"more"}}`)
			return
		}
		// The second page repeats C1, as a far side ignoring the cursor would.
		io.WriteString(w, `{"ok":true,"channels":[{"id":"C1","name":"admin"},{"id":"C2","name":"team","is_private":true}]}`)
	}))
	defer srv.Close()

	got, err := Channels(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "C1" || got[1].ID != "C2" {
		t.Fatalf("expected C1 and C2 once each, got %+v", got)
	}
	if len(forms) != 2 {
		t.Fatalf("expected two requests, got %d", len(forms))
	}
	first := forms[0]
	if first.Get("types") != "public_channel,private_channel" {
		t.Errorf("types = %q, want both channel types and no DMs", first.Get("types"))
	}
	if first.Get("exclude_archived") != "true" {
		t.Errorf("exclude_archived = %q", first.Get("exclude_archived"))
	}
	if first.Get("limit") == "" || first.Get("cursor") != "" {
		t.Errorf("first page: limit %q cursor %q", first.Get("limit"), first.Get("cursor"))
	}
	if forms[1].Get("cursor") != "more" {
		t.Errorf("second request cursor = %q, want more", forms[1].Get("cursor"))
	}
}

// A bot with channels:read but not groups:read keeps its public channels.
func TestChannelsFallsBackToPublicOnMissingScope(t *testing.T) {
	var types []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		types = append(types, r.PostForm.Get("types"))
		if len(types) == 1 {
			io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"channels":[{"id":"C1","name":"general"}]}`)
	}))
	defer srv.Close()

	got, err := Channels(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(types) != 2 || types[1] != "public_channel" {
		t.Fatalf("got %+v after asking for %q", got, types)
	}
}

// A refusal keeps the administrator-facing sentence, not Slack's slug.
func TestChannelsRefusalIsASentence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer srv.Close()
	_, err := Channels(context.Background(), testConfig(srv.URL))
	if err == nil || !strings.Contains(err.Error(), "re-install") {
		t.Errorf("err = %v, want the bot-token sentence", err)
	}
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	_, err = Channels(context.Background(), testConfig(limited.URL))
	if err == nil || !strings.Contains(err.Error(), "rate-limiting") {
		t.Errorf("err = %v, want the rate-limit sentence", err)
	}
}

func TestRateLimitIsAFailureNotARetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	err := Post(context.Background(), testConfig(srv.URL), Message{Channel: "C1", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Errorf("a 429 should be reported as a dropped message, got %v", err)
	}
}

// With the install named, no platform post says "Toft" — the fallback, the
// blocks and the channel test — and every one closes with the origin.
func TestNamedInstallPostsUnderItsName(t *testing.T) {
	const name = "Zebra Works Platform"
	d := notify.TemplateData{
		BaseURL: "https://apps.example.com", AppName: "Payroll", EventTitle: "Deploy failed",
		EventDetail: "exit status 1", Severity: "error", Link: "https://apps.example.com/apps/payroll",
		RequesterName: "Ada", RequesterLogin: "ada",
	}
	for tmplName, tmpl := range notify.Templates {
		if tmpl.Render == nil || !tmpl.Carries(notify.ChannelPoster) {
			continue
		}
		msg, err := Render(tmplName, name, d)
		if err != nil {
			t.Fatalf("%s: %v", tmplName, err)
		}
		raw, _ := json.Marshal(msg)
		if strings.Contains(string(raw), "Toft") {
			t.Errorf("%s says Toft with the install named: %s", tmplName, raw)
		}
		last := msg.Blocks[len(msg.Blocks)-1]
		if last.Type != "context" || !strings.HasSuffix(last.Elements[0].Text, "Sent by "+name+" at https://apps.example.com") {
			t.Errorf("%s does not close with the origin: %+v", tmplName, last)
		}
	}
	for _, line := range []string{
		appContext("Payroll", "", name).Elements[0].Text,
		appComment("", "Payroll", "https://apps.example.com/apps/payroll", name),
	} {
		if strings.Contains(line, "Toft") || !strings.HasSuffix(line, "an application on "+name) {
			t.Errorf("app attribution %q", line)
		}
	}
	if got := appContext("Payroll", "", "").Elements[0].Text; got != "Sent by *Payroll*, an application on Toft" {
		t.Errorf("unnamed app attribution changed: %q", got)
	}
}

// Listing channels and posting to a chosen one need only the token: a
// connection created without a default channel — the create dialog sends none —
// must still be able to offer the menu that picks one.
func TestConfigForNeedsNoDefaultChannel(t *testing.T) {
	target := func(token string) integrations.Target {
		return integrations.Target{Credentials: map[string]string{"bot_token": token}}
	}
	if _, err := configFor(target("xoxb-ok")); err != nil {
		t.Errorf("a token and no default channel was refused: %v", err)
	}
	for _, bad := range []string{"", "xoxp-user", "xoxb-ok\n"} {
		if _, err := configFor(target(bad)); err == nil {
			t.Errorf("token %q was accepted", bad)
		}
	}
}

func TestPostToAChosenChannelNeedsNoDefault(t *testing.T) {
	var channel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		channel, _ = body["channel"].(string)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.DefaultChannel = ""
	if err := Post(context.Background(), cfg, Message{Channel: "C9", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if channel != "C9" {
		t.Errorf("posted to %q, want C9", channel)
	}
}

// Slack's upload ticket is in the path of the URL, and a failed transfer must
// not repeat it.
func TestAFailedFileUploadDoesNotRepeatItsURL(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	dead := closed.URL
	closed.Close()
	err := putFile(context.Background(), dead+"/upload/v1/TICKETSECRET", "a.csv", []byte("x"))
	if err == nil {
		t.Fatal("an upload to a closed port succeeded")
	}
	if strings.Contains(err.Error(), "TICKETSECRET") {
		t.Errorf("the error repeats the upload URL: %v", err)
	}
}

// VN-6 (2026-11-15): every text object is verbatim, so a bare `@channel` in a
// requester's display name is not parsed into a channel-wide mention.
func TestEveryTextObjectIsVerbatim(t *testing.T) {
	msg, err := Render("promotion_requested", "", notify.TemplateData{
		RequesterName: "@channel", RequesterLogin: "@here", Link: "https://toft.example/admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Blocks) == 0 {
		t.Fatal("no blocks rendered")
	}
	for _, b := range msg.Blocks {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		texts := strings.Count(string(raw), `"type":"mrkdwn"`)
		if verb := strings.Count(string(raw), `"verbatim":true`); texts == 0 || verb != texts {
			t.Errorf("block %s: %d mrkdwn text objects, %d verbatim", raw, texts, verb)
		}
	}
}

// VN-7 (2026-11-15): no section passes Slack's 3000-character limit, which
// makes Slack refuse the whole post — not a long title, and not an excerpt
// that escaping grew past it.
func TestEverySectionIsWithinTheSlackSectionLimit(t *testing.T) {
	msg, err := Render("app_event", "", notify.TemplateData{
		AppName:     "a",
		EventTitle:  strings.Repeat("&", 5000),
		EventDetail: strings.Repeat("<", 1500),
		Link:        "https://toft.example/apps/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	sawFence := false
	for _, b := range msg.Blocks {
		if b.Text == nil {
			continue
		}
		text := b.Text.Text
		if len(text) > 3000 {
			t.Errorf("a section is %d bytes, over Slack's limit", len(text))
		}
		body := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(text, "*"), "…"), "\n```")
		if amp := strings.LastIndexByte(body, '&'); amp >= 0 && !strings.Contains(body[amp:], ";") {
			t.Errorf("a section ends inside an entity: …%q", body[max(0, len(body)-12):])
		}
		if strings.HasPrefix(text, "```") {
			sawFence = true
			if !strings.HasSuffix(text, "\n```") {
				t.Error("the code block lost its closing fence")
			}
		}
	}
	if !sawFence {
		t.Error("no code block rendered for the detail")
	}

	// A single line too long once escaped still fits.
	if out := codeBlock(strings.Repeat("&", 1500)); len(out) > 3000 || !strings.HasSuffix(out, "\n```") {
		t.Errorf("a one-line excerpt is %d bytes or lost its fence", len(out))
	}
}
