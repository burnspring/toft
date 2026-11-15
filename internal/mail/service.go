package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/telemetry"
)

// Service is the platform's handle on email. It reads its settings from the
// database on every call rather than caching them, so an administrator who
// fixes a relay hostname does not have to restart the binary to find out
// whether the fix worked.
type Service struct {
	db  *database.DB
	enc *crypto.Encryptor
	// envBaseURL is the BASE_URL the process was started with — the fallback
	// for when no administrator has set a public address.
	//
	// Links in email never come from the request that triggered the send: the
	// trigger is often a request from someone on a different origin than the
	// person who will read the mail, and a failure notification has no request
	// behind it at all.
	envBaseURL string
}

// NewService builds the mailer. It never fails: an install with no relay
// configured gets a Service that reports Enabled() false and drops every send
// with a debug log.
func NewService(db *database.DB, enc *crypto.Encryptor, envBaseURL string) *Service {
	return &Service{db: db, enc: enc, envBaseURL: strings.TrimSuffix(envBaseURL, "/")}
}

// BaseURL is where links in email should point: the administrator's stored
// public address, or the BASE_URL this process was started with when they have
// not set one.
//
// Read per call rather than cached at construction, for the same reason the
// relay settings are: an administrator who has just corrected the address every
// notification points at should be able to send a test message and see the fix,
// without an operator restarting the binary for them.
func (s *Service) BaseURL() string {
	if s == nil {
		return ""
	}
	return s.db.ResolvePublicBaseURL(s.envBaseURL)
}

// SenderName is what this install calls itself in mail: the administrator's
// name for it, or "Toft". Read per call, like BaseURL, so a rename shows in
// the next message rather than after a restart.
//
// The send path applies it itself (reframe), which is the point: the sites that
// build a message never pass it, so none can forget to. Exported for the paths
// that render without sending — a preview, and app mail's sender suffix.
func (s *Service) SenderName() string {
	if s == nil {
		return database.DefaultSenderName
	}
	return s.db.ResolveSenderName()
}

// Enabled reports whether email is configured and switched on. Callers use it
// to decide whether to say anything to the user about notifications — not to
// decide whether to send, which SendAsync already handles.
func (s *Service) Enabled() bool {
	if s == nil {
		return false
	}
	cfg, err := s.config()
	return err == nil && cfg != nil
}

// AppNotificationsEnabled reports whether a message to the people who *use*
// this install would actually go out: email is configured and switched on, and
// the administrator has not silenced the app audience.
//
// It exists because one caller has to decide whether to **offer** a send rather
// than whether to make one. Every other notification in the product is sent
// because something happened, so a switched-off audience is correctly a silent
// drop; the share notice is a checkbox somebody ticks, and a checkbox that
// promises mail an administrator has switched off is the notification
// invariant's "copy may not promise mail" broken by a control rather than by a
// sentence.
//
// Read per call for the reason everything else here is: an administrator who
// has just turned the audience back on should not need a restart before the
// checkbox reappears.
func (s *Service) AppNotificationsEnabled() bool {
	if s == nil {
		return false
	}
	settings, cfg, err := s.load()
	return err == nil && cfg != nil && audienceEnabled(settings, AudienceApp)
}

