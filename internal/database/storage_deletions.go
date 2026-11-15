package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Deleting a storage resource's data (ADR §12a). Two tables: storage_claims,
// every volume claim a resource was ever given and every folder binding of a
// store, recorded before the claim exists; and storage_deletions, a delete the cluster has not finished, which
// holds the slug until it has.

// ErrStorageDeleting refuses a create naming a slug whose previous resource's
// volumes are still being deleted. A slug used again provisions onto the same
// directory, so a create inside that window would have its files removed by
// its predecessor's late delete.
var ErrStorageDeleting = errors.New("a storage resource of that name is still being deleted")

// ErrStorageDirectoryKept refuses a create naming a slug whose previous
// resource was a cluster volume whose files were kept. The chart's EFS class
// roots every access point at /lpv-<slug>, so the new volume would be handed
// the old one's files; the name stays held until an administrator releases it
// (ReleaseKeptStorageName).
var ErrStorageDirectoryKept = errors.New("a deleted storage resource of that name left its files behind")

// StorageDeletion is one delete in flight.
type StorageDeletion struct {
	Slug         string
	ResourceID   string
	Kind         string
	Backing      string
	StorageClass string
	Root         string
	Claims       []string
	Volumes      []string
	// Report is the destroy's report as sent, and nil until the destroy has
	// been dispatched — which is how the reconciler tells a delete cut short
	// before it reached the cluster from one waiting on it.
	Report json.RawMessage
	// PredatesClaims is a resource created before storage_claims existed,
	// whose folders released back then nothing names.
	PredatesClaims bool
	ActorID        string
	StartedAt      time.Time
	// Kept is a delete that has finished and left a cluster volume's directory
	// behind. The row stays only to hold the name; nothing is left to do.
	Kept bool
}

