package database

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Delegated storage: who may attach a resource to an app without being an
// administrator, and what a revocation of that delegation takes back.
//
// Three tables already answered three questions about a resource — what it is
// (`storage_resources`), where it appears on an app (`storage_mappings`), and
// who may reach a store's bytes from outside (`store_grants`). This is the
// fourth: who may *decide* the second one. See migration 00011 for why it is a
// table of its own rather than a column on either of the others.
//
// The half with the security story is not attaching. It is revoking: a
// delegation that can be taken away has to take the mounts with it, which means
// a mapping records the authority that wrote it and a revocation collects
// exactly those rows. Re-deriving "who could have written this" at revocation
// time would be a query whose answer changes as group membership changes.

// Authority values for storage_mappings.authority.
//
// An administrator's write is `AuthorityAdmin` and adopts what it touches: the
// stamp is the authority of the *last* write, so a mapping an administrator has
// edited never disappears with somebody else's delegation.
const AuthorityAdmin = "admin"

// StorageDelegation is one route by which a caller manages a resource — their
// own row, or one belonging to a group they are in.
type StorageDelegation struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	MaxAccess   string `json:"max_access"`
}

// AuthorityKey is what a mapping written through this delegation is stamped
// with, and what a revocation collects by.
func (d StorageDelegation) AuthorityKey() string {
	return d.SubjectType + ":" + d.SubjectID
}

// AuthorityKey builds the stamp from a subject pair, so the format has one
// writer.
func AuthorityKey(subjectType, subjectID string) string {
	return subjectType + ":" + subjectID
}

// ResolveAttachAuthority picks the one delegation a delegate's attach is
// stamped to.
//
// Pure, and deliberately so: this decides what a *later* revocation removes, so
// the rule has to be testable without a database and has to be the same rule
// every time it is asked. Highest ceiling wins — union-of-max, the same
// resolution `store_grants` already uses, because "the direct one wins" would
// make adding a narrow delegation a silent revocation of a wide one. Ties are
// broken direct-over-group, because a person's own row is the more specific
// statement about them, and then by subject id ascending so two groups at the
// same level resolve to the same one on every call rather than to whichever the
// database returned first.
//
// The second return is false when the caller manages nothing here.
func ResolveAttachAuthority(delegations []StorageDelegation) (StorageDelegation, bool) {
	best := StorageDelegation{}
	found := false
	for _, d := range delegations {
		if storeAccessRank[d.MaxAccess] == 0 {
			// A level on neither ladder is not a narrower delegation, it is a
			// row that means nothing. Ignored rather than ranked at zero, so it
			// can never win by being the only candidate.
			continue
		}
		if !found || betterDelegation(d, best) {
			best, found = d, true
		}
	}
	return best, found
}

// betterDelegation is the ordering above, in the one place it is expressed.
func betterDelegation(a, b StorageDelegation) bool {
	ra, rb := storeAccessRank[a.MaxAccess], storeAccessRank[b.MaxAccess]
	if ra != rb {
		return ra > rb
	}
	if a.SubjectType != b.SubjectType {
		return a.SubjectType == StoreSubjectUser
	}
	return a.SubjectID < b.SubjectID
}

// sortDelegations puts a caller's delegations in the order above, for a listing
// that has to be stable rather than for a decision.
func sortDelegations(ds []StorageDelegation) {
	sort.SliceStable(ds, func(i, j int) bool { return betterDelegation(ds[i], ds[j]) })
}

