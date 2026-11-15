package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"toft/internal/deploysteps"
	"toft/internal/runtime"
	"toft/internal/textutil"
)

type DB struct {
	conn *Conn

	// The password the built-in administrator is created with on a FRESH
	// install — see ensureBuiltinAdmin. Empty means "generate one", which is the
	// secure default; an install that wants a known value supplies it.
	bootstrapAdminPassword string

	// The settings read on every request — the session policy and the
	// gateway's header switch — cached together. See requestPolicy.
	cachedPolicy atomic.Pointer[cachedRequestPolicy]

	// The range storage gate gids are allocated from; zero is the default.
	// See storage_gates.go.
	gateLow, gateHigh int
	// accountLow, accountHigh bound app account numbers (app_accounts.go).
	accountLow, accountHigh int
}

// Option configures Open.
type Option func(*DB)

// WithBootstrapAdminPassword sets the password the built-in administrator is
// created with on an install that does not have one yet.
//
// It applies only when the account is being given its first password. A boot
// that finds one already there ignores this entirely — otherwise a stale value
// in the environment would silently reset the administrator's chosen password on
// every restart, which is both a lockout and a backdoor depending on who knows
// the value.
func WithBootstrapAdminPassword(pw string) Option {
	return func(db *DB) { db.bootstrapAdminPassword = pw }
}

// Open connects to Postgres, migrates the schema to the latest version, and
// applies the idempotent bootstrap steps.
//
// dsn is a standard Postgres URL, e.g.
// postgres://toft:toft@localhost:5432/toft?sslmode=disable
func Open(ctx context.Context, dsn string, opts ...Option) (*DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Deployed apps are long-lived and mostly idle; the platform's own load is
	// bursty around deploys. Keep the pool small enough that N Toft nodes
	// do not exhaust Postgres' default 100 connections.
	//
	// **Idle connections are kept up to the cap and retired by age, not by
	// count.** A cap of 5 idle under 25 open closed every connection a burst
	// opened the moment it was returned, so the next burst dialled them again —
	// against RDS a TLS handshake and a SCRAM exchange each, tens of
	// milliseconds in front of a one-millisecond query. That stretched every
	// query's hold on its connection until the 25 were all taken: the drills on
	// trytoft, 2026-10-28, waited 3m31s across 9,315 waits with RDS at 5% CPU
	// and CloudWatch's minutely sample never above 5 connections. Keeping what a
	// burst opened costs nothing past the cap Postgres already budgets for, and
	// ConnMaxIdleTime hands them back once the load has gone.
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	if err := Migrate(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}

	db := &DB{conn: &Conn{db: sqlDB}}
	for _, opt := range opts {
		opt(db)
	}
	if err := db.Bootstrap(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("bootstrapping database: %w", err)
	}

	return db, nil
}

func (db *DB) Close() error {
	return db.conn.db.Close()
}

// Bootstrap runs the idempotent, data-level steps that must hold on every boot
// rather than once at a schema version: they depend on rows, not on structure.
//
// Open calls it. Exported because it is safe to re-run at any time — that is the
// whole point of it — and because a test that wants to prove an install heals
// itself has to be able to ask for a second boot.
func (db *DB) Bootstrap() error {
	// The built-in local administrator must always have a password, or the door
	// migration 21 opened leads nowhere.
	//
	// This is where the platform's guarantee of "there is always one way in"
	// actually lives. It replaced a step that promoted the oldest user to
	// system_admin whenever no admin existed — a rule that made whoever signed in
	// first the owner of the install, which on a corporate IdP is a race between
	// employees rather than a bootstrap.
	if err := db.ensureBuiltinAdmin(); err != nil {
		return fmt.Errorf("ensuring the built-in administrator: %w", err)
	}

	// Every app's owning principal must hold an owner grant. Covers rows
	// imported from outside the normal creation path.
	//
	// Both principal types, because the owner grant row is what makes group
	// ownership cheap: readableAppsPredicate already resolves a group grant, so
	// a group-owned app is readable by its members through the clause that was
	// always there. An owner grant that went missing would take read access
	// with it. granted_by is left NULL for a group — the column names a person,
	// and a group is not one.
	if _, err := db.conn.Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by)
		SELECT id, 'user', owner_user_id, 'owner', owner_user_id FROM apps
		WHERE owner_user_id IS NOT NULL AND owner_user_id <> ''
		ON CONFLICT (object_id, principal_type, principal_id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("backfilling owner grants: %w", err)
	}
	if _, err := db.conn.Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by)
		SELECT id, 'group', owner_group_id, 'owner', NULL FROM apps
		WHERE owner_group_id IS NOT NULL AND owner_group_id <> ''
		ON CONFLICT (object_id, principal_type, principal_id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("backfilling group owner grants: %w", err)
	}

	return nil
}

