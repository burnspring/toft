package database_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func mkEvent(t *testing.T, db *database.DB, appID, kind, fingerprint string, at time.Time) *database.AppEvent {
	t.Helper()
	e := &database.AppEvent{
		AppID:       appID,
		Kind:        kind,
		Severity:    database.EventSeverityError,
		Source:      database.EventSourceAutodeploy,
		Summary:     kind + " on " + appID,
		Fingerprint: fingerprint,
		OccurredAt:  at,
	}
	if err := db.InsertAppEvent(e); err != nil {
		t.Fatalf("InsertAppEvent: %v", err)
	}
	return e
}

// The cooldown collapses mail, never the record. Two occurrences of the same
// problem inside the window both land as rows; only the first claims the right
// to send.
func TestClaimEventNotifyCooldown(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "cooldown")
	now := time.Now().UTC()

	first := mkEvent(t, db, app.ID, "git_auth_failed", "auth", now.Add(-time.Minute))
	ok, err := db.ClaimEventNotify(first.ID, app.ID, first.Kind, first.Fingerprint, 6*time.Hour)
	if err != nil {
		t.Fatalf("ClaimEventNotify: %v", err)
	}
	if !ok {
		t.Fatal("first occurrence did not win the claim")
	}

	second := mkEvent(t, db, app.ID, "git_auth_failed", "auth", now)
	ok, err = db.ClaimEventNotify(second.ID, app.ID, second.Kind, second.Fingerprint, 6*time.Hour)
	if err != nil {
		t.Fatalf("ClaimEventNotify: %v", err)
	}
	if ok {
		t.Fatal("second occurrence inside the cooldown won a claim")
	}

	// But it is still on the record — that is the whole point.
	evs, err := db.ListAppEvents(app.ID, database.AppEventFilter{})
	if err != nil {
		t.Fatalf("ListAppEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("events recorded = %d, want 2", len(evs))
	}

	// A different fingerprint is a different problem and mails on its own.
	other := mkEvent(t, db, app.ID, "git_auth_failed", "network", now)
	ok, err = db.ClaimEventNotify(other.ID, app.ID, other.Kind, other.Fingerprint, 6*time.Hour)
	if err != nil {
		t.Fatalf("ClaimEventNotify: %v", err)
	}
	if !ok {
		t.Fatal("a new fingerprint did not win a claim")
	}

	// VN-3: once resolved, the same problem coming back is told once more,
	// and a third occurrence inside the window is not.
	if _, err := db.ResolveAppEvents(app.ID, "git_auth_failed", ""); err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	back := mkEvent(t, db, app.ID, "git_auth_failed", "auth", now)
	ok, err = db.ClaimEventNotify(back.ID, app.ID, back.Kind, back.Fingerprint, 6*time.Hour)
	if err != nil {
		t.Fatalf("ClaimEventNotify: %v", err)
	}
	if !ok {
		t.Fatal("a resolved problem that came back did not win a claim")
	}
	if _, err := db.ResolveAppEvents(app.ID, "git_auth_failed", ""); err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	third := mkEvent(t, db, app.ID, "git_auth_failed", "auth", now)
	ok, err = db.ClaimEventNotify(third.ID, app.ID, third.Kind, third.Fingerprint, 6*time.Hour)
	if err != nil {
		t.Fatalf("ClaimEventNotify: %v", err)
	}
	if ok {
		t.Fatal("a third occurrence inside the window won a claim: the cap of two is gone")
	}

	// And the same fingerprint mails again once the window has passed.
	later := mkEvent(t, db, app.ID, "git_auth_failed", "auth", now)
	ok, err = db.ClaimEventNotify(later.ID, app.ID, later.Kind, later.Fingerprint, time.Nanosecond)
	if err != nil {
		t.Fatalf("ClaimEventNotify: %v", err)
	}
	if !ok {
		t.Fatal("claim refused after the cooldown expired")
	}
}

// A claim is taken exactly once even if two nodes race for it.
func TestClaimEventNotifyIsIdempotent(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "claim-once")
	e := mkEvent(t, db, app.ID, "app_crashed", "rel-1", time.Now().UTC())

	first, err := db.ClaimEventNotify(e.ID, app.ID, e.Kind, e.Fingerprint, time.Hour)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true, nil", first, err)
	}
	again, err := db.ClaimEventNotify(e.ID, app.ID, e.Kind, e.Fingerprint, time.Hour)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again {
		t.Fatal("the same event claimed the right to send twice")
	}
}

