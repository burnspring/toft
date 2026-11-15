package notify

import (
	"fmt"
	"strings"
)

// The one declaration of what every platform notification *is*.
//
// It replaces a mechanism that worked and could not survive eight channels.
// ADR §24 made three notifications email-only by **not writing a Slack
// template for them** — an omission enforced by absence, which is as strong as
// enforcement gets and needs no test. That mechanism does not scale: five
// notifications times seven channels as separate template files is thirty-five
// files nobody keeps in step, and the drift would be invisible, because a
// channel quietly rendering last month's wording is not something anybody
// reports.
//
// **So the replacement is weaker, and that is stated rather than hidden.** A
// notification renders once to a neutral Message and each channel turns that
// into its own format; the mail-only ones now have a *field* saying so instead
// of a file that does not exist, and a field can be copied from the wrong
// neighbour by somebody adding an eleventh. What stands in for absence is
// `TestPersonFacingNotificationsAreMailOnly`, which asserts the three by name.
// That is the price of not maintaining thirty-five files, and it is worth
// paying once rather than seven times.

// The channels a template may declare.
const (
	// ChannelMail is the relay. Mail is the outlier and always has been: it
	// computes a recipient list, where every other channel resolves one
	// destination from an attachment. That is why there is no Channel interface
	// over both (see this package's doc comment) and why mail renders its own
	// bodies rather than the neutral shape.
	ChannelMail = "mail"
	// ChannelPoster is every channel that addresses a destination — Slack, a
	// Teams workflow, a Discord channel, a signed webhook. They share the
	// neutral rendering because what differs between them is markup, and markup
	// is not a reason to write the same sentence seven times.
	ChannelPoster = "poster"
)

// Line is one labelled fact in a notification: "Account", "ada".
//
// An ordered list rather than a paragraph, because the channels differ in what
// they can do with structure — Slack has fields, a Teams card has a fact set,
// a webhook has JSON — and a sentence with the label welded into it can only
// ever be rendered one way.
type Line struct {
	Label string
	Value string
}

// Message is one notification rendered once, in the shape every posting channel
// can turn into its own.
//
// Deliberately small. It is the intersection of what seven far ends can
// express, not the union: a field only one of them can render is a field the
// other six have to ignore, which is how a shared shape rots into a switch.
type Message struct {
	// Title is the headline, and Fallback is the one line a client shows in a
	// sidebar or a desktop toast. Fallback has to stand alone — "New message"
	// is what a blank one gets you.
	Title    string
	Fallback string
	// Severity is "error", "warning" or "info", used only to pick a word or a
	// colour. It is the event's own, and a warning that reads like an outage is
	// how people learn to ignore both.
	Severity string
	// Body is the sentence under the title.
	Body string
	// Lines are the labelled facts.
	Lines []Line
	// Detail is an excerpt that explains — a build-log tail, git's own stderr.
	// A channel renders it preformatted and may trim it; a shared surface is
	// not an inbox somebody opted into.
	Detail string
	// LinkText and LinkURL are the one deep link. A pointer, not a credential:
	// whoever follows it still signs in and is still authorized.
	LinkText string
	LinkURL  string
	// Footer is the line after the link — what the reader should know about
	// where else this is recorded.
	Footer string
}

// Template is one notification's declaration: who it is for, which channels may
// carry it, and how it renders neutrally.
type Template struct {
	// Audience is who this message is for, and it lives here rather than at the
	// send site: a post site cannot declare its own audience, so a notification
	// added next year cannot quietly escape the administrator's switches.
	Audience string
	// Channels is which channels may carry it. A template with only ChannelMail
	// has no neutral rendering and is never posted anywhere.
	Channels []string
	// Render builds the neutral message. Nil for a mail-only template.
	//
	// sender is the install's name, or "Toft", resolved by the transport — the
	// Slack service and the platform poster — and never by the code that built
	// the data. Most renderings do not use it; the channel each posts to names
	// the sender in its own frame.
	Render func(d TemplateData, sender string) Message
}

// Carries reports whether this template may go out on a channel.
func (t Template) Carries(channel string) bool {
	for _, c := range t.Channels {
		if c == channel {
			return true
		}
	}
	return false
}

var mailOnly = []string{ChannelMail}
var everywhere = []string{ChannelMail, ChannelPoster}

