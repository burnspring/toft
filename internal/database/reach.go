package database

import (
	"database/sql"
	"encoding/json"
	"time"
)

// What an app can reach: everything this platform has given one app, in one
// place, and the same set across the estate.
//
// Nothing here is a new fact. Every row it reads was written by a feature that
// had its own reason to write it, and every one of them already has a screen —
// on five different tabs, which is the whole finding. The one question a person
// asks about code nobody read is *what does this thing touch*, and until this
// file existed the answer was assembled by hand by somebody who knew where all
// five tabs were.
//
// **Reach is what an app *can* touch, never what it *did*.** The send ledger,
// the usage tables and audit_log are history and each already has a reader. An
// app that has posted nothing to Slack this month still may, and a zero that
// meant "has not" would be read as "cannot".
//
// **Names, never values.** Credential names, environment keys, resource slugs,
// mount paths, integration kinds and destination labels. No credential value
// under any circumstance: a governance reader must not become the second route
// that reads a credential, and it does not need to be — *which* credential an
// app holds is the entire governance fact.

// ----------------------------------------------------------- the closed list

// ReachSource is one way this platform lets an app touch something.
//
// The slice below is the specification, on appint.Known's precedent: a fact
// absent from it is not reach, and the absence has to be argued rather than
// happen. The alternative is a reader that assembles whatever its author
// remembered, and that fails silently and permanently — a later feature gives
// apps a new way to touch something, the page keeps rendering, and the only
// signal is that the answer was quietly wrong for a year.
type ReachSource struct {
	// Key is the wire name of the section, stable across releases.
	Key string
	// Label is what a person is shown.
	Label string
	// Tables are the rows this source reads that carry an `app_id`, and they
	// are what TestReachSourcesAreTheSpecification matches against the schema.
	// Empty for a source that is a column on `apps` rather than a table of its
	// own — those cannot go missing without the apps table changing shape,
	// which no walk of app_id columns would catch anyway.
	Tables []string
	// Elsewhere are the tables it also reads that are keyed some other way:
	// object_grants by object_id, a schedule's recipients by schedule_id. They
	// are outside the walk by construction and named here anyway, so this
	// slice stays a complete account of what the reader touches rather than a
	// list of whatever the guard happened to be able to check.
	Elsewhere []string
}

// ReachSources is every fact this reader assembles, in the order both surfaces
// render them: who may open it, who may frame it, what it is told, what it
// holds, what it can write to, and what it can send.
var ReachSources = []ReachSource{
	{Key: "grants", Label: "Who may open it", Elsewhere: []string{"object_grants"}},
	{Key: "embedding", Label: "Who may frame it"},
	{Key: "viewer_identity", Label: "Whether it is told who is looking"},
	{Key: "credentials", Label: "Values its owner never holds", Tables: []string{"app_credentials"}},
	{Key: "env", Label: "Values its owner set", Tables: []string{"env_vars"}},
	{Key: "storage", Label: "Durable storage it mounts", Tables: []string{"storage_mappings"}},
	{Key: "integrations", Label: "What it may send, and where", Tables: []string{"app_integrations"}},
	{Key: "data", Label: "A document it keeps", Tables: []string{"app_data"}},
	{Key: "report_recipients", Label: "People it emails on a timer", Tables: []string{"app_render_schedules"}, Elsewhere: []string{"app_render_schedule_recipients"}},
}