// SendAsync delivers in the background and never returns an error, because no
// caller of it should change what it does based on whether a relay answered.
// The request that triggered the notification has already succeeded; a failure
// here is an operations problem, logged, not a user-facing one.
func (s *Service) SendAsync(msg Message) {
	if s == nil {
		return
	}
	settings, cfg, err := s.load()
	if err != nil {
		slog.Error("email settings unreadable, notification dropped", "subject", msg.Subject, "error", err)
		return
	}
	if cfg == nil {
		slog.Debug("email disabled, notification skipped", "subject", msg.Subject, "recipients", len(msg.To))
		return
	}
	if !audienceEnabled(settings, msg.Audience) {
		// Not a failure. An administrator switched this audience off, or — if
		// the audience is blank — a send site was written without declaring one,
		// which is a bug this refuses to paper over.
		if msg.Audience == "" {
			slog.Error("notification has no audience and was dropped; set one on its template", "subject", msg.Subject)
		} else {
			slog.Debug("notifications for this audience are off, message skipped",
				"audience", msg.Audience, "subject", msg.Subject, "recipients", len(msg.To))
		}
		return
	}
	msg.To = addressed(settings, msg)
	if len(msg.To) == 0 {
		// Loud rather than silent, so the next send site that forgets to
		// address its message is visible in the log instead of reaching nobody.
		slog.Warn("notification has no recipients and was dropped", "subject", msg.Subject, "audience", msg.Audience)
		return
	}
	msg = reframe(msg, s.SenderName())
	go func() {
		// Timed and counted here, at the one point every audience-passing
		// message goes through, rather than at each of the send sites. It is the
		// same argument the audience switch itself makes: a notification that
		// could escape SendAsync could escape the accounting too.
		//
		// The *subject* is never an attribute — it names an app and sometimes a
		// person. The audience is, because it is a closed set from
		// internal/notify.
		start := time.Now()
		delivered, failed, firstErr := sendEach(*cfg, msg)
		var outcome error
		if delivered == 0 {
			outcome = firstErr
		}
		telemetry.RecordNotify(context.Background(), telemetry.ChannelEmail, msg.Audience,
			telemetry.Outcome(outcome), time.Since(start))
		if failed > 0 {
			slog.Error("sending email failed", "subject", msg.Subject,
				"recipients", delivered, "failed", failed, "error", firstErr)
			if delivered == 0 {
				return
			}
		}
		slog.Info("email sent", "subject", msg.Subject, "recipients", delivered, "failed", failed)
	}()
}

// sendEach delivers a notification as one message per recipient, each under
// its own timeout, and counts the outcome.
//
// **One message per recipient, not one addressed to all.** On SMTP one refused
// RCPT makes go-mail reset the transaction and deliver to nobody, and one
// address that does not parse fails the whole list before the relay is asked;
// so one dead mailbox among an app's owners silenced every notice to the rest.
// go-mail does not say which recipient was refused, so "retry without it" is
// not available; separate messages are. Reports and announcements already
// work this way.
//
// The loop stops early on a failure that is not about the recipient — a relay
// that is down, a credential refused — so a dead relay costs one timeout, not
// one per recipient. A message carrying CC or BCC (platform notifications never
// do) is sent once as it is, since splitting it would copy them N times.
func sendEach(cfg Config, msg Message) (delivered, failed int, firstErr error) {
	one := func(m Message) error {
		ctx, cancel := context.WithTimeout(context.Background(), SendTimeout)
		defer cancel()
		return Send(ctx, cfg, m)
	}
	if len(msg.CC) > 0 || len(msg.BCC) > 0 || len(msg.To) <= 1 {
		if err := one(msg); err != nil {
			return 0, len(msg.To), err
		}
		return len(msg.To), 0, nil
	}
	for i, to := range msg.To {
		m := msg
		m.To = []string{to}
		err := one(m)
		if err == nil {
			delivered++
			continue
		}
		failed++
		if firstErr == nil {
			firstErr = err
		}
		if !recipientLevel(err) {
			failed += len(msg.To) - i - 1
			break
		}
	}
	return delivered, failed, firstErr
}

