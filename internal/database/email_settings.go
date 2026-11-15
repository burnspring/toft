package database

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Email settings live in the same `settings` table as PlatformSettings but are
// read and written as their own document, deliberately.
//
// SetPlatformSettings audits the full before/after JSON of every key it owns.
// The SMTP password must never appear in an audit row, and the cheapest way to
// guarantee that is for the password to live in a document that blob never
// touches. Keeping them apart also means "email is off" is one read, not a
// half-loaded view of an unrelated capability document.
//
// This is a settings row rather than an environment variable because how we send
// is a decision an administrator revisits — a relay moves, a password rotates, a
// team switches to a sending API — and they should not need a redeploy to do it.
// Both credentials are encrypted at rest with the same AES-256-GCM encryptor as
// auth_providers.client_secret, and, as there, ciphertext goes in and plaintext
// only ever comes out at the point of use.

const (
	SettingEmailEnabled     = "email.enabled"
	SettingEmailTransport   = "email.transport"
	SettingEmailHost        = "email.host"
	SettingEmailPort        = "email.port"
	SettingEmailSecurity    = "email.security"
	SettingEmailUsername    = "email.username"
	SettingEmailPassword    = "email.password"
	SettingEmailAPIProvider = "email.api_provider"
	SettingEmailAPIKey      = "email.api_key"
	SettingEmailFromAddress = "email.from_address"
	SettingEmailFromName    = "email.from_name"
	SettingEmailReplyTo     = "email.reply_to"
	SettingEmailSkipVerify  = "email.skip_verify"
	SettingEmailNotifyAdmin = "email.notify_admin"
	SettingEmailNotifyApp   = "email.notify_app"
	SettingEmailAdminGroup  = "email.admin_group_address"
)

// Transports. SMTP is the default and stays the one an install gets by
// accident: it is what a corporate relay speaks. The API transport exists for
// the installs where 587 is closed outbound and 443 is not.
const (
	EmailTransportSMTP = "smtp"
	EmailTransportAPI  = "api"
)

// Sending-API providers. One for now; a second is a case in mail.sendAPI plus a
// name here, not a new shape of document.
const EmailAPIProviderResend = "resend"

// ValidEmailTransport reports whether a transport is one the mailer knows.
func ValidEmailTransport(t string) bool {
	switch t {
	case EmailTransportSMTP, EmailTransportAPI:
		return true
	}
	return false
}

// ValidEmailAPIProvider reports whether a sending API is one the mailer knows.
func ValidEmailAPIProvider(p string) bool {
	return p == EmailAPIProviderResend
}

// Transport security modes. STARTTLS is the default because it is what port 587
// speaks and what almost every corporate relay expects.
const (
	EmailSecurityStartTLS    = "starttls"
	EmailSecurityImplicitTLS = "implicit_tls"
	EmailSecurityNone        = "none"
)

// ValidEmailSecurity reports whether a mode is one the mailer knows.
func ValidEmailSecurity(mode string) bool {
	switch mode {
	case EmailSecurityStartTLS, EmailSecurityImplicitTLS, EmailSecurityNone:
		return true
	}
	return false
}