// NotReach names every table carrying an app_id that is deliberately *not* a
// reach source, with the reason it is not.
//
// **This half is the load-bearing one.** It is what makes "we decided this is
// not reach" different from "we forgot", and it is why the guard can be a walk
// of the schema rather than a list somebody remembers to update. Adding a table
// here is a decision that shows up in review; leaving one out fails the build.
var NotReach = map[string]string{
	// History. Every one of these records something that already happened, and
	// each has a reader of its own. Mixing capability with activity would
	// answer neither question.
	"audit_log":                  "history — who did what to the app; app_id says whose history a row is, not what the app holds",
	"api_key_usage_daily":        "history — what a key did, not what the app holds",
	"app_capability_usage_daily": "history — sends counted after the fact",
	"app_consumption_daily":      "history — what the app cost",
	"app_consumption_intervals":  "history — the raw intervals behind the cost",
	"app_events":                 "history — what went wrong with the app, and when",
	"app_integration_sends":      "history — the send ledger, which is the transpose of the integrations source and not a second copy of it",
	"app_platform_log":           "history — the platform's own voice about the app",
	"app_usage_daily":            "history — who opened it",
	"app_audience_daily":         "history — how many opened it",
	"job_runs":                   "history — what a job did",
	"report_deliveries":          "history — what happened to each recipient on each run",
	"dependency_scans":           "history — what a scan found in a release, which is the dependency reader's subject",

	// Things the app points at *itself*. An app starting its own entrypoint
	// reaches nothing it did not already reach, so listing it here would pad
	// the page with rows that change no answer.
	"job_definitions":       "a different entrypoint of the same app, which reaches exactly what the app reaches",
	"app_restart_schedules": "the platform restarting the app on a timetable — the same process, reaching what it already reaches",

	// Facts *about* the app rather than things given to it.
	"app_tags":           "a label somebody filed the app under — it reaches nothing",
	"deployments":        "the release, its provenance and its build — what the app *is*, not what it was given",
	"dependency_waivers": "a verdict about the packages in a release, which is the dependency policy's subject",

	// The seam itself, and what came out of it.
	"app_tokens":             "the app's own outbound credential, minted per deployment by the platform — the seam every source above is read through, never a row an app holds",
	"integration_placements": "residue — what an app already left in somebody else's system, which outlives the app and is history by construction",
}

// ------------------------------------------------------------- the per-app read

// ReachGrant is one principal who may open the app, at one rung.
type ReachGrant struct {
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
	Name          string `json:"name"`
	Role          string `json:"role"`
}

// ReachCredential is a bag of values an app's own owner never holds. The name
// and nothing else: see the file header.
type ReachCredential struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Keys are the value *names* inside it, which is what makes the row useful
	// to somebody deciding whether a rotation touches this app. Never a value.
	Keys []string `json:"keys"`
}

// ReachEnvKey is one environment variable the owner set.
//
// Sensitive says the value is write-only and is not carried here; a value that
// is not sensitive is not carried here either, because this reader answers what
// an app is *given* and the Settings tab is where a value is read and changed.
type ReachEnvKey struct {
	Key       string `json:"key"`
	Sensitive bool   `json:"sensitive"`
}

// ReachMount is one durable location the app has, at one level.
type ReachMount struct {
	ResourceID   string `json:"resource_id"`
	ResourceSlug string `json:"resource_slug"`
	ResourceName string `json:"resource_name"`
	// ResourceKind is `volume` or `store`, and it is not decoration: the access
	// ladder is not symmetric between them — a volume has read and write, a
	// store has two rungs above write — so the same word means different things
	// and a surface without the kind would label one of them wrongly.
	ResourceKind string `json:"resource_kind"`
	Path         string `json:"path"`
	Access       string `json:"access"`
	Folder       string `json:"folder,omitempty"`
	// Authority is which delegation wrote the mapping — `admin`, `user:<id>`
	// or `group:<id>`. It rides here because "who gave this app write access to
	// the finance share" is the second question anybody asks after seeing the
	// first.
	Authority string `json:"authority"`
	// ResourceMissing is a mapping whose resource is gone. The mount is still
	// reported: an app mounting something the platform can no longer describe
	// is a fact, not a row to drop.
	//
	// **Unreachable today, and kept deliberately.**
	// storage_mappings_resource_id_fkey cascades, so deleting a resource takes
	// its mappings with it and DeleteStorageResource refuses while any app
	// mounts one — there is no path to an orphan. The LEFT JOIN and this flag
	// cost nothing and mean a future change to either of those does not
	// silently make the reader one row shorter than the database. Nobody
	// should read the flag as evidence that orphans occur.
	ResourceMissing bool `json:"resource_missing,omitempty"`
}

