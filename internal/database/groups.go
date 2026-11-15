package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/auth"
)

// --- Groups ---

func (db *DB) CreateGroup(ctx context.Context, name, actorID string, meta RequestMeta) (*Group, error) {
	g := &Group{
		ID:        uuid.New().String(),
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO groups (id, name, created_at) VALUES (?, ?, ?)`, g.ID, g.Name, g.CreatedAt); err != nil {
		// The same collision the rename can hit, and it was reaching the
		// handler as an opaque error and being answered with a 500 — a person
		// typing a name somebody else already used is not a server fault, and
		// "internal error" gives them nothing to act on.
		if isUniqueViolation(err) {
			return nil, ErrGroupNameTaken
		}
		return nil, err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.create", TargetType: "group", TargetID: g.ID,
		After:     jsonStr(map[string]string{"name": name}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return g, nil
}

func (db *DB) GetGroup(id string) (*Group, error) {
	g := &Group{}
	err := db.conn.QueryRow(`SELECT id, name, email, created_at FROM groups WHERE id=?`, id).Scan(&g.ID, &g.Name, &g.Email, &g.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return g, err
}

// SetGroupEmail sets or clears the address accountability reaches this group
// at. An empty value clears it, which puts the group back on the member
// expansion rather than turning notification off — there is no way to make a
// group's apps notify nobody, and that is deliberate.
func (db *DB) SetGroupEmail(ctx context.Context, groupID, email, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var before string
	if err := tx.QueryRow(`SELECT email FROM groups WHERE id=?`, groupID).Scan(&before); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE groups SET email=? WHERE id=?`, email, groupID); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.update", TargetType: "group", TargetID: groupID,
		Before:    jsonStr(map[string]string{"email": before}),
		After:     jsonStr(map[string]string{"email": email}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrGroupNameTaken is a rename or a create that collided with a group that
// already has the name. `groups.name` is `UNIQUE` in the schema, so this is the
// database's answer rather than a check-then-write — which would be a race, and
// on a name people type at the same moment as one another it is not a
// theoretical one.
var ErrGroupNameTaken = errors.New("a group with that name already exists")

// SetGroupName renames a group.
//
// A group's name is the whole of how it is identified by a person — there is no
// slug beside it, unlike an app — so this is the only rename it has, and the id
// underneath is what every grant, every owned app and every audit row actually
// points at. Nothing else moves: a rename changes no membership, no grant and
// no ownership, which is why it needs no confirmation.
//
// Audited as `group.update` with the old name, and it is the old name that
// makes the row worth having: after the write, that string exists nowhere else
// on the install, so an audit row carrying only the new one could not answer
// "what was the group called when this app was shared with it".
func (db *DB) SetGroupName(ctx context.Context, groupID, name, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var before string
	if err := tx.QueryRow(`SELECT name FROM groups WHERE id=?`, groupID).Scan(&before); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE groups SET name=? WHERE id=?`, name, groupID); err != nil {
		if isUniqueViolation(err) {
			return ErrGroupNameTaken
		}
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.update", TargetType: "group", TargetID: groupID,
		Before:    jsonStr(map[string]string{"name": before}),
		After:     jsonStr(map[string]string{"name": name}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SearchGroups finds groups by name substring, case-insensitively, for the
// sharing panel's principal picker.
func (db *DB) SearchGroups(q string, limit int) ([]Group, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := db.conn.Query(`
		SELECT id, name, email, created_at FROM groups
		WHERE name ILIKE ? ESCAPE '\' ORDER BY name ASC LIMIT ?
	`, "%"+escaped+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gs []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Email, &g.CreatedAt); err != nil {
			return nil, err
		}
		gs = append(gs, g)
	}
	return gs, rows.Err()
}

func (db *DB) ListGroups() ([]GroupWithCount, error) {
	// Two scalar subqueries rather than two more LEFT JOINs: joining
	// group_members and apps in one statement multiplies the rows and both
	// counts come back wrong, which is the classic way this query breaks.
	rows, err := db.conn.Query(`
		SELECT g.id, g.name, g.email, g.created_at,
		       (SELECT COUNT(*) FROM group_members m WHERE m.group_id = g.id),
		       (SELECT COUNT(*) FROM apps a WHERE a.owner_group_id = g.id)
		FROM groups g
		ORDER BY g.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gs []GroupWithCount
	for rows.Next() {
		var g GroupWithCount
		if err := rows.Scan(&g.ID, &g.Name, &g.Email, &g.CreatedAt, &g.MemberCount, &g.OwnsApps); err != nil {
			return nil, err
		}
		gs = append(gs, g)
	}
	return gs, rows.Err()
}

// ErrGroupOwnsApps is §9.1 and §9.2: a group that owns apps cannot be deleted,
// and cannot lose its last eligible member. Both refusals exist for the reason
// handleDeleteUser already names for people — an app with no administrable
// owner is reachable only by system_admins through a side door nobody chose.
var ErrGroupOwnsApps = errors.New("group owns apps")

// ErrGroupWouldBeEmpty is §9.2 specifically: the removal is refused because it
// would take away the last member who could actually act as owner. Separate
// from ErrGroupOwnsApps so the API can say which of the two happened.
var ErrGroupWouldBeEmpty = errors.New("group would have no eligible members")

// CountEligibleGroupMembers counts the members of a group who can actually
// exercise ownership: not locked, and not a system viewer.
//
// Those two exclusions are not new policy — the middleware already rejects a
// locked session and auth.EffectiveObjectRole already clamps a system viewer
// down from owner. This counts the same people those two rules leave standing,
// which is what makes "at least one eligible member" mean something rather than
// counting rows that confer nothing.
func (db *DB) CountEligibleGroupMembers(groupID string, excludeUserID string) (int, error) {
	var n int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM group_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.group_id = ?
		  AND u.locked_at IS NULL
		  AND u.system_role <> ?
		  AND (? = '' OR u.id <> ?)
	`, groupID, SystemRoleViewer, excludeUserID, excludeUserID).Scan(&n)
	return n, err
}

// DeleteGroup deletes a group and every object_grant whose principal is this group.
// An audit entry is written for the deletion and for each removed grant.
//
// Refused with ErrGroupOwnsApps while the group owns anything (§9.1), mirroring
// handleDeleteUser exactly. The distinction matters and is easy to miss:
// object_grants cascades on group deletion — a grant on a deleted group is a
// live authorization row pointing at nothing — but *ownership* must not. The
// database enforces it too, since apps.owner_group_id carries no ON DELETE
// action; this check is what turns that constraint violation into a 409 naming
// the count instead of a 500.
func (db *DB) DeleteGroup(ctx context.Context, groupID, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owned int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM apps WHERE owner_group_id=?`, groupID).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return ErrGroupOwnsApps
	}

	// Fetch grants first so we can audit each one.
	rows, err := tx.Query(`SELECT object_id, role FROM object_grants WHERE principal_type='group' AND principal_id=?`, groupID)
	if err != nil {
		return err
	}
	type gr struct{ oid, role string }
	var grs []gr
	for rows.Next() {
		var r gr
		if err := rows.Scan(&r.oid, &r.role); err != nil {
			rows.Close()
			return err
		}
		grs = append(grs, r)
	}
	rows.Close()

	if _, err := tx.Exec(`DELETE FROM object_grants WHERE principal_type='group' AND principal_id=?`, groupID); err != nil {
		return err
	}
	// The same sweep for store grants, which carry no foreign key on their
	// subject for the same polymorphic reason. Not audited row by row the way
	// the object grants above are: a store grant's audit target is the
	// resource, and group.delete already names the group that lost them.
	if _, err := tx.Exec(`DELETE FROM store_grants WHERE subject_type='group' AND subject_id=?`, groupID); err != nil {
		return err
	}
	// The delegations too — refused while one stamps a live mapping; see
	// DeleteStorageManagersForSubject.
	if err := db.DeleteStorageManagersForSubject(tx, StoreSubjectGroup, groupID,
		actorID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		return err
	}
	// Members' sessions still carry this group in their JWT claims; revoke
	// them before the membership rows disappear.
	if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id IN (SELECT user_id FROM group_members WHERE group_id=?)`, groupID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM group_members WHERE group_id=?`, groupID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM groups WHERE id=?`, groupID); err != nil {
		return err
	}

	for _, r := range grs {
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: PrincipalTypeUser, ActorID: actorID,
			Action: "object_grant.remove", TargetType: "app", TargetID: r.oid,
			Before:    jsonStr(map[string]string{"principal_type": "group", "principal_id": groupID, "role": r.role}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
		}); err != nil {
			return err
		}
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.delete", TargetType: "group", TargetID: groupID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}

	return tx.Commit()
}

// AddGroupMember adds a user to a group, enforcing the 10-group-per-user cap.
// The cap is enforced both by DB trigger and by explicit check here for a
// controlled 409 response.
var ErrGroupCapReached = errors.New("user already in 10 groups")

// GroupMembershipCap is the number the trigger enforces. Stated here so the two
// places that count are counting to the same thing.
const GroupMembershipCap = 10

// groupSlotsLeft reports how many more groups a user may join inside tx.
//
// The count exists so an ordinary add gets a controlled 409 rather than a
// Postgres exception, and it has a second caller now: the directory reconciler,
// which has no administrator in front of it to receive an error and must stop
// before the trigger rather than after. One helper, so a change to what is
// counted cannot reach one caller and not the other.
func groupSlotsLeft(tx *Tx, userID string) (int, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM group_members WHERE user_id=?`, userID).Scan(&n); err != nil {
		return 0, err
	}
	if n >= GroupMembershipCap {
		return 0, nil
	}
	return GroupMembershipCap - n, nil
}

