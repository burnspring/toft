package appint

import "strings"

// The catalog: every integration this binary offers, in one list.
//
// **The list is the specification.** A kind absent from Known is not an
// integration — asking about one is a 400 that says so, which is what the
// `default` arm of the old switch did and is the one behaviour of it worth
// keeping. Everything else that used to be a switch arm, a store method or a
// hand-written refusal sentence is a field below, read by the gates, the
// wiring, the admin API, the settings index, the ledger and the telemetry
// labels.
//
// It is a slice rather than a map assembled by each transport registering
// itself in init(), on `entitlements.Known`'s precedent and for two reasons of
// its own. The order of registration would decide the order of the admin UI,
// which is a product decision made by a linker. And a build that omitted a
// transport package would change what the product *is* without saying so —
// where a slice makes the omission a compile error at the registry instead.
//
// What a Kind may not carry is anything about *how* a send goes out. There is
// no transport here, no client, no URL: this package still imports none of them
// (see the package comment), and the registry that maps a kind to a transport
// lives in the wiring layer, which is the only place that knows about all of
// them.

// Integration names, as they appear in the ledger and in a capability string.
//
// A constant per kind rather than free strings, so a typo is a compile error
// rather than a kind that is silently never matched. They are only the names the
// rest of the tree needs to *say*; a kind nothing else names by hand needs no
// constant, and Known is still the specification.
const (
	IntegrationSlack   = "slack"
	IntegrationEmail   = "email"
	IntegrationWebhook = "webhook"

	IntegrationSharePoint = "sharepoint"
	IntegrationOneDrive   = "onedrive"
	IntegrationTeams      = "teams"
	IntegrationTeamsBot   = "teamsbot"

	IntegrationGoogleDrive = "gdrive"
	IntegrationGoogleChat  = "gchat"
	IntegrationDiscord     = "discord"
	IntegrationPagerDuty   = "pagerduty"
)

// Carries says what an action puts on the wire, which is what decides whether
// the app-facing route is JSON or multipart.
type Carries string

const (
	// CarriesText is a message: JSON in, no file.
	CarriesText Carries = "text"
	// CarriesFile is one file: multipart in.
	CarriesFile Carries = "file"
	// CarriesNothing is an action that sends nothing: the app asks, and is
	// handed something. The token action is the one member.
	CarriesNothing Carries = "nothing"
)

// ActionToken is the action that hands an app the connection's own short-lived
// access token, so it can call the provider with the provider's own SDK. It is
// on the three file kinds only, and granted only while the connection's
// issue_tokens switch is on (Check, Capabilities).
const ActionToken = "token"

// tokenAction is the same entry on each kind that issues tokens.
var tokenAction = Action{Name: ActionToken, Carries: CarriesNothing,
	Summary: "Get the connection's own access token, while the connection lets apps " +
		"have one. It reaches everything the connection's grant does, not only this folder."}

// Action is one thing an app may do with an integration.
//
// The name is the second half of a capability — "post" in "slack.post" — and it
// is what the ledger's `action` column records. Actions are *not separately
// granted*: an app that may post to Slack may post a file, and an app that may
// send mail may attach one (migration 44). They are named because the ledger
// records them and because an app asks about one by name, never because each is
// a switch.
type Action struct {
	Name    string  `json:"name"`
	Carries Carries `json:"carries"`
	// Summary is the one line the attach dialog shows beside the action, so an
	// administrator granting a kind can read what granting it lets an app do.
	Summary string `json:"summary"`
}

// Credential is one field the connection form asks for.
//
// Secret decides which column the value lands in — `secret`, encrypted at rest,
// or `config`, a jsonb document readable in the clear. That is a property of
// the field and not of the form, so a kind cannot accidentally put a routing
// key somewhere an audit row will quote it.
type Credential struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Secret   bool   `json:"secret"`
	Required bool   `json:"required"`
}

