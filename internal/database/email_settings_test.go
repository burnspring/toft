package database_test

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// Where administrator notifications go.
//
// The rule has one job: an install that names a distribution list stops caring
// which people hold the admin role. The tests below pin the two halves of that —
// the group address replaces the individuals rather than joining them, and it
// works in the case the individuals cannot cover, which is nobody having an
// address on file at all.

// seedAdmin adds a real administrator with an address on file.
func seedAdmin(t *testing.T, db *database.DB, id string) string {
	t.Helper()
	addr := id + "@corp.example"
	if err := db.UpsertUser(&database.User{ID: id, Login: id, Email: addr}); err != nil {
		t.Fatalf("UpsertUser %s: %v", id, err)
	}
	if err := db.UpdateUserRole(id, database.SystemRoleAdmin); err != nil {
		t.Fatalf("UpdateUserRole %s: %v", id, err)
	}
	return addr
}

func TestAdminRecipientsWithoutAGroupAddress(t *testing.T) {
	db := dbtest.New(t)

	// A fresh install's only administrator is the built-in one, whose address
	// is the unresolvable placeholder admin@toft.local. It is nobody to mail
	// (VN-2, 2026-11-15), so the out-of-the-box answer is "nobody", which is
	// what lets the send sites warn.
	to, err := db.AdminRecipients()
	if err != nil {
		t.Fatalf("AdminRecipients: %v", err)
	}
	if len(to) != 0 {
		t.Fatalf("a fresh install's recipients = %v, want none: the placeholder is not an address", to)
	}

	want := seedAdmin(t, db, "ops")
	to, err = db.AdminRecipients()
	if err != nil {
		t.Fatalf("AdminRecipients: %v", err)
	}
	if len(to) != 1 || to[0] != want {
		t.Fatalf("recipients = %v, want [%s] alone", to, want)
	}
	for _, addr := range to {
		if addr == "" || addr == database.BuiltinAdminEmail {
			t.Errorf("%q made it into the recipient list", addr)
		}
	}
}

func TestAdminGroupAddressReplacesTheIndividualAdministrators(t *testing.T) {
	db := dbtest.New(t)
	seedAdmin(t, db, "ops")

	before, err := db.AdminRecipients()
	if err != nil {
		t.Fatalf("AdminRecipients: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("no administrator to be replaced, so this proves nothing")
	}

	settings, err := db.GetEmailSettings()
	if err != nil {
		t.Fatalf("GetEmailSettings: %v", err)
	}
	settings.AdminGroupAddress = "toft-admins@corp.example"
	if err := db.SetEmailSettings(&settings, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("SetEmailSettings: %v", err)
	}

	after, err := db.AdminRecipients()
	if err != nil {
		t.Fatalf("AdminRecipients: %v", err)
	}
	// Replaces, not joins: an alias that already contains the administrators
	// would otherwise deliver to each of them twice.
	if len(after) != 1 || after[0] != "toft-admins@corp.example" {
		t.Fatalf("recipients = %v, want the group address alone", after)
	}

	// And it round-trips, so the Admin -> Email form can show what is set.
	reread, err := db.GetEmailSettings()
	if err != nil {
		t.Fatalf("GetEmailSettings: %v", err)
	}
	if reread.AdminGroupAddress != "toft-admins@corp.example" {
		t.Errorf("stored group address = %q, want it back verbatim", reread.AdminGroupAddress)
	}

	// The audit row carries it in the clear. Unlike the two credentials in this
	// document it is not a secret, and "who redirected every administrator
	// notification to an address of their own" is exactly the question this log
	// exists to answer — a change recorded with its substance redacted would be
	// visible and useless.
	var audited string
	if err := db.Conn().QueryRow(
		`SELECT after FROM audit_log WHERE action = 'email_settings.update'
		 ORDER BY occurred_at DESC LIMIT 1`).Scan(&audited); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if !strings.Contains(audited, "toft-admins@corp.example") {
		t.Errorf("audit row does not record the group address that was set: %s", audited)
	}
}

// The case the individual addresses cannot cover. Provider-supplied addresses
// may be absent entirely, and before the group address that meant admin mail had
// nowhere to go — the setting is most useful exactly here.
func TestAdminGroupAddressReachesAnInstallWithNoAdminAddresses(t *testing.T) {
	db := dbtest.New(t)

	if _, err := db.Conn().Exec(`UPDATE users SET email = '' WHERE system_role = 'system_admin'`); err != nil {
		t.Fatalf("clearing admin addresses: %v", err)
	}
	empty, err := db.AdminRecipients()
	if err != nil {
		t.Fatalf("AdminRecipients: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("recipients = %v, want none — the premise of this test", empty)
	}

	settings, err := db.GetEmailSettings()
	if err != nil {
		t.Fatalf("GetEmailSettings: %v", err)
	}
	settings.AdminGroupAddress = "ops@corp.example"
	if err := db.SetEmailSettings(&settings, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("SetEmailSettings: %v", err)
	}

	to, err := db.AdminRecipients()
	if err != nil {
		t.Fatalf("AdminRecipients: %v", err)
	}
	if len(to) != 1 || to[0] != "ops@corp.example" {
		t.Fatalf("recipients = %v, want the group address", to)
	}
}

// Clearing it is a state an administrator has to be able to get back to:
// notifying each administrator individually is the default, not a one-way door.
func TestAdminGroupAddressCanBeCleared(t *testing.T) {
	db := dbtest.New(t)
	seedAdmin(t, db, "ops")

	settings, err := db.GetEmailSettings()
	if err != nil {
		t.Fatalf("GetEmailSettings: %v", err)
	}
	settings.AdminGroupAddress = "toft-admins@corp.example"
	if err := db.SetEmailSettings(&settings, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("SetEmailSettings: %v", err)
	}
	settings.AdminGroupAddress = ""
	if err := db.SetEmailSettings(&settings, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("SetEmailSettings clearing: %v", err)
	}

	to, err := db.AdminRecipients()
	if err != nil {
		t.Fatalf("AdminRecipients: %v", err)
	}
	if len(to) == 1 && to[0] == "toft-admins@corp.example" {
		t.Fatal("the group address survived being cleared")
	}
	if len(to) == 0 {
		t.Fatal("clearing the group address reached nobody, rather than falling back to the administrators")
	}
}
