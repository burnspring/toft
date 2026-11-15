package database_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// A pending sign-in is a row held for ten minutes, and nothing bounded how many
// one caller could hold. One source gets twenty at once; another source is
// unaffected.
func TestPendingSignInRequestsAreBoundedPerSource(t *testing.T) {
	db := dbtest.New(t)
	now := time.Now().UTC()
	mk := func(i int, from string) error {
		return db.CreateDeviceCode(&database.DeviceCode{
			UserCode: fmt.Sprintf("CODE-%04d", i), Client: database.DeviceClientLP,
			RequestedFrom: from, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		}, fmt.Sprintf("hash-%d", i))
	}
	for i := 0; i < 20; i++ {
		if err := mk(i, "198.51.100.9"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if err := mk(20, "198.51.100.9"); !errors.Is(err, database.ErrTooManyPendingCodes) {
		t.Fatalf("the twenty-first pending request from one source = %v, want ErrTooManyPendingCodes", err)
	}
	if err := mk(21, "203.0.113.7"); err != nil {
		t.Fatalf("another source was refused: %v", err)
	}
}
