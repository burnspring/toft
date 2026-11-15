package database

import (
	"database/sql"
	"errors"
	"time"
)

// StorageDriveLabel is what an administrator calls one drive. The drive itself
// is declared by the operator; this is only its name for people (ADR §12a).
type StorageDriveLabel struct {
	Key         string    `json:"key"`
	Label       string    `json:"label"`
	Description string    `json:"description"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DriveKeyForRoot and DriveKeyForClass are the two shapes of a drive key. The
// prefix keeps a root and a class of the same name apart.
func DriveKeyForRoot(name string) string  { return "root:" + name }
func DriveKeyForClass(name string) string { return "class:" + name }

// ListStorageDriveLabels returns every label, by key.
func (db *DB) ListStorageDriveLabels() (map[string]StorageDriveLabel, error) {
	rows, err := db.conn.Query(`SELECT key, label, description, COALESCE(updated_by, ''), updated_at FROM storage_drives`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]StorageDriveLabel{}
	for rows.Next() {
		var l StorageDriveLabel
		if err := rows.Scan(&l.Key, &l.Label, &l.Description, &l.UpdatedBy, &l.UpdatedAt); err != nil {
			return nil, err
		}
		out[l.Key] = l
	}
	return out, rows.Err()
}

// PutStorageDriveLabel sets or, with an empty label, clears one drive's label,
// audited as storage_drive.label with what it was before.
func (db *DB) PutStorageDriveLabel(key, label, description, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var before *StorageDriveLabel
	var prev StorageDriveLabel
	err = tx.QueryRow(`SELECT key, label, description FROM storage_drives WHERE key = ?`, key).
		Scan(&prev.Key, &prev.Label, &prev.Description)
	switch {
	case err == nil:
		before = &prev
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	var after *StorageDriveLabel
	if label == "" {
		if _, err := tx.Exec(`DELETE FROM storage_drives WHERE key = ?`, key); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`
			INSERT INTO storage_drives (key, label, description, updated_by, updated_at)
			VALUES (?, ?, ?, ?, now())
			ON CONFLICT (key) DO UPDATE SET label = EXCLUDED.label, description = EXCLUDED.description,
				updated_by = EXCLUDED.updated_by, updated_at = now()`,
			key, label, description, nullIfEmpty(actorID)); err != nil {
			return err
		}
		after = &StorageDriveLabel{Key: key, Label: label, Description: description}
	}

	entry := AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_drive.label",
		TargetType: "storage_drive", TargetID: key,
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}
	if before != nil {
		entry.Before = jsonStr(before)
	}
	if after != nil {
		entry.After = jsonStr(after)
	}
	if err := writeAuditTx(tx, entry); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordStorageClasses writes the class each cluster volume is bound from into
// every one that names none, using resolve for the backing's class. It runs at
// startup and changes a row once: a volume naming no class used to follow
// KUBE_STORAGE_CLASS at every binding, so changing the variable moved where its
// next claim went. Audited per row, as the system. Returns how many it wrote.
func (db *DB) RecordStorageClasses(resolve func(backing string) string) (int, error) {
	resources, err := db.ListStorageResources()
	if err != nil {
		return 0, err
	}
	written := 0
	for _, r := range resources {
		if r.Kind != StorageKindVolume || r.Root != "" || r.StorageClass != "" {
			continue
		}
		class := resolve(r.Backing)
		if class == "" {
			continue
		}
		tx, err := db.conn.Begin()
		if err != nil {
			return written, err
		}
		res, err := tx.Exec(`UPDATE storage_resources SET storage_class = ?, updated_at = now()
			WHERE id = ? AND storage_class = ''`, class, r.ID)
		if err != nil {
			tx.Rollback()
			return written, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			tx.Rollback()
			continue
		}
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeSystem, Action: "storage_resource.class_recorded",
			TargetType: "storage_resource", TargetID: r.ID,
			Before: jsonStr(map[string]string{"storage_class": ""}),
			After:  jsonStr(map[string]string{"storage_class": class}),
		}); err != nil {
			tx.Rollback()
			return written, err
		}
		if err := tx.Commit(); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// ErrRepointRaced is a re-point that found fewer resources on the old name than
// it was asked to move: something changed between the proof and the write.
var ErrRepointRaced = errors.New("the resources on that drive changed while it was being re-pointed; check again")

// RepointStorageResources moves every resource recorded against one drive name
// to another, in one transaction: kind "root" moves storage_root, "class"
// moves storage_class. want is the slugs the caller proved, and the write
// refuses unless it moves exactly those — the proof and the move are about the
// same set or neither happens. The old drive's label is carried to the new key
// when the new one has none. One audit row per resource.
func (db *DB) RepointStorageResources(kind, from, to string, want []string, actorID, requestID, ip, userAgent string) error {
	column := map[string]string{"root": "storage_root", "class": "storage_class"}[kind]
	if column == "" {
		return errors.New("a drive is a root or a class")
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id, slug FROM storage_resources WHERE `+column+` = ? ORDER BY slug FOR UPDATE`, from)
	if err != nil {
		return err
	}
	type moved struct{ id, slug string }
	var found []moved
	for rows.Next() {
		var m moved
		if err := rows.Scan(&m.id, &m.slug); err != nil {
			rows.Close()
			return err
		}
		found = append(found, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) != len(want) {
		return ErrRepointRaced
	}
	wanted := map[string]bool{}
	for _, s := range want {
		wanted[s] = true
	}
	for _, m := range found {
		if !wanted[m.slug] {
			return ErrRepointRaced
		}
	}

	for _, m := range found {
		if _, err := tx.Exec(`UPDATE storage_resources SET `+column+` = ?, updated_at = now() WHERE id = ?`, to, m.id); err != nil {
			return err
		}
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeUser, ActorID: actorID,
			Action:     "storage_resource.repoint",
			TargetType: "storage_resource", TargetID: m.id,
			Before:     jsonStr(map[string]string{column: from}),
			After:      jsonStr(map[string]string{column: to}),
			RequestID:  requestID, IP: ip, UserAgent: userAgent,
		}); err != nil {
			return err
		}
	}

	oldKey, newKey := kind+":"+from, kind+":"+to
	if _, err := tx.Exec(`UPDATE storage_drives SET key = ? WHERE key = ?
		AND NOT EXISTS (SELECT 1 FROM storage_drives WHERE key = ?)`, newKey, oldKey, newKey); err != nil {
		return err
	}
	return tx.Commit()
}