// isGroupCapViolation reports whether an error is the trigger firing.
//
// A string match on an exception message, which is as fragile as it looks and
// is kept deliberately: it is the backstop for the race the trigger exists to
// win, not the primary check. `groupSlotsLeft` is the primary check, and this
// is what catches the write that was legal when it was counted and is not by
// the time it lands.
func isGroupCapViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "maximum of 10 groups")
}

func (db *DB) AddGroupMember(ctx context.Context, groupID, userID, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	slots, err := groupSlotsLeft(tx, userID)
	if err != nil {
		return err
	}
	if slots == 0 {
		return ErrGroupCapReached
	}
	if _, err := tx.Exec(`INSERT INTO group_members (group_id, user_id, added_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, groupID, userID, time.Now().UTC()); err != nil {
		if isGroupCapViolation(err) {
			return ErrGroupCapReached
		}
		return err
	}
	// Sessions bake group membership into the JWT; revoke them so the new
	// grant takes effect now rather than at token expiry.
	if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id=?`, userID); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.member_add", TargetType: "group", TargetID: groupID,
		After:     jsonStr(map[string]string{"user_id": userID}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveGroupMember takes a user out of a group and revokes their sessions, so
// that everything the membership conferred is gone on their next request.
//
// That is now the whole offboarding story rather than a tidy-up: for a group
// that owns apps, this single row is what made a departing administrator an
// owner of all of them, and removing it takes owner away everywhere at once,
// with no session TTL to wait out.
//
// Which is also why it can be refused. §9.2: a group that owns apps cannot lose
// its last *eligible* member — one who is neither locked nor a system viewer,
// since neither can act as owner anyway. Without the refusal, removing one
// person silently makes every app that group owns administrable only by
// system_admins.
func (db *DB) RemoveGroupMember(ctx context.Context, groupID, userID, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owned int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM apps WHERE owner_group_id=?`, groupID).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		// Counted as it would be *after* the removal, inside the same
		// transaction as the delete — asking "how many are left" before the
		// write and acting on it afterwards is a race two administrators can
		// win together, each removing the second-to-last member.
		var remaining int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM group_members m
			JOIN users u ON u.id = m.user_id
			WHERE m.group_id = ? AND m.user_id <> ?
			  AND u.locked_at IS NULL AND u.system_role <> ?
		`, groupID, userID, SystemRoleViewer).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			return ErrGroupWouldBeEmpty
		}
	}

	if _, err := tx.Exec(`DELETE FROM group_members WHERE group_id=? AND user_id=?`, groupID, userID); err != nil {
		return err
	}
	// Revoke the removed member's sessions — group-derived grants live in
	// the JWT until it expires otherwise.
	if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id=?`, userID); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "group.member_remove", TargetType: "group", TargetID: groupID,
		Before:    jsonStr(map[string]string{"user_id": userID}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) GetUserGroupIDs(userID string) ([]string, error) {
	rows, err := db.conn.Query(`SELECT group_id FROM group_members WHERE user_id=? ORDER BY group_id LIMIT 10`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) ListGroupMembers(groupID string) ([]GroupMember, error) {
	rows, err := db.conn.Query(`
		SELECT u.id, u.login, u.name, u.email, u.avatar_url, u.system_role, u.locked_at, u.created_at,
		       m.source, m.source_provider_id
		FROM users u
		JOIN group_members m ON m.user_id = u.id
		WHERE m.group_id = ?
		ORDER BY u.login
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var us []GroupMember
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.ID, &m.Login, &m.Name, &m.Email, &m.AvatarURL, &m.SystemRole,
			&m.LockedAt, &m.CreatedAt, &m.Source, &m.SourceProviderID); err != nil {
			return nil, err
		}
		us = append(us, m)
	}
	return us, rows.Err()
}

