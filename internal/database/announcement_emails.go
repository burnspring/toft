package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The announcement an administrator sends: one row of content and one row per
// resolved recipient.
//
// It is the first mail in the product whose *arrival* is the point, and this
// file is where that difference lives. Everything else the platform sends is
// allowed to vanish — `mail.SendAsync` returns nothing and a dead relay is a
// log line — which is right for a message sent because something happened, and
// wrong for a frozen row claiming it reached 214 people.
//
// The shape is `app_integration_sends`'s, deliberately: a status that
// distinguishes what actually happened, recipients kept in full rather than
// counted, a partial index feeding a startup sweep that abandons what a restart
// orphaned, and a separate retention answer for the recipient rows because they
// are named people and the counts are not.

// Statuses on an announcement. Three, and a failure is a count rather than a
// fourth: an announcement where twelve sends failed is *sent, with twelve
// failures*. A `partially_sent` state would encode a comparison two integers
// already express, and every reader — the list, the detail page, the audit row
// — would have to decide independently where its boundary was.
const (
	// AnnouncementDraft is editable, deletable, and has reached nobody.
	AnnouncementDraft = "draft"
	// AnnouncementSending is frozen. The transition into it is the freeze, and
	// it happens at the *start* of the run: content stops being editable the
	// moment the first recipient could have received it.
	AnnouncementSending = "sending"
	// AnnouncementSent is terminal, whatever the outcomes were.
	AnnouncementSent = "sent"
)

// Audiences. Two, resolved by one query. `publishers` includes administrators —
// see ResolveAnnouncementAudience.
const (
	AnnouncementAudienceAll        = "all"
	AnnouncementAudiencePublishers = "publishers"
)

// Statuses on a recipient row. One row per *resolved* user, whatever happened
// to them: a count cannot answer "why did Bob not get it", and that is the only
// question anybody ever asks a record like this.
const (
	AnnouncementPending   = "pending"
	AnnouncementDelivered = "delivered"
	AnnouncementFailed    = "failed"
	AnnouncementSkipped   = "skipped"
	// AnnouncementAbandoned is a recipient the process was still holding when
	// it restarted. The ledger never claims a send happened that did not.
	AnnouncementAbandoned = "abandoned"
)

// The two skip reasons, and the list is the specification.
//
// The names are the grant notifier's (`internal/api/grant_notify.go`) so the
// product has one set of words for why somebody was not mailed. Never-signed-in
// is deliberately not a third: an auto-provisioned account belonging to somebody
// who has not arrived yet is exactly who a welcome announcement is for.
const (
	AnnouncementSkipLocked    = "locked"
	AnnouncementSkipNoAddress = "no_address"
)

// ValidAnnouncementAudience reports whether a value is one of the two.
func ValidAnnouncementAudience(a string) bool {
	return a == AnnouncementAudienceAll || a == AnnouncementAudiencePublishers
}

// AnnouncementEmail is one announcement: the content, the audience it was or
// will be sent to, and the counts once it has been.
type AnnouncementEmail struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	LinkURL  string `json:"link_url,omitempty"`
	LinkText string `json:"link_text,omitempty"`
	Audience string `json:"audience"`
	Status   string `json:"status"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ClonedFrom carries no foreign key: the source may be deleted, and
	// "cloned from something that is gone" is still true.
	ClonedFrom string `json:"cloned_from,omitempty"`
	// ResendOf is the announcement this row resends to the people that run
	// did not reach. No foreign key, for the reason ClonedFrom has none.
	ResendOf string `json:"resend_of,omitempty"`
	// ResentAs is the resend of this row, when there is one — read through
	// the unique index on resend_of, so there is at most one.
	ResentAs string `json:"resent_as,omitempty"`

	SentBy      string     `json:"sent_by,omitempty"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// The five counts. Resolved is the size of the audience at send time and
	// never moves again; the other four are the recipient rows grouped by
	// status, and Recount is what keeps that true.
	Resolved  int `json:"resolved"`
	Delivered int `json:"delivered"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Abandoned int `json:"abandoned"`

	// CreatedByLogin and SentByLogin are filled by the list and the single
	// read, which join them. A record of who told the install what is not
	// readable as a pair of opaque ids.
	CreatedByLogin string `json:"created_by_login,omitempty"`
	SentByLogin    string `json:"sent_by_login,omitempty"`
}

// AnnouncementRecipient is one resolved user and what happened to them.
//
// The login and the address are copied onto the row rather than read back
// through the foreign key, because the record has to outlive the account: an
// announcement that says it reached somebody who has since been deleted is
// still telling the truth about the day it was sent.
type AnnouncementRecipient struct {
	ID             string     `json:"id"`
	AnnouncementID string     `json:"announcement_id"`
	UserID         string     `json:"user_id,omitempty"`
	Login          string     `json:"login"`
	Email          string     `json:"email,omitempty"`
	Status         string     `json:"status"`
	Reason         string     `json:"reason,omitempty"`
	Attempts       int        `json:"attempts"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// AnnouncementEstimate is what the composer shows before the freeze: how many