// EmailSettings is the sending document. Whichever transport is selected, the
// other one's fields are kept rather than cleared: an administrator who tries
// the API and goes back to the relay should find the relay still filled in.
// Password and APIKey carry ciphertext everywhere except inside the mailer,
// which decrypts one of them immediately before sending.
type EmailSettings struct {
	Enabled bool `json:"enabled"`
	// Transport is smtp or api. It decides which half of this document is
	// load-bearing and which is dormant.
	Transport string `json:"transport"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Security  string `json:"security"`
	Username  string `json:"username"`
	// Password is the stored SMTP ciphertext. It is never marshalled to an API
	// client — the handler builds its own DTO — and never written to audit.
	Password    string `json:"-"`
	APIProvider string `json:"api_provider"`
	// APIKey is the stored sending-API ciphertext, held apart from Password
	// rather than sharing one credential slot: switching transport must not
	// quietly present an SMTP password to a provider's API as a bearer token.
	APIKey      string `json:"-"`
	FromAddress string `json:"from_address"`
	FromName    string `json:"from_name"`
	ReplyTo     string `json:"reply_to"`
	// SkipVerify accepts a self-signed relay certificate. Off by default; the
	// UI says what it costs. SMTP only — a public sending API has a public CA
	// certificate, and there is no reason to offer to stop checking it.
	SkipVerify bool `json:"skip_verify"`

	// NotifyAdmin and NotifyApp switch off a whole audience without switching
	// off email, and default on so that turning email on means what it says.
	// They are separate because the two complaints are separate: an operations
	// team that wants the platform silent towards its users is not the same as
	// an administrator who wants their own inbox quiet. Neither affects the
	// test send, which is a diagnostic rather than a notification.
	// The gate itself lives in mail.Service, which knows the audience names.
	NotifyAdmin bool `json:"notify_admin"`
	NotifyApp   bool `json:"notify_app"`

	// AdminGroupAddress is where administrator notifications go instead of to
	// each administrator's own address. Set it to a distribution list and the
	// install stops caring which people hold the role — which is the point: an
	// admin team that rotates should not have to remember that adding someone
	// to the role is also how they start getting mail.
	//
	// Exactly one address. Fanning out to several people is what a distribution
	// list already does, and doing it here as well would be a second, worse mail
	// system with no membership, no archive and no way to unsubscribe.
	//
	// Empty means not set, and the individual administrators are used — the same
	// "absent means not offered" rule the rest of this document follows. It
	// replaces them rather than joining them: an alias that already contains the
	// administrators would otherwise deliver twice.
	//
	// It is deliberately not a credential and not a switch: it changes *where*
	// administrator mail goes, never *whether* it is sent. NotifyAdmin still
	// decides that, and is applied first — silencing the audience silences it for
	// the group address too.
	AdminGroupAddress string `json:"admin_group_address"`
}

// DefaultEmailSettings is what an install starts as: email off, and nothing
// configured. Absent means not offered — no notification is sent, no route
// pretends otherwise, and nothing errors.
func DefaultEmailSettings() EmailSettings {
	return EmailSettings{
		Enabled:   false,
		Transport: EmailTransportSMTP,
		Port:      587,
		Security:  EmailSecurityStartTLS,
		// A provider is preselected so the form has a coherent state the moment
		// someone flips the transport; it means nothing until a key is stored.
		APIProvider: EmailAPIProviderResend,
		// Both audiences on: an install that turns email on gets the email the
		// features describe. Absent keys — every install that predates these —
		// therefore keep behaving exactly as they did.
		NotifyAdmin: true,
		NotifyApp:   true,
	}
}

// GetEmailSettings reads the document, filling anything unset from defaults.
// The returned Password is ciphertext.
func (db *DB) GetEmailSettings() (EmailSettings, error) {
	s := DefaultEmailSettings()

	rows, err := db.conn.Query(
		`SELECT key, value FROM settings WHERE key IN (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		SettingEmailEnabled, SettingEmailTransport, SettingEmailHost, SettingEmailPort,
		SettingEmailSecurity, SettingEmailUsername, SettingEmailPassword,
		SettingEmailAPIProvider, SettingEmailAPIKey, SettingEmailFromAddress,
		SettingEmailFromName, SettingEmailReplyTo, SettingEmailSkipVerify,
		SettingEmailNotifyAdmin, SettingEmailNotifyApp, SettingEmailAdminGroup)
	if err != nil {
		return s, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return s, err
		}
		switch key {
		case SettingEmailEnabled:
			s.Enabled = value == "true"
		case SettingEmailTransport:
			if ValidEmailTransport(value) {
				s.Transport = value
			}
		case SettingEmailHost:
			s.Host = value
		case SettingEmailPort:
			if n, err := strconv.Atoi(value); err == nil {
				s.Port = n
			}
		case SettingEmailSecurity:
			if ValidEmailSecurity(value) {
				s.Security = value
			}
		case SettingEmailUsername:
			s.Username = value
		case SettingEmailPassword:
			s.Password = value
		case SettingEmailAPIProvider:
			if ValidEmailAPIProvider(value) {
				s.APIProvider = value
			}
		case SettingEmailAPIKey:
			s.APIKey = value
		case SettingEmailFromAddress:
			s.FromAddress = value
		case SettingEmailFromName:
			s.FromName = value
		case SettingEmailReplyTo:
			s.ReplyTo = value
		case SettingEmailSkipVerify:
			s.SkipVerify = value == "true"
		case SettingEmailNotifyAdmin:
			s.NotifyAdmin = value == "true"
		case SettingEmailNotifyApp:
			s.NotifyApp = value == "true"
		case SettingEmailAdminGroup:
			s.AdminGroupAddress = value
		}
	}
	return s, rows.Err()
}

