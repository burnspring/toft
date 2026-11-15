package database_test

import (
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// **An app is born listable, on every path that creates one.**
//
// Asserted by reading the row back rather than by trusting the App the caller
// was handed: CreateApp sets the field on the struct, so an assertion against
// that struct passes whether or not the column exists. What this catches is the
// opposite — a row whose column says something the struct does not — and the
// failure it is really here for is a *fifth* creation path added later that
// never mentions the field at all.
//
// That path inherits the column's DEFAULT and works, which is exactly the
// problem: it works right up until the day somebody wants to change the
// default, and then it is the one app in the estate that answers differently
// with nothing in the code to point at. There is no INSERT column list that
// could catch it, because the whole point is that the new path did not write
// one — only a test that reaches for the row can.
func TestANewAppIsBornListable(t *testing.T) {
	db := dbtest.New(t)

	owner := mkUserIn(t, db, "u-born-listable", database.SystemRolePublisher, nil)

	// Path 1: CreateApp.
	plain := &database.App{
		Slug: "born-plain", Name: "born-plain", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/born-plain", Branch: "main",
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
	}
	if err := db.CreateApp(plain); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	// Path 2: CreateAppTx, which writes the owner grant in the same
	// transaction and is the path every real creation actually takes.
	inTx := &database.App{
		Slug: "born-tx", Name: "born-tx", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/born-tx", Branch: "main",
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
	}
	if err := db.CreateAppTx(inTx, "", owner.ID, "", "", ""); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}

	for _, created := range []*database.App{plain, inTx} {
		if !created.Listable {
			t.Errorf("%s: the App handed back to the caller says listable=false — the first build is "+
				"enqueued with this struct and the Sharing tab draws its control from it, so a struct "+
				"disagreeing with its own row renders the switch off on the page that just created the app",
				created.Slug)
		}
		// The column itself, not GetApp — a re-read goes back through
		// appColumns, and this assertion has to survive somebody changing what
		// that list selects.
		var stored bool
		if err := db.Conn().QueryRow(
			`SELECT listable FROM apps WHERE id = ?`, created.ID).Scan(&stored); err != nil {
			t.Fatalf("reading apps.listable for %s: %v", created.Slug, err)
		}
		if !stored {
			t.Errorf("%s: the stored row says listable=false — a creation path that does not write the "+
				"column must still inherit a default of true, or the app is invisible to an install "+
				"whose administrator never turned anything off", created.Slug)
		}
	}
}

