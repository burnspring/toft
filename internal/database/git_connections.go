package database

import (
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Connected git hosts: the row an administrator creates once so that apps stop
// carrying credentials of their own.
//
// The three ciphertexts on the row are unexported, for the reason
// `AuthProvider.secret` is: a struct that cannot marshal a field cannot leak it
// through a handler that forgot to build a DTO. Encryption happens in the API
// layer, which owns the key; this package persists and nothing more.

// Connection kinds — which host.
const (
	GitConnGitHub = "github"
	GitConnGitLab = "gitlab"
)

// Auth methods — how it authenticates. They vary independently of the kind,
// which is why they are two columns and not one enum of pairs.
const (
	// GitAuthApp signs a JWT with a private key and exchanges it for a
	// short-lived installation token, narrowed to the repository about to be
	// cloned. What is at rest is a signing key, worth nothing unless the host
	// agrees to mint.
	GitAuthApp = "app"
	// GitAuthToken uses a stored access token as it is. What is at rest is
	// repository read access, usable the moment the ciphertext is opened.
	GitAuthToken = "token"
)

// Credential kinds, for the one sentence the admin page says about reach. The
// prefix of a GitHub token says which it is, so the page can state it without
// asking anybody.
const (
	// GitCredFineGrained is `github_pat_` — scoped to selected repositories.
	GitCredFineGrained = "fine_grained"
	// GitCredClassic is `ghp_` — everything its owner can see.
	GitCredClassic = "classic"
	// GitCredGroup is a GitLab group access token, which belongs to the group
	// rather than to a person.
	GitCredGroup = "group"
	// GitCredApp is the App method's signing key, which reaches whatever the
	// installation was granted and nothing else.
	GitCredApp = "app"
)

var (
	// ErrGitConnectionNotFound means no such connection row.
	ErrGitConnectionNotFound = errors.New("git connection not found")
	// ErrGitConnectionInUse means apps still point at it. Distinct so the API
	// can answer 409 and name them rather than returning a foreign-key error.
	ErrGitConnectionInUse = errors.New("apps still deploy through this connection")
)

// GitConnection is one connected git host.
type GitConnection struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	BaseURL     string `json:"base_url"`
	AuthMethod  string `json:"auth_method"`
	Enabled     bool   `json:"enabled"`

	// GitHubAppID is the GitHub App's own identifier, and the column behind it
	// is `github_app_id`. It was `app_id`, in a schema where that name means a
	// Toft app in twenty-one other tables — the one place the schema stated
	// something false. The JSON tag stays `app_id` because it is on the wire:
	// the admin form posts it, and a field name is a different document from a
	// column name.
	GitHubAppID    string `json:"app_id,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
	ClientID       string `json:"client_id,omitempty"`

	// CredentialKind, CredentialOwner and CredentialExpiresAt are what the
	// admin page states about a stored token's provenance, reach and expiry —
	// the three things the weaker auth method gives up, said on the page and
	// not only in a design document.
	CredentialKind      string     `json:"credential_kind,omitempty"`
	CredentialOwner     string     `json:"credential_owner,omitempty"`
	CredentialExpiresAt *time.Time `json:"credential_expires_at,omitempty"`

	// Coverage is the newline-separated pattern list a `token` connection is
	// bounded by. Empty under `app`, where the host is asked instead.
	Coverage string `json:"coverage,omitempty"`

	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	LastOKAt    *time.Time `json:"last_ok_at,omitempty"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// AppCount is how many apps deploy through it — what an administrator
	// needs before disabling it and what a removal is refused on.
	AppCount int `json:"app_count"`

	// CloneSecretSet and WebhookSecretSet report that a credential is stored,
	// which is the only thing about either any caller may learn.
	CloneSecretSet   bool `json:"clone_secret_set"`
	WebhookSecretSet bool `json:"webhook_secret_set"`
	ClientSecretSet  bool `json:"client_secret_set"`

	clientSecret  string
	cloneSecret   string
	webhookSecret string
}

// EncryptedCloneSecret returns the stored ciphertext for the one caller that
// holds the decryption key. Named for what it is so no call site mistakes it
// for a usable credential.
func (c *GitConnection) EncryptedCloneSecret() string { return c.cloneSecret }