// GroupMember is a person and the membership that put them here.
//
// A person is a User and a membership is not, so the provenance rides on this
// rather than on the user row: the same person is a manual member of one group
// and a directory member of another, and a field on User could not say both.
type GroupMember struct {
	User
	// Source is 'manual' or 'directory'. SourceProviderID names which provider
	// wrote it, and is empty for a manual row.
	Source           string `json:"source"`
	SourceProviderID string `json:"source_provider_id,omitempty"`
}

// PrincipalForUser builds the principal authorization runs against for one
// person, reading their system role and group membership live.
//
// One function rather than a literal at each call site, because the fields are
// what authorization is decided from and a caller that forgot Groups would be
// deciding against a person who is in no groups — a silent narrowing that looks
// like a working query. Three callers need it: the act-for path, where the
// platform holds the real principal at the moment it decides; the role a
// scheduled task's firing carries; and a job workload's trigger role.
//
// Nil with a nil error means there is no such user — a person deleted while a
// blob or a queued run still names them. Callers treat that as *nobody*, which
// authorizes as anonymous, rather than as an error.
func (db *DB) PrincipalForUser(userID string) (*auth.Principal, error) {
	if userID == "" {
		return nil, nil
	}
	user, err := db.GetUser(userID)
	if err != nil || user == nil {
		return nil, err
	}
	groups, err := db.GetUserGroupIDs(user.ID)
	if err != nil {
		return nil, err
	}
	return &auth.Principal{
		Type:       PrincipalTypeUser,
		ID:         user.ID,
		SystemRole: user.SystemRole,
		Groups:     groups,
	}, nil
}
