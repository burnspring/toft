package database

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Store grants: who may reach a store from outside the platform.
//
// `storage_mappings` answers "where does this resource appear on that app's
// filesystem". This answers "who may reach this from outside", and the two are
// deliberately different relations — a grant has no path, no ordering against a
// deploy, and a different lifetime. See migration 00007 for the full argument.
//
// Every mutation writes audit_log in the same transaction, as every
// authorization-relevant mutation on this platform does.
//
// **There is no administrator bypass.** An administrator with no grant reaches
// no store, and gets the same 404 as anybody else. Granting themselves is one
// audited call, which is strictly better than an implicit reach that no
// audit_log row explains — and it keeps the ladder expressed in exactly one
// place, with no second path around it.

// Subject types. A group is a set of humans and never an actor, so a grant to
// one is only ever resolved through its members.
const (
	StoreSubjectUser  = "user"
	StoreSubjectGroup = "group"
)

// ErrStoreGrantNoBucket means a grant was aimed at a resource with no objects
// behind it. Distinct so the API can answer 422 naming what the resource is
// rather than a 500.
//
// It was ErrStoreGrantNoBucket until migration 00018, and the rename is the point:
// the test is no longer "is this a store" but "does this name a bucket", and an
// error whose name asserts the old test is how the next reader comes to trust
// the wrong rule.
var ErrStoreGrantNoBucket = errors.New("only a resource with objects behind it takes grants")

