package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The gateway's header switch: on with no row, on with a row that will not
// parse, and visible on the very next read after either write door — the
// proxy reads it from the cache on every request, so a write that left the
// cache standing would be a switch that saves and does nothing.

func TestAppSecurityHeadersAreOnWithNoRow(t *testing.T) {
	db := dbtest.New(t)
	if !db.AppSecurityHeaders() {
		t.Error("a fresh install has the baseline off — an upgrade would never turn it on")
	}
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !s.AppSecurityHeaders {
		t.Error("the settings document reads it off with no row")
	}
}

func TestAnUnparseableAppSecurityHeadersRowReadsOn(t *testing.T) {
	db := dbtest.New(t)
	if _, err := db.PutSetting(database.SettingAppSecurityHeaders, "maybe", database.BuiltinAdminID); err != nil {
		t.Fatal(err)
	}
	if !db.AppSecurityHeaders() {
		t.Error("an unreadable row turned the baseline off")
	}
}

func TestTurningAppSecurityHeadersOffTakesEffectOnTheNextRead(t *testing.T) {
	db := dbtest.New(t)
	if !db.AppSecurityHeaders() { // a cached value to invalidate
		t.Fatal("want on before the write")
	}

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.AppSecurityHeaders = false
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if db.AppSecurityHeaders() {
		t.Fatal("still on after SetPlatformSettings — the cache outlived the write")
	}

	if _, err := db.PutSettingAudited(database.SettingAppSecurityHeaders, "true",
		database.AuditEntry{ActorType: "user", ActorID: database.BuiltinAdminID, Action: "platform_settings.update"}); err != nil {
		t.Fatal(err)
	}
	if !db.AppSecurityHeaders() {
		t.Error("still off after PutSettingAudited — the cache outlived the write")
	}
}