// rebind converts the '?' placeholders used throughout this package into the
// '$N' form Postgres requires.
//
// Keeping '?' at the call sites is deliberate: it left the ~200 existing
// queries untouched during the Postgres port, so the migration carried no risk
// of a mis-numbered placeholder. Quoted literals are skipped: Postgres escapes
// a quote inside a string by doubling it, and the toggle handles that naturally
// because the pair flips the state out and straight back in.
func rebind(query string) string {
	if !strings.ContainsRune(query, '?') {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + 16)

	n := 0
	inString := false
	inComment := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		// A line comment runs to the newline and nothing in it is SQL.
		//
		// This is not tidiness. An apostrophe inside a comment used to flip
		// inString for the whole rest of the query, so every ? after it stayed
		// a ? and Postgres answered "syntax error at or near )" — pointing at
		// a line nobody had touched, about a query that had worked for a year,
		// because somebody wrote "the app's slug" in a comment above it. The
		// queries in this package are long and heavily commented by house
		// style, which makes this a trap the style walks into.
		case inComment:
			b.WriteByte(c)
			if c == '\n' {
				inComment = false
			}
		case !inString && c == '-' && i+1 < len(query) && query[i+1] == '-':
			inComment = true
			b.WriteByte(c)
		case c == '\'':
			inString = !inString
			b.WriteByte(c)
		case c == '?' && !inString:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Conn wraps *sql.DB so every query is rebound on the way through. It exposes
// the same method names as *sql.DB, so call sites read unchanged.
type Conn struct {
	db *sql.DB
}

func (c *Conn) Exec(query string, args ...any) (sql.Result, error) {
	return c.db.Exec(rebind(query), args...)
}

func (c *Conn) Query(query string, args ...any) (*sql.Rows, error) {
	return c.db.Query(rebind(query), args...)
}

func (c *Conn) QueryRow(query string, args ...any) *sql.Row {
	return c.db.QueryRow(rebind(query), args...)
}

func (c *Conn) Begin() (*Tx, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

func (c *Conn) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// SQL exposes the raw handle for callers that need it (migrations, health
// checks). Queries issued through it are *not* rebound.
func (c *Conn) SQL() *sql.DB { return c.db }

// Tx wraps *sql.Tx with the same rebinding behaviour as Conn.
type Tx struct {
	tx *sql.Tx
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(rebind(query), args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(rebind(query), args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(rebind(query), args...)
}

func (t *Tx) Commit() error   { return t.tx.Commit() }
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// --- Users ---

func (db *DB) UpsertUser(u *User) error {
	_, err := db.conn.Exec(`
		INSERT INTO users (id, login, name, email, avatar_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET login=excluded.login, name=excluded.name,
			email=excluded.email, avatar_url=excluded.avatar_url
	`, u.ID, u.Login, u.Name, u.Email, u.AvatarURL, time.Now().UTC())
	return err
}

func (db *DB) GetUser(id string) (*User, error) {
	u := &User{}
	err := db.conn.QueryRow(`SELECT id, login, name, email, avatar_url, system_role, locked_at, created_at, last_login_at, unlocked_at, pending_activation_at, scanner_since FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Login, &u.Name, &u.Email, &u.AvatarURL, &u.SystemRole, &u.LockedAt, &u.CreatedAt, &u.LastLoginAt, &u.UnlockedAt, &u.PendingActivationAt, &u.ScannerSince)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// GetTokenVersion returns the user's current token_version. Sessions minted
// with an older version are dead — see auth.Middleware. Missing users report
// an error so callers fail closed.
func (db *DB) GetTokenVersion(userID string) (int, error) {
	var v int
	err := db.conn.QueryRow(`SELECT token_version FROM users WHERE id = ?`, userID).Scan(&v)
	return v, err
}

// GetSessionState returns the user's token_version and whether the account is
// locked, in a single read — auth.Middleware checks both on every request.
// Missing users report an error so callers fail closed.
func (db *DB) GetSessionState(userID string) (tokenVersion int, locked bool, err error) {
	var lockedAt sql.NullTime
	err = db.conn.QueryRow(`SELECT token_version, locked_at FROM users WHERE id = ?`, userID).Scan(&tokenVersion, &lockedAt)
	return tokenVersion, lockedAt.Valid, err
}

// RecordLogin stamps the user's last sign-in. Called from the one place a
// session is minted, so it counts sign-ins rather than requests — putting it
// on the request path would violate the no-DB-writes-in-the-proxy rule and
// turn it into a liveness ping nobody asked for.
func (db *DB) RecordLogin(userID string) error {
	_, err := db.conn.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().UTC(), userID)
	return err
}

// ErrUserOwnsApps and ErrLastAdmin are two of the three reasons a deletion is
// refused; ErrSubjectHoldsMappings (storage_managers.go) is the third.
// Both are checked inside the deleting transaction, so a concurrent transfer
// or demotion cannot slip past them.
//
// ErrLastAdmin's text is the sentence every surface shows, because the lock,
// the demotion, the deletion and the SCIM deactivation are one refusal: the
// predicate is OtherUnlockedAdminsTx, and "who can sign in" is the half of it
// the old spellings lost.
var (
	ErrUserOwnsApps = errors.New("user still owns apps")
	ErrLastAdmin    = errors.New("this is the last administrator who can sign in")
)

// DeleteUser removes an account and the authority attached to it, in one
// transaction with its audit entry.
//
// Grants and access requests go with the account because they are live
// authority naming an id that will no longer resolve. The audit log and
// app_usage_daily stay: one is the record of what happened, the other has no
// FK to users on purpose so an app's traffic still adds up afterwards.
//
// auth_providers.created_by is nulled rather than cascaded — a provider is
// infrastructure, and removing the admin who added it must not remove the way
// everyone else signs in.
func (db *DB) DeleteUser(userID string, audit AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owned int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM apps WHERE owner_user_id = ?`, userID).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return ErrUserOwnsApps
	}

	var role string
	if err := tx.QueryRow(`SELECT system_role FROM users WHERE id = ?`, userID).Scan(&role); err != nil {
		return err
	}
	if role == SystemRoleAdmin {
		others, err := OtherUnlockedAdminsTx(tx, userID)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}

	if _, err := tx.Exec(`UPDATE auth_providers SET created_by = NULL WHERE created_by = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM object_grants WHERE principal_type = 'user' AND principal_id = ?`, userID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM access_requests WHERE requester_id = ?`, userID); err != nil {
		return err
	}
	// Beside the object_grants sweep and for the same reason: store_grants
	// carries no foreign key on its subject, because the subject is
	// polymorphic. A grant left behind here is a live authorization row
	// pointing at nobody.
	if _, err := tx.Exec(
		`DELETE FROM store_grants WHERE subject_type = 'user' AND subject_id = ?`, userID,
	); err != nil {
		return err
	}
	// And the delegations, which carry the same polymorphic subject and one
	// thing store grants do not: mappings stamped to them. A delegation that
	// stamps a live mapping refuses the delete (*SubjectHoldsMappingsError) —
	// see DeleteStorageManagersForSubject for why neither silent removal nor a
	// restart is right.
	if err := db.DeleteStorageManagersForSubject(tx, StoreSubjectUser, userID,
		audit.ActorID, audit.RequestID, audit.IP, audit.UserAgent); err != nil {
		return err
	}
	// user_identities and group_members cascade.
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, userID); err != nil {
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// BumpTokenVersion invalidates every outstanding session of the user.
func (db *DB) BumpTokenVersion(userID string) error {
	_, err := db.conn.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id = ?`, userID)
	return err
}

// RevokeAllSessions is BumpTokenVersion with a name on it.
//
// The mechanism has existed since sessions did — a demotion, a group change and
// an administrator's lock all bump this column — but nothing ever let a person
// do it to themselves, which is the one case where it is urgent: a laptop is
// gone and every session it holds should stop being a session.
//
// Audited in the same transaction, unlike the sign-in family, which is written
// outside one. The distinction is real: a sign-in is an outcome to record, and
// an audit insert that failed must never be the reason somebody cannot get in.
// This is an authorization-relevant *mutation*, so the invariant applies —
// either the sessions end and the ledger says who ended them, or neither
// happens.
func (db *DB) RevokeAllSessions(userID string, audit AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id = ?`, userID); err != nil {
		return err
	}

	audit.Action = "session.revoke_all"
	audit.TargetType = "user"
	audit.TargetID = userID
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// Listing users lives in dormancy.go: the admin list carries each account's
// dormancy clock, and that expression has one home so the People page and the
// sweep cannot come to different conclusions about the same account. See
// ListUsersWithActivity.

// GetUsersByIDs resolves a set of ids to user rows, keyed by id. Ids with no
// row are simply absent from the map: callers name people who were active or
// who own something, and an account deleted since then is a gap to render, not
// an error to fail on.
func (db *DB) GetUsersByIDs(ids []string) (map[string]User, error) {
	out := make(map[string]User, len(ids))
	if len(ids) == 0 {
		return out, nil // an empty IN () is a syntax error
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := db.conn.Query(`
		SELECT id, login, name, email, avatar_url, system_role, locked_at, created_at, last_login_at, unlocked_at, pending_activation_at
		FROM users WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Login, &u.Name, &u.Email, &u.AvatarURL, &u.SystemRole, &u.LockedAt, &u.CreatedAt, &u.LastLoginAt, &u.UnlockedAt, &u.PendingActivationAt); err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

// likeContains is a LIKE pattern matching q anywhere, with q's own `%`, `_` and
// `\` matched literally. Use it with `ESCAPE '\'`: Postgres's default escape is
// already the backslash, so an unescaped `?q=foo\` was a pattern ending in an
// escape character — an error, answered as a 500 — and a `_` a wildcard.
func likeContains(q string) string {
	return "%" + likeEscape(q) + "%"
}

// likeEscape is q with LIKE's metacharacters matched literally, for a pattern
// the caller assembles itself. Use it with `ESCAPE '\'`.
func likeEscape(q string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
}

// SearchUsers finds users by login substring, case-insensitively, for the
// directory endpoint that feeds sharing/transfer pickers. Email is deliberately
// neither searched nor meant for exposure — the handler builds its own DTO.
func (db *DB) SearchUsers(q string, limit int) ([]User, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := db.conn.Query(`
		SELECT id, login, name, email, avatar_url, system_role, locked_at, created_at, pending_activation_at FROM users
		WHERE login ILIKE ? ESCAPE '\' ORDER BY login ASC LIMIT ?
	`, "%"+escaped+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Login, &u.Name, &u.Email, &u.AvatarURL, &u.SystemRole, &u.LockedAt, &u.CreatedAt, &u.PendingActivationAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (db *DB) CountSystemAdmins() (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM users WHERE system_role = 'system_admin'`).Scan(&n)
	return n, err
}

// UpdateUserRole updates a user's system_role. It does NOT enforce the last-admin invariant —
// callers must perform that check inside a transaction via CountSystemAdmins before calling.
func (db *DB) UpdateUserRole(id, role string) error {
	_, err := db.conn.Exec(`UPDATE users SET system_role=? WHERE id=?`, role, id)
	return err
}

// Conn returns the wrapped connection for transactional work.
func (db *DB) Conn() *Conn {
	return db.conn
}

// --- Apps ---

func (db *DB) CreateApp(a *App) error {
	a.ID = uuid.New().String()
	a.CreatedAt = time.Now().UTC()
	a.UpdatedAt = a.CreatedAt
	// No framework default. This said "nextjs" from the days the platform hosted
	// one thing, and what it produces now is a lie on the row: an app whose very
	// first build never got as far as detection — an unsupported repository, an
	// archive that would not unpack — is filed under Next.js forever, badged as
	// Next.js on its page and counted as Next.js under the framework tag.
	//
	// Empty is the honest value and the rest of the product already reads it
	// that way: tags.FrameworkValue answers not-ok for "" precisely because an
	// app that has never built has no framework, and frameworkMeta renders no
	// badge for it. The builder writes the real one when detection succeeds.
	if a.Visibility == "" {
		a.Visibility = VisibilityPrivate
	}
	// Shared is the product default and the only mode every topology offers.
	if a.ExecutionMode == "" {
		a.ExecutionMode = string(runtime.ModeShared)
	}
	// A new app cleans up after itself. Set on the struct rather than left to
	// the column default because the caller goes on using this App — the first
	// build is enqueued with it — and an App that says cleanup is off while its
	// row says otherwise would skip the prune on exactly one deploy, the first.
	a.AutoDeleteReleases = true
	a.NotifyFailures = true
	// And a new app is born listable, on the same rule for the same reason: the
	// App is serialized straight back to whoever created it, and the Sharing tab
	// draws its control from this field. Left to the column default it would
	// render as un-listed on the page that just created the app.
	//
	// On the struct rather than in the INSERT's column list, which is where the
	// value is genuinely defended: a creation path added later inherits the
	// column default either way, so what catches one that forgot is a test
	// asserting the *row* on every path — internal/database/app_listable_db_test.go
	// — not a fourth place to remember the word.
	//
	// BornUnlisted is the one input that can turn it off, and it is why
	// `listable` is now in the INSERT's column list rather than riding the
	// column default: a value that can be false has to be written, or the row
	// and the struct disagree on exactly the path that asked for false.
	a.Listable = !a.BornUnlisted
	_, err := db.conn.Exec(`
		INSERT INTO apps (id, slug, name, owner_user_id, owner_group_id, repo_url, branch, repo_subdir, framework, entrypoint, start_cmd, port, endpoint_host, status, visibility, listable, legacy_prefix_rewrite, current_release, release_ref, execution_mode, running_mode, auto_deploy, last_seen_sha, gallery_item_id, gallery_kind, gallery_version, git_token, git_connection_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, a.Slug, a.Name, nullIfEmpty(a.OwnerID), nullIfEmpty(a.OwnerGroupID), a.RepoURL, a.Branch, a.RepoSubdir, a.Framework, a.Entrypoint, a.StartCmd, a.Port, a.EndpointHost, a.Status, a.Visibility, a.Listable, a.LegacyPrefixRewrite, a.CurrentRelease, a.ReleaseRef, a.ExecutionMode, a.RunningMode, a.AutoDeploy, a.LastSeenSHA, a.GalleryItemID, a.GalleryKind, a.GalleryVersion, a.GitToken, nullIfEmpty(a.GitConnectionID), a.CreatedAt, a.UpdatedAt)
	return err
}

// CreateAppTx creates an app and its owner grant in a single transaction.
// Writes an audit_log entry for the grant creation using the provided actor
// metadata.
//
// actorType is passed rather than assumed, because the third credential class
// is not a person: an app created by a deploy key is attributed to
// `deploy_key`/{id}, with no users row behind it. Empty means the historical
// default, a user — every existing caller and every test.
func (db *DB) CreateAppTx(a *App, actorType, actorID, requestID, ip, userAgent string) error {
	if actorType == "" {
		actorType = PrincipalTypeUser
	}
	a.ID = uuid.New().String()
	a.CreatedAt = time.Now().UTC()
	a.UpdatedAt = a.CreatedAt
	// No framework default. This said "nextjs" from the days the platform hosted
	// one thing, and what it produces now is a lie on the row: an app whose very
	// first build never got as far as detection — an unsupported repository, an
	// archive that would not unpack — is filed under Next.js forever, badged as
	// Next.js on its page and counted as Next.js under the framework tag.
	//
	// Empty is the honest value and the rest of the product already reads it
	// that way: tags.FrameworkValue answers not-ok for "" precisely because an
	// app that has never built has no framework, and frameworkMeta renders no
	// badge for it. The builder writes the real one when detection succeeds.
	if a.Visibility == "" {
		a.Visibility = VisibilityPrivate
	}
	// Shared is the product default and the only mode every topology offers.
	if a.ExecutionMode == "" {
		a.ExecutionMode = string(runtime.ModeShared)
	}
	// A new app cleans up after itself. Set on the struct rather than left to
	// the column default because the caller goes on using this App — the first
	// build is enqueued with it — and an App that says cleanup is off while its
	// row says otherwise would skip the prune on exactly one deploy, the first.
	a.AutoDeleteReleases = true
	// Likewise: a new app notifies its people when it breaks, and the caller
	// goes on serializing this App back to whoever created it. Leaving it to
	// the column default would render the switch off on the page that just
	// created the app while the row said on.
	a.NotifyFailures = true
	// And listable, for the reason stated in CreateApp — including its one
	// input: an item that asked to be born hidden arrives here, since the
	// gallery creates through this path and not through CreateApp.
	a.Listable = !a.BornUnlisted

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO apps (id, slug, name, owner_user_id, owner_group_id, repo_url, branch, repo_subdir, framework, entrypoint, start_cmd, port, endpoint_host, status, visibility, listable, legacy_prefix_rewrite, current_release, release_ref, execution_mode, running_mode, auto_deploy, last_seen_sha, gallery_item_id, gallery_kind, gallery_version, git_token, git_connection_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, a.Slug, a.Name, nullIfEmpty(a.OwnerID), nullIfEmpty(a.OwnerGroupID), a.RepoURL, a.Branch, a.RepoSubdir, a.Framework, a.Entrypoint, a.StartCmd, a.Port, a.EndpointHost, a.Status, a.Visibility, a.Listable, a.LegacyPrefixRewrite, a.CurrentRelease, a.ReleaseRef, a.ExecutionMode, a.RunningMode, a.AutoDeploy, a.LastSeenSHA, a.GalleryItemID, a.GalleryKind, a.GalleryVersion, a.GitToken, nullIfEmpty(a.GitConnectionID), a.CreatedAt, a.UpdatedAt); err != nil {
		return err
	}

	// The app itself, before the grant that comes with it.
	//
	// "Who put this here, and when" is the first question asked of an app
	// nobody recognises, and until this row existed the only answer in the
	// audit was the owner grant written below — a row about *access* standing
	// in for a row about existence, which reads as a share rather than a
	// create and says nothing about where the code came from. The repository is
	// the part worth carrying: an app is a URL somebody pointed the platform
	// at, and that URL is the fact a reviewer wants without having to find a
	// row that may since have been deleted.
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.create",
		TargetType: "app", TargetID: a.ID,
		After: jsonStr(map[string]string{
			"slug": a.Slug, "name": a.Name, "repo_url": a.RepoURL, "branch": a.Branch, "repo_subdir": a.RepoSubdir,
			"visibility": a.Visibility, "execution_mode": a.ExecutionMode,
		}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}

	// The owner grant, for whichever principal owns it. A group-owned app is
	// readable by its members *only* through this row, so writing it in the
	// same transaction as the app matters more than it did when the owner was
	// always a person with a denormalised column to fall back on.
	ownerType, ownerID := a.OwnerPrincipal()
	// granted_by names a *person* and is a foreign key to users(id), so it is
	// NULL when the actor is not one. A deploy key creating an app is the case:
	// the audit row below records `deploy_key`/{id} as the actor, which is the
	// honest attribution, while the grant row records that no human granted it.
	grantedBy := nullIfEmpty(actorID)
	if actorType != PrincipalTypeUser {
		grantedBy = nil
	}
	if _, err := tx.Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by, granted_at)
		VALUES (?, ?, ?, 'owner', ?, ?)
	`, a.ID, ownerType, ownerID, grantedBy, a.CreatedAt); err != nil {
		return err
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "object_grant.create",
		TargetType: "app", TargetID: a.ID,
		After:     jsonStr(map[string]string{"principal_type": ownerType, "principal_id": ownerID, "role": ObjectRoleOwner}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}

	// An app born in a mode that is not the default says so, once, in the same
	// transaction as the row it describes.
	//
	// The same action name and the same shape SetAppExecutionMode writes, so
	// the audit's readers do not learn a second vocabulary for one subject —
	// and `before` names the mode this app would otherwise have had, so the row
	// is readable on its own without knowing what a create defaults to.
	//
	// Nothing is written when the app takes the default, for the same reason a
	// create does not record every column that took one. What makes this
	// column worth the exception is that it is a spend decision: an isolated
	// app costs the operator elastic capacity from its first build.
	if a.ExecutionMode != string(runtime.ModeShared) {
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: actorType, ActorID: actorID,
			Action:     "app.execution_mode.change",
			TargetType: "app", TargetID: a.ID,
			Before:    jsonStr(map[string]string{"execution_mode": string(runtime.ModeShared)}),
			After:     jsonStr(map[string]string{"execution_mode": a.ExecutionMode}),
			RequestID: requestID, IP: ip, UserAgent: userAgent,
		}); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (db *DB) GetApp(id string) (*App, error) {
	a := &App{}
	err := scanApp(db.conn.QueryRow(`SELECT `+appColumns+` FROM apps WHERE apps.id = ?`, id), a)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (db *DB) GetAppBySlug(slug string) (*App, error) {
	a := &App{}
	err := scanApp(db.conn.QueryRow(`SELECT `+appColumns+` FROM apps WHERE apps.slug = ?`, slug), a)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// publicAppsOffExpr is true when an administrator has turned public apps off
// for this install: a SQL fragment, spelled once, dropped into every query that
// has to know.
//
// One spelling, for the reason the Go half has one: the substitution has two
// enforcement seams — the authorizer and the anonymous listing — and a fragment
// written out twice is how they come to disagree, at which point an app is
// enumerated by name to the anonymous internet and refused at the gateway, or
// the reverse.
//
// It asks whether the row says `false` rather than whether it says `true`,
// because the default is **on**: on an install where nobody has ever opened the
// settings page there is no row at all, and a query testing for `'true'` would
// read that absence as "public apps are off" and take the whole estate dark on
// upgrade. PlatformSettings reads it the same way round, deliberately.
//
// Uncorrelated, so Postgres evaluates it once per query rather than once per
// row, over the settings table's primary key.
const publicAppsOffExpr = `EXISTS (SELECT 1 FROM settings ps
	WHERE ps.key = '` + SettingPublicAppsEnabled + `' AND ps.value = 'false')`

// appsListableToAuthenticatedExpr is true unless an administrator has switched
// off the install-wide `list` rung: every signed-in person sees every app in
// their listing and may open none of it that they were not already granted.
//
// Written exactly like publicAppsOffExpr above, and for its reason rather than
// the mirror of it. This one *offers* something where that one takes something
// away — but both default on, and a default-on switch has only one correct
// spelling: an install where nobody has ever opened the settings page has no
// row at all, and a query asking for `'true'` would read that absence as "off"
// and quietly leave every install that never touched the setting behaving as it
// did before. The installs this is meant to reach are exactly those. So it asks
// whether the row says `'false'`, and PlatformSettings reads it the same way
// round, deliberately.
//
// It appears in exactly one place, listableAppsPredicate's signed-in branch,
// ANDed with apps.listable — the owner's half of the same rung — and it is a
// clause rather than a grant row: turning it off withdraws exactly what it
// granted, and nothing an owner wrote goes with it.
//
// Uncorrelated, so Postgres evaluates it once per query over the settings
// table's primary key.
const appsListableToAuthenticatedExpr = `NOT EXISTS (SELECT 1 FROM settings ls
	WHERE ls.key = '` + SettingAppsListableToAuthenticated + `' AND ls.value = 'false')`

// appColumns is the SELECT list every app query shares, qualified with the
// table name so it can be dropped into a join unchanged.
//
// public_withheld is derived rather than stored, and every reader of an app
// row needs the answer because objectFromApp is a pure function of that row. A
// correlated subquery rather than a LEFT JOIN because this constant is a
// column list dropped into half a dozen FROM clauses across three files, and a
// join would have to be added to each of them and remembered by the next one.
//
// **Every query that loads an app goes through this list**, which is the
// property the substitution rests on: a reader assembling its own SELECT would
// carry a false where the others carry the truth, and would then authorize
// from it.
const appColumns = "apps.id, apps.slug, apps.name, COALESCE(apps.owner_user_id,'') AS owner_user_id, COALESCE(apps.owner_group_id,'') AS owner_group_id, apps.repo_url, apps.branch, apps.repo_subdir, apps.framework, apps.entrypoint, apps.start_cmd, apps.port, apps.endpoint_host, apps.status, apps.visibility, apps.listable, apps.frame_ancestors, apps.legacy_prefix_rewrite, apps.current_release, apps.release_ref, apps.execution_mode, apps.running_mode, apps.auto_deploy, apps.last_seen_sha, apps.last_seen_tree, apps.auto_delete_releases, apps.releases_kept, apps.never_sleep, apps.viewer_identity, apps.locked_at, COALESCE(apps.locked_by,'') AS locked_by, apps.locked_message, apps.locked_source, apps.notify_failures, apps.gallery_item_id, apps.gallery_kind, apps.gallery_version, apps.git_token, COALESCE(apps.git_connection_id,'') AS git_connection_id, apps.build_timeout_seconds, apps.memory_limit_mb, apps.cpu_limit_millicores, apps.release_base_path, COALESCE(apps.hostname,'') AS hostname, apps.hostname_enabled, apps.created_at, apps.updated_at, apps.serving_since, apps.status_changed_at, " +
	"(" + publicAppsOffExpr + ") AS public_withheld, " +
	"(" + appsListableToAuthenticatedExpr + ") AS listable_to_all, " +
	lastDeployedAtExpr + " AS last_deployed_at, apps.garth_linked_at"

// scanApp reads one row selecting appColumns.
//
// A shared scanner rather than the same argument list written out at each
// call site, so that a column added to appColumns is one edit here — which is
// how GetApp came to select a shorter list than ListApps did.
func scanApp(row interface{ Scan(...any) error }, a *App) error {
	if err := row.Scan(&a.ID, &a.Slug, &a.Name, &a.OwnerID, &a.OwnerGroupID, &a.RepoURL, &a.Branch, &a.RepoSubdir, &a.Framework, &a.Entrypoint, &a.StartCmd, &a.Port, &a.EndpointHost, &a.Status, &a.Visibility, &a.Listable, &a.FrameAncestors, &a.LegacyPrefixRewrite, &a.CurrentRelease, &a.ReleaseRef, &a.ExecutionMode, &a.RunningMode, &a.AutoDeploy, &a.LastSeenSHA, &a.LastSeenTree, &a.AutoDeleteReleases, &a.ReleasesKept, &a.NeverSleep, &a.ViewerIdentity, &a.LockedAt, &a.LockedBy, &a.LockedMessage, &a.LockedSource, &a.NotifyFailures, &a.GalleryItemID, &a.GalleryKind, &a.GalleryVersion, &a.GitToken, &a.GitConnectionID, &a.BuildTimeoutSeconds, &a.MemoryLimitMB, &a.CPULimitMillicores, &a.ReleaseBasePath, &a.Hostname, &a.HostnameEnabled, &a.CreatedAt, &a.UpdatedAt, &a.ServingSince, &a.StatusChangedAt, &a.PublicWithheld, &a.ListableToAll, &a.LastDeployedAt, &a.GarthLinkedAt); err != nil {
		return err
	}
	// The column is NOT NULL DEFAULT 'admin' and inert while locked_at is
	// NULL, so an unlocked app read back as `locked_source: "admin"` — a lock
	// source with no lock (drills rerun 2026-10-22 #9). Who placed a lock is
	// said only while there is one; LockSource() still answers admin for "".
	if a.LockedAt == nil {
		a.LockedSource = ""
	}
	return nil
}

// scanAppRows drains a result set selecting appColumns.
func scanAppRows(rows *sql.Rows) ([]App, error) {
	defer rows.Close()
	var apps []App
	for rows.Next() {
		var a App
		if err := scanApp(rows, &a); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// There are two definitions of the set of apps a principal may see, and which
// one a query wants is the question every caller has to answer out loud.
//
// **readableAppsPredicate** is apps the principal may *open*. It is what an
// object check means, and it excludes the list rung.
//
// **listableAppsPredicate** is apps the principal may *see the existence of*.
// It is readable plus a list grant, and it is what a listing means.
//
// listable ⊇ readable by construction — the second calls the first and ORs one
// disjunct onto it — and the containment is asserted anyway over generated
// grant states (TestListableContainsReadable), because "by construction" stops
// being true the first time somebody edits one of them. Getting the two backwards in
// either direction is a bug a test with one app in it cannot see: a listing on
// `readable` hides apps from people who were granted the rung, and an object
// check on `listable` makes an app *readable* to everyone who could see its
// name. The second is the one that matters, and it is why these are two named
// functions rather than one with a boolean argument passed by position.
//
// Both return a parenthesized SQL fragment in the '?' dialect together with its
// args. An empty principalID means anonymous (public apps only); a nil groups
// slice skips group-based access (e.g. service accounts). Columns are qualified
// apps.* so the fragment drops into a join query unchanged.
func readableAppsPredicate(principalID string, principalType string, groups []string) (string, []interface{}) {

	// The one place in this SQL where the ladder is consulted at all. A grant
	// row used to mean read access whatever its role, which is exactly what must
	// not happen the moment migration 00039 admits the word `list` — so both
	// grant clauses below subtract the rung, and listableAppsPredicate adds it
	// back as a disjunct of its own rather than by relaxing them.
	const notListRung = " AND g.role <> '" + ObjectRoleList + "'"

	args := []interface{}{}
	var where []string

	// The bottom rung, and the one clause in here that is not about the
	// ladder. The install-wide switch (§70) withdraws anonymity and nothing
	// else: an administrator who has turned public apps off leaves the
	// signed-in branch below untouched, so every colleague keeps what the
	// owner gave them. It is `AND`ed onto the public test rather than
	// replacing it, so an anonymous caller on such an install matches nothing.
	// Written here in SQL as well as in objectFromApp, because a withdrawal that
	// landed only in Go would leave the app hidden from the gateway and still
	// enumerated to the anonymous internet by name and by slug.
	if principalID == "" {
		where = append(where, `apps.visibility = 'public'
			AND NOT `+publicAppsOffExpr)
	}

	if principalID != "" {
		where = append(where, "apps.visibility IN ('public', 'authenticated')")
		where = append(where, "apps.owner_user_id = ?")
		args = append(args, principalID)

		// Direct grants
		where = append(where, "EXISTS (SELECT 1 FROM object_grants g WHERE g.object_id = apps.id AND g.principal_type = ? AND g.principal_id = ?"+notListRung+")")
		args = append(args, principalType, principalID)

		// Group grants (users only)
		if principalType == PrincipalTypeUser && len(groups) > 0 {
			placeholders := ""
			for i, gid := range groups {
				if i > 0 {
					placeholders += ","
				}
				placeholders += "?"
				args = append(args, gid)
			}
			where = append(where, "EXISTS (SELECT 1 FROM object_grants g WHERE g.object_id = apps.id AND g.principal_type = 'group' AND g.principal_id IN ("+placeholders+")"+notListRung+")")
		}
	}

	pred := ""
	for i, w := range where {
		if i > 0 {
			pred += " OR "
		}
		pred += "(" + w + ")"
	}
	return "(" + pred + ")", args
}

// listableAppsPredicate is readableAppsPredicate plus a list-grant clause, and
// it is written as exactly that — the readable fragment, OR one disjunct — so
// the containment is a property of the code and not of two parallel query
// builders that have to be kept in step.
//
// The disjunct is added to the set rather than allowed to widen the clauses
// above it: a list grant confers listability and nothing else, so it may not
// reach ownership, visibility, or a grant of any other rung.
//
// An anonymous caller is never told about anything they cannot read — the rung
// is a grant naming a principal, and there is none — so for that case the two
// predicates are the same fragment and an install's public listing is untouched
// by the rung entirely.
func listableAppsPredicate(principalID string, principalType string, groups []string) (string, []interface{}) {
	pred, args := readableAppsPredicate(principalID, principalType, groups)
	if principalID == "" {
		return pred, args
	}

	// The install-wide rung, which needs no grant row at all: any app, to
	// anybody signed in. It is inside the signed-in branch, so an anonymous
	// listing cannot reach it.
	//
	// `AND apps.listable` is the owner's half, and **it is ANDed onto this
	// disjunct alone** — never onto the ownership clause, the visibility clause
	// or either grant clause, all of which live in readableAppsPredicate and are
	// returned above untouched. That is what makes the column incapable of
	// subtracting anything but what the install gave: `listable ⊇ readable`
	// survives because the fragment being narrowed is not part of `readable`,
	// and a grant always outranks the column because a grant reaches a different
	// disjunct. An owner who un-lists an app takes away what the install gave
	// and nothing they gave themselves.
	extra := "(" + appsListableToAuthenticatedExpr + " AND apps.listable)"

	grantees := []string{"(g.principal_type = ? AND g.principal_id = ?)"}
	args = append(args, principalType, principalID)
	if principalType == PrincipalTypeUser && len(groups) > 0 {
		placeholders := ""
		for i, gid := range groups {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, gid)
		}
		grantees = append(grantees, "(g.principal_type = 'group' AND g.principal_id IN ("+placeholders+"))")
	}

	return "(" + pred + " OR " + extra + " OR EXISTS (SELECT 1 FROM object_grants g WHERE g.object_id = apps.id AND g.role = '" +
		ObjectRoleList + "' AND (" + strings.Join(grantees, " OR ") + ")))", args
}

// ListApps returns apps the given principal may see the existence of — which is
// listable rather than readable, so an app granted at the list rung appears here
// and 403s when opened. That is the rung's entire purpose.
// If principalID is empty, only public-visibility apps are returned (anonymous).
// If groups is nil, group-based access is skipped (e.g. service accounts).
func (db *DB) ListApps(principalID string, principalType string, groups []string) ([]App, error) {
	pred, args := listableAppsPredicate(principalID, principalType, groups)

	rows, err := db.conn.Query(
		"SELECT "+appColumns+" FROM apps WHERE "+pred+" ORDER BY apps.created_at DESC", args...)
	if err != nil {
		return nil, err
	}
	return scanAppRows(rows)
}

// ListAllApps returns every app — used internally (e.g. system_admin views, background jobs).
func (db *DB) ListAllApps() ([]App, error) {
	rows, err := db.conn.Query("SELECT " + appColumns + " FROM apps ORDER BY apps.created_at DESC")
	if err != nil {
		return nil, err
	}
	return scanAppRows(rows)
}

// UpdateApp deliberately leaves branch, auto_deploy, last_seen_sha, git_token
// and **status** alone: the builder calls this mid-build with an App loaded
// minutes earlier, and writing those columns here would silently revert a
// concurrent settings change or auto-deploy claim. They have dedicated setters
// below.
//
// **`status` is the fifth name on that list and it was the last to arrive.**
// The builder sets the app `building`, then persists the detected framework
// about a hundred milliseconds later with this call — and the App it holds
// still carries the status the row had *before* the build started. Every
// rebuild therefore went `sleeping -> building -> sleeping` inside a fifth of a
// second, which is why the estate list's Building tab never showed one: a
// rebuild is invisible there, while a *first* build looks fine only because the
// row was inserted `building` and the stale value clobbers it with itself.
//
// Caught with an AFTER UPDATE trigger recording `current_query()`, which is
// what turned "the write does not happen" into "the write happens and is
// overwritten by this statement".
func (db *DB) UpdateApp(a *App) error {
	a.UpdatedAt = time.Now().UTC()
	_, err := db.conn.Exec(`
		UPDATE apps SET slug=?, name=?, framework=?, entrypoint=?, start_cmd=?, port=?, endpoint_host=?, visibility=?, legacy_prefix_rewrite=?, current_release=?, release_ref=?, running_mode=?, updated_at=?,
			serving_since = CASE WHEN current_release = ? THEN serving_since WHEN ? = '' THEN NULL ELSE ? END
		WHERE id=?
	`, a.Slug, a.Name, a.Framework, a.Entrypoint, a.StartCmd, a.Port, a.EndpointHost, a.Visibility, a.LegacyPrefixRewrite, a.CurrentRelease, a.ReleaseRef, a.RunningMode, a.UpdatedAt,
		a.CurrentRelease, a.CurrentRelease, a.UpdatedAt, a.ID)
	return err
}

// SetAppNameAndSlug writes the two columns a settings change owns, and nothing
// else. The settings handler used to write the row through UpdateApp with the
// App it loaded at the start of the request, so a PATCH that straddled a
// deploy's flip wrote the old current_release and port back over the new, and
// one that straddled another's visibility change reverted it with no audit
// row (code review 2026-11-11, P1-1).
func (db *DB) SetAppNameAndSlug(id, name, slug string) error {
	_, err := db.conn.Exec(`UPDATE apps SET name=?, slug=?, updated_at=? WHERE id=?`, name, slug, time.Now().UTC(), id)
	return err
}

// SetAppDetection records what a build detected: framework, entrypoint and
// start command, and nothing else.
//
// **The builder must not write the row through UpdateApp.** It holds an App
// loaded when the build started, and UpdateApp writes every column it owns --
// name, slug and visibility among them -- so a rename or a visibility change
// made while the build ran was silently reverted the moment the build saved
// what it detected. An owner who made an app private during its first build
// could find it public again. These two setters write only what a build owns.
func (db *DB) SetAppDetection(id, framework, entrypoint, startCmd string) error {
	_, err := db.conn.Exec(`
		UPDATE apps SET framework=?, entrypoint=?, start_cmd=?, updated_at=? WHERE id=?`,
		framework, entrypoint, startCmd, time.Now().UTC(), id)
	return err
}

// SetDeploymentDetection records what a build detected on the deployment it
// belongs to, beside the app row SetAppDetection writes.
//
// The app row is written minutes before the release serves and never reverted
// by a failed build, so a workload of the *serving* release that restarts
// meanwhile must not read it: the bootstrap and a restore read the workload's
// own deployment instead (ListDeploymentReleases).
func (db *DB) SetDeploymentDetection(id, framework, entrypoint, startCmd string) error {
	_, err := db.conn.Exec(`
		UPDATE deployments SET framework=?, entrypoint=?, start_cmd=? WHERE id=?`,
		framework, entrypoint, startCmd, id)
	return err
}

// SetDeploymentBasePath records the base path a release was built for.
func (db *DB) SetDeploymentBasePath(id, basePath string) error {
	_, err := db.conn.Exec(`UPDATE deployments SET base_path=? WHERE id=?`, basePath, id)
	return err
}

// ClearDeploymentSource makes a rollback row an ordinary release, and leaves
// restored_from naming what it restored. A rollback
// across base paths is rebuilt into its own directory (builder.Reseat), and a
// row still naming a source is read everywhere as owning no tree — so retention
// would never count it, and rollback would never offer it.
func (db *DB) ClearDeploymentSource(id string) error {
	_, err := db.conn.Exec(`UPDATE deployments SET source_deploy_id='' WHERE id=?`, id)
	return err
}

// SetAppRelease records the release a deploy flipped to: where it listens,
// which release and backend serve it, the base path it was built for, and
// since when -- the release half of UpdateApp, for the same reason
// SetAppDetection exists. The base path is in this statement because it must
// change at the same instant as the release the proxy reaches.
func (db *DB) SetAppRelease(a *App) error {
	a.UpdatedAt = time.Now().UTC()
	_, err := db.conn.Exec(`
		UPDATE apps SET framework=?, entrypoint=?, start_cmd=?, port=?, endpoint_host=?,
			current_release=?, release_ref=?, running_mode=?, release_base_path=?, updated_at=?,
			serving_since = CASE WHEN current_release = ? THEN serving_since WHEN ? = '' THEN NULL ELSE ? END
		WHERE id=?
	`, a.Framework, a.Entrypoint, a.StartCmd, a.Port, a.EndpointHost,
		a.CurrentRelease, a.ReleaseRef, a.RunningMode, a.ReleaseBasePath, a.UpdatedAt,
		a.CurrentRelease, a.CurrentRelease, a.UpdatedAt, a.ID)
	return err
}

// SetAppEndpoint records where the backend says this app's workload listens.
// Kept off UpdateApp for the same reason branch and auto-deploy are: the
// builder holds a stale App across a long build, and the reconciler may move
// an app's address while that build is still running.
func (db *DB) SetAppEndpoint(id, host string, port int) error {
	var p any
	if port != 0 {
		p = port
	}
	_, err := db.conn.Exec(`UPDATE apps SET endpoint_host=?, port=?, updated_at=? WHERE id=?`,
		host, p, time.Now().UTC(), id)
	return err
}

// SetAppExecutionMode promotes an app to isolated mode or returns it to
// shared.
//
// A dedicated setter for the same reason branch and auto-deploy have one:
// UpdateApp is called by the builder mid-build with an App loaded before the
// build started, so anything it writes can be silently reverted by a build
// that overlaps the change. A promotion quietly undone by a concurrent deploy
// would be near-impossible to explain from the outside.
// The change and its audit entry are one transaction, as every
// authorization-relevant mutation is: "who moved this app, and when" must not
// be able to go missing because a second write failed.
func (db *DB) SetAppExecutionMode(id, from, to, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE apps SET execution_mode=?, updated_at=? WHERE id=?`,
		to, time.Now().UTC(), id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "app.execution_mode.change",
		TargetType: "app", TargetID: id,
		Before:    jsonStr(map[string]string{"execution_mode": from}),
		After:     jsonStr(map[string]string{"execution_mode": to}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAppMemoryLimit records one app's own memory ceiling.
//
// A dedicated setter for the reason SetAppExecutionMode has one — UpdateApp is
// called by the builder with an App loaded before the build started, so a
// change written through it can be silently reverted by a deploy that overlaps
// it — and audited for the reason that one is: this is a platform decision
// about somebody else's app, made by an administrator rather than its owner,
// and it decides how much of the machine that app may take.
//
// 0 is "not set" and hands the number back to the install's dial, never
// "uncapped". Both figures are in the record, so "who raised this and from
// what" is answerable from the audit log alone.
func (db *DB) SetAppMemoryLimit(id string, from, to int, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE apps SET memory_limit_mb=?, updated_at=? WHERE id=?`,
		to, time.Now().UTC(), id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "app.memory_limit.change",
		TargetType: "app", TargetID: id,
		Before:    jsonStr(map[string]int{"memory_limit_mb": from}),
		After:     jsonStr(map[string]int{"memory_limit_mb": to}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAppCPULimit records one app's own CPU limit, in millicores: an
// administrator's, audited, through its own setter for the reasons
// SetAppMemoryLimit gives. 0 hands the number back to the install's dial.
func (db *DB) SetAppCPULimit(id string, from, to int, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE apps SET cpu_limit_millicores=?, updated_at=? WHERE id=?`,
		to, time.Now().UTC(), id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "app.cpu_limit.change",
		TargetType: "app", TargetID: id,
		Before:    jsonStr(map[string]int{"cpu_limit_millicores": from}),
		After:     jsonStr(map[string]int{"cpu_limit_millicores": to}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAppFrameAncestors records who may put this app in an iframe.
//
// A dedicated setter for the reason SetAppExecutionMode has one — UpdateApp is
// called by the builder with an App loaded before the build started, so a
// change written through it can be silently reverted by a deploy that overlaps
// it — and audited for the reason SetAppVisibility is: this decides who may
// wrap this app in their own page, which is a statement about who may reach it
// and belongs in the same record as the rest of them.
func (db *DB) SetAppFrameAncestors(id string, from, to FrameAncestors, actorType, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE apps SET frame_ancestors=?, updated_at=? WHERE id=?`,
		to.String(), time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.frame_ancestors.change",
		TargetType: "app", TargetID: id,
		// The lists, not a count: "who may embed this app" is the question a
		// reader of this row is asking, and a number does not answer it.
		Before:    jsonStr(map[string]string{"frame_ancestors": from.String()}),
		After:     jsonStr(map[string]string{"frame_ancestors": to.String()}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAppListable records the owner's half of the install-wide `list` rung:
// whether this app appears in the listing of somebody who holds nothing on it.
//
// A dedicated setter for both of SetAppVisibility's reasons. The builder holds
// an App loaded before a long build started, so a value carried on UpdateApp's
// SET list can be silently reverted by a deploy that overlaps the owner's
// change — and this is an authorization-relevant mutation, so the row and the
// audit entry that says who made it are one transaction.
//
// The caller decides that this is a change. A PATCH that re-sends the value the
// app already has records nothing, because "who changed who may see that this
// app exists" is a question an identical before and after cannot answer.
//
// Its own action rather than a field on app.visibility_change: the two are
// different disclosures — one is who may *open* the app, the other is who may
// know it is there — and an audit reader looking for the second would have to
// open every row of the first to find out whether it was one.
func (db *DB) SetAppListable(id string, from, to bool, actorType, actorID, requestID, ip, userAgent, apiKeyID string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE apps SET listable=?, updated_at=? WHERE id=?`,
		to, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	// A row that was not there is a change that did not happen, and the audit
	// row is what would say otherwise.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.listable_change",
		TargetType: "app", TargetID: id,
		Before:    jsonStr(map[string]bool{"listable": from}),
		After:     jsonStr(map[string]bool{"listable": to}),
		RequestID: requestID, IP: ip, UserAgent: userAgent, APIKeyID: apiKeyID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAppVisibility moves an app between the rungs of the ladder.
//
// A dedicated setter for the reason SetAppExecutionMode has one, plus a second
// that is this handler's own: PATCH /apps/{appID} used to write the audit row
// in a transaction that committed *before* UpdateApp ran, so a failed update
// left behind a record of a change that never happened. The write and the row
// that says who made it are one transaction here, as every
// authorization-relevant mutation is.
//
// The caller decides that this is a change; a call whose from and to are equal
// would record a change nobody made.
//
// actorType comes from api.auditActor rather than being assumed to be a person.
// A promotion carries visibility to the destination and is performed by a deploy
// key, which belongs to nobody: recording that as a user with an empty id would
// be the attribution collapse ADR §51 exists to avoid.
func (db *DB) SetAppVisibility(id, from, to, actorType, actorID, requestID, ip, userAgent, apiKeyID string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE apps SET visibility=?, updated_at=? WHERE id=?`,
		to, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	// A row that was not there is a change that did not happen, and the audit
	// row is what would say otherwise. Rolled back rather than reported as a
	// success with nothing behind it.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.visibility_change",
		TargetType: "app", TargetID: id,
		Before:    jsonStr(map[string]string{"visibility": from}),
		After:     jsonStr(map[string]string{"visibility": to}),
		RequestID: requestID, IP: ip, UserAgent: userAgent, APIKeyID: apiKeyID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAppBranch changes the tracked branch. If auto-deploy is on, the poller
// will pick up the new branch's tip on its next pass.
func (db *DB) SetAppBranch(id, branch string) error {
	_, err := db.conn.Exec(`UPDATE apps SET branch=?, updated_at=? WHERE id=?`, branch, time.Now().UTC(), id)
	return err
}

// SetAppRepoSubdir changes the folder of the repository the app is built
// from. It clears last_seen_sha and last_seen_tree for the same reason
// SetAppRepoURL does: the commit the poller last saw built a different folder,
// so the next pass should not treat the tip as already deployed.
func (db *DB) SetAppRepoSubdir(id, subdir string) error {
	_, err := db.conn.Exec(`UPDATE apps SET repo_subdir=?, last_seen_sha='', last_seen_tree='', updated_at=? WHERE id=?`, subdir, time.Now().UTC(), id)
	return err
}

// SetAutoDeploy toggles auto-deploy. Enabling seeds last_seen_sha (normally
// with the last successfully built commit) so the toggle itself doesn't fire a
// deploy unless the remote has actually moved on. It clears last_seen_tree, so
// the first push after enabling builds even if it leaves the app's folder as
// it was: the seed is a commit, and nothing says its tree is what is serving.
func (db *DB) SetAutoDeploy(id string, enabled bool, seedSHA string) error {
	if enabled {
		_, err := db.conn.Exec(`UPDATE apps SET auto_deploy=TRUE, last_seen_sha=?, last_seen_tree='', updated_at=? WHERE id=?`, seedSHA, time.Now().UTC(), id)
		return err
	}
	_, err := db.conn.Exec(`UPDATE apps SET auto_deploy=FALSE, updated_at=? WHERE id=?`, time.Now().UTC(), id)
	return err
}

// SetGitToken stores the app's encrypted clone credential; empty clears it. An
// audit entry, when given, is written in the same transaction as the change.
func (db *DB) SetGitToken(id, encryptedToken string, audit ...AuditEntry) error {
	return db.execAudited(audit, `UPDATE apps SET git_token=?, updated_at=? WHERE id=?`,
		encryptedToken, time.Now().UTC(), id)
}

// ListAutoDeployApps returns the apps the poller must check: auto-deploy on
// and an actual git remote (uploaded sources have nothing to poll).
func (db *DB) ListAutoDeployApps() ([]App, error) {
	rows, err := db.conn.Query(`SELECT ` + appColumns + `
		FROM apps WHERE apps.auto_deploy AND apps.repo_url LIKE 'https://%' ORDER BY apps.created_at`)
	if err != nil {
		return nil, err
	}
	return scanAppRows(rows)
}

// ClaimAutoDeploySHA advances last_seen_sha from oldSHA to newSHA and reports
// whether this caller won. The WHERE clause on the old value is the whole
// multi-node story: every node observes the same push, exactly one row update
// succeeds, and only that node enqueues a build.
func (db *DB) ClaimAutoDeploySHA(appID, oldSHA, newSHA string) (bool, error) {
	res, err := db.conn.Exec(`
		UPDATE apps SET last_seen_sha=?, updated_at=? WHERE id=? AND last_seen_sha=?
	`, newSHA, time.Now().UTC(), appID, oldSHA)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetLastSeen records the commit a successful build produced, so the poller
// doesn't re-deploy a tip that a manual deploy already shipped, and — for an
// app built from a folder — that folder's tree at it, so a later commit that
// leaves the folder alone is claimed without a build. tree is "" for an app at
// the repository root.
func (db *DB) SetLastSeen(appID, sha, tree string) error {
	_, err := db.conn.Exec(`UPDATE apps SET last_seen_sha=?, last_seen_tree=?, updated_at=? WHERE id=?`, sha, tree, time.Now().UTC(), appID)
	return err
}

// AdvanceLastSeen is SetLastSeen for a finishing build: it writes only while
// last_seen_sha is still the value the build read when it cloned, or already
// its own commit. A claim the poller made after the clone — a newer push — is
// never rewound to this build's older commit, which the next poll would then
// claim and build a second time (security review 2026-11-14, DG-13).
func (db *DB) AdvanceLastSeen(appID, sha, tree, seenAtClone string) error {
	_, err := db.conn.Exec(`UPDATE apps SET last_seen_sha=?, last_seen_tree=?, updated_at=?
		WHERE id=? AND last_seen_sha IN (?, ?)`, sha, tree, time.Now().UTC(), appID, seenAtClone, sha)
	return err
}

// SetDeploymentCommit records the commit a build cloned on its row the moment
// it is known, so the poller can see that an in-flight build already covers a
// tip. Narrow on purpose: UpdateDeployment rewrites the log.
func (db *DB) SetDeploymentCommit(id, sha string) error {
	_, err := db.conn.Exec(`UPDATE deployments SET commit_sha=? WHERE id=?`, sha, id)
	return err
}

// UpdateAppStatus writes status, and status_changed_at only when the value
// actually differs: the builder writes `running` twice around a flip, and a
// clock that moved on the second would date the status from nothing.
func (db *DB) UpdateAppStatus(id, status string) error {
	now := time.Now().UTC()
	_, err := db.conn.Exec(`
		UPDATE apps SET status=?, updated_at=?,
			status_changed_at = CASE WHEN status = ? THEN status_changed_at ELSE ? END
		WHERE id=?`, status, now, status, now, id)
	return err
}

// UpdateAppStatusFrom writes status only while the row still says from, and
// reports whether it moved. It is the write for anything that acts on a
// snapshot — the health watcher and the idle reaper read every app once per
// sweep and write minutes of work later — so a person's Stop or Restart that
// lands in between is never overwritten: `stopped` is somebody's decision.
func (db *DB) UpdateAppStatusFrom(id, from, status string) (bool, error) {
	now := time.Now().UTC()
	res, err := db.conn.Exec(`
		UPDATE apps SET status=?, updated_at=?,
			status_changed_at = CASE WHEN status = ? THEN status_changed_at ELSE ? END
		WHERE id=? AND status=?`, status, now, status, now, id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkAppBuilding writes `building` at the start of a build and returns the
// status the app held before it, which it also records in status_before_build
// so the boot sweep can restore it when a restart kills the build. A row that
// already says `building` is another build's: its recorded before-state is
// kept and returned.
func (db *DB) MarkAppBuilding(id string) (string, error) {
	now := time.Now().UTC()
	var before string
	err := db.conn.QueryRow(`
		UPDATE apps SET
			status_before_build = CASE WHEN status = ? THEN status_before_build ELSE status END,
			status = ?, updated_at = ?,
			status_changed_at = CASE WHEN status = ? THEN status_changed_at ELSE ? END
		WHERE id=?
		RETURNING status_before_build`,
		AppStatusBuilding, AppStatusBuilding, now, AppStatusBuilding, now, id).Scan(&before)
	return before, err
}

// UpdateAppStatusUnless writes status unless the row says unless, and reports
// whether it moved. It is the deploy flip's write: two deploys of one app can
// overlap, so the row may say `building` or the live status the other flip
// wrote, and both must move — the one word that must survive is `stopped`,
// which a person's Stop or an administrator's lock wrote during the build
// (security review 2026-11-14, AP-2).
func (db *DB) UpdateAppStatusUnless(id, unless, status string) (bool, error) {
	now := time.Now().UTC()
	res, err := db.conn.Exec(`
		UPDATE apps SET status=?, updated_at=?,
			status_changed_at = CASE WHEN status = ? THEN status_changed_at ELSE ? END
		WHERE id=? AND status<>?`, status, now, status, now, id, unless)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (db *DB) DeleteApp(id string) error {
	_, err := db.conn.Exec(`DELETE FROM apps WHERE id=?`, id)
	return err
}

// DeleteAppTx removes an app and records that somebody did, in one transaction.
//
// Separate from DeleteApp rather than replacing it, because two callers undo a
// *failed create* with that one — an app that never finished being made, whose
// deletion is not an act anybody performed and would read in the log as an app
// having been removed minutes after it appeared.
//
// The audit row carries what the app *was*: an id on its own names nothing once
// the row it pointed at is gone, and the whole reason this record exists is to
// be readable after the thing it describes cannot be looked up. `audit_log` has
// no foreign keys precisely so that it survives this (see the schema comment on
// the table), which is what makes writing it in the same transaction safe.
func (db *DB) DeleteAppTx(a *App, audit AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM apps WHERE id=?`, a.ID); err != nil {
		return err
	}

	audit.Action = "app.delete"
	audit.TargetType = "app"
	audit.TargetID = a.ID
	audit.Before = jsonStr(map[string]string{
		"slug": a.Slug, "name": a.Name, "repo_url": a.RepoURL,
		"visibility": a.Visibility, "execution_mode": a.ExecutionMode,
	})
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) SlugExists(slug string) (bool, error) {
	var count int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM apps WHERE slug=?`, slug).Scan(&count)
	return count > 0, err
}

// --- Port allocation ---

// UsedPorts returns every port claimed by a persisted app. Choosing a free port
// from this set is the job of internal/ports, which also guards against
// concurrent allocation and ports still held by a draining process.
func (db *DB) UsedPorts() ([]int, error) {
	rows, err := db.conn.Query(`SELECT port FROM apps WHERE port IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var used []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		used = append(used, p)
	}
	return used, rows.Err()
}

// --- Env vars ---

// SetEnvVar stores one variable, refused with *EnvKeyFromCredentialError when
// an attached credential supplies the key. Checked under the app's row lock,
// which AttachCredential takes too, so the two writes of one key cannot both
// pass their checks (DB1-3).
func (db *DB) SetEnvVar(appID, key, encryptedValue string, sensitive bool) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockAppRow(tx, appID); err != nil {
		return err
	}
	from, err := envKeyFromCredential(tx, appID, key)
	if err != nil {
		return err
	}
	if from != "" {
		return &EnvKeyFromCredentialError{Key: key, Credential: from}
	}
	id := uuid.New().String()
	if _, err := tx.Exec(`
		INSERT INTO env_vars (id, app_id, key, value, sensitive)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(app_id, key) DO UPDATE SET value=excluded.value, sensitive=excluded.sensitive
	`, id, appID, key, encryptedValue, sensitive); err != nil {
		return err
	}
	return tx.Commit()
}

// AddEnvVarIfAbsent stores an env var only when the app has no value for the
// key, and reports whether a row was inserted. ON CONFLICT DO NOTHING rather
// than read-then-write, so "seeding never overwrites a set value" holds even
// against a concurrent save from the app's Settings tab. A key an attached
// credential supplies is refused with *EnvKeyFromCredentialError, under the
// app's row lock as in SetEnvVar: no write path, seeding included, stores a
// row the credential would silently shadow at start.
func (db *DB) AddEnvVarIfAbsent(appID, key, encryptedValue string, sensitive bool) (bool, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockAppRow(tx, appID); err != nil {
		return false, err
	}
	from, err := envKeyFromCredential(tx, appID, key)
	if err != nil {
		return false, err
	}
	if from != "" {
		return false, &EnvKeyFromCredentialError{Key: key, Credential: from}
	}
	id := uuid.New().String()
	res, err := tx.Exec(`
		INSERT INTO env_vars (id, app_id, key, value, sensitive)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(app_id, key) DO NOTHING
	`, id, appID, key, encryptedValue, sensitive)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// EnvSet is one variable an ApplyEnvChanges call sets. A nil Value changes
// only the flag, and only towards sensitive: a stored secret is never revealed
// by flipping it. A nil Sensitive keeps a stored row's flag and makes a new
// row visible: which variables are secrets is the caller's decision.
type EnvSet struct {
	Value     *string
	Sensitive *bool
}

// EnvCipher is the encryptor ApplyEnvChanges compares and stores through. The
// comparison needs plaintext — a fresh nonce makes two ciphertexts of one
// value differ — and it has to happen under the lock, so it happens here.
type EnvCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(record string) (string, error)
}

// EnvChangeResult is what ApplyEnvChanges did, per key and never a value.
// Set is what was written; a key sent with the value and flag it already had
// is Unchanged. Changed counts rows written or deleted.
type EnvChangeResult struct {
	Set        []string `json:"set"`
	Unchanged  []string `json:"unchanged"`
	Kept       []string `json:"kept"`
	Cleared    []string `json:"cleared"`
	Unset      []string `json:"unset"`
	NotPresent []string `json:"not_present"`
	Changed    int      `json:"-"`
}

// EnvChangeError refuses one key of an ApplyEnvChanges body; nothing is
// written. Code is the API's: sensitive_needs_value, value_required.
type EnvChangeError struct {
	Key, Code, Message string
}

func (e *EnvChangeError) Error() string { return e.Message }

// ApplyEnvChanges is the one write to an existing app's environment: the keys
// in set are written, the keys in unset deleted, and every other key left
// alone. With onlyMissing a stored key is kept rather than overwritten — unless
// it is empty — decided
// here per key under the app's row lock, so "set it unless already set" is not
// a race a client loses. One transaction for the whole body, the env_version
// bump and the audit row: a credential-supplied key or a refused flag change
// rolls back every key, because a half-landed rotation is worse than none.
// The audit row, written only when something changed, names keys and flags.
func (db *DB) ApplyEnvChanges(appID string, set map[string]EnvSet, unset []string, onlyMissing bool, c EnvCipher, audit AuditEntry) (EnvChangeResult, error) {
	res := EnvChangeResult{Set: []string{}, Unchanged: []string{}, Kept: []string{}, Cleared: []string{}, Unset: []string{}, NotPresent: []string{}}
	tx, err := db.conn.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	if err := lockAppRow(tx, appID); err != nil {
		return res, err
	}

	type written struct {
		Key       string `json:"key"`
		Sensitive bool   `json:"sensitive"`
	}
	var wrote []written

	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the same refusal for the same request, every time
	for _, k := range keys {
		ch := set[k]
		from, err := envKeyFromCredential(tx, appID, k)
		if err != nil {
			return res, err
		}
		if from != "" {
			return res, &EnvKeyFromCredentialError{Key: k, Credential: from}
		}
		var curValue string
		var curSensitive bool
		err = tx.QueryRow(`SELECT value, sensitive FROM env_vars WHERE app_id=? AND key=?`, appID, k).Scan(&curValue, &curSensitive)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return res, err
		}
		// "Missing" is absent or empty — which is what a toft.toml [env]
		// variable with no default is until somebody fills it. Without the
		// second half, filling a new install's declared variables with
		// --only-missing would keep every one of them.
		if exists && onlyMissing {
			was, err := c.Decrypt(curValue)
			if err != nil {
				return res, err
			}
			if was != "" {
				res.Kept = append(res.Kept, k)
				continue
			}
		}
		sensitive := false
		if exists {
			sensitive = curSensitive
		}
		if ch.Sensitive != nil {
			sensitive = *ch.Sensitive
		}

		var plain string
		switch {
		case ch.Value != nil:
			plain = *ch.Value
		case !exists:
			return res, &EnvChangeError{Key: k, Code: "value_required",
				Message: k + " is not set on this app; send a value to create it"}
		case curSensitive && !sensitive:
			return res, &EnvChangeError{Key: k, Code: "sensitive_needs_value",
				Message: k + " is sensitive and its stored value is never shown; send a new value to make it visible"}
		default:
			if plain, err = c.Decrypt(curValue); err != nil {
				return res, err
			}
		}

		if exists && sensitive == curSensitive {
			if was, err := c.Decrypt(curValue); err == nil && was == plain {
				res.Unchanged = append(res.Unchanged, k)
				continue
			}
		}
		encrypted, err := c.Encrypt(plain)
		if err != nil {
			return res, err
		}
		if _, err := tx.Exec(`
			INSERT INTO env_vars (id, app_id, key, value, sensitive)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(app_id, key) DO UPDATE SET value=excluded.value, sensitive=excluded.sensitive
		`, uuid.New().String(), appID, k, encrypted, sensitive); err != nil {
			return res, err
		}
		if exists && ch.Value != nil && plain == "" {
			res.Cleared = append(res.Cleared, k)
		} else {
			res.Set = append(res.Set, k)
		}
		wrote = append(wrote, written{Key: k, Sensitive: sensitive})
		res.Changed++
	}

	sortedUnset := append([]string(nil), unset...)
	sort.Strings(sortedUnset)
	for _, k := range sortedUnset {
		r, err := tx.Exec(`DELETE FROM env_vars WHERE app_id=? AND key=?`, appID, k)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Unset = append(res.Unset, k)
			res.Changed++
		} else {
			res.NotPresent = append(res.NotPresent, k)
		}
	}

	if res.Changed > 0 {
		if _, err := tx.Exec(`UPDATE apps SET env_version = env_version + ? WHERE id = ?`, res.Changed, appID); err != nil {
			return res, err
		}
		if wrote == nil {
			wrote = []written{}
		}
		audit.After = jsonStr(map[string]any{"set": wrote, "unset": res.Unset})
		if err := writeAuditTx(tx, audit); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

func (db *DB) GetEnvVars(appID string) ([]EnvVar, error) {
	// Ordered so the settings form renders the same list on every poll —
	// heap order shuffles after updates, and the UI diffs the response.
	rows, err := db.conn.Query(`SELECT id, app_id, key, value, sensitive FROM env_vars WHERE app_id=? ORDER BY key`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vars []EnvVar
	for rows.Next() {
		var v EnvVar
		if err := rows.Scan(&v.ID, &v.AppID, &v.Key, &v.Value, &v.Sensitive); err != nil {
			return nil, err
		}
		vars = append(vars, v)
	}
	return vars, rows.Err()
}

func (db *DB) DeleteEnvVar(appID, key string) error {
	_, err := db.conn.Exec(`DELETE FROM env_vars WHERE app_id=? AND key=?`, appID, key)
	return err
}

func (db *DB) DeleteAllEnvVars(appID string) error {
	_, err := db.conn.Exec(`DELETE FROM env_vars WHERE app_id=?`, appID)
	return err
}

// --- Deployments ---

// ActionAppDeploy is a build that brings code, written in the transaction
// that creates its deployment: who started it and how the code came in.
const ActionAppDeploy = "app.deploy"

// DeployRecord is who started a build and how its code arrived. Via is git,
// upload, connect or gallery. A system actor (auto-deploy, a scheduled
// re-render) has no ActorID: a push came from someone Toft never
// authenticated, and a schedule fires without anyone.
type DeployRecord struct {
	ActorType, ActorID                 string
	RequestID, IP, UserAgent, APIKeyID string
	Via                                string
	// CommitSHA is the commit the starter already knows, when it does — only
	// an auto-deploy, which claimed it. Elsewhere the clone resolves it later
	// and the row names the deployment instead.
	CommitSHA string
}

// CreateDeployment inserts a deployment, and with a record writes app.deploy in
// the same transaction, so a build never exists without the row saying who
// started it. The record is a required argument so a door added later has to
// decide: nil only for the three that build no new code — a rollback, whose
// app.release_promoted names the actor, a move, whose actor is on the audit
// row of the address change that queued it, and a refused auto-deploy, which
// records a push that was not built.
func (db *DB) CreateDeployment(d *Deployment, rec *DeployRecord) error {
	d.ID = uuid.New().String()
	now := time.Now().UTC()
	d.StartedAt = &now
	if d.TriggeredBy == "" {
		d.TriggeredBy = DeployTriggerManual
	}
	// The builder decides the mode when it starts the release and writes it
	// back; until then a row reads as shared, which is what it will be on every
	// install that offers nothing else.
	if d.ExecutionMode == "" {
		d.ExecutionMode = string(runtime.ModeShared)
	}
	const insert = `
		INSERT INTO deployments (id, app_id, commit_sha, branch, status, triggered_by, execution_mode, build_log, artifact_key, gallery_version, source_deploy_id, restored_from, started_at, deployed_by_type, deployed_by_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	// A rollback's restored_from is its source at queue, and outlives it.
	if d.RestoredFrom == "" {
		d.RestoredFrom = d.SourceDeployID
	}
	args := []any{d.ID, d.AppID, d.CommitSHA, d.Branch, d.Status, d.TriggeredBy, d.ExecutionMode, d.BuildLog, d.ArtifactKey, d.GalleryVersion, d.SourceDeployID, d.RestoredFrom, d.StartedAt, d.DeployedByType, d.DeployedByID}
	if rec == nil {
		_, err := db.conn.Exec(insert, args...)
		return err
	}
	after := map[string]string{
		"deployment_id": d.ID, "trigger": d.TriggeredBy, "via": rec.Via, "branch": d.Branch,
	}
	if sha := d.CommitSHA; sha != "" {
		after["commit_sha"] = sha
	} else if rec.CommitSHA != "" {
		after["commit_sha"] = rec.CommitSHA
	}
	return db.execAudited([]AuditEntry{{
		ActorType: rec.ActorType, ActorID: rec.ActorID,
		Action: ActionAppDeploy, TargetType: "app", TargetID: d.AppID,
		After:     jsonStr(after),
		RequestID: rec.RequestID, IP: rec.IP, UserAgent: rec.UserAgent, APIKeyID: rec.APIKeyID,
	}}, insert, args...)
}

func (db *DB) GetDeployment(id string) (*Deployment, error) {
	d := &Deployment{}
	err := db.conn.QueryRow(`
		SELECT id, app_id, commit_sha, branch, status, triggered_by, execution_mode, build_log, artifact_key, gallery_version, runtime_language, runtime_version, disk_bytes, artifact_bytes, current_step, step_started_at, step_timings, started_at, finished_at, failure_reason, failure_code, failure_message, warnings, source_deploy_id, restored_from, base_path
		FROM deployments WHERE id = ?
	`, id).Scan(&d.ID, &d.AppID, &d.CommitSHA, &d.Branch, &d.Status, &d.TriggeredBy, &d.ExecutionMode, &d.BuildLog, &d.ArtifactKey, &d.GalleryVersion, &d.RuntimeLanguage, &d.RuntimeVersion, &d.DiskBytes, &d.ArtifactBytes, &d.CurrentStep, &d.StepStartedAt, &d.StepTimings, &d.StartedAt, &d.FinishedAt, &d.FailureReason, &d.FailureCode, &d.FailureMessage, &d.Warnings, &d.SourceDeployID, &d.RestoredFrom, &d.BasePath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	d.Phase = string(deploysteps.PhaseOf(d.CurrentStep))
	return d, err
}

func (db *DB) ListDeployments(appID string) ([]Deployment, error) {
	rows, err := db.conn.Query(`
		SELECT id, app_id, commit_sha, branch, status, triggered_by, execution_mode, build_log, artifact_key, gallery_version, runtime_language, runtime_version, disk_bytes, artifact_bytes, current_step, step_started_at, step_timings, started_at, finished_at, failure_reason, failure_code, failure_message, warnings, source_deploy_id, restored_from, base_path
		FROM deployments WHERE app_id=? ORDER BY started_at DESC
	`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deps []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.AppID, &d.CommitSHA, &d.Branch, &d.Status, &d.TriggeredBy, &d.ExecutionMode, &d.BuildLog, &d.ArtifactKey, &d.GalleryVersion, &d.RuntimeLanguage, &d.RuntimeVersion, &d.DiskBytes, &d.ArtifactBytes, &d.CurrentStep, &d.StepStartedAt, &d.StepTimings, &d.StartedAt, &d.FinishedAt, &d.FailureReason, &d.FailureCode, &d.FailureMessage, &d.Warnings, &d.SourceDeployID, &d.RestoredFrom, &d.BasePath); err != nil {
			return nil, err
		}
		d.Phase = string(deploysteps.PhaseOf(d.CurrentStep))
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// DeploymentRelease is the little a caller needs to resolve a *workload* to the
// release it belongs to: which deployment it is, what that release runs as, and
// whether its artifact was published.
//
// Narrow on purpose. Every deployment row carries a build log, and both callers
// — the runner bootstrap and `/api/v1/app/self` — do this lookup per request
// from a workload, where dragging every log of every release of an app through
// the connection to read two short columns is the whole cost of the query.
type DeploymentRelease struct {
	ID            string
	ExecutionMode string
	ArtifactBytes *int64

	// What this release's build detected (SetDeploymentDetection). Framework
	// empty means a deployment made before the columns existed, and the
	// caller falls back to the app row.
	Framework  string
	Entrypoint string
	StartCmd   string
	// BasePath is the path this release was built for; nil on a row from
	// before the column, where the caller falls back to the app row.
	BasePath *string
}

// ListDeploymentReleases is an app's deployments, newest first, in that narrow
// shape. Newest first because a workload name matches exactly one of them and
// the current release is the overwhelmingly likely match.
func (db *DB) ListDeploymentReleases(appID string) ([]DeploymentRelease, error) {
	rows, err := db.conn.Query(`
		SELECT id, execution_mode, artifact_bytes, framework, entrypoint, start_cmd, base_path
		FROM deployments WHERE app_id=? ORDER BY started_at DESC
	`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeploymentRelease
	for rows.Next() {
		var d DeploymentRelease
		if err := rows.Scan(&d.ID, &d.ExecutionMode, &d.ArtifactBytes, &d.Framework, &d.Entrypoint, &d.StartCmd, &d.BasePath); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// HasPendingDeployment reports whether a deployment is queued but not yet
// picked up by a build worker. The `since` cutoff keeps rows orphaned by a
// process restart (the build queue is in-memory) from blocking deploys forever.
func (db *DB) HasPendingDeployment(appID string, since time.Time) (bool, error) {
	var n int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM deployments WHERE app_id=? AND status=? AND started_at > ?
	`, appID, DeployStatusPending, since).Scan(&n)
	return n > 0, err
}

// InFlightDeployments is what the auto-deploy poller asks before claiming a
// tip: whether a build is queued or has not cloned yet (either will take the
// tip when it clones), and the commits the running builds have cloned. A
// building row is in-flight work too — counting only pending rows let a tip
// that a running build was already deploying be claimed and built again
// (security review 2026-11-14, DG-13).
func (db *DB) InFlightDeployments(appID string, since time.Time) (uncloned bool, building []string, err error) {
	rows, err := db.conn.Query(`
		SELECT status, COALESCE(commit_sha, '') FROM deployments
		WHERE app_id=? AND status IN (?, ?) AND started_at > ?`,
		appID, DeployStatusPending, DeployStatusBuilding, since)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status, sha string
		if err := rows.Scan(&status, &sha); err != nil {
			return false, nil, err
		}
		if status == DeployStatusPending || sha == "" {
			uncloned = true
			continue
		}
		building = append(building, sha)
	}
	return uncloned, building, rows.Err()
}

// QueuedDeployment is one build that has not finished, with enough of its app
// to render a row. The queue itself is an in-memory channel inside the
// builder, so this is the queue as the database can see it: every deployment
// still `pending` or `building`, oldest first, which is the order the single
// worker will take them in.
type QueuedDeployment struct {
	ID         string `json:"id"`
	AppID      string `json:"app_id"`
	AppName    string `json:"app_name"`
	AppSlug    string `json:"app_slug"`
	OwnerLogin string `json:"owner_login,omitempty"`
	// The owning principal, so a group-owned app is not rendered as having no
	// owner at all — which is the one thing the ownership model says is
	// impossible. `AppOwner` on the client renders either from these.
	OwnerID      string     `json:"owner_id,omitempty"`
	OwnerGroupID string     `json:"owner_group_id,omitempty"`
	Status       string     `json:"status"`
	TriggeredBy  string     `json:"triggered_by"`
	CommitSHA    string     `json:"commit_sha,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	EnqueuedAt   *time.Time `json:"enqueued_at"`
}

// ListActiveDeployments returns every unfinished build across the estate.
func (db *DB) ListActiveDeployments() ([]QueuedDeployment, error) {
	rows, err := db.conn.Query(`
		SELECT d.id, d.app_id, a.name, a.slug, COALESCE(u.login, ''),
		       COALESCE(a.owner_user_id, ''), COALESCE(a.owner_group_id, ''),
		       d.status, d.triggered_by, d.commit_sha, d.branch, d.started_at
		FROM deployments d
			JOIN apps a ON a.id = d.app_id
			LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE d.status IN (?, ?)
		ORDER BY d.started_at`,
		DeployStatusPending, DeployStatusBuilding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []QueuedDeployment
	for rows.Next() {
		var q QueuedDeployment
		if err := rows.Scan(&q.ID, &q.AppID, &q.AppName, &q.AppSlug, &q.OwnerLogin,
			&q.OwnerID, &q.OwnerGroupID,
			&q.Status, &q.TriggeredBy, &q.CommitSHA, &q.Branch, &q.EnqueuedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// FailOrphanedDeployments marks every deployment still pending or building as
// failed and settles the owning apps out of `building`. Called once at boot,
// before the API can queue new work: the build queue is in-memory, so a row in
// either state after a restart belongs to a process that no longer exists —
// nothing will ever finish it, the app shows "building" forever, and a stuck
// pending row holds the one-pending-deployment slot against real deploys.
// Returns how many rows were swept.
func (db *DB) FailOrphanedDeployments() (int, error) {
	now := time.Now().UTC()
	rows, err := db.conn.Query(`
		UPDATE deployments
		SET status=?, finished_at=?, build_log = COALESCE(build_log,'') || ?
		WHERE status IN (?, ?)
		RETURNING app_id
	`, DeployStatusFailed, now, "\nERROR: interrupted by a platform restart — deploy again.",
		DeployStatusPending, DeployStatusBuilding)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	swept := 0
	appIDs := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return swept, err
		}
		swept++
		appIDs[id] = true
	}
	if err := rows.Err(); err != nil {
		return swept, err
	}

	// Close to the rule a build failure follows (restoredStatus): an app with
	// no release has nothing to fall back to; one that was stopped or asleep
	// before the build (status_before_build, written by MarkAppBuilding) is
	// again, so boot Reconcile and the supervisor leave it alone; any other is
	// still running its live release. `crashed` is not restored: at boot
	// nothing restarts a crashed isolated row, while `running` is re-detected
	// by the watcher. Only rows the dead build left in `building` are touched.
	for id := range appIDs {
		_, err := db.conn.Exec(`
			UPDATE apps
			SET status = CASE WHEN current_release = '' THEN ?
			                  WHEN status_before_build IN (?, ?) THEN status_before_build
			                  WHEN framework LIKE 'batch-%' THEN ? ELSE ? END, updated_at=?, status_changed_at=?
			WHERE id=? AND status=?
		`, AppStatusFailed, AppStatusSleeping, AppStatusStopped, AppStatusReady, AppStatusRunning, now, now, id, AppStatusBuilding)
		if err != nil {
			return swept, err
		}
	}
	return swept, nil
}

// LatestSuccessfulSHA returns the commit of the app's newest successful
// deployment, or "" if it has never built. Used to seed last_seen_sha when
// auto-deploy is switched on.
func (db *DB) LatestSuccessfulSHA(appID string) (string, error) {
	var sha string
	err := db.conn.QueryRow(`
		SELECT commit_sha FROM deployments WHERE app_id=? AND status=? ORDER BY started_at DESC LIMIT 1
	`, appID, DeployStatusSuccess).Scan(&sha)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sha, err
}

// UpdateDeployment writes a deployment's status, log and outcome.
//
// The log is cleaned on the way in (textutil.Clean): it is tool output, and one
// NUL byte or invalid UTF-8 sequence in it failed the whole statement, so the
// status and finished_at never landed either (security review 2026-11-14,
// PB-2). SetDeploymentFailure cleans its message for the same reason.
func (db *DB) UpdateDeployment(d *Deployment) error {
	_, err := db.conn.Exec(`
		UPDATE deployments SET status=?, build_log=?, commit_sha=?, artifact_key=?, execution_mode=?, finished_at=? WHERE id=?
	`, d.Status, textutil.Clean(d.BuildLog), d.CommitSHA, d.ArtifactKey, d.ExecutionMode, d.FinishedAt, d.ID)
	return err
}

// SetDeploymentSizes records what a release costs. It is a dedicated setter for
// the same reason SetAppBranch is: the builder calls UpdateDeployment several
// times with a row it has been holding since before the build, and folding
// sizes into that write would null out a measurement taken concurrently.
//
// A nil argument means "not measured now" and leaves the stored value alone, so
// the disk walk and the artifact Put can report independently and in either
// order.
func (db *DB) SetDeploymentSizes(id string, diskBytes, artifactBytes *int64) error {
	_, err := db.conn.Exec(`
		UPDATE deployments
		SET disk_bytes = COALESCE(?, disk_bytes), artifact_bytes = COALESCE(?, artifact_bytes)
		WHERE id = ?
	`, diskBytes, artifactBytes, id)
	return err
}

// SetDeploymentStep records which phase a build has reached, and when it got
// there.
//
// A dedicated setter for the same reason SetDeploymentSizes is one: the builder
// holds a Deployment struct from before the build started and writes it back
// several times, so folding the phase into UpdateDeployment would have each of
// those writes reset the phase to whatever the stale struct last held.
//
// Called nine times per build, from the builder's own goroutine. That is not
// the request path the "no writes on the hot path" rule is about — a build
// takes tens of seconds and already writes this row repeatedly.
func (db *DB) SetDeploymentStep(id, step string) error {
	_, err := db.conn.Exec(`
		UPDATE deployments SET current_step = ?, step_started_at = ? WHERE id = ?
	`, step, time.Now().UTC(), id)
	return err
}

// SetDeploymentFailure records why a deploy failed: the key a program branches
// on and the paragraph a person reads.
//
// A dedicated setter for SetDeploymentStep's reason — the builder holds a
// Deployment struct from before the build started and writes it back several
// times, so folding this into UpdateDeployment would have a later write reset
// it from a stale struct.
//
// Written **before** the terminal progress frame is published, so that a client
// which reconnects the instant the deploy ends reads the same answer from the
// row that the live frame carried. The other order would leave a window in
// which the frame says why and the snapshot does not, which is the exact bug
// these columns exist to close.
func (db *DB) SetDeploymentFailure(id, reason, code, message string) error {
	_, err := db.conn.Exec(`
		UPDATE deployments SET failure_reason = ?, failure_code = ?, failure_message = ? WHERE id = ?
	`, reason, code, textutil.Clean(message), id)
	return err
}

// SetDeploymentFailureOutput records what a start that failed printed, each
// line with its own time and stream (BB-038).
func (db *DB) SetDeploymentFailureOutput(id, output string) error {
	_, err := db.conn.Exec(`UPDATE deployments SET failure_output = ? WHERE id = ?`, output, id)
	return err
}

// SetDeploymentWarnings records what this deploy noticed and carried on past.
//
// A dedicated setter for the reason SetDeploymentFailure is one: the builder
// holds a Deployment struct from before the build started and writes it back
// several times, so folding this into UpdateDeployment would have a later write
// reset it from a stale copy.
//
// Written when the manifest is read, which is before the deploy can succeed or
// fail — so the warnings are on the row by the time either terminal frame is
// published, and a client that reconnects the instant a deploy ends reads the
// same answer the live frame carried.
func (db *DB) SetDeploymentWarnings(id, warnings string) error {
	_, err := db.conn.Exec(`UPDATE deployments SET warnings = ? WHERE id = ?`, warnings, id)
	return err
}

// SetDeploymentTimings records what each phase cost, once, when the build ends.
// The argument is the JSON the UI renders; this layer does not interpret it.
func (db *DB) SetDeploymentTimings(id, timings string) error {
	_, err := db.conn.Exec(`UPDATE deployments SET step_timings = ? WHERE id = ?`, timings, id)
	return err
}

// SetDeploymentRuntime records which language version a release was built
// against. Written once, during the deploy phase, before anything is compiled —
// so a build that then fails still says what it was going to use, which is
// exactly the case somebody is reading the row to understand.
func (db *DB) SetDeploymentRuntime(id, language, version string) error {
	_, err := db.conn.Exec(`
		UPDATE deployments SET runtime_language = ?, runtime_version = ? WHERE id = ?
	`, language, version, id)
	return err
}

// DeleteDeployment removes one deployment row, scoped to its app: the caller
// authorized against the app in the URL, so the app ID belongs in the query
// too. Reports whether a row was actually removed.
//
// The row is deletable at all because nothing references it. A rollback copies
// its source's branch and commit rather than pointing at it, so removing an old
// deployment leaves no dangling reference — only its build log is lost, which
// is the trade the caller is making on purpose.
func (db *DB) DeleteDeployment(appID, id string) (bool, error) {
	res, err := db.conn.Exec(`DELETE FROM deployments WHERE id=? AND app_id=?`, id, appID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeploymentsMissingSizes lists deployments that have never been measured, so
// the boot backfill knows what to look at. Failed builds are skipped: they
// published nothing and left no release directory to walk, so they would be
// re-examined fruitlessly on every boot.
func (db *DB) DeploymentsMissingSizes() ([]Deployment, error) {
	rows, err := db.conn.Query(`
		SELECT id, app_id, artifact_key
		FROM deployments
		WHERE status=? AND (disk_bytes IS NULL OR artifact_bytes IS NULL)
		ORDER BY started_at DESC
	`, DeployStatusSuccess)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deps []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.AppID, &d.ArtifactKey); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// --- The environment a workload was started with ---

// BumpEnvVersion records that n of an app's variables actually changed.
//
// Called once per save with the number the handler counted, rather than once
// per write: the settings form resubmits every row, and a counter bumped per
// write would report five un-applied changes for a save that changed nothing.
// The handler is also the only layer that *can* count — values are encrypted
// with a fresh nonce every time, so two ciphertexts of one value differ and the
// comparison has to happen where the plaintext is.
func (db *DB) BumpEnvVersion(appID string, n int) error {
	if n <= 0 {
		return nil
	}
	_, err := db.conn.Exec(`UPDATE apps SET env_version = env_version + ? WHERE id = ?`, n, appID)
	return err
}

// MarkEnvApplied records that the workload now running was started with the
// app's current variables. Called on every successful start — which since ADR
// §4 is the only kind there is — and never on a start that failed readiness,
// because the process that would be carrying those values is not serving.
func (db *DB) MarkEnvApplied(appID string) error {
	_, err := db.conn.Exec(`UPDATE apps SET env_applied_version = env_version WHERE id = ?`, appID)
	return err
}

// UnappliedEnvChanges is how many variable changes have been saved since the
// running workload was started. Zero when there is nothing pending, which is
// the normal answer and the one that renders no notice at all.
func (db *DB) UnappliedEnvChanges(appID string) (int, error) {
	var n int
	err := db.conn.QueryRow(
		`SELECT env_version - env_applied_version FROM apps WHERE id = ?`, appID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if n < 0 {
		// Not reachable by design, and cheap to refuse to render: a negative
		// count would read as a bug in the copy rather than in the data.
		return 0, nil
	}
	return n, nil
}

// MarkAppGarthLinked records that a deploy from a Garth session reached this
// app (ADR §158). The first one stamps the time and later ones leave it, so
// the mark says when the link began. A dedicated setter, not an UpdateApp
// field, for SetAppVisibility's reason: the builder holds a stale App.
func (db *DB) MarkAppGarthLinked(id string) error {
	_, err := db.conn.Exec(`UPDATE apps SET garth_linked_at=? WHERE id=? AND garth_linked_at IS NULL`, time.Now().UTC(), id)
	return err
}

// UnlinkAppFromGarth clears the mark, so the app page stops offering Open in
// Garth until the next deploy from a Garth session. Audited as
// `app.garth.unlink`; sql.ErrNoRows when there is no such app.
func (db *DB) UnlinkAppFromGarth(id, actorType, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE apps SET garth_linked_at=NULL, updated_at=? WHERE id=?`, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.garth.unlink",
		TargetType: "app", TargetID: id,
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// CountApps is every app row: the number the tier's app cap refuses on. A
// deleted app does not count; a locked, retired or stopped one does.
func (db *DB) CountApps() (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM apps`).Scan(&n)
	return n, err
}
