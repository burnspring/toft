package mail

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	texttemplate "text/template"

	"toft/internal/database"
	"toft/internal/notify"
)

// The templates are Go templates rather than a file per message because they
// share one frame. HTML bodies go through html/template, which escapes
// user-supplied names, logins, admin-authored deny reasons and an
// administrator's announcement text in the right context; the text bodies go
// through text/template, where escaping would be wrong.
//
// The visual language is deliberately plain — system email that looks like a
// marketing campaign gets filtered like one, and the corporate relays these
// installs sit behind are unforgiving about it.
//
// Plain is not the same as *unrelated*. The hexes below are the product's own
// tokens transcribed from `web/src/index.css` — background, card, border,
// foreground, muted-foreground and the indigo primary — because mail clients
// cannot read `oklch()` and cannot load Geist, so a palette here is the one
// place a second opinion would go unnoticed. The button had been near-black
// while every button in the application is indigo.

// The frame is the SPA's public frame (`PublicFrame`: the landing page, the
// sign-in pages, the gateway cards) carried as far as a mail client lets it go:
// one white surface rather than a card on grey, the wordmark over a hairline,
// a left-aligned display heading, and the footer under a second hairline.
// **The heading is the subject**, which every message already words for a
// reader, so no template draws one of its own; a `[app]` prefix that is there
// for an inbox filter is lifted out above the heading (`frameData`), the way
// the interstitial sets the app's name above its state.
//
// **The sender is the install's name** (`frameData.Sender`), resolved at send
// rather than passed by the code that built the message. With a name set the
// header is that name in text and the mark is not drawn — a message headed with
// Toft's mark and signed with somebody else's name names two senders. With none
// it is the mark and "Toft", byte for byte what it always was.
const htmlFrame = `<!doctype html>
<html><body style="margin:0;padding:0;background:#ffffff;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#ffffff;padding:32px 16px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;">
<tr><td style="padding:0 0 14px;border-bottom:1px solid #e0e2e5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:15px;font-weight:600;letter-spacing:-0.01em;color:#0a0a0a;">{{if .Named}}<span style="vertical-align:middle;">{{.Sender}}</span>{{else}}` + markImg + `&nbsp;&nbsp;<span style="vertical-align:middle;">Toft</span>{{end}}</td></tr>
<tr><td style="padding:32px 0 0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;color:#0a0a0a;font-size:15px;line-height:1.6;">
{{if .Eyebrow}}<div style="font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;color:#636363;padding-bottom:10px;">{{.Eyebrow}}</div>
{{end}}<h1 style="margin:0 0 20px;font-size:26px;line-height:1.2;font-weight:600;letter-spacing:-0.02em;color:#0a0a0a;">{{.Heading}}</h1>
{{template "body" .}}
</td></tr>
<tr><td style="padding:32px 0 0;">
<div style="border-top:1px solid #e0e2e5;padding-top:16px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:12px;line-height:1.5;color:#636363;">
Sent by {{.Sender}} at <a href="{{.BaseURL}}" style="color:#636363;">{{.BaseURL}}</a>.
</div>
</td></tr>
</table>
</td></tr>
</table>
</body></html>`

// frameData is what the frame executes with: the message's own data, promoted,
// plus the heading and the label above it, both derived from the subject, and
// the sender. The subject and the text half execute with it too, so a sentence
// that names the install reads Sender in all three.
//
// Sender is never empty: it is the install's name or "Toft". Named says which,
// for the few sentences that are worded differently when the install has a name
// of its own ("everyone on this Toft install" against "everyone on Staging").
type frameData struct {
	TemplateData
	Eyebrow, Heading string
	Sender           string
	Named            bool
}

// splitSubject lifts a leading `[name] ` off a subject. The prefix is for the
// inbox — a filter, a thread — and set as a heading it reads as markup.
func splitSubject(subject string) (eyebrow, heading string) {
	if strings.HasPrefix(subject, "[") {
		if i := strings.Index(subject, "] "); i > 1 {
			return subject[1:i], strings.TrimSpace(subject[i+2:])
		}
	}
	return "", subject
}

