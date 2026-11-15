package notify

import (
	"strings"
	"testing"
)

// **This test replaces a mechanism, and the mechanism it replaces was stronger.**
//
// ADR §24 made three notifications email-only by not writing a Slack template
// for them: rendering returned an error for anything the file did not define,
// so the omission was enforced by absence and needed no test at all. Under a
// shared shape the template exists and only a field stops it — and a field can
// be copied from the wrong neighbour by somebody adding an eleventh
// notification.
//
// So this asserts them **by name**. Each is about a *person* rather than
// about an app: "your publisher access was declined, here is the
// administrator's reason" is a private refusal, "somebody shared an app with
// you" names who, and a shared channel is not where either goes. Nothing about
// the shared rendering changed that; what changed is that the rule now needs
// saying out loud.
func TestPersonFacingNotificationsAreMailOnly(t *testing.T) {
	for _, name := range []string{
		"promotion_approved", // access granted
		"promotion_denied",   // access declined
		"app_shared",         // a share
		"account_activated",  // an account admitted
		"access_approved",    // an app access request allowed
		"access_denied",      // an app access request refused
		// The last, and mail-only for a different reason than the ones above
		// it: recipient selection is the whole of an announcement's addressing
		// decision, and it has no meaning in a channel.
		"announcement",
	} {
		tmpl, ok := Templates[name]
		if !ok {
			t.Errorf("%s is not declared — if it was renamed, rename it here too, "+
				"because this is the only thing keeping it off a channel", name)
			continue
		}
		if len(tmpl.Channels) != 1 || tmpl.Channels[0] != ChannelMail {
			t.Errorf("%s carries %v — it is about a person, and a shared channel is not "+
				"where a private refusal goes", name, tmpl.Channels)
		}
		if tmpl.Render != nil {
			t.Errorf("%s has a neutral rendering, which is a rendering for channels it "+
				"must never reach", name)
		}
		// And the belt: Render refuses it whatever the field says.
		if _, ok := Render(name, "", TemplateData{}); ok {
			t.Errorf("%s rendered for a posting channel", name)
		}
	}
}

// Every declaration is complete, in the way the catalog's is: a notification
// with no audience is one that escapes the administrator's switches, and one
// with a poster channel and no renderer is a post that silently never happens.
func TestEveryTemplateIsCompletelyDeclared(t *testing.T) {
	for name, tmpl := range Templates {
		switch {
		case tmpl.Audience == "":
			t.Errorf("%s declares no audience — a message with none escapes every switch", name)
		case len(tmpl.Channels) == 0:
			t.Errorf("%s carries no channel at all, which is a notification nobody gets", name)
		}
		switch tmpl.Audience {
		case AudienceAdmin, AudienceApp, AudienceAnnouncement, AudienceReport, AudienceTest:
		default:
			t.Errorf("%s has audience %q, which is not one this package knows",
				name, tmpl.Audience)
		}
		for _, c := range tmpl.Channels {
			if c != ChannelMail && c != ChannelPoster {
				t.Errorf("%s names channel %q, which is not one this package knows", name, c)
			}
		}
		if tmpl.Carries(ChannelPoster) && tmpl.Render == nil {
			t.Errorf("%s says it may be posted and has no rendering — it would be silently "+
				"skipped on every channel", name)
		}
		if !tmpl.Carries(ChannelPoster) && tmpl.Render != nil {
			t.Errorf("%s has a rendering for channels it does not carry", name)
		}
	}
}

// A rendered message stands on its own: a fallback that says nothing is what a
// reader sees in a sidebar, and a link with no text is a bare URL.
func TestEveryRenderingProducesSomethingReadable(t *testing.T) {
	data := TemplateData{
		BaseURL:        "https://toft.example",
		AppName:        "Reconciler",
		RequesterName:  "Ada Lovelace",
		RequesterLogin: "ada",
		EventTitle:     "the build failed",
		EventDetail:    "npm ERR! code ELIFECYCLE",
		EventWhen:      "2 Jan 2026 at 15:04 UTC",
		Severity:       "error",
		Link:           "https://toft.example/apps/reconciler",
	}
	for name, tmpl := range Templates {
		if tmpl.Render == nil {
			continue
		}
		msg := tmpl.Render(data, "Toft")
		if strings.TrimSpace(msg.Fallback) == "" {
			t.Errorf("%s has no fallback — that is what a desktop toast shows", name)
		}
		if strings.TrimSpace(msg.Title) == "" {
			t.Errorf("%s has no title", name)
		}
		if msg.LinkURL != "" && msg.LinkText == "" {
			t.Errorf("%s links to %q with no words for it", name, msg.LinkURL)
		}
	}
}

// An unknown name is "nothing to post", not a failure: it is also how the
// mail-only notices stay mail-only, so a caller that treated it as an error
// would log one every time somebody's access was declined.
func TestAnUnknownNameIsNothingToPost(t *testing.T) {
	if _, ok := Render("no_such_notification", "", TemplateData{}); ok {
		t.Error("a notification nobody declared rendered")
	}
	if got := AudienceOf("no_such_notification"); got != "" {
		t.Errorf("audience of an undeclared notification = %q", got)
	}
	if _, err := MustAudience("no_such_notification"); err == nil {
		t.Error("MustAudience accepted a name nobody declared")
	}
}
