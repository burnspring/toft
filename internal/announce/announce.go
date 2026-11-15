// Package announce sends the announcement an administrator wrote.
//
// It is one goroutine per frozen row, and it exists as a package rather than as
// a handler's `go func` for one reason: **everything interesting about this
// feature is a failure mode**, and none of them are reachable from an HTTP test.
// A relay that rate-limits at recipient forty, a process that restarts at three
// hundred, a link that stopped being valid between the save and the send — the
// handler's whole job is to freeze the row and start this, and this is where the
// record either stays true or quietly stops being.
//
// The three rules it holds:
//
//   - **A count that cannot distinguish outcomes is not a record.** Every
//     recipient gets a row, the row carries what happened and why, and the five
//     counts on the announcement are recomputed from those rows rather than
//     incremented alongside them.
//   - **Patience within one run, and no queue.** A recipient is retried a bounded
//     number of times with a backoff, and sends are paced. Nothing survives the
//     process: a restart abandons, because resuming needs an at-least-once story
//     and a duplicate-suppression answer, which is the outbox this product has
//     ruled out.
//   - **It never lies by omission.** A run that cannot start still completes the
//     row, with every recipient carrying the reason.
package announce

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"toft/internal/database"
	"toft/internal/mail"
)

// The caps on what an administrator may type. A field over its cap **refuses
// the save** rather than being truncated: a message going out carrying words
// its author never wrote is worse than a refusal, which is the rule the share
// note already set.
const (
	MaxSubject  = 200
	MaxBody     = 4000
	MaxLinkText = 60
	// MaxLinkURL is not a product decision — it is the longest URL anything
	// downstream will reliably carry, and a field with no bound at all is a
	// field somebody pastes a megabyte into.
	MaxLinkURL = 2000
)

// The pacing and the retry schedule. Constants in the sender rather than
// settings: an administrator tuning a rate limit against their own relay is a
// knob with no good value and no way to discover a bad one.
//
// The retry is the one thing this send path adds that SendAsync refuses, and it
// is added on the ground SendTimeout's comment leaves open. That comment rules
// out *an outbox* — a durable queue that survives the process and retries
// tomorrow. This is neither: it is one run's own patience with a relay that
// rate-limited it, and a 429 partway through five hundred sends is the expected
// behaviour of every relay this will meet rather than an exception.
const (
	// defaultPace is the gap between recipients.
	defaultPace = 250 * time.Millisecond
)

// defaultBackoff is the wait before each retry. One initial attempt plus one
// retry per entry, so four attempts in all before a recipient is failed.
var defaultBackoff = []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}

// Mailer is the half of mail.Service this package uses. An interface so the
// tests can fail a relay on demand, which is the only way the retry, the
// pacing and the failure counts can be asserted at all.
type Mailer interface {
	SendOne(ctx context.Context, msg mail.Message) error
	BaseURL() string
	// SenderName is the install's name, or "Toft", so what Render builds names
	// the sender the relay will be handed. SendOne frames the message again
	// with it at send, so this is about the two agreeing, not about trust.
	SenderName() string
	Enabled() bool
}

// Runner sends announcements. One per process, held by the API server.
type Runner struct {
	db   *database.DB
	mail Mailer

	// Overridden only by this package's tests, which would otherwise take
	// forty seconds to prove a recipient fails.
	pace    time.Duration
	backoff []time.Duration
}

// New builds the runner. A nil Mailer is a valid install — one with no relay —
// and Start refuses rather than panicking, though the handler will have
// refused first.
func New(db *database.DB, m Mailer) *Runner {
	return &Runner{db: db, mail: m, pace: defaultPace, backoff: defaultBackoff}
}

// Start runs one frozen announcement in the background.
//
// The goroutine is owned here rather than in internal/api because the run
// outlives the request that started it by minutes, and a handler that owned it
// would be a handler whose failure modes are somebody else's.
func (r *Runner) Start(id string) {
	go func() {
		if err := r.Run(context.Background(), id); err != nil {
			slog.Error("announcement send failed", "announcement", id, "error", err)
		}
	}()
}

