package mail

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/unnamed.golden.json from the current templates")

// goldenData fills every field any template reads, so the golden file covers
// every branch a populated message takes.
func goldenData() TemplateData {
	return TemplateData{
		BaseURL:             "https://apps.example.com",
		RequesterName:       "Ada Lovelace",
		RequesterLogin:      "ada",
		RequesterEmail:      "ada@example.com",
		ApproverName:        "Grace Hopper",
		Reason:              "Not this quarter.",
		Link:                "https://apps.example.com/admin",
		Via:                 "your SMTP relay at smtp.example.com",
		AppName:             "Payroll",
		EventTitle:          "Deploy failed",
		EventDetail:         "exit status 1",
		EventWhen:           "2026-10-30 09:00 UTC",
		Severity:            "error",
		Role:                "viewer",
		AnnouncementSubject: "Maintenance tonight",
		AnnouncementBody:    "We will be down from 22:00.",
		Note:                "Here you go.",
		ActivationMessage:   "Welcome aboard.",
		Address:             "https://payroll.example.com",
		PreviousAddress:     "https://apps.example.com/apps/payroll",
	}
}

type goldenMessage struct {
	Subject, HTML, Text string
}

func renderAllUnnamed(t *testing.T) map[string]goldenMessage {
	t.Helper()
	out := map[string]goldenMessage{}
	for name := range templates {
		for _, variant := range []string{"full", "empty"} {
			d := goldenData()
			if variant == "empty" {
				d = TemplateData{BaseURL: "https://apps.example.com"}
			}
			msg, err := Render(name, d)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out[name+"/"+variant] = goldenMessage{msg.Subject, msg.HTML, msg.Text}
		}
	}
	return out
}

// An install with no name sends what it sent before the setting existed: the
// HTML, the subject and the text body byte for byte, the text body now
// followed by the origin it never used to carry. A regression here changes
// every install's mail and nothing would notice until somebody read a footer.
func TestUnnamedInstallRendersTodaysBytes(t *testing.T) {
	path := filepath.Join("testdata", "unnamed.golden.json")
	got := renderAllUnnamed(t)
	if *updateGolden {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]goldenMessage
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, ok := want[k]
		if !ok {
			t.Errorf("%s: no golden output; a template added after the setting is captured by -update-golden", k)
			continue
		}
		g := got[k]
		if g.Subject != w.Subject {
			t.Errorf("%s subject:\n got %q\nwant %q", k, g.Subject, w.Subject)
		}
		if g.HTML != w.HTML {
			t.Errorf("%s HTML differs from today's", k)
		}
		footer := "\n\n--\nSent by Toft at https://apps.example.com\n"
		if !strings.HasSuffix(g.Text, footer) || strings.TrimSuffix(g.Text, footer) != strings.TrimRight(w.Text, "\n") {
			t.Errorf("%s text:\n got %q\nwant %q + the origin", k, g.Text, w.Text)
		}
	}
}

// sentinel is an install name no template could contain by accident.
const sentinel = "Zebra Works Platform"

// No platform sender says "Toft" once the install has a name: every mail
// template in both halves and its subject, and the app frame. A sentence that
// names the *product* rather than the sender would be allowed by name here, and
// there is none — so a "Toft" that turns up is template copy that forgot the
// resolver, the defect this setting would otherwise half fix.
func TestNamedInstallNeverCallsItselfToft(t *testing.T) {
	allowed := map[string]bool{} // the specification: empty
	for name := range templates {
		for _, d := range []TemplateData{goldenData(), {BaseURL: "https://apps.example.com"}} {
			msg, err := RenderAs(sentinel, name, d)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for part, body := range map[string]string{"subject": msg.Subject, "html": msg.HTML, "text": msg.Text} {
				if strings.Contains(body, "Toft") && !allowed[name] {
					t.Errorf("%s %s says Toft with the install named %q", name, part, sentinel)
				}
				if !strings.Contains(body, sentinel) && part != "subject" {
					t.Errorf("%s %s never names the install", name, part)
				}
			}
			if usesMark(msg.HTML) {
				t.Errorf("%s draws Toft's mark beside another name", name)
			}
			if want := "\n\n--\nSent by " + sentinel + " at https://apps.example.com\n"; !strings.HasSuffix(msg.Text, want) {
				t.Errorf("%s text half does not end in the origin: %q", name, msg.Text)
			}
		}
	}

	app := BuildAppMessage(sentinel, "Payroll", "https://apps.example.com/apps/payroll", "",
		"Monthly", "text", "<p>html</p>", []string{"a@example.com"}, nil, nil, nil)
	for part, body := range map[string]string{"from": app.FromName, "html": app.HTML, "text": app.Text} {
		if strings.Contains(body, "Toft") {
			t.Errorf("app mail %s says Toft with the install named: %q", part, body)
		}
	}
	if usesMark(app.HTML) {
		t.Error("app mail draws Toft's mark beside another name")
	}
}

// The name is applied at send, not at render: a message built before a rename
// and handed to the relay after it carries the new name, and keeps everything
// its caller set on it in between.
func TestReframeAppliesTheNameAtSend(t *testing.T) {
	msg, err := Render("app_event", goldenData())
	if err != nil {
		t.Fatal(err)
	}
	msg.To = []string{"ops@example.com"}
	sent := reframe(msg, sentinel)
	if !strings.Contains(sent.HTML, "Sent by "+sentinel+" at") || strings.Contains(sent.HTML, "Sent by Toft") {
		t.Errorf("the frame at send does not carry the new name")
	}
	if len(sent.To) != 1 || sent.Audience != msg.Audience {
		t.Errorf("reframing lost what the caller set: %+v", sent)
	}
	if unnamed := reframe(msg, ""); unnamed.HTML != msg.HTML || unnamed.Text != msg.Text {
		t.Error("reframing an unnamed install changed the message")
	}

	// App mail has no template behind it and carries its own frame.
	app := BuildAppMessage("", "Payroll", "", "", "s", "t", "", []string{"a@example.com"}, nil, nil, nil)
	if got := reframe(app, sentinel); got.Text != app.Text || got.FromName != app.FromName {
		t.Error("reframe touched app mail")
	}
}

// App mail's sender is "<App> (via <install>)", and "(via Toft)" without a
// name. An app that puts "(via …)" or the install's name in its own name
// changes nothing the platform appends.
func TestAppMailSenderFollowsTheInstallName(t *testing.T) {
	for _, tc := range []struct{ install, app, want string }{
		{"", "Payroll", "Payroll (via Toft)"},
		{sentinel, "Payroll", "Payroll (via " + sentinel + ")"},
		{sentinel, "Payroll (via Toft)", "Payroll (via Toft) (via " + sentinel + ")"},
		{"", "Zebra Works Platform", "Zebra Works Platform (via Toft)"},
	} {
		msg := BuildAppMessage(tc.install, tc.app, "", "", "s", "t", "", []string{"a@example.com"}, nil, nil, nil)
		if msg.FromName != tc.want {
			t.Errorf("install %q, app %q: From name %q, want %q", tc.install, tc.app, msg.FromName, tc.want)
		}
	}
}
