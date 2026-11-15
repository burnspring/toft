package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Credentials an administrator holds and attaches to apps.
//
// The whole of what makes this different from an app's own `sensitive`
// environment variable is **who can read it back**, and the answer is nobody.
// A sensitive env var is write-only in the API, but its owner typed it, so they
// hold it by definition. These values are returned by no route, to no caller,
// including an administrator — what an administrator reads is which keys exist,
// not what they are.
//
// What that buys is accountability rather than containment: app code can read
// an injected value trivially, and nothing here pretends otherwise.

// Credential is one named bag, without its values.
//
// There is deliberately no shape in which this type carries a value. A struct
// that *could* hold one is a struct somebody eventually renders.
type Credential struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Keys        []string  `json:"keys"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// AppCount is how many apps hold it — what an administrator needs before
	// deciding whether rotating it is a small act or a large one.
	AppCount int `json:"app_count"`
}

// CredentialApp is one app holding one credential, for the credential's own
// page.
//
// The list is a **separate read** from the inventory, not a field on
// `Credential`: the inventory is every credential on the install and the apps
// holding them is a fan-out nobody browsing it asked for. What the inventory
// carries is still the count.
type CredentialApp struct {
	AppID   string `json:"app_id"`
	AppName string `json:"app_name"`
	AppSlug string `json:"app_slug"`
	// AttachedBy is who decided, resolved to a name. Empty when the account is
	// gone — the row is still the fact that it happened.
	AttachedBy string    `json:"attached_by,omitempty"`
	AttachedAt time.Time `json:"attached_at"`
}

var (
	// ErrCredentialNameTaken refuses a second credential under one name. The
	// name is what an administrator attaches by and what an app owner sees, so
	// two of them is an ambiguity in both directions.
	ErrCredentialNameTaken = errors.New("a credential with that name already exists")
	// ErrCredentialInUse refuses deleting one that is still attached. The
	// foreign key would refuse it anyway; this is the refusal that can name the
	// apps.
	ErrCredentialInUse = errors.New("this credential is attached to apps")
	// ErrCredentialNotFound is a name or id nobody has.
	ErrCredentialNotFound = errors.New("no such credential")
)

// KeyCollision is one key an attachment cannot supply because something else
// already does.
type KeyCollision struct {
	Key string `json:"key"`
	// From is what already supplies it: "the app's own variables", or the name
	// of another credential.
	From string `json:"from"`
}

// ListCredentials returns every credential with its key names and how many apps
// hold it. Never any values.
func (db *DB) ListCredentials() ([]Credential, error) {
	rows, err := db.conn.Query(`
		SELECT c.id, c.name, c.description, COALESCE(c.created_by, ''), c.created_at, c.updated_at,
		       (SELECT COUNT(*) FROM app_credentials ac WHERE ac.credential_id = c.id)
		FROM credentials c ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Credential{}
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.CreatedBy,
			&c.CreatedAt, &c.UpdatedAt, &c.AppCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		keys, err := db.credentialKeys(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Keys = keys
	}
	return out, nil
}

