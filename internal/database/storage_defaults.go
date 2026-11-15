package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Default delegations (ADR § Storage somebody else can attach): an install's
// list of subjects every storage resource is delegated to when it is created.
//
// **Materialised, never consulted.** A resource's create writes one ordinary
// storage_managers row per default (origin `default`), and from then on it is a
// delegation like any other — listed on the resource, revocable there alone,
// editable to a lower ceiling. A wildcard row consulted at attach time could
// not exclude the one sensitive resource an administrator creates later, since
// the product has no deny, and its stamp could not say which row it came
// through.
//
// **Absent, not off.** No row, no default; nothing turns the feature on.

// StorageDefaultManager is one entry of the list.
type StorageDefaultManager struct {
	ID          string    `json:"id"`
	SubjectType string    `json:"subject_type"`
	SubjectID   string    `json:"subject_id"`
	SubjectName string    `json:"subject_name,omitempty"`
	MaxAccess   string    `json:"max_access"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// Delegations is how many resources carry a delegation this default
	// wrote, and Apps the apps those delegations attach storage to — what
	// removing it with revoke_existing would revoke and restart, read before
	// the click.
	Delegations int                 `json:"delegations"`
	Apps        []StorageManagedApp `json:"apps"`
}

// Origins of a storage_managers row.
const (
	DelegationOriginAdmin   = "admin"
	DelegationOriginDefault = "default"
)

// ValidDefaultAccess is the two levels every kind's ladder has, which is all a
// default may name: it is written onto volumes and stores alike.
func ValidDefaultAccess(level string) bool { return level == "read" || level == "full" }

func (db *DB) ListStorageDefaultManagers() ([]StorageDefaultManager, error) {
	rows, err := db.conn.Query(`
		SELECT d.id, d.subject_type, COALESCE(d.user_id, d.group_id), d.max_access,
		       COALESCE(d.created_by, ''), d.created_at,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, g.name, '')
		FROM storage_default_managers d
		LEFT JOIN users u ON u.id = d.user_id
		LEFT JOIN groups g ON g.id = d.group_id
		ORDER BY d.subject_type, COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, g.name, '')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StorageDefaultManager{}
	for rows.Next() {
		var d StorageDefaultManager
		if err := rows.Scan(&d.ID, &d.SubjectType, &d.SubjectID, &d.MaxAccess, &d.CreatedBy, &d.CreatedAt, &d.SubjectName); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err := db.defaultReach(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// defaultReach fills what a default's delegations hold.
func (db *DB) defaultReach(d *StorageDefaultManager) error {
	if err := db.conn.QueryRow(`SELECT count(*) FROM storage_managers
		WHERE subject_type = ? AND subject_id = ? AND origin = 'default'`, d.SubjectType, d.SubjectID).
		Scan(&d.Delegations); err != nil {
		return err
	}
	rows, err := db.conn.Query(`
		SELECT DISTINCT apps.id, apps.slug, apps.name
		FROM storage_mappings m
		JOIN storage_managers sm ON sm.resource_id = m.resource_id AND sm.subject_type = ? AND sm.subject_id = ?
		     AND sm.origin = 'default'
		JOIN apps ON apps.id = m.app_id
		WHERE m.authority = ?
		ORDER BY apps.name`, d.SubjectType, d.SubjectID, AuthorityKey(d.SubjectType, d.SubjectID))
	if err != nil {
		return err
	}
	defer rows.Close()
	d.Apps = []StorageManagedApp{}
	for rows.Next() {
		var a StorageManagedApp
		if err := rows.Scan(&a.ID, &a.Slug, &a.Name); err != nil {
			return err
		}
		d.Apps = append(d.Apps, a)
	}
	return rows.Err()
}

func subjectColumns(subjectType, subjectID string) (user, group any) {
	if subjectType == StoreSubjectGroup {
		return nil, subjectID
	}
	return subjectID, nil
}

// PutStorageDefaultManager adds a default, or changes its level. It writes no
// delegation by itself: future creates do, and ApplyStorageDefault offers the
// existing resources.
func (db *DB) PutStorageDefaultManager(d *StorageDefaultManager, actorID string, meta RequestMeta) error {
	if !ValidDefaultAccess(d.MaxAccess) {
		return fmt.Errorf("%w: a default is read or full", ErrStorageCeiling)
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	col := "user_id"
	if d.SubjectType == StoreSubjectGroup {
		col = "group_id"
	}
	var before string
	err = tx.QueryRow(`SELECT id, max_access FROM storage_default_managers WHERE `+col+` = ?`, d.SubjectID).
		Scan(&d.ID, &before)
	now := time.Now().UTC()
	switch {
	case err == sql.ErrNoRows:
		d.ID, d.CreatedBy, d.CreatedAt = uuid.New().String(), actorID, now
		user, group := subjectColumns(d.SubjectType, d.SubjectID)
		if _, err := tx.Exec(`INSERT INTO storage_default_managers (id, subject_type, user_id, group_id, max_access, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, d.ID, d.SubjectType, user, group, d.MaxAccess, nullIfEmpty(actorID), now); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := tx.Exec(`UPDATE storage_default_managers SET max_access = ? WHERE id = ?`, d.MaxAccess, d.ID); err != nil {
			return err
		}
	}
	e := AuditEntry{ActorType: PrincipalTypeUser, ActorID: actorID, Action: "storage_default_manager.put",
		TargetType: d.SubjectType, TargetID: d.SubjectID,
		After:     jsonStr(map[string]string{"subject_type": d.SubjectType, "subject_id": d.SubjectID, "max_access": d.MaxAccess}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent}
	if before != "" {
		e.Before = jsonStr(map[string]string{"max_access": before})
	}
	if err := writeAuditTx(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteStorageDefaultManager stops future creates delegating to the subject.
// It revokes nothing: a settings edit that restarts every production app is
// the surprise lowering a ceiling already refuses to be.
func (db *DB) DeleteStorageDefaultManager(subjectType, subjectID, actorID string, meta RequestMeta) error {
	col := "user_id"
	if subjectType == StoreSubjectGroup {
		col = "group_id"
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM storage_default_managers WHERE subject_type = ? AND `+col+` = ?`, subjectType, subjectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := writeAuditTx(tx, AuditEntry{ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "storage_default_manager.delete", TargetType: subjectType, TargetID: subjectID,
		Before:    jsonStr(map[string]string{"subject_type": subjectType, "subject_id": subjectID}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent}); err != nil {
		return err
	}
	return tx.Commit()
}

// insertDefaultDelegationsTx is the create's half: one delegation per default
// onto the resource being created, in its transaction, audited as the
// creator's storage_manager.put.
func insertDefaultDelegationsTx(tx *Tx, resourceID, actorID, requestID, ip, userAgent string) error {
	rows, err := tx.Query(`SELECT subject_type, COALESCE(user_id, group_id), max_access FROM storage_default_managers`)
	if err != nil {
		return err
	}
	type def struct{ subjectType, subjectID, maxAccess string }
	var defs []def
	for rows.Next() {
		var d def
		if err := rows.Scan(&d.subjectType, &d.subjectID, &d.maxAccess); err != nil {
			rows.Close()
			return err
		}
		defs = append(defs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, d := range defs {
		if _, err := tx.Exec(`
			INSERT INTO storage_managers (id, resource_id, subject_type, subject_id, max_access, origin, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'default', ?, ?, ?)`,
			uuid.New().String(), resourceID, d.subjectType, d.subjectID, d.maxAccess,
			nullIfEmpty(actorID), now, now); err != nil {
			return err
		}
		if err := writeAuditTx(tx, AuditEntry{ActorType: PrincipalTypeUser, ActorID: actorID,
			Action: "storage_manager.put", TargetType: "storage_resource", TargetID: resourceID,
			After: jsonStr(map[string]string{"subject_type": d.subjectType, "subject_id": d.subjectID,
				"max_access": d.maxAccess, "origin": DelegationOriginDefault}),
			RequestID: requestID, IP: ip, UserAgent: userAgent}); err != nil {
			return err
		}
	}
	return nil
}

// DefaultApplyReport is what offering a default to the existing resources did.
type DefaultApplyReport struct {
	// Applied are the resources the subject is now delegated on.
	Applied []string `json:"applied"`
	// Differing are resources where the subject already had a delegation at
	// another ceiling — left alone, because a default must not raise or lower
	// a choice an administrator made per resource.
	Differing []DefaultDiffering `json:"differing"`
}

// DefaultDiffering is one resource left at its own ceiling.
type DefaultDiffering struct {
	Resource  string `json:"resource"`
	MaxAccess string `json:"max_access"`
}

// ApplyStorageDefault writes the default's delegation on every existing
// resource where the subject has none, and reports the ones where it already
// has one at a different ceiling.
func (db *DB) ApplyStorageDefault(subjectType, subjectID, maxAccess, actorID string, meta RequestMeta) (DefaultApplyReport, error) {
	report := DefaultApplyReport{Applied: []string{}, Differing: []DefaultDiffering{}}
	tx, err := db.conn.Begin()
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
		SELECT r.id, r.slug, COALESCE(m.max_access, '')
		FROM storage_resources r
		LEFT JOIN storage_managers m ON m.resource_id = r.id AND m.subject_type = ? AND m.subject_id = ?
		ORDER BY r.slug`, subjectType, subjectID)
	if err != nil {
		return report, err
	}
	type res struct{ id, slug, have string }
	var all []res
	for rows.Next() {
		var r res
		if err := rows.Scan(&r.id, &r.slug, &r.have); err != nil {
			rows.Close()
			return report, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, err
	}
	now := time.Now().UTC()
	for _, r := range all {
		switch {
		case r.have == maxAccess:
			continue
		case r.have != "":
			report.Differing = append(report.Differing, DefaultDiffering{Resource: r.slug, MaxAccess: r.have})
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO storage_managers (id, resource_id, subject_type, subject_id, max_access, origin, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'default', ?, ?, ?)`,
			uuid.New().String(), r.id, subjectType, subjectID, maxAccess, nullIfEmpty(actorID), now, now); err != nil {
			return report, err
		}
		if err := writeAuditTx(tx, AuditEntry{ActorType: PrincipalTypeUser, ActorID: actorID,
			Action: "storage_manager.put", TargetType: "storage_resource", TargetID: r.id,
			After: jsonStr(map[string]string{"subject_type": subjectType, "subject_id": subjectID,
				"max_access": maxAccess, "origin": DelegationOriginDefault}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent}); err != nil {
			return report, err
		}
		report.Applied = append(report.Applied, r.slug)
	}
	return report, tx.Commit()
}

// DefaultDelegationResources lists the resources on which the subject holds a
// delegation a default wrote — what revoke_existing collects, and nothing an
// administrator delegated per resource.
func (db *DB) DefaultDelegationResources(subjectType, subjectID string) ([]string, error) {
	rows, err := db.conn.Query(`SELECT resource_id FROM storage_managers
		WHERE subject_type = ? AND subject_id = ? AND origin = 'default' ORDER BY resource_id`, subjectType, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
