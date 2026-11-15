package api

import (
	"toft/internal/database"
)

// What deleting an app removes, and what it leaves — a description of this
// binary's delete path (handleDeleteApp), checked against the schema rather
// than remembered: every table keyed by an app is either removed with it by a
// cascading foreign key, read from the schema at request time, or named here
// as left behind, with why and until when. TestDeletionListMatchesTheSchema
// fails on a table that is neither.

// leftBehind is one app-keyed table a delete does not empty.
type leftBehind struct {
	Reason string `json:"reason"`
	Until  string `json:"until"`
}

// DeletionLeftBehind is the list, and it is the specification.
var DeletionLeftBehind = map[string]leftBehind{
	"audit_log": {"the record of what was done survives its subject, by design (ADR §45)",
		"the access record's retention, if one is set"},
	"app_usage_daily":            {"who opened it is part of the access record", "the access record's retention, if one is set"},
	"api_key_usage_daily":        {"what a key did is part of the access record", "the access record's retention, if one is set"},
	"app_audience_daily":         {"how many anonymous browsers opened it, with nobody in it", "the access record's retention, if one is set"},
	"app_capability_usage_daily": {"a counter of what the app was granted and used, kept for the estate's history", "kept"},
	"app_consumption_daily":      {"what the app cost is the install's accounting", "the consumption roll-up's retention"},
	"app_consumption_intervals":  {"the intervals behind the cost", "the consumption roll-up's retention"},
	"integration_placements":     {"objects the app left in another system are listed so they can be found there", "kept, by construction"},
}

// deletionNotRows is everything a delete touches or leaves that is not a row.
var deletionNotRows = struct {
	Removed []string `json:"removed"`
	Left    []string `json:"left"`
}{
	Removed: []string{
		"the running workload, stopped and deleted (an error does not block the delete)",
		"APPS_DIR/<app-id>: every release and its files, through the trash",
		"JOB_RUN_DIR/<app-id> and LOGS_DIR/<app-id>: its job-run directories and its output files",
		"every artifact in the artifact store (a store that refuses keeps the app)",
		"the app's OS account, where apps run as accounts of their own",
	},
	Left: []string{
		"files in attached storage — the storage is the administrator's",
		"the credentials themselves — only their attachment goes",
		"a job run already in flight, which runs to its end",
		"bucket versions and backups, which are the operator's",
		"copies of the app's output a log collector already holds",
	},
}

// deletionFor is the section for one app: what goes and what stays, with
// this app's row counts beside each table.
func deletionFor(db *database.DB, appID string) map[string]any {
	keys, err := db.AppForeignKeys()
	if err != nil {
		return map[string]any{"error": "the schema could not be read: " + err.Error()}
	}
	cascades := map[string]string{}
	for _, k := range keys {
		if k.DeleteRule == "CASCADE" {
			cascades[k.Table] = k.Column
		}
	}
	keyed, err := db.AppKeyedTables()
	if err != nil {
		return map[string]any{"error": "the schema could not be read: " + err.Error()}
	}
	stays := map[string]string{}
	for t := range DeletionLeftBehind {
		if c, ok := keyed[t]; ok {
			stays[t] = c
		}
	}
	removed := db.AppRowCounts(appID, cascades)
	left := db.AppRowCounts(appID, stays)
	leftRows := map[string]any{}
	for t, n := range left {
		leftRows[t] = map[string]any{"rows": n, "reason": DeletionLeftBehind[t].Reason, "until": DeletionLeftBehind[t].Until}
	}
	return map[string]any{
		"rows_removed": removed,
		"rows_left":    leftRows,
		"also_removed": deletionNotRows.Removed,
		"also_left":    deletionNotRows.Left,
	}
}
