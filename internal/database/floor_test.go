package database_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The floor, exercised — because without a test it is four lines nobody runs,
// guarding an install nobody has yet.
//
// There are three kinds of database, and two of the three do nothing
// observable: a converged install records head's version already and goose
// runs nothing, and a fresh one records nothing and runs the whole directory.
// The third is the one that has to be refused, and
// it is the only one whose failure is silent otherwise — goose does not treat a
// version in the database with no file as a problem, so it would read the
// baseline as pending and run a from-nothing CREATE against populated tables.
// Measured, that fails partway through with a duplicate-object message naming
// whichever function or table it reached first: at boot, with no explanation,
// on a schema that is not wrong.
func TestSchemaVersionFloorRefuses(t *testing.T) {
	floor, head := migrationBoundsFromDisk(t)

	t.Run("one short of the floor is refused by name", func(t *testing.T) {
		conn := rawScratchDatabase(t)

		stamp(t, conn, floor-1)

		err := database.Migrate(context.Background(), conn)
		if err == nil {
			t.Fatal("a database below the floor migrated; goose would have run the baseline's " +
				"CREATE statements against tables that already exist")
		}
		// Both numbers, because "unsupported schema version" without them is a
		// message an operator cannot act on.
		for _, want := range []string{strconv.FormatInt(floor-1, 10), strconv.FormatInt(floor, 10)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name version %s: %v", want, err)
			}
		}
	})

	t.Run("a database that has never been migrated passes", func(t *testing.T) {
		conn := rawScratchDatabase(t)

		if err := database.Migrate(context.Background(), conn); err != nil {
			t.Fatalf("a fresh database was refused: %v", err)
		}

		// Zero is not "below the floor", it is "nothing has run yet" — and the
		// distinction is the whole reason the check is `0 < max < floor` rather
		// than `max < floor`.
		var version int64
		if err := conn.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil {
			t.Fatalf("reading the version: %v", err)
		}
		// Head, not the floor. These are the same number only while the
		// baseline is the newest file in the directory, and the baseline's own
		// header says a schema change is a migration above it -- so a test
		// pinned to the floor here fails on the first ordinary migration and
		// reads as the migration being wrong.
		if version != head {
			t.Errorf("a fresh database came out at version %d, want head's %d", version, head)
		}
	})

	t.Run("a converged database is untouched", func(t *testing.T) {
		db := dbtest.New(t)

		// dbtest hands out a migrated database, so this is the case that has to
		// do nothing at all: calling Migrate again is what every boot does.
		if err := database.Migrate(context.Background(), db.Conn().SQL()); err != nil {
			t.Fatalf("re-migrating a converged database: %v", err)
		}
	})
}

// migrationBoundsFromDisk reads both ends of the migration directory the way an
// operator would: the lowest number is the floor a database must already be on,
// the highest is where a fresh one lands. Deliberately not asked of the package
// — a test that reads the value from the code it is testing proves the code
// agrees with itself.
func migrationBoundsFromDisk(t *testing.T) (floor, head int64) {
	t.Helper()

	entries, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(entries) == 0 {
		t.Fatalf("globbing migrations: %v (%d found)", err, len(entries))
	}
	for _, name := range entries {
		prefix, _, ok := strings.Cut(filepath.Base(name), "_")
		if !ok {
			t.Fatalf("migration %q has no version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %q has an unreadable version prefix: %v", name, err)
		}
		if floor == 0 || version < floor {
			floor = version
		}
		if version > head {
			head = version
		}
	}
	return floor, head
}

// rawScratchDatabase is dbtest's database without dbtest's migration: these
// cases are about what Migrate does when it is called, so it cannot already
// have been.
func rawScratchDatabase(t *testing.T) *sql.DB {
	t.Helper()

	baseDSN := os.Getenv(dbtest.EnvVar)
	if baseDSN == "" {
		t.Skipf("%s not set — skipping database test", dbtest.EnvVar)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("opening %s: %v", dbtest.EnvVar, err)
	}
	defer admin.Close()

	name := fmt.Sprintf("toft_test_floor_%d", time.Now().UnixNano())
	// Generated here and never caller-supplied, so interpolating it is safe;
	// Postgres has no placeholder form for an identifier.
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}

	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	u.Path = "/" + name

	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}

	t.Cleanup(func() {
		conn.Close()
		dropper, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`); err != nil {
			t.Logf("cleanup: dropping %s: %v", name, err)
		}
	})

	return conn
}

// stamp writes goose's bookkeeping without goose: a database that claims to be
// at a version whose file this binary does not carry. That is exactly the state
// an install is in after upgrading past a rebaseline it skipped, and there is no
// other way to produce it once the chain is gone.
func stamp(t *testing.T, conn *sql.DB, version int64) {
	t.Helper()

	if _, err := conn.Exec(`
		CREATE TABLE goose_db_version (
			id         SERIAL PRIMARY KEY,
			version_id BIGINT NOT NULL,
			is_applied BOOLEAN NOT NULL,
			tstamp     TIMESTAMP NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("creating the version table: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, TRUE), ($1, TRUE)`,
		version); err != nil {
		t.Fatalf("stamping version %d: %v", version, err)
	}
}