// Templates is every platform notification this binary sends.
//
// **The list is the specification**, in both directions: a name absent from it
// is not a notification, and a name present in it declares one audience and one
// set of channels that both transports read. Mail keeps its own bodies —
// subjects, HTML, text — and reads the audience from here.
var Templates = map[string]Template{
	// An app broke. AudienceApp: it goes to the app's own destination when its
	// people have chosen one, and to the connection's default when they have
	// not.
	"app_event": {
		Audience: AudienceApp,
		Channels: everywhere,
		Render: func(d TemplateData, sender string) Message {
			if d.Severity == "info" {
				// Good news on the same template — a job with notify = "always"
				// that succeeded (BB-8): no "broken", no
				// failure footer.
				return Message{
					Title:    d.AppName + ": " + d.EventTitle,
					Fallback: "[" + d.AppName + "] " + d.EventTitle,
					Severity: d.Severity,
					Detail:   d.EventDetail,
					LinkText: "Open the app",
					LinkURL:  d.Link,
					Lines:    []Line{{Label: "When", Value: d.EventWhen}},
				}
			}
			return Message{
				Title:    d.AppName + " " + problemVerb(d.Severity),
				Fallback: "[" + d.AppName + "] " + d.EventTitle,
				Severity: d.Severity,
				Body:     d.EventTitle,
				Detail:   d.EventDetail,
				LinkText: "Open the app",
				LinkURL:  d.Link,
				Lines:    []Line{{Label: "When", Value: d.EventWhen}},
				Footer: "every failure is on the Logs tab, whether or not it was posted " +
					"here",
			}
		},
	},
	// The same problem, over. Sent only where the failure itself was announced —
	// Recorder.Clear checks that before calling.
	"app_event_recovered": {
		Audience: AudienceApp,
		Channels: everywhere,
		Render: func(d TemplateData, sender string) Message {
			return Message{
				Title:    d.AppName + " is working again.",
				Fallback: "[" + d.AppName + "] Recovered: " + d.EventTitle,
				Severity: "info",
				Body:     d.EventTitle,
				LinkText: "Open the app",
				LinkURL:  d.Link,
				Lines:    []Line{{Label: "When", Value: d.EventWhen}},
			}
		},
	},
	// The platform locked an app by itself, because its serving release
	// installs a package the install's dependency policy will not have running.
	//
	// **AudienceApp**, because the owner is who has to act: deploy a release
	// that does not install it, or ask an administrator. An administrator
	// audience would be a second template with a digest question behind it, and
	// the audit row and Admin → Dependencies are already the administrator's
	// immediate record.
	//
	// The copy names what was found and what to do, and promises nothing else
	// was told: either notification switch may be off, so it points at the
	// Dependencies tab, which is populated whatever the mail settings say.
	"app_locked_dependency": {
		Audience: AudienceApp,
		Channels: everywhere,
		Render: func(d TemplateData, sender string) Message {
			return Message{
				Title:    d.AppName + " has been locked.",
				Fallback: "[" + d.AppName + "] Locked: " + d.EventTitle,
				Severity: "error",
				Body:     d.EventTitle,
				Detail:   d.EventDetail,
				LinkText: "Open the app",
				LinkURL:  d.Link,
				Lines:    []Line{{Label: "When", Value: d.EventWhen}},
				Footer: "the finding is on the app's Dependencies tab, whether or not it " +
					"was posted here",
			}
		},
	},
	// Somebody wants publisher access. AudienceAdmin, so it goes to the
	// connection's default destination and never to an app's — an app's channel
	// is for that app.
	//
	// The link is a pointer, not a credential: it lands on the admin queue and
	// whoever follows it signs in and is authorized normally. There is no
	// one-click approve, here or in the email.
	"promotion_requested": {
		Audience: AudienceAdmin,
		Channels: everywhere,
		Render: func(d TemplateData, sender string) Message {
			return Message{
				Title: d.RequesterName + " has asked for publisher access so they can " +
					"create and deploy apps.",
				Fallback: "Publisher access requested by " + d.RequesterName,
				Severity: "info",
				Lines:    []Line{{Label: "Account", Value: d.RequesterLogin}},
				LinkText: "Review the request",
				LinkURL:  d.Link,
				Footer:   "you will be asked to sign in; this link approves nothing on its own",
			}
		},
	},
	// Somebody wants access to one app. **AudienceApp**, and that is the whole
	// difference from the publisher request above: this is about one app rather
	// than about the install, so it goes to that app's own destination — the
	// channel its team already watches — and never to the install default,
	// where a request for one of two hundred apps is somebody else's problem
	// scrolling past.
	//
	// The mail half of this notification is addressed to the app's *owners*
	// alone (ListAppDeciders), because approving is owner-only. The channel
	// half cannot be that precise — a channel is a room, and the room is the
	// app's — which is a real difference between the two transports and is why
	// the copy names the app rather than assuming the reader already knows
	// which one this is.
	//
	// The link is a pointer, not a credential. It lands on the app's Access
	// tab; whoever follows it signs in and is authorized normally, and there is
	// no one-click approve here or in the email.
	// An administrator moved an app to a hostname of its own, or back. Mail
	// only, to the owners, sent when the change is saved rather than when the
	// move's build flips: the owner's links are about to change either way,
	// and a cross-origin fetch to the old path stops working at the flip.
	"app_hostname_changed": {
		Audience: AudienceApp,
		Channels: mailOnly,
	},
	"access_requested": {
		Audience: AudienceApp,
		Channels: everywhere,
		Render: func(d TemplateData, sender string) Message {
			m := Message{
				Title: d.RequesterName + " has asked for " + d.Role + " access to " +
					d.AppName + ".",
				Fallback: "[" + d.AppName + "] Access requested by " + d.RequesterName,
				Severity: "info",
				Lines: []Line{
					{Label: "Account", Value: d.RequesterLogin},
					{Label: "Asked for", Value: d.Role},
				},
				LinkText: "Review the request",
				LinkURL:  d.Link,
				Footer:   "you will be asked to sign in; this link grants nothing on its own",
			}
			// Their own words, where they wrote any. Body rather than a Line:
			// it is a sentence somebody typed, not a labelled fact, and the
			// channels that can quote it should.
			if d.Note != "" {
				m.Body = d.Note
			}
			return m
		},
	},
	// A provider created an account under a `pending` policy and it is waiting
	// on an administrator. AudienceAdmin, like the publisher request above it
	// and for the same reason: it belongs to the install's default destination
	// and never to an app's.
	//
	// Sent once, when the account is created — not on every sign-in it is
	// refused. The queue is the record; this is the nudge that says to go and
	// look at it, and a nudge that repeats every time somebody retries is a
	// nudge people filter.
	"account_pending_activation": {
		Audience: AudienceAdmin,
		Channels: everywhere,
		Render: func(d TemplateData, sender string) Message {
			return Message{
				Title:    d.RequesterName + " signed in for the first time and is waiting to be activated.",
				Fallback: "Account awaiting activation: " + d.RequesterName,
				Severity: "info",
				Lines: []Line{
					{Label: "Account", Value: d.RequesterLogin},
					{Label: "Signed in with", Value: d.EventTitle},
				},
				LinkText: "Open the queue",
				LinkURL:  d.Link,
				Footer:   "you will be asked to sign in; this link activates nothing on its own",
			}
		},
	},
	// A git host was connected, or the source allowlist was widened. Same
	// message as the email and the same posture: it shortens how long such a
	// change goes unnoticed, and it is not a control.
	"source_reach_widened": {
		Audience: AudienceAdmin,
		Channels: everywhere,
		Render: func(d TemplateData, sender string) Message {
			return Message{
				Title:    d.EventTitle,
				Fallback: d.EventTitle,
				Severity: "warning",
				Body:     d.EventDetail,
				LinkText: "Open Admin → Sources",
				LinkURL:  d.Link,
				Footer:   "Admin → Audit shows who made the change",
			}
		},
	},
	// The administrator's own diagnostic. AudienceTest belongs to no switch — a
	// test that goes quiet because notifications are off tests nothing and
	// reads as a broken integration.
	"test_post": {
		Audience: AudienceTest,
		Channels: []string{ChannelPoster},
		Render: func(d TemplateData, sender string) Message {
			return Message{
				Title:    sender + " can reach this destination.",
				Fallback: testFallback(sender),
				Severity: "info",
				LinkText: d.BaseURL,
				LinkURL:  d.BaseURL,
				Footer: "links in " + notificationsFrom(sender) + " point there. If that " +
					"is not the address people use to reach this install, set the public " +
					"address on the Email settings page",
			}
		},
	},

	// ---------------------------------------------------------- mail only

	// **The deliberate ones.** Each is about a *person* rather than about
	// an app: "your publisher access was declined, here is the administrator's
	// reason" is a private refusal, and a shared channel is not where it goes.
	// This is the rule ADR §24 used to enforce by a Slack template not
	// existing, and `TestPersonFacingNotificationsAreMailOnly` is what replaces
	// that absence.
	// A scheduled render landed, and the people its owner named are told. Mail
	// only and deliberately so: a chat channel is a room, and a report's
	// audience is a list of individuals its owner chose.
	"report_rendered":    {Audience: AudienceReport, Channels: mailOnly},
	"promotion_approved": {Audience: AudienceApp, Channels: mailOnly},
	"promotion_denied":   {Audience: AudienceApp, Channels: mailOnly},
	"app_shared":         {Audience: AudienceApp, Channels: mailOnly},
	// The two outcomes of an app access request, and mail-only for exactly the
	// reason the two promotion outcomes are. The *ask* is about an app and goes
	// to the app's channel; the answer is about one person's standing, and
	// "your request for the payroll dashboard was declined, here is why" posted
	// into the team room is a private refusal read out loud.
	"access_approved": {Audience: AudienceApp, Channels: mailOnly},
	"access_denied":   {Audience: AudienceApp, Channels: mailOnly},
	// The fourth, and the first that is good news. Same rule and the same
	// reason: "your account here has been activated" is about one person's
	// standing, and posting it into a team channel would make an admission an
	// announcement.
	"account_activated": {Audience: AudienceApp, Channels: mailOnly},

	// The administrator's own announcement. Mail-only **structurally**, and it
	// is a kind of mail-only of its own again: not a private refusal and not an
	// undecided rendering, but a message whose entire addressing decision has
	// no meaning in a channel. "All users" posted to #engineering is neither
	// all users nor a decision anybody made — and the install already has a
	// banner for "everyone who is looking", which is what a channel post is.
	//
	// Named in TestPersonFacingNotificationsAreMailOnly, which is what makes
	// the field honest.
	"announcement": {Audience: AudienceAnnouncement, Channels: mailOnly},

	// The relay's own diagnostic, and mail's alone: it proves a relay.
	"test_email": {Audience: AudienceTest, Channels: mailOnly},
}