// SendOne delivers one message synchronously and returns whether the relay took
// it. It is SendAsync's opposite, and it exists for exactly one caller.
//
// **Every other notification in this product is allowed to vanish**, and
// SendAsync's doc comment says why: no caller of it should change what it does
// based on whether a relay answered, because the request that triggered the
// notification has already succeeded. An announcement is the one message where
// that is false. Its whole product is a frozen row saying who this install told
// what, and a count that cannot distinguish *delivered* from *the relay refused*
// is not a record, it is a number that will one day be wrong in a direction
// nobody can reconstruct.
//
// It applies the relay check, the audience gate and the group-address rule and
// nothing else. There is no retry here and no pacing: those are one run's own
// patience with a relay, they belong to the runner that knows how many
// recipients are left, and putting them here would make every other send site
// inherit a queue this product has ruled out.
func (s *Service) SendOne(ctx context.Context, msg Message) error {
	if s == nil {
		return errors.New("email is not configured on this install")
	}
	settings, cfg, err := s.load()
	if err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("email is turned off on this install")
	}
	if !audienceEnabled(settings, msg.Audience) {
		// The same refusal SendAsync makes, and for the same reason: a message
		// with no audience is a send site that forgot to declare one, and this
		// path must not be the way somebody discovers they can skip it.
		if msg.Audience == "" {
			return errors.New("this message declares no audience; set one on its template")
		}
		return fmt.Errorf("notifications for the %q audience are switched off", msg.Audience)
	}
	msg.To = addressed(settings, msg)
	if len(msg.To) == 0 {
		return errors.New("no recipient")
	}
	msg = reframe(msg, s.SenderName())
	start := time.Now()
	err = Send(ctx, *cfg, msg)
	// Counted at the same point SendAsync counts, so an announcement's sends
	// appear in the operator's notification metric beside every other one. The
	// audience is a bounded label from internal/notify; the subject never is.
	telemetry.RecordNotify(ctx, telemetry.ChannelEmail, msg.Audience,
		telemetry.Outcome(err), time.Since(start))
	return err
}

// AdminRecipients is where administrator mail will actually land: the group
// address when one is set, otherwise every administrator who can receive mail —
// unlocked, with an address on file.
//
// Callers use it to answer "is there anyone to tell?", which is worth a warning
// when the answer is no: a publisher request nobody is notified about still sits
// in the queue, but only if someone thinks to look. They do not use it to decide
// where to send. SendAsync applies the same group-address rule itself, so a
// caller that skips this cannot escape the setting — only fail to warn.
func (s *Service) AdminRecipients() ([]string, error) {
	if s == nil {
		return nil, nil
	}
	return s.db.AdminRecipients()
}

// SendTest delivers synchronously and returns the relay's or provider's own
// error, because the administrator pressing the button is waiting for exactly
// that. It uses the saved settings, not a submitted form: what it proves is that
// the stored configuration works.
//
// It ignores the per-audience switches on purpose. A test send is a diagnostic,
// not a notification — an administrator who has silenced both audiences still
// needs to know whether the credential they just pasted works, and a button that
// went quiet instead would read as a broken relay.
func (s *Service) SendTest(ctx context.Context, to string) error {
	if s == nil {
		return errors.New("email is not configured on this install")
	}
	settings, cfg, err := s.load()
	if err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("email is turned off — enable it and save before sending a test")
	}
	msg, err := RenderAs(s.SenderName(), "test_email", TemplateData{BaseURL: s.BaseURL(), Via: describeSender(settings)})
	if err != nil {
		return err
	}
	msg.To = []string{to}
	return Send(ctx, *cfg, msg)
}

// addressed returns who a message is actually delivered to: the admin group
// address alone when the message is for administrators and one is set, otherwise
// whatever the caller asked for.
//
// It lives on the send path rather than at the call sites, for the reason the
// audience switches do: SendAsync is the one place every notification passes
// through, so a send site written next year cannot route around the setting by
// building its own recipient list.
//
// Two things about the order it is applied in are load-bearing. It runs *after*
// the audience switch, because the switch decides whether to send and this
// decides only where — an administrator who silenced the audience does not get it
// back by naming a group. It runs *before* the empty-recipients check, because an
// install where no administrator has an address on file is exactly the case a
// group address solves, and dropping the message there would make the setting
// useless in the situation it most obviously helps.
//
// It never touches AudienceApp. Mail about an app goes to the people who own it;
// an admin alias is not one of them, and copying it there would turn a setting
// about administrator noise into an install-wide interception.
func addressed(settings database.EmailSettings, msg Message) []string {
	if msg.Audience == AudienceAdmin && settings.AdminGroupAddress != "" {
		return []string{settings.AdminGroupAddress}
	}
	return msg.To
}

