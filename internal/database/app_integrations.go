package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"toft/internal/textutil"
)

// The ledger's own settings, and only the ledger's.
//
// The two switches that used to live here — `integrations.slack` and
// `integrations.email` — are gone: a connection that exists with `for_apps`
// set *is* the install switch for its kind, which is one fact where there were
// two rows that could disagree. What is left governs the record rather than the
// capability, which is why it survived the catalog: retention is about how long
// the install keeps evidence, and that is not a property of any one integration.
const (
	SettingIntegrationsRetainBodies  = "integrations.retain_bodies"
	SettingIntegrationsRetentionDays = "integrations.retention_days"
)

// DefaultSendRetentionDays is how long the ledger keeps a send.
//
// Long enough to answer an incident that surfaces months later — which matters
// because the ledger is the *only* control over app email — and not forever,
// because it holds third-party addresses.
const DefaultSendRetentionDays = 365

// MinSendRetentionDays is the floor an administrator may set. Below this the
// ledger stops being able to answer the question it exists for.
const MinSendRetentionDays = 30

// RetainedBodyDays is how long a retained body survives, regardless of the
// row's own retention. A body is a debugging aid for a live problem, not a
// year-long archive of every payload the install has transmitted.
const RetainedBodyDays = 30

// IntegrationSettings is the install's posture on the send ledger.
type IntegrationSettings struct {
	// RetainBodies stores the message body beside its hash. Off by default:
	// the hash is what proves two sends were identical and matches a leaked
	// message to a send, and that is enough for almost every install.
	RetainBodies bool `json:"retain_bodies"`
	// RetentionDays bounds the ledger. Never below MinSendRetentionDays as read.
	RetentionDays int `json:"retention_days"`
}

// DefaultIntegrationSettings is what an install starts as: a year of ledger,
// no bodies kept.
func DefaultIntegrationSettings() IntegrationSettings {
	return IntegrationSettings{RetentionDays: DefaultSendRetentionDays}
}

// GetIntegrationSettings reads the document, filling anything unset from
// defaults.
func (db *DB) GetIntegrationSettings() (IntegrationSettings, error) {
	s := DefaultIntegrationSettings()
	rows, err := db.conn.Query(
		`SELECT key, value FROM settings WHERE key IN (?, ?)`,
		SettingIntegrationsRetainBodies, SettingIntegrationsRetentionDays)
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
		case SettingIntegrationsRetainBodies:
			s.RetainBodies = value == "true"
		case SettingIntegrationsRetentionDays:
			if n, err := parseInt(value); err == nil {
				s.RetentionDays = n
			}
		}
	}
	if s.RetentionDays < MinSendRetentionDays {
		s.RetentionDays = MinSendRetentionDays
	}
	return s, rows.Err()
}