// ReachIntegration is one channel the app may send on, and the three booleans
// that decide whether it actually can.
//
// **Present and not effective is never reported as absent.** An install that
// switched a kind off after an app attached it has an attachment an
// administrator has to re-examine when they switch it back on, and hiding the
// row is how that gets forgotten.
type ReachIntegration struct {
	Kind        string `json:"kind"`
	Destination string `json:"destination,omitempty"`
	// Connected is whether the connection this app would send through offers
	// itself to apps; Attached is whether this app has one at all; Enabled is
	// the attachment's own switch. All three, because the refusal a person has
	// to act on is a different one in each case.
	Connected bool `json:"connected"`
	Attached  bool `json:"attached"`
	Enabled   bool `json:"enabled"`
}

// Effective is the send actually going through — appint.Check's first three
// gates, and nothing about whether the app is running.
func (r ReachIntegration) Effective() bool { return r.Connected && r.Attached && r.Enabled }

// ReachData is the app's own JSON document, described and never read. One
// document, no keys, no listing — see ADR § App data.
type ReachData struct {
	Bytes     int       `json:"bytes"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReachRecipient is one person the app's re-render mails on a timer.
type ReachRecipient struct {
	ScheduleID string `json:"schedule_id"`
	UserID     string `json:"user_id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	// Enabled is the schedule's switch, not the person's: a suspended schedule
	// still names who it would mail, and that is the fact worth reporting.
	Enabled bool `json:"enabled"`
}

// AppReach is one app's whole assembly.
//
// The integration triple is deliberately absent: it is resolved through
// appint.ResolveAttachment by the layer that can import appint, and this
// package cannot without a cycle. See ADR §92 D8 for why the estate reader
// answers the same three booleans in SQL instead, and what guards the pair.
type AppReach struct {
	AppID string `json:"app_id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`

	// Who may open it. Visibility is the stored column and AnonymityWithheld
	// is whether anything has taken anonymous access away; the substitution is
	// AuthObjectOf's and belongs to the layer that serves this, exactly as it
	// does for EstateTarget.
	Visibility        string       `json:"visibility"`
	AnonymityWithheld bool         `json:"-"`
	Listable          bool         `json:"listable"`
	Grants            []ReachGrant `json:"grants"`

	// Who may frame it. Empty is the install's own answer, which is not
	// "anyone" — see ADR § Process and network boundaries.
	FrameAncestors string `json:"frame_ancestors"`

	// Whether it is told who is looking. A per-app column with no install-wide
	// switch behind it, so there is nothing to resolve it against.
	ViewerIdentity bool `json:"viewer_identity"`

	Credentials []ReachCredential `json:"credentials"`
	EnvKeys     []ReachEnvKey     `json:"env_keys"`
	Mounts      []ReachMount      `json:"mounts"`
	Data        *ReachData        `json:"data,omitempty"`
	Recipients  []ReachRecipient  `json:"report_recipients"`
}