// Clearing reports how many of the closed events had been mailed, because that
// is what decides whether a recovery notice goes out.
func TestResolveAppEvents(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "resolve")
	now := time.Now().UTC()

	quiet := mkEvent(t, db, app.ID, "cron_run_failed", "job-1", now)
	loud := mkEvent(t, db, app.ID, "app_crashed", "rel-1", now)
	if _, err := db.ClaimEventNotify(loud.ID, app.ID, loud.Kind, loud.Fingerprint, time.Hour); err != nil {
		t.Fatalf("ClaimEventNotify: %v", err)
	}
	if err := db.MarkEventNoticeSent(loud.ID); err != nil {
		t.Fatalf("MarkEventNoticeSent: %v", err)
	}

	// Resolving a kind nobody was mailed about reports zero.
	n, err := db.ResolveAppEvents(app.ID, quiet.Kind, "")
	if err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	if n != 0 {
		t.Fatalf("notified count for an unmailed kind = %d, want 0", n)
	}

	n, err = db.ResolveAppEvents(app.ID, loud.Kind, "")
	if err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	if n != 1 {
		t.Fatalf("notified count = %d, want 1", n)
	}

	// Nothing is left open, so a second clear is a no-op.
	open, err := db.HasOpenAppEvent(app.ID, loud.Kind)
	if err != nil {
		t.Fatalf("HasOpenAppEvent: %v", err)
	}
	if open {
		t.Fatal("event still open after resolve")
	}
	n, err = db.ResolveAppEvents(app.ID, loud.Kind, "")
	if err != nil {
		t.Fatalf("second ResolveAppEvents: %v", err)
	}
	if n != 0 {
		t.Fatalf("second resolve reported %d mailed, want 0", n)
	}
}

// Holding the cooldown claim is not the same as anybody having been told, and
// only the second one earns a recovery notice.
func TestResolveCountsNoticeSentNotTheClaim(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "claim-vs-sent")
	e := mkEvent(t, db, app.ID, "git_auth_failed", "auth", time.Now().UTC())

	// The claim is taken even when the app's switch is off, so no backlog can
	// build up — but nobody heard, so clearing it must report zero.
	if ok, err := db.ClaimEventNotify(e.ID, app.ID, e.Kind, e.Fingerprint, time.Hour); err != nil || !ok {
		t.Fatalf("ClaimEventNotify = %v, %v", ok, err)
	}
	n, err := db.ResolveAppEvents(app.ID, e.Kind, "")
	if err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	if n != 0 {
		t.Fatalf("a claimed-but-unsent event reported %d told, want 0", n)
	}
}

