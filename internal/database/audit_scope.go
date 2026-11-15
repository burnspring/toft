package database

import "strings"

// Which audit actions belong to an app's history, which to the install's, and
// which can be either. The list is the specification: an action in no scope
// fails web/src/lib/audit-actions.test.ts, and a row written against its
// scope — an app-scoped action with no app_id, an install-scoped one with one
// — fails the database test that wrote it, at cleanup (dbtest).
//
// Here rather than beside the Activity chips in internal/api because the
// runtime check runs in dbtest, which cannot import the API.

// AuditScope is whose history an action is part of.
type AuditScope string

const (
	// AuditScopeApp must carry an app_id.
	AuditScopeApp AuditScope = "app"
	// AuditScopeInstall must not.
	AuditScopeInstall AuditScope = "install"
	// AuditScopeEither is decided by the row, not the action.
	AuditScopeEither AuditScope = "either"
)

// The actions only a mechanism or a migration writes, never a handler: named as
// constants so the extractor in audit-actions.test.ts sees them, and so their
// phrases are not orphans.
const (
	// ActionAuditPruned is a retention pass that removed rows.
	ActionAuditPruned = "audit_log.pruned"
	// ActionAppScopeStarted is where app attribution begins on an install that
	// had a log before audit_log.app_id existed (migration 00071).
	ActionAppScopeStarted = "audit_log.app_scope_started"
	// ActionStorageLevelPromoted is a mapping, grant or delegation migration
	// 00074 moved to `full`: a widening from `overwrite`, or a volume mapping's
	// `write` renamed with no change to its mount.
	ActionStorageLevelPromoted = "storage.level_promoted"
)

// AuditAppScope maps action patterns to their scope, in the activity chips'
// spelling: an exact action, or a prefix ending in `*`. The most specific
// pattern wins, so an exception is an exact entry beside its family.
var AuditAppScope = map[string]AuditScope{
	// An app's own life and settings.
	"app.*":                   AuditScopeApp,
	"app_source_changed":      AuditScopeApp,
	"credential.attach":       AuditScopeApp,
	"credential.detach":       AuditScopeApp,
	"dependency_waiver_*":     AuditScopeApp,
	"gallery.*":               AuditScopeApp,
	"integration.attached":    AuditScopeApp,
	"integration.detached":    AuditScopeApp,
	"integration.updated":     AuditScopeApp,
	"integration.destination": AuditScopeApp,
	"job.*":                   AuditScopeApp,
	"job_run.*":               AuditScopeApp,
	"object_grant.*":          AuditScopeApp,
	"storage_mapping.*":       AuditScopeApp,

	// Decided by the row.
	//   store.presign: an app's presign is its history, and its actor says so;
	//   a person's from /me is about a store.
	"store.presign": AuditScopeEither,
	//   store.link: built by an app (its actor) — the app's history.
	"store.link": AuditScopeApp,
	//   store.link.refused: a person's open that found nothing — the install's.
	"store.link.refused": AuditScopeInstall,
	//   access_request.*: an object-access request is about one app; a role
	//   promotion is about the install.
	"access_request.*": AuditScopeEither,
	//   deploy_source_refused: a redeploy's refusal has an app; a create's has
	//   none yet.
	"deploy_source_refused": AuditScopeEither,
	//   knowledge_base.*: which app is the install's knowledge base is the
	//   install's fact and that app's; a pick names the app, a clear may not.
	"knowledge_base.*": AuditScopeEither,
	//   storage.level_promoted: migration 00074 merging `overwrite` into
	//   `full`. A mapping's row is its app's; a grant's or a delegation's is
	//   the install's.
	"storage.level_promoted": AuditScopeEither,

	// The install's. app.create_refused_tier is the exception in its family:
	// the refused app was never made.
	"app.create_refused_tier":      AuditScopeInstall,
	"admin.*":                      AuditScopeInstall,
	"announcement*":                AuditScopeInstall,
	"audit_log.*":                  AuditScopeInstall,
	"auth_provider.*":              AuditScopeInstall,
	"build_cache.*":                AuditScopeInstall,
	"credential.*":                 AuditScopeInstall,
	"dependency_feed_credential.*": AuditScopeInstall,
	"deploy_key.*":                 AuditScopeInstall,
	"email_settings.*":             AuditScopeInstall,
	"feature.*":                    AuditScopeInstall,
	"git_connection.*":             AuditScopeInstall,
	"group.*":                      AuditScopeInstall,
	"identity.*":                   AuditScopeInstall,
	"integration.connection.*":     AuditScopeInstall,
	"integration.place.*":          AuditScopeInstall,
	"integrations.settings":        AuditScopeInstall,
	"password.*":                   AuditScopeInstall,
	"platform_settings.*":          AuditScopeInstall,
	"scim.*":                       AuditScopeInstall,
	"session.*":                    AuditScopeInstall,
	"source_pattern_*":             AuditScopeInstall,
	"source_policy_*":              AuditScopeInstall,
	"storage_drive.*":              AuditScopeInstall,
	// A manager's removal takes a mapping with it, and that is the app's own
	// storage_mapping.delete row on the app.
	"storage_manager.*": AuditScopeInstall,
	// The install's default delegations: who every new resource is delegated
	// to — a fact about the install, never about one app.
	"storage_default_manager.*": AuditScopeInstall,
	"storage_resource.*":        AuditScopeInstall,
	"store_grant.*":             AuditScopeInstall,
	"tag.*":                     AuditScopeInstall,
	"telemetry_settings.*":      AuditScopeInstall,
	"user.*":                    AuditScopeInstall,
}

// ScopeOfAuditAction is the scope of one action, by its most specific pattern,
// and false for an action in none.
func ScopeOfAuditAction(action string) (AuditScope, bool) {
	if s, ok := AuditAppScope[action]; ok {
		return s, true
	}
	best, found := "", AuditScope("")
	for pattern, s := range AuditAppScope {
		prefix, wild := strings.CutSuffix(pattern, "*")
		if wild && strings.HasPrefix(action, prefix) && len(prefix) > len(best) {
			best, found = prefix, s
		}
	}
	return found, found != ""
}

// AuditScopeViolations names every action in this database's log written
// against its scope: an app-scoped action with no app, an install-scoped one
// with one. dbtest calls it at each test database's cleanup, so every
// database-backed test is the check. An action in no scope is not reported
// here; the web guard is what fails on that.
func (db *DB) AuditScopeViolations() ([]string, error) {
	rows, err := db.conn.Query(`SELECT DISTINCT action, app_id <> '' FROM audit_log`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action string
		var hasApp bool
		if err := rows.Scan(&action, &hasApp); err != nil {
			return nil, err
		}
		switch scope, _ := ScopeOfAuditAction(action); {
		case scope == AuditScopeApp && !hasApp:
			out = append(out, action+" (an app's history, written with no app_id)")
		case scope == AuditScopeInstall && hasApp:
			out = append(out, action+" (the install's, written with an app_id)")
		}
	}
	return out, rows.Err()
}
