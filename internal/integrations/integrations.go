// Package integrations is the seam between the send queue and the eight things
// a send can go out through.
//
// It exists because `deliverAppSend` used to be a `switch` on the integration
// name with an arm per transport, and eight arms is where a switch stops being
// readable and starts being a place a link goes missing. What replaces it is a
// registry — one entry per kind, built once at startup — and the shapes below
// are what an entry has to satisfy.
//
// **Nothing here knows about a specific provider, and nothing here talks to
// one.** The transports live in the packages beneath this one; `internal/slack`
// and `internal/mail` stay exactly where they are and get adapters at the
// wiring layer, because moving working code to make a directory listing
// symmetrical is a diff that reviews as noise and rebases as conflict.
//
// **The credential is decrypted above this line and never below it.** A Target
// arrives holding plaintext, built at the point of use by the one caller that
// holds the encryptor, and a transport neither reads the database nor knows
// what a crypto record is.
package integrations

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"toft/internal/appint"
	"toft/internal/database"
)

// Target is everything a transport is told about where a send goes.
type Target struct {
	// Connection is the row, including the scope it is welded to. Its Secret
	// field is the stored ciphertext and a transport must not read it — the
	// plaintext is in Credentials.
	Connection database.IntegrationConnection
	// Credentials are the kind's secret fields, decrypted, at the point of use.
	Credentials map[string]string
	// Places are the far-side places an administrator pasted onto the
	// connection and the far side verified (integration_places). Empty for
	// every kind that enumerates by asking its far side one question.
	Places []database.IntegrationPlace
	// Destination is where inside the scope this send lands: the app's own
	// choice, already resolved through the connection's default. Empty for a
	// welded kind, which has exactly one destination by construction.
	Destination database.Destination
}

// Config reads one non-secret credential field.
func (t Target) Config(key string) string { return t.Connection.Config[key] }

// Credential reads one secret field.
func (t Target) Credential(key string) string { return t.Credentials[key] }

// Send is one outbound message, in the shape every transport can read.
//
// It is deliberately not a union of eight provider payloads. What the platform
// promises an app is a message, optionally one file, and an attribution it
// cannot forge; a transport turns that into whatever its provider wants, and a
// field here that only one provider uses is a field the other seven have to
// ignore — which is how a seam rots.
type Send struct {
	// Action is the catalog action: "post", "file", "upload", "trigger".
	Action string

	Text    string
	Title   string
	Comment string

	// Subject and the recipient lists are mail's, and mail's alone. They are
	// here rather than in a mail-shaped side channel because the alternative is
	// a second Send type, and two message shapes is the thing internal/notify
	// argues against for notifications and this package argues against for
	// sends.
	Subject string
	HTML    string
	To      []string
	CC      []string
	BCC     []string

	// File is the one attachment a file action carries. Nil for a text action.
	File *appint.File
	// Files is mail's list, which is the one transport that takes more than one.
	Files []appint.File

	// Severity and DedupKey are the on-call vocabulary: what a page says about
	// itself and what closes it. Empty everywhere else.
	Severity string
	DedupKey string

	// AppID is the app this send belongs to. It is here for two reasons —
	// namespacing an on-call dedup key, so an app can only resolve what it
	// triggered, and resolving mail's reply-to to the app's owner when its
	// people set none — and a transport that used it for anything else would
	// be reintroducing an id into a layer that otherwise has none.
	AppID string

	// AppName and AppURL are the frame that says an application sent this, not
	// the platform. Composed above this line and never reachable from anything
	// app code sent — an app never speaks in the platform's voice.
	AppName string
	AppURL  string
	// Platform is the install's name, which every transport's attribution
	// names as the platform the app is on ("Sent by Payroll on Staging").
	// Set by the platform from the install's setting — never from anything app
	// code sent — and "" reads "Toft". PlatformName is how a transport reads it.
	Platform string
	// FromPlatform marks a notification the platform itself sends — the
	// catalog poster and an administrator's connection test — which is named
	// once rather than as itself on itself. Set only by those two callers,
	// never on an app's send.
	FromPlatform bool

	// SendID is the ledger row, for a transport that wants to make its request
	// idempotent on the far side.
	SendID string
}

// Result is what a transport reports back beyond success.
type Result struct {
	// Placement is set by a kind whose descriptor says it leaves residue, and
	// must be set by every one of them: it is the record that this app put this
	// object into that system, and it is the only durable thing on this platform
	// designed to outlive the app it names.
	Placement *Placement
	// Destination lets a transport correct the label the ledger recorded, where
	// the far side knows a better name for where the message landed.
	Destination string
}

// Placement is an object that now exists in somebody else's system.
type Placement struct {
	ItemID  string
	ItemURL string
	Name    string
	Bytes   int64
	Type    string
	SHA256  string
}

// Transport is one kind's outbound half, and the only thing a kind must have.
type Transport interface {
	Send(ctx context.Context, t Target, s Send) (Result, error)
}

// Enumerator is the second half, and only a kind whose descriptor says
// `Enumerates` may implement it.
//
// The two must agree, and a test says so: a kind that claims to enumerate and
// has no enumerator would render a picker that always errors, and one that
// enumerates without claiming it would hide a menu somebody built.
type Enumerator interface {
	Destinations(ctx context.Context, t Target) ([]database.Destination, error)
}

// Verifier proves a connection at *save* time and returns the scope's own label.
//
// Save-time rather than send-time is the whole point. An administrator who
// pastes a site they cannot read, or a webhook URL that resolves inward, should
// learn it while looking at the form — not at 2am, in a queue worker, in a log
// nobody is reading. It is optional because not every kind can prove anything
// without sending something, and a "verification" that posts a message into a
// channel is a test button wearing a disguise.
type Verifier interface {
	Verify(ctx context.Context, t Target) (label string, err error)
}

// ScopeSplitter is a kind whose scope, as an administrator pastes it, carries
// its own credential — a Teams Workflows URL, whose `sig` query parameter is
// what lets anybody holding it post. SplitScope returns the part that is safe
// to store in the clear, show and audit, and the part that is sealed with the
// kind's other secret fields. The transport puts them back together at the
// point of use, and nothing else does.
type ScopeSplitter interface {
	SplitScope(raw string) (scope string, secrets map[string]string, err error)
}

// PlatformName is the install's name for an attribution line, or "Toft".
func (s Send) PlatformName() string {
	if v := strings.TrimSpace(s.Platform); v != "" {
		return v
	}
	return "Toft"
}

// Attribution is the line every chat transport closes an app's message with:
// "<shown> on <platform>", where shown is the sender as the transport renders
// it — a name, or a name inside a link. A platform notification, whose sender
// *is* the install, is named once rather than as itself on itself.
//
// **Told by FromPlatform, never by comparing names.** An app named after the
// install posted in the platform's voice, with no "on <platform>" after it
// (security review 2026-11-10, F-3); an app's send always carries the frame.
func (s Send) Attribution(shown string) string {
	if s.FromPlatform {
		return shown
	}
	return shown + " on " + s.PlatformName()
}

// RedactURL cuts the URL in a transport error down to its scheme and host.
//
// For an upload URL that is its own credential — a Graph upload session's query
// string, a Slack upload ticket in the path — net/http's *url.Error text is the
// credential, and that text reaches the send ledger, the app's events, the log
// and the app itself. url.Error strips only a password in the authority. Err is
// kept, so errors.Is(err, context.DeadlineExceeded) and the retry
// classification still see what happened.
func RedactURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	short := ""
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		short = u.Scheme + "://" + u.Host
	}
	return &url.Error{Op: ue.Op, URL: short, Err: ue.Err}
}
