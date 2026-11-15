package database

import (
	"context"
	"database/sql"
	"time"

	"toft/internal/auth"
)

func (db *DB) ListGrants(objectID string) ([]ObjectGrant, error) {
	rows, err := db.conn.Query(`
		SELECT object_id, principal_type, principal_id, role, COALESCE(granted_by,''), granted_at
		FROM object_grants WHERE object_id=? ORDER BY principal_type, principal_id
	`, objectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gs []ObjectGrant
	for rows.Next() {
		var g ObjectGrant
		if err := rows.Scan(&g.ObjectID, &g.PrincipalType, &g.PrincipalID, &g.Role, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		gs = append(gs, g)
	}
	return gs, rows.Err()
}

// GrantWithPrincipal is a grant row enriched with the principal's display
// identity, for the sharing panel — raw principal IDs mean nothing to users.
type GrantWithPrincipal struct {
	ObjectGrant
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// ListGrantsDetailed returns an app's grants with each principal's display
// name (user login or group name; the raw ID if the principal is gone).
func (db *DB) ListGrantsDetailed(objectID string) ([]GrantWithPrincipal, error) {
	rows, err := db.conn.Query(`
		SELECT g.object_id, g.principal_type, g.principal_id, g.role, COALESCE(g.granted_by,''), g.granted_at,
		       COALESCE(u.login, gr.name, g.principal_id), COALESCE(u.avatar_url, '')
		FROM object_grants g
		LEFT JOIN users u ON g.principal_type = 'user' AND u.id = g.principal_id
		LEFT JOIN groups gr ON g.principal_type = 'group' AND gr.id = g.principal_id
		WHERE g.object_id = ?
		ORDER BY g.principal_type, 7
	`, objectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gs []GrantWithPrincipal
	for rows.Next() {
		var g GrantWithPrincipal
		if err := rows.Scan(&g.ObjectID, &g.PrincipalType, &g.PrincipalID, &g.Role, &g.GrantedBy, &g.GrantedAt, &g.DisplayName, &g.AvatarURL); err != nil {
			return nil, err
		}
		gs = append(gs, g)
	}
	return gs, rows.Err()
}

// GetGrant returns the grant for a specific (object, principal) tuple, or nil.
func (db *DB) GetGrant(objectID, principalType, principalID string) (*ObjectGrant, error) {
	g := &ObjectGrant{}
	err := db.conn.QueryRow(`
		SELECT object_id, principal_type, principal_id, role, COALESCE(granted_by,''), granted_at
		FROM object_grants WHERE object_id=? AND principal_type=? AND principal_id=?
	`, objectID, principalType, principalID).Scan(&g.ObjectID, &g.PrincipalType, &g.PrincipalID, &g.Role, &g.GrantedBy, &g.GrantedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return g, err
}

// UpsertGrant inserts or updates a grant and writes an audit entry in the same transaction.
func (db *DB) UpsertGrant(ctx context.Context, objectID, principalType, principalID, role, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var before *ObjectGrant
	row := tx.QueryRow(`SELECT role FROM object_grants WHERE object_id=? AND principal_type=? AND principal_id=?`, objectID, principalType, principalID)
	var oldRole string
	if err := row.Scan(&oldRole); err == nil {
		before = &ObjectGrant{ObjectID: objectID, PrincipalType: principalType, PrincipalID: principalID, Role: oldRole}
	} else if err != sql.ErrNoRows {
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by, granted_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(object_id, principal_type, principal_id)
		DO UPDATE SET
			-- The stamp moves only when the role does: a re-save at the same
			-- role is a re-assertion the audit row records, not a new grant.
			granted_by = CASE WHEN object_grants.role = excluded.role THEN object_grants.granted_by ELSE excluded.granted_by END,
			granted_at = CASE WHEN object_grants.role = excluded.role THEN object_grants.granted_at ELSE excluded.granted_at END,
			role = excluded.role
	`, objectID, principalType, principalID, role, nullIfEmpty(actorID), time.Now().UTC()); err != nil {
		return err
	}

	action := "object_grant.create"
	if before != nil {
		action = "object_grant.modify"
	}
	var beforeJSON string
	if before != nil {
		beforeJSON = jsonStr(map[string]string{"principal_type": principalType, "principal_id": principalID, "role": before.Role})
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: action, TargetType: "app", TargetID: objectID,
		Before:    beforeJSON,
		After:     jsonStr(map[string]string{"principal_type": principalType, "principal_id": principalID, "role": role}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) DeleteGrant(ctx context.Context, objectID, principalType, principalID, actorID string, meta RequestMeta) (bool, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var oldRole string
	err = tx.QueryRow(`SELECT role FROM object_grants WHERE object_id=? AND principal_type=? AND principal_id=?`, objectID, principalType, principalID).Scan(&oldRole)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if _, err := tx.Exec(`DELETE FROM object_grants WHERE object_id=? AND principal_type=? AND principal_id=?`, objectID, principalType, principalID); err != nil {
		return false, err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "object_grant.remove", TargetType: "app", TargetID: objectID,
		Before:    jsonStr(map[string]string{"principal_type": principalType, "principal_id": principalID, "role": oldRole}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// EffectiveRole returns the highest-precedence object role for the given
// (objectID, userID, groups) combination. Returns "" if no grant applies.
func (db *DB) EffectiveRole(objectID, principalType, principalID string, groups []string) (string, error) {
	var role string

	if err := db.conn.QueryRow(`
		SELECT role FROM object_grants WHERE object_id=? AND principal_type=? AND principal_id=?
	`, objectID, principalType, principalID).Scan(&role); err != nil && err != sql.ErrNoRows {
		return "", err
	}

	// The strongest word wins, compared on internal/auth's ladder — the only
	// ranking of these rungs there is.
	best := role

	if principalType == PrincipalTypeUser && len(groups) > 0 {
		placeholders := ""
		args := []interface{}{objectID}
		for i, g := range groups {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, g)
		}
		rows, err := db.conn.Query(`
			SELECT role FROM object_grants
			WHERE object_id=? AND principal_type='group' AND principal_id IN (`+placeholders+`)
		`, args...)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return "", err
			}
			// A group resolves to its stored role. The clamp that used to sit
			// here — every group-derived role held down to viewer — went with
			// group ownership (ADR §51): a group that owns an app has to be
			// able to confer owner, and permitting owner while refusing the
			// strictly weaker editor would be a rung that exists only to
			// be explained.
			//
			// The clamp that survives is a different rule that happened to sit
			// next to this one: a user whose *system* role is viewer never
			// holds owner or editor, whatever any grant says. It lives in
			// auth.EffectiveObjectRole, applies to direct grants too, and is
			// what still makes a system viewer in an owning group a viewer.
			if auth.RolePrecedence(r) > auth.RolePrecedence(best) {
				best = r
			}
		}
	}

	return best, nil
}

// EffectiveRolesForObjects is EffectiveRole for a whole page of objects, in one
// query.
//
// The apps list needs the caller's role on every row — it decides which actions
// a row's menu may offer — and it polls. Calling EffectiveRole per row would put
// two queries per app on the hot path of the page an operator opens during an
// incident, which is the same reason the tags and the dependency alerts beside
// it are batched.
//
// Objects with no grant are simply absent from the map, which reads as the
// empty role — the same answer EffectiveRole gives for a row with no grant.
func (db *DB) EffectiveRolesForObjects(
	objectIDs []string, principalType, principalID string, groups []string,
) (map[string]string, error) {
	out := map[string]string{}
	if len(objectIDs) == 0 || principalID == "" {
		return out, nil
	}

	args := make([]interface{}, 0, len(objectIDs)+len(groups)+2)
	objPlaceholders := ""
	for i, id := range objectIDs {
		if i > 0 {
			objPlaceholders += ","
		}
		objPlaceholders += "?"
		args = append(args, id)
	}

	// The principal's own grant, plus any grant to a group they are in — the
	// same two sources EffectiveRole reads, ORed into one pass.
	where := "object_id IN (" + objPlaceholders + ") AND ((principal_type=? AND principal_id=?)"
	args = append(args, principalType, principalID)
	if principalType == PrincipalTypeUser && len(groups) > 0 {
		groupPlaceholders := ""
		for i, g := range groups {
			if i > 0 {
				groupPlaceholders += ","
			}
			groupPlaceholders += "?"
			args = append(args, g)
		}
		where += " OR (principal_type='group' AND principal_id IN (" + groupPlaceholders + "))"
	}
	where += ")"

	rows, err := db.conn.Query(`SELECT object_id, role FROM object_grants WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var objectID, role string
		if err := rows.Scan(&objectID, &role); err != nil {
			return nil, err
		}
		if auth.RolePrecedence(role) > auth.RolePrecedence(out[objectID]) {
			out[objectID] = role
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UserAccessGrant is one grant row feeding the admin access-review report:
// an app the user can reach explicitly, and the grant that says why.
type UserAccessGrant struct {
	AppID   string
	AppSlug string
	AppName string
	// AppOwnerID is the owning user, empty when a group owns the app;
	// AppOwnerGroupID is the other half. Same pair, same meaning, as on App.
	AppOwnerID      string
	AppOwnerGroupID string
	AppVisibility   string
	AppStatus       string
	// AppPublicWithheld travels with AppVisibility because the two are read
	// together: what the owner chose, and whether the install is honouring the
	// anonymous half of it — public apps turned off altogether (§70). A report
	// about who can reach what, answered from the stored column alone, would
	// describe an install that does not exist.
	AppPublicWithheld bool
	PrincipalType     string // user | group
	PrincipalID       string
	Role              string
}

// ListUserAccessGrants returns every grant row that gives the user explicit
// access — direct grants plus grants to any of the given groups — with the
// app they attach to. Ownership is reported separately via ListAppsOwnedBy;
// roles are stored values, the caller applies read-time clamps.
func (db *DB) ListUserAccessGrants(userID string, groups []string) ([]UserAccessGrant, error) {
	where := "(g.principal_type = 'user' AND g.principal_id = ?)"
	args := []interface{}{userID}
	if len(groups) > 0 {
		placeholders := ""
		for i, gid := range groups {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, gid)
		}
		where += " OR (g.principal_type = 'group' AND g.principal_id IN (" + placeholders + "))"
	}

	rows, err := db.conn.Query(`
		SELECT a.id, a.slug, a.name, COALESCE(a.owner_user_id,''), COALESCE(a.owner_group_id,''), a.visibility, a.status,
			(`+publicAppsOffExpr+`),
			g.principal_type, g.principal_id, g.role
		FROM object_grants g
		JOIN apps a ON a.id = g.object_id
		WHERE `+where+`
		ORDER BY a.slug, g.principal_type, g.principal_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserAccessGrant
	for rows.Next() {
		var r UserAccessGrant
		if err := rows.Scan(&r.AppID, &r.AppSlug, &r.AppName, &r.AppOwnerID, &r.AppOwnerGroupID, &r.AppVisibility, &r.AppStatus,
			&r.AppPublicWithheld, &r.PrincipalType, &r.PrincipalID, &r.Role); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAppsOwnedBy returns the apps this user owns **personally**.
//
// Deliberately not "apps they hold owner on": that set now includes everything
// owned by every group they belong to, and conflating the two is the bug this
// whole feature exists to remove. This is the set that blocks deleting the
// account, and a group-owned app must not — the departing administrator's
// membership is removed, the apps stay owned, and the account goes.
//
// Use ListAppsOwnedByGroup for the other half.
func (db *DB) ListAppsOwnedBy(userID string) ([]App, error) {
	return db.listOwnedApps(`owner_user_id = ?`, userID)
}

// ListAppsOwnedByGroup returns the apps a group owns. Deleting the group, or
// emptying it of eligible members, is refused while this is non-empty — an app
// whose owning group has no members is unadministrable except through a side
// door nobody chose, which is the hazard handleDeleteUser already names for
// people.
func (db *DB) ListAppsOwnedByGroup(groupID string) ([]App, error) {
	return db.listOwnedApps(`owner_group_id = ?`, groupID)
}

// CountAppsOwnedByGroup is ListAppsOwnedByGroup for the callers that only need
// the number — the "Owns N apps" column, and the 409 that names the count.
func (db *DB) CountAppsOwnedByGroup(groupID string) (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM apps WHERE owner_group_id = ?`, groupID).Scan(&n)
	return n, err
}

func (db *DB) listOwnedApps(where string, id string) ([]App, error) {
	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.name, COALESCE(apps.owner_user_id,''), COALESCE(apps.owner_group_id,''),
			apps.visibility, apps.status,
			(`+publicAppsOffExpr+`)
		FROM apps WHERE `+where+` ORDER BY apps.slug
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Slug, &a.Name, &a.OwnerID, &a.OwnerGroupID, &a.Visibility, &a.Status,
			&a.PublicWithheld); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// CountAppsByVisibility returns how many apps are public, how many of those
// are public *in effect*, and how many are visible to all logged-in users —
// the blanket-access summary of the report.
//
// Two public numbers, deliberately. The install-wide switch (§70) can make an
// app public by its owner's decision and not public in effect: an
// administrator asking "how exposed are we?" wants the second, and the same
// administrator asking "who chose what?" wants the first. On an install where
// public apps are off the second number is zero however many owners chose
// Public.
func (db *DB) CountAppsByVisibility() (public, publicInEffect, authenticated int, err error) {
	err = db.conn.QueryRow(`
		SELECT
			COUNT(*) FILTER (WHERE apps.visibility = 'public'),
			COUNT(*) FILTER (WHERE apps.visibility = 'public' AND NOT `+publicAppsOffExpr+`),
			COUNT(*) FILTER (WHERE apps.visibility = 'authenticated')
		FROM apps
	`).Scan(&public, &publicInEffect, &authenticated)
	return public, publicInEffect, authenticated, err
}

// ListUserGroups returns the groups the user belongs to (id + name), for
// attributing group-derived access in the report.
func (db *DB) ListUserGroups(userID string) ([]Group, error) {
	rows, err := db.conn.Query(`
		SELECT g.id, g.name, g.created_at
		FROM groups g JOIN group_members m ON m.group_id = g.id
		WHERE m.user_id = ? ORDER BY g.name
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gs []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt); err != nil {
			return nil, err
		}
		gs = append(gs, g)
	}
	return gs, rows.Err()
}

// UserGroupMembership is one group the user belongs to, with when they were
// added. AddedAt is nil for memberships made before the column existed —
// unknown, not epoch.
type UserGroupMembership struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	AddedAt *time.Time `json:"added_at,omitempty"`
}

// ListUserGroupMemberships returns the user's groups for the admin detail
// page. Separate from ListUserGroups, which feeds authorization and must stay
// the cheap id+name read on the sign-in path.
func (db *DB) ListUserGroupMemberships(userID string) ([]UserGroupMembership, error) {
	rows, err := db.conn.Query(`
		SELECT g.id, g.name, m.added_at
		FROM groups g JOIN group_members m ON m.group_id = g.id
		WHERE m.user_id = ? ORDER BY g.name
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserGroupMembership{}
	for rows.Next() {
		var m UserGroupMembership
		if err := rows.Scan(&m.ID, &m.Name, &m.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