// Refusals are the words each gate says.
//
// On the Kind rather than written out at the enforcement point, which is what
// makes them one thing to read rather than four sentences per kind scattered
// through a switch. Every one of them names the remedy, because the standing
// rule on this surface is that a refusal a person can act on beats a 403 they
// have to ask about.
type Refusals struct {
	// NotConnected: no connection of this kind exists, or it is not offered to
	// apps. An administrator's job.
	NotConnected string `json:"not_connected"`
	// NotAttached: the install has one and nobody attached it to this app.
	NotAttached string `json:"not_attached"`
	// Disabled: attached and switched off. A different sentence from the one
	// above, because it has a different fix.
	Disabled string `json:"disabled"`
	// TokensOff: the token action, on a connection that does not let apps have
	// one. Set on the kinds that issue tokens and nowhere else.
	TokensOff string `json:"tokens_off,omitempty"`
}

// Kind is one integration, completely described.
type Kind struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`

	// ScopeLabel names what a connection of this kind is welded to — "workspace",
	// "site", "webhook URL" — and ScopeHint is the sentence under the field.
	//
	// The scope is fixed when a connection is created and is never editable.
	// Changing the site or the URL under a live attachment would silently
	// re-point every app on it, which is a disclosure wearing a different hat.
	ScopeLabel string `json:"scope_label"`
	ScopeHint  string `json:"scope_hint,omitempty"`

	// DescriptionHint is the placeholder in the connection's description
	// field, which every kind has and none requires: it says what is worth
	// writing down about a connection of this kind. DescriptionMissing, when a
	// kind sets it, is what the connection's card says while the description
	// is empty — set only where the far side cannot be read from Toft, so the
	// note is the one record of who answers for it.
	DescriptionHint    string `json:"description_hint,omitempty"`
	DescriptionMissing string `json:"description_missing,omitempty"`

	Credentials []Credential `json:"credentials"`

	// Enumerates reports whether a connection of this kind can list the
	// destinations it may reach — Slack lists the channels the bot is in.
	//
	// A property of the kind rather than a second table, and where it is false
	// the destinations route 404s rather than returning an empty list: an empty
	// list reads as *none configured yet*, which is a different and wrong thing
	// to tell somebody about a welded webhook that has exactly one destination
	// by construction.
	Enumerates bool `json:"enumerates"`
	// DestinationLabel is the noun a person reads — "channel", "folder".
	DestinationLabel string `json:"destination_label,omitempty"`

	Actions []Action `json:"actions"`

	// TextLimit bounds a text action's body and FileLimit bounds one file.
	//
	// Per kind because the far sides differ by three orders of magnitude — a
	// Slack section is 3000 characters and a mail body is a quarter of a
	// megabyte — and because a single limit would have to be the smallest,
	// which would silently make mail worse to buy a table one row narrower.
	// Every limit is a refusal that names the limit, never a truncation: an app
	// that quietly loses the tail of a report has a report that is wrong in a
	// way nobody notices.
	TextLimit int `json:"text_limit"`
	FileLimit int `json:"file_limit"`

	// LeavesResidue reports that a send of this kind puts an object in somebody
	// else's system that outlives the send, the attachment and the app.
	//
	// This is the one field with a schema consequence: a ledger row for a
	// residue-leaving kind is exempt from retention's delete (D9), and a
	// placement row is written against the *connection* so it survives the app
	// (D10). Chatter expires; a file in an organization's document library is
	// not chatter.
	LeavesResidue bool `json:"leaves_residue"`

	// PlatformAudiences is which of notify's audiences this kind will carry on
	// the platform seam. Empty means the kind is app-initiated only.
	//
	// Data rather than a special case. Slack and email carry both. The three
	// file kinds carry none, because "your app crashed" as a text file dropped
	// into a document library is a notification nobody reads. Every other kind
	// carries none *yet* -- PagerDuty included: what the platform says through
	// it is a decision nobody has taken (its entry says so), and if one is
	// taken, the app audience without the admin one is the likely shape, since
	// paging the operators about a publisher request is how a channel earns a
	// filter rule.
	PlatformAudiences []string `json:"platform_audiences"`

	Refusals Refusals `json:"refusals"`
}

// Known is every integration this binary offers, in the order the admin surface
// lists them.
//
// Slack and email are first and are not new. They are in the catalog rather than
// beside it because two answers to "is this app attached" is exactly what the
// catalog exists to prevent — see D18, which drops their tables rather than
// leaving a compatibility read nothing writes.
var Known = []Kind{
	{
		Name:       IntegrationSlack,
		Title:      "Slack",
		Summary:    "Post an app's own messages, and upload a file, into a Slack channel.",
		ScopeLabel: "workspace",
		ScopeHint: "The bot token decides the workspace. An app can reach exactly the " +
			"channels somebody invited the bot into — that membership is the boundary, " +
			"not a list kept here.",
		Credentials: []Credential{
			{Key: "bot_token", Label: "Bot token", Secret: true, Required: true,
				Help: "Starts xoxb-. Needs chat:write, plus channels:read and groups:read " +
					"so a channel can be picked from a menu instead of pasted."},
		},
		Enumerates:       true,
		DestinationLabel: "channel",
		Actions: []Action{
			{Name: "post", Carries: CarriesText, Summary: "Post a message the app wrote."},
			{Name: "file", Carries: CarriesFile, Summary: "Upload one file to the channel."},
		},
		TextLimit:         3000,
		FileLimit:         25 << 20,
		PlatformAudiences: []string{"admin", "app"},
		Refusals: Refusals{
			NotConnected: "Slack is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "Slack is not enabled for this app — an administrator can enable " +
				"it on the app's Integrations tab",
			Disabled: "Slack is switched off for this app — an administrator can switch it " +
				"back on from the app's Integrations tab",
		},
	},
	{
		Name:       IntegrationEmail,
		Title:      "Email",
		Summary:    "Send a message as this app, through the install's own relay.",
		ScopeLabel: "relay",
		ScopeHint: "Mail is configured on the Email settings page, and this connection is " +
			"that relay. What it decides is which apps may send through it.",
		// No credential fields: the relay's own are on the Email settings page,
		// which is where they already were and where an operator looks for them.
		// A second copy here would be a second thing to rotate.
		Credentials:      []Credential{},
		Enumerates:       false,
		DestinationLabel: "reply-to address",
		Actions: []Action{
			{Name: "send", Carries: CarriesText, Summary: "Send a message to addresses the app names."},
			{Name: "attach", Carries: CarriesFile, Summary: "Send a message carrying files."},
		},
		TextLimit:         256 << 10,
		FileLimit:         10 << 20,
		PlatformAudiences: []string{"admin", "app"},
		Refusals: Refusals{
			NotConnected: "app email is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "email is not enabled for this app — an administrator can enable " +
				"it on the app's Integrations tab",
			Disabled: "email is switched off for this app — an administrator can switch it " +
				"back on from the app's Integrations tab",
		},
	},
	{
		Name:  IntegrationWebhook,
		Title: "Signed webhook",
		Summary: "Post an app's message to a URL you control, signed so the receiver can " +
			"prove it came from this install.",
		ScopeLabel: "receiver URL",
		ScopeHint: "https only, and it must resolve to a public address — Toft will not " +
			"be talked into posting inside your own network, and there is no way to switch " +
			"that off. The URL is fixed once this connection is made.",
		Credentials: []Credential{
			{Key: "signing_secret", Label: "Signing secret", Secret: true, Required: true,
				Help: "Shared with the receiver. Every request carries " +
					"X-Toft-Signature: t=<unix>,v1=<hmac-sha256 of \"t.body\">, over " +
					"the exact bytes sent — a receiver that re-serializes before verifying " +
					"will fail."},
		},
		// Welded: one URL, fixed when the connection was made, and nothing
		// inside it to choose between. The destinations route 404s rather than
		// answering with an empty list, which would read as *none configured
		// yet*.
		Enumerates: false,
		Actions: []Action{
			{Name: "post", Carries: CarriesText,
				Summary: "Post a signed JSON message the app wrote."},
		},
		TextLimit: 16 << 10,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "no webhook is connected on this install — an administrator " +
				"connects one in Settings › Integrations",
			NotAttached: "this app has no webhook — an administrator can attach one on the " +
				"app's Integrations tab",
			Disabled: "this app's webhook is switched off — an administrator can switch it " +
				"back on from the app's Integrations tab",
		},
	},
	{
		Name:  IntegrationSharePoint,
		Title: "SharePoint",
		Summary: "Put an app's files into a site's document library, where the team " +
			"already looks for them.",
		ScopeLabel: "site",
		ScopeHint: "Pasted rather than picked, because Microsoft grants this one site at a " +
			"time: Sites.Selected is granted per site by a tenant administrator and has no " +
			"\"sites I may touch\" endpoint, so there is nothing to list. Like " +
			"contoso.sharepoint.com/sites/finance. Saving checks that the grant exists.",
		Credentials: []Credential{
			{Key: "tenant_id", Label: "Directory (tenant) ID", Required: true,
				Help: "From the app registration's Overview page in Entra."},
			{Key: "client_id", Label: "Application (client) ID", Required: true,
				Help: "The same page."},
			{Key: "client_secret", Label: "Client secret", Secret: true, Required: true,
				Help: "Certificates & secrets. Note when it expires — Entra secrets do, and " +
					"an expired one stops every app on this connection."},
		},
		Enumerates:       true,
		DestinationLabel: "folder",
		Actions: []Action{
			{Name: "upload", Carries: CarriesFile,
				Summary: "Add a file to the folder its people chose. It never replaces one."},
			tokenAction,
		},
		TextLimit: 1 << 10,
		FileLimit: 25 << 20,
		// The one field with a schema consequence. A file in an organization's
		// document library outlives the send, the attachment and the app, so
		// the row that records it is exempt from the ledger's retention delete
		// and carries no foreign key to the app.
		LeavesResidue: true,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "SharePoint is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app has no SharePoint site — an administrator can attach one " +
				"on the app's Integrations tab",
			Disabled: "this app's SharePoint is switched off — an administrator can switch " +
				"it back on from the app's Integrations tab",
			TokensOff: "this SharePoint connection does not hand out access tokens — an " +
				"administrator can turn on Let apps get access tokens on it in Settings › Integrations",
		},
	},
	{
		Name:  IntegrationOneDrive,
		Title: "OneDrive",
		Summary: "Put an app's files into one person's OneDrive — a service account's, " +
			"usually.",
		ScopeLabel: "account",
		ScopeHint: "The sign-in name whose drive this writes to. A shared service account " +
			"rather than a colleague's: files an app puts in somebody's personal drive " +
			"leave with them.",
		Credentials: []Credential{
			{Key: "tenant_id", Label: "Directory (tenant) ID", Required: true,
				Help: "From the app registration's Overview page in Entra."},
			{Key: "client_id", Label: "Application (client) ID", Required: true,
				Help: "The same page."},
			{Key: "client_secret", Label: "Client secret", Secret: true, Required: true,
				Help: "Certificates & secrets. Note when it expires — Entra secrets do, and " +
					"an expired one stops every app on this connection."},
		},
		Enumerates:       true,
		DestinationLabel: "folder",
		Actions: []Action{
			{Name: "upload", Carries: CarriesFile,
				Summary: "Add a file to the folder its people chose. It never replaces one."},
			tokenAction,
		},
		TextLimit:     1 << 10,
		FileLimit:     25 << 20,
		LeavesResidue: true,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "OneDrive is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app has no OneDrive — an administrator can attach one on the " +
				"app's Integrations tab",
			Disabled: "this app's OneDrive is switched off — an administrator can switch it " +
				"back on from the app's Integrations tab",
			TokensOff: "this OneDrive connection does not hand out access tokens — an " +
				"administrator can turn on Let apps get access tokens on it in Settings › Integrations",
		},
	},
	{
		Name:  IntegrationTeams,
		Title: "Microsoft Teams",
		Summary: "Post an app's messages into a Teams channel, through a Workflows " +
			"webhook you create in that channel.",
		ScopeLabel: "workflow URL",
		ScopeHint: "**This is a webhook, not a Graph integration.** Posting to a channel as " +
			"an application needs delegated authority — a person — or Teams' protected " +
			"APIs, which are behind Microsoft's own approval and billing. So the boundary " +
			"here is whoever owns the workflow in Power Automate, not a membership you can " +
			"inspect from Toft. In the channel, open Workflows and add the template " +
			"\"Send webhook alerts to a channel\" — not the \"from specific people\" or " +
			"\"from people in an org\" ones, which expect a sign-in Toft does not send — " +
			"and paste its URL. For a boundary Teams admins can see, use Microsoft Teams (bot).",
		DescriptionHint: "Which team and channel this posts to, and who owns the " +
			"workflow in Power Automate — the person to ask when posts stop.",
		DescriptionMissing: "No description: nothing here says who owns this " +
			"workflow, and they are the only one who can change where it posts.",
		// No credential of its own: the URL *is* the credential, which is
		// another way of saying this kind's boundary is weaker than the others
		// and the copy above has to say so.
		Credentials:      []Credential{},
		Enumerates:       false,
		DestinationLabel: "",
		Actions: []Action{
			{Name: "post", Carries: CarriesText,
				Summary: "Post a message the app wrote, as a card naming the app."},
		},
		TextLimit: 16 << 10,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "Teams is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app has no Teams channel — an administrator can attach one " +
				"on the app's Integrations tab",
			Disabled: "this app's Teams is switched off — an administrator can switch it " +
				"back on from the app's Integrations tab",
		},
	},
	{
		Name:  IntegrationTeamsBot,
		Title: "Microsoft Teams (bot)",
		Summary: "Post an app's messages, and Toft's own notices, into the Teams channels " +
			"and chats where your organization installed the Toft app.",
		ScopeLabel: "bot",
		ScopeHint: "The bot's application (client) ID, from its registration in the Teams " +
			"Developer Portal or Azure. Toft can post only where the Toft app is installed — " +
			"that installation, which Teams admins can see and remove, is the boundary. " +
			"After connecting, download the Teams app here, upload it in the Teams admin " +
			"center, add it to a team, and paste the team's link.",
		DescriptionHint: "Which teams this bot is for, and who registered it — the person who " +
			"can rotate its secret.",
		Credentials: []Credential{
			{Key: "tenant_id", Label: "Directory (tenant) ID", Required: true,
				Help: "The bot is single-tenant: it answers to this directory only."},
			{Key: "client_secret", Label: "Client secret", Secret: true, Required: true,
				Help: "The bot's client secret — the value, not its ID."},
		},
		Enumerates:       true,
		DestinationLabel: "channel or chat",
		Actions: []Action{
			{Name: "post", Carries: CarriesText,
				Summary: "Post a message the app wrote, as a card naming the app."},
		},
		TextLimit:         16 << 10,
		PlatformAudiences: []string{"admin", "app"},
		Refusals: Refusals{
			NotConnected: "the Teams bot is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app has no Teams channel — an administrator can attach one on " +
				"the app's Integrations tab",
			Disabled: "this app's Teams bot is switched off — an administrator can switch it " +
				"back on from the app's Integrations tab",
		},
	},
	{
		Name:  IntegrationGoogleDrive,
		Title: "Google Drive",
		Summary: "Put an app's files into a Drive folder, where the team already looks " +
			"for them.",
		ScopeLabel: "folder",
		ScopeHint: "The folder id — the last part of the folder's URL. The folder must be " +
			"in a shared drive, with the service account's own address added to that " +
			"shared drive as Contributor or above: a service account owns no storage, so " +
			"it cannot add a file to somebody's My Drive. That membership is the boundary, " +
			"and you can change it from Google without touching Toft. Use a service " +
			"account for this connection alone — anything else shared with it is " +
			"reachable by the same credential.",
		Credentials: []Credential{
			{Key: "service_account", Label: "Service account key", Secret: true, Required: true,
				Help: "The whole JSON file Google Cloud gave you, pasted as it is. A service " +
					"account rather than your own account, so an app does not stop working " +
					"the week somebody leaves."},
		},
		Enumerates:       true,
		DestinationLabel: "folder",
		Actions: []Action{
			{Name: "upload", Carries: CarriesFile,
				Summary: "Add a file to the folder its people chose. It never replaces one."},
			tokenAction,
		},
		TextLimit:     1 << 10,
		FileLimit:     25 << 20,
		LeavesResidue: true,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "Google Drive is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app has no Google Drive folder — an administrator can attach " +
				"one on the app's Integrations tab",
			Disabled: "this app's Google Drive is switched off — an administrator can switch " +
				"it back on from the app's Integrations tab",
			TokensOff: "this Google Drive connection does not hand out access tokens — an " +
				"administrator can turn on Let apps get access tokens on it in Settings › Integrations",
		},
	},
	{
		Name:       IntegrationGoogleChat,
		Title:      "Google Chat",
		Summary:    "Post an app's messages into a Google Chat space.",
		ScopeLabel: "workspace",
		ScopeHint: "The service account decides the workspace. An app can reach exactly the " +
			"spaces somebody added the Chat app to — that membership is the boundary, not " +
			"a list kept here.",
		Credentials: []Credential{
			{Key: "service_account", Label: "Service account key", Secret: true, Required: true,
				Help: "The whole JSON file Google Cloud gave you, with the Chat API enabled " +
					"and a Chat app configured on it."},
		},
		Enumerates:       true,
		DestinationLabel: "space",
		Actions: []Action{
			{Name: "post", Carries: CarriesText, Summary: "Post a message the app wrote."},
		},
		TextLimit: 4 << 10,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "Google Chat is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app has no Google Chat space — an administrator can attach " +
				"one on the app's Integrations tab",
			Disabled: "this app's Google Chat is switched off — an administrator can switch " +
				"it back on from the app's Integrations tab",
		},
	},
	{
		Name:       IntegrationDiscord,
		Title:      "Discord",
		Summary:    "Post an app's messages into a Discord channel.",
		ScopeLabel: "server",
		ScopeHint: "The server id. Turn on Developer Mode in Discord, right-click the " +
			"server and Copy Server ID. An app can reach exactly the channels the bot can " +
			"see there — that is the boundary, and removing the bot takes effect at once.",
		Credentials: []Credential{
			{Key: "bot_token", Label: "Bot token", Secret: true, Required: true,
				Help: "From your application's Bot page on the Discord developer portal. It " +
					"needs no privileged intents: Toft posts and never listens."},
		},
		Enumerates:       true,
		DestinationLabel: "channel",
		Actions: []Action{
			{Name: "post", Carries: CarriesText, Summary: "Post a message the app wrote."},
		},
		TextLimit: 2000,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "Discord is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app has no Discord channel — an administrator can attach one " +
				"on the app's Integrations tab",
			Disabled: "this app's Discord is switched off — an administrator can switch it " +
				"back on from the app's Integrations tab",
		},
	},
	{
		Name:  IntegrationPagerDuty,
		Title: "PagerDuty",
		Summary: "Let an app raise an incident when something only it can see goes wrong — " +
			"and close the one it raised.",
		ScopeLabel: "service",
		ScopeHint: "What you call the service this pages. The integration key decides which " +
			"one; this is the label people read here.",
		Credentials: []Credential{
			{Key: "routing_key", Label: "Integration key", Secret: true, Required: true,
				Help: "From an Events API v2 integration on the service you want paged — " +
					"the 32-character key on its Integrations tab, not an API token."},
		},
		// Welded: the key is the service, and there is nothing inside it to
		// choose between.
		Enumerates: false,
		Actions: []Action{
			{Name: "trigger", Carries: CarriesText,
				Summary: "Raise an incident, with a severity and a key that names it."},
			{Name: "resolve", Carries: CarriesText,
				Summary: "Close an incident this app raised. Its keys are namespaced with " +
					"the app, so it can close no other app's."},
		},
		TextLimit: 4 << 10,
		// Nothing on the platform seam yet: what Toft says about an app
		// through this kind is a decision nobody has taken, and turning it on
		// silently in the change that made it possible would be a behaviour
		// change nobody asked for. Empty rather than nil, because a list on the
		// wire is a list.
		PlatformAudiences: []string{},
		Refusals: Refusals{
			NotConnected: "PagerDuty is not connected on this install — an administrator " +
				"connects it in Settings › Integrations",
			NotAttached: "this app cannot page anybody — an administrator can attach " +
				"PagerDuty on the app's Integrations tab",
			Disabled: "this app's PagerDuty is switched off — an administrator can switch it " +
				"back on from the app's Integrations tab",
		},
	},
}

// Lookup returns the kind by name, or nil. Nil is "not an integration this
// platform offers" and is answered as a 400 rather than a 404, because the
// caller named something that does not exist rather than something they may not
// see.
func Lookup(name string) *Kind {
	for i := range Known {
		if Known[i].Name == name {
			return &Known[i]
		}
	}
	return nil
}

// KindNames is every kind's name, for a filter menu or a test.
func KindNames() []string {
	out := make([]string, 0, len(Known))
	for _, k := range Known {
		out = append(out, k.Name)
	}
	return out
}

// KindTitles is every kind's display name, keyed by its name — what a page
// prints where a slug like "gdrive" would otherwise be capitalized into a word
// nobody spells that way.
func KindTitles() map[string]string {
	out := make(map[string]string, len(Known))
	for _, k := range Known {
		out[k.Name] = k.Title
	}
	return out
}

// Action returns the named action on this kind, or nil.
func (k *Kind) Action(name string) *Action {
	for i := range k.Actions {
		if k.Actions[i].Name == name {
			return &k.Actions[i]
		}
	}
	return nil
}

// Capability is the string an app asks about and the gates are called with:
// the kind, a dot, the action.
func (k *Kind) Capability(action string) string { return k.Name + "." + action }

// Capabilities is every capability string this kind offers.
func (k *Kind) Capabilities() []string {
	out := make([]string, 0, len(k.Actions))
	for _, a := range k.Actions {
		out = append(out, k.Capability(a.Name))
	}
	return out
}

// IssuesTokens reports whether a connection of this kind can hand an app its
// access token: the three file kinds, whose far-side grant can be one folder or
// one shared drive.
func (k *Kind) IssuesTokens() bool { return k.Action(ActionToken) != nil }

// Serves reports whether this kind carries the named platform audience.
func (k *Kind) Serves(audience string) bool {
	for _, a := range k.PlatformAudiences {
		if a == audience {
			return true
		}
	}
	return false
}

// Secrets reports whether any credential field of this kind is a secret, which
// is what decides whether the connection form has a write-only field on it.
func (k *Kind) Secrets() bool {
	for _, c := range k.Credentials {
		if c.Secret {
			return true
		}
	}
	return false
}

// IntegrationOf is the kind a capability belongs to: "email.send" → "email".
// One function so the ledger, the gates and the UI cannot disagree about which
// is which.
func IntegrationOf(capability string) string {
	if i := strings.IndexByte(capability, '.'); i > 0 {
		return capability[:i]
	}
	return capability
}

// ActionOf is the action half: "email.send" → "send".
func ActionOf(capability string) string {
	if i := strings.IndexByte(capability, '.'); i > 0 && i+1 < len(capability) {
		return capability[i+1:]
	}
	return ""
}

// TextAction is the kind's text-carrying action, or nil.
func (k *Kind) TextAction() *Action { return k.actionCarrying(CarriesText) }

// FileAction is the kind's file-carrying action, or nil.
func (k *Kind) FileAction() *Action { return k.actionCarrying(CarriesFile) }

func (k *Kind) actionCarrying(c Carries) *Action {
	for i := range k.Actions {
		if k.Actions[i].Carries == c {
			return &k.Actions[i]
		}
	}
	return nil
}