// ListAppEvents is scoped by app id — the IDOR rule — and honours its filters.
func TestListAppEventsScopedAndFiltered(t *testing.T) {
	db := dbtest.New(t)
	mine := mkApp(t, db, "mine")
	theirs := mkApp(t, db, "theirs")
	now := time.Now().UTC()

	mkEvent(t, db, mine.ID, "deploy_failed", "build", now.Add(-2*time.Hour))
	warn := &database.AppEvent{
		AppID: mine.ID, Kind: "git_fetch_failed", Severity: database.EventSeverityWarning,
		Source: database.EventSourceAutodeploy, Summary: "network", OccurredAt: now,
	}
	if err := db.InsertAppEvent(warn); err != nil {
		t.Fatalf("InsertAppEvent: %v", err)
	}
	mkEvent(t, db, theirs.ID, "deploy_failed", "build", now)

	evs, err := db.ListAppEvents(mine.ID, database.AppEventFilter{})
	if err != nil {
		t.Fatalf("ListAppEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (the other app's must not appear)", len(evs))
	}
	if evs[0].Kind != "git_fetch_failed" {
		t.Fatalf("newest first broken: got %q", evs[0].Kind)
	}

	evs, err = db.ListAppEvents(mine.ID, database.AppEventFilter{Severity: database.EventSeverityWarning})
	if err != nil {
		t.Fatalf("ListAppEvents severity: %v", err)
	}
	if len(evs) != 1 || evs[0].Severity != database.EventSeverityWarning {
		t.Fatalf("severity filter returned %+v", evs)
	}

	evs, err = db.ListAppEvents(mine.ID, database.AppEventFilter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("ListAppEvents since: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("since filter returned %d events, want 1", len(evs))
	}
}

// Recipients are the people who can actually fix it: owners and editors
// with a usable address. Viewers, locked accounts, empty addresses and group
// principals are all out.
func TestListAppRecipients(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "recipients")

	mkUser := func(id, email, role string, locked bool) {
		t.Helper()
		u := &database.User{ID: id, Login: id, Email: email}
		if err := db.UpsertUser(u); err != nil {
			t.Fatalf("UpsertUser %s: %v", id, err)
		}
		// UpsertUser deliberately never writes system_role — a provider sign-in
		// must not be able to promote anyone — so set it the way the admin API
		// would.
		if _, err := db.Conn().Exec(`UPDATE users SET system_role=? WHERE id=?`, role, id); err != nil {
			t.Fatalf("setting role for %s: %v", id, err)
		}
		if locked {
			if _, err := db.Conn().Exec(`UPDATE users SET locked_at=? WHERE id=?`, time.Now().UTC(), id); err != nil {
				t.Fatalf("locking %s: %v", id, err)
			}
		}
	}
	grant := func(principalType, principalID, role string) {
		t.Helper()
		if _, err := db.Conn().Exec(`
			INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by)
			VALUES (?, ?, ?, ?, NULL)`, app.ID, principalType, principalID, role); err != nil {
			t.Fatalf("granting %s: %v", principalID, err)
		}
	}

	mkUser("collab", "collab@example.com", database.SystemRolePublisher, false)
	mkUser("viewer-role", "viewer@example.com", database.SystemRoleViewer, false)
	mkUser("locked", "locked@example.com", database.SystemRolePublisher, true)
	mkUser("emailless", "", database.SystemRolePublisher, false)
	mkUser("watcher", "watcher@example.com", database.SystemRolePublisher, false)

	grant("user", "collab", database.ObjectRoleEditor)
	grant("user", "viewer-role", database.ObjectRoleEditor) // system viewer: still out
	grant("user", "locked", database.ObjectRoleEditor)
	grant("user", "emailless", database.ObjectRoleEditor)
	grant("user", "watcher", database.ObjectRoleViewer) // object viewer: out
	grant("group", "everyone", database.ObjectRoleViewer)

	// The owner's grant is written by CreateApp itself; give them an address.
	if _, err := db.Conn().Exec(`UPDATE users SET email='owner@example.com' WHERE id=?`, app.OwnerID); err != nil {
		t.Fatalf("setting owner email: %v", err)
	}

	got, err := db.ListAppRecipients(app.ID)
	if err != nil {
		t.Fatalf("ListAppRecipients: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range got {
		if u.UserID != nil {
			seen[*u.UserID] = true
		}
	}
	// mkApp uses CreateApp, which writes no owner grant — only CreateAppTx
	// does — so the owner is present here only if a grant row exists.
	for _, want := range []string{"collab"} {
		if !seen[want] {
			t.Errorf("recipient %q missing; got %v", want, seen)
		}
	}
	for _, unwanted := range []string{"viewer-role", "locked", "emailless", "watcher", "everyone"} {
		if seen[unwanted] {
			t.Errorf("recipient %q should have been excluded", unwanted)
		}
	}
}

// The owner is reached through their owner grant, which CreateAppTx writes in
// the same transaction as the app row — so no union with apps.owner_id is
// needed, and this is the test that says so.
func TestListAppRecipientsIncludesOwnerViaGrant(t *testing.T) {
	db := dbtest.New(t)
	owner := &database.User{ID: "grant-owner", Login: "grant-owner", Email: "grant-owner@example.com"}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if _, err := db.Conn().Exec(`UPDATE users SET system_role=? WHERE id=?`,
		database.SystemRolePublisher, owner.ID); err != nil {
		t.Fatalf("setting owner role: %v", err)
	}
	app := &database.App{Slug: "owned", Name: "owned", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/owned", Branch: "main", Status: database.AppStatusStopped}
	if err := db.CreateAppTx(app, "", owner.ID, "", "", ""); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}

	got, err := db.ListAppRecipients(app.ID)
	if err != nil {
		t.Fatalf("ListAppRecipients: %v", err)
	}
	if len(got) != 1 || got[0].UserID == nil || *got[0].UserID != owner.ID {
		t.Fatalf("recipients = %+v, want just the owner", got)
	}
	if got[0].Address != owner.Email {
		t.Fatalf("recipient address = %q, want %q", got[0].Address, owner.Email)
	}
}

// A new app notifies, and the switch survives a round trip through every app
// read path — the column has to be in each SELECT list and each Scan.
func TestNotifyFailuresDefaultsOnAndRoundTrips(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "switch")
	if !app.NotifyFailures {
		t.Fatal("CreateApp returned an app with notification off")
	}

	got, err := db.GetApp(app.ID)
	if err != nil || got == nil {
		t.Fatalf("GetApp: %v", err)
	}
	if !got.NotifyFailures {
		t.Fatal("GetApp reports notification off on a new app")
	}

	if err := db.SetAppNotifyFailures(app.ID, false); err != nil {
		t.Fatalf("SetAppNotifyFailures: %v", err)
	}
	on, err := db.AppNotifyFailures(app.ID)
	if err != nil {
		t.Fatalf("AppNotifyFailures: %v", err)
	}
	if on {
		t.Fatal("switch did not turn off")
	}
	got, err = db.GetAppBySlug(app.Slug)
	if err != nil || got == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	if got.NotifyFailures {
		t.Fatal("GetAppBySlug did not see the change")
	}

	// A missing app has nobody to tell.
	on, err = db.AppNotifyFailures("no-such-app")
	if err != nil {
		t.Fatalf("AppNotifyFailures on a missing app: %v", err)
	}
	if on {
		t.Fatal("a missing app reported notification on")
	}
}

