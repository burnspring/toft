package mail

import (
	"strings"
	"testing"

	"toft/internal/database"
)

// The one rule these tests exist for: **an app never speaks in the platform's
// voice.** Everything below is a way that rule could be broken.

func TestTheFrameIsPresentAndNamesTheApp(t *testing.T) {
	msg := BuildAppMessage("", "Weekly Reporter", "https://toft.example/apps/reporter", "",
		"Weekly reconciliation", "3 breaks this week.", "<p>3 breaks</p>",
		[]string{"finance@example.com"}, nil, nil, nil)

	if !strings.Contains(msg.FromName, "Weekly Reporter") {
		t.Errorf("FromName = %q, does not name the app", msg.FromName)
	}
	if !strings.HasSuffix(msg.FromName, AppDisplaySuffix("")) {
		t.Errorf("FromName = %q, does not carry %q", msg.FromName, AppDisplaySuffix(""))
	}
	// The plain-text half is the one no styling can paint over, which is why
	// text is required even when HTML is supplied.
	if !strings.Contains(msg.Text, "not by Toft itself") {
		t.Errorf("the text body does not disclaim the platform:\n%s", msg.Text)
	}
	if !strings.Contains(msg.HTML, "not by Toft itself") {
		t.Errorf("the html body does not disclaim the platform:\n%s", msg.HTML)
	}
	if !strings.Contains(msg.Text, "3 breaks this week.") {
		t.Error("the app's own text did not survive framing")
	}
	if msg.Audience != AudienceAppOutbound {
		t.Errorf("audience = %q, want %q", msg.Audience, AudienceAppOutbound)
	}
}

// An app named with a line break is not an address-header injection.
func TestAHostileAppNameCannotBreakTheHeader(t *testing.T) {
	msg := BuildAppMessage("", "Evil\r\nBcc: victim@example.com", "", "",
		"s", "t", "", []string{"a@example.com"}, nil, nil, nil)

	if strings.ContainsAny(msg.FromName, "\r\n") {
		t.Fatalf("FromName carries a line break: %q", msg.FromName)
	}
	if strings.Contains(strings.ToLower(msg.FromName), "bcc:") &&
		strings.Contains(msg.FromName, "\n") {
		t.Errorf("FromName could inject a header: %q", msg.FromName)
	}
}

func TestABlankAppNameNeverReadsAsThePlatform(t *testing.T) {
	if got := SanitizeDisplayName("   "); got == "" {
		t.Error("a blank app name produced a blank display name, which reads as Toft itself")
	}
}

func TestALongAppNameIsBounded(t *testing.T) {
	if got := SanitizeDisplayName(strings.Repeat("a", 500)); len(got) > appNameLimit {
		t.Errorf("display name is %d characters, limit is %d", len(got), appNameLimit)
	}
}

func TestASubjectCannotCarryALineBreak(t *testing.T) {
	if got := SanitizeSubject("Report\r\nBcc: victim@example.com"); strings.ContainsAny(got, "\r\n") {
		t.Errorf("subject carries a line break: %q", got)
	}
}

func TestRecipientsAreReRenderedFromTheParse(t *testing.T) {
	got, err := ValidateRecipient("Finance Team <finance@example.com>")
	if err != nil {
		t.Fatalf("ValidateRecipient: %v", err)
	}
	// Re-rendered rather than passed through, which is what neutralizes
	// anything hiding in a display name.
	if got != "finance@example.com" {
		t.Errorf("address = %q, want the bare address", got)
	}
	for _, bad := range []string{"not-an-address", "a@example.com\r\nBcc: b@example.com", ""} {
		if _, err := ValidateRecipient(bad); err == nil {
			t.Errorf("ValidateRecipient(%q) was accepted", bad)
		}
	}
}

// ------------------------------------------------------------- sanitization

