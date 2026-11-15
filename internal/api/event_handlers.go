package api

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/auth"
	"toft/internal/database"
)

// Reading app failure events back — one app's stream, and the merged
// governance-and-system feed an administrator reads.
//
// Both are read-only. Nothing here writes an event: events come from the
// background workers that notice things break, never from a request.

// eventsAction gates an app's event stream.
//
// The same action the log surfaces use, and for the same reason: these rows
// carry build-log tails and git's own stderr, which can name internal hosts,
// repository paths and occasionally a token's shape. Someone who may only
// *run* an app has no business reading them.
const eventsAction = auth.ActionEditContent

// GET /api/v1/apps/{appID}/events
func (s *Server) handleListAppEvents(w http.ResponseWriter, r *http.Request) {
	app, ok := s.authorizeAppEvents(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	f := database.AppEventFilter{
		Kind:     q.Get("kind"),
		Severity: q.Get("severity"),
		Limit:    atoiOr(q.Get("limit"), 0),
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			f.Since = t
		}
	}

	// Scoped by the path app id, never by anything in the query — that is the
	// IDOR rule, and this endpoint is exactly the shape that gets it wrong.
	list, err := s.db.ListAppEvents(app.ID, f)
	if err != nil {
		log.Printf("listing events for %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if list == nil {
		list = []database.AppEvent{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) authorizeAppEvents(w http.ResponseWriter, r *http.Request) (*database.App, bool) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, false
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	if d := auth.Authorize(p, eventsAction, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, auth.CanRead(p, obj, s.db))
		return nil, false
	}
	return app, true
}

// activityCategories are the admin feed's chips, defined server-side.
//
// They live here rather than in the client because they are families of audit
// actions — "everything about access" is `identity.*`, `user.*`, `group.*` and
// so on — and a client-side list would drift silently every time a new action
// string is written. The one the UI cannot express as actions is "system",
// which is not an action filter at all but the whole app_events half.
//
// Every action the server writes belongs to exactly one family, and every
// pattern matches something it writes: web/src/lib/audit-actions.test.ts reads
// this map and the Go source and fails on an action no chip shows or a pattern
// nothing can reach. The audit_by_category compliance item reads the same map,
// so an action left out is missing from that evidence too, not only the chip.
var activityCategories = map[string][]string{
	// What was deployed, run or removed — the app's life, not its settings.
	"deploys": {"app.release_*", "app.create", "app.deploy", "app.delete", "app.create_refused_tier",
		"app.start", "app.restart", "app.stop", "app.data_reset", "app_source_changed", "deploy_source_refused",
		"gallery.*", "job_run.*"},
	// Who may do or see what: people, groups, keys, grants, and what an app
	// was given or who may open it.
	"access": {"identity.*", "user.*", "object_grant.*", "access_request.*", "group.*",
		"auth_provider.*", "password.*", "scim.*", "admin.*", "deploy_key.*",
		"credential.*", "store_grant.*", "storage_manager.*", "storage_default_manager.*", "store.*", "storage.level_promoted",
		"app.visibility_change", "app.listable_change", "app.lock*", "app.unlock*",
		"app.transfer_ownership", "app.viewer_identity.*",
		"app.frame_ancestors.*"},
	// How the install, and each app, is configured.
	"settings": {"platform_settings.*", "email_settings.*", "telemetry_settings.*",
		"announcement*", "knowledge_base.*", "tag.*", "app.tag_*", "feature.*",
		"integration*", "dependency_feed_credential.*", "git_connection.*", "app.git_*",
		"storage_resource.*", "storage_drive.*", "storage_mapping.*", "job.*",
		"build_cache.*", "dependency_waiver_*", "source_policy_*", "source_pattern_*", "app.retention_change", "app.never_sleep_change", "app.hostname_change", "app.env_change",
		"app.notify_failures_change", "app.garth.*", "app.execution_mode.*", "app.memory_limit.*", "app.cpu_limit.*", "audit_log.*"},
	// Sign-ins are their own chip rather than part of "access", and that is the
	// whole reason the session ledger could be added at all: `session.*` is the
	// highest-volume family in this table, and folding it into the family that
	// answers "who was given what" would cost an administrator the view they
	// actually use. See internal/api/auth_audit.go.
	"signins": {"session.*"},
}

// activityParams is every query parameter GET /admin/audit reads; any other is
// refused, so a misspelled filter is not an unfiltered answer.
var activityParams = map[string]bool{"q": true, "category": true, "severity": true, "limit": true,
	"since": true, "until": true, "before": true, "before_id": true, "app": true,
	"actor": true, "action": true}

const activityParamList = "q, category, action, actor, app, severity, since, until, limit, before and before_id"

// activityAction is one action, or a family on a trailing '*': `store.presign`,
// `session.*`. Anything else is a typo, and a typo that matched nothing would
// read as "this never happened".
var activityAction = regexp.MustCompile(`^[a-z][a-z0-9_.]*\*?$`)

// activitySeverities is what `severity` may name, alone or comma-separated.
var activitySeverities = map[string]bool{
	database.EventSeverityInfo: true, database.EventSeverityWarning: true, database.EventSeverityError: true,
}

// GET /api/v1/admin/audit — the merged feed.
func (s *Server) handleListActivity(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		// 404 rather than 403, matching /system/settings: the existence of an
		// install-wide audit trail, and its shape, is not something a
		// non-administrator is owed. An app's own events are a different
		// question and are answered on the app.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	q := r.URL.Query()
	// A parameter the feed does not read is refused rather than ignored. An
	// ignored filter answers with every row, which reads exactly like a filter
	// that matched them all: `?action=store.presign` did, and the runbook
	// built on it looked as if it worked (drills rerun 2026-10-22 #9).
	for name := range q {
		if activityParams[name] {
			continue
		}
		msg := fmt.Sprintf("the activity feed has no %q parameter — it takes %s", name, activityParamList)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": "unknown_parameter",
			"hint": "pass only the parameters the feed reads: " + activityParamList})
		return
	}
	f := database.ActivityFilter{
		Query:    strings.TrimSpace(q.Get("q")),
		ActorID:  q.Get("actor"),
		Action:   q.Get("action"),
		BeforeID: q.Get("before_id"),
		Limit:    atoiOr(q.Get("limit"), database.DefaultActivityLimit),
	}
	// A filter value the feed cannot match is refused for the reason an
	// unknown parameter is: `?severity=errors` and `?action=Store Presign`
	// would each answer an empty feed, which on an audit trail is a finding.
	if f.Action != "" && !activityAction.MatchString(f.Action) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "`action` must be one action, such as store.presign, or a family ending in *, such as session.*",
			"code":  "invalid_parameter", "hint": "pass action as it is printed in the feed; q matches part of one",
		})
		return
	}
	if raw := q.Get("severity"); raw != "" {
		for _, sev := range strings.Split(raw, ",") {
			if !activitySeverities[sev] {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "`severity` must be info, warning or error, or several separated by commas",
					"code":  "invalid_parameter", "hint": "pass severity=warning,error for everything refused or failed",
				})
				return
			}
			f.Severities = append(f.Severities, sev)
		}
	}
	if f.Limit <= 0 || f.Limit > database.MaxActivityLimit {
		f.Limit = database.DefaultActivityLimit
	}
	// A window that will not parse is refused for the reason an unknown
	// parameter is: dropped, `?since=2026-01-01` — a date, which is what a
	// person types — answered with the whole log, and a malformed `before`
	// restarted a paging client at the first page forever.
	for _, win := range []struct {
		name string
		into *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}, {"before", &f.Before}} {
		raw := q.Get(win.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "`" + win.name + "` must be an RFC 3339 timestamp, such as 2026-01-01T00:00:00Z",
				"code":  "invalid_parameter", "hint": "pass the time as RFC 3339, like 2026-01-01T00:00:00Z",
			})
			return
		}
		*win.into = t
	}

	// One app's history, by id: a slug outlives nothing (a deleted app's can
	// be taken by the next one) and the id is the one name its whole history
	// shares. A well-formed id with no live app still answers, because a
	// deleted app's history is what somebody arrives looking for.
	if app := q.Get("app"); app != "" {
		if _, err := uuid.Parse(app); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "`app` must be an app id", "code": "invalid_parameter", "hint": "pass app as an app id; GET /api/v1/apps lists them",
			})
			return
		}
		f.AppID = app
	}

	switch cat := q.Get("category"); cat {
	case "", "all":
	case "system":
		f.Source = "event"
	default:
		actions, ok := activityCategories[cat]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown category", "hint": "pass category as deploys, access, settings or signins, or leave it out"})
			return
		}
		f.Actions = actions
	}

	rows, err := s.db.ListActivity(f)
	if err != nil {
		log.Printf("listing activity: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if rows == nil {
		rows = []database.ActivityEntry{}
	}

	// The cursor for the next page rides on the response rather than being
	// reassembled by the client from the last row: keyset paging is only
	// correct if both halves agree on what "the last row" was, and a feed that
	// grows while it is being read is exactly where an off-by-one shows up as a
	// silently skipped entry.
	resp := map[string]interface{}{"entries": rows}
	// Where attribution starts, so a narrowed history from before it is not
	// read as complete: older rows carry an app only when it was their target.
	if f.AppID != "" {
		if started, err := s.db.AppScopeStartedAt(); err == nil && !started.IsZero() {
			resp["app_scope_started"] = started.Format(time.RFC3339)
		}
	}
	// Who the feed was narrowed to, by the name the Who column prints: the
	// page arrives here from a link holding only an id, and an actor with no
	// rows on this page has no row to read the name from.
	if f.ActorID != "" {
		if label, err := s.db.ActivityActorLabel(f.ActorID); err == nil {
			resp["actor_label"] = label
		}
	}
	if len(rows) >= f.Limit {
		last := rows[len(rows)-1]
		resp["next_before"] = last.OccurredAt.Format(time.RFC3339Nano)
		resp["next_before_id"] = last.ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func atoiOr(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return fallback
}
