package database_test

import (
	"fmt"
	"sort"
	"testing"

	"toft/internal/database/dbtest"
)

// The authorship idiom, enforced.
//
// **Every column naming a person is a foreign key to users(id).** That rule has
// been written down in docs/dev-reference.md since ADR §49 and enforced
// nowhere, so it drifted back: five of the nine columns the second review
// corrected were written *after* §49 fixed exactly this, in migrations 16, 29,
// 31 and 32, each one adopting the nullable spelling and skipping the
// constraint. A schema that has been reviewed once and is not checked will need
// reviewing again, and the second review costs what the first one did.
//
// This is the test that stops there being a third. A sixth drift fails here and
// names the column, rather than surviving a review and being found by the next
// one.
//
// What it does *not* check is the delete action, and that is deliberate rather
// than an omission. `ON DELETE SET NULL` is the rule for an attribution — the
// row outlives the person and clearing gives what the migrations that refused
// the constraint actually wanted — but a column that is NOT NULL cannot take
// it, and for those the row *is* the person: a personal key, an identity, a
// group membership. Both readings are correct and which applies is a per-table
// decision the ADR carries. Encoding it here would be a second rule with its
// own exception list, which is how two answers to one question come to exist.
func TestAuthorshipColumnsReference(t *testing.T) {
	db := dbtest.New(t)

	// The columns that look like authorship and are not. Each is here with the
	// reason it is here, because an exception list without reasons is a list
	// that grows.
	exceptions := map[string]string{
		// The one place a foreign key is exactly wrong. An audit record has to
		// survive the deletion of its actor — that is most of the point of
		// keeping it — and a foreign key is precisely the thing that would
		// prevent the deletion or erase the attribution (ADR §49).
		"audit_log.actor_id": "an audit record outlives its actor, by design",
		// Not a person: the category of trigger — manual, auto, rollback,
		// render — with a CHECK carrying the whole vocabulary. Not renamed,
		// because the name is on the wire in models.go and the SPA reads it.
		"deployments.triggered_by": "a trigger category, not a person",
		// Not a person: whichever node won the claim. Not renamed, for
		// deployments.triggered_by's reason.
		"job_runs.claimed_by": "a worker identity, not a person",
		"job_runs.started_by": "how a run started (app, viewer, cli, schedule, ui); the person is actor_id",
		// Not an attribution at all: a dimension of a counter and part of the
		// primary key, where '' is a real value meaning an anonymous viewer of
		// a public app. A deleted user's traffic is still a fact, and an
		// accounting record that vanishes when its subject does cannot answer
		// "what did we spend last quarter" (00001, and the same argument
		// api_key_usage_daily.key_id makes).
		"app_usage_daily.user_id": "a counter dimension, not an attribution",
		// The person a key's traffic belonged to, written on the day of the
		// traffic. A foreign key with SET NULL would erase exactly what the
		// column exists to keep: who opened the app, after the account is gone.
		"app_usage_daily.key_owner_user_id": "a counter's attribution that has to outlive the person",
	}

	// Landed with this test and emptied by the migration that corrects them.
	// The list is the specification: it shrinks in a diff, which is what makes
	// the fix reviewable rather than a wall of DDL.
	// Emptied by the migration that corrected them, which is what the list was
	// for: it landed holding seven and shrank in a diff. It stays as a map
	// rather than being deleted so the next review has somewhere to put its own
	// failures before it fixes them.
	knownDrift := map[string]string{}

	rows, err := db.Conn().Query(`
		SELECT c.table_name, c.column_name,
		       EXISTS (
		           SELECT 1
		           FROM information_schema.key_column_usage k
		           JOIN information_schema.referential_constraints r
		             ON r.constraint_name = k.constraint_name
		            AND r.constraint_schema = k.constraint_schema
		           JOIN information_schema.constraint_column_usage u
		             ON u.constraint_name = r.unique_constraint_name
		            AND u.constraint_schema = r.unique_constraint_schema
		           WHERE k.table_schema = c.table_schema
		             AND k.table_name = c.table_name
		             AND k.column_name = c.column_name
		             AND u.table_name = 'users'
		             AND u.column_name = 'id'
		       ) AS references_users
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema
		 AND t.table_name = c.table_name
		 AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema = 'public'
		  AND (c.column_name LIKE '%\_by'
		       OR c.column_name LIKE '%user\_id'
		       OR c.column_name IN ('actor_id', 'requester_id', 'approver_id', 'granted_by'))
		ORDER BY c.table_name, c.column_name`)
	if err != nil {
		t.Fatalf("reading the authorship columns: %v", err)
	}
	defer rows.Close()

	var offenders, drifted []string
	seen := 0
	for rows.Next() {
		var table, column string
		var referencesUsers bool
		if err := rows.Scan(&table, &column, &referencesUsers); err != nil {
			t.Fatalf("scanning the authorship columns: %v", err)
		}
		seen++
		name := table + "." + column
		if referencesUsers {
			if reason, ok := exceptions[name]; ok {
				t.Errorf("%s references users(id) but is on the exception list as %q — "+
					"remove it from the list rather than leaving two answers to one question",
					name, reason)
			}
			if _, ok := knownDrift[name]; ok {
				drifted = append(drifted, name)
			}
			continue
		}
		if _, ok := exceptions[name]; ok {
			continue
		}
		if _, ok := knownDrift[name]; ok {
			continue
		}
		offenders = append(offenders, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the authorship columns: %v", err)
	}

	// A query that matched nothing would pass silently, which is the failure
	// mode a test like this actually has.
	if seen == 0 {
		t.Fatal("no authorship-shaped columns found at all; the query above proved nothing")
	}

	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s names a person and is not a foreign key to users(id). "+
			"Every column naming a person is one, with ON DELETE SET NULL where the row "+
			"outlives the person — see docs/invariants.md § The schema's own rules. If it "+
			"does not name a person, add it to the exception list above with the reason",
			o)
	}

	sort.Strings(drifted)
	for _, d := range drifted {
		t.Errorf("%s is on the known-drift list and has been corrected — take it off the list", d)
	}

	if len(knownDrift) > 0 && len(offenders) == 0 && len(drifted) == 0 {
		t.Log(knownDriftNote(knownDrift))
	}
}

func knownDriftNote(drift map[string]string) string {
	names := make([]string, 0, len(drift))
	for n := range drift {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Sprintf("%d authorship columns are still on the known-drift list: %v", len(names), names)
}
