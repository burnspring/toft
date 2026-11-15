package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3/lock"

	"toft/internal/database"
)

// DB1-10: Migrate's comment promised a session-level advisory lock that goose
// only takes when handed a locker, and it was handed none — two nodes booting
// at once both ran the same migration and the loser crash-looped. Held here
// from another session, the lock must make Migrate wait; and two Migrates on a
// fresh database at once must both succeed.
func TestMigrateWaitsForTheAdvisoryLock(t *testing.T) {
	conn := rawScratchDatabase(t)
	ctx := context.Background()

	holder, err := conn.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lock.DefaultLockID); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}

	done := make(chan error, 2)
	go func() { done <- database.Migrate(ctx, conn) }()
	go func() { done <- database.Migrate(ctx, conn) }()

	select {
	case err := <-done:
		t.Fatalf("Migrate returned (%v) while another session held the migration lock", err)
	case <-time.After(1500 * time.Millisecond):
	}

	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lock.DefaultLockID); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Migrate %d after the lock was released: %v", i, err)
			}
		case <-time.After(2 * time.Minute):
			t.Fatal("Migrate never finished after the lock was released")
		}
	}
}