// Run resolves the audience, sends to everyone in it, and completes the row.
//
// Synchronous, and exported for the tests: every failure worth asserting about
// this feature happens inside here, and a test that had to drive it through a
// goroutine could only assert on timing.
func (r *Runner) Run(ctx context.Context, id string) error {
	a, err := r.db.GetAnnouncementEmail(id)
	if err != nil {
		return fmt.Errorf("reading announcement %s: %w", id, err)
	}
	if a == nil {
		return fmt.Errorf("no announcement %s", id)
	}
	if a.Status != database.AnnouncementSending {
		// Not an error. The freeze is what decides who runs, so a second caller
		// arriving here has already lost that race and must not send anything.
		slog.Debug("announcement is not sending, nothing to run",
			"announcement", id, "status", a.Status)
		return nil
	}

	// A resend is sent to the original's failed and abandoned people rather
	// than to its audience; everything after the resolution is the same run.
	var pending []database.AnnouncementRecipient
	if a.ResendOf != "" {
		pending, err = r.db.ResolveAnnouncementResend(a.ID, a.ResendOf)
	} else {
		pending, err = r.db.ResolveAnnouncementRecipients(a.ID, a.Audience)
	}
	if err != nil {
		return fmt.Errorf("resolving the audience of %s: %w", id, err)
	}

	// Re-validated here, immediately before the loop, and not as belt and
	// braces: this is the last point at which the URL that will wear this
	// install's From address is checked, and the row has been frozen since the
	// save that checked it first.
	//
	// A refusal here fails every recipient with the reason rather than leaving
	// the row in `sending` forever. Nobody is mailed, the record says why, and
	// it is terminal — which is the whole difference between a record and a
	// status meaning "we do not know".
	if linkErr := ValidateLink(a.LinkURL); linkErr != nil {
		slog.Error("announcement link refused at send", "announcement", id, "error", linkErr)
		for _, rec := range pending {
			_ = r.db.CompleteAnnouncementRecipient(rec.ID, database.AnnouncementFailed,
				"the link on this announcement was refused: "+linkErr.Error(), 0)
		}
		return r.db.FinishAnnouncementSend(id)
	}

	msg, err := Render(r.mail.BaseURL(), r.mail.SenderName(), a)
	if err != nil {
		for _, rec := range pending {
			_ = r.db.CompleteAnnouncementRecipient(rec.ID, database.AnnouncementFailed,
				"this announcement could not be rendered: "+err.Error(), 0)
		}
		if finishErr := r.db.FinishAnnouncementSend(id); finishErr != nil {
			return finishErr
		}
		return fmt.Errorf("rendering announcement %s: %w", id, err)
	}

	slog.Info("announcement send starting",
		"announcement", id, "audience", a.Audience, "recipients", len(pending))

	for i, rec := range pending {
		if i > 0 {
			if !sleep(ctx, r.pace) {
				// The context is done, which on this path means the process is
				// going away. Everything still pending stays pending, and the
				// startup sweep abandons it — never resumed and never claimed.
				return ctx.Err()
			}
		}
		status, reason, attempts := r.deliver(ctx, msg, rec.Email)
		if err := r.db.CompleteAnnouncementRecipient(rec.ID, status, reason, attempts); err != nil {
			slog.Error("recording an announcement recipient failed",
				"announcement", id, "recipient", rec.ID, "error", err)
		}
		// Recomputed from the rows after every recipient, so the detail page a
		// nervous administrator is watching is never ahead of or behind what
		// actually happened.
		if err := r.db.RecountAnnouncementEmail(id); err != nil {
			slog.Error("recounting an announcement failed", "announcement", id, "error", err)
		}
	}

	if err := r.db.FinishAnnouncementSend(id); err != nil {
		return fmt.Errorf("completing announcement %s: %w", id, err)
	}
	done, err := r.db.GetAnnouncementEmail(id)
	if err == nil && done != nil {
		slog.Info("announcement sent", "announcement", id,
			"delivered", done.Delivered, "failed", done.Failed,
			"skipped", done.Skipped, "abandoned", done.Abandoned)
	}
	return nil
}

