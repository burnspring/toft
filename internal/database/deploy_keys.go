package database

import (
	"context"
	"database/sql"
	"time"
)

// The deploy key — `toftd_`, the third machine credential class.
//
// The axis that separates the two is **who the credential belongs to**:
//
//	toftu_  a personal key  — an identity. Belongs to the person. Dies with them.
//	toftd_  a deploy key    — a grant on the install's deploy surface. Belongs to
//	                        nobody. Dies only when somebody revokes it.
//
// The second exists because ownership was only half the departure problem. A
// promotion pipeline authenticating with a person's `toftu_` breaks when that
// person leaves, and the field fix is to share one human's token — which is
// precisely the shared-credential shape NIST AC-2(9) constrains, IA-2 forbids
// and AU-3 makes unauditable. A distinct class is the compliance argument for
// the third prefix, not an aesthetic one (ADR §51).
//
// KeyStateRevoked is this class's own state. Disabled is reversible and shared
// with the other two; revoked is not, and the row survives revocation so the
// audit trail still resolves `deploy_key:{id}` to a name.
const KeyStateRevoked = "revoked"

// The two scopes inside the class, and what separates them is blast radius
// rather than belonging.
//
// A read-scoped key enumerates apps and groups, reads deployment history and a
// dependency verdict, and downloads a release archive. A deploy-scoped key does
// all of that and pushes: create, upload, deploy, roll back, restart, stop.
// `read` is a strict subset of `deploy`, which is a strict subset of what a
// person's own key may do, and a chained test says so (ADR §51).
//
// Deploy is the default because it is what the class already meant. A scope is
// chosen at mint and never edited: widening one in place would make it a label
// rather than a boundary, so nothing in this package writes the column after
// the insert.
const (
	DeployKeyScopeRead   = "read"
	DeployKeyScopeDeploy = "deploy"
)

// ValidDeployKeyScope reports whether a scope names one of the two. One
// definition, because the API refuses on it and the database constrains on it
// and a third spelling is how those two come to disagree.
func ValidDeployKeyScope(s string) bool {
	return s == DeployKeyScopeRead || s == DeployKeyScopeDeploy
}

