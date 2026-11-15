package api

import (
	"log/slog"

	"toft/internal/database"
	mailer "toft/internal/mail"
	"toft/internal/notify"
)

// notifyAccountActivated tells somebody their account here is ready.
//
// Best-effort like every notification in this product, and here that matters
// more than usual: the account is active the moment the transaction commits,
// so a relay that is down costs a message rather than an admission. Nothing
// tells the person to wait for this — the refusal they were shown at the door
// promises no email, for exactly this reason — so on an install with no relay
// they simply try again and get in.
//
// The words are the administrator's, and the platform adds only the link. It
// does not name who activated the account: a promotion is a decision somebody
// asked for and is owed an answer to, while this is the organization admitting
// a new person, and the sentence it wrote is the whole of what it wanted to
// say.
func (s *Server) notifyAccountActivated(user *database.User) {
	if s.mailer == nil || user == nil {
		return
	}
	if user.Email == "" {
		slog.Debug("account activated, but the account has no email address on file",
			"user", user.Login)
		return
	}

	// Read here rather than passed in: the sentence is install-wide standing
	// copy, and the activation handler has no other reason to load settings.
	message := database.DefaultActivationMessage
	if settings, err := s.db.GetPlatformSettings(); err != nil {
		slog.Error("activation notify: reading the activation message, using the default", "error", err)
	} else {
		message = settings.ActivationMessage
	}

	msg, err := mailer.Render("account_activated", mailer.TemplateData{
		BaseURL:           s.mailer.BaseURL(),
		RequesterName:     user.Name,
		RequesterLogin:    user.Login,
		ActivationMessage: message,
		Link:              s.mailer.BaseURL() + "/",
	})
	if err != nil {
		slog.Error("activation notify: rendering the email", "error", err)
		return
	}
	msg.To = []string{user.Email}
	s.mailer.SendAsync(msg)
}

// notifyActivationPending tells the administrators that somebody is waiting.
//
// Called once, where the account is created, and never on the sign-ins it is
// refused afterwards: the queue on the People page is the record, and this is
// only the nudge to go and work it. A nudge that repeated every time the person
// retried would be a nudge people filter, and the queue would go back to being
// a page nobody opens.
//
// Both channels, because this is about the install rather than about a person's
// standing — an administrator is being asked to do something, which is exactly
// what a shared channel is good at. The person-facing half of this workflow,
// notifyAccountActivated, is mail-only for the opposite reason.
func (s *Server) notifyActivationPending(user *database.User, providerName string) {
	if user == nil {
		return
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	link := "/people?status=pending"

	s.postPlatform("account_pending_activation", "", func(base string) notify.TemplateData {
		return notify.TemplateData{
			BaseURL:        base,
			RequesterName:  name,
			RequesterLogin: user.Login,
			RequesterEmail: user.Email,
			EventTitle:     providerName,
			Link:           base + link,
		}
	})

	if s.mailer == nil {
		return
	}
	to, err := s.mailer.AdminRecipients()
	if err != nil {
		slog.Error("activation notify: listing admin recipients", "error", err)
		return
	}
	if len(to) == 0 {
		// Worth a warning rather than silence, for the reason the publisher
		// queue warns: an account nobody is told about still sits in the queue,
		// but only if somebody thinks to look.
		slog.Warn("an account is awaiting activation, but no administrator has an email address on file and no admin group address is set",
			"user", user.Login)
		return
	}

	msg, err := mailer.Render("account_pending_activation", mailer.TemplateData{
		BaseURL:        s.mailer.BaseURL(),
		RequesterName:  name,
		RequesterLogin: user.Login,
		RequesterEmail: user.Email,
		EventTitle:     providerName,
		Link:           s.mailer.BaseURL() + link,
	})
	if err != nil {
		slog.Error("activation notify: rendering the queue email", "error", err)
		return
	}
	msg.To = to
	s.mailer.SendAsync(msg)
}
