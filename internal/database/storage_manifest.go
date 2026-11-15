package database

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/runtime"
)

// Manifest storage's reads (ADR § Storage somebody else can attach): toft.toml
// names a resource by name, and the app's *owner* holds the mapping.

// OwnerAttachAuthority is the delegation an app's owner would attach this
// resource through. A user owner resolves as AttachAuthority does — their own
// rows and their groups' — and a **group owner through the group's own row
// only**: a group is not its members, and a mapping held by whichever member
// happened to be asked would move with the membership.
func (db *DB) OwnerAttachAuthority(app *App, resourceID string) (StorageDelegation, bool, error) {
	if app.OwnerGroupID == "" {
		return db.AttachAuthority(app.OwnerID, resourceID)
	}
	var d StorageDelegation
	err := db.conn.QueryRow(`
		SELECT subject_type, subject_id, max_access FROM storage_managers
		WHERE resource_id = ? AND subject_type = 'group' AND subject_id = ?`,
		resourceID, app.OwnerGroupID).Scan(&d.SubjectType, &d.SubjectID, &d.MaxAccess)
	if err == sql.ErrNoRows {
		return StorageDelegation{}, false, nil
	}
	if err != nil {
		return StorageDelegation{}, false, err
	}
	best, ok := ResolveAttachAuthority([]StorageDelegation{d})
	return best, ok, nil
}