// deliver sends to one address, with the run's own patience.
//
// It returns the recipient's outcome rather than an error, because there is no
// caller above it that would do anything different with one: every result of
// trying to mail a person is a row, and a failure is a count rather than a
// reason to stop the run.
func (r *Runner) deliver(ctx context.Context, msg mail.Message, to string) (status, reason string, attempts int) {
	msg.To = []string{to}
	var last error
	for attempt := 0; attempt <= len(r.backoff); attempt++ {
		if attempt > 0 && !sleep(ctx, r.backoff[attempt-1]) {
			break
		}
		attempts++
		send, cancel := context.WithTimeout(ctx, mail.SendTimeout)
		last = r.mail.SendOne(send, msg)
		cancel()
		if last == nil {
			return database.AnnouncementDelivered, "", attempts
		}
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = errors.New("the send was abandoned before it was attempted")
	}
	// The relay's own words, kept. An administrator chasing a bounce needs what
	// the far side actually said, and "delivery failed" is a sentence they can
	// do nothing with.
	return database.AnnouncementFailed, last.Error(), attempts
}

// Available reports whether this install can send an announcement at all.
//
// The condition is the relay alone. There is no audience switch to consult —
// this is the one audience nothing silences — and the surface uses this to make
// Send **absent** rather than disabled: a button that promises mail an install
// cannot send is "copy may not promise mail" broken by a control rather than by
// a sentence.
func (r *Runner) Available() bool {
	return r != nil && r.mail != nil && r.mail.Enabled()
}

// SendTest renders the real template with the real content and delivers it to
// one address, synchronously, returning the relay's own error.
//
// It freezes nothing, writes no recipient row and moves no count. There is no
// un-send, and a preview pane does not test the relay, the From address, the
// link, or what the body looks like in Outlook — so the diagnostic is a real
// message through the real path, and the only thing it does not do is reach
// anybody else.
func (r *Runner) SendTest(ctx context.Context, a *database.AnnouncementEmail, to string) error {
	if r == nil || r.mail == nil {
		return errors.New("email is not configured on this install")
	}
	msg, err := Render(r.mail.BaseURL(), r.mail.SenderName(), a)
	if err != nil {
		return err
	}
	msg.To = []string{to}
	send, cancel := context.WithTimeout(ctx, mail.SendTimeout)
	defer cancel()
	return r.mail.SendOne(send, msg)
}

// AbandonStranded completes any announcement this process found mid-send.
//
// Run before the listener, because a row in `sending` while nothing is sending
// is the state every reader would misread — and because an administrator who
// re-sends by hand at that moment should be looking at a completed record
// rather than a live one.
func (r *Runner) AbandonStranded() error {
	announcements, recipients, err := r.db.AbandonStrandedAnnouncements()
	if err != nil {
		return err
	}
	if announcements > 0 {
		slog.Warn("announcements were abandoned by a restart",
			"announcements", announcements, "recipients", recipients)
	}
	return nil
}

// Render builds the message for one announcement.
//
// Exported because the test send needs exactly this — the real template with
// the real content — and a preview that rendered something else would be a
// preview of nothing. It is the only thing shared between the two paths, and
// sender is the install's name for the same reason: it renders through the one
// frame function the send path uses.
func Render(baseURL, sender string, a *database.AnnouncementEmail) (mail.Message, error) {
	return mail.RenderAs(sender, "announcement", mail.TemplateData{
		BaseURL:             baseURL,
		AnnouncementSubject: a.Subject,
		AnnouncementBody:    a.Body,
		Link:                a.LinkURL,
		LinkText:            a.LinkText,
	})
}

// ValidateLink refuses a URL this install will not put its own From address
// behind.
//
// http and https only, a host required, and no credentials in the authority. It
// is checked at the save and again immediately before the send, and that is not
// belt and braces: a javascript: URL inside mail wearing this install's sender
// is a phishing primitive, and unlike the public base address — which only an
// administrator can reach — this field is reachable by anyone who can compose
// and is mailed to everybody.
//
// An empty link is valid. Most announcements have nothing to link to.
func ValidateLink(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if len(raw) > MaxLinkURL {
		return fmt.Errorf("the link is longer than %d characters", MaxLinkURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("that is not a URL")
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return errors.New("a link must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("a link needs a host — http://example.com/notice, not http:///notice")
	}
	if u.User != nil {
		return errors.New("a link may not carry a username or password")
	}
	return nil
}

// sleep waits, and reports whether the wait finished rather than the context
// ending. A false is the process going away, and every caller of it stops.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