func TestSanitizerRemovesWhatCanPaintOverTheFrame(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		absent  []string
		present []string
	}{
		{"script", `<p>hi</p><script>alert(1)</script>`,
			[]string{"<script", "alert(1)"}, []string{"<p>hi</p>"}},
		{"style block", `<style>body{display:none}</style><p>hi</p>`,
			[]string{"<style", "display:none"}, []string{"<p>hi</p>"}},
		{"event handler", `<div onclick="steal()">hi</div>`,
			[]string{"onclick", "steal()"}, []string{"hi"}},
		{"iframe", `<iframe src="https://evil.example"></iframe><p>hi</p>`,
			[]string{"<iframe", "evil.example"}, []string{"<p>hi</p>"}},
		{"form", `<form action="https://evil.example"><input name="pw"></form>`,
			[]string{"<form", "<input"}, nil},
		{"javascript href", `<a href="javascript:alert(1)">click</a>`,
			[]string{"javascript:"}, []string{"click"}},
		{"positioning", `<div style="position:absolute;top:-100px;color:red">hi</div>`,
			[]string{"position", "absolute"}, []string{"color:red"}},
		{"negative margin in a later token", `<div style="margin:0 0 -400px 0;color:red">hi</div>`,
			[]string{"margin", "-400px"}, []string{"color:red"}},
		{"negative offset inside calc()", `<div style="margin-top:calc(0px - 400px)">hi</div>`,
			[]string{"calc("}, []string{"hi"}},
		{"calc() with a parenthesised operand", `<div style="margin-bottom:calc(0px - (400px))">hi</div>`,
			[]string{"calc(", "margin-bottom"}, []string{"hi"}},
		{"calc() with a nested max()", `<div style="margin-top:calc(0px - max(400px))">hi</div>`,
			[]string{"calc(", "max("}, []string{"hi"}},
		{"escaped minus", `<div style="margin:0 0 \2d 400px">hi</div>`,
			[]string{"margin"}, []string{"hi"}},
		{"css url()", `<div style="background:url(https://evil.example/px.gif)">hi</div>`,
			[]string{"url("}, []string{"hi"}},
		{"http image", `<img src="http://evil.example/px.gif">`,
			[]string{"evil.example"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeHTML(tc.in)
			for _, s := range tc.absent {
				if strings.Contains(got, s) {
					t.Errorf("output still contains %q:\n%s", s, got)
				}
			}
			for _, s := range tc.present {
				if !strings.Contains(got, s) {
					t.Errorf("output lost %q:\n%s", s, got)
				}
			}
		})
	}
}

// Sanitizing must not make HTML mail useless: reports are tables with a little
// colour, and stripping all of that would leave nothing worth sending.
func TestSanitizerKeepsWhatAReportNeeds(t *testing.T) {
	in := `<table><tr><th colspan="2">Breaks</th></tr>` +
		`<tr><td style="color:#b91c1c">ACME</td><td>3</td></tr></table>` +
		`<p style="color:rgb(10,20,30)">rgb</p>` +
		`<p><strong>1,204</strong> rows · <a href="https://example.com/report">detail</a></p>`
	got := SanitizeHTML(in)

	for _, want := range []string{
		"<table>", "<th colspan=\"2\">", "color:#b91c1c", "color:rgb(10,20,30)",
		"<strong>1,204</strong>", `href="https://example.com/report"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lost %q:\n%s", want, got)
		}
	}
}

// A stray closing tag must not be able to close the frame's own wrapper and
// escape into the attribution block.
func TestAStrayCloseTagCannotEscapeTheWrapper(t *testing.T) {
	got := SanitizeHTML(`</div></div><p>hi</p>`)
	if strings.Contains(got, "</div>") {
		t.Errorf("a close tag the sanitizer never opened survived:\n%s", got)
	}
}

func TestUnclosedTagsAreClosed(t *testing.T) {
	got := SanitizeHTML(`<div><p>hi`)
	if strings.Count(got, "</p>") != 1 || strings.Count(got, "</div>") != 1 {
		t.Errorf("unbalanced fragment was not closed:\n%s", got)
	}
}

func TestTextIsEscapedRatherThanPassedThrough(t *testing.T) {
	got := SanitizeHTML(`5 < 6 & 7 > 6`)
	if strings.Contains(got, "<") && !strings.Contains(got, "&lt;") {
		t.Errorf("raw text was not escaped:\n%s", got)
	}
}

// Empty in, empty out: an app that sent no HTML gets a plain-text message
// rather than a frame wrapped around nothing.
func TestNoHTMLMeansNoHTMLPart(t *testing.T) {
	msg := BuildAppMessage("", "App", "", "", "s", "t", "", []string{"a@example.com"}, nil, nil, nil)
	if msg.HTML != "" {
		t.Errorf("HTML = %q, want empty", msg.HTML)
	}
}

func TestAudienceAppOutboundIsNotGovernedByNotifyApp(t *testing.T) {
	// The install has silenced platform chatter to app authors entirely.
	settings := database.EmailSettings{NotifyAdmin: false, NotifyApp: false}
	if !audienceEnabled(settings, AudienceAppOutbound) {
		t.Error("an app's own message was dropped by the platform-chatter switches")
	}
	// ...and the platform's own app notifications are still silenced by it.
	if audienceEnabled(settings, AudienceApp) {
		t.Error("notify_app is off but platform app mail was allowed")
	}
}
