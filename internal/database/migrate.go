package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// migrationFS carries the schema into the binary, so a deployed Toft
// migrates itself on startup with no external tooling, no migration files on
// the host, and no goose CLI.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

func newProvider(conn *sql.DB, opts ...goose.ProviderOption) (*goose.Provider, error) {
	// goose globs "*.sql" at the root of the filesystem it is handed, so the
	// embed FS — where the files sit under migrations/ — has to be rooted there
	// first. Passing migrationFS directly finds nothing and migrating becomes a
	// silent no-op.
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("rooting migration filesystem: %w", err)
	}
	return goose.NewProvider(goose.DialectPostgres, conn, sub, opts...)
}

// migrationLocker is the session-level advisory lock Migrate holds while it
// applies anything. goose takes no lock unless it is handed one — "If
// WithSessionLocker is not called, locking is disabled" — so this is what
// makes two nodes booting at once safe, rather than a property of goose.
//
// goose's own lock id, so a goose CLI pointed at the same database by hand
// waits too. An advisory lock is scoped to its database, so test databases on
// one server never contend. The wait is long because the holder may be
// running a migration that rewrites a large table: thirty minutes, probed
// every second, and a node still waiting after that fails its boot.
func migrationLocker() (goose.ProviderOption, error) {
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 30*60))
	if err != nil {
		return nil, err
	}
	return goose.WithSessionLocker(locker), nil
}

// PendingMigrations counts migrations this binary carries that the database has
// not applied.
//
// Exported for the system checks page, and the provider is package-private for
// the reason it should be: nothing outside here may apply one.
//
// **What it can actually catch is narrow, and saying so is the point.** A
// process that failed to migrate does not exist to be asked: `New` closes the
// pool and returns the error, so a role that cannot DDL, a build pointed at
// somebody else's database and a database below the floor are all startup
// refusals rather than rows on a page. An older binary running beside a newer
// one carries *fewer* files, which goose reports as untracked and this does not
// count. What is left is the database moving underneath a process that is
// already up — a restore from a backup taken before the migrations — and that
// is the one this answers.
func PendingMigrations(ctx context.Context, conn *sql.DB) (int, error) {
	provider, err := newProvider(conn)
	if err != nil {
		return 0, fmt.Errorf("creating migration provider: %w", err)
	}
	status, err := provider.Status(ctx)
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, m := range status {
		if m.State == goose.StatePending {
			pending++
		}
	}
	return pending, nil
}

// schemaFloor is the version a database must already be on for this binary to
// migrate it: the baseline's own number, read from the embedded set rather than
// typed as a literal, so it cannot fall out of step with the file it guards.
//
// It is the *lowest* version present, which is the baseline whether or not
// migrations have since been added above it.
func schemaFloor() (int64, error) {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return 0, fmt.Errorf("globbing embedded migrations: %w", err)
	}
	floor := int64(0)
	for _, name := range entries {
		prefix, _, ok := strings.Cut(path.Base(name), "_")
		if !ok {
			return 0, fmt.Errorf("migration %q has no version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %q has an unreadable version prefix: %w", name, err)
		}
		if floor == 0 || version < floor {
			floor = version
		}
	}
	if floor == 0 {
		return 0, fmt.Errorf("no migrations embedded — check the //go:embed directive")
	}
	return floor, nil
}

// appliedVersion is the highest version this database has recorded, or zero if
// it has never been migrated. Asked of the table directly rather than through
// goose, because goose's own answer requires a provider and the whole point of
// the question is to decide whether to hand this database to one.
func appliedVersion(ctx context.Context, conn *sql.DB) (int64, error) {
	var present bool
	if err := conn.QueryRowContext(ctx,
		`SELECT to_regclass('public.goose_db_version') IS NOT NULL`).Scan(&present); err != nil {
		return 0, fmt.Errorf("looking for the migration table: %w", err)
	}
	if !present {
		return 0, nil
	}
	var version sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil {
		return 0, fmt.Errorf("reading the schema version: %w", err)
	}
	return version.Int64, nil
}

// Migrate brings the database up to the latest schema version. It is safe to
// call on every boot, and safe when several nodes boot at once: the provider
// holds a session-level advisory lock (migrationLocker) while it applies, so
// exactly one process applies a given migration while the others wait and then
// find nothing to do. Without that option goose locks nothing.
//
// **The floor is checked first, and it is the whole safety story of the second
// baseline.** The migration directory is one generated file at head's version,
// so there are exactly three kinds of database. One that has never been
// migrated records nothing, and the baseline builds it. One that ran the chain
// records the baseline's version already, so goose finds nothing to do and
// never reads the file. One that stopped in between records a version whose
// file no longer exists — and goose does not notice that, because a *missing*
// migration to goose means one present on disk and unapplied, never a version in
// the database with no file. It would read the baseline as pending and run a
// from-nothing CREATE against a populated database, which fails somewhere in the
// middle with a message about a duplicate object: at boot, with no explanation,
// on a schema that is not actually wrong.
//
// So that database is refused here instead, by name and in a sentence. It is a
// statement about which databases have an upgrade path rather than an upgrade
// path itself; the answer for one below the floor is to upgrade through the
// release that still carried the chain, or to restore a backup.
func Migrate(ctx context.Context, conn *sql.DB) error {
	floor, err := schemaFloor()
	if err != nil {
		return err
	}
	current, err := appliedVersion(ctx, conn)
	if err != nil {
		return err
	}
	if current > 0 && current < floor {
		return fmt.Errorf("this database is at schema version %d and this release requires %d: "+
			"the migrations that would carry it forward were collapsed into a baseline and no "+
			"longer exist, so it must be upgraded through the last release that carried them "+
			"(or restored from a backup) before this one can start", current, floor)
	}

	locker, err := migrationLocker()
	if err != nil {
		return fmt.Errorf("creating migration lock: %w", err)
	}
	provider, err := newProvider(conn, locker)
	if err != nil {
		return fmt.Errorf("creating migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	for _, r := range results {
		if r.Error != nil {
			return fmt.Errorf("migration %s: %w", r.Source.Path, r.Error)
		}
	}
	return nil
}