// ReachOf assembles everything one app can touch.
//
// One app, so one query per source rather than the estate reader's single
// aggregate: the shapes here are lists a person reads in full, and ten reads
// for one page is the cost of rendering them rather than a scaling problem.
// EstateReach is where that trade goes the other way, and ADR §92 D7 is why.
//
// Returns nil for an app that does not exist. Authorization is the caller's:
// this returns everything about the app it is asked about.
func (db *DB) ReachOf(appID string) (*AppReach, error) {
	r := &AppReach{
		Grants:      []ReachGrant{},
		Credentials: []ReachCredential{},
		EnvKeys:     []ReachEnvKey{},
		Mounts:      []ReachMount{},
		Recipients:  []ReachRecipient{},
	}

	err := db.conn.QueryRow(`
		SELECT apps.id, apps.slug, apps.name, apps.visibility, apps.listable,
		       apps.viewer_identity, apps.frame_ancestors,
		       (`+publicAppsOffExpr+`)
		FROM apps WHERE apps.id = ?`, appID).Scan(
		&r.AppID, &r.Slug, &r.Name, &r.Visibility, &r.Listable,
		&r.ViewerIdentity, &r.FrameAncestors, &r.AnonymityWithheld)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Who may open it. The owner is not a grant row — apps_one_owner makes
	// ownership a column — so this is the *shared* list, and the layer above
	// renders the owner beside it.
	if err := db.scan(`
		SELECT g.principal_type, g.principal_id, g.role,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.login,''), u.email, gr.name, '')
		FROM object_grants g
		LEFT JOIN users  u  ON g.principal_type = 'user'  AND u.id  = g.principal_id
		LEFT JOIN groups gr ON g.principal_type = 'group' AND gr.id = g.principal_id
		WHERE g.object_id = ?
		ORDER BY g.principal_type, 4, g.principal_id`, appID, func(rows *sql.Rows) error {
		var g ReachGrant
		if err := rows.Scan(&g.PrincipalType, &g.PrincipalID, &g.Role, &g.Name); err != nil {
			return err
		}
		r.Grants = append(r.Grants, g)
		return nil
	}); err != nil {
		return nil, err
	}

	// Values its owner never holds. The key *names* come along because they
	// are what tells somebody whether a rotation touches this app; the values
	// live encrypted in credential_values and are not read here or anywhere
	// else this reader can reach.
	if err := db.scan(`
		SELECT c.id, c.name,
		       COALESCE((SELECT json_agg(v.key ORDER BY v.key)
		                 FROM credential_values v WHERE v.credential_id = c.id), '[]')
		FROM app_credentials ac
		JOIN credentials c ON c.id = ac.credential_id
		WHERE ac.app_id = ?
		ORDER BY c.name`, appID, func(rows *sql.Rows) error {
		var c ReachCredential
		var keys []byte
		if err := rows.Scan(&c.ID, &c.Name, &keys); err != nil {
			return err
		}
		if err := json.Unmarshal(keys, &c.Keys); err != nil {
			return err
		}
		r.Credentials = append(r.Credentials, c)
		return nil
	}); err != nil {
		return nil, err
	}

	// Values its owner set. Keys only, and the sensitive flag — never a value,
	// including the ones envVarResponse would have handed back, because this
	// page is an inventory and the Settings tab is the editor.
	if err := db.scan(`
		SELECT e.key, e.sensitive FROM env_vars e WHERE e.app_id = ? ORDER BY e.key`,
		appID, func(rows *sql.Rows) error {
			var e ReachEnvKey
			if err := rows.Scan(&e.Key, &e.Sensitive); err != nil {
				return err
			}
			r.EnvKeys = append(r.EnvKeys, e)
			return nil
		}); err != nil {
		return nil, err
	}

	// Durable storage it mounts. LEFT JOIN, not JOIN: a mapping whose resource
	// has been deleted is still a mount this app has, and dropping the row
	// would make the reader quietly less complete than the database.
	if err := db.scan(`
		SELECT m.resource_id, COALESCE(r.slug,''), COALESCE(r.name,''),
		       COALESCE(r.kind,''), m.path, m.access, m.folder, m.authority, r.id IS NULL
		FROM storage_mappings m
		LEFT JOIN storage_resources r ON r.id = m.resource_id
		WHERE m.app_id = ?
		ORDER BY m.path`, appID, func(rows *sql.Rows) error {
		var m ReachMount
		if err := rows.Scan(&m.ResourceID, &m.ResourceSlug, &m.ResourceName,
			&m.ResourceKind, &m.Path, &m.Access, &m.Folder, &m.Authority,
			&m.ResourceMissing); err != nil {
			return err
		}
		r.Mounts = append(r.Mounts, m)
		return nil
	}); err != nil {
		return nil, err
	}

	// A document it keeps. Described, never read: length rather than content,
	// which is the same thing GET /apps/{id}/data answers.
	var d ReachData
	err = db.conn.QueryRow(`
		SELECT LENGTH(document), version, updated_at FROM app_data
		 WHERE app_id = ? AND document <> ''`,
		appID).Scan(&d.Bytes, &d.Version, &d.UpdatedAt)
	switch {
	case err == sql.ErrNoRows:
		// Never written. Absent rather than a zero-byte document, because the
		// two are tellable apart and § App data says they must stay so.
	case err != nil:
		return nil, err
	default:
		r.Data = &d
	}

	// People it emails on a timer.
	if err := db.scan(`
		SELECT rs.id, rr.user_id, rs.enabled,
		       COALESCE(NULLIF(u.name,''), NULLIF(u.login,''), ''), COALESCE(u.email,'')
		FROM app_render_schedules rs
		JOIN app_render_schedule_recipients rr ON rr.schedule_id = rs.id
		LEFT JOIN users u ON u.id = rr.user_id
		WHERE rs.app_id = ?
		ORDER BY 4, 5`, appID, func(rows *sql.Rows) error {
		var p ReachRecipient
		if err := rows.Scan(&p.ScheduleID, &p.UserID, &p.Enabled, &p.Name, &p.Email); err != nil {
			return err
		}
		r.Recipients = append(r.Recipients, p)
		return nil
	}); err != nil {
		return nil, err
	}

	return r, nil
}