// EncryptedClientSecret is the picker's OAuth client secret, ciphertext.
func (c *GitConnection) EncryptedClientSecret() string { return c.clientSecret }

// EncryptedWebhookSecret is the HMAC key a delivery is verified against,
// ciphertext.
func (c *GitConnection) EncryptedWebhookSecret() string { return c.webhookSecret }

// SetEncryptedCloneSecret stores already-encrypted ciphertext.
func (c *GitConnection) SetEncryptedCloneSecret(ciphertext string) {
	c.cloneSecret = ciphertext
	c.CloneSecretSet = ciphertext != ""
}

// SetEncryptedClientSecret stores already-encrypted ciphertext.
func (c *GitConnection) SetEncryptedClientSecret(ciphertext string) {
	c.clientSecret = ciphertext
	c.ClientSecretSet = ciphertext != ""
}

// SetEncryptedWebhookSecret stores already-encrypted ciphertext.
func (c *GitConnection) SetEncryptedWebhookSecret(ciphertext string) {
	c.webhookSecret = ciphertext
	c.WebhookSecretSet = ciphertext != ""
}

// HasPicker reports whether a developer gets a repository list rather than a
// URL field.
//
// **It is the auth method and nothing else.** The picker signs a person in
// against the host as themselves, which needs an OAuth client; a GitHub App is
// one, at no extra cost to the administrator, and a pasted token is not. There
// is no separate field, because an optional OAuth application registered
// alongside a token connection would be a second artifact to create on the very
// kind chosen because creating things was the obstacle.
func (c *GitConnection) HasPicker() bool {
	return c.AuthMethod == GitAuthApp && c.ClientID != ""
}