// **The column subtracts, and it can never add.**
//
// The one bug this feature would be reported as is an app hidden from somebody
// it was *granted* to: an owner un-lists an app, and a colleague who was given
// viewer on it watches it disappear from their list. Nothing on any screen says
// why, and the app is simply not there.
//
// So the property is asserted over every rung rather than over a case or two.
// `listable = false` must remove the app from the listing of somebody holding
// nothing, and must remove it from nobody else's — including the `list` rung
// itself, which is the rung this column is a narrowing of and the easiest one
// to take away by accident.
func TestTheListableColumnOnlySubtractsFromTheInstallWideRung(t *testing.T) {
	db := dbtest.New(t)

	app := mkVisibleApp(t, db, "withdrawn", database.VisibilityPrivate, "flask")

	// The switch is on by default, so nothing is written to turn it on.
	for _, tc := range []struct {
		name   string
		role   string
		listed bool
	}{
		{name: "nobody", role: "", listed: false},
		{name: "the rung itself", role: database.ObjectRoleList, listed: true},
		{name: "viewer", role: database.ObjectRoleViewer, listed: true},
		{name: "editor", role: database.ObjectRoleEditor, listed: true},
		{name: "owner", role: database.ObjectRoleOwner, listed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := mkUserIn(t, db, "u-sub-"+tc.name, database.SystemRolePublisher, nil)
			grantTo(t, db, app.ID, database.PrincipalTypeUser, user.ID, tc.role)

			// Listable: everybody sees it, which is what the install-wide
			// default is for.
			listed, err := db.ListApps(user.ID, database.PrincipalTypeUser, nil)
			if err != nil {
				t.Fatalf("ListApps: %v", err)
			}
			if !slugs(listed)[app.Slug] {
				t.Fatalf("holding %q, the app was absent while listable — the install-wide default "+
					"lists every app to everyone signed in", tc.role)
			}

			// Un-listed: only the grantless lose it.
			if err := db.SetAppListable(app.ID, true, false,
				database.PrincipalTypeUser, database.BuiltinAdminID, "", "", "", ""); err != nil {
				t.Fatalf("SetAppListable(false): %v", err)
			}
			listed, err = db.ListApps(user.ID, database.PrincipalTypeUser, nil)
			if err != nil {
				t.Fatalf("ListApps: %v", err)
			}
			if got := slugs(listed)[app.Slug]; got != tc.listed {
				if tc.listed {
					t.Errorf("holding %q, the app vanished when its owner un-listed it — the column may "+
						"only take back what the install gave, and a grant always outranks it", tc.role)
				} else {
					t.Error("holding nothing, the app was still listed after its owner un-listed it — " +
						"the column is what takes the install-wide disclosure back")
				}
			}

			// And the authorizer agrees with the listing, which is the whole
			// reason both read one fact: an app in somebody's list that 404s
			// when they open it is the disagreement this shape prevents.
			fresh, err := db.GetApp(app.ID)
			if err != nil {
				t.Fatalf("GetApp: %v", err)
			}
			p := &auth.Principal{
				Type: database.PrincipalTypeUser, ID: user.ID,
				SystemRole: database.SystemRolePublisher,
			}
			// Authorize's answer, because the install-wide floor lives there;
			// the ladder beneath it reports explicit relationships only.
			role := auth.Authorize(p, auth.ActionRead, fresh.AuthObject(), db).EffectiveRole
			if tc.role == "" && role != "none" {
				t.Errorf("holding nothing on an un-listed app, the effective role is %q, want none — "+
					"the floor must fall with the listing, or the app is refused with a door instead "+
					"of a 404 after its owner withdrew it", role)
			}
			if tc.role != "" && auth.RolePrecedence(role) < auth.RolePrecedence(tc.role) {
				t.Errorf("holding %q on an un-listed app, the effective role fell to %q — the column "+
					"reached a grant, which it must never do", tc.role, role)
			}

			// Put it back for the next rung.
			if err := db.SetAppListable(app.ID, false, true,
				database.PrincipalTypeUser, database.BuiltinAdminID, "", "", "", ""); err != nil {
				t.Fatalf("SetAppListable(true): %v", err)
			}
		})
	}
}

// The column is inert above `private`, which is the sentence the Sharing tab's
// copy makes and the reason the control is absent there. A public or
// signed-in-visible app is readable by everyone who can sign in, so it reaches
// the listing through readableAppsPredicate's visibility clause — a fragment
// the column is deliberately not ANDed onto.
//
// If this fails, the control being hidden above `private` has become a lie: an
// owner would have an app withdrawn from listings with no switch anywhere to
// put it back.
func TestUnListingIsInertOnAnAppEverybodyCanOpen(t *testing.T) {
	db := dbtest.New(t)

	stranger := mkUserIn(t, db, "u-inert", database.SystemRolePublisher, nil)

	for _, visibility := range []string{database.VisibilityAuthenticated, database.VisibilityPublic} {
		t.Run(visibility, func(t *testing.T) {
			app := mkVisibleApp(t, db, "inert-"+visibility, visibility, "flask")
			if err := db.SetAppListable(app.ID, true, false,
				database.PrincipalTypeUser, database.BuiltinAdminID, "", "", "", ""); err != nil {
				t.Fatalf("SetAppListable: %v", err)
			}
			listed, err := db.ListApps(stranger.ID, database.PrincipalTypeUser, nil)
			if err != nil {
				t.Fatalf("ListApps: %v", err)
			}
			if !slugs(listed)[app.Slug] {
				t.Errorf("a %s app disappeared from a stranger's listing when it was un-listed — the "+
					"column is inert above private, which is why no control for it is drawn there",
					visibility)
			}
		})
	}
}

