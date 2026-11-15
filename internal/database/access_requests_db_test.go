package database_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// Approving an access request, and the one thing it must never do.
//
// A request is asked for once and decided whenever somebody gets to it, so the
// two are far enough apart that the world moves in between — and the grant the
// approval writes is the role that was asked for weeks ago. That made
// "approved" able to mean "reduced".

// seedRequest writes one pending object access request and returns its id.
func seedRequest(t *testing.T, db *database.DB, appID, requesterID, role string) string {
	t.Helper()
	req := &database.AccessRequest{
		RequesterID:   requesterID,
		RequestType:   database.AccessReqTypeObject,
		ObjectID:      appID,
		RequestedRole: role,
	}
	if err := db.CreateAccessRequest(context.Background(), req, false, "", database.RequestMeta{}); err != nil {
		t.Fatalf("CreateAccessRequest: %v", err)
	}
	return req.ID
}

func grantRole(t *testing.T, db *database.DB, appID, userID, role string) {
	t.Helper()
	if err := db.UpsertGrant(context.Background(), appID, database.PrincipalTypeUser,
		userID, role, "", database.RequestMeta{}); err != nil {
		t.Fatalf("UpsertGrant %s=%s: %v", userID, role, err)
	}
}

func roleOn(t *testing.T, db *database.DB, appID, userID string) string {
	t.Helper()
	var role string
	err := db.Conn().QueryRow(`
		SELECT role FROM object_grants
		WHERE object_id=? AND principal_type='user' AND principal_id=?`, appID, userID).Scan(&role)
	if err != nil {
		t.Fatalf("reading the grant on %s: %v", userID, err)
	}
	return role
}

