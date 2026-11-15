package database

import (
	"database/sql"
	"time"
)

// The reads the app's audit file makes that no route makes.

// attributionActions is the whole of what AppAttributions reads.
var attributionActions = []string{"app.create", ActionAppDeploy, "app.release_promoted"}

// Attribution is who did one of the three things AppAttributions reads.
type Attribution struct {
	Action       string    `json:"action"`
	ActorType    string    `json:"actor_type"`
	ActorID      string    `json:"actor_id,omitempty"`
	ActorLogin   string    `json:"actor_login,omitempty"`
	APIKeyID     string    `json:"api_key_id,omitempty"`
	DeploymentID string    `json:"deployment_id,omitempty"`
	At           time.Time `json:"at"`
}

// AppAttributions is audit_log's third reader, and it is bounded in code the
// way ListMySignins is: one app's target rows, three actions, a fixed column
// list — never before, never the rest of after, never ip or user_agent — and
// no cursor or caller filter. Its readers are the audit file's identity
// section (who created the app) and its releases section (who started each
// build, and who made one current again). See invariants § Identity.
func (db *DB) AppAttributions(appID string) ([]Attribution, error) {
	rows, err := db.conn.Query(`
		SELECT a.action, a.actor_type, COALESCE(a.actor_id, ''), COALESCE(u.login, ''),
		       COALESCE(a.api_key_id, ''), COALESCE(a.after->>'deployment_id', ''), a.occurred_at
		FROM audit_log a
		LEFT JOIN users u ON a.actor_type = 'user' AND u.id = a.actor_id
		WHERE a.target_type = 'app' AND a.target_id = ? AND a.action IN (?, ?, ?)
		ORDER BY a.occurred_at`, appID, attributionActions[0], attributionActions[1], attributionActions[2])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attribution
	for rows.Next() {
		var a Attribution
		if err := rows.Scan(&a.Action, &a.ActorType, &a.ActorID, &a.ActorLogin, &a.APIKeyID,
			&a.DeploymentID, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuditedGrant is one current grant with who gave its current role and when.
type AuditedGrant struct {
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
	DisplayName   string `json:"display_name"`
	Role          string `json:"role"`
	// GrantedBy is empty when no person is recorded; GrantedByLogin is the
	// account's current name, and GrantedByLocked marks one since locked.
	GrantedBy       string `json:"granted_by,omitempty"`
	GrantedByLogin  string `json:"granted_by_login,omitempty"`
	GrantedByLocked bool   `json:"granted_by_locked,omitempty"`
	// RoleSince is granted_at: when the current role was given. A write
	// that leaves the role unchanged leaves it alone.
	RoleSince time.Time `json:"role_since"`
	// For a group: how many members, by how they joined. A count, never a
	// roster, which is the directory's or the administrator's.
	ManualMembers    int `json:"manual_members,omitempty"`
	DirectoryMembers int `json:"directory_members,omitempty"`
}

// GrantsOf is every current grant on an app, the owner's own row included —
// it is the one record of when ownership last moved, which reach's fold
// drops.
func (db *DB) GrantsOf(appID string) ([]AuditedGrant, error) {
	rows, err := db.conn.Query(`
		SELECT g.principal_type, g.principal_id,
		       COALESCE(u.login, gr.name, g.principal_id), g.role,
		       COALESCE(g.granted_by, ''), COALESCE(gb.login, ''), COALESCE(gb.locked_at IS NOT NULL, false),
		       g.granted_at,
		       (SELECT COUNT(*) FROM group_members m WHERE g.principal_type = 'group' AND m.group_id = g.principal_id AND m.source = 'manual'),
		       (SELECT COUNT(*) FROM group_members m WHERE g.principal_type = 'group' AND m.group_id = g.principal_id AND m.source = 'directory')
		FROM object_grants g
		LEFT JOIN users u ON g.principal_type = 'user' AND u.id = g.principal_id
		LEFT JOIN groups gr ON g.principal_type = 'group' AND gr.id = g.principal_id
		LEFT JOIN users gb ON gb.id = g.granted_by
		WHERE g.object_id = ?
		ORDER BY CASE g.role WHEN 'owner' THEN 0 ELSE 1 END, g.principal_type, 3`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditedGrant
	for rows.Next() {
		var g AuditedGrant
		if err := rows.Scan(&g.PrincipalType, &g.PrincipalID, &g.DisplayName, &g.Role,
			&g.GrantedBy, &g.GrantedByLogin, &g.GrantedByLocked, &g.RoleSince,
			&g.ManualMembers, &g.DirectoryMembers); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CredentialAttachment is one credential an app holds, and since when.
type CredentialAttachment struct {
	CredentialID string    `json:"credential_id"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	// LastChanged is updated_at: any edit, a rotation or a rename — never
	// "last rotated", which nothing records.
	LastChanged time.Time `json:"last_changed"`
	AttachedAt  time.Time `json:"attached_at"`
	AttachedBy  string    `json:"attached_by,omitempty"`
}

// AppCredentialAttachments is every credential attached to an app, with
// when it was attached and by whom.
func (db *DB) AppCredentialAttachments(appID string) ([]CredentialAttachment, error) {
	rows, err := db.conn.Query(`
		SELECT c.id, c.name, c.created_at, c.updated_at, ac.attached_at, COALESCE(ac.attached_by, '')
		FROM app_credentials ac JOIN credentials c ON c.id = ac.credential_id
		WHERE ac.app_id = ? ORDER BY c.name`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CredentialAttachment
	for rows.Next() {
		var c CredentialAttachment
		if err := rows.Scan(&c.CredentialID, &c.Name, &c.CreatedAt, &c.LastChanged, &c.AttachedAt, &c.AttachedBy); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppRowCounts is how many rows each app-keyed table holds for one app, for
// the deletion section. tables is the caller's list; a table that will not
// read answers -1 rather than failing the section.
func (db *DB) AppRowCounts(appID string, tables map[string]string) map[string]int {
	out := make(map[string]int, len(tables))
	for table, column := range tables {
		var n int
		err := db.conn.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?`, appID).Scan(&n)
		if err != nil && err != sql.ErrNoRows {
			n = -1
		}
		out[table] = n
	}
	return out
}

// AppForeignKey is one column that references apps(id), and what deleting the
// app does to its rows.
type AppForeignKey struct {
	Table, Column, DeleteRule string
}

// AppForeignKeys is every foreign key to apps(id), read from the schema
// itself, so the deletion section's "removed with the app" is derived rather
// than remembered.
func (db *DB) AppForeignKeys() ([]AppForeignKey, error) {
	rows, err := db.conn.Query(`
		SELECT kcu.table_name, kcu.column_name, rc.delete_rule
		FROM information_schema.referential_constraints rc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = rc.constraint_name AND kcu.constraint_schema = rc.constraint_schema
		JOIN information_schema.constraint_column_usage ccu
		  ON ccu.constraint_name = rc.unique_constraint_name AND ccu.constraint_schema = rc.unique_constraint_schema
		WHERE ccu.table_name = 'apps' AND ccu.column_name = 'id' AND kcu.table_schema = 'public'
		ORDER BY kcu.table_name, kcu.column_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppForeignKey
	for rows.Next() {
		var k AppForeignKey
		if err := rows.Scan(&k.Table, &k.Column, &k.DeleteRule); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AppKeyedTables is every public table with an app_id or object_id column,
// for the guard that every one is either removed with the app or named as
// left behind.
func (db *DB) AppKeyedTables() (map[string]string, error) {
	rows, err := db.conn.Query(`
		SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name IN ('app_id', 'object_id')
		  AND table_name IN (SELECT table_name FROM information_schema.tables
		                     WHERE table_schema = 'public' AND table_type = 'BASE TABLE')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			return nil, err
		}
		out[t] = c
	}
	return out, rows.Err()
}
