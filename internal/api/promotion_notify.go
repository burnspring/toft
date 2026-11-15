package api

import (
	"log/slog"
	"net/url"

	"toft/internal/database"
	mailer "toft/internal/mail"
	"toft/internal/notify"
)

// Email that accompanies the publisher-request workflow.
//
// Every function here is best-effort by construction. The request has already
// been recorded and is already visible in the UI by the time any of them runs,
// so a relay that is down costs a nudge and nothing else — which is why none of
// them return an error and why none of them are called before the response is
// written.
//
// The links are pointers, not credentials. There is no one-click approve token:
// an administrator following the link signs in normally and is authorized
// normally. A mailbox is not an authorization boundary, and a link that
// promoted someone just by being clicked would make it one.

// notifyPromotionRequested tells the administrators that someone is waiting on
// them — by email, in the Slack default channel, or both, depending on what is
// configured.
//
// The Slack post is deliberately not conditional on the mail path succeeding.
// An install with no relay and a connected workspace is a supported shape, and
// the two channels answer to their own switches.
func (s *Server) notifyPromotionRequested(req *database.AccessRequest, requester *database.User) {
	if req == nil || requester == nil {
		return
	}

	{
		name := requester.Name
		if name == "" {
			name = requester.Login
		}
		// AudienceAdmin, so this goes to the install's default channel and
		// never to an app's — hence the empty app id.
		s.postPlatform("promotion_requested", "", func(base string) notify.TemplateData {
			return notify.TemplateData{
				BaseURL:        base,
				RequesterName:  name,
				RequesterLogin: requester.Login,
				RequesterEmail: requester.Email,
				Link:           base + "/admin/publisher-requests?request=" + url.QueryEscape(req.ID),
			}
		})
	}

	if s.mailer == nil {
		return
	}
	// Asked of the mailer rather than the database, because the answer depends on
	// the admin group address: with one set there is somewhere to send even when
	// no administrator has an address on file. The mailer re-addresses the
	// message itself, so this decides only whether the warning below is warranted.
	to, err := s.mailer.AdminRecipients()
	if err != nil {
		slog.Error("promotion notify: listing admin recipients", "error", err)
		return
	}
	if len(to) == 0 {
		// Worth a warning rather than silence: a request nobody is told about
		// still sits in the queue, but only if someone thinks to look.
		slog.Warn("publisher request created, but no administrator has an email address on file and no admin group address is set",
			"request_id", req.ID, "requester", requester.Login)
		return
	}

	name := requester.Name
	if name == "" {
		name = requester.Login
	}
	msg, err := mailer.Render("promotion_requested", mailer.TemplateData{
		BaseURL:        s.mailer.BaseURL(),
		RequesterName:  name,
		RequesterLogin: requester.Login,
		RequesterEmail: requester.Email,
		Link:           s.publisherRequestLink(req.ID),
	})
	if err != nil {
		slog.Error("promotion notify: rendering request email", "error", err)
		return
	}
	msg.To = to
	s.mailer.SendAsync(msg)
}

// notifyPromotionDecided tells the requester what was decided. A denial carries
// the administrator's reason, because "no" without a reason is the version of
// this workflow people route around by asking in chat instead.
//
// Email only, and structurally so: internal/slack defines no rendering for
// either outcome, so even a call that reached the poster would be dropped.
// These messages are about a *person* rather than an app — they have no app and
// therefore no channel, and publishing "your publisher access was declined,
// here is the administrator's reason" into a team channel would turn a private
// refusal into an announcement.
func (s *Server) notifyPromotionDecided(reqID string, approved bool) {
	if s.mailer == nil {
		return
	}
	detail, err := s.db.GetPromotionRequestDetail(reqID)
	if err != nil {
		slog.Error("promotion notify: loading request", "request_id", reqID, "error", err)
		return
	}
	if detail == nil {
		return
	}
	if detail.RequesterEmail == "" {
		slog.Debug("promotion decided, requester has no email address on file",
			"request_id", reqID, "requester", detail.RequesterLogin)
		return
	}

	template := "promotion_denied"
	if approved {
		template = "promotion_approved"
	}
	msg, err := mailer.Render(template, mailer.TemplateData{
		BaseURL:       s.mailer.BaseURL(),
		RequesterName: detail.DisplayName(),
		ApproverName:  detail.ApproverLogin,
		Reason:        detail.Reason,
		Link:          s.mailer.BaseURL() + "/",
	})
	if err != nil {
		slog.Error("promotion notify: rendering decision email", "error", err)
		return
	}
	msg.To = []string{detail.RequesterEmail}
	s.mailer.SendAsync(msg)
}

// publisherRequestLink deep-links an administrator at the row they were
// emailed about. The queue page reads ?request= and highlights it; an id that
// no longer matches anything simply shows the queue.
func (s *Server) publisherRequestLink(reqID string) string {
	return s.mailer.BaseURL() + "/admin/publisher-requests?request=" + url.QueryEscape(reqID)
}
