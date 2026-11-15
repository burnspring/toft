package database_test

import (
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// connect makes one connection of a kind, offered to apps.
func connect(t *testing.T, db *database.DB, id, kind, name string) *database.IntegrationConnection {
	t.Helper()
	c := &database.IntegrationConnection{
		ID: id, Kind: kind, Name: name,
		ForApps:            true,
		DefaultDestination: database.Destination{ID: "C0ADMIN", Label: "admin"},
	}
	if err := db.SaveConnection(c, "", database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	return c
}

// An app has no integration until an administrator attaches one. Not "off" —
// absent: there is no row, nothing to resolve, and no possibility of a send.
// This is the gate the whole feature turns on.
func TestAppWithNoAttachmentResolvesToNothing(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "unattached")
	connect(t, db, "iconn-a", "slack", "Workspace")

	row, err := db.GetAppIntegration(app.ID, "slack")
	if err != nil {
		t.Fatalf("GetAppIntegration: %v", err)
	}
	if row != nil {
		t.Fatal("an app nobody attached Slack to has a row")
	}

	a, err := db.ResolveAttachment(app.ID, "slack")
	if err != nil {
		t.Fatalf("ResolveAttachment: %v", err)
	}
	// Connected and not attached, which is the distinction the old resolver
	// could not draw: they are two refusals with two different fixes.
	if !a.Connected {
		t.Error("a connection exists and offers itself to apps, so the install is connected")
	}
	if a.Attached || a.Sendable() {
		t.Fatalf("resolved to %+v, want unattached", a)
	}

	if _, ok, err := db.ResolveDestination(app.ID, "slack"); err != nil || ok {
		t.Fatalf("ResolveDestination on an unattached app = %v, %v", ok, err)
	}
}

// Attaching does not choose a destination: an empty one means the connection's
// default, which is what an administrator gets immediately rather than after a
// second person does a second thing.
func TestAttachedAppFallsBackToTheConnectionDefault(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "attached")
	conn := connect(t, db, "iconn-b", "slack", "Workspace")

	if err := db.AttachAppIntegration(&database.AppIntegration{
		AppID: app.ID, Kind: "slack", ConnectionID: conn.ID, Enabled: true,
	}, "", database.RequestMeta{}); err != nil {
		t.Fatalf("AttachAppIntegration: %v", err)
	}

	dest, ok, err := db.ResolveDestination(app.ID, "slack")
	if err != nil {
		t.Fatalf("ResolveDestination: %v", err)
	}
	if !ok {
		t.Fatal("an attached app should resolve as sendable")
	}
	if dest.ID != "C0ADMIN" {
		t.Errorf("destination = %q, want the connection's default", dest.ID)
	}
}

// The administrator's per-app switch closes the gate without destroying the
// destination the app's people chose, so turning it back on restores their
// intent. Detaching is the destructive one, and is a different verb for that
// reason.
func TestDisableKeepsTheDestinationAndDetachForgetsIt(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "toggled")
	conn := connect(t, db, "iconn-c", "slack", "Workspace")

	attach := func(enabled bool) {
		t.Helper()
		if err := db.AttachAppIntegration(&database.AppIntegration{
			AppID: app.ID, Kind: "slack", ConnectionID: conn.ID, Enabled: enabled,
		}, "", database.RequestMeta{}); err != nil {
			t.Fatalf("AttachAppIntegration(%v): %v", enabled, err)
		}
	}

	attach(true)
	if err := db.SetAppIntegrationDestination(app.ID, "slack",
		database.Destination{ID: "C0TEAM", Label: "team-payroll"}, "",
		database.RequestMeta{}); err != nil {
		t.Fatalf("SetAppIntegrationDestination: %v", err)
	}

	// Switched off: the gate is shut...
	attach(false)
	if a, _ := db.ResolveAttachment(app.ID, "slack"); a.Sendable() {
		t.Error("a switched-off app is still sendable")
	}
	// ...but the choice survives.
	row, err := db.GetAppIntegration(app.ID, "slack")
	if err != nil || row == nil {
		t.Fatalf("GetAppIntegration after disable: %v", err)
	}
	if row.Destination.ID != "C0TEAM" {
		t.Errorf("disabling forgot the destination: %q", row.Destination.ID)
	}

	// Back on, and their intent is restored without anyone re-entering it.
	attach(true)
	dest, ok, _ := db.ResolveDestination(app.ID, "slack")
	if !ok || dest.ID != "C0TEAM" {
		t.Errorf("re-enabled app resolved to (%q, %v)", dest.ID, ok)
	}

	// Detach is the one that forgets.
	if err := db.DetachAppIntegration(app.ID, "slack", "", database.RequestMeta{}); err != nil {
		t.Fatalf("DetachAppIntegration: %v", err)
	}
	if row, _ := db.GetAppIntegration(app.ID, "slack"); row != nil {
		t.Error("detach left a row behind")
	}
}

