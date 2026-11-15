package database_test

import (
	"strings"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The session policy: two numbers in the settings table, and what a fresh
// install answers before anybody has touched them.
//
// The absolute lifetime is the one that must not have moved — an upgrade that
// changed how long a session lives in the dimension it already had would sign
// people out for no reason anybody could name. The idle timeout is the one that
// deliberately did.

func TestAFreshInstallHasTheDefaultSessionPolicy(t *testing.T) {
	db := dbtest.New(t)

	p := db.SessionPolicy()
	if want := 8 * time.Hour; p.Absolute != want {
		t.Errorf("absolute = %v, want %v — the constant it replaced", p.Absolute, want)
	}
	if want := 30 * time.Minute; p.Idle != want {
		t.Errorf("idle = %v, want %v — AAL2's number, on by default", p.Idle, want)
	}
}

// The cache is dropped on write, or an administrator watches a policy change
// succeed and it never takes effect.
func TestChangingThePolicyTakesEffectWithoutARestart(t *testing.T) {
	db := dbtest.New(t)

	// Read first, so there is a cached value to invalidate.
	if got := db.SessionPolicy().Absolute; got != 8*time.Hour {
		t.Fatalf("absolute = %v, want the default", got)
	}

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.SessionAbsoluteHours = 1
	s.SessionIdleMinutes = 0
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}

	p := db.SessionPolicy()
	if p.Absolute != time.Hour {
		t.Errorf("absolute = %v, want 1h — the cache outlived the write", p.Absolute)
	}
	if p.Idle != 0 {
		t.Errorf("idle = %v, want 0 — the escape hatch is a real value, not 'unset'", p.Idle)
	}
}

// Zero on the idle timeout survives a round trip. It is the whole escape hatch,
// and a clamp that read it as "unset" would restore the default over an
// administrator's deliberate choice.
func TestTheIdleTimeoutSurvivesBeingTurnedOff(t *testing.T) {
	db := dbtest.New(t)

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.SessionIdleMinutes = 0
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}

	after, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if after.SessionIdleMinutes != 0 {
		t.Errorf("idle minutes = %d, want 0", after.SessionIdleMinutes)
	}
}

// A row outside its declared range is corruption or a downgrade, and the read
// path is the only thing standing between it and a policy nobody chose.
func TestOutOfRangePolicyRowsFallBackToTheDefaults(t *testing.T) {
	db := dbtest.New(t)
	d := database.DefaultPlatformSettings()

	for _, tc := range []struct {
		name, key, value string
		read             func(database.PlatformSettings) int
		want             int
	}{
		{"absolute above the ceiling", database.SettingSessionAbsoluteHours, "999",
			func(s database.PlatformSettings) int { return s.SessionAbsoluteHours }, d.SessionAbsoluteHours},
		{"absolute at zero", database.SettingSessionAbsoluteHours, "0",
			func(s database.PlatformSettings) int { return s.SessionAbsoluteHours }, d.SessionAbsoluteHours},
		{"idle below the floor", database.SettingSessionIdleMinutes, "1",
			func(s database.PlatformSettings) int { return s.SessionIdleMinutes }, d.SessionIdleMinutes},
		{"idle above the ceiling", database.SettingSessionIdleMinutes, "10000",
			func(s database.PlatformSettings) int { return s.SessionIdleMinutes }, d.SessionIdleMinutes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.PutSetting(tc.key, tc.value, database.BuiltinAdminID); err != nil {
				t.Fatal(err)
			}
			s, err := db.GetPlatformSettings()
			if err != nil {
				t.Fatal(err)
			}
			if got := tc.read(s); got != tc.want {
				t.Errorf("%s = %d, want the default %d", tc.key, got, tc.want)
			}
		})
	}
}

// A policy change is audited by the change it already goes through. No new
// audit action: expiry is not an act anybody performs, and the settings row is
// the complete record of the decision.
func TestAPolicyChangeIsAuditedAsASettingsUpdate(t *testing.T) {
	db := dbtest.New(t)

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.SessionIdleMinutes = 15
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "req-1", "10.0.0.1", "curl"); err != nil {
		t.Fatal(err)
	}

	var after string
	err = db.Conn().QueryRow(
		`SELECT after FROM audit_log WHERE action = 'platform_settings.update'
		 ORDER BY occurred_at DESC LIMIT 1`).Scan(&after)
	if err != nil {
		t.Fatalf("a session policy change wrote no platform_settings.update row: %v", err)
	}
	if !strings.Contains(after, `"session_idle_minutes": 15`) {
		t.Errorf("the audit row does not carry the new policy: %s", after)
	}
}

// The two settings and the durations every enforcement point wants are one
// conversion, not one per call site.
func TestSessionPolicyConvertsTheStoredUnits(t *testing.T) {
	s := database.PlatformSettings{SessionAbsoluteHours: 12, SessionIdleMinutes: 45}
	got := s.SessionPolicy()
	if want := (auth.SessionPolicy{Absolute: 12 * time.Hour, Idle: 45 * time.Minute}); got != want {
		t.Errorf("SessionPolicy() = %+v, want %+v", got, want)
	}
}