// The merged feed orders both halves together and keyset-pages without gaps or
// repeats.
func TestListActivityMergesAndPages(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "feed")
	base := time.Now().UTC().Add(-time.Hour)

	// Six events and six audit rows, interleaved a minute apart.
	for i := 0; i < 6; i++ {
		mkEvent(t, db, app.ID, "deploy_failed", "build", base.Add(time.Duration(2*i)*time.Minute))
		if _, err := db.Conn().Exec(`
			INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id, occurred_at)
			VALUES (?, 'user', ?, 'user.lock', 'app', ?, ?)`,
			"audit-"+string(rune('a'+i)), app.OwnerID, app.ID,
			base.Add(time.Duration(2*i+1)*time.Minute)); err != nil {
			t.Fatalf("inserting audit row: %v", err)
		}
	}

	// Page through in fours and check we see all twelve exactly once.
	seen := map[string]bool{}
	f := database.ActivityFilter{Limit: 4}
	for page := 0; page < 5; page++ {
		rows, err := db.ListActivity(f)
		if err != nil {
			t.Fatalf("ListActivity page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		var prev time.Time
		for i, r := range rows {
			if seen[r.ID] {
				t.Fatalf("row %s repeated across pages", r.ID)
			}
			seen[r.ID] = true
			if i > 0 && r.OccurredAt.After(prev) {
				t.Fatalf("page %d not ordered newest first", page)
			}
			prev = r.OccurredAt
		}
		last := rows[len(rows)-1]
		f.Before, f.BeforeID = last.OccurredAt, last.ID
	}
	if len(seen) != 12 {
		t.Fatalf("paged through %d rows, want 12", len(seen))
	}

	// Labels: an event reads as system:<source> against the app's slug; an
	// audit row reads as the actor's login.
	rows, err := db.ListActivity(database.ActivityFilter{Source: "event", Limit: 1})
	if err != nil {
		t.Fatalf("ListActivity events: %v", err)
	}
	if len(rows) != 1 || rows[0].ActorLabel != "system:autodeploy" || rows[0].TargetLabel != app.Slug {
		t.Fatalf("event row = %+v", rows)
	}
	if rows[0].Action != "app.deploy_failed" {
		t.Fatalf("event action = %q, want app.deploy_failed", rows[0].Action)
	}

	rows, err = db.ListActivity(database.ActivityFilter{Source: "audit", Limit: 20})
	if err != nil {
		t.Fatalf("ListActivity audit: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("audit-only feed returned %d rows, want 6", len(rows))
	}
	if rows[0].ActorLabel != "owner-feed" {
		t.Fatalf("audit actor label = %q, want the owner's login", rows[0].ActorLabel)
	}
}

// The request-scoped half of an audit row reaches the feed: the address a
// trusted proxy vouched for, the user agent, the credential by name, and the
// request id the caller was handed on the response.
func TestListActivityCarriesRequestMetadata(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reqmeta")
	now := time.Now().UTC()

	if _, err := db.Conn().Exec(`
		INSERT INTO user_api_keys (id, user_id, name, key_hash)
		VALUES ('key-1', ?, 'ci-runner', 'hash-1')`, app.OwnerID); err != nil {
		t.Fatalf("inserting key: %v", err)
	}
	if _, err := db.Conn().Exec(`
		INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id,
		                       occurred_at, request_id, ip, user_agent, api_key_id, app_id)
		VALUES ('a-meta', 'user', ?, 'app.env.update', 'app', ?, ?, 'req-abc', '203.0.113.7', 'toft/1.2.3', 'key-1', ?)`,
		app.OwnerID, app.ID, now, app.ID); err != nil {
		t.Fatalf("inserting audit row: %v", err)
	}
	mkEvent(t, db, app.ID, "app_crashed", "rel-1", now.Add(-time.Minute))

	rows, err := db.ListActivity(database.ActivityFilter{Source: "audit", Limit: 10})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want 1", len(rows))
	}
	got := rows[0]
	if got.IP != "203.0.113.7" || got.UserAgent != "toft/1.2.3" || got.RequestID != "req-abc" {
		t.Errorf("request metadata = ip %q, ua %q, rid %q", got.IP, got.UserAgent, got.RequestID)
	}
	// The id survives the key; the name is what an administrator reads.
	if got.APIKeyID != "key-1" || got.APIKeyName != "ci-runner" {
		t.Errorf("credential = %q / %q, want key-1 / ci-runner", got.APIKeyID, got.APIKeyName)
	}

	// An observation had no request behind it and must not borrow one.
	rows, err = db.ListActivity(database.ActivityFilter{Source: "event", Limit: 10})
	if err != nil {
		t.Fatalf("ListActivity events: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d event rows, want 1", len(rows))
	}
	if e := rows[0]; e.IP != "" || e.UserAgent != "" || e.APIKeyID != "" || e.APIKeyName != "" || e.RequestID != "" {
		t.Errorf("event row carried request metadata: %+v", e)
	}
}

// A deleted key must not take the row's attribution with it — the id is on the
// row, and only the name was ever a join.
func TestListActivitySurvivesADeletedKey(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "deadkey")

	if _, err := db.Conn().Exec(`
		INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id,
		                       occurred_at, api_key_id, app_id)
		VALUES ('a-dead', 'user', ?, 'app.env.update', 'app', ?, ?, 'key-gone', ?)`,
		app.OwnerID, app.ID, time.Now().UTC(), app.ID); err != nil {
		t.Fatalf("inserting audit row: %v", err)
	}

	rows, err := db.ListActivity(database.ActivityFilter{Source: "audit", Limit: 10})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(rows) != 1 || rows[0].APIKeyID != "key-gone" || rows[0].APIKeyName != "" {
		t.Fatalf("row = %+v, want the id kept and no name", rows)
	}
}