// Render builds the neutral message for a template, or reports that this
// notification is not one any posting channel carries. sender is the install's
// name, or "" for an install with none, which reads "Toft".
//
// A missing name is not necessarily a programming error — it is also how the
// mail-only notifications stay mail-only — so a caller treats false as "nothing
// to post" rather than as a failure.
func Render(name, sender string, d TemplateData) (Message, bool) {
	t, ok := Templates[name]
	if !ok || t.Render == nil || !t.Carries(ChannelPoster) {
		return Message{}, false
	}
	return t.Render(d, SenderOrDefault(sender)), true
}

// SenderOrDefault is the install's name, or "Toft" when it has none.
func SenderOrDefault(sender string) string {
	if v := strings.TrimSpace(sender); v != "" {
		return v
	}
	return DefaultSender
}

// DefaultSender is what an install with no name calls itself. The same word as
// database.DefaultSenderName, repeated because this package sits below it.
const DefaultSender = "Toft"

// testFallback and notificationsFrom word the channel test for an install
// with a name and for one without, which reads exactly as it always has.
func testFallback(sender string) string {
	if sender == DefaultSender {
		return "Toft test message"
	}
	return "Test message from " + sender
}

func notificationsFrom(sender string) string {
	if sender == DefaultSender {
		return "Toft notifications"
	}
	return "notifications from " + sender
}

// AudienceOf is who a notification is for, or "" for a name nobody declared.
func AudienceOf(name string) string { return Templates[name].Audience }

// Carries reports whether a named notification may go out on a channel.
func Carries(name, channel string) bool {
	t, ok := Templates[name]
	return ok && t.Carries(channel)
}

// MustAudience is AudienceOf with a name a caller believes in, for the
// transports that would otherwise send with no audience at all.
func MustAudience(name string) (string, error) {
	a := AudienceOf(name)
	if a == "" {
		return "", fmt.Errorf("no notification called %q is declared", name)
	}
	return a, nil
}

// problemVerb picks the word for how bad this is. Severity is the event's own,
// and a warning that reads like an outage is how people learn to ignore both.
func problemVerb(severity string) string {
	if severity == "warning" {
		return "has a problem."
	}
	return "is broken."
}
