package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/mail"
	"strings"

	"toft/internal/auth"
	"toft/internal/database"
	mailer "toft/internal/mail"
)

// Email settings are administered here. The credential — SMTP password or
// sending-API key — goes in and never comes out, same contract as an auth
// provider's client secret and for the same reason: an admin API that can read
// back a credential is a credential store with an HTTP interface.
//
// Non-admins get 404, not 403, matching handleGetSettings: whether this install
// can send mail, and through whose relay, is infrastructure detail.

// Mailer is the Server's view of the mail service. An interface rather than the
// concrete type so tests can record what would have been sent without a relay,
// and so a nil one — the default — makes every notification a no-op instead of
// a panic.
type Mailer interface {
	// Enabled reports whether a relay is configured and switched on.
	Enabled() bool
	// BaseURL is where links in email should point.
	BaseURL() string
	// SendAsync delivers in the background and never fails the caller.
	SendAsync(mailer.Message)
	// AdminRecipients is where administrator mail will land — the group address
	// when one is set, otherwise the individual administrators. For deciding
	// whether a notification has anywhere to go, never for addressing it:
	// SendAsync applies the group address itself.
	AdminRecipients() ([]string, error)
	// SendTest delivers synchronously and returns the relay's own error.
	SendTest(ctx context.Context, to string) error
	// AppNotificationsEnabled reports whether a message to the people who *use*
	// this install would actually go out — configured, on, and the app audience
	// not silenced. For deciding whether to **offer** a send, which is a
	// question only the share notice asks: everything else is sent because
	// something happened, and a silenced audience is correctly a silent drop.
	AppNotificationsEnabled() bool
}

// WithMailer supplies the email service. Absent — which is every test that does
// not ask for one — the platform still records and shows every request it would
// have notified about; it just says nothing out of band.
func WithMailer(m Mailer) ServerOption {
	return func(s *Server) { s.mailer = m }
}

type emailSettingsResponse struct {
	Enabled     bool   `json:"enabled"`
	Transport   string `json:"transport"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"`
	Username    string `json:"username"`
	APIProvider string `json:"api_provider"`
	FromAddress string `json:"from_address"`
	FromName    string `json:"from_name"`
	ReplyTo     string `json:"reply_to"`
	SkipVerify  bool   `json:"skip_verify"`
	NotifyAdmin bool   `json:"notify_admin"`
	NotifyApp   bool   `json:"notify_app"`
	// AdminGroupAddress is readable in the clear. It is not a credential, and an
	// administrator cannot reason about where their install's alerts go if the
	// field they are meant to manage comes back blanked.
	AdminGroupAddress string `json:"admin_group_address"`
	// PasswordSet and APIKeySet are all the client learns about either stored
	// credential: whether there is one. The form uses them to decide between
	// "enter it" and "leave blank to keep the stored one".
	PasswordSet bool `json:"password_set"`
	APIKeySet   bool `json:"api_key_set"`
	// …and whether the stored one can still be read. Set-but-unreadable is a
	// lost or changed ENCRYPTION_KEY, and it is the state where "leave blank to
	// keep" quietly keeps nothing. See credentials.go.
	PasswordUnreadable bool `json:"password_unreadable"`
	APIKeyUnreadable   bool `json:"api_key_unreadable"`
	// TestRecipient is the address the test button would send to — the caller's
	// own. Empty when the caller has no address on file, which is what the UI
	// needs to explain instead of offering a button that cannot work.
	TestRecipient string `json:"test_recipient"`
}

func (srv *Server) emailSettingsDTO(s database.EmailSettings, testRecipient string) emailSettingsResponse {
	return emailSettingsResponse{
		Enabled: s.Enabled, Transport: s.Transport, Host: s.Host, Port: s.Port,
		Security: s.Security, Username: s.Username, APIProvider: s.APIProvider,
		FromAddress: s.FromAddress, FromName: s.FromName,
		ReplyTo: s.ReplyTo, SkipVerify: s.SkipVerify,
		NotifyAdmin:        s.NotifyAdmin,
		NotifyApp:          s.NotifyApp,
		AdminGroupAddress:  s.AdminGroupAddress,
		PasswordSet:        s.Password != "",
		APIKeySet:          s.APIKey != "",
		PasswordUnreadable: srv.credentialUnreadable(s.Password),
		APIKeyUnreadable:   srv.credentialUnreadable(s.APIKey),
		TestRecipient:      testRecipient,
	}
}

