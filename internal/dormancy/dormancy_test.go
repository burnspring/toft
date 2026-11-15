package dormancy

import (
	"errors"
	"testing"
	"time"

	"toft/internal/database"
)

// fakeStore records what the sweep asked for and what it did.
type fakeStore struct {
	settings    database.PlatformSettings
	settingsErr error

	users    []database.UserActivity
	listErr  error
	listedAt []time.Time // the cutoff of each ListDormantUsers call

	locked    []string // logins, in the order they were locked
	lockErr   error
	lockRaces map[string]bool // logins whose lock reports "somebody got there first"
}

func (f *fakeStore) GetPlatformSettings() (database.PlatformSettings, error) {
	return f.settings, f.settingsErr
}

func (f *fakeStore) ListDormantUsers(cutoff time.Time) ([]database.UserActivity, error) {
	f.listedAt = append(f.listedAt, cutoff)
	if f.listErr != nil {
		return nil, f.listErr
	}
	// The real query filters on the clock; the fake does too, so a test can
	// hand over a whole population and let the cutoff decide.
	var out []database.UserActivity
	for _, u := range f.users {
		if u.LastActiveAt.Before(cutoff) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeStore) LockDormantUser(u database.UserActivity, days int, now time.Time) (bool, error) {
	if f.lockErr != nil {
		return false, f.lockErr
	}
	if f.lockRaces[u.Login] {
		return false, nil
	}
	f.locked = append(f.locked, u.Login)
	return true, nil
}

// fixedNow is the clock every test in this file runs at.
var fixedNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func user(login, role string, idleDays int, locked bool) database.UserActivity {
	u := database.UserActivity{
		User: database.User{
			ID: "id-" + login, Login: login, SystemRole: role,
		},
		LastActiveAt: fixedNow.AddDate(0, 0, -idleDays),
	}
	if locked {
		t := fixedNow.AddDate(0, 0, -1)
		u.LockedAt = &t
	}
	return u
}

func sweeperFor(store *fakeStore) *Sweeper {
	s := New(store)
	s.now = func() time.Time { return fixedNow }
	return s
}

func enabled(days int) database.PlatformSettings {
	s := database.DefaultPlatformSettings()
	s.DormantLockEnabled = true
	s.DormantLockDays = days
	return s
}

// Off means nobody is touched and the sweep does not even ask who is dormant.
// "Absent, not dormant" for a feature that is itself about dormancy.
func TestDisabledLocksNobodyAndQueriesNothing(t *testing.T) {
	store := &fakeStore{
		settings: database.DefaultPlatformSettings(), // enabled: false
		users:    []database.UserActivity{user("alice", database.SystemRoleViewer, 400, false)},
	}
	res := sweeperFor(store).Tick()

	if res.Locked != 0 {
		t.Errorf("locked %d accounts with the feature off, want 0", res.Locked)
	}
	if len(store.listedAt) != 0 {
		t.Errorf("queried for dormant users %d times with the feature off, want 0", len(store.listedAt))
	}
}

// The default has to be off, or an upgrade starts locking an install's users on
// a clock nobody set.
func TestDefaultSettingsAreOff(t *testing.T) {
	if database.DefaultPlatformSettings().DormantLockEnabled {
		t.Fatal("dormancy locking defaults to on; an upgrade would lock accounts nobody asked it to")
	}
}

func TestLocksDormantNonAdmins(t *testing.T) {
	store := &fakeStore{
		settings: enabled(90),
		users: []database.UserActivity{
			user("dormant", database.SystemRoleViewer, 120, false),
			user("active", database.SystemRoleViewer, 3, false),
		},
	}
	res := sweeperFor(store).Tick()

	if res.Locked != 1 || len(store.locked) != 1 || store.locked[0] != "dormant" {
		t.Fatalf("locked %v, want [dormant]", store.locked)
	}
}

// The constraint the whole feature is built around. An administrator past the
// threshold is reported and never locked — see the comment in Tick for the
// three reasons.
func TestNeverLocksAdministrators(t *testing.T) {
	store := &fakeStore{
		settings: enabled(90),
		users: []database.UserActivity{
			// The built-in local administrator on a federated install: dormant
			// by design, and the account you need when the IdP breaks.
			user("admin", database.SystemRoleAdmin, 900, false),
			user("someone", database.SystemRoleViewer, 900, false),
		},
	}
	res := sweeperFor(store).Tick()

	for _, login := range store.locked {
		if login == "admin" {
			t.Fatal("locked an administrator; administrators are never locked automatically")
		}
	}
	if res.DormantAdmins != 1 {
		t.Errorf("DormantAdmins = %d, want 1 — a dormant admin must be reported, not silently skipped", res.DormantAdmins)
	}
	if res.Locked != 1 {
		t.Errorf("locked %d, want 1 (the non-admin)", res.Locked)
	}
}

// A settings read that fails must not fall back to a default threshold: the
// default is a policy, and guessing it locks real people out.
func TestSettingsErrorSweepsNobody(t *testing.T) {
	store := &fakeStore{
		settingsErr: errors.New("database is down"),
		users:       []database.UserActivity{user("alice", database.SystemRoleViewer, 900, false)},
	}
	res := sweeperFor(store).Tick()

	if res.Locked != 0 || len(store.locked) != 0 {
		t.Fatalf("locked %v after a failed settings read, want none", store.locked)
	}
	if len(store.listedAt) != 0 {
		t.Error("queried for dormant users after a failed settings read")
	}
}

// The cutoff is the threshold subtracted from now, and it is what the store is
// asked for — the sweep never re-derives dormancy itself.
func TestCutoffIsThresholdBeforeNow(t *testing.T) {
	store := &fakeStore{settings: enabled(45)}
	sweeperFor(store).Tick()

	if len(store.listedAt) != 1 {
		t.Fatalf("queried %d times, want 1", len(store.listedAt))
	}
	want := fixedNow.Add(-45 * 24 * time.Hour)
	if !store.listedAt[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", store.listedAt[0], want)
	}
}

// An account already locked is counted, not re-locked — and never re-audited.
func TestAlreadyLockedIsCountedNotRelocked(t *testing.T) {
	store := &fakeStore{
		settings: enabled(90),
		users:    []database.UserActivity{user("gone", database.SystemRoleViewer, 400, true)},
	}
	res := sweeperFor(store).Tick()

	if len(store.locked) != 0 {
		t.Fatalf("re-locked %v", store.locked)
	}
	if res.AlreadyLocked != 1 {
		t.Errorf("AlreadyLocked = %d, want 1", res.AlreadyLocked)
	}
}

// Losing the race with an administrator's own lock is not an error and not a
// second lock.
func TestLockRaceIsCountedAsAlreadyLocked(t *testing.T) {
	store := &fakeStore{
		settings:  enabled(90),
		users:     []database.UserActivity{user("raced", database.SystemRoleViewer, 400, false)},
		lockRaces: map[string]bool{"raced": true},
	}
	res := sweeperFor(store).Tick()

	if res.Locked != 0 {
		t.Errorf("Locked = %d, want 0", res.Locked)
	}
	if res.AlreadyLocked != 1 {
		t.Errorf("AlreadyLocked = %d, want 1", res.AlreadyLocked)
	}
}

// One account failing to lock does not abandon the rest of the pass.
func TestLockErrorDoesNotAbortTheSweep(t *testing.T) {
	store := &fakeStore{
		settings: enabled(90),
		users: []database.UserActivity{
			user("a", database.SystemRoleViewer, 400, false),
			user("b", database.SystemRoleViewer, 400, false),
		},
		lockErr: errors.New("write failed"),
	}
	res := sweeperFor(store).Tick()

	// Both were attempted; neither succeeded; the sweep did not panic or return
	// early, and the next pass will find them again.
	if res.Locked != 0 {
		t.Errorf("Locked = %d, want 0", res.Locked)
	}
	if len(store.listedAt) != 1 {
		t.Errorf("listed %d times, want 1", len(store.listedAt))
	}
}

// A user exactly at the threshold is not yet past it — the store's filter is
// strictly-before, and the sweep must not widen it.
func TestExactlyAtThresholdIsNotDormant(t *testing.T) {
	store := &fakeStore{
		settings: enabled(90),
		users:    []database.UserActivity{user("edge", database.SystemRoleViewer, 90, false)},
	}
	if res := sweeperFor(store).Tick(); res.Locked != 0 {
		t.Errorf("locked an account exactly at the threshold; want the boundary exclusive")
	}
}