// SetIntegrationSettings writes the document and audits it in the same
// transaction. Every value here is readable in the clear — none of them is a
// credential — so the audit blob carries the whole before and after.
func (db *DB) SetIntegrationSettings(s *IntegrationSettings, actorID string, meta RequestMeta) error {
	before, err := db.GetIntegrationSettings()
	if err != nil {
		return err
	}
	if s.RetentionDays < MinSendRetentionDays {
		s.RetentionDays = MinSendRetentionDays
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	writes := map[string]string{
		SettingIntegrationsRetainBodies:  boolValue(s.RetainBodies),
		SettingIntegrationsRetentionDays: fmt.Sprintf("%d", s.RetentionDays),
	}
	for key, value := range writes {
		if _, err := tx.Exec(`
			INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
			return err
		}
	}

	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(s)
	if err := WriteAuditTx(tx, AuditEntry{
		ActorType:  "user",
		ActorID:    actorID,
		Action:     "integrations.settings",
		TargetType: "settings",
		TargetID:   "integrations",
		Before:     string(beforeJSON),
		After:      string(afterJSON),
		OccurredAt: time.Now().UTC(),
		RequestID:  meta.RequestID,
		IP:         meta.IP,
		UserAgent:  meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------------ ledger

// Send statuses. See migration 29 for why blocked, abandoned and failed are
// three things rather than one.
const (
	// SendQueued is accepted and recorded, not yet handed to a transport.
	SendQueued = "queued"
	// SendSent was accepted by Slack or the relay.
	SendSent = "sent"
	// SendFailed reached a transport and did not succeed.
	SendFailed = "failed"
	// SendBlocked had its capability withdrawn between acceptance and delivery.
	// Nothing is wrong with the message; retrying will not help until somebody
	// re-attaches the integration.
	SendBlocked = "blocked"
	// SendAbandoned was queued by a process that no longer exists.
	SendAbandoned = "abandoned"
)

// SendAttachment is one file on a send, recorded by name, size, type and
// digest — never by content.
type SendAttachment struct {
	Name   string `json:"name"`
	Bytes  int    `json:"bytes"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
}

// IntegrationSend is one row of the ledger.
type IntegrationSend struct {
	ID    string `json:"id"`
	AppID string `json:"app_id"`
	// Kind is the catalog's name for the integration. The column was called
	// `integration` and is renamed here rather than duplicated, for the reason
	// the old tables are dropped rather than shadowed: two columns answering
	// "which integration" is the drift this feature exists to remove. The one
	// place the old word survives is /app/sends/{id}, which is published and
	// keeps emitting it.
	Kind        string `json:"kind"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	Destination string `json:"destination,omitempty"`
	// Recipients is every address, in full. A count cannot answer the question
	// an incident opens with.
	Recipients     []string         `json:"recipients"`
	Subject        string           `json:"subject,omitempty"`
	BodyBytes      int              `json:"body_bytes"`
	BodySHA256     string           `json:"body_sha256,omitempty"`
	Body           string           `json:"body,omitempty"`
	Attachments    []SendAttachment `json:"attachments"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
	DeploymentID   string           `json:"deployment_id,omitempty"`
	Error          string           `json:"error,omitempty"`
	// PlacementID names the object this send left in somebody else's system,
	// where it left one. It is what makes the retention exemption a property of
	// the *row* rather than a lookup by kind at sweep time: a row that named an
	// object stays kept whatever the catalog says next year.
	PlacementID string     `json:"placement_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// AppSlug and AppName are filled only by the install-wide read, which joins
	// them. The app's own view already knows which app it is looking at, and a
	// join it does not need is a join it should not pay for.
	AppSlug string `json:"app_slug,omitempty"`
	AppName string `json:"app_name,omitempty"`
}

const sendColumns = `id, app_id, kind, action, status, destination, recipients,
	subject, body_bytes, body_sha256, COALESCE(body,''), attachments,
	COALESCE(idempotency_key,''), COALESCE(deployment_id,''), error, placement_id,
	created_at, completed_at`

// sendColumnsAliased is the same list for the query that joins apps, where a
// bare `id` would be ambiguous.
//
// Written out rather than derived from sendColumns by string substitution:
// "id" is a substring of "app_id" and "deployment_id", so any such rewrite
// silently produces "app_s.id" and a query that does not run. Two literals that
// must be kept in step is the lesser problem, and scanSend fails loudly if they
// ever diverge.
const sendColumnsAliased = `s.id, s.app_id, s.kind, s.action, s.status, s.destination,
	s.recipients, s.subject, s.body_bytes, s.body_sha256, COALESCE(s.body,''), s.attachments,
	COALESCE(s.idempotency_key,''), COALESCE(s.deployment_id,''), s.error, s.placement_id,
	s.created_at, s.completed_at`

func scanSend(scan func(dest ...any) error) (*IntegrationSend, error) {
	var s IntegrationSend
	var recipients, attachments []byte
	if err := scan(&s.ID, &s.AppID, &s.Kind, &s.Action, &s.Status, &s.Destination,
		&recipients, &s.Subject, &s.BodyBytes, &s.BodySHA256, &s.Body, &attachments,
		&s.IdempotencyKey, &s.DeploymentID, &s.Error, &s.PlacementID,
		&s.CreatedAt, &s.CompletedAt); err != nil {
		return nil, err
	}
	// A malformed document is not worth failing a read over: the columns that
	// matter to an incident are scalars, and losing the recipient list to a
	// decode error would hide the row entirely.
	_ = json.Unmarshal(recipients, &s.Recipients)
	_ = json.Unmarshal(attachments, &s.Attachments)
	if s.Recipients == nil {
		s.Recipients = []string{}
	}
	if s.Attachments == nil {
		s.Attachments = []SendAttachment{}
	}
	return &s, nil
}

// InsertSend records a send *before* it is attempted, which is what lets the
// 202 the caller receives be a promise the ledger can keep.
func (db *DB) InsertSend(s *IntegrationSend) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if s.Status == "" {
		s.Status = SendQueued
	}
	recipients, err := json.Marshal(orEmpty(s.Recipients))
	if err != nil {
		return err
	}
	attachments, err := json.Marshal(orEmptyAttachments(s.Attachments))
	if err != nil {
		return err
	}
	_, err = db.conn.Exec(`
		INSERT INTO app_integration_sends
			(id, app_id, kind, action, status, destination, recipients, subject,
			 body_bytes, body_sha256, body, attachments, idempotency_key, deployment_id,
			 error, placement_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.AppID, s.Kind, s.Action, s.Status, s.Destination, string(recipients),
		s.Subject, s.BodyBytes, s.BodySHA256, nullIfEmpty(s.Body), string(attachments),
		nullIfEmpty(s.IdempotencyKey), nullIfEmpty(s.DeploymentID), s.Error,
		s.PlacementID, s.CreatedAt)
	return err
}

// CompleteSend records the outcome. Only a queued row moves, so a status set by
// a gate re-check cannot be overwritten by a transport that was already in
// flight.
func (db *DB) CompleteSend(id, status, errMsg string) error {
	_, err := db.conn.Exec(`
		UPDATE app_integration_sends
		SET status = ?, error = ?, completed_at = ?
		WHERE id = ? AND status = ?`,
		status, truncate(errMsg, 2000), time.Now().UTC(), id, SendQueued)
	return err
}

// SetSendSubject records a subject learned after the row was written — a token
// issuance's expiry, which exists only once the provider has answered.
func (db *DB) SetSendSubject(id, subject string) error {
	_, err := db.conn.Exec(
		`UPDATE app_integration_sends SET subject = ? WHERE id = ?`, truncate(subject, 500), id)
	return err
}

// MarkSendPlacement stamps a send with the object it left behind.
//
// Separate from CompleteSend because it happens *before* the outcome is
// decided: the object exists the moment the transport returns, and a row that
// recorded the placement only on success would lose it to a failure between the
// far side's 201 and this process's next line.
func (db *DB) MarkSendPlacement(id, placementID string) error {
	_, err := db.conn.Exec(
		`UPDATE app_integration_sends SET placement_id = ? WHERE id = ?`, placementID, id)
	return err
}

// GetAppSend reads one send back, scoped to the app that made it.
//
// The scoping is the point: this is the only thing an app may read about its
// own sends, and there is deliberately no listing. Reading back an id it minted
// about content it wrote tells an app nothing it did not know; browsing would
// teach it which addresses its administrator finds interesting.
func (db *DB) GetAppSend(appID, id string) (*IntegrationSend, error) {
	s, err := scanSend(db.conn.QueryRow(
		`SELECT `+sendColumns+` FROM app_integration_sends WHERE id = ? AND app_id = ?`,
		id, appID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// FindSendByIdempotencyKey answers "have I already accepted this one?".
//
// Scoped to the app, so a key is never a cross-app oracle, and bounded by the
// window, so a key is reusable next week rather than burnt forever.
// ReleaseSendIdempotencyKey detaches a send from its Idempotency-Key, so a
// resend under the same key is a new send rather than a replay of this one.
// For a send the queue refused (I-2): the row stays as the record of the
// refusal, and the key is free for the attempt the 429 asked for.
func (db *DB) ReleaseSendIdempotencyKey(id string) error {
	_, err := db.conn.Exec(`UPDATE app_integration_sends SET idempotency_key = NULL WHERE id = ?`, id)
	return err
}

func (db *DB) FindSendByIdempotencyKey(appID, key string, window time.Duration) (*IntegrationSend, error) {
	if key == "" {
		return nil, nil
	}
	s, err := scanSend(db.conn.QueryRow(
		`SELECT `+sendColumns+` FROM app_integration_sends
		 WHERE app_id = ? AND idempotency_key = ? AND created_at > ?`,
		appID, key, time.Now().UTC().Add(-window)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// ListAppSends is the app's own recent history, for its Integrations tab.
// An action, when given, narrows it to that action — "token" tells an app's
// token issuances apart from its sends.
func (db *DB) ListAppSends(appID, action string, limit int) ([]IntegrationSend, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := db.conn.Query(
		`SELECT `+sendColumns+` FROM app_integration_sends
		 WHERE app_id = ? AND (? = '' OR action = ?) ORDER BY created_at DESC LIMIT ?`,
		appID, action, action, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSends(rows)
}

// SendFilter is the admin log's query. Recipient is the one that matters: it is
// what an incident actually starts with, and the reason this page is the
// control rather than a convenience.
type SendFilter struct {
	AppID string
	Kind  string
	// Action narrows to one catalog action, so token issuances can be told
	// apart from sends.
	Action    string
	Status    string
	Recipient string
	Since     *time.Time
	Limit     int
}

// ListSends is the install-wide ledger.
func (db *DB) ListSends(f SendFilter) ([]IntegrationSend, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.AppID != "" {
		where = append(where, "s.app_id = ?")
		args = append(args, f.AppID)
	}
	if f.Kind != "" {
		where = append(where, "s.kind = ?")
		args = append(args, f.Kind)
	}
	if f.Action != "" {
		where = append(where, "s.action = ?")
		args = append(args, f.Action)
	}
	if f.Status != "" {
		where = append(where, "s.status = ?")
		args = append(args, f.Status)
	}
	if f.Recipient != "" {
		// Containment against the JSONB document, which is what the GIN index
		// on this column answers.
		where = append(where, "s.recipients @> ?::jsonb")
		encoded, _ := json.Marshal([]string{strings.ToLower(strings.TrimSpace(f.Recipient))})
		args = append(args, string(encoded))
	}
	if f.Since != nil {
		where = append(where, "s.created_at >= ?")
		args = append(args, *f.Since)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args = append(args, limit)

	rows, err := db.conn.Query(
		`SELECT `+sendColumnsAliased+`, COALESCE(a.slug,''), COALESCE(a.name,'')
		 FROM app_integration_sends s LEFT JOIN apps a ON a.id = s.app_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY s.created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []IntegrationSend{}
	for rows.Next() {
		var s IntegrationSend
		var recipients, attachments []byte
		if err := rows.Scan(&s.ID, &s.AppID, &s.Kind, &s.Action, &s.Status,
			&s.Destination, &recipients, &s.Subject, &s.BodyBytes, &s.BodySHA256, &s.Body,
			&attachments, &s.IdempotencyKey, &s.DeploymentID, &s.Error, &s.PlacementID,
			&s.CreatedAt, &s.CompletedAt, &s.AppSlug, &s.AppName); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(recipients, &s.Recipients)
		_ = json.Unmarshal(attachments, &s.Attachments)
		if s.Recipients == nil {
			s.Recipients = []string{}
		}
		if s.Attachments == nil {
			s.Attachments = []SendAttachment{}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AbandonQueuedSendsBefore marks rows a vanished process left behind.
//
// Run at startup with this process's start time. The queue is in memory, so a
// restart loses what was in it; without this the ledger would keep claiming
// those sends were on their way forever, and an owner would have no way to tell
// "still going" from "never happened".
func (db *DB) AbandonQueuedSendsBefore(cutoff time.Time) (int64, error) {
	res, err := db.conn.Exec(`
		UPDATE app_integration_sends
		SET status = ?, error = ?, completed_at = ?
		WHERE status = ? AND created_at < ?`,
		SendAbandoned,
		"the platform restarted before this was delivered — it was not sent",
		time.Now().UTC(), SendQueued, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneSends enforces retention: whole rows past retentionDays, and retained
// bodies past RetainedBodyDays whatever the row's own age.
//
// **A row that named a placement is exempt from the delete and not from the
// body null.** The message is chatter and expires; the fact that this app put
// this object into that library is not chatter, and it does not, because the
// object it describes does not either. Every other durable record here is
// designed to be deleted and that is right for all of them — a record about an
// app is worthless once the app is gone. This is the one exception, and it is
// bounded by exactly what it describes: one row per object that still exists
// somewhere else. An install that finds it unbounded has an app writing
// unbounded files into somebody's document store, which is the thing the row is
// there to make visible.
//
// It returns the horizon it actually used along with the count, because the
// floor above is applied here and the caller reports the cutoff on its span. A
// caller that recomputed it from the setting would report a date the sweep did
// not use on any install that has one below the floor.
func (db *DB) PruneSends(retentionDays int) (int64, time.Time, error) {
	if retentionDays < MinSendRetentionDays {
		retentionDays = MinSendRetentionDays
	}
	now := time.Now().UTC()

	// The body null takes the placement-bearing rows with it: what is kept is
	// the record of the object, never the message that carried it.
	if _, err := db.conn.Exec(`
		UPDATE app_integration_sends SET body = NULL
		WHERE body IS NOT NULL AND created_at < ?`,
		now.AddDate(0, 0, -RetainedBodyDays)); err != nil {
		return 0, time.Time{}, err
	}

	cutoff := now.AddDate(0, 0, -retentionDays)
	res, err := db.conn.Exec(
		`DELETE FROM app_integration_sends WHERE created_at < ? AND placement_id = ''`, cutoff)
	if err != nil {
		return 0, cutoff, err
	}
	n, err := res.RowsAffected()
	return n, cutoff, err
}

func collectSends(rows *sql.Rows) ([]IntegrationSend, error) {
	out := []IntegrationSend{}
	for rows.Next() {
		s, err := scanSend(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orEmptyAttachments(v []SendAttachment) []SendAttachment {
	if v == nil {
		return []SendAttachment{}
	}
	return v
}

// truncate cuts on a rune boundary and drops what a text column refuses: the
// text is a transport's error or a subject, foreign and often non-ASCII, and a
// byte-wise cut through a rune makes Postgres refuse the UPDATE that records
// the outcome — leaving the send queued forever (DB2-1).
func truncate(s string, n int) string {
	return textutil.Clip(s, n)
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

func boolValue(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
