package api

import (
	"log/slog"
	"strings"

	"toft/internal/database"
	mailer "toft/internal/mail"
	"toft/internal/notify"
)

// Telling somebody an app has been shared with them.
//
// **Opt-in per share, and that is the whole shape of it.** Every other
// notification in this product is sent because something happened; this one is
// sent because the person doing the sharing ticked a box. So it carries their
// name and their words, it is sent exactly once, and there is nothing to
// unsubscribe from.
//
// Per the notification invariant the send is optional and never load-bearing:
// the grant is already written and already in force by the time any of this
// runs. Nothing here returns an error, and nothing here can fail the share.
//
// **Email only, and structurally so.** `internal/slack` defines no rendering
// for it, so even a call that reached the poster would be dropped. The reason
// is the one the publisher-decision messages give: this is about a *person*
// rather than about an app, so it has no channel — and posting "Alice gave Bob
// access to payroll" into a team channel turns one person's decision into an
// announcement to everybody who can read it.

// Why a share was not notified. The empty string means it was.
//
// These are reported back to the caller rather than logged and forgotten,
// because every one of them is something the person who ticked the box would
// otherwise assume did not happen. A share that silently sends nothing is worse
// than one that never offered to.
const (
	// notifySkipNotAPerson is a group or a service account. A group is a set of
	// humans and not an actor, and this notice is one person writing to
	// another; mailing a group address on somebody's behalf is a different
	// feature with a different audience.
	notifySkipNotAPerson = "not_a_person"
	// notifySkipNotNew is a role change or a re-grant. The message says "gave
	// you access", which is not true the second time, and a viewer promoted to
	// editor being told they have been given access reads as a mistake.
	notifySkipNotNew = "not_new"
	// notifySkipSelf is somebody granting themselves — an administrator, in
	// practice. Nothing is gained by mailing them what they just did.
	notifySkipSelf = "self"
	// notifySkipNoAddress is a person with no email address on file. Common
	// enough to be worth saying out loud: `users.email` is provider-supplied
	// and an install signing in through a directory that asserts none has it
	// blank for everybody.
	notifySkipNoAddress = "no_address"
	// notifySkipEmailOff is email not configured, or the app audience switched
	// off. The UI does not offer the box in that state; a caller that asks
	// anyway is told rather than quietly ignored.
	notifySkipEmailOff = "email_off"
	// notifySkipNotAccess is a grant at the `list` rung. The message this
	// function sends says somebody gave you access to an app and here is the
	// link — and the rung is the one grant that gives no access and whose link
	// leads to a refusal. There is a notification worth sending about a list
	// grant, but it is not this one and it is not written yet, so nothing is
	// sent rather than a promise the app will not keep.
	notifySkipNotAccess = "not_access"
)

// notifyGrantShared sends the share notice and reports whether it did, and why
// not.
//
// The refusals are checked in the order somebody would explain them, which is
// also cheapest-first: what kind of principal this is, whether this is actually
// a new share, and only then anything that costs a query.
func (s *Server) notifyGrantShared(
	app *database.App, principalType, principalID, role, note string, existed bool, actorID string,
) (bool, string) {
	if principalType != database.PrincipalTypeUser {
		return false, notifySkipNotAPerson
	}
	if existed {
		return false, notifySkipNotNew
	}
	if principalID == actorID {
		return false, notifySkipSelf
	}
	if role == database.ObjectRoleList {
		return false, notifySkipNotAccess
	}
	if s.mailer == nil || !s.mailer.AppNotificationsEnabled() {
		return false, notifySkipEmailOff
	}

	target, err := s.db.GetUser(principalID)
	if err != nil || target == nil {
		slog.Error("share notify: loading the recipient", "user", principalID, "error", err)
		return false, notifySkipNoAddress
	}
	if target.Email == "" {
		return false, notifySkipNoAddress
	}

	base := s.mailer.BaseURL()
	msg, err := mailer.Render("app_shared", mailer.TemplateData{
		BaseURL: base,
		// The sharer, as their colleagues know them. RequesterName is the
		// field every person-facing template already renders a human by; the
		// share notice is the first one where that person is the *sender*
		// rather than the subject, which changes nothing about what it holds.
		RequesterName: s.displayNameOf(actorID),
		AppName:       appDisplayName(app),
		Role:          grantRoleWord(role),
		Note:          strings.TrimSpace(note),
		// A pointer, not a credential: the recipient signs in normally and is
		// authorized by the grant that was just written. The **id**, for the
		// reason on notify.AppPageURL — the slug this used to carry addresses
		// the running app on the apps origin, never its page here.
		Link: notify.AppPageURL(base, app.ID),
	})
	if err != nil {
		slog.Error("share notify: rendering the message", "error", err)
		return false, notifySkipEmailOff
	}
	msg.To = []string{target.Email}
	s.mailer.SendAsync(msg)
	return true, ""
}

// displayNameOf is who an actor is to somebody reading mail: their name where
// the provider asserted one, their login otherwise, and the id only if the row
// has gone missing — which is not a case worth failing a send over.
func (s *Server) displayNameOf(userID string) string {
	u, err := s.db.GetUser(userID)
	if err != nil || u == nil {
		return userID
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Login
}

// appDisplayName is the app as its people call it, falling back to the slug —
// the same rule the failure notifications use.
func appDisplayName(app *database.App) string {
	if app.Name != "" {
		return app.Name
	}
	return app.Slug
}

// grantRoleWord is a role as it appears mid-sentence. Deliberately not the
// stored constant: "gave you access to Payroll as viewer" is what interpolating
// that directly would say, and the article is what makes it a sentence.
// The `list` rung has no word here on purpose: notifyGrantShared refuses to
// send for it before this is reached, because the message it would appear in
// promises access the rung does not confer.
func grantRoleWord(role string) string {
	switch role {
	case database.ObjectRoleEditor:
		return "an editor"
	case database.ObjectRoleViewer:
		return "a viewer"
	default:
		return ""
	}
}