// The demotion. Somebody asks to be a viewer; before anybody decides it, the
// owner makes them an editor from the Access tab; the owner then clears the
// stale row. Approving must not be what takes the editor rung away.
func TestApprovingAnAccessRequestNeverTakesAccessAway(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "payroll")
	if err := db.UpsertUser(&database.User{ID: "ada", Login: "ada"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	reqID := seedRequest(t, db, app.ID, "ada", database.ObjectRoleViewer)
	grantRole(t, db, app.ID, "ada", database.ObjectRoleEditor)

	granted, err := db.ApproveObjectAccessRequest(context.Background(), reqID, app.ID,
		app.OwnerID, database.RequestMeta{})
	if err != nil {
		t.Fatalf("ApproveObjectAccessRequest: %v", err)
	}
	if granted != database.ObjectRoleEditor {
		t.Errorf("approval reports %q, want editor — it is what the row now says", granted)
	}
	if got := roleOn(t, db, app.ID, "ada"); got != database.ObjectRoleEditor {
		t.Fatalf("the grant is %q after approving an older viewer request, want editor — "+
			"approving demoted somebody, and the audit log calls it a grant", got)
	}

	// The request is still decided: the row does not stay pending just because
	// nothing had to move.
	req, err := db.GetAccessRequest(reqID)
	if err != nil || req == nil {
		t.Fatalf("GetAccessRequest: %v", err)
	}
	if req.Status != database.AccessReqStatusApproved {
		t.Errorf("status = %q, want approved", req.Status)
	}
}

// The ordinary approval, and the one the rung was built for: `list` is the
// weaker rung, so the ask raises it.
func TestApprovingAnAccessRequestRaisesTheStandingRung(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "ledger")
	if err := db.UpsertUser(&database.User{ID: "grace", Login: "grace"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	for _, tc := range []struct {
		name     string
		standing string
		want     string
	}{
		{"nothing at all", "", database.ObjectRoleViewer},
		{"the list rung", database.ObjectRoleList, database.ObjectRoleViewer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Conn().Exec(`DELETE FROM object_grants WHERE object_id=? AND principal_id='grace'`, app.ID); err != nil {
				t.Fatalf("clearing grants: %v", err)
			}
			if tc.standing != "" {
				grantRole(t, db, app.ID, "grace", tc.standing)
			}
			reqID := seedRequest(t, db, app.ID, "grace", database.ObjectRoleViewer)

			granted, err := db.ApproveObjectAccessRequest(context.Background(), reqID, app.ID,
				app.OwnerID, database.RequestMeta{})
			if err != nil {
				t.Fatalf("ApproveObjectAccessRequest: %v", err)
			}
			if granted != tc.want {
				t.Errorf("approval reports %q, want %q", granted, tc.want)
			}
			if got := roleOn(t, db, app.ID, "grace"); got != tc.want {
				t.Errorf("grant = %q, want %q", got, tc.want)
			}
		})
	}
}

// What the audit row says when the two differ. `role` is always what the grant
// now carries — the question an administrator reading the log is asking — and
// `requested_role` appears only to record that the ask was for less.
func TestApprovingRecordsBothRolesOnlyWhenTheyDiffer(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "closing")
	if err := db.UpsertUser(&database.User{ID: "ada", Login: "ada"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	// Decoded rather than matched as text: jsonb round-trips with its own
	// spacing, and a test that asserted the spelling would be asserting
	// Postgres's formatting rather than what the record says.
	after := func(reqID string) map[string]string {
		t.Helper()
		var out string
		err := db.Conn().QueryRow(`
			SELECT COALESCE(after::text,'') FROM audit_log
			WHERE action='access_request.approve' AND after::text LIKE '%'||?||'%'
			ORDER BY occurred_at DESC LIMIT 1`, reqID).Scan(&out)
		if err != nil {
			t.Fatalf("reading the audit row for %s: %v", reqID, err)
		}
		var fields map[string]string
		if err := json.Unmarshal([]byte(out), &fields); err != nil {
			t.Fatalf("audit row %s is not the shape this record has always had: %v", out, err)
		}
		return fields
	}

	// Asked for viewer, already an editor: both roles recorded.
	grantRole(t, db, app.ID, "ada", database.ObjectRoleEditor)
	kept := seedRequest(t, db, app.ID, "ada", database.ObjectRoleViewer)
	if _, err := db.ApproveObjectAccessRequest(context.Background(), kept, app.ID, app.OwnerID, database.RequestMeta{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	row := after(kept)
	if row["role"] != database.ObjectRoleEditor {
		t.Errorf("audit role = %q, want the role the grant now carries", row["role"])
	}
	if row["requested_role"] != database.ObjectRoleViewer {
		t.Errorf("audit requested_role = %q — the log cannot say the ask was for less",
			row["requested_role"])
	}

	// Asked for editor with nothing standing in the way: one role, no noise.
	if _, err := db.Conn().Exec(`DELETE FROM object_grants WHERE object_id=? AND principal_id='ada'`, app.ID); err != nil {
		t.Fatalf("clearing grants: %v", err)
	}
	plain := seedRequest(t, db, app.ID, "ada", database.ObjectRoleEditor)
	if _, err := db.ApproveObjectAccessRequest(context.Background(), plain, app.ID, app.OwnerID, database.RequestMeta{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	row = after(plain)
	if _, ok := row["requested_role"]; ok {
		t.Errorf("audit row %v carries requested_role when nothing differed", row)
	}
	if row["role"] != database.ObjectRoleEditor {
		t.Errorf("audit role = %q, want editor", row["role"])
	}
}

// Who is written to when somebody is waiting on a decision: the people who can
// make it. Approving is owner-only, so an editor told about a request is a
// person sent a message about a button they do not have.
func TestListAppDecidersIsOwnersAlone(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "deciders")

	mk := func(id string) {
		t.Helper()
		if err := db.UpsertUser(&database.User{ID: id, Login: id, Email: id + "@example.com"}); err != nil {
			t.Fatalf("UpsertUser %s: %v", id, err)
		}
		if _, err := db.Conn().Exec(`UPDATE users SET system_role=? WHERE id=?`,
			database.SystemRolePublisher, id); err != nil {
			t.Fatalf("role for %s: %v", id, err)
		}
	}
	mk("theowner")
	mk("theeditor")
	mk("theviewer")
	grantRole(t, db, app.ID, "theowner", database.ObjectRoleOwner)
	grantRole(t, db, app.ID, "theeditor", database.ObjectRoleEditor)
	grantRole(t, db, app.ID, "theviewer", database.ObjectRoleViewer)

	got, err := db.ListAppDeciders(app.ID)
	if err != nil {
		t.Fatalf("ListAppDeciders: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.Address] = true
	}
	if !seen["theowner@example.com"] {
		t.Errorf("the owner was not written to; got %v", seen)
	}
	for _, unwanted := range []string{"theeditor@example.com", "theviewer@example.com"} {
		if seen[unwanted] {
			t.Errorf("%s is not a decider and should not be mailed about a request", unwanted)
		}
	}

	// And the failure notices still reach the editor, which is the whole reason
	// these are two lists rather than one.
	all, err := db.ListAppRecipients(app.ID)
	if err != nil {
		t.Fatalf("ListAppRecipients: %v", err)
	}
	editor := false
	for _, r := range all {
		if r.Address == "theeditor@example.com" {
			editor = true
		}
	}
	if !editor {
		t.Error("narrowing the decider list narrowed the failure list with it")
	}
}

// The grant's stamp says when its current role was given, and by whom: a
// write that leaves the role unchanged — a re-save, or an approval below what
// the person already holds — leaves it alone, while the audit row still
// records the act. A role change moves both.
func TestGrantStampSurvivesASameRoleWrite(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "claims")
	for _, u := range []string{"ada", "bo"} {
		if err := db.UpsertUser(&database.User{ID: u, Login: u}); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func() (string, time.Time) {
		t.Helper()
		var by sql.NullString
		var at time.Time
		if err := db.Conn().QueryRow(`SELECT granted_by, granted_at FROM object_grants
			WHERE object_id=? AND principal_type='user' AND principal_id='ada'`, app.ID).Scan(&by, &at); err != nil {
			t.Fatal(err)
		}
		return by.String, at
	}
	put := func(role, actor string) {
		t.Helper()
		if err := db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser, "ada", role, actor,
			database.RequestMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	put(database.ObjectRoleEditor, "bo")
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.Conn().Exec(`UPDATE object_grants SET granted_at=? WHERE object_id=? AND principal_id='ada'`, old, app.ID); err != nil {
		t.Fatal(err)
	}

	put(database.ObjectRoleEditor, app.OwnerID) // a re-save
	reqID := seedRequest(t, db, app.ID, "ada", database.ObjectRoleViewer)
	if _, err := db.ApproveObjectAccessRequest(context.Background(), reqID, app.ID, app.OwnerID, database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if by, at := stamp(); by != "bo" || !at.Equal(old) {
		t.Errorf("after same-role writes the stamp is %s at %s, want bo at %s", by, at, old)
	}
	var modifies int
	if err := db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'object_grant.modify'`).Scan(&modifies); err != nil || modifies != 1 {
		t.Errorf("object_grant.modify rows = %d (%v), want the re-save recorded", modifies, err)
	}

	put(database.ObjectRoleViewer, app.OwnerID) // a role change
	if by, at := stamp(); by != app.OwnerID || !at.After(old) {
		t.Errorf("after a role change the stamp is %s at %s, want the owner, now", by, at)
	}
}

// A request withdrawn before the approval's write is not approved: the claim
// carries `status='pending'` as deny and withdraw do, and no grant is written
// (security review 2026-11-14, DB-6).
func TestApprovingAWithdrawnRequestIsRefused(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "withdrawn")
	if err := db.UpsertUser(&database.User{ID: "lin", Login: "lin"}); err != nil {
		t.Fatal(err)
	}
	reqID := seedRequest(t, db, app.ID, "lin", database.ObjectRoleViewer)
	if err := db.WithdrawAccessRequest(context.Background(), reqID, "lin", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApproveObjectAccessRequest(context.Background(), reqID, app.ID, app.OwnerID, database.RequestMeta{}); err != database.ErrRequestNotPending {
		t.Fatalf("approve after withdraw = %v, want ErrRequestNotPending", err)
	}
	var n int
	db.Conn().QueryRow(`SELECT COUNT(*) FROM object_grants WHERE object_id=? AND principal_id='lin'`, app.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d grant rows written for a withdrawn request", n)
	}
}

// An approval of a stale `viewer` request that races a raise to editor never
// lowers it: the grant is written raise-only against the row as it is once the
// raise has committed.
func TestApprovingDuringARaiseKeepsTheRaise(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "racing")
	if err := db.UpsertUser(&database.User{ID: "kim", Login: "kim"}); err != nil {
		t.Fatal(err)
	}
	grantRole(t, db, app.ID, "kim", database.ObjectRoleList)
	reqID := seedRequest(t, db, app.ID, "kim", database.ObjectRoleViewer)

	raise, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer raise.Rollback()
	if _, err := raise.Exec(`UPDATE object_grants SET role=? WHERE object_id=? AND principal_id='kim'`,
		database.ObjectRoleEditor, app.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := db.ApproveObjectAccessRequest(context.Background(), reqID, app.ID, app.OwnerID, database.RequestMeta{})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := raise.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := roleOn(t, db, app.ID, "kim"); got != database.ObjectRoleEditor {
		t.Errorf("grant = %q after a racing approval, want the raise to editor", got)
	}
}

// A double-clicked access request is one pending row: the duplicate count is
// serialised per requester and app (security review 2026-11-14, DB-11).
func TestCreateAccessRequestTwiceAtOnceIsOneRow(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "double-click")
	if err := db.UpsertUser(&database.User{ID: "dc", Login: "dc"}); err != nil {
		t.Fatal(err)
	}
	const tries = 4
	errs := make(chan error, tries)
	start := make(chan struct{})
	for i := 0; i < tries; i++ {
		go func() {
			<-start
			errs <- db.CreateAccessRequest(context.Background(), &database.AccessRequest{
				RequesterID: "dc", RequestType: database.AccessReqTypeObject,
				ObjectID: app.ID, RequestedRole: database.ObjectRoleViewer,
			}, false, "", database.RequestMeta{})
		}()
	}
	close(start)
	ok := 0
	for i := 0; i < tries; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case err != database.ErrDuplicateRequest:
			t.Errorf("create = %v, want nil or ErrDuplicateRequest", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d creates succeeded, want exactly 1", ok)
	}
}