// StorageManager is one subject's delegation on one resource, as the admin
// surface reads it.
type StorageManager struct {
	ID          string `json:"id"`
	ResourceID  string `json:"resource_id"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	MaxAccess   string `json:"max_access"`
	// Origin is DelegationOriginAdmin or DelegationOriginDefault — written by
	// an administrator on this resource, or from the install's defaults when
	// the resource was created. Provenance, not a kind: it checks the same.
	Origin    string `json:"origin"`
	CreatedBy string `json:"created_by,omitempty"`
	// Denormalized for display, the same reason StoreGrant denormalizes: a
	// list of delegations is unreadable as a column of uuids.
	SubjectName  string `json:"subject_name,omitempty"`
	SubjectEmail string `json:"subject_email,omitempty"`
	// Apps are the apps this delegation's mappings mount, by name.
	//
	// Names rather than a count, and this is the one place in the storage admin
	// surface where that difference is load-bearing: an administrator about to
	// revoke is about to restart these apps, and a number does not let them
	// warn anybody. `mapping_count` on the resource is still a count, because
	// nothing there is about to restart.
	Apps      []StorageManagedApp `json:"apps"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// StorageManagedApp is one app a delegation put a mount on. Enough to name it
// on a confirmation, and enough to restart it afterwards.
type StorageManagedApp struct {
	ID     string `json:"id"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Access string `json:"access"`
}

// AttachableResource is one resource a caller may attach, and their ceiling on
// it — the whole of GET /me/storage.
//
// **No bucket and no prefix.** Those are the containment boundary (ADR §12a)
// and belong to an administrator: a delegate needs to pick a resource and a
// mount path, and neither requires knowing which bucket it is.
type AttachableResource struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	MaxAccess   string `json:"max_access"`
	// Partitioned tells the attach form a folder is required.
	Partitioned bool `json:"partitioned"`
	// Authority is the stamp a mapping written through this delegation carries.
	// Sent so the UI can say which route a person is acting through when they
	// hold more than one, and never accepted back from a client — the server
	// re-resolves it on every write.
	Authority string `json:"authority"`
}

// ErrStorageNotManaged means the caller holds no delegation on the resource, or
// held one that has since been revoked. Distinct so an attach can answer 403
// rather than 500 — and so the race in Q6 has a name.
var ErrStorageNotManaged = errors.New("no delegation on this storage resource")

// ErrStorageCeiling means the level asked for is above the delegation's.
var ErrStorageCeiling = errors.New("above the delegation ceiling")

// --- Reading ---

// storageDelegationQuery is the union-of-routes read: a caller's own row, and
// the row of every group they belong to. Which one an attach is stamped to is
// ResolveAttachAuthority's decision and not this query's — the query returns
// every candidate, deliberately, because the tie-break has to be a pure
// function rather than an ORDER BY nobody can test without a database.
const storageDelegationQuery = `
	SELECT m.subject_type, m.subject_id, m.max_access
	FROM storage_managers m
	WHERE ( (m.subject_type = 'user'  AND m.subject_id = ?)
	     OR (m.subject_type = 'group' AND m.subject_id IN (
	            SELECT group_id FROM group_members WHERE user_id = ?)) )`

// AttachAuthority resolves which delegation, if any, this caller would attach
// this resource through — and at what ceiling.
//
// Read per request and never cached, for the reason store reach is: a revoked
// delegation has to stop working on the next call, not in a while.
func (db *DB) AttachAuthority(userID, resourceID string) (StorageDelegation, bool, error) {
	rows, err := db.conn.Query(storageDelegationQuery+` AND m.resource_id = ?`, userID, userID, resourceID)
	if err != nil {
		return StorageDelegation{}, false, err
	}
	defer rows.Close()

	var candidates []StorageDelegation
	for rows.Next() {
		var d StorageDelegation
		if err := rows.Scan(&d.SubjectType, &d.SubjectID, &d.MaxAccess); err != nil {
			return StorageDelegation{}, false, err
		}
		candidates = append(candidates, d)
	}
	if err := rows.Err(); err != nil {
		return StorageDelegation{}, false, err
	}
	best, ok := ResolveAttachAuthority(candidates)
	return best, ok, nil
}

// ListAttachableStorage answers "which resources may I attach, and at what
// ceiling" — GET /me/storage, and nothing else reads it.
//
// Both kinds, unlike ListReachableStores: a volume has no data plane and so
// takes no grant, but it is very much attachable, and leaving it out here is
// what would make the word "manage" mean two things.
func (db *DB) ListAttachableStorage(userID string) ([]AttachableResource, error) {
	rows, err := db.conn.Query(`
		SELECT r.id, r.slug, r.name, r.kind, r.description, r.partitioned, m.subject_type, m.subject_id, m.max_access
		FROM storage_managers m
		JOIN storage_resources r ON r.id = m.resource_id
		WHERE ( (m.subject_type = 'user'  AND m.subject_id = ?)
		     OR (m.subject_type = 'group' AND m.subject_id IN (
		            SELECT group_id FROM group_members WHERE user_id = ?)) )
		ORDER BY r.name`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Collected per resource rather than resolved in SQL, so the tie-break is
	// the pure function above in every caller that has one.
	order := []string{}
	byID := map[string]*AttachableResource{}
	routes := map[string][]StorageDelegation{}
	for rows.Next() {
		var (
			res AttachableResource
			d   StorageDelegation
		)
		if err := rows.Scan(&res.ID, &res.Slug, &res.Name, &res.Kind, &res.Description, &res.Partitioned,
			&d.SubjectType, &d.SubjectID, &d.MaxAccess); err != nil {
			return nil, err
		}
		if _, seen := byID[res.ID]; !seen {
			order = append(order, res.ID)
			byID[res.ID] = &res
		}
		routes[res.ID] = append(routes[res.ID], d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []AttachableResource{}
	for _, id := range order {
		res := byID[id]
		best, ok := ResolveAttachAuthority(routes[id])
		if !ok {
			continue
		}
		res.MaxAccess = best.MaxAccess
		res.Authority = best.AuthorityKey()
		out = append(out, *res)
	}
	return out, nil
}

// ListStorageManagers answers "who may attach this, and what have they
// attached it to" — the admin panel, and the input to the revoke confirmation.
func (db *DB) ListStorageManagers(resourceID string) ([]StorageManager, error) {
	rows, err := db.conn.Query(`
		SELECT m.id, m.resource_id, m.subject_type, m.subject_id, m.max_access, m.origin,
		       COALESCE(m.created_by,''), m.created_at, m.updated_at,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, gr.name, ''), COALESCE(u.email, '')
		FROM storage_managers m
		LEFT JOIN users  u  ON m.subject_type = 'user'  AND u.id  = m.subject_id
		LEFT JOIN groups gr ON m.subject_type = 'group' AND gr.id = m.subject_id
		WHERE m.resource_id = ?
		ORDER BY m.subject_type, COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, gr.name, '')`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StorageManager{}
	for rows.Next() {
		var m StorageManager
		if err := rows.Scan(&m.ID, &m.ResourceID, &m.SubjectType, &m.SubjectID, &m.MaxAccess, &m.Origin,
			&m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &m.SubjectName, &m.SubjectEmail); err != nil {
			return nil, err
		}
		m.Apps = []StorageManagedApp{}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byAuthority, err := db.managedAppsByAuthority(resourceID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if apps := byAuthority[AuthorityKey(out[i].SubjectType, out[i].SubjectID)]; apps != nil {
			out[i].Apps = apps
		}
	}
	return out, nil
}

// managedAppsByAuthority is what each delegation on this resource has actually
// mounted, keyed by the stamp a revocation would collect by. One query for the
// whole panel rather than one per subject.
func (db *DB) managedAppsByAuthority(resourceID string) (map[string][]StorageManagedApp, error) {
	rows, err := db.conn.Query(`
		SELECT m.authority, apps.id, apps.slug, apps.name, m.path, m.access
		FROM storage_mappings m
		JOIN apps ON apps.id = m.app_id
		WHERE m.resource_id = ? AND m.authority <> 'admin'
		ORDER BY apps.name`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]StorageManagedApp{}
	for rows.Next() {
		var (
			authority string
			app       StorageManagedApp
		)
		if err := rows.Scan(&authority, &app.ID, &app.Slug, &app.Name, &app.Path, &app.Access); err != nil {
			return nil, err
		}
		out[authority] = append(out[authority], app)
	}
	return out, rows.Err()
}

// --- Administration ---

// PutStorageManager creates or moves one subject's delegation on one resource.
//
// The (resource, subject) pair is the identity, which is what makes the PUT
// idempotent; the ceiling is what it carries. A second delegation at another
// level is an edit of the first, never a row beside it — and it has to be, or
// the stamp on a mapping would point at two rows.
func (db *DB) PutStorageManager(m *StorageManager, actorID, requestID, ip, userAgent string) error {
	now := time.Now().UTC()

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Refused here as well as by the trigger, so the API can name the ladder
	// rather than surfacing a plpgsql message.
	var kind string
	err = tx.QueryRow(`SELECT kind FROM storage_resources WHERE id=?`, m.ResourceID).Scan(&kind)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if !ValidStorageAccess(kind, m.MaxAccess) {
		return fmt.Errorf("%w: a %s offers %s", ErrStorageCeiling, kind,
			strings.Join(StorageAccessLevels(kind), ", "))
	}

	var beforeID, beforeAccess string
	err = tx.QueryRow(`SELECT id, max_access FROM storage_managers WHERE resource_id=? AND subject_type=? AND subject_id=?`,
		m.ResourceID, m.SubjectType, m.SubjectID).Scan(&beforeID, &beforeAccess)
	existing := true
	switch {
	case err == sql.ErrNoRows:
		existing = false
	case err != nil:
		return err
	}

	if existing {
		m.ID = beforeID
		if _, err := tx.Exec(`UPDATE storage_managers SET max_access=?, updated_at=? WHERE id=?`,
			m.MaxAccess, now, m.ID); err != nil {
			return err
		}
	} else {
		m.ID = uuid.New().String()
		m.CreatedBy = actorID
		m.CreatedAt = now
		if _, err := tx.Exec(`
			INSERT INTO storage_managers (id, resource_id, subject_type, subject_id, max_access, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.ResourceID, m.SubjectType, m.SubjectID, m.MaxAccess,
			nullIfEmpty(m.CreatedBy), now, now); err != nil {
			return err
		}
	}
	m.UpdatedAt = now

	// Lowering a ceiling does not reach mappings already written above it.
	// Deliberate, and the same rule D12 states for a membership change: the way
	// to remove a live mount is to revoke, which is loud, named and confirmed —
	// not to edit a number and have production apps restart as a side effect.
	var beforeJSON string
	if existing {
		beforeJSON = jsonStr(map[string]string{"max_access": beforeAccess})
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_manager.put",
		TargetType: "storage_resource", TargetID: m.ResourceID,
		Before: beforeJSON,
		After: jsonStr(map[string]string{
			"subject_type": m.SubjectType, "subject_id": m.SubjectID, "max_access": m.MaxAccess}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteStorageManagerAndMappings is the revocation, and it is the reason this
// feature is more than an authorization check.
//
// **In this transaction:** the delegation row, every mapping stamped to that
// subject, and the audit for each — because authorization-relevant mutations
// write the audit in the same transaction, and because the rows *are* the
// authorization. What is deliberately not in here is the restart of the apps
// that were carrying those mounts: that is I/O against a cluster, takes seconds
// per app and can fail, and holding a transaction open across it would make an
// administrator's revocation fail because somebody's app would not come up.
// The affected apps are returned instead, for a caller to restart off the
// request path. **The remount never fails the revoke.**
//
// The delegation row is deleted *before* the mappings are collected, and that
// order closes the race in the other direction: an attach in flight holds this
// row with SELECT … FOR UPDATE, so either it committed before this statement
// and its mapping is in the collection below, or it blocks here and finds the
// delegation gone when it resumes.
func (db *DB) DeleteStorageManagerAndMappings(resourceID, subjectType, subjectID, actorID, requestID, ip, userAgent string) ([]StorageManagedApp, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var maxAccess string
	err = tx.QueryRow(`SELECT max_access FROM storage_managers WHERE resource_id=? AND subject_type=? AND subject_id=? FOR UPDATE`,
		resourceID, subjectType, subjectID).Scan(&maxAccess)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM storage_managers WHERE resource_id=? AND subject_type=? AND subject_id=?`,
		resourceID, subjectType, subjectID); err != nil {
		return nil, err
	}

	authority := AuthorityKey(subjectType, subjectID)
	rows, err := tx.Query(`
		SELECT apps.id, apps.slug, apps.name, m.path, m.access
		FROM storage_mappings m
		JOIN apps ON apps.id = m.app_id
		WHERE m.resource_id = ? AND m.authority = ?
		ORDER BY apps.name`, resourceID, authority)
	if err != nil {
		return nil, err
	}
	affected := []StorageManagedApp{}
	for rows.Next() {
		var app StorageManagedApp
		if err := rows.Scan(&app.ID, &app.Slug, &app.Name, &app.Path, &app.Access); err != nil {
			rows.Close()
			return nil, err
		}
		affected = append(affected, app)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`DELETE FROM storage_mappings WHERE resource_id=? AND authority=?`,
		resourceID, authority); err != nil {
		return nil, err
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_manager.delete",
		TargetType: "storage_resource", TargetID: resourceID,
		Before: jsonStr(map[string]string{
			"subject_type": subjectType, "subject_id": subjectID, "max_access": maxAccess}),
		After:     jsonStr(map[string]int{"mappings_removed": len(affected)}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return nil, err
	}
	// One row per mapping as well as one for the delegation, using the action
	// the mapping delete already has: an app losing a mount is a fact about
	// that app, and an auditor reading one app's history must not have to know
	// that a delegation somewhere else was revoked to find it.
	for _, app := range affected {
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeUser, ActorID: actorID,
			Action:     "storage_mapping.delete",
			TargetType: "app", TargetID: app.ID,
			Before: jsonStr(map[string]string{
				"resource_id": resourceID, "path": app.Path, "access": app.Access,
				"authority": authority}),
			RequestID: requestID, IP: ip, UserAgent: userAgent,
		}); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affected, nil
}

// ErrSubjectHoldsMappings is why a user or group whose delegations stamp live
// mappings is not deleted. Errors of this kind are *SubjectHoldsMappingsError,
// which names the apps.
var ErrSubjectHoldsMappings = errors.New("this subject's delegations hold storage mapped into apps")

// SubjectHoldsMappingsError names every app a subject's delegations mount
// storage into, with the resource each mapping is on, so the refusal can point
// at the revoke that would clear it.
type SubjectHoldsMappingsError struct {
	Mappings []SubjectMapping
}

// SubjectMapping is one mapping stamped to the subject being deleted.
type SubjectMapping struct {
	ResourceID   string `json:"resource_id"`
	ResourceName string `json:"resource_name"`
	AppID        string `json:"app_id"`
	AppSlug      string `json:"app_slug"`
	AppName      string `json:"app_name"`
	Path         string `json:"path"`
}

func (e *SubjectHoldsMappingsError) Error() string { return ErrSubjectHoldsMappings.Error() }
func (e *SubjectHoldsMappingsError) Is(target error) bool {
	return target == ErrSubjectHoldsMappings
}

// DeleteStorageManagersForSubject is the deleted-user and deleted-group path:
// the delegation rows go, on every resource at once — **unless one of them
// stamps a live mapping**, which refuses the delete with
// *SubjectHoldsMappingsError (ADR § Storage somebody else can attach).
//
// The mappings must not outlive the authority that wrote them, and deleting
// them silently is not "unmounted": Builder.Start reads the table, so the mounts
// would vanish at each app's next *start* — a crash, a drain, an idle wake — at
// a random later time, while the running workload kept a mount no row records.
// Restarting the apps instead is a user deletion that takes production down. So
// neither: the administrator revokes first, through the revoke that names and
// restarts the apps, and then deletes.
func (db *DB) DeleteStorageManagersForSubject(tx *Tx, subjectType, subjectID, actorID, requestID, ip, userAgent string) error {
	authority := AuthorityKey(subjectType, subjectID)

	// The delegation rows first, locked. An attach or a deploy flip holds its
	// delegation row FOR UPDATE while it writes a mapping stamped to it; read
	// without the lock, that uncommitted mapping was missed by the check and
	// outlived its authority (security review 2026-11-14, DB-8).
	locks, err := tx.Query(`SELECT id FROM storage_managers WHERE subject_type=? AND subject_id=? FOR UPDATE`,
		subjectType, subjectID)
	if err != nil {
		return err
	}
	for locks.Next() {
	}
	locks.Close()
	if err := locks.Err(); err != nil {
		return err
	}

	held, err := subjectMappingsTx(tx, authority)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		return &SubjectHoldsMappingsError{Mappings: held}
	}
	res, err := tx.Exec(`DELETE FROM storage_managers WHERE subject_type=? AND subject_id=?`,
		subjectType, subjectID)
	if err != nil {
		return err
	}
	delegations, _ := res.RowsAffected()
	if delegations == 0 {
		return nil
	}
	return writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "storage_manager.delete",
		TargetType: subjectType, TargetID: subjectID,
		Before: jsonStr(map[string]any{
			"subject_type": subjectType, "subject_id": subjectID,
			"delegations": delegations}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	})
}

// SubjectMappings lists the mappings stamped to a subject's delegations — what
// would refuse its deletion. The delete dialogs read it before the click.
func (db *DB) SubjectMappings(subjectType, subjectID string) ([]SubjectMapping, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return subjectMappingsTx(tx, AuthorityKey(subjectType, subjectID))
}

func subjectMappingsTx(tx *Tx, authority string) ([]SubjectMapping, error) {
	rows, err := tx.Query(`
		SELECT r.id, r.name, apps.id, apps.slug, apps.name, m.path
		FROM storage_mappings m
		JOIN storage_resources r ON r.id = m.resource_id
		JOIN apps ON apps.id = m.app_id
		WHERE m.authority = ?
		ORDER BY apps.name, r.name, m.path`, authority)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubjectMapping{}
	for rows.Next() {
		var m SubjectMapping
		if err := rows.Scan(&m.ResourceID, &m.ResourceName, &m.AppID, &m.AppSlug, &m.AppName, &m.Path); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
