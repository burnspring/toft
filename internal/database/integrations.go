package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The integration catalog's reads and writes.
//
// One table pair — `integration_connections` and `app_integrations` — behind
// every kind, replacing the table-per-integration shape that produced a gate
// chain per integration with nothing making the chains agree.
//
// `ResolveAttachment` and `ResolveDestination` are the two reads, and they are
// two functions for the reason `ResolveRetention` and `ResolveSourcePolicy` are
// one function each: two call sites that could disagree about whether an app
// may send, or about where its messages go, is a bug nobody finds until the
// moment it matters.

// Attachment is what the four gates need to know about one (app, kind), read
// once.
//
// Connected, Attached and Enabled are three separate booleans and not one,
// because the three refusals they produce have three different fixes and a
// caller collapsing them would have to invent the sentence back. That is the
// distinction `ResolveSlackChannel` did not draw — it answered `attached=false`
// for both "no attachment" and "attached and switched off", which was harmless
// for a poster that must stay quiet either way and wrong for a gate that has to
// say which.
type Attachment struct {
	// Connected: a connection of this kind exists on the install and is offered
	// to apps at all.
	Connected bool
	// Attached: an administrator attached that connection to this app.
	Attached bool
	// Enabled: the attachment is switched on.
	Enabled bool
	// IssueTokens: the connection behind the attachment lets its apps hold its
	// access token. A property of the connection, not of this attachment,
	// because every app on it would hold the same reach.
	IssueTokens bool

	// ConnectionID is the connection behind it, empty when not connected.
	ConnectionID string
	// Destination is where this app's sends of this kind go, already resolved
	// through the connection's default. Empty when the kind has no destination.
	Destination Destination
}

// Destination is one place a send can land: a Slack channel, a document
// library folder, a PagerDuty service. Id is what routes; Label is what a
// person reads and nothing routes on it.
type Destination struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Sendable reports whether the three configuration gates are all open. The
// fourth — the app being stopped — is not this row's business.
func (a Attachment) Sendable() bool { return a.Connected && a.Attached && a.Enabled }

// ---------------------------------------------------------------- connections