// StorageResourceByName is the resource a manifest names, or nil. deleting is
// a name whose resource is gone and whose cluster objects are still being
// removed — a name that cannot be attached and will be free again.
func (db *DB) StorageResourceByName(name string) (res *StorageResource, deleting bool, err error) {
	var id string
	err = db.conn.QueryRow(`SELECT id FROM storage_resources WHERE slug = ?`, name).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		var held bool
		if err := db.conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM storage_deletions WHERE slug = ?)`, name).Scan(&held); err != nil {
			return nil, false, err
		}
		return nil, held, nil
	case err != nil:
		return nil, false, err
	}
	res, err = db.GetStorageResource(id)
	return res, false, err
}

// ManifestMapping is one mapping a deploy's plan writes at its flip.
type ManifestMapping struct {
	ResourceID string
	Folder     string
	Path       string
	Access     string
	// Authority is the owner's delegation the plan resolved, as a stamp.
	Authority string
}

// ErrStorageRevokedDuringDeploy is a delegation a plan relied on that was
// revoked, or lowered below the planned level, before the flip. Errors of
// this kind are *StorageRevokedError, which names the resources.
var ErrStorageRevokedDuringDeploy = errors.New("a storage delegation this deploy relied on was revoked during it")

// StorageRevokedError names what the flip found gone.
type StorageRevokedError struct{ Resources []string }

func (e *StorageRevokedError) Error() string {
	return ErrStorageRevokedDuringDeploy.Error() + ": " + strings.Join(e.Resources, ", ")
}
func (e *StorageRevokedError) Is(target error) bool { return target == ErrStorageRevokedDuringDeploy }

// ErrManifestConflictsConsole is a console mapping attached at a planned
// (resource, folder) since the plan, and differing from it.
var ErrManifestConflictsConsole = errors.New("a mapping attached in the console since this deploy planned differs from toft.toml")

// ReconcileManifestStorage writes a deploy's storage plan at its flip, in one
// transaction (ADR § Storage somebody else can attach).
//
// **The owner's delegations are locked first, and a revocation that raced the
// deploy wins.** Each planned stamp's row is read `FOR UPDATE` — the row a
// revocation deletes first — so either the revocation committed and this finds
// it gone or lowered and refuses, or this commits and the revocation collects
// what it wrote. A warning, as jobs have, would leave a workload holding a mount
// no row records, which no revocation could ever collect.
//
// Then the table is re-diffed against want, because another deploy may have
// flipped since this one planned: manifest rows not wanted are deleted (and
// returned, for the binding sweep), changed ones updated, new ones inserted
// `source = manifest`. A console row at a wanted key is left alone when it is
// identical and refuses the flip when it is not. Each change is audited with
// `source: manifest`, as actorType/actorID — the deploying principal, or the
// app itself when none asked (a removal, which needs nobody).
func (db *DB) ReconcileManifestStorage(appID string, want []ManifestMapping, actorType, actorID string) ([]StorageMapping, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT 1 FROM apps WHERE id = ? FOR NO KEY UPDATE`, appID); err != nil {
		return nil, err
	}

	// The delegation rows are locked in (resource, authority) order, not in
	// toft.toml's: two flips of different apps sharing two delegations would
	// otherwise take them in opposite orders and deadlock (AP-5). The ceilings
	// are then judged in want's order, so a refusal lists what toft.toml did.
	byOrder := slices.Clone(want)
	slices.SortFunc(byOrder, func(a, b ManifestMapping) int {
		return cmp.Or(cmp.Compare(a.ResourceID, b.ResourceID), cmp.Compare(a.Authority, b.Authority))
	})
	type delegationKey struct{ resourceID, authority string }
	ceilings := map[delegationKey]string{}
	for _, w := range byOrder {
		k := delegationKey{w.ResourceID, w.Authority}
		if _, done := ceilings[k]; done || w.Authority == AuthorityAdmin {
			continue
		}
		subjectType, subjectID, ok := strings.Cut(w.Authority, ":")
		var ceiling string
		err := sql.ErrNoRows
		if ok {
			err = tx.QueryRow(`SELECT max_access FROM storage_managers WHERE resource_id=? AND subject_type=? AND subject_id=? FOR UPDATE`,
				w.ResourceID, subjectType, subjectID).Scan(&ceiling)
		}
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		ceilings[k] = ceiling // "" when there is no row: below every level
	}
	var revoked []string
	for _, w := range want {
		if w.Authority == AuthorityAdmin {
			continue
		}
		if ceiling := ceilings[delegationKey{w.ResourceID, w.Authority}]; ceiling == "" || !StoreAccessAtLeast(ceiling, w.Access) {
			var slug string
			_ = tx.QueryRow(`SELECT slug FROM storage_resources WHERE id=?`, w.ResourceID).Scan(&slug)
			revoked = append(revoked, slug)
		}
	}
	if len(revoked) > 0 {
		return nil, &StorageRevokedError{Resources: revoked}
	}

	rows, err := tx.Query(`SELECT id, resource_id, folder, path, access, authority, source
		FROM storage_mappings WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	key := func(resourceID, folder string) string { return resourceID + "\x00" + folder }
	current := map[string]StorageMapping{}
	for rows.Next() {
		var m StorageMapping
		if err := rows.Scan(&m.ID, &m.ResourceID, &m.Folder, &m.Path, &m.Access, &m.Authority, &m.Source); err != nil {
			rows.Close()
			return nil, err
		}
		m.AppID = appID
		current[key(m.ResourceID, m.Folder)] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	audit := func(action string, before, after map[string]string) error {
		e := AuditEntry{ActorType: actorType, ActorID: actorID, Action: action, TargetType: "app", TargetID: appID}
		if before != nil {
			before["source"] = MappingSourceManifest
			e.Before = jsonStr(before)
		}
		if after != nil {
			after["source"] = MappingSourceManifest
			e.After = jsonStr(after)
		}
		return writeAuditTx(tx, e)
	}

	wanted := map[string]bool{}
	for _, w := range want {
		wanted[key(w.ResourceID, w.Folder)] = true
	}
	// Removals first, so a path a removed entry held is free for an entry
	// that takes it in the same deploy.
	removed := []StorageMapping{}
	for k, m := range current {
		if m.Source != MappingSourceManifest || wanted[k] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM storage_mappings WHERE id = ?`, m.ID); err != nil {
			return nil, err
		}
		if err := audit("storage_mapping.delete", map[string]string{"resource_id": m.ResourceID,
			"path": m.Path, "access": m.Access, "folder": m.Folder, "authority": m.Authority}, nil); err != nil {
			return nil, err
		}
		removed = append(removed, m)
	}

	now := time.Now().UTC()
	// A row whose path moves is parked first, at a path nothing else can hold,
	// so the writes below land in any order: UNIQUE (app_id, path) is not
	// deferrable, and an entry taking the path another entry is leaving in
	// the same deploy — or two entries swapping — failed the flip after a
	// whole build, every time (code review 2026-11-11, B-2).
	for _, w := range want {
		m, has := current[key(w.ResourceID, w.Folder)]
		if !has || m.Source != MappingSourceManifest || m.Path == w.Path {
			continue
		}
		if _, err := tx.Exec(`UPDATE storage_mappings SET path=? WHERE id=?`, "/.moving/"+m.ID, m.ID); err != nil {
			return nil, err
		}
	}
	for _, w := range want {
		m, has := current[key(w.ResourceID, w.Folder)]
		after := map[string]string{"resource_id": w.ResourceID, "path": w.Path, "access": w.Access,
			"folder": w.Folder, "authority": w.Authority}
		switch {
		case has && m.Source != MappingSourceManifest:
			if m.Path != w.Path || m.Access != w.Access {
				return nil, ErrManifestConflictsConsole
			}
		case has:
			if m.Path == w.Path && m.Access == w.Access && m.Authority == w.Authority {
				continue
			}
			if _, err := tx.Exec(`UPDATE storage_mappings SET path=?, access=?, authority=?, updated_at=? WHERE id=?`,
				w.Path, w.Access, w.Authority, now, m.ID); err != nil {
				return nil, err
			}
			if err := audit("storage_mapping.update", map[string]string{"path": m.Path, "access": m.Access,
				"folder": m.Folder, "authority": m.Authority}, after); err != nil {
				return nil, err
			}
		default:
			if _, err := tx.Exec(`
				INSERT INTO storage_mappings (id, resource_id, app_id, path, access, folder, authority, source, created_by, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, 'manifest', ?, ?, ?)`,
				uuid.New().String(), w.ResourceID, appID, w.Path, w.Access, w.Folder, w.Authority,
				nullIfEmpty(personOrEmpty(actorType, actorID)), now, now); err != nil {
				return nil, err
			}
			if err := audit("storage_mapping.create", nil, after); err != nil {
				return nil, err
			}
		}
	}

	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM storage_mappings WHERE app_id = ?`, appID).Scan(&count); err != nil {
		return nil, err
	}
	if count > MaxAppStorageMappings {
		return nil, ErrAppMappingCap
	}
	return removed, tx.Commit()
}

// personOrEmpty is created_by for a manifest row: the person who deployed, or
// nobody — the column is a foreign key to users.
func personOrEmpty(actorType, actorID string) string {
	if actorType == PrincipalTypeUser {
		return actorID
	}
	return ""
}

// EnsureFolderGate numbers a partitioned host-root folder's gate before a
// release that mounts it starts — the deploy's start comes before its flip,
// where the mapping is written. Idempotent, and a number is never reused, so a
// deploy that fails after this leaves a gate nothing holds and nothing else.
func (db *DB) EnsureFolderGate(resourceID, folder string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slug, root string
	var partitioned bool
	if err := tx.QueryRow(`SELECT slug, storage_root, partitioned FROM storage_resources WHERE id = ?`,
		resourceID).Scan(&slug, &root, &partitioned); err != nil {
		return err
	}
	if root == "" || !partitioned {
		return nil
	}
	if _, err := db.ensureGateTx(tx, runtime.FolderGateName(slug, folder), resourceID, folder); err != nil {
		return err
	}
	return tx.Commit()
}

// PlannedMounts is what a release mounts when its plan is carried out: the
// app's console mappings as the table has them, and want — the plan's keep,
// add and change — built the way AppMounts builds a row.
func (db *DB) PlannedMounts(appID string, want []ManifestMapping) ([]runtime.Mount, error) {
	mappings, err := db.ListStorageMappingsForApp(appID)
	if err != nil {
		return nil, err
	}
	gates, err := db.appGates(appID)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Mount, 0, len(mappings)+len(want))
	for _, m := range mappings {
		if m.Source == MappingSourceManifest {
			continue
		}
		out = append(out, mountOf(m, gates))
	}
	for _, w := range want {
		res, err := db.GetStorageResource(w.ResourceID)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, fmt.Errorf("storage resource %s is gone", w.ResourceID)
		}
		m := StorageMapping{ResourceID: res.ID, Path: w.Path, Access: w.Access, Folder: w.Folder,
			ResourceSlug: res.Slug, ResourceKind: res.Kind, ResourceRoot: res.Root, ResourcePartitioned: res.Partitioned}
		g := map[string]runtime.Gate{}
		if gid, name, ok, err := db.liveGate(res.ID, gateFolder(m)); err != nil {
			return nil, err
		} else if ok {
			g[gateKey(res.ID, gateFolder(m))] = runtime.Gate{Group: name, GID: gid}
		}
		out = append(out, mountOf(m, g))
	}
	return out, nil
}

func (db *DB) liveGate(resourceID, folder string) (int, string, bool, error) {
	var gid int
	var name string
	err := db.conn.QueryRow(`SELECT gid, group_name FROM storage_gates
		WHERE resource_id = ? AND folder = ? AND retired_at IS NULL`, resourceID, folder).Scan(&gid, &name)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	return gid, name, err == nil, err
}

// ErrStorageNotAttachableByNewOwner refuses a transfer to an owner who cannot
// attach storage the app's toft.toml declares. Errors of this kind are
// *StorageNotAttachableByNewOwnerError, which names each resource and level.
var ErrStorageNotAttachableByNewOwner = errors.New("the new owner cannot attach storage this app's toft.toml declares")

// StorageNotAttachableByNewOwnerError names what the new owner lacks.
type StorageNotAttachableByNewOwnerError struct {
	Missing []StorageNeed
}

// StorageNeed is one resource at one level.
type StorageNeed struct {
	Resource string `json:"resource"`
	Folder   string `json:"folder,omitempty"`
	Access   string `json:"access"`
}

func (e *StorageNotAttachableByNewOwnerError) Error() string {
	return ErrStorageNotAttachableByNewOwner.Error()
}
func (e *StorageNotAttachableByNewOwnerError) Is(target error) bool {
	return target == ErrStorageNotAttachableByNewOwner
}

// restampManifestStorageTx moves each manifest mapping's stamp to the new
// owner's delegation, inside the transfer's transaction. A user owner resolves
// through their own rows and their groups', a group through its own row only —
// OwnerAttachAuthority's rule, asked of the transaction.
func restampManifestStorageTx(tx *Tx, appID, ownerType, ownerID, actorID string, meta RequestMeta, opts TransferOptions) error {
	rows, err := tx.Query(`
		SELECT m.id, m.resource_id, r.slug, m.folder, m.access, m.authority
		FROM storage_mappings m JOIN storage_resources r ON r.id = m.resource_id
		WHERE m.app_id = ? AND m.source = 'manifest' ORDER BY r.slug, m.folder`, appID)
	if err != nil {
		return err
	}
	type row struct{ id, resourceID, slug, folder, access, authority string }
	var ms []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.resourceID, &r.slug, &r.folder, &r.access, &r.authority); err != nil {
			rows.Close()
			return err
		}
		ms = append(ms, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var missing []StorageNeed
	for _, m := range ms {
		var q string
		var args []any
		if ownerType == PrincipalTypeGroup {
			q = `SELECT subject_type, subject_id, max_access FROM storage_managers
				WHERE resource_id = ? AND subject_type = 'group' AND subject_id = ? FOR UPDATE`
			args = []any{m.resourceID, ownerID}
		} else {
			q = storageDelegationQuery + ` AND m.resource_id = ? FOR UPDATE`
			args = []any{ownerID, ownerID, m.resourceID}
		}
		drows, err := tx.Query(q, args...)
		if err != nil {
			return err
		}
		var candidates []StorageDelegation
		for drows.Next() {
			var d StorageDelegation
			if err := drows.Scan(&d.SubjectType, &d.SubjectID, &d.MaxAccess); err != nil {
				drows.Close()
				return err
			}
			candidates = append(candidates, d)
		}
		drows.Close()
		stamp := ""
		if best, ok := ResolveAttachAuthority(candidates); ok && StoreAccessAtLeast(best.MaxAccess, m.access) {
			stamp = best.AuthorityKey()
		} else if opts.KeepStorageAsAdmin {
			stamp = AuthorityAdmin
		} else {
			missing = append(missing, StorageNeed{Resource: m.slug, Folder: m.folder, Access: m.access})
			continue
		}
		if stamp == m.authority {
			continue
		}
		if _, err := tx.Exec(`UPDATE storage_mappings SET authority = ?, updated_at = now() WHERE id = ?`, stamp, m.id); err != nil {
			return err
		}
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeUser, ActorID: actorID,
			Action: "storage_mapping.update", TargetType: "app", TargetID: appID,
			Before: jsonStr(map[string]string{"resource_id": m.resourceID, "folder": m.folder,
				"authority": m.authority, "source": MappingSourceManifest}),
			After: jsonStr(map[string]string{"resource_id": m.resourceID, "folder": m.folder,
				"authority": stamp, "source": MappingSourceManifest, "reason": "transfer"}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
		}); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return &StorageNotAttachableByNewOwnerError{Missing: missing}
	}
	return nil
}

// ManifestMappingsOn reports which of an app's mappings of a resource were
// declared in toft.toml — at one folder, or at any when folder is nil. The
// console asks before it writes or detaches one.
func (db *DB) ManifestMappingsOn(resourceID, appID string, folder *string) (int, error) {
	q := `SELECT count(*) FROM storage_mappings WHERE resource_id = ? AND app_id = ? AND source = 'manifest'`
	args := []any{resourceID, appID}
	if folder != nil {
		q += ` AND folder = ?`
		args = append(args, *folder)
	}
	var n int
	err := db.conn.QueryRow(q, args...).Scan(&n)
	return n, err
}
