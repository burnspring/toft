package database

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrAppGone is the app being deleted underneath a writer that is out of the
// request path. It is an expected outcome rather than a failure: an app's
// failures are recorded asynchronously on purpose — *telling someone never
// fails the thing that broke* — so a watcher recording `app_crashed` for an app
// somebody deleted a moment ago is the design working, not the database.
//
// Named so the caller can say so at the right volume. Closing the race at the
// source would mean a delete waiting for the recorder to quiesce, which is the
// recording back in the request path, for a window in which the only thing lost
// is a record of something that no longer exists.
var ErrAppGone = errors.New("the app no longer exists")

// isAppForeignKeyViolation reports whether a write lost a race against a
// deletion. Checked through the driver's error type rather than by matching the
// message, the way isUniqueViolation matches 23505; app_id is the only foreign
// key on app_events, so the code alone is the answer here.
func isAppForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" &&
		strings.Contains(pgErr.ConstraintName, "app_id")
}

// App failure events — the durable record of everything that breaks in a
// hosted app, and the queries the recorder (internal/events) and the API read
// them back through.
//
// The table is deliberately separate from audit_log: that one is actor-driven
// and append-only for governance, these are machine observations that repeat.
// They are merged only at read time, in ListActivity.

// Event severities. Not an exhaustive taxonomy — three levels is what a list
// can usefully colour and what a reader can usefully act on.
const (
	EventSeverityError   = "error"
	EventSeverityWarning = "warning"
	EventSeverityInfo    = "info"
)

// Event sources: which subsystem noticed. Used as the actor label in the admin
// feed, where every one of these rows reads as "system:<source>".
const (
	EventSourceBuilder    = "builder"
	EventSourceAutodeploy = "autodeploy"
	EventSourceHealth     = "health"
	EventSourceJobs       = "jobs"
	// EventSourceIntegrations is a message the app itself asked to send that
	// did not go out. Only failures reach app_events: a successful send is not
	// an event about an app's health, and forty a day would drown the one
	// deploy failure this feed exists to surface. The full record of every
	// send, successful or not, is app_integration_sends.
	EventSourceIntegrations = "integrations"
	// EventSourceDependency is the dependency sweep re-judging a release that
	// is already serving, against a feed that moved after it was built. It is
	// not EventSourceBuilder: nothing was built, and the distinction is the
	// point of the row -- a finding here is about code that shipped clean and
	// has since been reclassified, which is the one advisory an owner cannot
	// have seen at deploy time.
	EventSourceDependency = "dependency"
)