// IntegrationConnection is one credential welded to one scope.
//
// The credential never leaves this struct in the clear: Secret is the stored
// ciphertext, exactly as SlackSettings.BotToken was, and the API layer builds
// its own DTO rather than marshalling this.
type IntegrationConnection struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Description is the administrator's own note — what the connection is
	// for and who owns the far side. Optional and editable, unlike the scope.
	Description string `json:"description"`

	ScopeID    string `json:"scope_id"`
	ScopeLabel string `json:"scope_label"`

	// Config is the kind's non-secret credential fields, readable in the clear.
	Config map[string]string `json:"config"`
	// Secret is the ciphertext of the kind's secret fields, as one record. Never
	// marshalled to an API client and never written to an audit row.
	Secret string `json:"-"`

	DefaultDestination Destination `json:"default_destination"`

	ForPlatform       bool     `json:"for_platform"`
	ForApps           bool     `json:"for_apps"`
	PlatformAudiences []string `json:"platform_audiences"`
	// IssueTokens lets apps attached here be handed the connection's access
	// token (file kinds only). Off by default.
	IssueTokens bool `json:"issue_tokens"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const connectionColumns = `id, kind, name, description, scope_id, scope_label, config,
	credential_secret, default_destination_id, default_destination_label,
	for_platform, for_apps, platform_audiences, issue_tokens, COALESCE(created_by,''),
	created_at, updated_at`

func scanConnection(scan func(dest ...any) error) (*IntegrationConnection, error) {
	var c IntegrationConnection
	var config, audiences []byte
	if err := scan(&c.ID, &c.Kind, &c.Name, &c.Description, &c.ScopeID, &c.ScopeLabel, &config,
		&c.Secret, &c.DefaultDestination.ID, &c.DefaultDestination.Label,
		&c.ForPlatform, &c.ForApps, &audiences, &c.IssueTokens, &c.CreatedBy,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(config, &c.Config)
	_ = json.Unmarshal(audiences, &c.PlatformAudiences)
	if c.Config == nil {
		c.Config = map[string]string{}
	}
	if c.PlatformAudiences == nil {
		c.PlatformAudiences = []string{}
	}
	return &c, nil
}

// ListConnections returns every connection on the install, oldest kind first
// then by name, which is the order the admin page renders.
func (db *DB) ListConnections() ([]IntegrationConnection, error) {
	rows, err := db.conn.Query(
		`SELECT ` + connectionColumns + ` FROM integration_connections ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IntegrationConnection{}
	for rows.Next() {
		c, err := scanConnection(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ListConnectionsOfKind is every connection of one kind.
func (db *DB) ListConnectionsOfKind(kind string) ([]IntegrationConnection, error) {
	rows, err := db.conn.Query(
		`SELECT `+connectionColumns+` FROM integration_connections
		 WHERE kind = ? ORDER BY name`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IntegrationConnection{}
	for rows.Next() {
		c, err := scanConnection(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetConnection reads one connection, or nil.
func (db *DB) GetConnection(id string) (*IntegrationConnection, error) {
	c, err := scanConnection(db.conn.QueryRow(
		`SELECT `+connectionColumns+` FROM integration_connections WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// PlatformPosters is every connection that carries platform notifications for
// the named audience.
//
// The one read behind events.Recorder's slice of posters, which is why it takes
// the audience rather than filtering afterwards: an audience switch that was
// applied in two places is an audience switch nobody can reason about.
func (db *DB) PlatformPosters(audience string) ([]IntegrationConnection, error) {
	all, err := db.ListConnections()
	if err != nil {
		return nil, err
	}
	out := []IntegrationConnection{}
	for _, c := range all {
		if !c.ForPlatform {
			continue
		}
		for _, a := range c.PlatformAudiences {
			if a == audience {
				out = append(out, c)
				break
			}
		}
	}
	return out, nil
}

// SaveConnection creates or updates a connection and audits it in the same
// transaction.
//
// The scope is written on create and never touched on update — see the
// migration for why that is a rule rather than an omission. An update that
// leaves Secret empty keeps the stored one, which is what makes the form's
// credential field write-only.
func (db *DB) SaveConnection(c *IntegrationConnection, actorID string, meta RequestMeta) error {
	before, err := db.GetConnection(c.ID)
	if err != nil {
		return err
	}
	c.Name = strings.TrimSpace(c.Name)
	c.Description = strings.TrimSpace(c.Description)
	if c.Config == nil {
		c.Config = map[string]string{}
	}
	if c.PlatformAudiences == nil {
		c.PlatformAudiences = []string{}
	}
	config, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	audiences, err := json.Marshal(c.PlatformAudiences)
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	action := "integration.connection.create"
	if before == nil {
		c.CreatedAt = now
		if _, err := tx.Exec(`
			INSERT INTO integration_connections
				(id, kind, name, description, scope_id, scope_label, config, credential_secret,
				 default_destination_id, default_destination_label,
				 for_platform, for_apps, platform_audiences, issue_tokens, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.Kind, c.Name, c.Description, c.ScopeID, c.ScopeLabel, string(config), c.Secret,
			c.DefaultDestination.ID, c.DefaultDestination.Label,
			c.ForPlatform, c.ForApps, string(audiences), c.IssueTokens,
			nullIfEmpty(actorID), now, now); err != nil {
			return err
		}
	} else {
		action = "integration.connection.update"
		// The credential is only rewritten when one was supplied, which is what
		// makes the field write-only rather than a field that clears itself
		// every time somebody renames a connection.
		secret := before.Secret
		if c.Secret != "" {
			secret = c.Secret
		}
		c.Secret = secret
		c.CreatedAt = before.CreatedAt
		c.ScopeID, c.ScopeLabel = before.ScopeID, before.ScopeLabel
		if _, err := tx.Exec(`
			UPDATE integration_connections SET
				name = ?, description = ?, config = ?, credential_secret = ?,
				default_destination_id = ?, default_destination_label = ?,
				for_platform = ?, for_apps = ?, platform_audiences = ?, issue_tokens = ?,
				updated_at = ?
			WHERE id = ?`,
			c.Name, c.Description, string(config), secret,
			c.DefaultDestination.ID, c.DefaultDestination.Label,
			c.ForPlatform, c.ForApps, string(audiences), c.IssueTokens, now, c.ID); err != nil {
			return err
		}
	}
	c.UpdatedAt = now

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     action,
		TargetType: "integration_connection", TargetID: c.ID,
		Before:    jsonStr(connectionAudit(before, false)),
		After:     jsonStr(connectionAudit(c, before != nil && before.Secret != c.Secret)),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing an integration connection: %w", err)
	}
	return tx.Commit()
}

// DeleteConnection removes a connection. The caller refuses first if apps are
// attached; the foreign key is the belt behind that.
func (db *DB) DeleteConnection(id, actorID string, meta RequestMeta) error {
	before, err := db.GetConnection(id)
	if err != nil || before == nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM integration_connections WHERE id = ?`, id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "integration.connection.delete",
		TargetType: "integration_connection", TargetID: id,
		Before:    jsonStr(connectionAudit(before, false)),
		After:     jsonStr(map[string]interface{}{"deleted": true}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing an integration connection deletion: %w", err)
	}
	return tx.Commit()
}

// connectionAudit is the redacted shape written to audit_log. Built by hand
// rather than by marshalling the struct so that adding a field cannot silently
// start leaking it — the same discipline slackAudit kept.
func connectionAudit(c *IntegrationConnection, secretChanged bool) map[string]interface{} {
	if c == nil {
		return map[string]interface{}{"exists": false}
	}
	return map[string]interface{}{
		"exists":      true,
		"kind":        c.Kind,
		"name":        c.Name,
		"description": c.Description,
		// The scope is in the clear and deliberately: which site or which
		// workspace an install pointed itself at is precisely the question this
		// log exists to answer.
		"scope_id":           auditScope(c.Kind, c.ScopeID),
		"scope_label":        c.ScopeLabel,
		"config":             c.Config,
		"default_dest_id":    c.DefaultDestination.ID,
		"default_dest_label": c.DefaultDestination.Label,
		"for_platform":       c.ForPlatform,
		"for_apps":           c.ForApps,
		"platform_audiences": c.PlatformAudiences,
		"issue_tokens":       c.IssueTokens,
		"secret_set":         c.Secret != "",
		"secret_changed":     secretChanged,
	}
}

// auditScope is the scope as the audit log may hold it. A Teams workflow URL's
// query string is its signature, sealed at save since the scope split; a row
// saved before the split still holds the whole URL as its scope, and the
// signature must not reach audit_log on that row's update or delete. Keyed on
// the kind's name because this package cannot ask the transport for its
// ScopeSplitter; every other kind's scope is in the clear in full, by design —
// the signed-webhook kind's URL is its scope.
func auditScope(kind, scope string) string {
	if kind != "teams" {
		return scope
	}
	if i := strings.IndexAny(scope, "?#"); i >= 0 {
		return scope[:i]
	}
	return scope
}

// --------------------------------------------------------------- attachments

// AppIntegration is one app's attachment to one kind.
type AppIntegration struct {
	AppID        string `json:"app_id"`
	Kind         string `json:"kind"`
	ConnectionID string `json:"connection_id"`
	Enabled      bool   `json:"enabled"`

	Destination Destination `json:"destination"`

	AttachedBy string    `json:"attached_by,omitempty"`
	AttachedAt time.Time `json:"attached_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// ConnectionName is filled by the reads that join it, for a page that has
	// to say which connection an app is on.
	ConnectionName string `json:"connection_name,omitempty"`
	// AppName and AppSlug are filled only by AppsUsingConnection.
	AppName string `json:"app_name,omitempty"`
	AppSlug string `json:"app_slug,omitempty"`
}

const appIntegrationColumns = `ai.app_id, ai.kind, ai.connection_id, ai.enabled,
	ai.destination_id, ai.destination_label, COALESCE(ai.attached_by,''),
	ai.attached_at, ai.updated_at`

func scanAppIntegration(scan func(dest ...any) error, extra ...any) (*AppIntegration, error) {
	var a AppIntegration
	dest := []any{&a.AppID, &a.Kind, &a.ConnectionID, &a.Enabled,
		&a.Destination.ID, &a.Destination.Label, &a.AttachedBy,
		&a.AttachedAt, &a.UpdatedAt}
	if err := scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAppIntegration reads one attachment, or nil. Nil is the common case and is
// not an error.
func (db *DB) GetAppIntegration(appID, kind string) (*AppIntegration, error) {
	var name string
	a, err := scanAppIntegration(db.conn.QueryRow(
		`SELECT `+appIntegrationColumns+`, c.name
		 FROM app_integrations ai
		 JOIN integration_connections c ON c.id = ai.connection_id
		 WHERE ai.app_id = ? AND ai.kind = ?`, appID, kind).Scan, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.ConnectionName = name
	return a, nil
}

// ListAppIntegrations is every attachment on one app.
func (db *DB) ListAppIntegrations(appID string) ([]AppIntegration, error) {
	rows, err := db.conn.Query(
		`SELECT `+appIntegrationColumns+`, c.name
		 FROM app_integrations ai
		 JOIN integration_connections c ON c.id = ai.connection_id
		 WHERE ai.app_id = ? ORDER BY ai.kind`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppIntegration{}
	for rows.Next() {
		var name string
		a, err := scanAppIntegration(rows.Scan, &name)
		if err != nil {
			return nil, err
		}
		a.ConnectionName = name
		out = append(out, *a)
	}
	return out, rows.Err()
}

// AppsUsingConnection is every app attached to one connection, for the refusal
// that names them.
func (db *DB) AppsUsingConnection(connID string) ([]AppIntegration, error) {
	rows, err := db.conn.Query(
		`SELECT `+appIntegrationColumns+`, COALESCE(a.name,''), COALESCE(a.slug,'')
		 FROM app_integrations ai
		 LEFT JOIN apps a ON a.id = ai.app_id
		 WHERE ai.connection_id = ? ORDER BY a.name`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppIntegration{}
	for rows.Next() {
		var name, slug string
		a, err := scanAppIntegration(rows.Scan, &name, &slug)
		if err != nil {
			return nil, err
		}
		a.AppName, a.AppSlug = name, slug
		out = append(out, *a)
	}
	return out, rows.Err()
}

// AttachAppIntegration gives an app one kind, or updates the parts an
// administrator owns on one it already has.
//
// Audited in the same transaction: this is the decision that lets an app reach
// out of the install carrying the organization's own identity, and it is the
// single most consequential grant in this feature.
func (db *DB) AttachAppIntegration(a *AppIntegration, actorID string, meta RequestMeta) error {
	before, err := db.GetAppIntegration(a.AppID, a.Kind)
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		INSERT INTO app_integrations
			(app_id, kind, connection_id, enabled, destination_id, destination_label,
			 attached_by, attached_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id, kind) DO UPDATE SET
			connection_id = EXCLUDED.connection_id,
			enabled = EXCLUDED.enabled,
			updated_at = EXCLUDED.updated_at`,
		a.AppID, a.Kind, a.ConnectionID, a.Enabled,
		a.Destination.ID, a.Destination.Label,
		nullIfEmpty(actorID), now, now); err != nil {
		return err
	}

	action := "integration.attached"
	if before != nil {
		action = "integration.updated"
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     action,
		TargetType: "app", TargetID: a.AppID,
		Before:    jsonStr(attachmentAudit(before)),
		After:     jsonStr(attachmentAudit(a)),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing an app's integration: %w", err)
	}
	return tx.Commit()
}

// DetachAppIntegration removes the attachment entirely, forgetting the
// destination the app's own people chose.
//
// The reversible half is an attach with Enabled false, which stops the sends
// and keeps the choice. One is a toggle; this destroys something a person
// entered — and it stops sends already sitting in the queue, because off means
// now.
//
// **It reaches nothing already written.** A file this app put in a document
// library stays there, and the placements on the connection are where an
// administrator sees what that is.
func (db *DB) DetachAppIntegration(appID, kind, actorID string, meta RequestMeta) error {
	before, err := db.GetAppIntegration(appID, kind)
	if err != nil || before == nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM app_integrations WHERE app_id = ? AND kind = ?`, appID, kind); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "integration.detached",
		TargetType: "app", TargetID: appID,
		Before:    jsonStr(attachmentAudit(before)),
		After:     jsonStr(map[string]interface{}{"attached": false, "kind": kind}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing an app's integration detachment: %w", err)
	}
	return tx.Commit()
}

// SetAppIntegrationDestination records the destination an app's own people
// chose, inside the administrator's scope.
//
// It refuses on an app with no attachment rather than creating one: an app's
// people cannot give themselves an integration, and a handler that forgot to
// check would otherwise silently grant one.
func (db *DB) SetAppIntegrationDestination(appID, kind string, dest Destination,
	actorID string, meta RequestMeta) error {

	before, err := db.GetAppIntegration(appID, kind)
	if err != nil {
		return err
	}
	if before == nil {
		return errors.New("this app has no such integration")
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		UPDATE app_integrations SET destination_id = ?, destination_label = ?, updated_at = ?
		WHERE app_id = ? AND kind = ?`,
		dest.ID, dest.Label, time.Now().UTC(), appID, kind); err != nil {
		return err
	}
	after := *before
	after.Destination = dest
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "integration.destination",
		TargetType: "app", TargetID: appID,
		Before:    jsonStr(attachmentAudit(before)),
		After:     jsonStr(attachmentAudit(&after)),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing an app's integration destination: %w", err)
	}
	return tx.Commit()
}

func attachmentAudit(a *AppIntegration) map[string]interface{} {
	if a == nil {
		return map[string]interface{}{"attached": false}
	}
	return map[string]interface{}{
		"attached":          true,
		"kind":              a.Kind,
		"connection_id":     a.ConnectionID,
		"enabled":           a.Enabled,
		"destination":       a.Destination.ID,
		"destination_label": a.Destination.Label,
	}
}

// ------------------------------------------------------------- the two reads

// ResolveAttachment answers gates 1 to 3 for one (app, kind), in one read.
//
// Gate 1 is *not* "a connection of this kind exists": it is "the connection
// this app would send through offers itself to apps". Where an app is attached
// that is the attachment's own connection, so switching one connection off does
// not open a hole through a second one; where it is not, any connection of the
// kind offered to apps is enough to make the refusal say "not attached to this
// app" rather than "not connected on this install", which are different asks.
func (db *DB) ResolveAttachment(appID, kind string) (Attachment, error) {
	row, err := db.GetAppIntegration(appID, kind)
	if err != nil {
		return Attachment{}, err
	}
	if row == nil {
		offered, err := db.kindIsOfferedToApps(kind)
		if err != nil {
			return Attachment{}, err
		}
		return Attachment{Connected: offered}, nil
	}

	conn, err := db.GetConnection(row.ConnectionID)
	if err != nil {
		return Attachment{}, err
	}
	if conn == nil {
		// The foreign key makes this unreachable; treating it as "not
		// connected" rather than panicking is the safe direction.
		return Attachment{}, nil
	}

	a := Attachment{
		Connected:    conn.ForApps,
		Attached:     true,
		Enabled:      row.Enabled,
		IssueTokens:  conn.IssueTokens,
		ConnectionID: conn.ID,
		Destination:  row.Destination,
	}
	if a.Destination.ID == "" && a.Destination.Label == "" {
		a.Destination = conn.DefaultDestination
	}
	return a, nil
}

func (db *DB) kindIsOfferedToApps(kind string) (bool, error) {
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM integration_connections WHERE kind = ? AND for_apps`,
		kind).Scan(&n)
	return n > 0, err
}

// ResolveDestination answers, in one place, the only two questions a transport
// asks about an app: may this app send at all, and where does it send.
//
// One function for the reason ResolveRetention and ResolvePublicBaseURL are one
// function: two call sites that could disagree about where an app's alerts go
// is a bug nobody finds until the moment it matters. It is ResolveSlackChannel's
// successor and keeps its rule — an unset destination falls back to the
// connection's default, a broken one fails.
func (db *DB) ResolveDestination(appID, kind string) (Destination, bool, error) {
	a, err := db.ResolveAttachment(appID, kind)
	if err != nil || !a.Sendable() {
		return Destination{}, false, err
	}
	return a.Destination, true, nil
}

// --------------------------------------------------------------- placements

// IntegrationPlacement is one object an app left in somebody else's system.
type IntegrationPlacement struct {
	ID             string `json:"id"`
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name"`
	Kind           string `json:"kind"`

	Destination Destination `json:"destination"`

	ItemID      string `json:"item_id"`
	ItemURL     string `json:"item_url"`
	Name        string `json:"name"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`

	// AppID and AppName carry no foreign key: the row outlives the app, and
	// the name is stamped in at the time so the row stays readable afterwards.
	AppID   string `json:"app_id"`
	AppName string `json:"app_name"`
	SendID  string `json:"send_id"`

	PlacedAt time.Time `json:"placed_at"`
}

// InsertPlacement records an object that now exists in somebody else's system.
func (db *DB) InsertPlacement(p *IntegrationPlacement) error {
	if p.PlacedAt.IsZero() {
		p.PlacedAt = time.Now().UTC()
	}
	_, err := db.conn.Exec(`
		INSERT INTO integration_placements
			(id, connection_id, connection_name, kind, destination_id, destination_label,
			 item_id, item_url, name, bytes, content_type, sha256,
			 app_id, app_name, send_id, placed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ConnectionID, p.ConnectionName, p.Kind,
		p.Destination.ID, p.Destination.Label,
		p.ItemID, p.ItemURL, p.Name, p.Bytes, p.ContentType, p.SHA256,
		p.AppID, p.AppName, p.SendID, p.PlacedAt)
	return err
}

// ListPlacements is what is out there, per connection, newest first.
//
// This is the whole of "detach reaches nothing already written" as something a
// person can act on. The alternative — the platform issuing bulk deletes inside
// an organization's document store on the strength of rows it wrote itself — is
// rejected outright: the first time it removes a file somebody edited afterwards
// is the last time anybody attaches the kind.
func (db *DB) ListPlacements(connID string, limit int) ([]IntegrationPlacement, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.conn.Query(`
		SELECT id, connection_id, connection_name, kind, destination_id, destination_label,
		       item_id, item_url, name, bytes, content_type, sha256,
		       app_id, app_name, send_id, placed_at
		FROM integration_placements WHERE connection_id = ?
		ORDER BY placed_at DESC LIMIT ?`, connID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IntegrationPlacement{}
	for rows.Next() {
		var p IntegrationPlacement
		if err := rows.Scan(&p.ID, &p.ConnectionID, &p.ConnectionName, &p.Kind,
			&p.Destination.ID, &p.Destination.Label,
			&p.ItemID, &p.ItemURL, &p.Name, &p.Bytes, &p.ContentType, &p.SHA256,
			&p.AppID, &p.AppName, &p.SendID, &p.PlacedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPlacements is how many objects a connection has out there, for the page
// that has to say so before somebody detaches.
func (db *DB) CountPlacements(connID string) (int, error) {
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM integration_placements WHERE connection_id = ?`, connID).Scan(&n)
	return n, err
}

// CountAppPlacements is the same figure for one app, for the delete
// confirmation that has to say what will not be cleaned up.
func (db *DB) CountAppPlacements(appID string) (int, error) {
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM integration_placements WHERE app_id = ?`, appID).Scan(&n)
	return n, err
}
