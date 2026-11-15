// Package appint is the seam every app-initiated send passes through.
//
// It holds exactly two mechanisms and deliberately no transport at all:
//
//   - Check — the four gates, in one place, returning a refusal already worded
//     for a person.
//   - Sender — the bounded queue and the ledger writes around it.
//
// The transports stay where they belong. slack renders blocks and posts to a
// channel; mail renders MIME and takes recipients; neither imports the other,
// and there is no Channel interface here for them to implement. internal/notify
// argues that case for platform-generated notifications and the argument still
// holds — what app-initiated sends share is not the rendering, it is the gate
// chain and the record.
//
// **What kind of seam this is, stated because it was previously overstated.**
// This is an *asynchronous-send* seam: something is accepted, recorded as
// queued, handed to a transport out of band, and completed in the ledger. Every
// member of it has that shape.
//
// The file used to end by saying that a third integration should add a
// transport and a capability row and touch nothing here — offered as the test
// of whether the seam is real. The third integration arrived and did not take
// that test: inference was request/response, synchronous, gated twice more than
// these four and recorded in its own ledger, so the claim went unexercised for
// as long as it existed. It has since been removed, and a comment asserting a
// property nobody has run is the kind of thing the next person builds on.
//
// The integration catalog is what exercises it. Every kind it adds is
// asynchronous, so the shape holds — but it is being proven now, not reused.
package appint

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"toft/internal/database"
)

// Capabilities. A capability is what /app/self reports, what toft.app.can() takes
// and what Check is called with.
//
// The file capabilities are **not separately granted**. They name an action —
// which is what the ledger's `action` column records, and what an app asks
// about by name — but they are gated exactly as their parent is: an app that
// may post to Slack may post a file, and an app that may send mail may attach
// one. The earlier design made each a switch of its own on the theory that a
// text post leaks a sentence and a file moves a dataset. That is true and it
// does not survive the arithmetic: an app that may mail any address on the
// internet can already put 768 KiB of body in front of it, so the second switch
// bought a multiple rather than a boundary — and it cost every administrator a
// second decision, and every app author a capability that was on in staging and
// off in production. The control over app sends is the ledger, which records an
// attachment by name, size, type and digest either way.
const (
	CapSlackPost   = "slack.post"
	CapSlackFile   = "slack.file"
	CapEmailSend   = "email.send"
	CapEmailAttach = "email.attach"

	// CapViewerIdentity is the app's ability to ask who is looking at it, and
	// CapViewerIdentityEmail is whether that answer will carry an address.
	//
	// Two strings for what an administrator sets with one switch, and the second
	// is *derived* rather than configured: the address is withheld on a public
	// app, whose visitors did not consent to a mailing list by visiting. That
	// makes the absence something an app is told at startup instead of a null it
	// meets at runtime — which is the argument for having /app/self at all, and
	// the reason this is not a second switch an administrator has to find.
	CapViewerIdentity      = "viewer.identity"
	CapViewerIdentityEmail = "viewer.identity.email"
)

// ViewerCapabilities is the viewer-identity half of /app/self's list.
//
// Pure, and takes the two facts rather than a store, because both live on the
// app row the caller has already loaded — and because the rule this encodes is
// worth being able to test without a database. Some frameworks never get the
// capability, for one reason: they run no commands, so they have no process to
// hold an app token and no way to make the call, and their browser-side
// JavaScript must never be handed one. Absent rather than failing at runtime.
//
// `static` is a folder of files. `notebook` is refused for exactly the same
// reason and it is the row worth pausing on, because "a notebook is Python" is
// the intuition somebody will bring: a notebook is Python at *build* time and a
// file server at run time, and this question is about run time. `rmarkdown` and
// `quarto` are refused on identical terms — R at build time, a file server
// after — while `shiny` and `plumber` are R apps with a process and are granted
// it. A `marimo` app has a process like any other server framework and is
// granted it too.
//
// So it is four frameworks now rather than two, and the rule is unchanged: a
// workload that runs no commands has no process to hold an app token and no way
// to make the call, and its browser-side JavaScript must never be handed one.
func ViewerCapabilities(enabled bool, visibility, framework string) []string {
	if !enabled || !FrameworkHasAProcess(framework) {
		return nil
	}
	if visibility == "public" {
		return []string{CapViewerIdentity}
	}
	return []string{CapViewerIdentity, CapViewerIdentityEmail}
}

