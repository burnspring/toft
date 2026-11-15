package api

import (
	"fmt"
	"testing"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// D8's entire mitigation.
//
// The per-app read resolves an app's integrations through
// appint.ResolveAttachment, kind by kind, which is ten queries per app by
// construction. The estate reader cannot do that — ten queries times five
// hundred apps is the page nobody opens until the morning they need it most —
// so it answers the same three booleans in SQL.
//
// **That is a second implementation of a rule, said out loud rather than
// apologised for.** What makes it safe is this test: it builds an app in every
// combination of connected / attached / enabled and asserts the two agree. The
// alternative, teaching appint a bulk resolver, is the better long-term answer
// and is a change to a package whose whole discipline is that it imports no
// transport; ADR §92 open question 1 carries it.

func TestReachAndCapabilitiesAgree(t *testing.T) {
	db := dbtest.New(t)
	if err := db.UpsertUser(&database.User{ID: "owner", Login: "owner", Email: "owner@example.com"}); err != nil {
		t.Fatalf("seeding the owner: %v", err)
	}

	cases := []struct {
		name     string
		offered  bool // a connection of the kind exists and offers itself to apps
		attached bool
		enabled  bool
	}{
		{name: "nothing-connected"},
		{name: "connected-not-attached", offered: true},
		{name: "attached-and-live", offered: true, attached: true, enabled: true},
		{name: "attached-then-disabled", offered: true, attached: true},
		// The row §5 exists for: an install that switched the kind off after an
		// app attached it. Present and not effective, never absent — an
		// administrator has to re-examine it when they switch it back on.
		{name: "attached-then-withdrawn", attached: true, enabled: true},
		{name: "attached-withdrawn-and-disabled", attached: true},
	}

	// One connection per case, because ForApps is a property of the connection
	// and two apps sharing one could not disagree about it.
	//
	// CreateApp mints the id, so the cases are indexed by the row it wrote
	// rather than by a name composed here.
	appIDs := map[string]string{}
	for i, tc := range cases {
		app := &database.App{
			Slug: tc.name, Name: tc.name, OwnerID: "owner",
			Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
		}
		if err := db.CreateApp(app); err != nil {
			t.Fatalf("CreateApp %s: %v", tc.name, err)
		}
		appIDs[tc.name] = app.ID
		if !tc.offered && !tc.attached {
			continue
		}
		conn := &database.IntegrationConnection{
			ID: fmt.Sprintf("iconn-%d", i), Kind: appint.IntegrationSlack,
			Name: tc.name, ForApps: tc.offered,
			DefaultDestination: database.Destination{ID: "C0DEF", Label: "default"},
		}
		if err := db.SaveConnection(conn, "", database.RequestMeta{}); err != nil {
			t.Fatalf("SaveConnection %s: %v", tc.name, err)
		}
		if tc.attached {
			if err := db.AttachAppIntegration(&database.AppIntegration{
				AppID: app.ID, Kind: appint.IntegrationSlack,
				ConnectionID: conn.ID, Enabled: tc.enabled,
			}, "", database.RequestMeta{}); err != nil {
				t.Fatalf("AttachAppIntegration %s: %v", tc.name, err)
			}
		}
	}

	set, err := db.EstateReach(database.AppScope{All: true})
	if err != nil {
		t.Fatalf("EstateReach: %v", err)
	}
	rows := map[string]database.EstateReachRow{}
	for _, r := range set.Rows {
		rows[r.AppID] = r
	}

	for _, tc := range cases {
		appID := appIDs[tc.name]
		row, ok := rows[appID]
		if !ok {
			t.Fatalf("%s: the estate reader omitted the app entirely", tc.name)
		}

		// What the SQL says.
		var sql database.ReachIntegration
		for _, in := range row.Integrations {
			if in.Kind == appint.IntegrationSlack {
				sql = in
			}
		}

		// What the resolver every send passes through says.
		a, err := db.ResolveAttachment(appID, appint.IntegrationSlack)
		if err != nil {
			t.Fatalf("%s: ResolveAttachment: %v", tc.name, err)
		}

		// An unattached kind is not a row on the estate reader, which is the
		// same statement as Attached being false: a kind an app was never
		// given is a thing it cannot do, not a thing it holds.
		if a.Attached != sql.Attached {
			t.Errorf("%s: appint says attached=%v, the estate query says %v",
				tc.name, a.Attached, sql.Attached)
			continue
		}
		if !a.Attached {
			continue
		}
		if a.Connected != sql.Connected || a.Enabled != sql.Enabled {
			t.Errorf("%s: appint says {connected:%v enabled:%v}, the estate query says {connected:%v enabled:%v}",
				tc.name, a.Connected, a.Enabled, sql.Connected, sql.Enabled)
		}
		if a.Destination.Label != sql.Destination {
			t.Errorf("%s: destination — appint %q, estate query %q",
				tc.name, a.Destination.Label, sql.Destination)
		}

		// And the derived word, against the list an app itself is handed at
		// startup: whether this app may actually send.
		caps, err := appint.Capabilities(db, appID)
		if err != nil {
			t.Fatalf("%s: Capabilities: %v", tc.name, err)
		}
		canSend := false
		for _, c := range caps {
			if c == appint.CapSlackPost {
				canSend = true
			}
		}
		if canSend != sql.Effective() {
			t.Errorf("%s: /app/self says the app can post = %v, the estate query's three booleans say %v",
				tc.name, canSend, sql.Effective())
		}
	}
}
