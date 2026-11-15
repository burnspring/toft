package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// Two transactions that each remove a different one of the last two unlocked
// administrators are serialised: the second counts after the first commits,
// and sees nobody left (security review 2026-11-14, DB-10).
func TestTheLastUnlockedAdministratorCountIsSerialised(t *testing.T) {
	db := dbtest.New(t)
	for _, id := range []string{"adm-a", "adm-b"} {
		if err := db.UpsertUser(&database.User{ID: id, Login: id, SystemRole: database.SystemRoleAdmin}); err != nil {
			t.Fatal(err)
		}
	}
	// Every other administrator — the built-in one included — locked.
	if _, err := db.Conn().Exec(`UPDATE users SET locked_at=NOW() WHERE system_role=? AND id NOT IN ('adm-a','adm-b')`,
		database.SystemRoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn().Exec(`UPDATE users SET system_role=? WHERE id IN ('adm-a','adm-b')`, database.SystemRoleAdmin); err != nil {
		t.Fatal(err)
	}

	first, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if n, err := database.OtherUnlockedAdminsTx(first, "adm-a"); err != nil || n != 1 {
		t.Fatalf("first count = %d, %v; want 1", n, err)
	}

	counted := make(chan int, 1)
	go func() {
		second, err := db.Conn().Begin()
		if err != nil {
			counted <- -1
			return
		}
		defer second.Rollback()
		n, err := database.OtherUnlockedAdminsTx(second, "adm-b")
		if err != nil {
			n = -1
		}
		counted <- n
	}()
	select {
	case n := <-counted:
		t.Fatalf("the second count returned %d while the first transaction was open", n)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := first.Exec(`UPDATE users SET system_role=? WHERE id='adm-a'`, database.SystemRoleViewer); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := <-counted; n != 0 {
		t.Errorf("second count = %d after the first demotion committed, want 0", n)
	}
}