// A deploy key is named for what it is, never by its id: an app a promotion
// created read "57d9b848-…" in the actor column.
func TestListActivityNamesADeployKey(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "promoted")

	if _, err := db.Conn().Exec(`
		INSERT INTO deploy_keys (id, name, key_hash, revoked_at) VALUES ('dk-1', 'mc-from-dev', 'h', ?)`,
		time.Now().UTC()); err != nil {
		t.Fatalf("inserting deploy key: %v", err)
	}
	if _, err := db.Conn().Exec(`
		INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id, occurred_at, app_id)
		VALUES ('a-dk', 'deploy_key', 'dk-1', 'app.create', 'app', ?, ?, ?)`,
		app.ID, time.Now().UTC(), app.ID); err != nil {
		t.Fatalf("inserting audit row: %v", err)
	}

	rows, err := db.ListActivity(database.ActivityFilter{Source: "audit", Limit: 10})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(rows) != 1 || rows[0].ActorLabel != "deploy key mc-from-dev" {
		t.Fatalf("rows = %+v, want the revoked key named", rows)
	}
}

// The address and the request id are ways *in* to the feed. Somebody arrives
// holding one of them and nothing else.
func TestListActivitySearchesIPAndRequestID(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "findme")
	now := time.Now().UTC()

	for i, row := range []struct{ id, rid, ip string }{
		{"s-1", "req-wanted", "198.51.100.4"},
		{"s-2", "req-other", "203.0.113.9"},
	} {
		if _, err := db.Conn().Exec(`
			INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id,
			                       occurred_at, request_id, ip, app_id)
			VALUES (?, 'user', ?, 'app.env.update', 'app', ?, ?, ?, ?, ?)`,
			row.id, app.OwnerID, app.ID, now.Add(time.Duration(i)*time.Second), row.rid, row.ip, app.ID); err != nil {
			t.Fatalf("inserting audit row: %v", err)
		}
	}

	for _, q := range []string{"req-wanted", "198.51.100.4"} {
		rows, err := db.ListActivity(database.ActivityFilter{Source: "audit", Query: q, Limit: 10})
		if err != nil {
			t.Fatalf("ListActivity %q: %v", q, err)
		}
		if len(rows) != 1 || rows[0].ID != "s-1" {
			t.Errorf("searching %q returned %d rows, want just s-1", q, len(rows))
		}
	}
}