// SetEmailSettings writes the document and audits the change in the same
// transaction.
//
// s.Password and s.APIKey must already be ciphertext; the caller encrypts,
// because only the API layer holds the encryptor. The audit blob replaces each
// credential with two booleans — whether one is set, and whether this write
// changed it — so the question the audit log exists to answer ("who pointed us
// at a new relay or sending API, and did they rotate the credential") is
// answerable without the credential.
func (db *DB) SetEmailSettings(s *EmailSettings, actorID string, meta RequestMeta) error {
	before, err := db.GetEmailSettings()
	if err != nil {
		return err
	}

	s.Host = strings.TrimSpace(s.Host)
	s.Username = strings.TrimSpace(s.Username)
	s.FromAddress = strings.TrimSpace(s.FromAddress)
	s.FromName = strings.TrimSpace(s.FromName)
	s.ReplyTo = strings.TrimSpace(s.ReplyTo)
	s.AdminGroupAddress = strings.TrimSpace(s.AdminGroupAddress)
	if !ValidEmailSecurity(s.Security) {
		s.Security = EmailSecurityStartTLS
	}
	if !ValidEmailTransport(s.Transport) {
		s.Transport = EmailTransportSMTP
	}
	if !ValidEmailAPIProvider(s.APIProvider) {
		s.APIProvider = EmailAPIProviderResend
	}
	if s.Port == 0 {
		s.Port = 587
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	write := func(key, value string) error {
		_, err := tx.Exec(`
			INSERT INTO settings (key, value, updated_at, updated_by)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET
				value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
			key, value, now, nullIfEmpty(actorID))
		return err
	}

	for _, kv := range []struct{ key, value string }{
		{SettingEmailEnabled, boolText(s.Enabled)},
		{SettingEmailTransport, s.Transport},
		{SettingEmailHost, s.Host},
		{SettingEmailPort, strconv.Itoa(s.Port)},
		{SettingEmailSecurity, s.Security},
		{SettingEmailUsername, s.Username},
		{SettingEmailPassword, s.Password},
		{SettingEmailAPIProvider, s.APIProvider},
		{SettingEmailAPIKey, s.APIKey},
		{SettingEmailFromAddress, s.FromAddress},
		{SettingEmailFromName, s.FromName},
		{SettingEmailReplyTo, s.ReplyTo},
		{SettingEmailSkipVerify, boolText(s.SkipVerify)},
		{SettingEmailNotifyAdmin, boolText(s.NotifyAdmin)},
		{SettingEmailNotifyApp, boolText(s.NotifyApp)},
		{SettingEmailAdminGroup, s.AdminGroupAddress},
	} {
		if err := write(kv.key, kv.value); err != nil {
			return err
		}
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "email_settings.update",
		TargetType: "platform", TargetID: "email",
		Before:    jsonStr(emailAudit(before, before.Password != "", before.APIKey != "")),
		After:     jsonStr(emailAudit(*s, s.Password != before.Password, s.APIKey != before.APIKey)),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing email settings change: %w", err)
	}

	return tx.Commit()
}

// emailAudit is the redacted shape written to audit_log. It is built by hand
// rather than by marshalling EmailSettings so that adding a field to the struct
// cannot silently start leaking it.
func emailAudit(s EmailSettings, passwordChanged, apiKeyChanged bool) map[string]interface{} {
	return map[string]interface{}{
		"enabled":      s.Enabled,
		"transport":    s.Transport,
		"host":         s.Host,
		"port":         s.Port,
		"security":     s.Security,
		"username":     s.Username,
		"api_provider": s.APIProvider,
		"from_address": s.FromAddress,
		"from_name":    s.FromName,
		"reply_to":     s.ReplyTo,
		"skip_verify":  s.SkipVerify,
		"notify_admin": s.NotifyAdmin,
		"notify_app":   s.NotifyApp,
		// In the clear, and deliberately: this is not a credential, and "who
		// redirected every administrator notification to an address of their
		// own" is precisely the question this log exists to answer. Redacting it
		// would leave the change visible and its substance invisible.
		"admin_group_address": s.AdminGroupAddress,
		"password_set":        s.Password != "",
		"password_changed":    passwordChanged,
		"api_key_set":         s.APIKey != "",
		"api_key_changed":     apiKeyChanged,
	}
}

// AdminRecipients returns where administrator notifications go: the admin group
// address alone when one is set, otherwise every administrator who can receive
// mail.
//
// One function, because two callers need the same answer and must not be able to
// disagree about it — the mailer, deciding whether a message has anywhere to go,
// and any handler deciding whether to warn that nobody will be told. The
// re-addressing itself happens in mail.Service.SendAsync, which already holds
// the settings and so applies the rule without a second query.
//
// An empty result means nobody would be reached at all: no group address, and no
// unlocked administrator with an address on file.
func (db *DB) AdminRecipients() ([]string, error) {
	settings, err := db.GetEmailSettings()
	if err != nil {
		return nil, err
	}
	if settings.AdminGroupAddress != "" {
		return []string{settings.AdminGroupAddress}, nil
	}
	admins, err := db.ListAdminRecipients()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(admins))
	for _, a := range admins {
		out = append(out, a.Email)
	}
	return out, nil
}

// ListAdminRecipients returns the email addresses of every administrator who
// can actually receive mail: unlocked, and with an address on file.
//
// A locked admin is excluded for the same reason their session is: the account
// is out of service. An admin with no address is excluded because users.email
// is provider-supplied and a provider may assert none — the caller warns when
// the list comes back empty rather than assuming someone was reached.
//
// The built-in administrator's placeholder (BuiltinAdminEmail, an unresolvable
// .local address no handler can change) is left out too: on its own it made
// every install's list non-empty, so the "nobody to tell" warning never fired,
// and a relay that refuses it bounced every administrator notice. Matched by
// the address, not the account, so an address somebody set there on purpose
// still counts.
func (db *DB) ListAdminRecipients() ([]User, error) {
	rows, err := db.conn.Query(`
		SELECT id, login, name, COALESCE(email,'')
		FROM users
		WHERE system_role='system_admin' AND locked_at IS NULL
		  AND email IS NOT NULL AND email <> '' AND email <> ?
		ORDER BY login`, BuiltinAdminEmail)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Login, &u.Name, &u.Email); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