// FrameworkHasAProcess reports whether a framework's *workload* runs the app's
// own code, which is the only question ViewerCapabilities is really asking.
//
// Exported so the detector's own constants can be held against it from the
// package that defines them, rather than this list being restated as strings in
// a test that would still pass if a fifth document framework were added.
func FrameworkHasAProcess(framework string) bool {
	switch framework {
	case "static", "notebook", "rmarkdown", "quarto":
		return false
	}
	// A batch app runs commands and answers no requests, so there is never a
	// viewer to identify.
	if strings.HasPrefix(framework, "batch-") {
		return false
	}
	return true
}

// IdempotencyWindow is how long a key is remembered. Long enough for a daily or
// weekly sender to be safely re-run after an outage; short enough that a key
// like "weekly-report-2026-W32" is not burnt forever.
const IdempotencyWindow = 24 * time.Hour

// Store is the slice of the database this package needs, declared here rather
// than taken as a *database.DB so a test needs no Postgres — the events.Store
// precedent.
type Store interface {
	// ResolveAttachment answers gates 1 to 3 for one (app, kind) in one read.
	// Three gates and one call because they are three readings of the same two
	// rows, and splitting them was what let every integration write its own
	// version of the chain.
	ResolveAttachment(appID, kind string) (database.Attachment, error)

	InsertSend(*database.IntegrationSend) error
	CompleteSend(id, status, errMsg string) error
	FindSendByIdempotencyKey(appID, key string, window time.Duration) (*database.IntegrationSend, error)

	// GetApp is read by the send worker for one fact, the lock: a send queued
	// before an app was locked must not go out after it (security review
	// 2026-11-15, LSA-3). Nil with no error is an app that no longer exists.
	GetApp(id string) (*database.App, error)
}

// Decision is the answer Check gives. A refusal carries the words a person
// reads, because the standing rule on this surface is that a refusal names the
// remedy: "email is not enabled for this app — an administrator can enable it
// on the app's Integrations tab" is a fixable situation, and 403 is not.
type Decision struct {
	Allowed bool
	Reason  string
	Code    string
	// Status is the HTTP status a handler should answer with. 403 for a
	// capability that is off, 409 for an app that is stopped — different things
	// with different fixes.
	Status int
	// Hint is the next step, in the envelope's own field: each gate's fix is
	// fixed, so a refusal that held it only in its sentence left the field an
	// agent acts on empty (drills 2026-10-28 evening #5).
	Hint string
}

func allow() Decision { return Decision{Allowed: true} }

func deny(code, reason string, status int) Decision {
	return Decision{Reason: reason, Code: code, Status: status}
}

// withHint is d with its next step.
func (d Decision) withHint(hint string) Decision {
	d.Hint = hint
	return d
}

// Check runs the four gates, in order, and is the only place any of them is
// enforced.
//
//  1. The install offers the integration at all.
//  2. An administrator attached it to this app.
//  3. The attachment is enabled.
//  4. The app is not stopped.
//
// The token action has one gate more, between 3 and 4: the connection lets apps
// hold its token (issue_tokens).
//
// Gate 3 is per integration and not per action: posting a file is gated as
// posting is, attaching is gated as sending is. See the capability constants
// for why the per-action switch was removed.
//
// Note what is *not* here. The app owner's notify_failures switch governs
// failure notices and is applied in events.Recorder; reusing it would silence a
// business message with a switch somebody threw about alert noise. The
// notify_app and notify_admin audience switches govern platform chatter; an
// administrator who stops Toft emailing app authors about their deploys is
// not asking to shut down a process that mails a finance team every Monday.
//
// The stopped check is last on purpose even though it is the cheapest: an app
// that is off *and* has no email attached should hear about the capability,
// because that is the one an administrator has to act on, and the app being off
// is usually the caller's own doing.
// UnknownCapabilityHint is the next step for a capability the catalog does not
// have: GET /api/v1/app/self as the list, and the shape spelled from the
// catalog — a send's integration.action, and a file integration's access token
// as <integration>.token, which "kind.action, like slack.post" left an author
// asking for sharepoint by guess (D-15).
func UnknownCapabilityHint() string {
	var tokens []string
	for i := range Known {
		if Known[i].IssuesTokens() {
			tokens = append(tokens, Known[i].Capability(ActionToken))
		}
	}
	hint := "name one of the capabilities GET /api/v1/app/self lists for this app — the integration, " +
		"a dot, the action: slack.post, email.send, sharepoint.upload"
	if len(tokens) > 0 {
		hint += "; a file integration's access token is " + strings.Join(tokens, ", ")
	}
	return hint
}