func (s *Server) handleGetEmailSettings(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	settings, err := s.db.GetEmailSettings()
	if err != nil {
		log.Printf("email settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.emailSettingsDTO(settings, s.callerEmail(p)))
}

type updateEmailSettingsRequest struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	Transport   *string `json:"transport,omitempty"`
	Host        *string `json:"host,omitempty"`
	Port        *int    `json:"port,omitempty"`
	Security    *string `json:"security,omitempty"`
	Username    *string `json:"username,omitempty"`
	APIProvider *string `json:"api_provider,omitempty"`
	FromAddress *string `json:"from_address,omitempty"`
	FromName    *string `json:"from_name,omitempty"`
	ReplyTo     *string `json:"reply_to,omitempty"`
	SkipVerify  *bool   `json:"skip_verify,omitempty"`
	NotifyAdmin *bool   `json:"notify_admin,omitempty"`
	NotifyApp   *bool   `json:"notify_app,omitempty"`
	// AdminGroupAddress, unlike the two credentials, *can* be cleared by sending
	// "": the empty state is a meaningful one an administrator has to be able to
	// get back to (notify each administrator individually), and there is nothing
	// to lose by clearing it accidentally.
	AdminGroupAddress *string `json:"admin_group_address,omitempty"`
	// Password and APIKey omitted or empty keep the stored one. There is
	// deliberately no way to say "clear it" and mean it by sending "" —
	// clearing a credential is what turning email off is for, and an empty
	// string is far more often a form that did not know it should omit the
	// field.
	Password *string `json:"password,omitempty"`
	APIKey   *string `json:"api_key,omitempty"`
}

func (s *Server) handleUpdateEmailSettings(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	var req updateEmailSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	settings, err := s.db.GetEmailSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if req.Enabled != nil {
		settings.Enabled = *req.Enabled
	}
	if req.Transport != nil {
		v := strings.TrimSpace(*req.Transport)
		if !database.ValidEmailTransport(v) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "transport must be smtp or api",
				"hint":  `send "transport": "smtp", or "api" to send through Resend`})
			return
		}
		settings.Transport = v
	}
	if req.Host != nil {
		settings.Host = strings.TrimSpace(*req.Host)
	}
	if req.Port != nil {
		settings.Port = *req.Port
	}
	if req.Security != nil {
		v := strings.TrimSpace(*req.Security)
		if !database.ValidEmailSecurity(v) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "security must be starttls, implicit_tls, or none",
				"hint":  `send "security": "starttls" for port 587, or "implicit_tls" for port 465`})
			return
		}
		settings.Security = v
	}
	if req.Username != nil {
		settings.Username = strings.TrimSpace(*req.Username)
	}
	if req.APIProvider != nil {
		v := strings.TrimSpace(*req.APIProvider)
		if !database.ValidEmailAPIProvider(v) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "resend is the only sending API this install can use",
				"hint":  `send "api_provider": "resend"`})
			return
		}
		settings.APIProvider = v
	}
	if req.FromAddress != nil {
		settings.FromAddress = strings.TrimSpace(*req.FromAddress)
	}
	if req.FromName != nil {
		settings.FromName = strings.TrimSpace(*req.FromName)
	}
	if req.ReplyTo != nil {
		settings.ReplyTo = strings.TrimSpace(*req.ReplyTo)
	}
	if req.SkipVerify != nil {
		settings.SkipVerify = *req.SkipVerify
	}
	if req.NotifyAdmin != nil {
		settings.NotifyAdmin = *req.NotifyAdmin
	}
	if req.NotifyApp != nil {
		settings.NotifyApp = *req.NotifyApp
	}
	if req.AdminGroupAddress != nil {
		settings.AdminGroupAddress = strings.TrimSpace(*req.AdminGroupAddress)
	}
	if req.Password != nil && *req.Password != "" {
		ciphertext, err := s.encryptor.Encrypt(*req.Password)
		if err != nil {
			log.Printf("email settings: encrypting password: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		settings.Password = ciphertext
	}
	if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
		// Trimmed because a key is pasted, and a pasted key routinely arrives
		// with a trailing newline that the provider would reject as a malformed
		// bearer token.
		ciphertext, err := s.encryptor.Encrypt(strings.TrimSpace(*req.APIKey))
		if err != nil {
			log.Printf("email settings: encrypting API key: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		settings.APIKey = ciphertext
	}

	// The admin group address is checked whichever way the switch is set, like
	// the port range below and unlike the relay fields. The difference is that a
	// blank relay host is a form still being filled in, whereas a string that is
	// not an email address is never going to become one — and this one silently
	// misroutes every administrator notification rather than failing loudly.
	if settings.AdminGroupAddress != "" {
		if _, err := mail.ParseAddress(settings.AdminGroupAddress); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "the admin notification address is not a valid email address",
				"hint":  `send one address, such as "admin_group_address": "it-admins@example.com", or "" for none`})
			return
		}
	}

	// Validation runs only when email is on. A half-filled form saved with the
	// switch off is how an administrator works — fill it in, turn it on when
	// the relay details arrive — and refusing that would make the off state
	// harder to reach than the on one.
	if settings.Enabled {
		if msg := validateEmailSettings(settings); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "hint": `fill in the field the sentence names and save again, or save with "enabled": false`})
			return
		}
	} else if settings.Port < 0 || settings.Port > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "port must be between 1 and 65535", "hint": `send a port such as "port": 587`})
		return
	}

	if err := s.db.SetEmailSettings(&settings, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("email settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.emailSettingsDTO(settings, s.callerEmail(p)))
}

// validateEmailSettings returns an empty string when the settings could
// plausibly deliver. It repeats internal/mail's Validate deliberately: this one
// speaks to an administrator filling in a form, that one guards the send.
//
// Only the selected transport is checked. An install that has moved to a sending
// API must not be held to a half-filled relay it no longer uses, and vice versa.
func validateEmailSettings(s database.EmailSettings) string {
	if s.Transport == database.EmailTransportAPI {
		if !database.ValidEmailAPIProvider(s.APIProvider) {
			return "resend is the only sending API this install can use"
		}
		if s.APIKey == "" {
			return "an API key is required to send through Resend"
		}
	} else if msg := validateEmailSMTP(s); msg != "" {
		return msg
	}
	if s.FromAddress == "" {
		return "a From address is required — it is what recipients will see and reply to"
	}
	if _, err := mail.ParseAddress(s.FromAddress); err != nil {
		return "the From address is not a valid email address"
	}
	if s.ReplyTo != "" {
		if _, err := mail.ParseAddress(s.ReplyTo); err != nil {
			return "the Reply-To address is not a valid email address"
		}
	}
	return ""
}

func validateEmailSMTP(s database.EmailSettings) string {
	if s.Host == "" {
		return "an SMTP host is required to turn email on"
	}
	if s.Port < 1 || s.Port > 65535 {
		return "port must be between 1 and 65535"
	}
	if !database.ValidEmailSecurity(s.Security) {
		return "security must be starttls, implicit_tls, or none"
	}
	return ""
}

// handleTestEmail sends to the caller's own address, using the *saved*
// settings.
//
// Their own address, because a test that can be pointed anywhere is a way to
// make the platform send mail to a stranger. Saved settings, because what an
// administrator needs to know is whether the configuration this install will
// actually use works — testing an unsaved form proves something about a form.
func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	if s.mailer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this install was started without email support"})
		return
	}
	if !s.mailer.Enabled() {
		// Not a relay failure, so not a 502: nothing was dialled. The saved
		// settings say email is off, or are incomplete enough that they could
		// not deliver anything.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "email is off, or the saved settings are incomplete — turn it on and save first",
			"hint":  `PUT /api/v1/admin/email-settings with "enabled": true and the relay's details, then test again`})
		return
	}
	to := s.callerEmail(p)
	if to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "your account has no email address on file, so there is nowhere to send the test",
			"hint":  "sign in with an account that has an email address and send the test from there"})
		return
	}
	if err := s.mailer.SendTest(r.Context(), to); err != nil {
		// The relay's own words. An administrator debugging a mail path needs
		// "535 authentication failed", not "internal error".
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"sent_to": to})
}

// callerEmail is the principal's address, or empty when they have none. Service
// accounts never have one; users.email is provider-supplied and may be blank.
func (s *Server) callerEmail(p *auth.Principal) string {
	if p == nil || p.Type != database.PrincipalTypeUser {
		return ""
	}
	u, err := s.db.GetUser(p.ID)
	if err != nil || u == nil {
		return ""
	}
	return strings.TrimSpace(u.Email)
}
