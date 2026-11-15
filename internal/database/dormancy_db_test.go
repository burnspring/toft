package database_test

import (
	"encoding/json"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The dormancy clock is four facts reduced to one, and every test here is about
// which of the four wins. The failure this guards against is not subtle: get it
// wrong and the sweep locks the wrong accounts.

// mkUser writes a user row with the three timestamps the clock reads. Raw SQL
// rather than UpsertUser because the whole point is controlling created_at,
// which UpsertUser stamps with NOW().
func mkUser(t *testing.T, db *database.DB, login, role string, created time.Time, lastLogin, unlocked *time.Time, locked bool) string {
	t.Helper()
	id := "u-" + login
	var lockedAt any
	if locked {
		lockedAt = created
	}
	_, err := db.Conn().Exec(`
		INSERT INTO users (id, login, name, email, avatar_url, system_role, created_at, last_login_at, unlocked_at, locked_at)
		VALUES (?, ?, '', ?, '', ?, ?, ?, ?, ?)`,
		id, login, login+"@example.com", role, created, lastLogin, unlocked, lockedAt)
	if err != nil {
		t.Fatalf("creating %s: %v", login, err)
	}
	return id
}

func mkKey(t *testing.T, db *database.DB, userID, name string, lastUsed *time.Time) {
	t.Helper()
	_, err := db.Conn().Exec(`
		INSERT INTO user_api_keys (id, user_id, name, key_hash, key_prefix, created_at, last_used_at)
		VALUES (?, ?, ?, ?, 'toftu_test', NOW(), ?)`,
		"k-"+userID+"-"+name, userID, name, "hash-"+userID+"-"+name, lastUsed)
	if err != nil {
		t.Fatalf("creating key for %s: %v", userID, err)
	}
}

func ptr(t time.Time) *time.Time { return &t }

func activityFor(t *testing.T, db *database.DB, login string) database.UserActivity {
	t.Helper()
	all, err := db.ListUsersWithActivity()
	if err != nil {
		t.Fatalf("ListUsersWithActivity: %v", err)
	}
	for _, u := range all {
		if u.Login == login {
			return u
		}
	}
	t.Fatalf("no row for %s", login)
	return database.UserActivity{}
}

// An account nobody has ever signed into reads its creation date, not zero.
// Without this the clock would be NULL and a fresh account would look
// infinitely dormant.
func TestClockFallsBackToCreatedAt(t *testing.T) {
	db := dbtest.New(t)
	created := time.Now().UTC().AddDate(0, 0, -10).Truncate(time.Second)
	mkUser(t, db, "never", database.SystemRoleViewer, created, nil, nil, false)

	got := activityFor(t, db, "never")
	if !got.LastActiveAt.Equal(created) {
		t.Errorf("clock = %v, want created_at %v", got.LastActiveAt, created)
	}
}

// The bug this whole feature turns on. A personal API key used last night
// **must** beat a sign-in a year ago, or the sweep locks the CI accounts.
func TestKeyUseBeatsAnOldSignIn(t *testing.T) {
	db := dbtest.New(t)
	created := time.Now().UTC().AddDate(-2, 0, 0).Truncate(time.Second)
	oldLogin := time.Now().UTC().AddDate(-1, 0, 0).Truncate(time.Second)
	keyUse := time.Now().UTC().AddDate(0, 0, -1).Truncate(time.Second)

	id := mkUser(t, db, "ci", database.SystemRoleViewer, created, ptr(oldLogin), nil, false)
	mkKey(t, db, id, "nightly", ptr(keyUse))

	got := activityFor(t, db, "ci")
	if !got.LastActiveAt.Equal(keyUse) {
		t.Errorf("clock = %v, want the key use %v — sweeping on last_login_at alone "+
			"is what locks the accounts that are most active", got.LastActiveAt, keyUse)
	}
	if got.LastKeyUseAt == nil || !got.LastKeyUseAt.Equal(keyUse) {
		t.Errorf("LastKeyUseAt = %v, want %v", got.LastKeyUseAt, keyUse)
	}
}

// Several keys, and the newest wins. A person with an abandoned key and a live
// one is active.
func TestNewestKeyWins(t *testing.T) {
	db := dbtest.New(t)
	created := time.Now().UTC().AddDate(-2, 0, 0).Truncate(time.Second)
	old := time.Now().UTC().AddDate(0, 0, -200).Truncate(time.Second)
	recent := time.Now().UTC().AddDate(0, 0, -2).Truncate(time.Second)

	id := mkUser(t, db, "many", database.SystemRoleViewer, created, nil, nil, false)
	mkKey(t, db, id, "abandoned", ptr(old))
	mkKey(t, db, id, "live", ptr(recent))

	if got := activityFor(t, db, "many"); !got.LastActiveAt.Equal(recent) {
		t.Errorf("clock = %v, want the newest key use %v", got.LastActiveAt, recent)
	}
}

// A key that has never been used contributes nothing — a NULL must not swallow
// the other three facts. This is the GREATEST-ignores-NULL behaviour the query
// depends on, asserted rather than assumed.
func TestUnusedKeyDoesNotBlankTheClock(t *testing.T) {
	db := dbtest.New(t)
	created := time.Now().UTC().AddDate(0, 0, -30).Truncate(time.Second)
	login := time.Now().UTC().AddDate(0, 0, -5).Truncate(time.Second)

	id := mkUser(t, db, "minted", database.SystemRoleViewer, created, ptr(login), nil, false)
	mkKey(t, db, id, "unused", nil)

	got := activityFor(t, db, "minted")
	if !got.LastActiveAt.Equal(login) {
		t.Errorf("clock = %v, want %v — an unused key must not blank the clock", got.LastActiveAt, login)
	}
	if got.LastKeyUseAt != nil {
		t.Errorf("LastKeyUseAt = %v, want nil", got.LastKeyUseAt)
	}
}

// An unlock restarts the clock. Without this the sweep undoes its own unlocks
// on the next pass, which reads as the unlock button not working.
func TestUnlockRestartsTheClock(t *testing.T) {
	db := dbtest.New(t)
	created := time.Now().UTC().AddDate(-2, 0, 0).Truncate(time.Second)
	oldLogin := time.Now().UTC().AddDate(0, 0, -300).Truncate(time.Second)
	unlocked := time.Now().UTC().AddDate(0, 0, -1).Truncate(time.Second)

	mkUser(t, db, "reinstated", database.SystemRoleViewer, created, ptr(oldLogin), ptr(unlocked), false)

	if got := activityFor(t, db, "reinstated"); !got.LastActiveAt.Equal(unlocked) {
		t.Errorf("clock = %v, want the unlock %v", got.LastActiveAt, unlocked)
	}

	// And so the account is not dormant against a 90-day cutoff any more.
	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	dormant, err := db.ListDormantUsers(cutoff)
	if err != nil {
		t.Fatalf("ListDormantUsers: %v", err)
	}
	for _, u := range dormant {
		if u.Login == "reinstated" {
			t.Fatal("a just-unlocked account is still dormant; the next sweep would re-lock it")
		}
	}
}

// ListDormantUsers filters on the clock and nothing else — admins and
// already-locked rows come back, because the sweep's rules live in the sweep.
func TestListDormantReturnsAdminsAndLockedRows(t *testing.T) {
	db := dbtest.New(t)
	old := time.Now().UTC().AddDate(0, 0, -400).Truncate(time.Second)
	fresh := time.Now().UTC().AddDate(0, 0, -1).Truncate(time.Second)

	mkUser(t, db, "oldadmin", database.SystemRoleAdmin, old, nil, nil, false)
	mkUser(t, db, "oldlocked", database.SystemRoleViewer, old, nil, nil, true)
	mkUser(t, db, "oldplain", database.SystemRoleViewer, old, nil, nil, false)
	mkUser(t, db, "current", database.SystemRoleViewer, fresh, nil, nil, false)

	got, err := db.ListDormantUsers(time.Now().UTC().AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("ListDormantUsers: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range got {
		seen[u.Login] = true
	}
	for _, want := range []string{"oldadmin", "oldlocked", "oldplain"} {
		if !seen[want] {
			t.Errorf("%s missing from the dormant list", want)
		}
	}
	if seen["current"] {
		t.Error("an account active yesterday was reported dormant against a 90-day cutoff")
	}
}

// The settings page's warning counts what the sweep would actually do: not
// admins, not the already-locked.
func TestCountDormantLockableExcludesAdminsAndLocked(t *testing.T) {
	db := dbtest.New(t)
	old := time.Now().UTC().AddDate(0, 0, -400).Truncate(time.Second)

	mkUser(t, db, "a-admin", database.SystemRoleAdmin, old, nil, nil, false)
	mkUser(t, db, "a-locked", database.SystemRoleViewer, old, nil, nil, true)
	mkUser(t, db, "a-one", database.SystemRoleViewer, old, nil, nil, false)
	mkUser(t, db, "a-two", database.SystemRolePublisher, old, nil, nil, false)

	// dbtest seeds the built-in administrator, whose row is fresh and an admin
	// either way — so the count is exactly the two lockable accounts above.
	n, err := db.CountDormantLockable(time.Now().UTC().AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("CountDormantLockable: %v", err)
	}
	if n != 2 {
		t.Errorf("CountDormantLockable = %d, want 2 (admins and locked accounts excluded)", n)
	}
}

// The lock writes the row and its audit entry together, and a second attempt
// changes nothing — the guard against two nodes sweeping at once.
func TestLockDormantUserIsAuditedAndIdempotent(t *testing.T) {
	db := dbtest.New(t)
	old := time.Now().UTC().AddDate(0, 0, -400).Truncate(time.Second)
	mkUser(t, db, "target", database.SystemRoleViewer, old, nil, nil, false)

	u := activityFor(t, db, "target")
	now := time.Now().UTC()

	before, err := db.CountAuditEntries()
	if err != nil {
		t.Fatalf("CountAuditEntries: %v", err)
	}

	locked, err := db.LockDormantUser(u, 90, now)
	if err != nil {
		t.Fatalf("LockDormantUser: %v", err)
	}
	if !locked {
		t.Fatal("first lock reported no change")
	}

	after, err := db.CountAuditEntries()
	if err != nil {
		t.Fatalf("CountAuditEntries: %v", err)
	}
	if after != before+1 {
		t.Errorf("audit rows went %d → %d, want exactly one more", before, after)
	}

	got, err := db.GetUser(u.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !got.Locked() {
		t.Error("the account is not locked after LockDormantUser")
	}

	// Second pass: no row changes, and no second audit entry either.
	locked, err = db.LockDormantUser(u, 90, now)
	if err != nil {
		t.Fatalf("second LockDormantUser: %v", err)
	}
	if locked {
		t.Error("locking an already-locked account reported a change")
	}
	again, err := db.CountAuditEntries()
	if err != nil {
		t.Fatalf("CountAuditEntries: %v", err)
	}
	if again != after {
		t.Errorf("a no-op lock wrote %d audit row(s)", again-after)
	}
}

// The sweep's audit entry says the platform did it, not a person — and carries
// the two figures that answer "why is my account locked".
func TestLockDormantUserAuditsAsTheSystem(t *testing.T) {
	db := dbtest.New(t)
	old := time.Now().UTC().AddDate(0, 0, -400).Truncate(time.Second)
	mkUser(t, db, "audited", database.SystemRoleViewer, old, nil, nil, false)

	u := activityFor(t, db, "audited")
	if _, err := db.LockDormantUser(u, 90, time.Now().UTC()); err != nil {
		t.Fatalf("LockDormantUser: %v", err)
	}

	var actorType, action, after string
	err := db.Conn().QueryRow(`
		SELECT actor_type, action, after FROM audit_log
		WHERE target_type = 'user' AND target_id = ?`, u.ID).Scan(&actorType, &action, &after)
	if err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if actorType != database.PrincipalTypeSystem {
		t.Errorf("actor_type = %q, want %q — no person locked this account",
			actorType, database.PrincipalTypeSystem)
	}
	if action != "user.lock_dormant" {
		t.Errorf("action = %q, want user.lock_dormant — it must be tellable apart "+
			"from an administrator's own user.lock", action)
	}
	// Compared as a document rather than as a substring: audit_log.after is
	// JSONB, so Postgres decides the key order and the spacing and a literal
	// match would be asserting the formatter rather than the record.
	var payload map[string]any
	if err := json.Unmarshal([]byte(after), &payload); err != nil {
		t.Fatalf("audit `after` is not JSON: %v (%s)", err, after)
	}
	if payload["reason"] != "dormant" {
		t.Errorf("audit `after` reason = %v, want dormant (%s)", payload["reason"], after)
	}
	if payload["threshold_days"] != float64(90) {
		t.Errorf("audit `after` threshold_days = %v, want 90 (%s)", payload["threshold_days"], after)
	}
	if _, ok := payload["last_active_at"]; !ok {
		t.Errorf("audit `after` is missing last_active_at (%s)", after)
	}
}

// The shell's recovery unlock restarts the clock like the API's does (DB1-5):
// without the stamp, a demoted and locked built-in account recovered from the
// shell was re-locked by the next dormancy pass.
func TestRecoveryUnlockRestartsTheClock(t *testing.T) {
	db := dbtest.New(t)
	old := time.Now().UTC().AddDate(-2, 0, 0).Truncate(time.Second)
	id := mkUser(t, db, "recovered", database.SystemRoleViewer, old, nil, nil, true)

	if err := db.UnlockUserForRecovery(id, database.AuditEntry{}); err != nil {
		t.Fatalf("UnlockUserForRecovery: %v", err)
	}
	u, err := db.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	if u.LockedAt != nil || u.UnlockedAt == nil || time.Since(*u.UnlockedAt) > time.Minute {
		t.Fatalf("locked_at = %v, unlocked_at = %v; want unlocked and stamped now", u.LockedAt, u.UnlockedAt)
	}
}