// An app owner cannot give themselves an integration. A handler that forgot to
// check would otherwise silently create the attachment.
func TestSettingADestinationOnAnUnattachedAppIsRefused(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "no-grant")
	connect(t, db, "iconn-d", "slack", "Workspace")

	err := db.SetAppIntegrationDestination(app.ID, "slack",
		database.Destination{ID: "C1", Label: "general"}, "", database.RequestMeta{})
	if err == nil {
		t.Fatal("setting a destination on an unattached app was allowed")
	}
	if row, _ := db.GetAppIntegration(app.ID, "slack"); row != nil {
		t.Error("a refused write created the attachment anyway")
	}
}

// Switching a connection off closes the gate for every app on it at once, and
// that is what makes "disable is immediate and unrefusable" true while "delete
// is refused while apps are attached" is also true.
func TestSwitchingAConnectionOffClosesTheGateForEveryAppOnIt(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "on-a-connection")
	conn := connect(t, db, "iconn-e", "slack", "Workspace")

	if err := db.AttachAppIntegration(&database.AppIntegration{
		AppID: app.ID, Kind: "slack", ConnectionID: conn.ID, Enabled: true,
	}, "", database.RequestMeta{}); err != nil {
		t.Fatalf("AttachAppIntegration: %v", err)
	}

	conn.ForApps = false
	if err := db.SaveConnection(conn, "", database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	a, err := db.ResolveAttachment(app.ID, "slack")
	if err != nil {
		t.Fatalf("ResolveAttachment: %v", err)
	}
	if a.Connected || a.Sendable() {
		t.Errorf("an app on a switched-off connection is still sendable: %+v", a)
	}
	// The attachment itself survives, so switching the connection back on
	// restores every app at once rather than asking somebody to re-attach eight.
	if !a.Attached {
		t.Error("switching a connection off forgot which apps were on it")
	}
}