// A storage access is found by the file it reached, not only by its store:
// the object key lives in the row's detail, and an access review asks about a
// file. Narrowed to store.* rows, so another action's after-state is not
// searched by accident.
func TestListActivitySearchesTheFileAStorageRowReached(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "files")
	now := time.Now().UTC()
	for i, row := range []struct{ id, action, after string }{
		{"k-1", "store.presign", `{"key":"reports/2026-11/summary.pdf","verb":"GET"}`},
		{"k-2", "store.presign", `{"key":"reports/2026-11/other.csv","verb":"GET"}`},
		{"k-3", "identity.provider.update", `{"key":"summary.pdf"}`},
	} {
		if _, err := db.Conn().Exec(`
			INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id, occurred_at, after)
			VALUES (?, 'user', ?, ?, 'storage_resource', 'res-1', ?, ?::jsonb)`,
			row.id, app.OwnerID, row.action, now.Add(time.Duration(i)*time.Second), row.after); err != nil {
			t.Fatalf("inserting audit row: %v", err)
		}
	}
	rows, err := db.ListActivity(database.ActivityFilter{Source: "audit", Query: "summary.pdf", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "k-1" {
		t.Errorf("searching the file name returned %+v, want just k-1", rows)
	}
}

// A severity filter other than info excludes the audit half rather than
// matching nothing; an action filter excludes the event half.
func TestListActivityFilters(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "filters")
	now := time.Now().UTC()

	mkEvent(t, db, app.ID, "app_crashed", "rel-1", now)
	if _, err := db.Conn().Exec(`
		INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id, occurred_at)
		VALUES ('a1', 'user', ?, 'identity.provider.update', 'provider', 'p1', ?)`,
		app.OwnerID, now); err != nil {
		t.Fatalf("inserting audit row: %v", err)
	}

	rows, err := db.ListActivity(database.ActivityFilter{Severities: []string{database.EventSeverityError}})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(rows) != 1 || rows[0].Source != "event" {
		t.Fatalf("severity=error returned %+v, want the one event", rows)
	}

	rows, err = db.ListActivity(database.ActivityFilter{Actions: []string{"identity.*"}})
	if err != nil {
		t.Fatalf("ListActivity actions: %v", err)
	}
	if len(rows) != 1 || rows[0].Source != "audit" {
		t.Fatalf("action prefix filter returned %+v", rows)
	}

	rows, err = db.ListActivity(database.ActivityFilter{Query: "crash"})
	if err != nil {
		t.Fatalf("ListActivity query: %v", err)
	}
	if len(rows) != 1 || rows[0].Source != "event" {
		t.Fatalf("free-text query returned %+v", rows)
	}
}