// StoreGrant is one subject's reach into one store.
type StoreGrant struct {
	ID          string `json:"id"`
	ResourceID  string `json:"resource_id"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Access      string `json:"access"`
	// Folder narrows the grant to one directory of the store, and empty is the
	// store's root — which is what every grant written before migration 00017
	// means. `omitempty`, so an install that names no folder has the wire shape
	// it has always had.
	Folder    string `json:"folder,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	// Denormalized for display, the same reason StorageMapping denormalizes:
	// a grants list is unreadable as a column of uuids.
	SubjectName  string    `json:"subject_name,omitempty"`
	SubjectEmail string    `json:"subject_email,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// StoreScope is one folder of a store and the level a caller holds on it.
// An empty Folder is the store's root, and therefore the whole store.
type StoreScope struct {
	Folder string `json:"folder,omitempty"`
	Access string `json:"access"`
}

// StoreScopes is one store a caller can reach, and every scope they hold on it.
//
// The bucket and prefix are here because the signer needs them and the caller
// never sees them: the API's response carries slug, name and the scopes only. A
// person who may upload has no business knowing which bucket the platform
// signs against.
//
// **A level is a property of the key, not of the store.** Before folders there
// was one level per store and every route resolved it before it knew what
// object was being asked for. Now the scopes come back together and the level
// is resolved against the thing being addressed, by LevelForKey or
// LevelForPath — the one place the covering test lives.
type StoreScopes struct {
	Resource StorageResource
	Scopes   []StoreScope
}

// LevelForKey is the level this caller holds on one object key, relative to the
// store's own prefix.
//
// **Union-of-max over covering folders, never most-specific-wins.** The reason
// union-of-max exists at all is that adding a narrow grant must never be a
// silent revocation of a wide one, and most-specific-wins is that same failure
// with a folder standing where the subject used to. So somebody holding `full`
// at the root does not lose delete on `reports/` by also being granted `read`
// on it. The consequence, stated rather than discovered: **a folder grant adds
// reach and never removes any**, and narrowing somebody means editing their
// wide grant.
func (s StoreScopes) LevelForKey(name string) string {
	level, _ := s.ScopeForKey(name)
	return level
}

// ScopeForKey is LevelForKey plus the folder that granted the level — the
// deepest of those covering the key at the winning level, because it is the
// narrowest containment that still authorizes the request. The host data plane
// opens its os.Root there (internal/hoststore), so a symlink an app planted
// inside a granted folder cannot reach a sibling the grant does not cover.
func (s StoreScopes) ScopeForKey(name string) (level, folder string) {
	return s.bestScope(func(f string) bool { return coversKey(f, name) })
}

// ScopeForPath is ScopeForKey for a folder path. See LevelForPath.
func (s StoreScopes) ScopeForPath(path string) (level, folder string) {
	path = strings.TrimSuffix(path, "/")
	return s.bestScope(func(f string) bool { return coversPath(f, path) })
}

func (s StoreScopes) bestScope(covers func(folder string) bool) (string, string) {
	best, folder := 0, ""
	for _, sc := range s.Scopes {
		if !covers(sc.Folder) {
			continue
		}
		r := storeAccessRank[sc.Access]
		if r > best || (r == best && len(sc.Folder) > len(folder)) {
			best, folder = r, sc.Folder
		}
	}
	return storeAccessForRank(best), folder
}

// LevelForPath is the same question about a folder path — what a listing takes.
//
// It differs from LevelForKey in exactly one case and the difference is
// load-bearing: a grant on `analytics` covers the *path* `analytics`, because
// that is the folder itself, and does not cover the *key* `analytics`, because
// an object of that name sits beside the folder rather than inside it. Folding
// the two together would hand somebody granted a folder an object they were
// never granted, on the strength of a name collision.
func (s StoreScopes) LevelForPath(path string) string {
	level, _ := s.ScopeForPath(path)
	return level
}

// Top is the highest level held anywhere in this store, which is what a
// listing of stores shows and what a caller's affordances are drawn from at the
// scope they are looking at. It authorizes nothing on its own: every route
// resolves against the key or the path it was given.
func (s StoreScopes) Top() string {
	best := 0
	for _, sc := range s.Scopes {
		if r := storeAccessRank[sc.Access]; r > best {
			best = r
		}
	}
	return storeAccessForRank(best)
}

// GrantedFolders is the minimal set of folders this caller reaches, and it is
// what the store root shows somebody who does not reach the root.
//
// Minimal: a folder covered by another granted folder is not in the set, so
// grants on `a` and `a/b` list `a` alone. Sorted, so the page does not reorder
// itself between requests. Empty when the caller reaches the root, because then
// the real listing is the answer and there is nothing to synthesize.
func (s StoreScopes) GrantedFolders() []string {
	for _, sc := range s.Scopes {
		if sc.Folder == "" {
			return nil
		}
	}
	out := []string{}
	for _, sc := range s.Scopes {
		covered := false
		for _, other := range s.Scopes {
			if other.Folder != sc.Folder && coversPath(other.Folder, sc.Folder) {
				covered = true
				break
			}
		}
		if !covered && !slices.Contains(out, sc.Folder) {
			out = append(out, sc.Folder)
		}
	}
	slices.Sort(out)
	return out
}

// coversKey reports whether a grant on folder reaches the object key name.
//
// The empty folder is the store root and reaches everything. Otherwise the key
// has to be *inside* the folder, which is a match on the folder plus a
// separator — never a bare string prefix, or a grant on `report` would reach
// every key under `reports/`, which is a containment breach that looks like a
// typo. It is the same reasoning, and the same trailing slash, that
// kube.StoreMountOptions applies to a mount's prefix.
func coversKey(folder, name string) bool {
	return folder == "" || strings.HasPrefix(name, folder+"/")
}

// CoversKey is coversKey for the audit that names the granting folder, so the
// row is computed by the match that authorized the mint and not by a second
// spelling of it.
func CoversKey(folder, name string) bool { return coversKey(folder, name) }

// coversPath is coversKey for a folder path, where the folder also covers
// itself. See LevelForPath.
func coversPath(folder, path string) bool {
	return folder == "" || path == folder || strings.HasPrefix(path, folder+"/")
}

// storeAccessRank orders the ladder so "the highest of several grants" is a
// MAX() rather than a hand-written comparison in three places.
//
// Kept beside the SQL that mirrors it: the CASE below and this map are the same
// ladder, and a rung added to one without the other is the bug this ordering
// exists to make impossible to write accidentally. Two more copies must agree:
// the authority trigger's CASEs (migration 00074) and the CLI's accessRank in
// cli/store_tree.go, a separate module that cannot import this one.
var storeAccessRank = map[string]int{
	"read": 1, "write": 2, "full": 3,
}

// storeAccessCase spells the ladder in SQL, for whichever column carries it.
// Two tables now hold a level on this ladder — a grant's `access` and a
// delegation's `max_access` — and they must rank identically or the union below
// would resolve to whichever arm happened to spell it more generously.
func storeAccessCase(col string) string {
	return `CASE ` + col + ` WHEN 'read' THEN 1 WHEN 'write' THEN 2 ` +
		`WHEN 'full' THEN 3 ELSE 0 END`
}

// StoreAccessAtLeast reports whether have is the same rung as want or higher.
// One definition, so a route never spells the comparison out for itself.
func StoreAccessAtLeast(have, want string) bool {
	return storeAccessRank[have] >= storeAccessRank[want] && storeAccessRank[want] > 0
}

func storeAccessForRank(rank int) string {
	for level, r := range storeAccessRank {
		if r == rank {
			return level
		}
	}
	return ""
}

// --- Resolution ---

// The union-of-max resolver. A person's level on a store is the highest of
// their direct grant, the grants of every group they belong to, and — since
// managing a store implies its data plane — the same over their delegations.
//
// **Managing a store confers its data plane through this query and never
// through a second row.** Writing a shadow `store_grants` row when a delegation
// is created would be two facts for one decision, and the revoke path would
// then have to know that some grant rows are shadows that must die with their
// delegation. That is the nullable-column table migration 00007 already refuses,
// arriving by another door. So it is a `UNION ALL` arm here: the same MAX over
// the same ladder, the same per-request read, the same absence of a cache, and
// a revocation lands on the next mint precisely as a grant revocation does.
//
// **On a resource with no data plane it confers nothing**, because there is
// nothing to confer — no presigned URL, no browser page, no CLI. The final
// filter — a bucket, or a volume under a host root (HasDataPlane) — is where
// that happens: managing a store, an S3 Files volume or a host-root volume
// confers its data plane at the delegation's ceiling, and managing an
// EFS-backed volume confers nothing but attaching.
//
// Read per request and never cached: revoking has to take effect on the next
// mint, and a cache with a TTL would make "revoked" mean "revoked in a while"
// for a credential that mints URLs.
//
// **A delegation's arm carries the empty folder**, which is the store root and
// therefore the whole store. That is not a default standing in for a missing
// column: managing a store confers its data plane on the resource, because a
// delegation is on the resource, and `storage_managers` gains no folder here
// (a narrow delegation is a revocation question, not a resolution one).
//
// The MAX is now per **(resource, folder)** rather than per resource, so what
// comes back is a caller's scopes and the covering test happens in Go, against
// the key or path the route was actually given.
var storeReachQuery = `
	SELECT r.id, r.slug, r.name, r.kind, r.backing, r.storage_class, r.storage_root, r.partitioned, r.bucket, r.prefix, r.claim_name, r.description,
	       COALESCE(r.created_by,''), r.created_at, r.updated_at,
	       reach.folder AS folder, MAX(reach.rank) AS rank
	FROM (
	        SELECT g.resource_id AS resource_id, g.folder AS folder,
	               ` + storeAccessCase("g.access") + ` AS rank
	        FROM store_grants g
	        WHERE ( (g.subject_type = 'user'  AND g.subject_id = ?)
	             OR (g.subject_type = 'group' AND g.subject_id IN (
	                    SELECT group_id FROM group_members WHERE user_id = ?)) )
	        UNION ALL
	        SELECT m.resource_id, '', ` + storeAccessCase("m.max_access") + `
	        FROM storage_managers m
	        WHERE ( (m.subject_type = 'user'  AND m.subject_id = ?)
	             OR (m.subject_type = 'group' AND m.subject_id IN (
	                    SELECT group_id FROM group_members WHERE user_id = ?)) )
	     ) reach
	JOIN storage_resources r ON r.id = reach.resource_id
	-- The bucket is the test, not the kind, and a host root is the second
	-- one: a resource with objects behind it, or a directory on the
	-- platform's own host, has a data plane and can be reached from outside;
	-- one with neither has nothing to sign for. See
	-- StorageResource.HasDataPlane, which is this sentence in Go.
	-- Parenthesised as a whole, because both callers append "AND …" to this
	-- WHERE: unparenthesised, "a OR b AND r.slug = ?" binds the slug to the
	-- second half only, and every bucket resource answers for any slug.
	WHERE (r.bucket <> '' OR (r.backing = 'host' AND r.storage_root <> ''))`

// storeReachGroupBy closes the query above. Every non-aggregated column, and
// the folder with them — spelled once because the two callers differ only in their
// filter and their ordering.
const storeReachGroupBy = `
		GROUP BY r.id, r.slug, r.name, r.kind, r.backing, r.storage_class, r.storage_root,
		         r.partitioned, r.bucket, r.prefix, r.claim_name, r.description, r.created_by, r.created_at,
		         r.updated_at, reach.folder`

// ListReachableStores answers "which stores can this caller reach, and at what
// scopes" — the self-scoped listing behind GET /store.
//
// One entry per store, carrying every scope. A store reached at two folders is
// one entry with two scopes rather than two entries: the resource is one thing,
// and the page decides how many rows to draw from it.
func (db *DB) ListReachableStores(userID string) ([]StoreScopes, error) {
	rows, err := db.conn.Query(storeReachQuery+storeReachGroupBy+`
		ORDER BY r.name, folder`, userID, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectStoreScopes(rows)
}

// StoreScopesBySlug resolves one store for one caller, or nil when they hold no
// scope on it at all — which the API turns into the same 404 as a slug that
// does not exist. A caller learns nothing about a store they cannot reach.
func (db *DB) StoreScopesBySlug(userID, slug string) (*StoreScopes, error) {
	rows, err := db.conn.Query(storeReachQuery+`
		  AND r.slug = ?`+storeReachGroupBy+`
		ORDER BY folder`, userID, userID, userID, userID, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out, err := collectStoreScopes(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// collectStoreScopes folds the query's one-row-per-scope answer into one entry
// per store. The rows arrive grouped by resource because they are ordered by
// it, but the fold keys on the id rather than trusting that — an ORDER BY is a
// presentation choice and this is a correctness one.
func collectStoreScopes(rows *sql.Rows) ([]StoreScopes, error) {
	out := []StoreScopes{}
	index := map[string]int{}
	for rows.Next() {
		var (
			r      StorageResource
			folder string
			rank   int
		)
		if err := rows.Scan(&r.ID, &r.Slug, &r.Name, &r.Kind, &r.Backing, &r.StorageClass,
			&r.Root, &r.Partitioned, &r.Bucket, &r.Prefix, &r.ClaimName, &r.Description, &r.CreatedBy,
			&r.CreatedAt, &r.UpdatedAt, &folder, &rank); err != nil {
			return nil, err
		}
		scope := StoreScope{Folder: folder, Access: storeAccessForRank(rank)}
		if i, ok := index[r.ID]; ok {
			out[i].Scopes = append(out[i].Scopes, scope)
			continue
		}
		index[r.ID] = len(out)
		out = append(out, StoreScopes{Resource: r, Scopes: []StoreScope{scope}})
	}
	return out, rows.Err()
}

// --- Administration ---

// ListStoreGrants answers "who may reach this store", for the admin panel.
//
// LEFT JOIN on both subject tables rather than two queries: a grant is either a
// user's or a group's, and the row that names which is the row that has to
// carry the name.
func (db *DB) ListStoreGrants(resourceID string) ([]StoreGrant, error) {
	rows, err := db.conn.Query(`
		SELECT g.id, g.resource_id, g.subject_type, g.subject_id, g.access, g.folder,
		       COALESCE(g.created_by,''), g.created_at, g.updated_at,
		       COALESCE(u.name, u.email, gr.name, ''), COALESCE(u.email, '')
		FROM store_grants g
		LEFT JOIN users  u  ON g.subject_type = 'user'  AND u.id  = g.subject_id
		LEFT JOIN groups gr ON g.subject_type = 'group' AND gr.id = g.subject_id
		WHERE g.resource_id = ?
		ORDER BY g.subject_type, COALESCE(u.name, u.email, gr.name, ''), g.folder`, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StoreGrant{}
	for rows.Next() {
		var g StoreGrant
		if err := rows.Scan(&g.ID, &g.ResourceID, &g.SubjectType, &g.SubjectID, &g.Access, &g.Folder,
			&g.CreatedBy, &g.CreatedAt, &g.UpdatedAt, &g.SubjectName, &g.SubjectEmail); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PutStoreGrant creates or moves one subject's grant on one scope of one store.
//
// The (resource, subject, folder) triple is the identity, which is what makes
// the PUT idempotent; the level is what it carries. A second grant to the same
// subject **at the same scope** is an edit of the first and never a row beside
// it — two rows would make "what may this person do here" a question with two
// answers. A grant on another folder is a different scope rather than a second
// answer about the same one, which is why the folder is part of the identity
// and not part of the payload.
func (db *DB) PutStoreGrant(g *StoreGrant, actorID, requestID, ip, userAgent string) error {
	now := time.Now().UTC()

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Refused here as well as by the trigger, so the API can say what the
	// resource is rather than surfacing a plpgsql message.
	//
	// The test is the bucket, which is what the trigger tests too. A resource
	// with no objects behind it has nothing to presign, and that is a fact
	// about the backing rather than about the kind — an EFS-backed file store
	// is refused here and an S3 Files one is not.
	var res StorageResource
	err = tx.QueryRow(`SELECT kind, backing, bucket, storage_root FROM storage_resources WHERE id=?`, g.ResourceID).
		Scan(&res.Kind, &res.Backing, &res.Bucket, &res.Root)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if !res.HasDataPlane() {
		return fmt.Errorf("%w: this resource is a %s backed by %s, which has no objects behind it",
			ErrStoreGrantNoBucket, res.Kind, res.Backing)
	}

	var beforeAccess string
	var beforeID string
	var beforeBy sql.NullString
	var beforeAt time.Time
	// The row's own creation is read too: a raised grant is answered with the
	// row it is, and without these the response said 0001-01-01 and nobody.
	err = tx.QueryRow(`SELECT id, access, created_by, created_at FROM store_grants WHERE resource_id=? AND subject_type=? AND subject_id=? AND folder=?`,
		g.ResourceID, g.SubjectType, g.SubjectID, g.Folder).Scan(&beforeID, &beforeAccess, &beforeBy, &beforeAt)
	existing := true
	switch {
	case err == sql.ErrNoRows:
		existing = false
	case err != nil:
		return err
	}

	if existing {
		g.ID = beforeID
		g.CreatedBy, g.CreatedAt = beforeBy.String, beforeAt
		if _, err := tx.Exec(`UPDATE store_grants SET access=?, updated_at=? WHERE id=?`,
			g.Access, now, g.ID); err != nil {
			return err
		}
	} else {
		g.ID = uuid.New().String()
		g.CreatedBy = actorID
		g.CreatedAt = now
		if _, err := tx.Exec(`
			INSERT INTO store_grants (id, resource_id, subject_type, subject_id, access, folder, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.ID, g.ResourceID, g.SubjectType, g.SubjectID, g.Access, g.Folder,
			nullIfEmpty(g.CreatedBy), now, now); err != nil {
			return err
		}
	}
	g.UpdatedAt = now

	var beforeJSON string
	if existing {
		beforeJSON = jsonStr(map[string]string{"access": beforeAccess})
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "store_grant.put",
		TargetType: "storage_resource", TargetID: g.ResourceID,
		Before: beforeJSON,
		After: jsonStr(map[string]string{
			"subject_type": g.SubjectType, "subject_id": g.SubjectID, "access": g.Access,
			// The scope the level was granted on. Without it an auditor reading
			// a grant on a store cannot tell the whole store from one folder of
			// it, which is the question a folder-scoped install is auditing for.
			"folder": g.Folder}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteStoreGrant revokes one subject's reach on one scope. It takes effect on
// the next mint; URLs already signed live out their TTL, which is what the TTL
// is for.
//
// The folder is part of what is being revoked, so revoking `analytics/` leaves
// a root grant standing and revoking the root leaves `analytics/` standing.
// That reads oddly for a moment and is the only coherent answer: the two are
// separate rows, granted separately and audited separately, and a revocation
// that swept scopes it was not pointed at would be one click away from removing
// reach nobody asked to remove.
func (db *DB) DeleteStoreGrant(resourceID, subjectType, subjectID, folder, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var access string
	err = tx.QueryRow(`SELECT access FROM store_grants WHERE resource_id=? AND subject_type=? AND subject_id=? AND folder=?`,
		resourceID, subjectType, subjectID, folder).Scan(&access)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM store_grants WHERE resource_id=? AND subject_type=? AND subject_id=? AND folder=?`,
		resourceID, subjectType, subjectID, folder); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "store_grant.delete",
		TargetType: "storage_resource", TargetID: resourceID,
		Before: jsonStr(map[string]string{
			"subject_type": subjectType, "subject_id": subjectID, "access": access,
			"folder": folder}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
