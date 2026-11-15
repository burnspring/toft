package database_test

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func TestNormalizeInstallName(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"  Staging Apps Platform ", "Staging Apps Platform", true},
		{"Acme Inc. Analytics", "Acme Inc. Analytics", true},
		{"Platform v2.0", "Platform v2.0", true},
		{"Søren’s Apps", "Søren’s Apps", true},
		{strings.Repeat("a", database.InstallNameLimit), strings.Repeat("a", database.InstallNameLimit), true},
		{strings.Repeat("a", database.InstallNameLimit+1), "", false},
		{"Staging\nApps", "", false},
		{"Staging\tApps", "", false},
		{"<b>Staging</b>", "", false},
		{"https://evil.example", "", false},
		{"evil.example", "", false},
		{"Apps at apps.example.com", "", false},
		{"www.acme", "", false},
		{"ftp://files", "", false},
	} {
		got, err := database.NormalizeInstallName(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("NormalizeInstallName(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// Empty reads "Toft"; a name is read back uncached, so a rename shows on the
// next message rather than after a restart.
func TestResolveSenderName(t *testing.T) {
	var nilDB *database.DB
	if got := nilDB.ResolveSenderName(); got != "Toft" {
		t.Errorf("nil DB: %q", got)
	}
	db := dbtest.New(t)
	if got := db.ResolveSenderName(); got != "Toft" {
		t.Errorf("fresh install: %q, want Toft", got)
	}
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.InstallName = "  Staging Apps Platform  "
	if err := db.SetPlatformSettings(&s, "builtin-admin", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := db.ResolveSenderName(); got != "Staging Apps Platform" {
		t.Errorf("after naming: %q", got)
	}
	if back, _ := db.GetPlatformSettings(); back.InstallName != "Staging Apps Platform" {
		t.Errorf("settings read back %q", back.InstallName)
	}
	s.InstallName = ""
	if err := db.SetPlatformSettings(&s, "builtin-admin", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := db.ResolveSenderName(); got != "Toft" {
		t.Errorf("after clearing: %q, want Toft", got)
	}
}