// people this audience resolves to *today*, and how many of them the install
// cannot reach.
//
// The word on it is "about". A draft composed on Monday and sent on Thursday is
// sent to Thursday's directory, and the frozen row carries `resolved`, which is
// the count that actually happened.
type AnnouncementEstimate struct {
	Audience string `json:"audience"`
	// Total is everybody the audience resolves to, reachable or not.
	Total int `json:"total"`
	// Deliverable is Total minus the two skip groups.
	Deliverable   int `json:"deliverable"`
	SkipLocked    int `json:"skipped_locked"`
	SkipNoAddress int `json:"skipped_no_address"`
	// ResolvedAt is when this was counted, so the composer can say so.
	ResolvedAt time.Time `json:"resolved_at"`
}

const announcementColumns = `a.id, a.subject, a.body, a.link_url, a.link_text,
	a.audience, a.status, COALESCE(a.created_by,''), a.created_at, a.updated_at,
	COALESCE(a.cloned_from,''), COALESCE(a.resend_of,''), COALESCE(rs.id,''),
	COALESCE(a.sent_by,''), a.sent_at, a.completed_at,
	a.resolved, a.delivered, a.failed, a.skipped, a.abandoned,
	COALESCE(c.login,''), COALESCE(s.login,'')`

const announcementFrom = `
	FROM announcement_emails a
	LEFT JOIN users c ON c.id = a.created_by
	LEFT JOIN users s ON s.id = a.sent_by
	LEFT JOIN announcement_emails rs ON rs.resend_of = a.id`

