// Package mail sends the platform's system email over SMTP.
//
// Two things about it are load-bearing:
//
//   - Email is optional. An install with no relay configured sends nothing and
//     errors at nothing; every feature that notifies still works, it just works
//     silently. "Absent means not offered" applies here as it does to the
//     artifact store and the isolated backend.
//   - A send never fails the request that triggered it. Someone asking for
//     publisher access must not see a 500 because a relay is down; the request
//     is recorded and visible in the UI either way, so a lost notification
//     costs a nudge, not the workflow.
//
// Two transports, one Message. SMTP is the default and what a corporate relay
// speaks; a sending API (Resend, in api.go) exists for the install where
// outbound 587 is closed and 443 is not — a firewall rule nobody here can
// change is not something an administrator can work around by choosing a
// different relay. The seam is deliberately narrow: Send picks a transport,
// everything above it — templates, fire-and-forget dispatch, the test button —
// is unaware which one ran, and a failure on either side comes back as one
// error string in the sender's own words.
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"

	"toft/internal/notify"
)

// Transport security modes, mirroring database.EmailSecurity*.
const (
	SecurityStartTLS    = "starttls"
	SecurityImplicitTLS = "implicit_tls"
	SecurityNone        = "none"
)

// SendTimeout bounds one delivery attempt. There is no retry queue behind it:
// a relay or API that is down when we call loses the message, and the log says
// so. A provider's 429 is treated the same way — this is not a sending platform
// with a queue, and pretending otherwise would need the outbox §14 rules out.
const SendTimeout = 30 * time.Second

// Config is a decrypted mirror of the stored email settings. The credential —
// SMTP password or API key, depending on Transport — is plaintext here and
// nowhere else: it is built at the point of use and dropped when the send
// returns.
type Config struct {
	// Transport is TransportSMTP or TransportAPI. Empty means SMTP, so a caller
	// that predates the API transport keeps working unchanged.
	Transport string

	// SMTP.
	Host       string
	Port       int
	Security   string
	Username   string
	Password   string
	SkipVerify bool

	// Sending API.
	APIProvider string
	APIKey      string
	// apiBaseURL redirects the API call at a test server. Unexported so no
	// caller outside this package — and so nothing an administrator can type —
	// can aim a credential at an arbitrary host.
	apiBaseURL string

	// Both.
	FromAddress string
	FromName    string
	ReplyTo     string
}

// Audiences. Every message belongs to exactly one, and an administrator can
// switch either off without turning email off: the two are wanted by different
// people for different reasons — "stop telling my users things" and "stop
// filling my inbox" are separate complaints.
//
// The names live in internal/notify because Slack switches on the same two, and
// an audience that meant one thing in email and another in Slack would be a
// setting nobody could reason about. Re-exported here so no call site has to
// know that.
const (
	AudienceAdmin       = notify.AudienceAdmin
	AudienceApp         = notify.AudienceApp
	AudienceAppOutbound = notify.AudienceAppOutbound
	// AudienceAnnouncement is the one audience no switch silences. See
	// internal/notify for why, and audienceEnabled for where that is enforced.
	AudienceAnnouncement = notify.AudienceAnnouncement
	// AudienceReport is the second whose arrival is the point, and the second
	// no switch silences. Its enforcement point is the recipient list.
	AudienceReport = notify.AudienceReport
	AudienceTest   = notify.AudienceTest
)

// Message is one email. Both bodies are always set: HTML for the client that
// can render it, text for the one that cannot and for the archive.
type Message struct {
	To      []string
	Subject string
	HTML    string
	Text    string
	// CC and BCC are used only by app-initiated mail. Platform notifications
	// address everyone in To: a template that quietly copied somebody would be
	// a disclosure nobody declared.
	CC  []string
	BCC []string
	// FromName overrides the install's sender *display name* only — never the
	// address, which SPF requires stays the install's. Set for app-initiated
	// mail to "<App Name> (via <install name>)", which is the frame that stops
	// an app speaking in the platform's voice.
	FromName string
	// ReplyTo overrides the install's reply-to. Set from the app's own
	// integration row, never from anything app code sent: a Reply-To an app
	// chooses routes answers to an address the company does not control while
	// wearing a From that it does.
	ReplyTo string
	// Attachments ride app-initiated mail only, and only when an administrator
	// switched them on for that app.
	Attachments []Attachment
	// Audience is set by Render from the template's own declaration, not by the
	// caller, so a new send site cannot forget it and quietly escape the
	// administrator's switches. A message that arrives here without one is
	// dropped rather than sent — see Service.SendAsync.
	Audience string

	// template and data are what Render built this from, so the Service can
	// frame it again with the install's name at send (see reframe). Empty for
	// a message built any other way.
	template string
	data     TemplateData
}

// Attachment is one file on a message. Content is held in memory: these are
// bounded well below the assembled-message limit, and streaming from disk would
// mean a send path that can fail on a file a caller has already been told was
// accepted.
type Attachment struct {
	Name    string
	Type    string
	Content []byte
}