type tmplPair struct {
	subject string
	html    string
	text    string
}

// TemplateData is the data carried into every template. An alias rather than a
// type of its own: Slack renders the same notifications from the same facts, so
// the struct lives in internal/notify and both channels share it. Aliasing
// keeps `mail.TemplateData` working at every existing call site.
type TemplateData = notify.TemplateData

var templates = map[string]tmplPair{
	// A git host was connected, or the source allowlist was widened.
	//
	// **This is a notification and not a control, and the copy has to survive
	// that.** An administrator — or somebody holding their session — can widen
	// where code may come from, and after connections the credential for
	// whatever they allowed is already in a row. A four-eyes approval would be
	// the control; it needs an approval model this platform does not have, for
	// one field, while every other administrator action stays single-handed. So
	// what is built is this: it shortens the time such a change goes unnoticed,
	// it prevents nothing, and it does not claim otherwise.
	"source_reach_widened": {
		subject: "{{.EventTitle}}",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">{{.EventDetail}}</p>
<p style="margin:0 0 12px;color:#424242;">This is how far this install can now reach for code. If you did not expect it, Admin &rarr; Audit shows who made the change and exactly what it was.</p>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open Admin &rarr; Sources</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You are being told because widening where code may come from is worth somebody noticing. Nothing was blocked and nothing is waiting on you.</p>
{{end}}`,
		text: `{{.EventDetail}}

This is how far this install can now reach for code. If you did not expect it,
Admin -> Audit shows who made the change and exactly what it was.

{{.Link}}

You are being told because widening where code may come from is worth somebody
noticing. Nothing was blocked and nothing is waiting on you.
`,
	},
	"promotion_requested": {
		subject: "Publisher access requested by {{.RequesterName}}",
		html: `{{define "body"}}
<p style="margin:0 0 12px;"><strong>{{.RequesterName}}</strong> has asked for publisher access so they can create and deploy apps.</p>
<table role="presentation" cellpadding="0" cellspacing="0" style="margin:16px 0;font-size:13px;color:#424242;">
<tr><td style="padding:2px 16px 2px 0;color:#636363;">Account</td><td style="padding:2px 0;">{{.RequesterLogin}}</td></tr>
{{if .RequesterEmail}}<tr><td style="padding:2px 16px 2px 0;color:#636363;">Email</td><td style="padding:2px 0;">{{.RequesterEmail}}</td></tr>{{end}}
</table>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Review the request</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You will be asked to sign in — this link approves nothing on its own.</p>
{{end}}`,
		text: `{{.RequesterName}} has asked for publisher access so they can create and deploy apps.

Account: {{.RequesterLogin}}
{{if .RequesterEmail}}Email:   {{.RequesterEmail}}
{{end}}
Review the request:
{{.Link}}

You will be asked to sign in — this link approves nothing on its own.
`,
	},
	// Somebody wants access to one app, and the app's owners are the only
	// people who can say yes — so this is addressed to them and not to the
	// install's administrators, which is the whole difference between it and
	// the publisher request above.
	//
	// The requester's own words are quoted where they wrote any. That is the
	// point of asking for them: an owner deciding whether to widen access to a
	// dashboard is answering "what for", and a request that cannot say is one
	// that gets ignored until somebody asks in chat instead.
	// An administrator moved an app to a hostname of its own, or back. The
	// owner's links, bookmarks and embeds are what move, so this says what
	// keeps working (a page visit redirects) and what does not (a script on
	// another site calling the old address).
	"app_hostname_changed": {
		subject: "{{.AppName}} has a new address",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">An administrator moved <strong>{{.AppName}}</strong>. It now answers at <a href="{{.Address}}" style="color:#2855c4;">{{.Address}}</a>, once its rebuild at the new address finishes.</p>
<p style="margin:0 0 12px;">Its old address, {{.PreviousAddress}}, redirects there, so links and bookmarks keep working. A script on another site that calls the old address will not follow the redirect, and needs the new one.</p>
<div style="padding:20px 0 4px;"><a href="{{.Address}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open {{.AppName}}</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You are getting this because you own this app.</p>
{{end}}`,
		text: `An administrator moved {{.AppName}}. It now answers at {{.Address}}, once its rebuild at the new address finishes.

Its old address, {{.PreviousAddress}}, redirects there, so links and bookmarks
keep working. A script on another site that calls the old address will not
follow the redirect, and needs the new one.

You are getting this because you own this app.
`,
	},
	"access_requested": {
		subject: "{{.RequesterName}} has asked for access to {{.AppName}}",
		html: `{{define "body"}}
<p style="margin:0 0 12px;"><strong>{{.RequesterName}}</strong> has asked for {{.Role}} access to <strong>{{.AppName}}</strong>.</p>
{{if .Note}}<div style="margin:16px 0;padding:12px 16px;background:#f7f8fa;border-left:3px solid #e0e2e5;color:#424242;font-size:13px;white-space:pre-wrap;">{{.Note}}</div>{{end}}
<table role="presentation" cellpadding="0" cellspacing="0" style="margin:16px 0;font-size:13px;color:#424242;">
<tr><td style="padding:2px 16px 2px 0;color:#636363;">Account</td><td style="padding:2px 0;">{{.RequesterLogin}}</td></tr>
<tr><td style="padding:2px 16px 2px 0;color:#636363;">Asked for</td><td style="padding:2px 0;">{{.Role}}</td></tr>
</table>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Review the request</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You are getting this because you own this app. You will be asked to sign in &mdash; this link grants nothing on its own, and the request waits on the app&rsquo;s Access tab until somebody decides it.</p>
{{end}}`,
		text: `{{.RequesterName}} has asked for {{.Role}} access to {{.AppName}}.
{{if .Note}}
{{.Note}}
{{end}}
Account:   {{.RequesterLogin}}
Asked for: {{.Role}}

Review the request:
{{.Link}}

You are getting this because you own this app. You will be asked to sign in —
this link grants nothing on its own, and the request waits on the app's Access
tab until somebody decides it.
`,
	},
	// The answer, to the person who asked. Mail-only and person-facing like the
	// promotion outcomes it is modelled on.
	//
	// Role is what they now hold rather than what they asked for, and the two
	// are not always the same: approving never takes access away, so somebody
	// who already held more keeps it. A message naming the smaller role would
	// be the platform telling them they had been demoted when nothing moved.
	"access_approved": {
		subject: "You now have access to {{.AppName}}",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">{{if .ApproverName}}<strong>{{.ApproverName}}</strong> approved{{else}}The owner approved{{end}} your request for access to <strong>{{.AppName}}</strong>{{if .Role}}, and you now have {{.Role}} access{{end}}.</p>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open {{.AppName}}</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You will be asked to sign in. Nothing else is waiting on you.</p>
{{end}}`,
		text: `{{if .ApproverName}}{{.ApproverName}} approved{{else}}The owner approved{{end}} your request for access to {{.AppName}}{{if .Role}}, and you now have {{.Role}} access{{end}}.

Open it:
{{.Link}}

You will be asked to sign in. Nothing else is waiting on you.
`,
	},
	// The refusal, and the reason with it where one was given — for
	// promotion_denied's reason said again: "no" with nothing after it is the
	// version of this workflow people route around by asking in chat.
	//
	// It does not say what to do next, because the platform does not know: the
	// person who declined is the person to talk to, and inviting a second
	// identical request would be the product arguing with a decision it just
	// delivered.
	"access_denied": {
		subject: "Your request for access to {{.AppName}} was declined",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">{{if .ApproverName}}<strong>{{.ApproverName}}</strong> declined{{else}}The owner declined{{end}} your request for access to <strong>{{.AppName}}</strong>.</p>
{{if .Reason}}<div style="margin:16px 0;padding:12px 16px;background:#f7f8fa;border-left:3px solid #e0e2e5;color:#424242;font-size:13px;white-space:pre-wrap;">{{.Reason}}</div>{{end}}
<p style="margin:12px 0 0;font-size:13px;color:#636363;">Nothing else about your account has changed. If you think this was a mistake, the app&rsquo;s owner is who to ask.</p>
{{end}}`,
		text: `{{if .ApproverName}}{{.ApproverName}} declined{{else}}The owner declined{{end}} your request for access to {{.AppName}}.
{{if .Reason}}
Reason:
{{.Reason}}
{{end}}
Nothing else about your account has changed. If you think this was a mistake,
the app's owner is who to ask.
`,
	},
	// The report went out. The link is the *app's* address, built by the caller
	// through builder.PublicURL — every other link in this file points at the
	// platform, and on a split-origin install those are different hosts.
	"report_rendered": {
		subject: "{{.AppName}} has been updated",
		html: `{{define "body"}}
<p style="margin:0 0 12px;"><strong>{{.AppName}}</strong> has re-run and the latest version is ready.</p>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open the report</a></div>
<p style="margin:16px 0 0;color:#636363;font-size:13px;">You are on this report’s list because {{if .ApproverName}}{{.ApproverName}}{{else}}its owner{{end}} added you. Ask them to take you off it if you would rather not receive this.</p>
{{end}}`,
		text: `{{.AppName}} has re-run and the latest version is ready.

{{.Link}}

You are on this report’s list because {{if .ApproverName}}{{.ApproverName}}{{else}}its owner{{end}} added you. Ask them to take you off it if you would rather not receive this.
`,
	},
	"promotion_approved": {
		subject: "Your publisher access has been approved",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">{{if .ApproverName}}<strong>{{.ApproverName}}</strong> approved{{else}}An administrator approved{{end}} your request for publisher access. You can now create and deploy apps.</p>
<p style="margin:0 0 12px;"><strong>Sign in again</strong> to start using it — your current session still carries your old role.</p>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Sign in to {{.Sender}}</a></div>
{{end}}`,
		text: `{{if .ApproverName}}{{.ApproverName}} approved{{else}}An administrator approved{{end}} your request for publisher access. You can now create and deploy apps.

Sign in again to start using it — your current session still carries your old role.

{{.Link}}
`,
	},
	// Somebody arrived through a provider that puts new accounts in a queue.
	// AudienceAdmin: it is the nudge that makes the queue a workflow rather
	// than a page nobody opens.
	"account_pending_activation": {
		subject: "{{.RequesterName}} is waiting for an account to be activated",
		html: `{{define "body"}}
<p style="margin:0 0 12px;"><strong>{{.RequesterName}}</strong> signed in through {{.EventTitle}} for the first time. Their account was created and cannot be used until somebody activates it.</p>
<p style="margin:0 0 12px;color:#424242;">Account: {{.RequesterLogin}}{{if .RequesterEmail}}<br>Email: {{.RequesterEmail}}{{end}}</p>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open the queue</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You will be asked to sign in — this link activates nothing on its own. Sent once, when the account was created, and not again each time they try.</p>
{{end}}`,
		text: `{{.RequesterName}} signed in through {{.EventTitle}} for the first time. Their
account was created and cannot be used until somebody activates it.

Account: {{.RequesterLogin}}
{{if .RequesterEmail}}Email:   {{.RequesterEmail}}
{{end}}
Open the queue:
{{.Link}}

You will be asked to sign in — this link activates nothing on its own. Sent
once, when the account was created, and not again each time they try.
`,
	},
	// An administrator activated an account a provider created but did not
	// finish. Mail-only and person-facing like its neighbours, and for the
	// same reason: this is about one person's standing here, and a shared
	// channel is not where that goes.
	//
	// The body is the administrator's own sentence, escaped by html/template
	// like every other piece of typed-in copy in this file. The platform adds
	// only the link — nothing here restates what the message says, because an
	// install that wrote a careful sentence should not have the product argue
	// with it underneath.
	"account_activated": {
		subject: "Your account is ready",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">{{.ActivationMessage}}</p>
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Sign in to {{.Sender}}</a></div>
{{end}}`,
		text: `{{.ActivationMessage}}

{{.Link}}
`,
	},
	"promotion_denied": {
		subject: "Your publisher access request was declined",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">{{if .ApproverName}}<strong>{{.ApproverName}}</strong> declined{{else}}An administrator declined{{end}} your request for publisher access.</p>
{{if .Reason}}<div style="margin:16px 0;padding:12px 16px;background:#f7f8fa;border-left:3px solid #e0e2e5;color:#424242;font-size:13px;white-space:pre-wrap;">{{.Reason}}</div>{{end}}
<p style="margin:12px 0 0;font-size:13px;color:#636363;">You can still view and run the apps you have access to. Talk to an administrator if you think this was a mistake.</p>
{{end}}`,
		text: `{{if .ApproverName}}{{.ApproverName}} declined{{else}}An administrator declined{{end}} your request for publisher access.
{{if .Reason}}
Reason:
{{.Reason}}
{{end}}
You can still view and run the apps you have access to. Talk to an administrator
if you think this was a mistake.
`,
	},
	// The two app-failure messages. Audience is AudienceApp: these go to the
	// app's owners and editors, and an administrator who has switched
	// that audience off has switched these off with it.
	//
	// The copy never promises that anyone else was told — either notification
	// switch may be off, and the app's own switch may be off — so it points at
	// the Logs tab, which is always populated whatever the mail settings say.
	"app_event": {
		subject: "[{{.AppName}}] {{.EventTitle}}",
		html: `{{define "body"}}
{{if eq .Severity "info"}}<p style="margin:0 0 12px;">News from <strong>{{.AppName}}</strong>: {{.EventTitle}}.</p>{{else}}<p style="margin:0 0 12px;">Something went wrong with <strong>{{.AppName}}</strong>.</p>{{end}}
{{if .EventDetail}}<div style="margin:16px 0;padding:12px 16px;background:#f7f8fa;border-left:3px solid #e0e2e5;color:#424242;font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;">{{.EventDetail}}</div>{{end}}
{{if .EventWhen}}<p style="margin:0 0 12px;font-size:13px;color:#636363;">{{.EventWhen}}</p>{{end}}
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open the app</a></div>
{{if eq .Severity "info"}}<p style="margin:16px 0 0;font-size:13px;color:#636363;">You are getting this because you own or help maintain this app and its job asks to be reported after every run (notify = "always" in toft.toml).</p>{{else}}<p style="margin:16px 0 0;font-size:13px;color:#636363;">You are getting this because you own or help maintain this app. Every failure is listed on the app's Logs tab, whether or not it was emailed — and the app's settings page is where you turn these messages off.</p>{{end}}
{{end}}`,
		text: `{{if eq .Severity "info"}}News from {{.AppName}}.{{else}}Something went wrong with {{.AppName}}.{{end}}

{{.EventTitle}}
{{if .EventDetail}}
{{.EventDetail}}
{{end}}{{if .EventWhen}}
{{.EventWhen}}
{{end}}
Open the app:
{{.Link}}
{{if eq .Severity "info"}}
You are getting this because you own or help maintain this app and its job asks
to be reported after every run (notify = "always" in toft.toml).
{{else}}
You are getting this because you own or help maintain this app. Every failure is
listed on the app's Logs tab, whether or not it was emailed — and the app's
settings page is where you turn these messages off.
{{end}}`,
	},
	// The platform locked an app by itself. AudienceApp, like the two above,
	// and the copy has the same two obligations: it never promises anyone else
	// was told, and it never oversells what was checked — this reads package
	// names and versions against published advisories, and it did not read the
	// app's code.
	"app_locked_dependency": {
		subject: "[{{.AppName}}] Locked: {{.EventTitle}}",
		html: `{{define "body"}}
<p style="margin:0 0 12px;"><strong>{{.AppName}}</strong> has been locked by this install's dependency policy, and stopped.</p>
{{if .EventDetail}}<div style="margin:16px 0;padding:12px 16px;background:#f7f8fa;border-left:3px solid #e0e2e5;color:#424242;font-size:12px;white-space:pre-wrap;">{{.EventDetail}}</div>{{end}}
{{if .EventWhen}}<p style="margin:0 0 12px;font-size:13px;color:#636363;">{{.EventWhen}}</p>{{end}}
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open the app</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You are getting this because you own or help maintain this app. The finding is on the app's Dependencies tab, whether or not it was emailed. This check reads the names and versions of the packages your release installed and compares them to published advisories; it does not read your app's code.</p>
{{end}}`,
		text: `{{.AppName}} has been locked by this install's dependency policy, and stopped.

{{.EventTitle}}
{{if .EventDetail}}
{{.EventDetail}}
{{end}}{{if .EventWhen}}
{{.EventWhen}}
{{end}}
Open the app:
{{.Link}}

You are getting this because you own or help maintain this app. The finding is on
the app's Dependencies tab, whether or not it was emailed. This check reads the
names and versions of the packages your release installed and compares them to
published advisories; it does not read your app's code.
`,
	},
	"app_event_recovered": {
		subject: "[{{.AppName}}] Recovered: {{.EventTitle}}",
		html: `{{define "body"}}
<p style="margin:0 0 12px;"><strong>{{.AppName}}</strong> is working again.</p>
{{if .EventWhen}}<p style="margin:0 0 12px;font-size:13px;color:#636363;">{{.EventWhen}}</p>{{end}}
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open the app</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You were told when this started, so you are being told that it stopped. The full history is on the app's Logs tab.</p>
{{end}}`,
		text: `{{.AppName}} is working again.

{{.EventTitle}}
{{if .EventWhen}}
{{.EventWhen}}
{{end}}
Open the app:
{{.Link}}

You were told when this started, so you are being told that it stopped. The full
history is on the app's Logs tab.
`,
	},
	// Somebody shared an app with a person, and asked for them to be told.
	//
	// **AudienceApp**, because this is a message to somebody who uses Toft
	// about their own access — the same audience the app-failure notices belong
	// to, and an administrator who has switched it off has switched this off
	// with it.
	//
	// It is **opt-in per share**, which is the difference between it and every
	// other template here. The others are sent because something happened; this
	// is sent because a person ticked a box, so the copy has to read as that
	// person's message rather than the platform's. Hence the sharer's name in
	// the subject, their note quoted where they wrote one, and no sentence
	// offering to switch anything off — there is nothing to unsubscribe from,
	// because nothing sends again.
	//
	// The link is a pointer and not a credential. It is the app's page on this
	// install; whoever follows it signs in normally and is authorized normally
	// by the grant that was just written, which is the rule every link in this
	// file follows.
	"app_shared": {
		subject: "{{.RequesterName}} shared {{.AppName}} with you",
		html: `{{define "body"}}
<p style="margin:0 0 12px;"><strong>{{.RequesterName}}</strong> gave you access to <strong>{{.AppName}}</strong>{{if .Role}} as {{.Role}}{{end}}.</p>
{{if .Note}}<div style="margin:16px 0;padding:12px 16px;background:#f7f8fa;border-left:3px solid #e0e2e5;color:#424242;font-size:13px;white-space:pre-wrap;">{{.Note}}</div>{{end}}
<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">Open {{.AppName}}</a></div>
<p style="margin:16px 0 0;font-size:13px;color:#636363;">You will be asked to sign in. This message was sent once, because {{.RequesterName}} asked for it — there is nothing here that sends again.</p>
{{end}}`,
		text: `{{.RequesterName}} gave you access to {{.AppName}}{{if .Role}} as {{.Role}}{{end}}.
{{if .Note}}
{{.Note}}
{{end}}
Open it:
{{.Link}}

You will be asked to sign in. This message was sent once, because
{{.RequesterName}} asked for it — there is nothing here that sends again.
`,
	},
	// The administrator's own announcement, and the only template here whose
	// subject and body are typed by a person rather than written by this file.
	//
	// **Three strings are the administrator's and everything else is the
	// platform's.** The frame, the greeting, the footer, the escaping and the
	// link rendering are ours; the subject, the body and the link text are
	// interpolated. That is what keeps the audience mechanism intact — the
	// alternative is a handler building a mail.Message and setting Audience on
	// it, which is a send site declaring its own audience and the one thing
	// Render exists to prevent.
	//
	// The body is plain text. It goes through html/template in the HTML part,
	// which escapes it, and through text/template in the text part, where
	// escaping would be wrong; its line breaks survive as white-space:pre-wrap
	// rather than as generated markup, which is the same device the denial
	// reason and the share note already use. No Markdown: a subset would need a
	// parser, a sanitiser and a disagreement with the composer about what it
	// did, and "simple text with a link" is the whole ask.
	//
	// The footer says where an answer goes, because there is no per-announcement
	// Reply-To: an announcement inviting replies to a mailbox nobody watches is
	// a real failure that nobody notices for weeks.
	"announcement": {
		subject: "{{.AnnouncementSubject}}",
		html: `{{define "body"}}
<div style="margin:0 0 12px;white-space:pre-wrap;">{{.AnnouncementBody}}</div>
{{if .Link}}<div style="padding:20px 0 4px;"><a href="{{.Link}}" style="display:inline-block;background:#2855c4;color:#ffffff;text-decoration:none;padding:10px 18px;border-radius:8px;font-weight:600;font-size:14px;">{{if .LinkText}}{{.LinkText}}{{else}}Open {{.Sender}}{{end}}</a></div>{{end}}
<p style="margin:16px 0 0;font-size:13px;color:#636363;">Sent to everyone on {{if .Named}}{{.Sender}}{{else}}this Toft install{{end}} by an administrator. Replies go to whoever runs it &mdash; there is nothing here to unsubscribe from, because nothing sends again.</p>
{{end}}`,
		text: `{{.AnnouncementBody}}
{{if .Link}}
{{if .LinkText}}{{.LinkText}}{{else}}Open {{.Sender}}{{end}}:
{{.Link}}
{{end}}
Sent to everyone on {{if .Named}}{{.Sender}}{{else}}this Toft install{{end}} by an administrator. Replies go to
whoever runs it — there is nothing here to unsubscribe from, because nothing
sends again.
`,
	},
	"test_email": {
		subject: "{{if .Named}}Test email from {{.Sender}}{{else}}Toft test email{{end}}",
		html: `{{define "body"}}
<p style="margin:0 0 12px;">Email is working. {{.Sender}} reached {{if .Via}}{{.Via}}{{else}}your mail provider{{end}} and delivered to this address.</p>
<p style="margin:0;font-size:13px;color:#636363;">Links in {{if .Named}}email from {{.Sender}}{{else}}Toft email{{end}} point at <strong>{{.BaseURL}}</strong>. If that is not the address people use to reach this install, set <code>BASE_URL</code> — otherwise every notification will send them somewhere they cannot sign in.</p>
{{end}}`,
		text: `Email is working. {{.Sender}} reached {{if .Via}}{{.Via}}{{else}}your mail provider{{end}} and delivered to this address.

Links in {{if .Named}}email from {{.Sender}}{{else}}Toft email{{end}} point at {{.BaseURL}}. If that is not the address
people use to reach this install, set BASE_URL — otherwise every notification
will send them somewhere they cannot sign in.
`,
	},
}

// Render builds a Message from a named template, framed as an install with no
// name. An unknown name is a programming error and says so rather than sending
// a blank email.
//
// **What it returns is a draft as far as the sender is concerned.** The message
// remembers its template and data, and the Service renders it again at send
// with the install's name (`RenderAs`) — so no call site passes the name, a new
// one cannot forget to, and a message rendered before a rename and sent after
// it carries the new name. The bodies filled in here are what a caller that
// only reads them (a log line, a test) sees.
func Render(name string, data TemplateData) (Message, error) {
	return RenderAs("", name, data)
}

// RenderAs renders a template as an install called sender; "" is an install
// with no name, which reads "Toft". It is the one function that frames a
// platform message: the send path and every preview call it, so a preview
// shows the sender the real mail will carry.
func RenderAs(sender, name string, data TemplateData) (Message, error) {
	pair, ok := templates[name]
	if !ok {
		return Message{}, fmt.Errorf("unknown email template %q", name)
	}
	fd := frameData{TemplateData: data, Sender: database.DefaultSenderName}
	if s := strings.TrimSpace(sender); s != "" && s != database.DefaultSenderName {
		fd.Sender, fd.Named = s, true
	}

	subject, err := renderText("subject:"+name, pair.subject, fd)
	if err != nil {
		return Message{}, err
	}
	text, err := renderText("text:"+name, pair.text, fd)
	if err != nil {
		return Message{}, err
	}

	t, err := template.New("frame:" + name).Parse(htmlFrame)
	if err != nil {
		return Message{}, err
	}
	if _, err := t.Parse(pair.html); err != nil {
		return Message{}, err
	}
	subject = strings.TrimSpace(subject)
	fd.Eyebrow, fd.Heading = splitSubject(subject)
	var buf bytes.Buffer
	if err := t.Execute(&buf, fd); err != nil {
		return Message{}, err
	}

	return Message{
		Subject: subject,
		HTML:    buf.String(),
		Text:    textFooter(text, fd.Sender, data.BaseURL),
		// **Read from the one declaration, not stored beside the bodies.** A
		// template's audience is a property of the message rather than of the
		// channel carrying it, and two copies is how a notification comes to be
		// admin-audience in mail and app-audience everywhere else. A name with
		// no declaration has no audience, which the audience gate refuses — a
		// new notification cannot quietly escape an administrator's switches by
		// forgetting to name one.
		Audience: notify.AudienceOf(name),
		template: name,
		data:     data,
	}, nil
}

// textFooter closes the plain-text half with the origin the HTML half carries
// in its footer. Appended here rather than written into each template's text,
// or the next template ships without it — and the plain-text half is the one a
// filtering relay, a screen reader and a quoted reply see.
func textFooter(text, sender, baseURL string) string {
	return strings.TrimRight(text, "\n") + "\n\n--\n" + notify.Origin(sender, baseURL) + "\n"
}

// reframe renders a template-backed message again as an install called
// sender, keeping everything the caller set after Render. A message built
// without a template — app mail, which carries its own frame — is returned as
// it is.
func reframe(msg Message, sender string) Message {
	if msg.template == "" {
		return msg
	}
	fresh, err := RenderAs(sender, msg.template, msg.data)
	if err != nil {
		// It rendered once with the same data; a failure now is a programming
		// error, and the message as first rendered is better than none.
		return msg
	}
	msg.Subject, msg.HTML, msg.Text = fresh.Subject, fresh.HTML, fresh.Text
	return msg
}

func renderText(name, src string, data frameData) (string, error) {
	t, err := texttemplate.New(name).Parse(src)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