func scanAnnouncement(scan func(dest ...any) error) (*AnnouncementEmail, error) {
	var a AnnouncementEmail
	if err := scan(&a.ID, &a.Subject, &a.Body, &a.LinkURL, &a.LinkText,
		&a.Audience, &a.Status, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt,
		&a.ClonedFrom, &a.ResendOf, &a.ResentAs, &a.SentBy, &a.SentAt, &a.CompletedAt,
		&a.Resolved, &a.Delivered, &a.Failed, &a.Skipped, &a.Abandoned,
		&a.CreatedByLogin, &a.SentByLogin); err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAnnouncementEmail writes a new draft. Status is always draft; the one
// row created already frozen is a resend, and CreateAnnouncementResend writes it.
func (db *DB) CreateAnnouncementEmail(a *AnnouncementEmail) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt, a.Status = now, now, AnnouncementDraft
	_, err := db.conn.Exec(`
		INSERT INTO announcement_emails
			(id, subject, body, link_url, link_text, audience, status,
			 created_by, created_at, updated_at, cloned_from)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Subject, a.Body, a.LinkURL, a.LinkText, a.Audience, a.Status,
		nullIfEmpty(a.CreatedBy), a.CreatedAt, a.UpdatedAt, nullIfEmpty(a.ClonedFrom))
	return err
}

// GetAnnouncementEmail reads one, or nil when there is no such row.
func (db *DB) GetAnnouncementEmail(id string) (*AnnouncementEmail, error) {
	a, err := scanAnnouncement(db.conn.QueryRow(
		`SELECT `+announcementColumns+announcementFrom+` WHERE a.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// ListAnnouncementEmails returns the install's whole announcement history,
// newest first.
func (db *DB) ListAnnouncementEmails() ([]AnnouncementEmail, error) {
	rows, err := db.conn.Query(
		`SELECT ` + announcementColumns + announcementFrom + ` ORDER BY a.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnouncementEmail{}
	for rows.Next() {
		a, err := scanAnnouncement(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpdateAnnouncementDraft rewrites the content of a draft, and reports whether
// there was a draft to rewrite.
//
// Guarded in SQL rather than by a read followed by a write, for the same reason
// the freeze is: `false` here means the row is `sending` or `sent`, and the
// caller answers 409 with the status it actually has.
func (db *DB) UpdateAnnouncementDraft(a *AnnouncementEmail) (bool, error) {
	now := time.Now().UTC()
	res, err := db.conn.Exec(`
		UPDATE announcement_emails
		SET subject = ?, body = ?, link_url = ?, link_text = ?, audience = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		a.Subject, a.Body, a.LinkURL, a.LinkText, a.Audience, now, a.ID, AnnouncementDraft)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	a.UpdatedAt = now
	return n > 0, nil
}

// DeleteAnnouncementEmail removes a draft or a completed record, and reports
// whether it removed one.
//
// Two of the three statuses, and the guard is in the SQL for the same reason
// the freeze is: `false` here means the row is `sending`, and the caller
// answers 409 with the status it actually has. A run in flight is refused
// because the runner is still writing recipient rows and a final count into
// this id — deleting underneath it is a goroutine writing to a row that is
// gone, not a policy question.
//
// A `sent` row deleting is a decision an administrator is allowed to make about
// their own install's history (ADR §79). The recipient rows go with it, by
// cascade — they are this row's dependants and there is no orphan story worth
// having — so what stands in for the record afterwards is the audit row the
// handler writes, which carries the subject, the audience and the five counts
// and is on the audit log's own retention.
func (db *DB) DeleteAnnouncementEmail(id string) (bool, error) {
	res, err := db.conn.Exec(
		`DELETE FROM announcement_emails WHERE id = ? AND status IN (?, ?)`,
		id, AnnouncementDraft, AnnouncementSent)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// BeginAnnouncementSend is the freeze: the one guarded transition from draft to
// sending, and the only writer of sent_by and sent_at.
//
// It reports whether this caller is the one that froze it. Zero rows affected
// means somebody else already did — two administrators on the Send button of
// the same draft is the ordinary case on a shared install, and a status check
// followed by an update sends the announcement twice.
func (db *DB) BeginAnnouncementSend(id, sentBy string) (bool, error) {
	now := time.Now().UTC()
	res, err := db.conn.Exec(`
		UPDATE announcement_emails
		SET status = ?, sent_by = ?, sent_at = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		AnnouncementSending, nullIfEmpty(sentBy), now, now, id, AnnouncementDraft)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ErrAlreadyResent is CreateAnnouncementResend losing to the unique index: the
// announcement has a resend already, most often because two administrators
// pressed the button at once.
var ErrAlreadyResent = errors.New("this announcement has already been resent")

// resendableStatuses are the outcomes a resend retries. Not `skipped`: a
// locked account or a missing address is the install being what it is, and a
// second run would skip them again for the same reason.
const resendableWhere = `p.status IN ('failed','abandoned')`

// CountAnnouncementResendable is how many people a resend of this announcement
// would resolve to now: its failed and abandoned rows whose account still
// exists. Zero once the per-person rows have been pruned.
func (db *DB) CountAnnouncementResendable(announcementID string) (int, error) {
	var n int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM announcement_email_recipients p
		JOIN users u ON u.id = p.user_id
		WHERE p.announcement_id = ? AND `+resendableWhere, announcementID).Scan(&n)
	return n, err
}

// CreateAnnouncementResend writes the resend of a sent announcement and freezes
// it in the same statement, and returns it — or nil when the original is not
// `sent`, and ErrAlreadyResent when it has been resent already.
//
// Created already `sending` rather than as a draft followed by the freeze: the
// content is the original's, frozen when it went out, and a draft would be a
// row somebody could edit into a second message under the first one's name.
// One INSERT … SELECT guarded on the original's status is the freeze, and the
// unique index on resend_of is what makes two presses one resend.
func (db *DB) CreateAnnouncementResend(originalID, sentBy string) (*AnnouncementEmail, error) {
	id := uuid.New().String()
	now := time.Now().UTC()
	res, err := db.conn.Exec(`
		INSERT INTO announcement_emails
			(id, subject, body, link_url, link_text, audience, status,
			 created_by, created_at, updated_at, resend_of, sent_by, sent_at)
		SELECT ?, subject, body, link_url, link_text, audience, ?,
		       ?, ?, ?, id, ?, ?
		FROM announcement_emails WHERE id = ? AND status = ?`,
		id, AnnouncementSending, nullIfEmpty(sentBy), now, now, nullIfEmpty(sentBy), now,
		originalID, AnnouncementSent)
	if isUniqueViolation(err) {
		return nil, ErrAlreadyResent
	}
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return nil, err
	}
	return db.GetAnnouncementEmail(id)
}

// audienceWhere is the one definition of who an audience is, used by both the
// estimate and the resolution so the number the composer shows and the set the
// runner mails cannot come from two different queries.
//
// `publishers` includes system_admin, and that is a decision rather than a
// convenience: an administrator is a strict superset of a publisher in
// capability, everybody who administers this install also puts things on it,
// and an announcement about the build queue that reaches every publisher
// *except the people who run the queue* is the one failure this switch can
// produce.
func audienceWhere(audience string) (string, error) {
	switch audience {
	case AnnouncementAudienceAll:
		return `TRUE`, nil
	case AnnouncementAudiencePublishers:
		return `system_role IN ('publisher','system_admin')`, nil
	default:
		return "", fmt.Errorf("unknown announcement audience %q", audience)
	}
}

// EstimateAnnouncementAudience counts who an audience resolves to right now,
// and how many of them cannot be reached.
//
// It is an estimate and the composer says so. Nothing is frozen by counting.
func (db *DB) EstimateAnnouncementAudience(audience string) (AnnouncementEstimate, error) {
	where, err := audienceWhere(audience)
	if err != nil {
		return AnnouncementEstimate{}, err
	}
	e := AnnouncementEstimate{Audience: audience, ResolvedAt: time.Now().UTC()}
	err = db.conn.QueryRow(`
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE locked_at IS NOT NULL),
		       COUNT(*) FILTER (WHERE locked_at IS NULL AND COALESCE(email,'') = '')
		FROM users WHERE `+where).
		Scan(&e.Total, &e.SkipLocked, &e.SkipNoAddress)
	if err != nil {
		return AnnouncementEstimate{}, err
	}
	e.Deliverable = e.Total - e.SkipLocked - e.SkipNoAddress
	return e, nil
}

// ResolveAnnouncementRecipients resolves the audience and writes one recipient
// row per person, in a single transaction, and returns the rows that still have
// somewhere to go.
//
// One query and one transaction is what makes the skip rule honest. A user
// locked between "who is in the audience" and "who is reachable" would otherwise
// be both in the set and skipped, or in neither; here the row is `skipped` with
// a reason or it is `pending`, and there is no window in which both are true.
//
// It also writes `resolved` and `skipped`, which is what the frozen record
// means by the size of the audience at send time.
func (db *DB) ResolveAnnouncementRecipients(announcementID, audience string) ([]AnnouncementRecipient, error) {
	where, err := audienceWhere(audience)
	if err != nil {
		return nil, err
	}
	return db.resolveRecipients(announcementID, `
		SELECT id, login, COALESCE(email,''), locked_at IS NOT NULL
		FROM users WHERE `+where+` ORDER BY login`)
}

// ResolveAnnouncementResend is ResolveAnnouncementRecipients for a resend: the
// original's failed and abandoned people, read back through their accounts
// *now*. An address fixed since the first run is the one mailed, an account
// locked since is skipped `locked` by the same rule, and an account deleted
// since is not in the set — it is nobody to send to, not a skip.
func (db *DB) ResolveAnnouncementResend(announcementID, originalID string) ([]AnnouncementRecipient, error) {
	return db.resolveRecipients(announcementID, `
		SELECT u.id, u.login, COALESCE(u.email,''), u.locked_at IS NOT NULL
		FROM announcement_email_recipients p
		JOIN users u ON u.id = p.user_id
		WHERE p.announcement_id = ? AND `+resendableWhere+`
		ORDER BY u.login`, originalID)
}

// resolveRecipients runs one "who" query and writes a recipient row per person
// it returns, in one transaction, with the skip rule applied.
func (db *DB) resolveRecipients(announcementID, who string, args ...any) ([]AnnouncementRecipient, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(who, args...)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var all []AnnouncementRecipient
	for rows.Next() {
		var r AnnouncementRecipient
		var locked bool
		if err := rows.Scan(&r.UserID, &r.Login, &r.Email, &locked); err != nil {
			rows.Close()
			return nil, err
		}
		r.ID = uuid.New().String()
		r.AnnouncementID = announcementID
		r.CreatedAt = now
		switch {
		case locked:
			r.Status, r.Reason, r.CompletedAt = AnnouncementSkipped, AnnouncementSkipLocked, &now
		case r.Email == "":
			r.Status, r.Reason, r.CompletedAt = AnnouncementSkipped, AnnouncementSkipNoAddress, &now
		default:
			r.Status = AnnouncementPending
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pending := make([]AnnouncementRecipient, 0, len(all))
	skipped := 0
	for _, r := range all {
		if _, err := tx.Exec(`
			INSERT INTO announcement_email_recipients
				(id, announcement_id, user_id, login, email, status, reason, created_at, completed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.AnnouncementID, nullIfEmpty(r.UserID), r.Login, r.Email,
			r.Status, r.Reason, r.CreatedAt, r.CompletedAt); err != nil {
			return nil, err
		}
		if r.Status == AnnouncementSkipped {
			skipped++
			continue
		}
		pending = append(pending, r)
	}
	if _, err := tx.Exec(`
		UPDATE announcement_emails SET resolved = ?, skipped = ?, updated_at = ?
		WHERE id = ?`, len(all), skipped, now, announcementID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return pending, nil
}

// CompleteAnnouncementRecipient records what happened to one person. Only a
// pending row moves, so a startup sweep that already abandoned a run cannot be
// overwritten by a transport that was still in flight.
func (db *DB) CompleteAnnouncementRecipient(id, status, reason string, attempts int) error {
	_, err := db.conn.Exec(`
		UPDATE announcement_email_recipients
		SET status = ?, reason = ?, attempts = ?, completed_at = ?
		WHERE id = ? AND status = ?`,
		status, truncate(reason, 2000), attempts, time.Now().UTC(), id, AnnouncementPending)
	return err
}

// ListAnnouncementRecipients returns the recipient rows for one announcement,
// worst news first: failures, then skips, then everything else.
//
// The order is the point of the page. An administrator opening a sent
// announcement is looking for who did not get it, and a list sorted by login
// buries that under four hundred successes.
func (db *DB) ListAnnouncementRecipients(announcementID string) ([]AnnouncementRecipient, error) {
	rows, err := db.conn.Query(`
		SELECT id, announcement_id, COALESCE(user_id,''), login, email, status, reason,
		       attempts, created_at, completed_at
		FROM announcement_email_recipients
		WHERE announcement_id = ?
		ORDER BY CASE status
			WHEN 'failed' THEN 0 WHEN 'abandoned' THEN 1 WHEN 'skipped' THEN 2
			WHEN 'pending' THEN 3 ELSE 4 END, login`, announcementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnnouncementRecipient{}
	for rows.Next() {
		var r AnnouncementRecipient
		if err := rows.Scan(&r.ID, &r.AnnouncementID, &r.UserID, &r.Login, &r.Email,
			&r.Status, &r.Reason, &r.Attempts, &r.CreatedAt, &r.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// countsFromRows is the one expression of "the counts are the recipient rows".
//
// Every writer of the four outcome counts goes through it — the running
// recount, the completion, and the startup abandon — so the row cannot report a
// figure the rows underneath it do not support. `resolved` is not recomputed
// here: it is the size of the audience at send time and never moves again.
const countsFromRows = `
	resolved  = a.resolved,
	delivered = c.delivered,
	failed    = c.failed,
	skipped   = c.skipped,
	abandoned = c.abandoned`

const countsSubquery = `
	SELECT COUNT(*) FILTER (WHERE status = 'delivered') AS delivered,
	       COUNT(*) FILTER (WHERE status = 'failed')    AS failed,
	       COUNT(*) FILTER (WHERE status = 'skipped')   AS skipped,
	       COUNT(*) FILTER (WHERE status = 'abandoned') AS abandoned
	FROM announcement_email_recipients WHERE announcement_id = ?`

// RecountAnnouncementEmail refreshes the counts from the rows while a run is in
// flight, so the detail page a nervous administrator is watching is never ahead
// of or behind what actually happened.
func (db *DB) RecountAnnouncementEmail(id string) error {
	_, err := db.conn.Exec(`
		UPDATE announcement_emails a SET`+countsFromRows+`
		FROM (`+countsSubquery+`) c
		WHERE a.id = ?`, id, id)
	return err
}

// FinishAnnouncementSend writes the final counts and moves the row to sent.
//
// Guarded on `sending` so a run that a startup sweep already completed cannot
// be completed a second time, and the counts and the status move together: a
// row that said `sent` with figures from before the last recipient would be the
// one lie this record cannot afford.
func (db *DB) FinishAnnouncementSend(id string) error {
	now := time.Now().UTC()
	_, err := db.conn.Exec(`
		UPDATE announcement_emails a SET`+countsFromRows+`,
			status = ?, completed_at = ?, updated_at = ?
		FROM (`+countsSubquery+`) c
		WHERE a.id = ? AND a.status = ?`,
		AnnouncementSent, now, now, id, id, AnnouncementSending)
	return err
}

// AbandonStrandedAnnouncements is the startup sweep. Any announcement still
// `sending` when this process starts belonged to a process that no longer
// exists: its pending recipients become `abandoned` and the row completes.
//
// It never resumes and it never claims. Resuming needs a durable queue with an
// at-least-once story, which is the outbox this product has ruled out; and
// leaving the row in `sending` forever would be a status meaning "we do not
// know", which is how a record starts lying by omission. The row then reads
// *203 delivered, 0 failed, 47 abandoned*, which is the truth.
func (db *DB) AbandonStrandedAnnouncements() (announcements, recipients int, err error) {
	rows, err := db.conn.Query(
		`SELECT id FROM announcement_emails WHERE status = ?`, AnnouncementSending)
	if err != nil {
		return 0, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		n, err := db.AbandonAnnouncementSend(id)
		if err != nil {
			return announcements, recipients, err
		}
		announcements++
		recipients += n
	}
	return announcements, recipients, nil
}

// AbandonAnnouncementSend abandons one run: pending recipients become
// abandoned, then the row completes with the counts that describes.
func (db *DB) AbandonAnnouncementSend(id string) (int, error) {
	res, err := db.conn.Exec(`
		UPDATE announcement_email_recipients
		SET status = ?, completed_at = ?
		WHERE announcement_id = ? AND status = ?`,
		AnnouncementAbandoned, time.Now().UTC(), id, AnnouncementPending)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), db.FinishAnnouncementSend(id)
}

// PruneAnnouncementRecipients deletes the recipient rows of announcements that
// completed longer ago than the install keeps them.
//
// Two facts with different lifetimes, and separating them is the point. "On the
// 3rd of March this install told 214 people the queue was down, and 12 sends
// failed" is the install's own record and there is no date on which it stops
// being worth having, so the announcement row and its counts are never pruned.
// "We mailed bob@example.com on the 3rd of March" is a named person's data with
// a shorter honest answer.
//
// The cutoff is on completed_at, so a run still in flight is never eligible: a
// row without one has no completion to be older than.
func (db *DB) PruneAnnouncementRecipients(days int) (int, time.Time, error) {
	if days <= 0 {
		return 0, time.Time{}, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	res, err := db.conn.Exec(`
		DELETE FROM announcement_email_recipients
		WHERE announcement_id IN (
			SELECT id FROM announcement_emails
			WHERE completed_at IS NOT NULL AND completed_at < ?)`, cutoff)
	if err != nil {
		return 0, cutoff, err
	}
	n, err := res.RowsAffected()
	return int(n), cutoff, err
}