// Validate reports whether the configuration could plausibly deliver anything.
// It is deliberately shallow — it catches the blank field and the typo'd port,
// not a wrong credential, which only a real send can tell you about.
func (c Config) Validate() error {
	switch c.Transport {
	case TransportAPI:
		if err := c.validateAPI(); err != nil {
			return err
		}
	case TransportSMTP, "":
		if err := c.validateSMTP(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown email transport %q", c.Transport)
	}

	// The sender identity is common to both transports: it is what a recipient
	// sees and replies to either way.
	if strings.TrimSpace(c.FromAddress) == "" {
		return errors.New("a From address is required")
	}
	if _, err := mail.ParseAddress(c.FromAddress); err != nil {
		return fmt.Errorf("From address %q is not a valid email address", c.FromAddress)
	}
	if c.ReplyTo != "" {
		if _, err := mail.ParseAddress(c.ReplyTo); err != nil {
			return fmt.Errorf("Reply-To address %q is not a valid email address", c.ReplyTo)
		}
	}
	return nil
}

func (c Config) validateSMTP() error {
	if strings.TrimSpace(c.Host) == "" {
		return errors.New("SMTP host is required")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("SMTP port %d is out of range", c.Port)
	}
	switch c.Security {
	case SecurityStartTLS, SecurityImplicitTLS, SecurityNone:
	default:
		return fmt.Errorf("unknown transport security %q", c.Security)
	}
	return nil
}

// Send delivers one message synchronously through whichever transport is
// configured. Callers that must not block use Service.SendAsync; this is for the
// test-email button, where an administrator is waiting for the relay's or
// provider's own error text.
func Send(ctx context.Context, cfg Config, msg Message) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if len(msg.To) == 0 {
		return errors.New("no recipients")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, SendTimeout)
		defer cancel()
	}
	if cfg.Transport == TransportAPI {
		return sendAPI(ctx, cfg, msg)
	}
	return sendSMTP(ctx, cfg, msg)
}

func sendSMTP(ctx context.Context, cfg Config, msg Message) error {
	m := gomail.NewMsg()
	if msg.FromName != "" {
		// The app's display name, over the install's. The address is untouched.
		if err := m.FromFormat(msg.FromName, cfg.FromAddress); err != nil {
			return fmt.Errorf("from address: %w", err)
		}
	} else if cfg.FromName != "" {
		if err := m.FromFormat(cfg.FromName, cfg.FromAddress); err != nil {
			return fmt.Errorf("from address: %w", err)
		}
	} else if err := m.From(cfg.FromAddress); err != nil {
		return fmt.Errorf("from address: %w", err)
	}
	if err := m.To(msg.To...); err != nil {
		return &recipientError{err: err}
	}
	if len(msg.CC) > 0 {
		if err := m.Cc(msg.CC...); err != nil {
			return fmt.Errorf("cc recipients: %w", err)
		}
	}
	if len(msg.BCC) > 0 {
		if err := m.Bcc(msg.BCC...); err != nil {
			return fmt.Errorf("bcc recipients: %w", err)
		}
	}
	replyTo := msg.ReplyTo
	if replyTo == "" {
		replyTo = cfg.ReplyTo
	}
	if replyTo != "" {
		if err := m.ReplyTo(replyTo); err != nil {
			return fmt.Errorf("reply-to address: %w", err)
		}
	}
	m.Subject(msg.Subject)
	// Text first, HTML as the alternative: a multipart/alternative is ordered
	// least-rich to most-rich, and clients pick the last part they understand.
	m.SetBodyString(gomail.TypeTextPlain, msg.Text)
	m.AddAlternativeString(gomail.TypeTextHTML, msg.HTML)
	for _, att := range msg.Attachments {
		content := att.Content
		m.AttachReader(att.Name, bytes.NewReader(content))
	}
	if usesMark(msg.HTML) {
		if err := m.EmbedReader(markName, bytes.NewReader(markPNG)); err != nil {
			return fmt.Errorf("embedding the Toft mark: %w", err)
		}
	}

	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	return client.DialAndSendWithContext(ctx, m)
}

// recipientError is a failure about the address a message was sent to rather
// than about the relay: an address that does not parse, or a provider's
// validation answer. Its text keeps the "recipients:" prefix it always had.
type recipientError struct{ err error }

func (e *recipientError) Error() string { return "recipients: " + e.err.Error() }
func (e *recipientError) Unwrap() error { return e.err }

// recipientLevel reports whether err is about the one recipient a message was
// addressed to — so the next recipient may still be reached — rather than about
// the relay or the provider, where trying the next one would only wait out the
// same failure again.
func recipientLevel(err error) bool {
	var re *recipientError
	if errors.As(err, &re) {
		return true
	}
	var se *gomail.SendError
	return errors.As(err, &se) && se.Reason == gomail.ErrSMTPRcptTo
}

func newClient(cfg Config) (*gomail.Client, error) {
	opts := []gomail.Option{
		gomail.WithPort(cfg.Port),
		gomail.WithTimeout(SendTimeout),
	}
	switch cfg.Security {
	case SecurityImplicitTLS:
		// TLS from the first byte (the submissions port, 465). No fallback to
		// cleartext: an admin who asked for implicit TLS meant it.
		opts = append(opts, gomail.WithSSL(), gomail.WithTLSPolicy(gomail.NoTLS))
	case SecurityNone:
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		// Mandatory, not opportunistic: a relay that suddenly stops offering
		// STARTTLS should fail loudly rather than quietly send the credential
		// in the clear.
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}
	if cfg.Username != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(cfg.Username),
			gomail.WithPassword(cfg.Password))
	}
	if cfg.SkipVerify {
		opts = append(opts, gomail.WithTLSConfig(&tls.Config{
			InsecureSkipVerify: true,
			ServerName:         cfg.Host,
		}))
	}
	c, err := gomail.NewClient(cfg.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("smtp client: %w", err)
	}
	return c, nil
}