// UnknownTokenKindHint is UnknownCapabilityHint for the token route, which
// takes the integration's kind alone (POST /api/v1/app/tokens/sharepoint, the
// SDKs' accessToken("sharepoint")): naming the capabilities there sent an
// author to "sharepoint.token.token" (TOFT-BB-008).
func UnknownTokenKindHint() string {
	var kinds []string
	for i := range Known {
		if Known[i].IssuesTokens() {
			kinds = append(kinds, Known[i].Name)
		}
	}
	return "pass the integration's kind alone, like accessToken(\"sharepoint\") — the kinds that issue tokens are " +
		strings.Join(kinds, ", ")
}

func Check(store Store, appID, appStatus, capability string) (Decision, error) {
	kind := Lookup(IntegrationOf(capability))
	if kind == nil || kind.Action(ActionOf(capability)) == nil {
		return deny("unknown_capability",
			fmt.Sprintf("%q is not a capability this platform offers", capability),
			http.StatusBadRequest).
			withHint(UnknownCapabilityHint()), nil
	}

	attachment, err := store.ResolveAttachment(appID, kind.Name)
	if err != nil {
		return Decision{}, err
	}
	switch {
	case !attachment.Connected:
		return deny(kind.Name+"_off", kind.Refusals.NotConnected, http.StatusForbidden).
			withHint("ask an administrator to connect " + kind.Title + " in Settings › Integrations"), nil
	case !attachment.Attached:
		return deny(kind.Name+"_not_attached", kind.Refusals.NotAttached, http.StatusForbidden).
			withHint("ask an administrator to enable " + kind.Title + " on this app's Integrations tab"), nil
	case !attachment.Enabled:
		return deny(kind.Name+"_disabled", kind.Refusals.Disabled, http.StatusForbidden).
			withHint("ask an administrator to switch " + kind.Title + " back on from this app's Integrations tab"), nil
	case ActionOf(capability) == ActionToken && !attachment.IssueTokens:
		// The one action that is not granted with the attachment alone: a token
		// reaches the connection's whole grant, which an attachment made for
		// "upload into this folder" did not consent to.
		return deny(kind.Name+"_tokens_off", kind.Refusals.TokensOff, http.StatusForbidden).
			withHint("ask an administrator to let apps have access tokens on the " + kind.Title +
				" connection in Settings › Integrations"), nil
	}

	if appStatus == database.AppStatusStopped {
		// Deliberately not sleeping (ADR §16): a request can wake a slept app
		// and the very next thing it does may be to send.
		return deny("app_stopped",
			"this app is stopped — a stopped app does nothing, including notify. "+
				"Start it to send again.", http.StatusConflict).
			withHint("start it — `toft start`, or Start on its page"), nil
	}

	return allow(), nil
}

// Capabilities is what an app may actually do right now, for /app/self.
//
// It reports attached-or-not and never *why* not. The reason is an
// administrator's business, and an app is not a principal that gets told about
// the install's configuration — the same line /system/settings draws by 404ing
// to everyone who is not an administrator. An app that asks and is told "no"
// can render "ask an administrator" on a screen, which is the whole point of
// asking at startup rather than discovering it at 2am.
func Capabilities(store Store, appID string) ([]string, error) {
	out := []string{}
	for i := range Known {
		kind := &Known[i]
		attachment, err := store.ResolveAttachment(appID, kind.Name)
		if err != nil {
			return nil, err
		}
		if attachment.Connected && attachment.Attached && attachment.Enabled {
			// Every action of the kind. The file capability is still named — an
			// app that asks can("slack.file") gets a straight answer rather than
			// having to know the two collapsed — but it is never granted
			// separately. The token is the exception, and it is the
			// connection's switch rather than a per-app one: listed only while
			// Check would let it through.
			for _, c := range kind.Capabilities() {
				if ActionOf(c) == ActionToken && !attachment.IssueTokens {
					continue
				}
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// TokenPrefix is what an app token starts with. Deliberately not a prefix of
// "toft_" or "toftu_": all three credential classes must stay distinguishable by a
// prefix test, because they point in different directions and a route reachable
// by one must never be reachable by another.
const TokenPrefix = "tofta_"

// MintAppToken generates one deployment's outbound credential.
//
// Returns the secret (which is injected into the workload and never stored),
// its SHA-256 (which is stored and is all the platform ever compares against),
// and a short visible prefix for identifying a row without it.
func MintAppToken() (secret, hash, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", err
	}
	secret = TokenPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(secret))
	return secret, hex.EncodeToString(sum[:]), secret[:8], nil
}