// AppEvent is one occurrence of one problem on one app.
type AppEvent struct {
	ID       string `json:"id"`
	AppID    string `json:"app_id"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
	// DeploymentID points at the deployment this came out of, when there is
	// one. It may name a row that retention has since pruned — events outlive
	// deployments on purpose — so a reader must tolerate a dangling id.
	DeploymentID string    `json:"deployment_id,omitempty"`
	Fingerprint  string    `json:"fingerprint"`
	OccurredAt   time.Time `json:"occurred_at"`
	// NotifiedAt is the cooldown claim — this occurrence won the right to be
	// the one people are told about. NoticeSent is whether anyone actually was.
	// They differ whenever the app's notify_failures switch is off, and the
	// difference is what stops a recovery notice going out about a problem
	// nobody heard start.
	NotifiedAt *time.Time `json:"notified_at,omitempty"`
	NoticeSent bool       `json:"notice_sent"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

const appEventColumns = `id, app_id, kind, severity, source, summary, detail,
	COALESCE(deployment_id,''), fingerprint, occurred_at, notified_at, notice_sent, resolved_at`

func scanAppEvents(rows *sql.Rows) ([]AppEvent, error) {
	defer rows.Close()
	var out []AppEvent
	for rows.Next() {
		var e AppEvent
		if err := rows.Scan(&e.ID, &e.AppID, &e.Kind, &e.Severity, &e.Source, &e.Summary,
			&e.Detail, &e.DeploymentID, &e.Fingerprint, &e.OccurredAt, &e.NotifiedAt, &e.NoticeSent, &e.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// InsertAppEvent records one occurrence and returns its id.
//
// Every occurrence is inserted, including the ones no one will be emailed
// about. Collapsing happens in ClaimEventNotify, one layer up, and only ever
// affects mail.
//
// Returns ErrAppGone when the app was deleted underneath the writer, so a
// caller can tell that apart from the write failing — the platform being unable
// to record what it observed is the signal that matters, and this is not it.
func (db *DB) InsertAppEvent(e *AppEvent) error {
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.Severity == "" {
		e.Severity = EventSeverityError
	}
	var deployment interface{}
	if e.DeploymentID != "" {
		deployment = e.DeploymentID
	}
	_, err := db.conn.Exec(`
		INSERT INTO app_events (id, app_id, kind, severity, source, summary, detail, deployment_id, fingerprint, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AppID, e.Kind, e.Severity, e.Source, e.Summary, e.Detail, deployment, e.Fingerprint, e.OccurredAt)
	if isAppForeignKeyViolation(err) {
		return ErrAppGone
	}
	return err
}

// ClaimEventNotify takes the right to send mail about this event, and reports
// whether this caller won it.
//
// The guarded UPDATE is the same shape as ClaimAutoDeploySHA: every node
// observes the same failure, exactly one row update succeeds, and only that
// node sends. The cooldown is two guards on the same (app, kind, fingerprint)
// inside the window:
//
//   - an occurrence that was claimed and is **still unresolved** blocks the
//     claim: one persisting problem rediscovered every minute is mailed once;
//   - two claimed occurrences block it whatever their state: a problem that
//     recovered and came back is told once more (otherwise the owners' last
//     word would be "working again" while the app is broken), but an app that
//     flaps every minute sends at most two failure notices per window.
//
// A refused claim leaves the row unnotified; it is still on the record.
//
// The claim is taken whether or not the app's notify_failures switch is on, so
// turning the switch back on releases no backlog of suppressed mail.
func (db *DB) ClaimEventNotify(eventID, appID, kind, fingerprint string, cooldown time.Duration) (bool, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-cooldown)
	res, err := db.conn.Exec(`
		UPDATE app_events SET notified_at = ?
		WHERE id = ?
		  AND notified_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM app_events prior
		      WHERE prior.app_id = ? AND prior.kind = ? AND prior.fingerprint = ?
		        AND prior.notified_at IS NOT NULL
		        AND prior.notified_at > ?
		        AND prior.resolved_at IS NULL)
		  AND (SELECT COUNT(*) FROM app_events p
		      WHERE p.app_id = ? AND p.kind = ? AND p.fingerprint = ?
		        AND p.notified_at IS NOT NULL
		        AND p.notified_at > ?) < 2`,
		now, eventID, appID, kind, fingerprint, cutoff, appID, kind, fingerprint, cutoff)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkEventNoticeSent records that a message about this occurrence was
// actually handed to the mailer — as distinct from ClaimEventNotify, which only
// takes the cooldown claim.
func (db *DB) MarkEventNoticeSent(eventID string) error {
	_, err := db.conn.Exec(`UPDATE app_events SET notice_sent = true WHERE id = ?`, eventID)
	return err
}

// ResolveAppEvents closes every open event of this kind for the app and
// reports how many of them anyone was actually told about.
//
// That count is what decides whether a recovery notice goes out: nobody wants
// "it's better now" about a problem they were never told had started. It counts
// notice_sent rather than notified_at for exactly that reason — an app with its
// notification switch off still takes the claim, and still tells nobody.
//
// A non-empty fingerprintPrefix narrows the close to the events whose
// fingerprint starts with it. That is how one automation's success resolves its
// own failure and nobody else's: a job's fingerprint is `definitionID:stage` and
// a task's `taskID:class`, and closing on kind alone meant an hourly job that
// succeeded reported a nightly one that was still failing as recovered. Empty
// keeps the whole-kind meaning every app-level caller wants. Compared with
// left() rather than LIKE, so an id is never read as a pattern.
func (db *DB) ResolveAppEvents(appID, kind, fingerprintPrefix string) (notified int, err error) {
	now := time.Now().UTC()
	err = db.conn.QueryRow(`
		WITH closed AS (
			UPDATE app_events SET resolved_at = ?
			WHERE app_id = ? AND kind = ? AND resolved_at IS NULL
				AND (? = '' OR left(fingerprint, length(?)) = ?)
			RETURNING notice_sent
		)
		SELECT COUNT(*) FROM closed WHERE notice_sent`,
		now, appID, kind, fingerprintPrefix, fingerprintPrefix, fingerprintPrefix).Scan(&notified)
	return notified, err
}

// HasOpenAppEvent reports whether the app has an unresolved event of this kind
// — "is this problem still going on", asked of the database rather than of a
// worker's in-memory state.
func (db *DB) HasOpenAppEvent(appID, kind string) (bool, error) {
	var n int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM app_events
		WHERE app_id = ? AND kind = ? AND resolved_at IS NULL`, appID, kind).Scan(&n)
	return n > 0, err
}

// Page sizes for the two event read paths. Exported because the API answers
// with a "there is more" cursor, and it can only decide that honestly if it
// knows the size of the page it actually asked for.
const (
	DefaultAppEventLimit = 100
	MaxAppEventLimit     = 500
	DefaultActivityLimit = 50
	MaxActivityLimit     = 200
)

// clampLimit resolves a caller's page size. An over-large request is lowered to
// the maximum rather than reset to the default: asking for more than is allowed
// should get as much as is allowed, not less than was asked for.
func clampLimit(want, def, max int) int {
	switch {
	case want <= 0:
		return def
	case want > max:
		return max
	default:
		return want
	}
}

// AppEventFilter narrows an app's event list. Zero values mean "no filter".
type AppEventFilter struct {
	Kind     string
	Severity string
	Since    time.Time
	Limit    int
}

// ListAppEvents returns one app's events, newest first.
//
// Always scoped by appID, which is the IDOR rule: this is reached through
// /apps/{appID}/events and the path id is the only thing that decides which
// rows come back.
func (db *DB) ListAppEvents(appID string, f AppEventFilter) ([]AppEvent, error) {
	q := `SELECT ` + appEventColumns + ` FROM app_events WHERE app_id = ?`
	args := []interface{}{appID}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.Severity != "" {
		q += ` AND severity = ?`
		args = append(args, f.Severity)
	}
	if !f.Since.IsZero() {
		q += ` AND occurred_at >= ?`
		args = append(args, f.Since)
	}
	limit := clampLimit(f.Limit, DefaultAppEventLimit, MaxAppEventLimit)
	q += ` ORDER BY occurred_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanAppEvents(rows)
}

// AppNotifyFailures reports whether this app's people should be emailed.
//
// Read fresh at send time rather than off a cached App: the recorder is handed
// an app id by a background worker that may have loaded its App minutes ago,
// and this is the one question where a stale answer sends mail somebody just
// asked not to receive. A missing app reads as false — there is nobody to tell.
func (db *DB) AppNotifyFailures(appID string) (bool, error) {
	var on bool
	err := db.conn.QueryRow(`SELECT notify_failures FROM apps WHERE id = ?`, appID).Scan(&on)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return on, err
}

// SetAppNotifyFailures turns this app's failure email on or off.
//
// Its own setter for the same reason SetAppRetention is: the builder calls
// UpdateApp mid-build with an App it loaded before the build started, so a
// change written through that path could be silently reverted by an
// overlapping deploy.
//
// Audited by the caller in the same request rather than here — like
// never_sleep, this changes nothing about who may do what.
func (db *DB) SetAppNotifyFailures(id string, on bool) error {
	_, err := db.conn.Exec(`UPDATE apps SET notify_failures=?, updated_at=? WHERE id=?`,
		on, time.Now().UTC(), id)
	return err
}

// DeleteAppEventsBefore prunes events older than cutoff and returns how many
// went. Called from pruneAppEvents in main.go, which sweeps daily with a cutoff
// of ninety days — the retention lives there, not here, because this takes the
// cutoff it is given.
func (db *DB) DeleteAppEventsBefore(cutoff time.Time) (int64, error) {
	res, err := db.conn.Exec(`DELETE FROM app_events WHERE occurred_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Recipient is somewhere a notification about an app can be sent.
//
// It replaced []User when a group could start owning apps, because the set to
// tell stopped being a set of people: a group's address is a rota, an alias or
// a ticket queue, and there is no user row behind it. UserID is nil for exactly
// that case, and every caller only ever wanted somewhere to send.
type Recipient struct {
	Address     string
	DisplayName string
	// UserID is the person this address belongs to, or nil when the address is
	// a group's. Nobody sends on it today; it is here so that a caller which
	// later needs to say "and this one is a distribution list" can, without
	// re-deriving it from the address.
	UserID *string
}

// ListAppRecipients returns where to write when this app breaks: its owners and
// editors, as addresses.
//
// object_grants used to be complete on its own, and the reason it no longer is
// is worth stating, because the old comment here argued the opposite and was
// right at the time. It said owners and editors are always individual
// users, because group grants could only ever confer viewer. That clamp is gone
// (ADR §51), so a group can hold owner or editor, and a grant row naming
// a group joins no users row at all.
//
// So this is two queries with one rule between them:
//
//  1. every *user* who holds owner or editor, directly or through any
//     group they belong to; and
//  2. for each *group* holding owner or editor, its own address if it has
//     one — because that is the thing a team can point at a rota — and its
//     members' addresses if it does not, so a group that has not thought about
//     it still gets told.
//
// Viewers are excluded because they cannot fix anything, locked accounts
// because they cannot sign in to look, and empty addresses because there is
// nowhere to send. Those exclusions apply to members expanded from a group
// exactly as they do to a direct grant; a group's own address is not a person
// and is subject to none of them.
func (db *DB) ListAppRecipients(appID string) ([]Recipient, error) {
	return db.listAppRecipients(appID, ObjectRoleOwner, ObjectRoleEditor)
}

// ListAppDeciders returns where to write when somebody is waiting on a decision
// about this app: the people who can actually make it.
//
// The same two queries with one rung instead of two, and that narrowing is the
// whole of it. ActionApproveAccessRequest is owner-only (internal/auth), so an
// editor told that somebody has asked for access is a person sent a message
// about a button they do not have — and the honest reading of a queue nobody
// mailed can act on is that the notification is noise.
//
// An install-wide administrator is not here either. They can approve, through
// the short-circuit in Authorize, but they are not who the request was routed
// to: `approver_group_id` says the owning group, and a request for one app
// arriving in every administrator's inbox is how a useful notification becomes
// a filtered one.
func (db *DB) ListAppDeciders(appID string) ([]Recipient, error) {
	return db.listAppRecipients(appID, ObjectRoleOwner)
}

// listAppRecipients is both of the above: which rungs count is the only thing
// that differs, and two copies of this SQL would be two chances to fix the
// group-address rule in one of them.
func (db *DB) listAppRecipients(appID string, roles ...string) ([]Recipient, error) {
	// The rung list, as placeholders. Built rather than written out because the
	// callers differ in how many they pass; the values are this package's own
	// constants and never anything a request carried.
	in := strings.TrimSuffix(strings.Repeat("?,", len(roles)), ",")
	roleArgs := make([]interface{}, 0, len(roles))
	for _, r := range roles {
		roleArgs = append(roleArgs, r)
	}
	userArgs := append([]interface{}{SystemRoleViewer, BuiltinAdminEmail, appID}, roleArgs...)
	userArgs = append(userArgs, appID)
	userArgs = append(userArgs, roleArgs...)

	// Users: direct grants, plus membership of any group that holds a
	// write-bearing role. The UNION is what makes a member of an owning group
	// reachable — they hold no row of their own.
	rows, err := db.conn.Query(`
		SELECT DISTINCT u.id, COALESCE(u.name,''), u.login, COALESCE(u.email,'')
		FROM users u
		WHERE u.system_role <> ?
		  AND u.locked_at IS NULL
		  AND u.email IS NOT NULL AND u.email <> ''
		  AND u.email <> ?
		  AND (
		    EXISTS (
		      SELECT 1 FROM object_grants g
		      WHERE g.object_id = ? AND g.principal_type = 'user'
		        AND g.principal_id = u.id AND g.role IN (`+in+`)
		    )
		    OR EXISTS (
		      SELECT 1 FROM object_grants g
		      JOIN group_members m ON m.group_id = g.principal_id
		      JOIN groups gr ON gr.id = g.principal_id
		      WHERE g.object_id = ? AND g.principal_type = 'group'
		        AND g.role IN (`+in+`) AND m.user_id = u.id
		        AND gr.email = ''
		    )
		  )
		ORDER BY u.login`,
		userArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Recipient
	for rows.Next() {
		var id, name, login, email string
		if err := rows.Scan(&id, &name, &login, &email); err != nil {
			return nil, err
		}
		display := name
		if display == "" {
			display = login
		}
		userID := id
		out = append(out, Recipient{Address: email, DisplayName: display, UserID: &userID})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Groups that have an address of their own. The members of those groups
	// were excluded above by the `gr.email = ''` clause — an address instead of
	// the members, not as well as, or a thirty-person team gets thirty-one
	// copies of every failure.
	grows, err := db.conn.Query(`
		SELECT DISTINCT gr.email, gr.name
		FROM object_grants g
		JOIN groups gr ON gr.id = g.principal_id
		WHERE g.object_id = ? AND g.principal_type = 'group'
		  AND g.role IN (`+in+`)
		  AND gr.email <> ''
		ORDER BY gr.name`,
		append([]interface{}{appID}, roleArgs...)...)
	if err != nil {
		return nil, err
	}
	defer grows.Close()
	for grows.Next() {
		var email, name string
		if err := grows.Scan(&email, &name); err != nil {
			return nil, err
		}
		out = append(out, Recipient{Address: email, DisplayName: name})
	}
	return out, grows.Err()
}

// ActivityEntry is one row of the merged admin feed: an audit_log action or an
// app_events observation, flattened into a shape a single table can render.
type ActivityEntry struct {
	ID     string `json:"id"`
	Source string `json:"source"` // audit | event
	// ActorLabel is who or what did it — a user's login, "cli" for an operator
	// action, or "system:<source>" for a machine observation.
	ActorLabel string `json:"actor_label"`
	// ActorID is the id behind the label on an audit row that has one — a
	// user, an app or a deploy key — and what `?actor=` narrows the feed by.
	// A label is not: a login can be reused, and "cli" names no one account.
	// Empty on an event row and for an actor with no id.
	ActorID     string `json:"actor_id,omitempty"`
	Action      string `json:"action"`
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
	TargetLabel string `json:"target_label"`
	// TargetGone is true when the target is an app that no longer exists. The
	// label is then the slug its deletion recorded, or the id when even that
	// is missing — either way not something to link to: the page linked a
	// deleted app's UUID to /apps/<id>, a door onto a 404 on every such row.
	TargetGone bool `json:"target_gone,omitempty"`
	// AppID is whose history the row is part of, which is not always its
	// target: a denied access request targets the request. Empty for an
	// install-wide act and for a row from before attribution (migration 00071).
	AppID      string    `json:"app_id,omitempty"`
	Severity   string    `json:"severity"`
	Summary    string    `json:"summary"`
	Detail     string    `json:"detail"`
	OccurredAt time.Time `json:"occurred_at"`

	// The request-scoped half of an audit row: where the call came from and
	// which credential made it. Recorded since the table existed and read by
	// nothing until now, which made an install able to answer "who changed
	// this" but not "from where".
	//
	// All five are empty on an `event` row. An observation has no request
	// behind it, and inventing an address for one would be a lie in the one
	// table nobody may lie in.
	//
	// IP is the *client* where a trusted proxy vouched for one — see
	// `realIPMiddleware` — so behind a load balancer this is the person, not
	// the balancer. APIKeyName is resolved at read time and is empty for a key
	// that has since been deleted; APIKeyID survives it either way.
	IP         string `json:"ip,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
	APIKeyID   string `json:"api_key_id,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
}

// ActivityFilter narrows the merged feed. Zero values mean "no filter".
type ActivityFilter struct {
	// Query matches the summary, the action and the target label.
	Query string
	// Severities keeps rows of these severities; empty keeps all. An audit
	// row is a warning when its action is a refusal or a failure
	// (auditProblem) and informational otherwise — never an error.
	Severities []string
	// ActorID keeps the audit rows this actor wrote. Exact, where Query
	// also finds the rows somebody was the *target* of, and every login
	// theirs is a substring of. Event rows have no actor and are excluded.
	ActorID string
	// Action keeps one action, or a family on a trailing '*'. Unlike
	// Actions it reads both halves: an event row's action is 'app.' || kind.
	Action string
	Since  time.Time
	Until  time.Time
	// Sources restricts which halves of the union are read: "audit", "event",
	// or empty for both. The UI's category chips map onto this plus Actions.
	Source string
	// Actions restricts audit rows to these action strings, prefix-matched on
	// a trailing '*'. Ignored for event rows.
	Actions []string
	// Before/BeforeID are the keyset cursor: return rows strictly older than
	// this (occurred_at, id) pair. Offset paging would skip or repeat rows as
	// new events land between pages, which on a feed that grows while you read
	// it is not a hypothetical.
	Before   time.Time
	BeforeID string
	Limit    int
	// AppID narrows both halves to one app's history: audit rows by
	// audit_log.app_id, events by their app. A deleted app's id still
	// answers — its history is what somebody arrives looking for.
	AppID string
}

// ListActivity returns the merged governance-and-system feed, newest first.
//
// This is the first read path audit_log has ever had. It is a UNION ALL over
// two tables that both have an occurred_at index, so Postgres merge-appends
// rather than sorting the world.
func (db *DB) ListActivity(f ActivityFilter) ([]ActivityEntry, error) {
	limit := clampLimit(f.Limit, DefaultActivityLimit, MaxActivityLimit)

	var parts []string
	var args []interface{}

	if f.Source == "" || f.Source == "audit" {
		q := `
		SELECT a.id AS id, 'audit' AS source, a.occurred_at AS occurred_at,
		       CASE
		           WHEN a.actor_type = 'operator' THEN 'cli'
		           -- A sign-in attempt against an account that does not exist.
		           -- Without this it would fall through to 'system', which reads
		           -- as the platform having done it.
		           WHEN a.actor_type = 'anonymous' THEN 'anonymous'
		           WHEN u.login IS NOT NULL THEN u.login
		           -- An app acting for itself (a store presign, a send) is named
		           -- by its slug, as a person is by their login: a bare UUID in
		           -- the actor column is a row nobody can place.
		           WHEN aa.slug IS NOT NULL THEN aa.slug
		           -- A deploy key belongs to nobody, so it is named for what it
		           -- is: an app a promotion created read "57d9b848-…" in the
		           -- actor column, which is the key's id and nothing a reader
		           -- can place. A revoked key keeps its row and its name.
		           WHEN dk.name IS NOT NULL THEN 'deploy key ' || dk.name
		           WHEN a.actor_id IS NOT NULL AND a.actor_id <> '' THEN a.actor_id
		           ELSE 'system'
		       END AS actor_label,
		       a.action AS action,
		       COALESCE(a.target_type,'') AS target_type,
		       COALESCE(a.target_id,'') AS target_id,
		       -- Every target type that has a name gets resolved to it. This
		       -- used to be apps and users and then a fallback to the raw
		       -- target_id, so half the Activity page pointed at a truncated
		       -- UUID: a storage resource or a group, named by an identifier
		       -- nobody can place, in the column beside app rows resolved to
		       -- their slug. A row that cannot say what it acted on is a row
		       -- that costs a query to read.
		       --
		       -- An app that has since been deleted is named by the slug its
		       -- own app.delete row recorded (DeleteAppTx writes it), so its
		       -- history still says *which* app rather than an id.
		       --
		       -- target_type in each lookup is what lets audit_log_target_idx
		       -- serve it: without it every row of a deleted app scanned the
		       -- whole log, and a page of the Activity log took ~43 s on an
		       -- install with a long one (BB-113).
		       COALESCE(apps.slug, tu.login, sr.slug, g.name,
		                CASE WHEN a.target_type = 'app' THEN (
		                    SELECT d.before->>'slug' FROM audit_log d
		                    WHERE d.target_type = 'app' AND d.target_id = a.target_id AND d.action = 'app.delete'
		                    LIMIT 1) END,
		                -- A deleted storage resource likewise, from its own
		                -- storage_resource.delete row: its destroy_started and
		                -- destroyed rows land after the row is gone.
		                CASE WHEN a.target_type = 'storage_resource' THEN (
		                    SELECT d.before->>'slug' FROM audit_log d
		                    WHERE d.target_type = 'storage_resource' AND d.target_id = a.target_id AND d.action = 'storage_resource.delete'
		                    LIMIT 1) END,
		                COALESCE(a.target_id,'')) AS target_label,
		       (a.target_type = 'app' AND COALESCE(a.target_id,'') <> '' AND apps.id IS NULL) AS target_gone,
		       CASE WHEN ` + auditProblem + ` THEN 'warning' ELSE 'info' END AS severity,
		       a.action AS summary,
		       -- ::text because after is JSONB and this half of the union
		       -- merges with the plain-text detail of an app_event.
		       --
		       -- "before" when there is no "after". The facts about a deletion
		       -- are all in the before column by construction — there is no
		       -- after-state of a thing that is gone — so every *.delete row on
		       -- this page had an empty detail and, since the join that
		       -- resolves a target to its name also finds nothing, said only
		       -- that an id nobody can place was removed. That is six actions
		       -- and counting (app.delete, auth_provider.delete,
		       -- credential.delete, storage_resource.delete,
		       -- storage_mapping.delete, store_grant.delete), and it is the one
		       -- class of row where a reader cannot go and look at the thing
		       -- itself to recover the answer.
		       COALESCE(a.after::text, a.before::text, '') AS detail,
		       COALESCE(a.ip,'') AS ip,
		       COALESCE(a.user_agent,'') AS user_agent,
		       COALESCE(a.api_key_id,'') AS api_key_id,
		       COALESCE(k.name,'') AS api_key_name,
		       COALESCE(a.request_id,'') AS request_id,
		       a.app_id AS app_id,
		       COALESCE(a.actor_id,'') AS actor_id
		FROM audit_log a
		LEFT JOIN users u ON u.id = a.actor_id AND a.actor_type = 'user'
		LEFT JOIN apps aa ON aa.id = a.actor_id AND a.actor_type = 'app'
		LEFT JOIN deploy_keys dk ON dk.id = a.actor_id AND a.actor_type = 'deploy_key'
		LEFT JOIN apps ON apps.id = a.target_id AND a.target_type = 'app'
		LEFT JOIN users tu ON tu.id = a.target_id AND a.target_type = 'user'
		LEFT JOIN storage_resources sr ON sr.id = a.target_id AND a.target_type = 'storage_resource'
		LEFT JOIN groups g ON g.id = a.target_id AND a.target_type = 'group'
		-- Personal keys only: apikey_auth.go is the one place a principal is
		-- given an APIKeyID, and it mints them from user_api_keys.
		LEFT JOIN user_api_keys k ON k.id = a.api_key_id
		WHERE 1=1`
		wantInfo, wantWarning := wantsSeverity(f.Severities, EventSeverityInfo), wantsSeverity(f.Severities, EventSeverityWarning)
		if !wantInfo && !wantWarning {
			// An audit row is informational or a warning, so a filter naming
			// neither excludes this half entirely rather than matching
			// nothing slowly.
			q = ""
		} else {
			switch {
			case !wantInfo:
				q += ` AND ` + auditProblem
			case !wantWarning:
				q += ` AND NOT ` + auditProblem
			}
			if f.ActorID != "" {
				q += ` AND a.actor_id = ?`
				args = append(args, f.ActorID)
			}
			if f.Action != "" {
				clause, cargs := actionMatchClause("a.action", []string{f.Action})
				q += ` AND ` + clause
				args = append(args, cargs...)
			}
			if f.AppID != "" {
				q += ` AND a.app_id = ?`
				args = append(args, f.AppID)
			}
			if !f.Since.IsZero() {
				q += ` AND a.occurred_at >= ?`
				args = append(args, f.Since)
			}
			if !f.Until.IsZero() {
				q += ` AND a.occurred_at <= ?`
				args = append(args, f.Until)
			}
			if !f.Before.IsZero() {
				q += ` AND (a.occurred_at < ? OR (a.occurred_at = ? AND a.id < ?))`
				args = append(args, f.Before, f.Before, f.BeforeID)
			}
			if len(f.Actions) > 0 {
				clause, cargs := actionMatchClause("a.action", f.Actions)
				q += ` AND ` + clause
				args = append(args, cargs...)
			}
			if f.Query != "" {
				like := likeContains(strings.ToLower(f.Query))
				// The address and the request id are searchable because they
				// are what somebody arrives holding: a request id off a failed
				// response, or an address out of a security report. Matching
				// them here is what makes either one a way *in* to the feed
				// rather than a column you scroll looking for.
				//
				// The rest is whatever the Who and Target columns print, because
				// a search that cannot find a name the page is showing reads as
				// "nothing happened": it matched a live app's slug and a target
				// user's login and nothing else, so a deleted app — named in
				// its rows by the slug its deletion recorded, and the usual
				// reason anyone searches for it — found nothing, and neither
				// did the person who did the thing. A deleted app or storage
				// resource is matched through its own delete row, as the label
				// is: one hashed subplan, not a lookup per row.
				q += ` AND (LOWER(a.action) LIKE ? ESCAPE '\'` +
					` OR LOWER(COALESCE(u.login,'')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(aa.slug,'')) LIKE ? ESCAPE '\'` +
					` OR LOWER(COALESCE(dk.name,'')) LIKE ? ESCAPE '\'` +
					` OR LOWER(COALESCE(apps.slug,'')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(tu.login,'')) LIKE ? ESCAPE '\'` +
					` OR LOWER(COALESCE(sr.slug,'')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(g.name,'')) LIKE ? ESCAPE '\'` +
					` OR (a.target_type IN ('app','storage_resource') AND a.target_id IN (` + deletedSlugMatch + `))` +
					` OR LOWER(COALESCE(a.target_id,'')) LIKE ? ESCAPE '\'` +
					` OR LOWER(COALESCE(a.ip,'')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(a.request_id,'')) LIKE ? ESCAPE '\'` +
					// The file, on a storage row. A signed link's audit names
					// the object only in its detail, so "who opened
					// summary.pdf" found nothing but the store — and the file
					// is the question an access review asks. Narrowed to the
					// store.* actions, whose `key` is an object key, rather
					// than every row's after-state.
					` OR (a.action LIKE 'store.%' AND LOWER(COALESCE(a.after->>'key','')) LIKE ? ESCAPE '\'))`
				args = append(args, like, like, like, like, like, like, like, like, like, like, like, like, like)
			}
		}
		if q != "" {
			parts = append(parts, q)
		}
	}

	// A category is a family of audit actions, and an actor is whoever wrote
	// an audit row; asking for either excludes event rows rather than silently
	// widening the result.
	if (f.Source == "" || f.Source == "event") && len(f.Actions) == 0 && f.ActorID == "" {
		q := `
		SELECT e.id AS id, 'event' AS source, e.occurred_at AS occurred_at,
		       'system:' || e.source AS actor_label,
		       'app.' || e.kind AS action,
		       'app' AS target_type,
		       e.app_id AS target_id,
		       COALESCE(ea.slug, (
		           SELECT d.before->>'slug' FROM audit_log d
		           WHERE d.target_type = 'app' AND d.target_id = e.app_id AND d.action = 'app.delete'
		           LIMIT 1), e.app_id) AS target_label,
		       (ea.id IS NULL) AS target_gone,
		       e.severity AS severity,
		       e.summary AS summary,
		       e.detail AS detail,
		       -- No request produced an observation, so these five are empty
		       -- rather than absent: the union needs the columns, and the UI
		       -- draws nothing when they are blank.
		       '' AS ip,
		       '' AS user_agent,
		       '' AS api_key_id,
		       '' AS api_key_name,
		       '' AS request_id,
		       e.app_id AS app_id,
		       '' AS actor_id
		FROM app_events e
		LEFT JOIN apps ea ON ea.id = e.app_id
		WHERE 1=1`
		if f.AppID != "" {
			q += ` AND e.app_id = ?`
			args = append(args, f.AppID)
		}
		if !f.Since.IsZero() {
			q += ` AND e.occurred_at >= ?`
			args = append(args, f.Since)
		}
		if !f.Until.IsZero() {
			q += ` AND e.occurred_at <= ?`
			args = append(args, f.Until)
		}
		if len(f.Severities) > 0 {
			q += ` AND e.severity IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Severities)), ",") + `)`
			for _, sev := range f.Severities {
				args = append(args, sev)
			}
		}
		if f.Action != "" {
			clause, cargs := actionMatchClause("('app.' || e.kind)", []string{f.Action})
			q += ` AND ` + clause
			args = append(args, cargs...)
		}
		if !f.Before.IsZero() {
			q += ` AND (e.occurred_at < ? OR (e.occurred_at = ? AND e.id < ?))`
			args = append(args, f.Before, f.Before, f.BeforeID)
		}
		if f.Query != "" {
			like := likeContains(strings.ToLower(f.Query))
			// A deleted app's observations are found by the slug its deletion
			// recorded, as its audit rows are.
			q += ` AND (LOWER(e.summary) LIKE ? ESCAPE '\' OR LOWER(e.kind) LIKE ? ESCAPE '\'` +
				` OR LOWER(COALESCE(ea.slug,'')) LIKE ? ESCAPE '\'` +
				` OR (ea.id IS NULL AND e.app_id IN (` + deletedSlugMatch + `)))`
			args = append(args, like, like, like, like)
		}
		parts = append(parts, q)
	}

	if len(parts) == 0 {
		return nil, nil
	}

	q := strings.Join(parts, "\nUNION ALL\n") +
		"\nORDER BY occurred_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityEntry
	for rows.Next() {
		var e ActivityEntry
		if err := rows.Scan(&e.ID, &e.Source, &e.OccurredAt, &e.ActorLabel, &e.Action,
			&e.TargetType, &e.TargetID, &e.TargetLabel, &e.TargetGone, &e.Severity, &e.Summary, &e.Detail,
			&e.IP, &e.UserAgent, &e.APIKeyID, &e.APIKeyName, &e.RequestID, &e.AppID, &e.ActorID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// auditProblem is true of an audit row whose action records that something was
// refused or failed — `deploy_source_refused`, `store.link.refused`,
// `session.signin_failed` — and is what makes such a row a warning.
//
// By the action's own name rather than a list: every refusal this table has
// ever recorded says so in its action, and a list kept here would be one more
// thing a new refusal has to remember. A person declining something
// (`access_request.deny`, `user.cli_login_deny`) is a decision, not a problem,
// and matches neither pattern.
const auditProblem = `(a.action LIKE '%refused%' OR a.action LIKE '%\_failed' ESCAPE '\')`

// wantsSeverity reports whether a severity filter keeps sev; no filter keeps all.
func wantsSeverity(filter []string, sev string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, s := range filter {
		if s == sev {
			return true
		}
	}
	return false
}

// ActivityActorLabel is the name the feed prints for an actor id — a user's
// login, an app's slug, a deploy key — or the id itself when nothing holds it
// any longer, which is also what the Who column falls back to.
func (db *DB) ActivityActorLabel(id string) (string, error) {
	var label string
	err := db.conn.QueryRow(`
		SELECT COALESCE(
			(SELECT login FROM users WHERE id = ?),
			(SELECT slug FROM apps WHERE id = ?),
			(SELECT 'deploy key ' || name FROM deploy_keys WHERE id = ?),
			?)`, id, id, id, id).Scan(&label)
	return label, err
}

// deletedSlugMatch is the ids of deleted apps and storage resources whose
// recorded slug matches one LIKE argument — the slug their own delete row
// wrote, which is what the feed names them by once they are gone.
const deletedSlugMatch = `SELECT d.target_id FROM audit_log d
	WHERE d.action IN ('app.delete','storage_resource.delete')
	  AND LOWER(COALESCE(d.before->>'slug','')) LIKE ? ESCAPE '\'`

// actionMatchClause builds an OR of equality and prefix tests for the audit
// action filter. A trailing '*' means prefix — the categories in the admin UI
// are families ("identity.*"), not enumerations that must be kept in sync with
// every action string the backend writes.
func actionMatchClause(col string, actions []string) (string, []interface{}) {
	var terms []string
	var args []interface{}
	for _, a := range actions {
		if strings.HasSuffix(a, "*") {
			terms = append(terms, col+" LIKE ?")
			args = append(args, strings.TrimSuffix(a, "*")+"%")
		} else {
			terms = append(terms, col+" = ?")
			args = append(args, a)
		}
	}
	return "(" + strings.Join(terms, " OR ") + ")", args
}

// AppScopeStartedAt is when app attribution began on this install — the
// audit_log.app_scope_started row migration 00071 wrote where the log already
// had rows — or the zero time when there is no such row: a fresh install, or
// one whose retention has since removed it with everything it described.
func (db *DB) AppScopeStartedAt() (time.Time, error) {
	var t time.Time
	err := db.conn.QueryRow(`SELECT occurred_at FROM audit_log WHERE action = ? ORDER BY occurred_at LIMIT 1`,
		ActionAppScopeStarted).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	return t, err
}
