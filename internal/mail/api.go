package mail

// The HTTP transport: hand the message to a sending API over 443 instead of
// dialling a relay on 587.
//
// Why this exists at all, given that every provider also speaks SMTP: on the
// installs that ask for it, outbound SMTP is closed at a firewall the person
// configuring Toft does not administer, and "use the relay instead" is not
// advice they can act on. Nothing else about the platform changes — same
// templates, same fire-and-forget dispatch, same "email is never load-bearing"
// rule.
//
// What keeps the cost of it small:
//
//   - No new dependency. net/http and encoding/json; a provider's SDK would
//     bring a transitive tree to POST one JSON document.
//   - No new failure vocabulary above this file. A provider's error becomes one
//     error string in its own words — "Resend: the domain is not verified (HTTP
//     403)" — which is what the SMTP path already gives an administrator.
//   - No new credential shape. The API key is stored, encrypted, audited and
//     write-only exactly as the SMTP password is.
//
// Adding a second provider is a case in sendAPI, an entry in database's
// ValidEmailAPIProvider, and a label in the UI. It is deliberately not a plugin
// seam: two providers do not justify one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
)

// Transports, mirroring database.EmailTransport*.
const (
	TransportSMTP = "smtp"
	TransportAPI  = "api"
)

// Providers, mirroring database.EmailAPIProvider*.
const ProviderResend = "resend"

// resendEndpoint is the only host this transport talks to, and it is a constant
// rather than a setting: a configurable endpoint is a way to aim a stored
// credential at an arbitrary server, which is a worse thing to hand an
// administrator than a provider they cannot change.
const resendEndpoint = "https://api.resend.com/emails"

// apiClient is shared: one connection pool, and the same timeout the SMTP path
// uses so a hung provider cannot outlive a hung relay.
var apiClient = &http.Client{Timeout: SendTimeout}

// ProviderLabel is the provider's own name, for error text and logs.
func ProviderLabel(provider string) string {
	if provider == ProviderResend {
		return "Resend"
	}
	return provider
}

func (c Config) validateAPI() error {
	if !ValidProvider(c.APIProvider) {
		return fmt.Errorf("unknown email API provider %q", c.APIProvider)
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("an API key is required to send through %s", ProviderLabel(c.APIProvider))
	}
	// A key that cannot go in a header is caught here rather than by the
	// transport, whose error text is about header syntax and tells an
	// administrator nothing about what they pasted.
	if strings.ContainsAny(c.APIKey, " \t\r\n") {
		return errors.New("the API key contains a space or line break — it was probably pasted with surrounding text")
	}
	return nil
}

// ValidProvider reports whether a sending API is one this package can talk to.
func ValidProvider(provider string) bool { return provider == ProviderResend }

func sendAPI(ctx context.Context, cfg Config, msg Message) error {
	switch cfg.APIProvider {
	case ProviderResend:
		return sendResend(ctx, cfg, msg)
	default:
		// Unreachable through Send, which validates first. Kept because a
		// silent no-op here would look exactly like a delivered message.
		return fmt.Errorf("unknown email API provider %q", cfg.APIProvider)
	}
}

// resendRequest is Resend's POST /emails body. Both bodies are always sent, for
// the same reason the SMTP path sends multipart/alternative.
//
// Every field of Message that the SMTP path honours is honoured here too. This
// transport once built its request from the install's Config alone, so app mail
// sent over it lost its "(via Toft)" sender frame and its Reply-To, and its CC,
// BCC and attachments were dropped while the ledger said delivered.
type resendRequest struct {
	From        string             `json:"from"`
	To          []string           `json:"to"`
	CC          []string           `json:"cc,omitempty"`
	BCC         []string           `json:"bcc,omitempty"`
	Subject     string             `json:"subject"`
	HTML        string             `json:"html,omitempty"`
	Text        string             `json:"text,omitempty"`
	ReplyTo     string             `json:"reply_to,omitempty"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
}

// resendAttachment is one file, its content base64 in the JSON — which is what
// encoding/json does with a []byte.
type resendAttachment struct {
	Filename    string `json:"filename"`
	Content     []byte `json:"content"`
	ContentType string `json:"content_type,omitempty"`
	// ContentID makes the attachment inline, referenced from the HTML as
	// cid:<ContentID> — how the Toft mark rides on the API transport.
	ContentID string `json:"content_id,omitempty"`
}

func sendResend(ctx context.Context, cfg Config, msg Message) error {
	replyTo := msg.ReplyTo
	if replyTo == "" {
		replyTo = cfg.ReplyTo
	}
	var attachments []resendAttachment
	for _, att := range msg.Attachments {
		attachments = append(attachments, resendAttachment{
			Filename: att.Name, Content: att.Content, ContentType: att.Type,
		})
	}
	if usesMark(msg.HTML) {
		attachments = append(attachments, resendAttachment{
			Filename: markName, Content: markPNG, ContentType: "image/png", ContentID: markName,
		})
	}
	body, err := json.Marshal(resendRequest{
		From:        formatFrom(cfg, msg),
		To:          msg.To,
		CC:          msg.CC,
		BCC:         msg.BCC,
		Subject:     msg.Subject,
		HTML:        msg.HTML,
		Text:        msg.Text,
		ReplyTo:     replyTo,
		Attachments: attachments,
	})
	if err != nil {
		return fmt.Errorf("building the Resend request: %w", err)
	}

	endpoint := resendEndpoint
	if cfg.apiBaseURL != "" {
		endpoint = cfg.apiBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the Resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := apiClient.Do(req)
	if err != nil {
		// The URL is in here; the credential is not — it travelled in a header,
		// and net/http does not echo header values into transport errors.
		return fmt.Errorf("Resend: %w", err)
	}
	defer resp.Body.Close()
	// Read even on success: the connection is only reusable if the body is
	// drained, and a 2xx body is small.
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode/100 == 2 {
		return nil
	}
	return apiError(ProviderLabel(ProviderResend), resp.StatusCode, payload)
}

// formatFrom builds the From header value both transports agree on: the
// message's display name over the install's, and always the install's address.
// net/mail does the quoting, so a display name containing a comma cannot split
// the header.
func formatFrom(cfg Config, msg Message) string {
	name := msg.FromName
	if name == "" {
		name = cfg.FromName
	}
	if name == "" {
		return cfg.FromAddress
	}
	addr := mail.Address{Name: name, Address: cfg.FromAddress}
	return addr.String()
}

// apiError turns a provider's refusal into one line an administrator can act
// on. Every provider worth supporting returns a human-readable message in JSON;
// when it does not, the status line plus a bounded slice of the body still beats
// "the send failed".
func apiError(provider string, status int, body []byte) error {
	var parsed struct {
		Message string `json:"message"`
		Name    string `json:"name"`
		Error   string `json:"error"`
	}
	detail := ""
	if err := json.Unmarshal(body, &parsed); err == nil {
		switch {
		case parsed.Message != "":
			detail = parsed.Message
		case parsed.Error != "":
			detail = parsed.Error
		}
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
		if detail == "" {
			detail = http.StatusText(status)
		}
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
	}
	// 401 is the one refusal whose provider-supplied wording ("API key is
	// invalid") is routinely less useful than naming what to do about it.
	if status == http.StatusUnauthorized {
		return fmt.Errorf("%s rejected the API key (HTTP 401): %s", provider, detail)
	}
	err := fmt.Errorf("%s: %s (HTTP %d)", provider, detail, status)
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		// A validation answer is about this message — for a one-recipient
		// notification, about its address — not about the provider.
		return &recipientError{err: err}
	}
	return err
}