// CoverageList splits the stored newline-separated form.
func (c *GitConnection) CoverageList() []string {
	out := []string{}
	for _, line := range strings.Split(c.Coverage, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}

const gitConnectionColumns = `id, kind, display_name, base_url, auth_method, enabled,
	github_app_id, installation_id, client_id, client_secret, clone_secret,
	credential_kind, credential_owner, credential_expires_at,
	webhook_secret, coverage,
	last_error, last_error_at, last_ok_at,
	COALESCE(created_by, ''), created_at, updated_at`

func scanGitConnection(scan func(...any) error) (*GitConnection, error) {
	c := &GitConnection{}
	err := scan(&c.ID, &c.Kind, &c.DisplayName, &c.BaseURL, &c.AuthMethod, &c.Enabled,
		&c.GitHubAppID, &c.InstallationID, &c.ClientID, &c.clientSecret, &c.cloneSecret,
		&c.CredentialKind, &c.CredentialOwner, &c.CredentialExpiresAt,
		&c.webhookSecret, &c.Coverage,
		&c.LastError, &c.LastErrorAt, &c.LastOKAt,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.CloneSecretSet = c.cloneSecret != ""
	c.ClientSecretSet = c.clientSecret != ""
	c.WebhookSecretSet = c.webhookSecret != ""
	return c, nil
}

// ListGitConnections returns every connection, enabled or not, with the number
// of apps deploying through each.
func (db *DB) ListGitConnections() ([]GitConnection, error) {
	rows, err := db.conn.Query(`SELECT ` + gitConnectionColumns + `,
		(SELECT COUNT(*) FROM apps a WHERE a.git_connection_id = git_connections.id)
		FROM git_connections ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GitConnection{}
	for rows.Next() {
		var count int
		c, err := scanGitConnection(func(dest ...any) error {
			return rows.Scan(append(dest, &count)...)
		})
		if err != nil {
			return nil, err
		}
		c.AppCount = count
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetGitConnection returns one connection, or ErrGitConnectionNotFound.
//
// **It is read live on every resolve.** The resolver holds no cached `enabled`,
// which is what makes a disable take effect on the next mint rather than on the
// next restart.
func (db *DB) GetGitConnection(id string) (*GitConnection, error) {
	c, err := scanGitConnection(db.conn.QueryRow(
		`SELECT `+gitConnectionColumns+` FROM git_connections WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGitConnectionNotFound
	}
	return c, err
}

// CreateGitConnection writes the row and its audit entry in one transaction.
func (db *DB) CreateGitConnection(c *GitConnection, audit AuditEntry) error {
	c.CreatedAt = time.Now().UTC()
	c.UpdatedAt = c.CreatedAt

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO git_connections (id, kind, display_name, base_url, auth_method, enabled,
			github_app_id, installation_id, client_id, client_secret, clone_secret,
			credential_kind, credential_owner, credential_expires_at,
			webhook_secret, coverage, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Kind, c.DisplayName, c.BaseURL, c.AuthMethod, c.Enabled,
		c.GitHubAppID, c.InstallationID, c.ClientID, c.clientSecret, c.cloneSecret,
		c.CredentialKind, c.CredentialOwner, c.CredentialExpiresAt,
		c.webhookSecret, c.Coverage,
		nullIfEmpty(c.CreatedBy), c.CreatedAt, c.UpdatedAt); err != nil {
		return err
	}

	audit.Action = "git_connection.created"
	audit.TargetType = "git_connection"
	audit.TargetID = c.ID
	audit.After = jsonStr(c)
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// SameGitHost reports whether two connection base URLs name one host and port —
// the unit a stored credential belongs to. An empty or unparseable URL is
// compared as written.
func SameGitHost(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "http") {
			return "80"
		}
		return "443"
	}
	return strings.EqualFold(ua.Hostname(), ub.Hostname()) && port(ua) == port(ub)
}

// UpdateGitConnection rewrites every mutable column. The kind is not one of
// them: it decides which host client and which mint path the connection uses,
// and changing it under a stored credential would be a different connection
// wearing the same id.
//
// An empty ciphertext means "keep the stored one" — a form never receives the
// current secret back, so an empty field cannot be allowed to mean "clear it".
func (db *DB) UpdateGitConnection(c *GitConnection, audit AuditEntry) error {
	before, err := db.GetGitConnection(c.ID)
	if err != nil {
		return err
	}
	if c.clientSecret == "" {
		c.clientSecret = before.clientSecret
	}
	if c.cloneSecret == "" {
		c.cloneSecret = before.cloneSecret
	}
	if c.webhookSecret == "" {
		c.webhookSecret = before.webhookSecret
	}
	c.Kind = before.Kind
	c.CreatedAt = before.CreatedAt
	c.CreatedBy = before.CreatedBy
	c.UpdatedAt = time.Now().UTC()
	c.CloneSecretSet = c.cloneSecret != ""
	c.ClientSecretSet = c.clientSecret != ""
	c.WebhookSecretSet = c.webhookSecret != ""

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// **A new host is a new set of people.** Each stored picker token was
	// issued by the old host, and the picker would send it to the new one the
	// next time its holder opened it (security review 2026-11-14, DG-9).
	if !SameGitHost(before.BaseURL, c.BaseURL) {
		if _, err := tx.Exec(`DELETE FROM git_connection_identities WHERE connection_id = ?`, c.ID); err != nil {
			return err
		}
	}

	res, err := tx.Exec(`
		UPDATE git_connections SET display_name = ?, base_url = ?, auth_method = ?,
			enabled = ?, github_app_id = ?, installation_id = ?, client_id = ?,
			client_secret = ?, clone_secret = ?, credential_kind = ?,
			credential_owner = ?, credential_expires_at = ?, webhook_secret = ?,
			coverage = ?, updated_at = ?
		WHERE id = ?`,
		c.DisplayName, c.BaseURL, c.AuthMethod, c.Enabled, c.GitHubAppID, c.InstallationID,
		c.ClientID, c.clientSecret, c.cloneSecret, c.CredentialKind,
		c.CredentialOwner, c.CredentialExpiresAt, c.webhookSecret, c.Coverage,
		c.UpdatedAt, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGitConnectionNotFound
	}

	audit.Action = "git_connection.updated"
	if before.Enabled && !c.Enabled {
		audit.Action = "git_connection.disabled"
	}
	audit.TargetType = "git_connection"
	audit.TargetID = c.ID
	audit.Before = jsonStr(before)
	audit.After = jsonStr(c)
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// RotateGitConnectionWebhookSecret replaces the stored webhook secret with an
// already-encrypted new one. The old secret stops verifying with this write.
//
// Its own method and its own audit action rather than an UpdateGitConnection:
// that writer reads an empty ciphertext as "keep", and a rotation is the one
// write whose whole point is that the old value does not survive.
func (db *DB) RotateGitConnectionWebhookSecret(id, ciphertext string, audit AuditEntry) error {
	if ciphertext == "" {
		return errors.New("rotating a webhook secret needs a new one")
	}
	return db.setGitConnectionWebhookSecret(id, ciphertext, audit)
}

// ClearGitConnectionWebhookSecret turns the connection's webhook off: with no
// secret, no delivery verifies, and the poller is the only way a push deploys
// — as it is on every connection nobody set a webhook up for.
func (db *DB) ClearGitConnectionWebhookSecret(id string, audit AuditEntry) error {
	return db.setGitConnectionWebhookSecret(id, "", audit)
}

// setGitConnectionWebhookSecret writes the secret and its audit row together.
// The action says which of the three it was: set up (none before), replaced, or
// turned off.
func (db *DB) setGitConnectionWebhookSecret(id, ciphertext string, audit AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var had string
	if err := tx.QueryRow(`SELECT webhook_secret FROM git_connections WHERE id = ?`, id).Scan(&had); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGitConnectionNotFound
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE git_connections SET webhook_secret = ?, updated_at = ? WHERE id = ?`,
		ciphertext, time.Now().UTC(), id); err != nil {
		return err
	}
	switch {
	case ciphertext == "":
		audit.Action = "git_connection.webhook_disabled"
	case had == "":
		audit.Action = "git_connection.webhook_enabled"
	default:
		audit.Action = "git_connection.webhook_secret_rotated"
	}
	audit.TargetType = "git_connection"
	audit.TargetID = id
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteGitConnection removes the row, and refuses while any app deploys
// through it.
//
// **Refused rather than cascaded, and refused rather than nulled.** Deleting
// the credential six teams deploy through is not a configuration change, and
// there is already a verb for turning it off immediately: disable, which is
// never refused. Separating them is what keeps both properties — off means now,
// and a row cannot be deleted out from under an estate by accident.
func (db *DB) DeleteGitConnection(id string, audit AuditEntry) error {
	before, err := db.GetGitConnection(id)
	if err != nil {
		return err
	}
	apps, err := db.AppsUsingGitConnection(id)
	if err != nil {
		return err
	}
	if len(apps) > 0 {
		return ErrGitConnectionInUse
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM git_connections WHERE id = ?`, id); err != nil {
		return err
	}

	audit.Action = "git_connection.deleted"
	audit.TargetType = "git_connection"
	audit.TargetID = id
	audit.Before = jsonStr(before)
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// GitConnectionApp is one app that deploys through a connection, in the terms
// a refusal has to name it in.
type GitConnectionApp struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// AppsUsingGitConnection is what a removal is refused on and what the admin
// page shows before an administrator tries.
func (db *DB) AppsUsingGitConnection(id string) ([]GitConnectionApp, error) {
	rows, err := db.conn.Query(
		`SELECT id, slug, name FROM apps WHERE git_connection_id = ? ORDER BY name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []GitConnectionApp{}
	for rows.Next() {
		var a GitConnectionApp
		if err := rows.Scan(&a.ID, &a.Slug, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordGitConnectionResult writes the connection's health, **once per
// transition**.
//
// A host that is down would otherwise write a row a minute per app that polls
// through it. One current value, one reader — the administrator looking at the
// page — and no retention rule to invent. It returns whether this was a change,
// which is what a notification would be sent on.
func (db *DB) RecordGitConnectionResult(id string, failure string) (changed bool, err error) {
	before, err := db.GetGitConnection(id)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	if failure == "" {
		if _, err := db.conn.Exec(
			`UPDATE git_connections SET last_error = '', last_error_at = NULL, last_ok_at = ?
			 WHERE id = ?`, now, id); err != nil {
			return false, err
		}
		return before.LastError != "", nil
	}
	if _, err := db.conn.Exec(
		`UPDATE git_connections SET last_error = ?, last_error_at = ? WHERE id = ?`,
		failure, now, id); err != nil {
		return false, err
	}
	return before.LastError != failure, nil
}

// SetAppGitConnection points an app at a connection, or at none.
//
// A setter of its own rather than a field on UpdateApp, for the reason
// `SetGitToken` is one: which credential an app clones with is a decision with
// its own authorization and its own audit line, and a generic column writer is
// where such a decision goes to be forgotten. That audit line, when given, is
// written in the same transaction as the change.
func (db *DB) SetAppGitConnection(appID, connectionID string, audit ...AuditEntry) error {
	return db.execAudited(audit,
		`UPDATE apps SET git_connection_id = ?, updated_at = NOW() WHERE id = ?`,
		nullIfEmpty(connectionID), appID)
}

// --- The picker's per-person identities ---

// GitConnectionIdentity is one person's own credential against one connected
// host, and it is **not** the clone credential.
//
// Listing repositories with the install's credential would turn the picker into
// an estate directory: an install-wide credential sees everything the
// organization exposed, so any signed-in developer would get a catalogue of the
// company's private repositories. The picker shows a person what they can
// already see, so the two are two tables and two types with no conversion
// between them.
type GitConnectionIdentity struct {
	ConnectionID string
	UserID       string
	ExpiresAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time

	token string
}

// EncryptedToken returns the stored ciphertext for the one caller that holds
// the decryption key.
func (i *GitConnectionIdentity) EncryptedToken() string { return i.token }

// PutGitConnectionIdentity stores this person's token for this connection,
// replacing whatever was there.
//
// No audit row. Signing in to a host as yourself, to see the repositories you
// could already see, is not an authorization-relevant mutation — nothing about
// what this install may clone changes, and a row per repository search would be
// a log of what developers browsed.
func (db *DB) PutGitConnectionIdentity(connectionID, userID, ciphertext string, expiresAt *time.Time) error {
	now := time.Now().UTC()
	_, err := db.conn.Exec(`
		INSERT INTO git_connection_identities (id, connection_id, user_id, access_token, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (connection_id, user_id) DO UPDATE
		SET access_token = EXCLUDED.access_token,
		    expires_at = EXCLUDED.expires_at,
		    updated_at = EXCLUDED.updated_at`,
		uuid.NewString(), connectionID, userID, ciphertext, expiresAt, now, now)
	return err
}

// GetGitConnectionIdentity returns this person's identity, or nil when they
// have not signed in to the host. Nil rather than an error: not having signed
// in yet is the ordinary first state, not a failure.
func (db *DB) GetGitConnectionIdentity(connectionID, userID string) (*GitConnectionIdentity, error) {
	i := &GitConnectionIdentity{ConnectionID: connectionID, UserID: userID}
	err := db.conn.QueryRow(`
		SELECT access_token, expires_at, created_at, updated_at
		FROM git_connection_identities WHERE connection_id = ? AND user_id = ?`,
		connectionID, userID).Scan(&i.token, &i.ExpiresAt, &i.CreatedAt, &i.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return i, nil
}

// AppsOnConnectionForRepo is what a verified webhook delivery marks: the apps
// that deploy through this connection from the delivered repository.
//
// Matched on the repository rather than only on the connection, because a
// delivery names one repository and waking every app on a connection because
// one of them was pushed to would make a webhook a way to poll the estate.
// `LIKE` on the URL with the owner/name appended is deliberately loose about
// the `.git` suffix, which both hosts spell inconsistently — and about nothing
// else: the name is escaped, so `acme/data_api` does not also wake
// `acme/dataXapi`, and compared case-insensitively, as both hosts compare
// repository names (DB2-3).
func (db *DB) AppsOnConnectionForRepo(connectionID, ownerAndName string) ([]string, error) {
	name := likeEscape(strings.ToLower(ownerAndName))
	rows, err := db.conn.Query(`
		SELECT id FROM apps
		WHERE git_connection_id = ?
		  AND auto_deploy
		  AND (LOWER(repo_url) LIKE ? ESCAPE '\' OR LOWER(repo_url) LIKE ? ESCAPE '\')`,
		connectionID, "%/"+name, "%/"+name+".git")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