// audienceEnabled maps a message's audience onto the administrator's switches.
// An unrecognised audience — including the empty one — is refused rather than
// waved through, so a message that escapes both switches cannot be sent by
// omission.
func audienceEnabled(settings database.EmailSettings, audience string) bool {
	switch audience {
	case AudienceAdmin:
		return settings.NotifyAdmin
	case AudienceApp:
		return settings.NotifyApp
	case AudienceAnnouncement:
		// **True unconditionally, and the reason is written here because this is
		// the enforcement point that is not a setting.**
		//
		// notify_app means "stop telling app authors platform chatter about
		// their apps". notify_admin means "stop filling my inbox". Neither of
		// them means "stop the person who runs this install from reaching the
		// people on it", and routing an announcement through either would
		// produce the one lie its frozen record cannot afford: a row reading
		// *sent, 0 delivered* because a switch nobody associated with this
		// feature was off.
		//
		// Its gate is the Send button — an announcement does not exist until an
		// administrator writes it and does not go out until the same
		// administrator sends it. That is a stronger switch than a checkbox and
		// one nobody can leave off by accident.
		return true
	case AudienceReport:
		// **True unconditionally, for the announcement's reason.** A report is
		// not platform chatter about an app, so `notify_app` does not describe
		// it, and an administrator throwing that switch is not asking to stop a
		// Monday report reaching the people who read it. Routing it through
		// either switch would make one setting mean two things, and the person
		// who discovered that would discover it as silence.
		//
		// Its gate is the recipient list: a report exists only where somebody
		// wrote a cron *and* named people, and an empty list sends nothing.
		return true
	case AudienceAppOutbound:
		// Gated by appint.Check — the install's integrations.email switch and
		// the per-app attachment — before it ever gets here, and deliberately
		// not by NotifyApp. Listed rather than left to the default so that an
		// app-initiated message routed through SendAsync is not silently
		// dropped as "unrecognised audience", which would look like a broken
		// relay rather than a design decision.
		return true
	case AudienceTest:
		// Reached only if a test message were ever handed to SendAsync; SendTest
		// does not consult this. Treated as a notification with no switch of its
		// own, which means: not sent this way.
		return false
	default:
		return false
	}
}

// describeSender names what will carry the message, in the words the
// administrator chose it by.
func describeSender(settings database.EmailSettings) string {
	if settings.Transport == database.EmailTransportAPI {
		return ProviderLabel(settings.APIProvider) + "'s API"
	}
	if settings.Host != "" {
		return "your SMTP relay at " + settings.Host
	}
	return "your SMTP relay"
}

// config returns the decrypted send configuration, or nil when email is off.
func (s *Service) config() (*Config, error) {
	_, cfg, err := s.load()
	return cfg, err
}

// load reads the stored settings and the decrypted configuration they describe.
// The configuration is nil when email is off. Only the credential the selected
// transport needs is decrypted — a stale SMTP password left behind by an install
// that has moved to a sending API must not be able to break sending — and a
// credential that will not decrypt is an error rather than a silent send without
// authentication.
func (s *Service) load() (database.EmailSettings, *Config, error) {
	settings, err := s.db.GetEmailSettings()
	if err != nil {
		return settings, nil, err
	}
	if !settings.Enabled {
		return settings, nil, nil
	}
	cfg := Config{
		Transport:   settings.Transport,
		Host:        settings.Host,
		Port:        settings.Port,
		Security:    settings.Security,
		Username:    settings.Username,
		APIProvider: settings.APIProvider,
		FromAddress: settings.FromAddress,
		FromName:    settings.FromName,
		ReplyTo:     settings.ReplyTo,
		SkipVerify:  settings.SkipVerify,
	}
	switch settings.Transport {
	case database.EmailTransportAPI:
		if settings.APIKey != "" {
			plain, err := s.enc.Decrypt(settings.APIKey)
			if err != nil {
				return settings, nil, errors.New("the stored API key could not be decrypted — re-enter it")
			}
			cfg.APIKey = plain
		}
	default:
		if settings.Password != "" {
			plain, err := s.enc.Decrypt(settings.Password)
			if err != nil {
				return settings, nil, errors.New("the stored SMTP password could not be decrypted — re-enter it")
			}
			cfg.Password = plain
		}
	}
	if err := cfg.Validate(); err != nil {
		return settings, nil, err
	}
	return settings, &cfg, nil
}