// DeployKey is the stored row. The secret itself is never stored — only its
// SHA-256 — which is also why it is shown exactly once, at mint.
type DeployKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyHash   string `json:"-"`
	KeyPrefix string `json:"key_prefix"`
	// Scope is `read` or `deploy` — which route list bounds this key. Set at
	// mint and never updated.
	Scope string `json:"scope"`
	// MayAttach lets a deploy carrying this key add storage toft.toml declares,
	// on the app owner's delegation alone: the administrator who set it stands
	// in for the person a change otherwise needs. Deploy scope only.
	MayAttach bool `json:"may_attach"`
	// CreatedBy is who minted it, for the audit trail and for
	// revocation-on-departure review. It is **not** an owning user: the row
	// outlives that account, because a key that died with its minter would
	// reintroduce the exact failure this class exists to remove. Empty means
	// the account has since been deleted, which the admin page shows as "minted
	// by someone who has left" — a prompt to rotate, not a cascade.
	CreatedBy      string     `json:"created_by"`
	CreatedByLogin string     `json:"created_by_login,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// State reports how this key would be treated if presented right now. Same
// vocabulary as the other two classes, plus revoked.
func (k *DeployKey) State(now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return KeyStateRevoked
	case k.DisabledAt != nil:
		return KeyStateDisabled
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return KeyStateExpired
	}
	return KeyStateActive
}

// MarshalState is what the DTO carries, so the UI never re-derives the rule.
func (k *DeployKey) MarshalState() string { return k.State(time.Now().UTC()) }

// Usable reports whether the key may authenticate. Off means absent and off
// means now: a disabled or revoked key stops working on the next request, not
// when something expires.
func (k *DeployKey) Usable(now time.Time) bool { return k.State(now) == KeyStateActive }

// CreateDeployKey inserts the key and its audit entry atomically: minting a
// credential is an access grant and must never happen unrecorded.
func (db *DB) CreateDeployKey(k *DeployKey, audit AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO deploy_keys (id, name, key_hash, key_prefix, scope, created_by, created_at, expires_at, may_attach)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.KeyHash, k.KeyPrefix, k.Scope, nullIfEmpty(k.CreatedBy), k.CreatedAt, k.ExpiresAt, k.MayAttach); err != nil {
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

const deployKeyColumns = `k.id, k.name, k.key_prefix, k.scope, COALESCE(k.created_by,''), COALESCE(u.login,''),
	k.created_at, k.expires_at, k.last_used_at, k.disabled_at, k.revoked_at, k.may_attach`

func scanDeployKey(sc interface{ Scan(...any) error }) (*DeployKey, error) {
	var k DeployKey
	if err := sc.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.Scope, &k.CreatedBy, &k.CreatedByLogin,
		&k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt, &k.DisabledAt, &k.RevokedAt, &k.MayAttach); err != nil {
		return nil, err
	}
	return &k, nil
}

// ListDeployKeys returns every deploy key on the install, newest first.
//
// LEFT JOIN, not JOIN: created_by is ON DELETE SET NULL and a key whose minter
// has left is exactly the row an administrator most needs to see.
func (db *DB) ListDeployKeys() ([]DeployKey, error) {
	rows, err := db.conn.Query(`
		SELECT ` + deployKeyColumns + `
		FROM deploy_keys k
		LEFT JOIN users u ON u.id = k.created_by
		ORDER BY k.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployKey
	for rows.Next() {
		k, err := scanDeployKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (db *DB) GetDeployKey(id string) (*DeployKey, error) {
	k, err := scanDeployKey(db.conn.QueryRow(`
		SELECT `+deployKeyColumns+`
		FROM deploy_keys k
		LEFT JOIN users u ON u.id = k.created_by
		WHERE k.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// ResolveDeployKey looks a presented secret up by its hash. It returns the row
// whatever state it is in — the caller decides, and says which refusal it is,
// because "disabled" and "expired" tell somebody who already holds the key
// nothing they could not infer and save them an afternoon.
func (db *DB) ResolveDeployKey(hash string) (*DeployKey, error) {
	k, err := scanDeployKey(db.conn.QueryRow(`
		SELECT `+deployKeyColumns+`
		FROM deploy_keys k
		LEFT JOIN users u ON u.id = k.created_by
		WHERE k.key_hash = ?`, hash))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// TouchDeployKey records last use. Called off the request path, throttled by
// the caller — the *Data path* invariant forbids adding a write per request.
func (db *DB) TouchDeployKey(id string, at time.Time) error {
	_, err := db.conn.Exec(`UPDATE deploy_keys SET last_used_at=? WHERE id=?`, at, id)
	return err
}

// SetDeployKeyDisabled turns a key off or back on. Reversible, unlike revoke.
func (db *DB) SetDeployKeyDisabled(ctx context.Context, id string, disabled bool, actorID string, meta RequestMeta) error {
	return db.stampDeployKey(ctx, id, "disabled_at", disabled, actorID, "deploy_key.disable", "deploy_key.enable", meta)
}

// SetDeployKeyMayAttach lets a deploy key add storage toft.toml declares, on
// the app owner's delegation alone, or stops it. Audited as
// deploy_key.may_attach_set with the administrator who chose: that name is the
// answer to who decided a pipeline may attach (ADR § Storage somebody else can
// attach). The CHECK refuses it on a read-scoped key.
func (db *DB) SetDeployKeyMayAttach(ctx context.Context, id string, mayAttach bool, actorID string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var before bool
	if err := tx.QueryRow(`SELECT may_attach FROM deploy_keys WHERE id=?`, id).Scan(&before); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE deploy_keys SET may_attach=? WHERE id=?`, mayAttach, id); err != nil {
		return err
	}
	if err := WriteAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "deploy_key.may_attach_set", TargetType: "deploy_key", TargetID: id,
		Before:    jsonStr(map[string]bool{"may_attach": before}),
		After:     jsonStr(map[string]bool{"may_attach": mayAttach}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeDeployKey ends a key permanently. The row stays: `deploy_key:{id}` is
// in the audit log and has to keep resolving to a name.
func (db *DB) RevokeDeployKey(ctx context.Context, id, actorID string, meta RequestMeta) error {
	return db.stampDeployKey(ctx, id, "revoked_at", true, actorID, "deploy_key.revoke", "", meta)
}

func (db *DB) stampDeployKey(ctx context.Context, id, column string, set bool, actorID, onAction, offAction string, meta RequestMeta) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var name string
	if err := tx.QueryRow(`SELECT name FROM deploy_keys WHERE id=?`, id).Scan(&name); err != nil {
		return err
	}

	action := onAction
	// The column name is not user input — it is one of two literals chosen by
	// the two exported wrappers above — but it is still interpolated, so it is
	// worth saying out loud that no caller can reach this with a string of
	// their own.
	if set {
		if _, err := tx.Exec(`UPDATE deploy_keys SET `+column+`=NOW() WHERE id=? AND `+column+` IS NULL`, id); err != nil {
			return err
		}
	} else {
		action = offAction
		if _, err := tx.Exec(`UPDATE deploy_keys SET `+column+`=NULL WHERE id=?`, id); err != nil {
			return err
		}
	}

	if err := WriteAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: action, TargetType: "deploy_key", TargetID: id,
		After:     jsonStr(map[string]string{"name": name}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