// The audit row, because an authorization-relevant mutation that leaves no
// record is the finding rather than the feature.
func TestUnListingIsAudited(t *testing.T) {
	db := dbtest.New(t)

	app := mkVisibleApp(t, db, "audited-withdrawal", database.VisibilityPrivate, "flask")
	if err := db.SetAppListable(app.ID, true, false,
		database.PrincipalTypeUser, database.BuiltinAdminID, "req-1", "10.0.0.1", "ua", ""); err != nil {
		t.Fatalf("SetAppListable: %v", err)
	}

	var before, after string
	err := db.Conn().QueryRow(
		`SELECT COALESCE(before::text, ''), COALESCE(after::text, '')
		   FROM audit_log WHERE target_id = ? AND action = 'app.listable_change'`,
		app.ID).Scan(&before, &after)
	if err != nil {
		t.Fatalf("un-listing an app wrote no app.listable_change row (%v) — this decides who may know "+
			"the app exists, and the invariant is that an authorization-relevant mutation is audited "+
			"in the same transaction as the change", err)
	}
	// Both figures, because "who withdrew this app and from what" has to be
	// answerable from the audit log alone.
	if !strings.Contains(before, "true") || !strings.Contains(after, "false") {
		t.Errorf("the audit row reads before=%s after=%s — it must carry both values", before, after)
	}

	// A row that was not there is a change that did not happen.
	if err := db.SetAppListable("no-such-app", true, false,
		database.PrincipalTypeUser, database.BuiltinAdminID, "", "", "", ""); err == nil {
		t.Error("SetAppListable on a missing app returned nil — it must refuse rather than commit an " +
			"audit row for a change to nothing")
	}
}

// **BornUnlisted is the only creation-time input to the column, and it reaches
// the row on both paths.**
//
// The sibling of TestANewAppIsBornListable and its mirror image. That one holds
// the default; this one holds the single exception to it — a gallery item that
// asked to be born hidden — and asserts the same two things about it: the
// struct the caller goes on using, and the row underneath.
//
// It is a *creation-time* input and nothing else. After the row exists the only
// writer is SetAppListable, which audits the transition; this field is not a
// second way to change an app's listability later, and a test that let it look
// like one would be inviting exactly that.
func TestBornUnlistedReachesTheRowOnBothPaths(t *testing.T) {
	db := dbtest.New(t)

	owner := mkUserIn(t, db, "u-born-hidden", database.SystemRolePublisher, nil)

	plain := &database.App{
		Slug: "hidden-plain", Name: "hidden-plain", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/hidden-plain", Branch: "main",
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
		BornUnlisted: true,
	}
	if err := db.CreateApp(plain); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	inTx := &database.App{
		Slug: "hidden-tx", Name: "hidden-tx", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/hidden-tx", Branch: "main",
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
		BornUnlisted: true,
	}
	if err := db.CreateAppTx(inTx, "", owner.ID, "", "", ""); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}

	for _, created := range []*database.App{plain, inTx} {
		if created.Listable {
			t.Errorf("%s: the App handed back says listable=true after BornUnlisted — the struct is what "+
				"the caller serializes back, so the Sharing tab would draw the switch on for an app the "+
				"row says is hidden", created.Slug)
		}
		var stored bool
		if err := db.Conn().QueryRow(
			`SELECT listable FROM apps WHERE id = ?`, created.ID).Scan(&stored); err != nil {
			t.Fatalf("reading apps.listable for %s: %v", created.Slug, err)
		}
		if stored {
			t.Errorf("%s: the stored row says listable=true — BornUnlisted did not reach the INSERT, and "+
				"an item that asked to be born hidden is listed to every signed-in person on the install",
				created.Slug)
		}
	}
}