// The three narrowings search could only approximate. An actor is whoever
// *wrote* the row — search also found the rows somebody was the target of. A
// refusal or a failure is a warning, so "what went wrong" is a filter rather
// than a list of action names to remember; a person declining something is
// not one. And an action is matched exactly, on both halves of the feed.
func TestListActivityNarrowsByActorSeverityAndAction(t *testing.T) {
	db := dbtest.New(t)
	one := mkApp(t, db, "narrow-one")
	two := mkApp(t, db, "narrow-two")
	now := time.Now().UTC()

	mkEvent(t, db, one.ID, "app_crashed", "rel-1", now)
	for i, r := range []struct{ id, actorType, actor, action, targetType, target string }{
		{"n1", "user", one.OwnerID, "store.presign", "user", two.OwnerID},
		{"n2", "user", two.OwnerID, "deploy_source_refused", "app", two.ID},
		{"n3", "anonymous", "", "session.signin_failed", "", ""},
		{"n4", "user", two.OwnerID, "access_request.deny", "app", two.ID},
	} {
		if _, err := db.Conn().Exec(`
			INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id, occurred_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.actorType, r.actor, r.action, r.targetType, r.target,
			now.Add(-time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatalf("inserting %s: %v", r.id, err)
		}
	}
	ids := func(f database.ActivityFilter) string {
		t.Helper()
		rows, err := db.ListActivity(f)
		if err != nil {
			t.Fatalf("ListActivity(%+v): %v", f, err)
		}
		var out []string
		for _, r := range rows {
			if r.Source == "event" {
				out = append(out, "event")
			} else {
				out = append(out, r.ID)
			}
		}
		return strings.Join(out, " ")
	}

	// n1 names the second owner as its target, and search would return it.
	if got := ids(database.ActivityFilter{ActorID: two.OwnerID}); got != "n2 n4" {
		t.Errorf("actor = %q, want the two rows they wrote: n2 n4", got)
	}
	if got := ids(database.ActivityFilter{Severities: []string{database.EventSeverityWarning, database.EventSeverityError}}); got != "event n2 n3" {
		t.Errorf("warning,error = %q, want the crash, the refusal and the failed sign-in", got)
	}
	if got := ids(database.ActivityFilter{Severities: []string{database.EventSeverityWarning}}); got != "n2 n3" {
		t.Errorf("warning = %q, want n2 n3", got)
	}
	if got := ids(database.ActivityFilter{Severities: []string{database.EventSeverityInfo}}); got != "n1 n4" {
		t.Errorf("info = %q, want n1 n4 — a person declining a request is a decision", got)
	}
	if got := ids(database.ActivityFilter{Action: "session.*"}); got != "n3" {
		t.Errorf("action session.* = %q, want n3", got)
	}
	if got := ids(database.ActivityFilter{Action: "store.presign"}); got != "n1" {
		t.Errorf("action store.presign = %q, want n1", got)
	}
	if got := ids(database.ActivityFilter{Action: "app.app_crashed"}); got != "event" {
		t.Errorf("action app.app_crashed = %q, want the event", got)
	}

	rows, err := db.ListActivity(database.ActivityFilter{ActorID: two.OwnerID})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Severity != database.EventSeverityWarning || rows[1].Severity != database.EventSeverityInfo {
		t.Errorf("severities = %s, %s; want the refusal a warning and the denial info", rows[0].Severity, rows[1].Severity)
	}
	if rows[0].ActorID != two.OwnerID {
		t.Errorf("actor_id = %q, want %q", rows[0].ActorID, two.OwnerID)
	}

	if label, err := db.ActivityActorLabel(two.OwnerID); err != nil || label != "owner-narrow-two" {
		t.Errorf("label of a user = %q, %v", label, err)
	}
	if label, err := db.ActivityActorLabel(one.ID); err != nil || label != one.Slug {
		t.Errorf("label of an app = %q, %v", label, err)
	}
	if label, err := db.ActivityActorLabel("gone"); err != nil || label != "gone" {
		t.Errorf("label of an id nothing holds = %q, %v; want the id", label, err)
	}
}

// Retention prunes by age and leaves everything newer alone.
func TestDeleteAppEventsBefore(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "prune")
	now := time.Now().UTC()

	mkEvent(t, db, app.ID, "deploy_failed", "old", now.Add(-100*24*time.Hour))
	mkEvent(t, db, app.ID, "deploy_failed", "new", now)

	n, err := db.DeleteAppEventsBefore(now.Add(-90 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("DeleteAppEventsBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}
	evs, err := db.ListAppEvents(app.ID, database.AppEventFilter{})
	if err != nil {
		t.Fatalf("ListAppEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].Fingerprint != "new" {
		t.Fatalf("survivors = %+v", evs)
	}
}

// Deleting an app takes its events with it.
func TestAppEventsCascadeOnAppDelete(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "cascade")
	mkEvent(t, db, app.ID, "app_crashed", "rel-1", time.Now().UTC())

	if _, err := db.Conn().Exec(`DELETE FROM apps WHERE id=?`, app.ID); err != nil {
		t.Fatalf("deleting app: %v", err)
	}
	var n int
	if err := db.Conn().QueryRow(`SELECT COUNT(*) FROM app_events WHERE app_id=?`, app.ID).Scan(&n); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d events survived the app", n)
	}
}

// The sentinel, against a real foreign key rather than a fake.
//
// events.Record decides how loudly to say this, and it can only decide if the
// database tells it which of the two happened. Matched on the driver's error
// code the way isUniqueViolation matches 23505 — the message is localized and
// version-dependent, so the typed check is the one that keeps working.
func TestInsertAppEventOnADeletedAppIsErrAppGone(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "event-gone")
	if err := db.DeleteApp(app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	err := db.InsertAppEvent(&database.AppEvent{
		AppID:    app.ID,
		Kind:     "app_crashed",
		Severity: database.EventSeverityError,
		Source:   database.EventSourceHealth,
		Summary:  "the workload exited",
	})
	if !errors.Is(err, database.ErrAppGone) {
		t.Fatalf("InsertAppEvent for a deleted app = %v, want ErrAppGone — without the sentinel "+
			"an expected race is indistinguishable from the database being unreachable", err)
	}
}

// A fingerprint prefix narrows the close to one automation's events, and an id
// is compared as text, never as a LIKE pattern.
func TestResolveAppEventsByFingerprintPrefix(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "resolve-prefix")
	now := time.Now().UTC()
	nightly := mkEvent(t, db, app.ID, "job_run_failed", "def_1:app_exec", now)
	mkEvent(t, db, app.ID, "job_run_failed", "def%1:app_exec", now)

	if _, err := db.ResolveAppEvents(app.ID, nightly.Kind, "def%"); err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	if open, _ := db.HasOpenAppEvent(app.ID, nightly.Kind); !open {
		t.Fatal("a prefix closed nothing it should have, or everything it should not")
	}
	if _, err := db.ResolveAppEvents(app.ID, nightly.Kind, "def_1:"); err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	if _, err := db.ResolveAppEvents(app.ID, nightly.Kind, "def%1:"); err != nil {
		t.Fatalf("ResolveAppEvents: %v", err)
	}
	if open, _ := db.HasOpenAppEvent(app.ID, nightly.Kind); open {
		t.Fatal("each automation's own prefix should have closed its own event")
	}
}

// An app acting for itself — a store presign, a send — is named by its slug on
// the Activity feed, as a person is by their login. It read as a bare UUID.
func TestAnAppActorIsNamedBySlug(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "presigner")
	if err := db.WriteAuditRow(database.AuditEntry{
		ActorType: database.PrincipalTypeApp, ActorID: app.ID,
		Action: "store.presign", TargetType: "app", TargetID: app.ID,
	}); err != nil {
		t.Fatalf("WriteAuditRow: %v", err)
	}
	rows, err := db.ListActivity(database.ActivityFilter{Source: "audit", Limit: 5})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	for _, r := range rows {
		if r.Action == "store.presign" {
			if r.ActorLabel != "presigner" {
				t.Errorf("app actor label = %q, want its slug", r.ActorLabel)
			}
			return
		}
	}
	t.Fatal("the presign row was not listed")
}
