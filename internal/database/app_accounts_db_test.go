package database_test

import (
	"errors"
	"sync"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// An app account is given its number once and keeps it: the database is the
// source, so a container's /etc/passwd recreated in any order gives every app
// the number its files carry (ADR §122). Two first starts at once agree.
func TestAnAppAccountKeepsItsNumber(t *testing.T) {
	db := dbtest.New(t)
	a, err := db.AppAccountNumber("toft-app-alpha")
	if err != nil || a != database.DefaultAccountIDLow {
		t.Fatalf("first account = %d, %v; want %d", a, err, database.DefaultAccountIDLow)
	}
	b, _ := db.AppAccountNumber("toft-app-beta")
	again, _ := db.AppAccountNumber("toft-app-alpha")
	if b != a+1 || again != a {
		t.Errorf("beta = %d, alpha again = %d; want %d and %d", b, again, a+1, a)
	}

	var wg sync.WaitGroup
	got := make([]int, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) { defer wg.Done(); got[i], _ = db.AppAccountNumber("toft-app-gamma") }(i)
	}
	wg.Wait()
	for _, n := range got {
		if n != got[0] || n == 0 {
			t.Fatalf("concurrent first starts disagreed: %v", got)
		}
	}
}

// A deleted app's number is retired, never reused — not by the next app, and
// not by a new app on the same slug, which could otherwise be handed files the
// deleted one left in shared storage.
func TestARetiredAccountNumberIsNeverReused(t *testing.T) {
	db := dbtest.New(t, database.WithAccountIDRange(63500, 63501))
	old, _ := db.AppAccountNumber("toft-app-alpha")
	if err := db.RetireAppAccount("toft-app-alpha"); err != nil {
		t.Fatal(err)
	}
	reborn, err := db.AppAccountNumber("toft-app-alpha")
	if err != nil || reborn == old {
		t.Fatalf("same slug after delete = %d, %v; want a new number, not %d", reborn, err, old)
	}
	if _, err := db.AppAccountNumber("toft-app-beta"); !errors.Is(err, database.ErrAccountRangeExhausted) {
		t.Errorf("a range of two after two numbers: err = %v, want ErrAccountRangeExhausted", err)
	}
}