// scan runs a query and feeds each row to fn, closing the rows either way.
//
// Local to this file rather than a package-wide helper: ReachOf is the only
// reader here with nine list queries in it, and nine copies of the same
// four-line rows/defer/Err dance is where a missed rows.Err() eventually hides.
func (db *DB) scan(query string, arg interface{}, fn func(*sql.Rows) error) error {
	rows, err := db.conn.Query(query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// -------------------------------------------------------- the estate reader

// EstateReachRow is one app, as *what it holds*.
//
// A row on /admin/estate/languages is a language and a count; a row on
// /admin/estate/targets is an app to visit; a row here is an app and what it
// was given. Three questions, three readers, on the argument §77 already made
// against merging the first two: a reader answering all three answers none of
// them well.
//
// Names and counts, never the full assembly. Enough to sort, filter and be
// alarmed by; the whole document stays on the app, because a payload carrying
// every fact about every app is one nobody renders and somebody eventually
// leaks.
type EstateReachRow struct {
	AppID string `json:"app_id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`

	// Visibility is the stored column and AnonymityWithheld is whether
	// anything has withdrawn anonymous access. Resolved into the effective
	// word by the layer that serves this, through the one function that owns
	// the substitution — exactly as EstateTarget is, and for the same reason.
	Visibility        string `json:"visibility"`
	AnonymityWithheld bool   `json:"-"`
	Listable          bool   `json:"listable"`
	Locked            bool   `json:"locked"`

	Owner        string `json:"owner,omitempty"`
	OwnerKind    string `json:"owner_kind,omitempty"`
	OwnerID      string `json:"owner_id,omitempty"`
	OwnerGroupID string `json:"owner_group_id,omitempty"`

	// FrameAncestors and ViewerIdentity are the two switches that ride on the
	// app row itself, so they cost nothing to carry and are the two an
	// administrator most often wants to filter on.
	FrameAncestors string `json:"frame_ancestors,omitempty"`
	ViewerIdentity bool   `json:"viewer_identity"`

	// Credentials and Mounts are named because the transpose in D10 is built
	// from these rows rather than from a second query — which is what makes
	// "which apps hold this credential" incapable of disagreeing with the list
	// it was derived from.
	Credentials  []EstateReachCredential `json:"credentials"`
	Mounts       []EstateReachMount      `json:"mounts"`
	Integrations []ReachIntegration      `json:"integrations"`

	// The rest are counts and flags: the app's own tab is where the list is.
	EnvKeys    int  `json:"env_keys"`
	Grants     int  `json:"grants"`
	Recipients int  `json:"report_recipients"`
	HasData    bool `json:"has_data"`
}

// EstateReachCredential is a credential name on an estate row — the second
// axis's join key, and never a value.
type EstateReachCredential struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EstateReachMount is a mount on an estate row: which resource, at what level.
type EstateReachMount struct {
	ResourceID   string `json:"resource_id"`
	ResourceSlug string `json:"resource_slug"`
	ResourceName string `json:"resource_name"`
	ResourceKind string `json:"resource_kind"`
	Path         string `json:"path"`
	Access       string `json:"access"`
}

// Writable is a mount an app can change something through. `read` is the one
// level that cannot, so this is stated once here rather than by every caller
// re-deciding which of the four words are the interesting ones.
func (m EstateReachMount) Writable() bool { return m.Access != "read" }

// EstateReachSet is the whole answer, rows and coverage together.
//
// The coverage is summed from the same rows the table renders, for the reason
// EstateCensus and EstateTargetSet carry theirs: two counts that can disagree
// is a governance reader quietly answering less than it knows, which is the
// defect § Optional capabilities exists to prevent.
//
// **Nothing is ever skipped.** An app with no grants, no credentials and no
// attachments is a row — on an estate where everything else has ten entries it
// is the most interesting row on the page, and omitting it would hide an app
// from the one read that is supposed to enumerate the estate.
type EstateReachSet struct {
	Rows []EstateReachRow `json:"rows"`
	// AppsTotal is every app the scope admits.
	AppsTotal int `json:"apps_total"`
	// AppsWithReach is how many hold anything at all: a credential, a mount, an
	// integration, an env key, a key, a recipient list or a document. Sharing
	// and framing are not counted — every app has an answer to those, so
	// counting them would make this number equal AppsTotal and say nothing.
	AppsWithReach int `json:"apps_with_reach"`
	// AppsPublic is how many are effectively reachable by anyone, resolved by
	// the caller from the pair on each row rather than counted here, so the
	// number and the rows cannot disagree about what `public` means.
	AppsPublic int `json:"-"`
	// AppsWritableStorage is how many mount storage at a level above `read`.
	// The one count worth having in the coverage line rather than derived by a
	// reader: public plus writable is the pair the page exists to surface.
	AppsWritableStorage int `json:"apps_writable_storage"`
}

// EstateReach lists every app a scope admits with what it holds, ordered by
// slug.
//
// **One query.** The natural implementation is ten per app and it is correct;
// on the five-hundred-app estate this exists for it is five thousand round
// trips, which would make the governance reader the slowest page on the install
// — the page nobody opens until the morning they need it most. So every source
// is an aggregate subquery over the app's own id, and the coverage is summed
// from the rows on the way out.
//
// The scope is the *reading* predicate, beside EstateTargets and
// EstateByLanguage: what an app holds is a fact about the app, and the `list`
// rung discloses the row that names it and not one field more.
//
// The integration triple is resolved here in SQL rather than through
// appint.ResolveAttachment, which is ten queries per app by construction. That
// is a second implementation of a rule and ADR §92 D8 says so out loud;
// TestReachAndCapabilitiesAgree is the whole of its mitigation.
func (db *DB) EstateReach(scope AppScope) (EstateReachSet, error) {
	pred, predArgs := scope.readingPredicate()

	rows, err := db.conn.Query(`
		SELECT apps.id, apps.slug, apps.name, apps.visibility, apps.listable,
		       apps.locked_at IS NOT NULL,
		       apps.frame_ancestors, apps.viewer_identity,
		       (`+publicAppsOffExpr+`),
		       COALESCE(apps.owner_user_id,''), COALESCE(apps.owner_group_id,''),
		       COALESCE(NULLIF(users.name,''), NULLIF(users.login,''), users.email, ''),
		       COALESCE(groups.name,''),
		       COALESCE((SELECT json_agg(json_build_object('id', c.id, 'name', c.name) ORDER BY c.name)
		                 FROM app_credentials ac JOIN credentials c ON c.id = ac.credential_id
		                 WHERE ac.app_id = apps.id), '[]'),
		       COALESCE((SELECT json_agg(json_build_object(
		                            'resource_id', m.resource_id,
		                            'resource_slug', COALESCE(sr.slug,''),
		                            'resource_name', COALESCE(sr.name,''),
		                            'resource_kind', COALESCE(sr.kind,''),
		                            'path', m.path,
		                            'access', m.access) ORDER BY m.path)
		                 FROM storage_mappings m
		                 LEFT JOIN storage_resources sr ON sr.id = m.resource_id
		                 WHERE m.app_id = apps.id), '[]'),
		       COALESCE((SELECT json_agg(json_build_object(
		                            'kind', ai.kind,
		                            'destination', COALESCE(NULLIF(ai.destination_label,''), ic.default_destination_label, ''),
		                            'attached', TRUE,
		                            'enabled', ai.enabled,
		                            'connected', COALESCE(ic.for_apps, FALSE)) ORDER BY ai.kind)
		                 FROM app_integrations ai
		                 LEFT JOIN integration_connections ic ON ic.id = ai.connection_id
		                 WHERE ai.app_id = apps.id), '[]'),
		       (SELECT COUNT(*) FROM env_vars e WHERE e.app_id = apps.id),
		       (SELECT COUNT(*) FROM object_grants g WHERE g.object_id = apps.id),
		       (SELECT COUNT(*) FROM app_render_schedules rs
		          JOIN app_render_schedule_recipients rr ON rr.schedule_id = rs.id
		         WHERE rs.app_id = apps.id),
		       EXISTS (SELECT 1 FROM app_data ad WHERE ad.app_id = apps.id AND ad.document <> '')
		FROM apps
		LEFT JOIN users  ON users.id  = apps.owner_user_id
		LEFT JOIN groups ON groups.id = apps.owner_group_id
		WHERE `+pred+`
		ORDER BY apps.slug`, predArgs...)
	if err != nil {
		return EstateReachSet{}, err
	}
	defer rows.Close()

	out := EstateReachSet{Rows: []EstateReachRow{}}
	for rows.Next() {
		var r EstateReachRow
		var userName, groupName string
		var creds, mounts, ints []byte
		if err := rows.Scan(&r.AppID, &r.Slug, &r.Name, &r.Visibility, &r.Listable,
			&r.Locked, &r.FrameAncestors, &r.ViewerIdentity, &r.AnonymityWithheld,
			&r.OwnerID, &r.OwnerGroupID, &userName, &groupName,
			&creds, &mounts, &ints,
			&r.EnvKeys, &r.Grants, &r.Recipients, &r.HasData); err != nil {
			return EstateReachSet{}, err
		}
		if err := json.Unmarshal(creds, &r.Credentials); err != nil {
			return EstateReachSet{}, err
		}
		if err := json.Unmarshal(mounts, &r.Mounts); err != nil {
			return EstateReachSet{}, err
		}
		if err := json.Unmarshal(ints, &r.Integrations); err != nil {
			return EstateReachSet{}, err
		}
		// Exactly one of the two is set — apps_one_owner is a database fact.
		// A name that has gone missing leaves the kind behind rather than
		// both, as EstateTargets does: "a group owns this and the group's name
		// did not come back" is a truer answer than an empty cell.
		switch {
		case r.OwnerGroupID != "":
			r.OwnerKind, r.Owner = "group", groupName
		case r.OwnerID != "":
			r.OwnerKind, r.Owner = "user", userName
		}

		out.Rows = append(out.Rows, r)
		out.AppsTotal++
		if r.holdsAnything() {
			out.AppsWithReach++
		}
		for _, m := range r.Mounts {
			if m.Writable() {
				out.AppsWritableStorage++
				break
			}
		}
	}
	return out, rows.Err()
}

// holdsAnything is the coverage line's question: was this app given anything at
// all beyond who may open it.
//
// Sharing and framing are excluded deliberately — every app has an answer to
// both, so counting them would make the number equal the total and the sentence
// above the table would stop saying anything.
func (r EstateReachRow) holdsAnything() bool {
	return len(r.Credentials) > 0 || len(r.Mounts) > 0 || len(r.Integrations) > 0 ||
		r.EnvKeys > 0 || r.Recipients > 0 || r.HasData
}
