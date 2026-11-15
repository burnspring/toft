package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Accepted risk, with an expiry and somebody's name on it.
//
// Three rules do the work, and each removes a way this control dies quietly:
//
//   - **A reason is required, and it is free text.** The next administrator,
//     six months on, needs the sentence somebody wrote at the time.
//   - **There is no permanent waiver.** A permanent one is how a list of
//     exceptions comes to be something nobody has read since the quarter they
//     were filed. An expired waiver is not renewed and not warned about in
//     advance beyond its visible expiry — the next deploy refuses again, and
//     that is the review.
//   - **Never per-app-blanket.** "This app is exempt from dependency policy"
//     would be an exception invisible in the list an administrator audits,
//     which is ADR §29 D3's reasoning kept whole.

// DependencyWaiver is one accepted risk.
type DependencyWaiver struct {
	ID         string `json:"id"`
	AppID      string `json:"app_id"`
	AppSlug    string `json:"app_slug,omitempty"`
	AdvisoryID string `json:"advisory_id"`
	// Package narrows the waiver to one package of an advisory that covers
	// several. Empty means the whole advisory on this app.
	Package string `json:"package,omitempty"`
	Reason  string `json:"reason"`

	ExpiresAt time.Time  `json:"expires_at"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`

	// CreatedByName is filled in for display. The list is read by whoever comes
	// next, and a GUID is not who accepted the risk.
	CreatedByName string `json:"created_by_name,omitempty"`
	// Severity and Summary describe what was waived, carried from the corpus
	// so the list an administrator audits says what each entry is *about*
	// rather than only its identifier.
	Severity string `json:"severity,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// Live reports whether this waiver applies right now.
func (w DependencyWaiver) Live() bool {
	return w.RevokedAt == nil && w.ExpiresAt.After(time.Now())
}

// Expiring is how long is left, for the copy that reads "expires in 6 days"
// rather than a bare date. **A lapsing waiver is the one transition nobody
// performs** — at the moment it expires the app's banner appears and the
// estate view gains a row, with nobody having pressed anything — so it must
// not be a surprise, even though there is deliberately no reminder and no
// renewal flow.
func (w DependencyWaiver) Expiring() time.Duration { return time.Until(w.ExpiresAt) }

// waiverSet is one app's live waivers, indexed for matching.
type waiverSet struct {
	// byAdvisory covers every package of an advisory.
	byAdvisory map[string]DependencyWaiver
	// byPackage is the narrowed form, keyed advisory+"\x00"+package.
	byPackage map[string]DependencyWaiver
}

// match finds the waiver that applies, preferring the narrow one.
func (s waiverSet) match(advisoryID, pkg string) (DependencyWaiver, bool) {
	if w, ok := s.byPackage[advisoryID+"\x00"+pkg]; ok {
		return w, true
	}
	w, ok := s.byAdvisory[advisoryID]
	return w, ok
}

func (db *DB) liveWaivers(appID string) (waiverSet, error) {
	set := waiverSet{byAdvisory: map[string]DependencyWaiver{}, byPackage: map[string]DependencyWaiver{}}
	if appID == "" {
		return set, nil
	}
	list, err := db.ListDependencyWaivers(appID, false)
	if err != nil {
		return set, err
	}
	for _, w := range list {
		if !w.Live() {
			continue
		}
		if w.Package != "" {
			set.byPackage[w.AdvisoryID+"\x00"+w.Package] = w
		} else {
			set.byAdvisory[w.AdvisoryID] = w
		}
	}
	return set, nil
}

// ListDependencyWaivers is one app's waivers. includeExpired brings back the
// lapsed and revoked ones, which the estate view wants and an app tab does not.
func (db *DB) ListDependencyWaivers(appID string, includeExpired bool) ([]DependencyWaiver, error) {
	where := "w.app_id = ?"
	args := []any{appID}
	if !includeExpired {
		where += " AND w.revoked_at IS NULL AND w.expires_at > ?"
		args = append(args, time.Now().UTC())
	}
	return db.queryWaivers(where, args)
}

// ListAllDependencyWaivers is every live waiver on the install, **sorted by
// expiry**, because the aggregate is what a security review asks for and it
// must not require walking forty app pages to assemble.
func (db *DB) ListAllDependencyWaivers() ([]DependencyWaiver, error) {
	return db.queryWaivers("w.revoked_at IS NULL AND w.expires_at > ?", []any{time.Now().UTC()})
}

func (db *DB) queryWaivers(where string, args []any) ([]DependencyWaiver, error) {
	rows, err := db.conn.Query(`
		SELECT w.id, w.app_id, COALESCE(apps.slug, ''), w.advisory_id, w.package, w.reason,
		       w.expires_at, COALESCE(w.created_by,''), w.created_at, w.revoked_at,
		       COALESCE(NULLIF(users.name, ''), NULLIF(users.login, ''), users.email, ''),
		       COALESCE(a.severity, ''), COALESCE(a.summary, '')
		FROM dependency_waivers w
		LEFT JOIN apps ON apps.id = w.app_id
		LEFT JOIN users ON users.id = w.created_by
		LEFT JOIN advisories a ON a.id = w.advisory_id
		WHERE `+where+`
		ORDER BY w.expires_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DependencyWaiver{}
	for rows.Next() {
		var w DependencyWaiver
		if err := rows.Scan(&w.ID, &w.AppID, &w.AppSlug, &w.AdvisoryID, &w.Package, &w.Reason,
			&w.ExpiresAt, &w.CreatedBy, &w.CreatedAt, &w.RevokedAt,
			&w.CreatedByName, &w.Severity, &w.Summary); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GrantDependencyWaiver records an accepted risk and audits it in the same
// transaction.
//
// Administrator-only, enforced by the handler: accepting risk on behalf of the
// organization is not content editing, which is the same argument that makes
// re-pointing an app's repository admin-only.
//
// maxDays is the install's cap. Zero means waivers are off entirely, and this
// refuses rather than storing one that would be a lie on every surface.
func (db *DB) GrantDependencyWaiver(w *DependencyWaiver, maxDays int, actorID string, meta RequestMeta) error {
	w.AdvisoryID = strings.TrimSpace(w.AdvisoryID)
	w.Package = strings.TrimSpace(w.Package)
	w.Reason = strings.TrimSpace(w.Reason)

	if w.AppID == "" || w.AdvisoryID == "" {
		return fmt.Errorf("a waiver needs an app and an advisory")
	}
	if w.Reason == "" {
		return fmt.Errorf("a waiver needs a reason — the next administrator has to be able to " +
			"read why this risk was accepted")
	}
	if maxDays <= 0 {
		return fmt.Errorf("this install does not grant dependency waivers")
	}
	if w.ExpiresAt.IsZero() {
		return fmt.Errorf("a waiver needs an expiry — there is no permanent waiver")
	}
	limit := time.Now().UTC().AddDate(0, 0, maxDays)
	if w.ExpiresAt.After(limit) {
		return fmt.Errorf("a waiver may run for at most %d days on this install, so it cannot "+
			"expire after %s", maxDays, limit.Format("2 January 2006"))
	}
	if !w.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("that expiry has already passed")
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	w.ID = uuid.New().String()
	w.CreatedBy = actorID
	w.CreatedAt = time.Now().UTC()
	w.ExpiresAt = w.ExpiresAt.UTC()

	if _, err := tx.Exec(`
		INSERT INTO dependency_waivers
			(id, app_id, advisory_id, package, reason, expires_at, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.AppID, w.AdvisoryID, w.Package, w.Reason, w.ExpiresAt, nullIfEmpty(w.CreatedBy), w.CreatedAt); err != nil {
		return err
	}

	// The whole entry, in the clear. A waiver is the compensating control's
	// subject rather than a secret: what was accepted, on what, until when and
	// why is exactly what the log exists to hold.
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "dependency_waiver_granted", TargetType: "app", TargetID: w.AppID,
		After: jsonStr(map[string]any{
			"advisory_id": w.AdvisoryID, "package": w.Package, "reason": w.Reason,
			"expires_at": w.ExpiresAt,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return fmt.Errorf("auditing the waiver: %w", err)
	}
	return tx.Commit()
}

// RevokeDependencyWaiver withdraws one, and audits that too.
//
// Revoked rather than deleted, so the estate view can still explain why a
// finding that was quiet last week is loud today.
func (db *DB) RevokeDependencyWaiver(appID, waiverID, actorID string, meta RequestMeta) (bool, error) {
	var advisoryID, pkg, reason string
	var expires time.Time
	// Scoped by app_id in the query, not filtered after: this route takes an
	// id from the URL, so the IDOR rule is in force.
	err := db.conn.QueryRow(`
		SELECT advisory_id, package, reason, expires_at FROM dependency_waivers
		WHERE id=? AND app_id=? AND revoked_at IS NULL`, waiverID, appID).
		Scan(&advisoryID, &pkg, &reason, &expires)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if _, err := tx.Exec(
		`UPDATE dependency_waivers SET revoked_at=? WHERE id=? AND app_id=?`,
		now, waiverID, appID); err != nil {
		return false, err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "dependency_waiver_revoked", TargetType: "app", TargetID: appID,
		Before: jsonStr(map[string]any{
			"advisory_id": advisoryID, "package": pkg, "reason": reason, "expires_at": expires,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return false, fmt.Errorf("auditing the revocation: %w", err)
	}
	return true, tx.Commit()
}
