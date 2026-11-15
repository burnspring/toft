package api

import (
	"log/slog"
	"strings"

	"toft/internal/database"
	mailer "toft/internal/mail"
	"toft/internal/notify"
)

// Email and channel posts that accompany the app access-request workflow.
//
// The twin of promotion_notify.go one axis over, and everything written there
// holds here: every function is best-effort by construction, none returns an
// error, none is called before the response is written, and every link is a
// pointer rather than a credential — there is no one-click approve, because a
// mailbox is not an authorization boundary.
//
// **What is different is who is told.** A publisher request is about the
// install and goes to its administrators; this is about one app and goes to
// that app's owners — the only people who can decide it, since
// ActionApproveAccessRequest is owner-only. An administrator can approve one
// through the short-circuit in Authorize and is still not mailed: they are not
// who it was routed to, and a request about one of two hundred apps arriving in
// every administrator's inbox is how a useful notification becomes a filtered
// one.
//
// Until these existed the whole feature rested on somebody thinking to open the
// Access tab of the right app. The queue is still the record — mail is
// optional and never load-bearing, and an install with no relay loses the nudge
// and nothing else.

// notifyAccessRequested tells an app's owners that somebody is waiting on them.
//
// Only for a request that is actually pending. A request auto-denied at
// creation — a system viewer asking for the editor role — is decided before it
// is stored and has nobody waiting on it; the requester is looking at the
// refusal in the response, and mailing an owner about a decision the platform
// already made would be asking them to act on a closed row.
func (s *Server) notifyAccessRequested(app *database.App, req *database.AccessRequest, requester *database.User) {
	if app == nil || req == nil || requester == nil {
		return
	}
	if req.Status != database.AccessReqStatusPending {
		return
	}

	name := requester.Name
	if name == "" {
		name = requester.Login
	}
	data := notify.TemplateData{
		AppName:        appDisplayName(app),
		RequesterName:  name,
		RequesterLogin: requester.Login,
		RequesterEmail: requester.Email,
		Role:           req.RequestedRole,
		// The requester's own words, where they wrote any. The one field of
		// this message somebody typed, and it is what an owner answers.
		Note: strings.TrimSpace(req.Reason),
	}

	// The channel first and independently of mail, for the reason the failure
	// notices are posted that way: a post is addressed to a destination rather
	// than to people, so an app whose owner has no address on file still
	// reaches the team that runs it. AudienceApp with the app's id, so this
	// lands in the app's own channel and never in the install default.
	s.postPlatform("access_requested", app.ID, func(base string) notify.TemplateData {
		d := data
		d.BaseURL = base
		d.Link = accessRequestLink(base, app.ID)
		return d
	})

	if s.mailer == nil {
		return
	}
	people, err := s.db.ListAppDeciders(app.ID)
	if err != nil {
		slog.Error("access notify: listing the app's owners", "app_id", app.ID, "error", err)
		return
	}
	if len(people) == 0 {
		// A warning rather than silence, and the same one the failure notices
		// raise: somebody is waiting on a decision nobody will be told about,
		// and the fix — an address on the owner, or on the owning group — is
		// something an administrator can make.
		slog.Warn("somebody asked for access to an app with nobody to email — no owner has an "+
			"email address on file, and no owning group has one either",
			"app", app.Slug, "requester", requester.Login)
		return
	}

	data.BaseURL = s.mailer.BaseURL()
	data.Link = accessRequestLink(s.mailer.BaseURL(), app.ID)
	msg, err := mailer.Render("access_requested", data)
	if err != nil {
		slog.Error("access notify: rendering the request email", "error", err)
		return
	}
	to := make([]string, 0, len(people))
	for _, p := range people {
		to = append(to, p.Address)
	}
	msg.To = to
	s.mailer.SendAsync(msg)
}

// notifyAccessDecided tells the requester what was decided.
//
// Email only, and structurally so: internal/notify declares both outcomes
// mail-only, so even a call that reached a poster would be dropped. The *ask*
// is about an app and belongs in the app's channel; the answer is about one
// person's standing, and "your request for the payroll dashboard was declined,
// here is why" posted into the team room is a private refusal read out loud.
//
// grantedRole is what the requester now holds, which is not always what they
// asked for — approving never takes access away, so somebody who already held
// more keeps it. Empty on a denial, where nothing was granted.
func (s *Server) notifyAccessDecided(app *database.App, reqID, grantedRole string, approved bool) {
	if s.mailer == nil || app == nil {
		return
	}
	// Read after the decision, so the reason this carries is the one that was
	// just written rather than the requester's own note it replaced.
	req, err := s.db.GetAccessRequest(reqID)
	if err != nil {
		slog.Error("access notify: loading the request", "request_id", reqID, "error", err)
		return
	}
	if req == nil || req.RequesterID == "" {
		return
	}
	target, err := s.db.GetUser(req.RequesterID)
	if err != nil {
		slog.Error("access notify: loading the requester", "user", req.RequesterID, "error", err)
		return
	}
	if target == nil || target.Email == "" {
		slog.Debug("access request decided, the requester has no email address on file",
			"request_id", reqID)
		return
	}

	template := "access_denied"
	if approved {
		template = "access_approved"
	}
	base := s.mailer.BaseURL()
	msg, err := mailer.Render(template, mailer.TemplateData{
		BaseURL: base,
		AppName: appDisplayName(app),
		// Who decided, as their colleagues know them — approver_id is written
		// at decision time, so on a group-owned app this is the member who
		// actually acted rather than the group they acted for.
		ApproverName: s.displayNameOf(req.ApproverID),
		Role:         grantedRole,
		Reason:       req.Reason,
		// A pointer, not a credential: the app's page, which the grant just
		// written is what authorizes them to open.
		Link: notify.AppPageURL(base, app.ID),
	})
	if err != nil {
		slog.Error("access notify: rendering the decision email", "template", template, "error", err)
		return
	}
	msg.To = []string{target.Email}
	s.mailer.SendAsync(msg)
}

// accessRequestLink deep-links a decider at the queue they are being asked to
// work: the app's Access tab, where the request is waiting.
//
// The tab is a path segment rather than a query parameter — `/apps/:id/:tab` —
// and it is `access` rather than `sharing`, which is what the tab was called
// before ADR §33 and is still redirected from. The queue's own section is the
// anchor, so the page lands on it rather than at the top of a tab holding the
// grants table as well.
//
// **The id, not the slug** — notify.AppPageURL says why at length, and this
// button is what made it worth saying: an owner told somebody was waiting on
// them followed it to their own app's front door on a single-origin install,
// and to a redirect to the apps origin on a split one. Never to the queue.
func accessRequestLink(base, appID string) string {
	page := notify.AppPageURL(base, appID)
	if page == "" {
		return ""
	}
	return page + "/access#access-requests"
}