func (db *DB) credentialKeys(id string) ([]string, error) {
	rows, err := db.conn.Query(`SELECT key FROM credential_values WHERE credential_id=? ORDER BY key`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// credentialKeysTx is credentialKeys inside the transaction that is about to
// rewrite them, so the set it reads is the one it changes.
func credentialKeysTx(tx *Tx, id string) ([]string, error) {
	rows, err := tx.Query(`SELECT key FROM credential_values WHERE credential_id=? ORDER BY key`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// CreateCredential writes the credential and its values in one transaction.
//
// values are plaintext; encrypt is applied here so no caller can forget it and
// no route can accidentally store one in the clear.
func (db *DB) CreateCredential(ctx context.Context, name, description string,
	values map[string]string, encrypt func(string) (string, error), audit AuditEntry) (*Credential, error) {

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("a name is required")
	}
	if len(values) == 0 {
		return nil, errors.New("a credential with no values would supply nothing")
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM credentials WHERE name=?`, name).Scan(&exists); err != nil {
		return nil, err
	}
	if exists > 0 {
		return nil, ErrCredentialNameTaken
	}

	now := time.Now().UTC()
	c := &Credential{ID: uuid.NewString(), Name: name, Description: strings.TrimSpace(description),
		CreatedBy: audit.ActorID, CreatedAt: now, UpdatedAt: now}
	if _, err := tx.Exec(`
		INSERT INTO credentials (id, name, description, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.Description, nullIfEmpty(audit.ActorID), now, now); err != nil {
		return nil, err
	}
	if err := writeCredentialValues(tx, c.ID, values, encrypt); err != nil {
		return nil, err
	}
	c.Keys = sortedMapKeys(values)

	audit.Action = "credential.create"
	audit.TargetType = "credential"
	audit.TargetID = c.ID
	// The keys, never the values — in this row and in every other one this file
	// writes. A credential's whole point is that its values are held in one
	// place, and an audit log is a second place.
	audit.After = jsonStr(map[string]any{"name": c.Name, "keys": c.Keys})
	if err := writeAuditTx(tx, audit); err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

// UpdateCredential changes what an administrator can change about one: what it
// is called, what it is for, and what it supplies.
//
// **A nil field is one nobody is touching**, which is what lets a rename and a
// rotation be two acts against one route rather than one form that has to carry
// both. `description` is a pointer for the same reason `values` is absent-able:
// the rotation dialog does not show a description field, and a zero string
// reaching here would clear one nobody edited.
//
// Renaming changes nothing operational — the name is a label an administrator
// attaches by and an app owner reads, never a key, never a path, and never
// anything a running workload holds. Nothing restarts and no app is marked as
// carrying a pending change.
//
// Replacing the values marks every attached
// app as having an unapplied change.
//
// **The fan-out is the point, not a side effect.** The unapplied-change banner
// is a version counter on the app row, so without bumping it here every app
// holding this credential would read "up to date" while running the old value —
// and an operator who has just rotated a leaked password would believe they were
// finished.
//
// Replaces rather than merges: a merge cannot express removing a key, and a
// credential that accumulated a key nobody meant to keep would inject it
// forever. **`keep` is how a rotation leaves a value alone without retyping
// it**: the names listed keep their stored value, the names in `values` take
// the new one, and every other stored name goes. The set is still stated in
// full — each name is either sent, kept, or dropped — so removing a key stays
// expressible, while rotating one password on a three-name credential no
// longer needs the other two typed back in (a blank field means *keep* on every
// other write-only form in the product). A kept name that is not stored is
// refused rather than ignored: the caller believed something that is not so.
func (db *DB) UpdateCredential(ctx context.Context, id string, name, description *string,
	values map[string]string, keep []string, encrypt func(string) (string, error),
	audit AuditEntry) error {

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var was string
	if err := tx.QueryRow(`SELECT name FROM credentials WHERE id=?`, id).Scan(&was); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredentialNotFound
		}
		return err
	}
	now := was
	if name != nil {
		now = strings.TrimSpace(*name)
		if now == "" {
			return errors.New("a name is required")
		}
		// The same refusal as a create, and for the same reason: the name is
		// what an administrator attaches by and what an app owner sees, so two
		// of them is an ambiguity in both directions.
		var taken int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM credentials WHERE name=? AND id<>?`,
			now, id).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return ErrCredentialNameTaken
		}
		if _, err := tx.Exec(`UPDATE credentials SET name=? WHERE id=?`, now, id); err != nil {
			return err
		}
	}

	var finalKeys []string
	if values != nil || keep != nil {
		stored, err := credentialKeysTx(tx, id)
		if err != nil {
			return err
		}
		has := make(map[string]bool, len(stored))
		for _, k := range stored {
			has[k] = true
		}
		// A name both sent and kept is sent: the new value is the one typed.
		kept := make(map[string]bool, len(keep))
		for _, k := range keep {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if _, sent := values[k]; sent {
				continue
			}
			if !has[k] {
				return fmt.Errorf("%s is not stored on this credential, so it cannot be kept", k)
			}
			kept[k] = true
		}
		if len(values)+len(kept) == 0 {
			return errors.New("a credential with no values would supply nothing")
		}
		for _, k := range stored {
			if kept[k] {
				continue
			}
			if _, err := tx.Exec(`DELETE FROM credential_values WHERE credential_id=? AND key=?`,
				id, k); err != nil {
				return err
			}
		}
		if err := writeCredentialValues(tx, id, values, encrypt); err != nil {
			return err
		}
		// A rotation can add a key, and an attached app may already have it —
		// its own variable or another credential's. Checked after the write,
		// under each app's row lock (the one attach and SetEnvVar take), so the
		// three writers of an app's key space cannot pass each other (DB1-3).
		// Locked in id order, so two rotations sharing apps cannot deadlock.
		if err := rotationCollisions(tx, id); err != nil {
			return err
		}
		all := make(map[string]string, len(values)+len(kept))
		for k := range values {
			all[strings.TrimSpace(k)] = ""
		}
		for k := range kept {
			all[k] = ""
		}
		delete(all, "")
		finalKeys = sortedMapKeys(all)
		// Every attached app now has a pending change. One statement rather
		// than a loop: the set is whatever is attached at this instant, inside
		// the transaction that changed the values.
		if _, err := tx.Exec(`
			UPDATE apps SET env_version = env_version + 1
			WHERE id IN (SELECT app_id FROM app_credentials WHERE credential_id = ?)`, id); err != nil {
			return err
		}
	}
	if description != nil {
		if _, err := tx.Exec(`UPDATE credentials SET description=? WHERE id=?`,
			strings.TrimSpace(*description), id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE credentials SET updated_at=NOW() WHERE id=?`, id); err != nil {
		return err
	}

	audit.Action = "credential.update"
	audit.TargetType = "credential"
	audit.TargetID = id
	after := map[string]any{"name": now}
	if finalKeys != nil {
		after["keys"] = finalKeys
	}
	audit.After = jsonStr(after)
	// The old name, so a rename is legible in the log as a rename rather than
	// as an update to a credential nobody can find under that name any more.
	if name != nil && now != was {
		audit.Before = jsonStr(map[string]string{"name": was})
	}
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func writeCredentialValues(tx *Tx, id string, values map[string]string, encrypt func(string) (string, error)) error {
	for k, v := range values {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		enc, err := encrypt(v)
		if err != nil {
			return fmt.Errorf("encrypting %s: %w", k, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO credential_values (credential_id, key, value) VALUES (?, ?, ?)`,
			id, k, enc); err != nil {
			return err
		}
	}
	return nil
}

// DeleteCredential removes one, refusing while it is attached.
//
// The foreign key would refuse it anyway (ON DELETE RESTRICT). This refusal
// exists so the caller can name the apps: an administrator told "in use" and
// not told by what has to go looking.
func (db *DB) DeleteCredential(ctx context.Context, id string, audit AuditEntry) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var name string
	if err := tx.QueryRow(`SELECT name FROM credentials WHERE id=?`, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredentialNotFound
		}
		return err
	}
	var attached int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM app_credentials WHERE credential_id=?`, id).Scan(&attached); err != nil {
		return err
	}
	if attached > 0 {
		return ErrCredentialInUse
	}
	if _, err := tx.Exec(`DELETE FROM credentials WHERE id=?`, id); err != nil {
		return err
	}

	audit.Action = "credential.delete"
	audit.TargetType = "credential"
	audit.TargetID = id
	audit.Before = jsonStr(map[string]string{"name": name})
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// ListCredentialApps is which apps hold one, and who decided.
//
// The counterpart of AppsUsingCredential, which answers the same question in
// the shape a refusal message needs. This one is the page's: it carries the id
// a detach is addressed by, so an administrator looking at "3 apps" can act on
// the three rather than go looking for them one app page at a time.
//
// Every app, not the ones the caller could otherwise see: the only caller is an
// administrator, and a credential's blast radius that quietly omitted a row
// would be worse than no list at all.
func (db *DB) ListCredentialApps(id string) ([]CredentialApp, error) {
	rows, err := db.conn.Query(`
		SELECT a.id, a.name, a.slug,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.email,''), u.login, ''),
		       ac.attached_at
		FROM app_credentials ac
		JOIN apps a ON a.id = ac.app_id
		LEFT JOIN users u ON u.id = ac.attached_by
		WHERE ac.credential_id = ?
		ORDER BY a.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CredentialApp{}
	for rows.Next() {
		var c CredentialApp
		if err := rows.Scan(&c.AppID, &c.AppName, &c.AppSlug, &c.AttachedBy, &c.AttachedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppsUsingCredential names the apps holding one, for a refusal to quote.
func (db *DB) AppsUsingCredential(id string) ([]string, error) {
	rows, err := db.conn.Query(`
		SELECT a.name FROM apps a JOIN app_credentials ac ON ac.app_id = a.id
		WHERE ac.credential_id = ? ORDER BY a.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CredentialCollisions reports the keys an attachment could not supply because
// something else on this app already does.
//
// **Refused at the attach rather than resolved at start**, because either
// precedence is defensible and both are silent: the app's value winning means an
// administrator's credential quietly does nothing, and the credential winning
// means an owner's variable quietly changes value. A refusal happens at the
// moment somebody is looking at both.
func (db *DB) CredentialCollisions(appID, credentialID string) ([]KeyCollision, error) {
	return credentialCollisions(db.conn, appID, credentialID)
}

func credentialCollisions(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, appID, credentialID string) ([]KeyCollision, error) {
	rows, err := q.Query(`
		SELECT k.key, COALESCE(other.name, '')
		FROM credential_values k
		LEFT JOIN app_credentials ac ON ac.app_id = ?
		LEFT JOIN credentials other ON other.id = ac.credential_id
		     AND other.id <> k.credential_id
		     AND EXISTS (SELECT 1 FROM credential_values ov
		                 WHERE ov.credential_id = other.id AND ov.key = k.key)
		WHERE k.credential_id = ?
		  AND (other.id IS NOT NULL
		       OR EXISTS (SELECT 1 FROM env_vars e WHERE e.app_id = ? AND e.key = k.key))
		ORDER BY k.key`, appID, credentialID, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	out := []KeyCollision{}
	for rows.Next() {
		var c KeyCollision
		if err := rows.Scan(&c.Key, &c.From); err != nil {
			return nil, err
		}
		if seen[c.Key] {
			continue
		}
		seen[c.Key] = true
		if c.From == "" {
			c.From = "this app's own variables"
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnvKeyIsFromCredential reports whether a key an owner is trying to set is
// already supplied by a credential attached to their app, and which one.
//
// The mirror of CredentialCollisions, for the other write. The owner is told the
// credential's **name** — which they are entitled to, since it is on their app's
// page — and nothing else about it.
func (db *DB) EnvKeyIsFromCredential(appID, key string) (string, error) {
	return envKeyFromCredential(db.conn, appID, key)
}

func envKeyFromCredential(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, appID, key string) (string, error) {
	var name string
	err := q.QueryRow(`
		SELECT c.name FROM app_credentials ac
		JOIN credentials c ON c.id = ac.credential_id
		JOIN credential_values v ON v.credential_id = c.id AND v.key = ?
		WHERE ac.app_id = ? LIMIT 1`, key, appID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// AttachCredential gives an app a credential. Refuses on a key collision.
//
// The collision check runs inside the insert's transaction, after the app's
// row is locked — the lock SetEnvVar takes too. Checked on the pool first, two
// attaches sharing a key, or an attach and an owner's save of that key, both
// passed and the app ended up with two suppliers of one key (DB1-3).
func (db *DB) AttachCredential(ctx context.Context, appID, credentialID string, audit AuditEntry) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := lockAppRow(tx, appID); err != nil {
		return err
	}
	collisions, err := credentialCollisions(tx, appID, credentialID)
	if err != nil {
		return err
	}
	if len(collisions) > 0 {
		return &CollisionError{Collisions: collisions}
	}

	var name string
	if err := tx.QueryRow(`SELECT name FROM credentials WHERE id=?`, credentialID).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCredentialNotFound
		}
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO app_credentials (app_id, credential_id, attached_by, attached_at)
		VALUES (?, ?, ?, NOW()) ON CONFLICT DO NOTHING`,
		appID, credentialID, nullIfEmpty(audit.ActorID)); err != nil {
		return err
	}
	// The app is not running this yet, which is the same fact a changed env var
	// produces and is rendered by the same banner.
	if _, err := tx.Exec(`UPDATE apps SET env_version = env_version + 1 WHERE id=?`, appID); err != nil {
		return err
	}

	audit.Action = "credential.attach"
	audit.TargetType = "app"
	audit.TargetID = appID
	audit.After = jsonStr(map[string]string{"credential": name, "credential_id": credentialID})
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// DetachCredential takes it away. The caller restarts the app: the value is in
// a live process's environment and only a start replaces that.
func (db *DB) DetachCredential(ctx context.Context, appID, credentialID string, audit AuditEntry) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var name string
	if err := tx.QueryRow(`SELECT name FROM credentials WHERE id=?`, credentialID).Scan(&name); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	res, err := tx.Exec(`DELETE FROM app_credentials WHERE app_id=? AND credential_id=?`, appID, credentialID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE apps SET env_version = env_version + 1 WHERE id=?`, appID); err != nil {
		return err
	}

	audit.Action = "credential.detach"
	audit.TargetType = "app"
	audit.TargetID = appID
	audit.Before = jsonStr(map[string]string{"credential": name, "credential_id": credentialID})
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// AppCredentials is what an app holds, for its own page: names and key names,
// never values.
func (db *DB) AppCredentials(appID string) ([]Credential, error) {
	// Every column, not the three this page renders. A partially filled struct
	// serialises its zero values as though they meant something — the app read
	// was answering `created_at: "0001-01-01T00:00:00Z"` and `app_count: 0`,
	// which are not facts about anything.
	rows, err := db.conn.Query(`
		SELECT c.id, c.name, c.description, COALESCE(c.created_by, ''), c.created_at, c.updated_at,
		       (SELECT COUNT(*) FROM app_credentials x WHERE x.credential_id = c.id)
		FROM app_credentials ac
		JOIN credentials c ON c.id = ac.credential_id
		WHERE ac.app_id = ? ORDER BY c.name`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Credential{}
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.CreatedBy,
			&c.CreatedAt, &c.UpdatedAt, &c.AppCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		keys, err := db.credentialKeys(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Keys = keys
	}
	return out, nil
}

// AppCredentialValues is the only function that returns plaintext, and it has
// exactly two callers, both assembling a workload's environment: the builder,
// for a shared workload, and the runner bootstrap, for an isolated one.
//
// It is not reachable from any route. A credential's values leave this package
// for a process and for nothing else.
func (db *DB) AppCredentialValues(appID string, decrypt func(string) (string, error)) (map[string]string, error) {
	rows, err := db.conn.Query(`
		SELECT v.key, v.value FROM app_credentials ac
		JOIN credential_values v ON v.credential_id = ac.credential_id
		WHERE ac.app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, enc string
		if err := rows.Scan(&k, &enc); err != nil {
			return nil, err
		}
		plain, err := decrypt(enc)
		if err != nil {
			return nil, fmt.Errorf("decrypting credential value %s: %w", k, err)
		}
		out[k] = plain
	}
	return out, rows.Err()
}

// rotationCollisions locks every app holding a credential and refuses with
// *CollisionError the first one on which its keys, as just rewritten, collide.
func rotationCollisions(tx *Tx, credentialID string) error {
	rows, err := tx.Query(`
		SELECT a.id, a.name FROM app_credentials ac JOIN apps a ON a.id = ac.app_id
		WHERE ac.credential_id=? ORDER BY a.id`, credentialID)
	if err != nil {
		return err
	}
	type held struct{ id, name string }
	var apps []held
	for rows.Next() {
		var a held
		if err := rows.Scan(&a.id, &a.name); err != nil {
			rows.Close()
			return err
		}
		apps = append(apps, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range apps {
		if err := lockAppRow(tx, a.id); err != nil {
			return err
		}
		collisions, err := credentialCollisions(tx, a.id, credentialID)
		if err != nil {
			return err
		}
		if len(collisions) > 0 {
			return &CollisionError{Collisions: collisions, App: a.name}
		}
	}
	return nil
}

// lockAppRow serializes the writers of an app's key space — a credential
// attach, a credential rotation and an env var save — on the app's row. A
// missing app locks nothing; the write that follows fails on its foreign key.
func lockAppRow(tx *Tx, appID string) error {
	var id string
	err := tx.QueryRow(`SELECT id FROM apps WHERE id=? FOR UPDATE`, appID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// EnvKeyFromCredentialError refuses an env var whose key an attached
// credential already supplies — the in-transaction half of the check the
// handlers make with EnvKeyIsFromCredential before they write.
type EnvKeyFromCredentialError struct {
	Key, Credential string
}

func (e *EnvKeyFromCredentialError) Error() string {
	return e.Key + " is supplied by the " + e.Credential + " credential attached to this app"
}

// CollisionError carries the keys that refused an attachment, or a rotation.
type CollisionError struct {
	Collisions []KeyCollision
	// App names the app a rotation collided on; empty for an attach, whose
	// caller already addressed one.
	App string
}

func (e *CollisionError) Error() string {
	keys := make([]string, 0, len(e.Collisions))
	for _, c := range e.Collisions {
		keys = append(keys, c.Key)
	}
	return "already supplied on this app: " + strings.Join(keys, ", ")
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