// RecordStorageClaim notes that a volume claim of this name is about to exist
// for a resource. Written **before** the claim is created: a claim the cluster
// made and nothing recorded is exactly what the resource delete could not find.
func (db *DB) RecordStorageClaim(resourceID, claim, folder string) error {
	_, err := db.conn.Exec(`
		INSERT INTO storage_claims (resource_id, claim, folder) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, resourceID, claim, folder)
	return err
}

// ListStorageClaims is a resource's recorded claims, in a stable order.
func (db *DB) ListStorageClaims(resourceID string) ([]string, error) {
	rows, err := db.conn.Query(`SELECT claim FROM storage_claims WHERE resource_id = ? ORDER BY claim`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListStorageDeletions is every delete still in flight, oldest first.
func (db *DB) ListStorageDeletions() ([]StorageDeletion, error) {
	rows, err := db.conn.Query(`
		SELECT slug, resource_id, kind, backing, storage_class, storage_root, claims, volumes,
		       report, predates_claims, COALESCE(actor_id, ''), started_at, kept
		FROM storage_deletions ORDER BY started_at, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StorageDeletion{}
	for rows.Next() {
		var d StorageDeletion
		var claims, volumes []byte
		var report sql.NullString
		if err := rows.Scan(&d.Slug, &d.ResourceID, &d.Kind, &d.Backing, &d.StorageClass, &d.Root,
			&claims, &volumes, &report, &d.PredatesClaims, &d.ActorID, &d.StartedAt, &d.Kept); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(claims, &d.Claims); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(volumes, &d.Volumes); err != nil {
			return nil, err
		}
		if report.Valid {
			d.Report = json.RawMessage(report.String)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetStorageDeletionReport records that the destroy has been dispatched: the
// volumes it flipped, which must be gone before the slug is free, and the
// report it made — on the tombstone, and as storage_resource.destroy_started on
// the Activity page, because the click and the data actually going can be an
// hour apart and what the cluster was told is the half an operator asks about.
// A dispatch the reconciler repeats is a second row: each is a time the
// cluster was told.
//
// **The volumes are a union with what the row already holds**, and the merged
// list is returned (AP-4). A second pass of a destroy finds a claim the first
// released as NotFound and cannot name the volume behind it, so a replace
// would forget a volume still being deleted and free the slug over it. The
// caller decides "nothing pending" from the merged list. A tombstone already
// gone returns nil volumes and no error.
func (db *DB) SetStorageDeletionReport(slug string, volumes []string, report any) ([]string, error) {
	v, err := volumesJSON(volumes)
	if err != nil {
		return nil, err
	}
	r, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var resourceID, actorID string
	var merged []byte
	err = tx.QueryRow(`
		UPDATE storage_deletions SET volumes = `+unionVolumes+`, report = ?::jsonb WHERE slug = ?
		RETURNING resource_id, COALESCE(actor_id, ''), volumes`, v, string(r), slug).Scan(&resourceID, &actorID, &merged)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []string
	if err := json.Unmarshal(merged, &all); err != nil {
		return nil, err
	}
	after, err := json.Marshal(map[string]any{"slug": slug, "volumes": all, "report": json.RawMessage(r)})
	if err != nil {
		return nil, err
	}
	if err := writeAuditTx(tx, storageDeletionAudit("storage_resource.destroy_started", resourceID, actorID, string(after))); err != nil {
		return nil, err
	}
	return all, tx.Commit()
}

// AddStorageDeletionVolumes records volumes a destroy flipped before it failed,
// leaving the report NULL so the reconciler dispatches it again. Without it
// the volumes a cut-short pass flipped were known only to that pass, and the
// next one — finding their claims released — could not name them (AP-4).
func (db *DB) AddStorageDeletionVolumes(slug string, volumes []string) error {
	if len(volumes) == 0 {
		return nil
	}
	v, err := volumesJSON(volumes)
	if err != nil {
		return err
	}
	_, err = db.conn.Exec(`UPDATE storage_deletions SET volumes = `+unionVolumes+` WHERE slug = ?`, v, slug)
	return err
}

// unionVolumes is the row's volumes merged with a JSON array parameter,
// distinct and ordered, never NULL.
const unionVolumes = `COALESCE((SELECT jsonb_agg(x.v ORDER BY x.v) FROM (
			SELECT jsonb_array_elements_text(volumes) AS v
			UNION SELECT jsonb_array_elements_text(?::jsonb)) x), '[]'::jsonb)`

func volumesJSON(volumes []string) (string, error) {
	if volumes == nil {
		volumes = []string{}
	}
	v, err := json.Marshal(volumes)
	return string(v), err
}

// storageDeletionAudit is a row about a delete in flight, attributed to the
// administrator who clicked it — the reconciler acts on their behalf — and to
// the system only when nobody is recorded.
func storageDeletionAudit(action, resourceID, actorID, after string) AuditEntry {
	e := AuditEntry{
		ActorType: PrincipalTypeSystem, Action: action,
		TargetType: "storage_resource", TargetID: resourceID, After: after,
	}
	if actorID != "" {
		e.ActorType, e.ActorID = PrincipalTypeUser, actorID
	}
	return e
}

// FinishStorageDeletion frees the slug and records that the delete is done,
// with what it removed and what it left. One audit row, written once: a second
// call finds no row, or a kept one, and writes nothing.
//
// **A cluster volume whose files were kept does not free its slug.** Its
// directory is /lpv-<slug> on the chart's EFS class, and the next volume of
// that name provisions an access point onto it — the old files, handed to
// whoever the new resource is for. The row stays, marked kept, and the create
// refuses the name with storage_directory_kept until an administrator
// releases it. A host root needs none of this: its create refuses an existing
// directory by looking.
func (db *DB) FinishStorageDeletion(slug string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var resourceID, actorID, kind, root string
	var report sql.NullString
	var kept bool
	err = tx.QueryRow(`
		SELECT resource_id, COALESCE(actor_id, ''), kind, storage_root, report, kept
		FROM storage_deletions WHERE slug = ? FOR UPDATE`, slug).Scan(&resourceID, &actorID, &kind, &root, &report, &kept)
	if errors.Is(err, sql.ErrNoRows) || kept {
		return nil
	}
	if err != nil {
		return err
	}
	leftBehind := keptDirectories(kind, root, report)
	if len(leftBehind) > 0 {
		_, err = tx.Exec(`UPDATE storage_deletions SET kept = true WHERE slug = ?`, slug)
	} else {
		_, err = tx.Exec(`DELETE FROM storage_deletions WHERE slug = ?`, slug)
	}
	if err != nil {
		return err
	}
	finished := map[string]any{"slug": slug}
	if report.Valid {
		finished["report"] = json.RawMessage(report.String)
	}
	if len(leftBehind) > 0 {
		finished["name_held"] = true
	}
	after, err := json.Marshal(finished)
	if err != nil {
		return err
	}
	if err := writeAuditTx(tx, storageDeletionAudit("storage_resource.destroyed", resourceID, actorID, string(after))); err != nil {
		return err
	}
	return tx.Commit()
}

// keptDirectories is what a finished delete left on the file system that a
// resource of the same name would be handed: anything the destroy reported
// left behind, for a cluster volume. Stores and host roots return nil — a
// store's bucket is named by its operator, and a host root's create looks
// before it takes a directory.
func keptDirectories(kind, root string, report sql.NullString) []string {
	if kind != StorageKindVolume || root != "" || !report.Valid {
		return nil
	}
	var r struct {
		LeftBehind []struct {
			What string `json:"what"`
		} `json:"left_behind"`
	}
	if json.Unmarshal([]byte(report.String), &r) != nil {
		return nil
	}
	out := make([]string, 0, len(r.LeftBehind))
	for _, l := range r.LeftBehind {
		out = append(out, l.What)
	}
	return out
}

// ReleaseKeptStorageName frees a slug a finished delete held because its
// files were kept, once an administrator has dealt with the directory (or
// decided the next resource of that name may have it). Only a kept row is
// released: a delete still in flight is the reconciler's to finish. Returns
// sql.ErrNoRows when no kept row holds the slug.
func (db *DB) ReleaseKeptStorageName(slug, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var resourceID string
	var report sql.NullString
	err = tx.QueryRow(`
		DELETE FROM storage_deletions WHERE slug = ? AND kept
		RETURNING resource_id, report`, slug).Scan(&resourceID, &report)
	if err != nil {
		return err
	}
	before := map[string]any{"slug": slug}
	if report.Valid {
		before["report"] = json.RawMessage(report.String)
	}
	b, err := json.Marshal(before)
	if err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_resource.name_released",
		TargetType: "storage_resource", TargetID: resourceID,
		Before:    string(b),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// StorageNameKept reports whether a finished delete holds the slug because
// its files were kept, and the storage class the volume was on ("" for the
// install's default).
func (db *DB) StorageNameKept(slug string) (kept bool, class string, err error) {
	err = db.conn.QueryRow(`SELECT kept, storage_class FROM storage_deletions WHERE slug = ?`, slug).Scan(&kept, &class)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return kept, class, err
}