// The credential is a credential: it round-trips as stored ciphertext, and the
// audit row records that it changed without recording what it is.
func TestConnectionRoundTripAndAuditRedaction(t *testing.T) {
	db := dbtest.New(t)

	c := &database.IntegrationConnection{
		ID: "iconn-audit", Kind: "slack", Name: "Workspace",
		ScopeID:            "T0EXAMPLE",
		ScopeLabel:         "example.slack.com",
		Secret:             "ciphertext-stand-in",
		DefaultDestination: database.Destination{ID: "C0ADMIN", Label: "admin"},
		ForPlatform:        true,
		ForApps:            true,
		PlatformAudiences:  []string{"admin", "app"},
	}
	if err := db.SaveConnection(c, "", database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	got, err := db.GetConnection(c.ID)
	if err != nil || got == nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Secret != "ciphertext-stand-in" || got.DefaultDestination.ID != "C0ADMIN" ||
		len(got.PlatformAudiences) != 2 {
		t.Fatalf("round-trip lost something: %+v", got)
	}

	var audited string
	if err := db.Conn().QueryRow(
		`SELECT after FROM audit_log WHERE action = 'integration.connection.create'
		 ORDER BY occurred_at DESC LIMIT 1`).Scan(&audited); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if strings.Contains(audited, "ciphertext-stand-in") {
		t.Error("the audit row contains the stored credential")
	}
	if !strings.Contains(audited, "secret_set") || !strings.Contains(audited, "secret_changed") {
		t.Errorf("audit should record that a credential is set and whether it changed: %s", audited)
	}
	// Not a credential, and deliberately in the clear: "who pointed this install
	// at that workspace" is what this log exists to answer.
	if !strings.Contains(audited, "T0EXAMPLE") || !strings.Contains(audited, "C0ADMIN") {
		t.Errorf("the scope and the default destination should be auditable in the clear: %s", audited)
	}
}

// An update with no credential keeps the stored one, which is what makes the
// form's field write-only rather than a field that clears itself every time
// somebody renames a connection.
func TestUpdatingWithoutACredentialKeepsTheStoredOne(t *testing.T) {
	db := dbtest.New(t)
	c := connect(t, db, "iconn-keep", "slack", "Workspace")
	c.Secret = "the-original"
	if err := db.SaveConnection(c, "", database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	c.Secret = ""
	c.Name = "Renamed"
	if err := db.SaveConnection(c, "", database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	got, err := db.GetConnection(c.ID)
	if err != nil || got == nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if got.Secret != "the-original" {
		t.Errorf("a rename cleared the credential: %q", got.Secret)
	}
	if got.Name != "Renamed" {
		t.Errorf("name = %q", got.Name)
	}
}

// The scope is welded. Changing a site or a URL under a live attachment would
// silently re-point every app on it, so a new scope is a new connection.
func TestTheScopeIsNotEditable(t *testing.T) {
	db := dbtest.New(t)
	c := &database.IntegrationConnection{
		ID: "iconn-weld", Kind: "slack", Name: "Workspace",
		ScopeID: "T0ORIGINAL", ScopeLabel: "original", ForApps: true,
	}
	if err := db.SaveConnection(c, "", database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	c.ScopeID = "T0SOMEWHERE-ELSE"
	c.ScopeLabel = "somewhere else"
	if err := db.SaveConnection(c, "", database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	got, _ := db.GetConnection(c.ID)
	if got.ScopeID != "T0ORIGINAL" {
		t.Errorf("the scope moved to %q — a re-pointed connection is a silent disclosure", got.ScopeID)
	}
}

// **The residue outlives the retention window.**
//
// PruneSends deletes whole rows past the horizon, and that is right for
// chatter: a message about an app is worthless once nobody is asking about it.
// It is wrong exactly once — a row saying this app put this file in that
// library is evidence about *the library*, and the thing it describes does not
// expire on a schedule an administrator set for a message log.
//
// If this regresses, nothing breaks and nothing errors. The residue list simply
// empties, months after anybody would connect the two, which is why it is one
// of the three tests that lead.
func TestPruneSendsKeepsPlacementBearingRows(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "writes-files")
	old := time.Now().UTC().AddDate(0, 0, -400)

	chatter := &database.IntegrationSend{
		ID: "send-chatter", AppID: app.ID, Kind: "slack", Action: "post",
		Status: database.SendSent, CreatedAt: old,
	}
	placed := &database.IntegrationSend{
		ID: "send-placed", AppID: app.ID, Kind: "sharepoint", Action: "upload",
		Status: database.SendSent, CreatedAt: old, PlacementID: "placement-1",
	}
	for _, s := range []*database.IntegrationSend{chatter, placed} {
		if err := db.InsertSend(s); err != nil {
			t.Fatalf("InsertSend(%s): %v", s.ID, err)
		}
	}

	// The floor, which is the shortest window an administrator can choose and
	// therefore the case this is most likely to be discovered in.
	n, _, err := db.PruneSends(database.MinSendRetentionDays)
	if err != nil {
		t.Fatalf("PruneSends: %v", err)
	}
	if n != 1 {
		t.Errorf("the sweep removed %d rows, want 1 — the message and not the placement", n)
	}

	if got, _ := db.GetAppSend(app.ID, chatter.ID); got != nil {
		t.Error("a year-old message survived the sweep")
	}
	got, err := db.GetAppSend(app.ID, placed.ID)
	if err != nil {
		t.Fatalf("GetAppSend: %v", err)
	}
	if got == nil {
		t.Fatal("the row naming an object that still exists elsewhere was deleted")
	}
	// Kept, and not kept whole: the message is chatter and expires, the record
	// of the object does not.
	if got.Body != "" {
		t.Error("a retained body survived on a placement-bearing row")
	}
}

// A placement outlives the app that made it, which is the whole reason it
// carries no foreign key. Every other per-app record on this platform cascades
// away on delete, and that is right for all of them.
func TestPlacementSurvivesAppDeletion(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "doomed")
	conn := connect(t, db, "iconn-placements", "sharepoint", "Team site")

	if err := db.InsertPlacement(&database.IntegrationPlacement{
		ID: "placement-1", ConnectionID: conn.ID, ConnectionName: conn.Name,
		Kind: "sharepoint", Name: "june.csv",
		AppID: app.ID, AppName: "Doomed",
	}); err != nil {
		t.Fatalf("InsertPlacement: %v", err)
	}

	if err := db.DeleteApp(app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	rows, err := db.ListPlacements(conn.ID, 0)
	if err != nil {
		t.Fatalf("ListPlacements: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the placement went with the app: %d rows", len(rows))
	}
	// Still readable, naming an app that no longer resolves — which is the
	// point, and why the name is stamped in at the time rather than joined.
	if rows[0].AppName != "Doomed" || rows[0].Name != "june.csv" {
		t.Errorf("the surviving row lost what it was about: %+v", rows[0])
	}
}

// A key is single-use within its window and reusable after. The pre-check in
// the handler always said so; the unique index said "forever", so a weekly
// report's second week was a 500 and the key was burnt until retention — or,
// for a send that left a placement, for good. The index is now bounded by the
// UTC day the send was created: the same day collides, a later day inserts.
func TestAnIdempotencyKeyIsReusableAfterItsWindow(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "weekly")
	day1 := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)

	first := &database.IntegrationSend{
		ID: "send-w1", AppID: app.ID, Kind: "slack", Action: "post",
		Status: database.SendSent, CreatedAt: day1, IdempotencyKey: "weekly-summary",
	}
	if err := db.InsertSend(first); err != nil {
		t.Fatalf("day 1: %v", err)
	}

	// Two replays racing past the pre-check on the same day: the index holds.
	race := &database.IntegrationSend{
		ID: "send-w1-race", AppID: app.ID, Kind: "slack", Action: "post",
		Status: database.SendQueued, CreatedAt: day1.Add(2 * time.Hour), IdempotencyKey: "weekly-summary",
	}
	if err := db.InsertSend(race); err == nil {
		t.Fatal("a second send with the same key on the same day was inserted — the index no longer holds")
	}

	// A week later the key is free, as IdempotencyWindow promised.
	second := &database.IntegrationSend{
		ID: "send-w2", AppID: app.ID, Kind: "slack", Action: "post",
		Status: database.SendQueued, CreatedAt: day1.AddDate(0, 0, 7), IdempotencyKey: "weekly-summary",
	}
	if err := db.InsertSend(second); err != nil {
		t.Fatalf("day 8 with the same key: %v — the key was burnt", err)
	}

	// And the window-bound lookup, asked on day 8, sees only day 8's send.
	got, err := db.FindSendByIdempotencyKey(app.ID, "weekly-summary", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		// Both rows are in the past relative to now; the lookup is bound to
		// the last 24 hours of wall-clock time, so neither is inside it.
		t.Errorf("a send from 2026-08 is inside a 24 h window ending now: %+v", got)
	}
}

// A connection does not issue tokens until an administrator says so, and the
// attachment reads the connection's switch rather than keeping one of its own.
// A failure here widens every attachment on upgrade.
func TestAConnectionIssuesTokensOnlyWhenSwitchedOn(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "tokens")
	conn := connect(t, db, "iconn-tok", "sharepoint", "Finance")
	if err := db.AttachAppIntegration(&database.AppIntegration{
		AppID: app.ID, Kind: "sharepoint", ConnectionID: conn.ID, Enabled: true,
	}, "", database.RequestMeta{}); err != nil {
		t.Fatalf("AttachAppIntegration: %v", err)
	}

	a, err := db.ResolveAttachment(app.ID, "sharepoint")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Sendable() || a.IssueTokens {
		t.Fatalf("a new connection resolved to %+v, want sendable without tokens", a)
	}

	conn.IssueTokens = true
	if err := db.SaveConnection(conn, "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if a, _ = db.ResolveAttachment(app.ID, "sharepoint"); !a.IssueTokens {
		t.Error("the switch on the connection did not reach the attachment")
	}
	if got, _ := db.GetConnection(conn.ID); got == nil || !got.IssueTokens {
		t.Error("the switch was not saved")
	}
}

// A send the queue refused keeps its row as the record of the refusal and
// gives up its Idempotency-Key, so the resend the 429 asks for is a new send
// and not a replay of a failed one (code review 2026-11-11, I-2).
func TestAReleasedIdempotencyKeyMatchesNothing(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "refused")
	s := &database.IntegrationSend{
		ID: "send-refused", AppID: app.ID, Kind: "slack", Action: "post",
		Status: database.SendQueued, CreatedAt: time.Now().UTC(), IdempotencyKey: "nightly-1",
	}
	if err := db.InsertSend(s); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteSend(s.ID, database.SendFailed, "the platform's send queue was full"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseSendIdempotencyKey(s.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := db.FindSendByIdempotencyKey(app.ID, "nightly-1", 24*time.Hour); err != nil || got != nil {
		t.Fatalf("a released key still matched: %+v, %v", got, err)
	}
	again := &database.IntegrationSend{
		ID: "send-again", AppID: app.ID, Kind: "slack", Action: "post",
		Status: database.SendQueued, CreatedAt: time.Now().UTC(), IdempotencyKey: "nightly-1",
	}
	if err := db.InsertSend(again); err != nil {
		t.Fatalf("the resend under the same key was refused: %v", err)
	}
}
