package database_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// internal/database/schema.sql is the schema at head, committed so that "what
// is the schema?" has an answer that does not involve migrating a database to
// find out. These tests are what stop it drifting away from the migrations it
// claims to describe.
//
// The check is structural rather than a byte diff, and deliberately so. The
// snapshot is produced by pg_dump, which is not on the PATH of a box that runs
// Postgres in a container — the usual arrangement here — so a byte comparison
// would skip exactly where it most needs to run. Comparing the *inventory*
// instead (tables, columns, indexes, named constraints) needs nothing but the
// database connection the rest of this package's tests already have, so it runs
// everywhere, and it catches the failure that actually happens: a migration
// that adds or drops something and forgets to regenerate the snapshot.
//
// Regenerate with scripts/schema-snapshot.sh.

const schemaSnapshotPath = "schema.sql"

func TestSchemaSnapshotIsCurrent(t *testing.T) {
	db := dbtest.New(t)

	snapshot, err := os.ReadFile(schemaSnapshotPath)
	if err != nil {
		t.Fatalf("reading %s: %v", schemaSnapshotPath, err)
	}
	text := string(snapshot)

	for _, c := range []struct {
		what   string
		inFile []string
		inDB   []string
	}{
		{"table", snapshotTables(text), liveNames(t, db, `
			SELECT tablename FROM pg_tables WHERE schemaname = 'public'`)},
		{"column", snapshotColumns(text), liveNames(t, db, `
			SELECT table_name || '.' || column_name FROM information_schema.columns
			WHERE table_schema = 'public'`)},
		// Only free-standing indexes. An index that backs a primary key or a
		// UNIQUE constraint is spelled ADD CONSTRAINT by pg_dump, never
		// CREATE INDEX, and is compared as a constraint below.
		{"index", snapshotIndexes(text), liveNames(t, db, `
			SELECT c.relname FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind = 'i'
			  AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = c.oid)`)},
		{"constraint", snapshotConstraints(text), liveNames(t, db, `
			SELECT conname FROM pg_constraint c
			JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public'`)},
		// The fifth dimension, and the one the collapse made load-bearing. A
		// generated baseline carries COMMENT ON and carries no file comments, so
		// the reasoning *is* schema now — and a table added without one is a
		// paragraph of argument that was never written down, which is the
		// failure the second baseline exists to stop repeating. Compared by the
		// object commented rather than by the text: the text is prose and a diff
		// of prose is a diff nobody reads.
		{"comment", snapshotComments(text), liveNames(t, db, `
			SELECT c.relname || COALESCE('.' || a.attname, '')
			FROM pg_description d
			JOIN pg_class c ON c.oid = d.objoid
			JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
			LEFT JOIN pg_attribute a
			  ON a.attrelid = c.oid AND a.attnum = d.objsubid AND d.objsubid > 0
			WHERE c.relkind = 'r'`)},
	} {
		missing, extra := diffNames(withoutGoose(c.inFile), withoutGoose(c.inDB))
		for _, m := range missing {
			t.Errorf("%s %q is in the database but not in %s — regenerate it with scripts/schema-snapshot.sh",
				c.what, m, schemaSnapshotPath)
		}
		for _, e := range extra {
			t.Errorf("%s %q is in %s but not in the database — the snapshot describes a schema the migrations no longer produce",
				c.what, e, schemaSnapshotPath)
		}
	}
}

// Every table says what it is, and that is a rule rather than a habit.
//
// The comment dimension above compares the snapshot file against the database,
// which catches a comment that was added and not regenerated -- and cannot
// catch the failure that actually happens, because a table added without one is
// missing it on both sides and the diff is empty. 00036 proved that within a
// day of the review: it added apps.memory_limit_mb, whose 0 means "the
// install's number" rather than "uncapped", and every test in this package
// passed.
//
// So the rule is stated where it can fail. Tables only: a table with nothing to
// say about itself does not exist, whereas most columns are their own
// documentation and an exception list holding three hundred of them would be a
// list nobody reads. What a column deserves stays a judgement, and ADR
// "The schema is decided once and captured" says so rather than pretending
// otherwise.
func TestEveryTableCarriesAComment(t *testing.T) {
	db := dbtest.New(t)

	for _, table := range withoutGoose(liveNames(t, db, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		  AND obj_description(c.oid) IS NULL`)) {
		t.Errorf("table %q has no COMMENT ON -- the schema is the only place its "+
			"reasoning can live now that the migration chain is one generated file", table)
	}
}

// The seeds are the half pg_dump --schema-only cannot see, and the half a new
// migration is most likely to lose: a table that vanishes is a compile-or-query
// error somewhere, whereas a missing seed row is a fresh install that simply
// does not work.
func TestFreshDatabaseCarriesItsSeeds(t *testing.T) {
	db := dbtest.New(t)

	for _, c := range []struct{ what, query string }{
		{"the Framework tag", `SELECT COUNT(*) FROM tags WHERE id = 'tag-framework' AND kind = 'system'`},
		{"the local auth provider", `SELECT COUNT(*) FROM auth_providers WHERE id = 'local' AND kind = 'password'`},
		{"the built-in administrator", `SELECT COUNT(*) FROM users WHERE id = 'builtin-admin' AND system_role = 'system_admin'`},
		{"the built-in administrator's identity", `SELECT COUNT(*) FROM user_identities WHERE provider_id = 'local' AND user_id = 'builtin-admin'`},
	} {
		var n int
		if err := db.Conn().QueryRow(c.query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if n != 1 {
			t.Errorf("a fresh database has %d rows for %s, want exactly 1", n, c.what)
		}
	}

	// There is deliberately no assertion about user_passwords here. The schema
	// seeds no password — the binary writes one at startup, because a hash in a
	// migration would be a copy of a Go function that cannot be kept in step —
	// but dbtest bootstraps one into every database it hands out, so this is
	// not the place that fact can be observed.
}

// TestSchemaSnapshotScratch is not a test — it is how scripts/schema-snapshot.sh
// gets a database migrated to head without a second binary to build. It skips
// unless that script invoked it.
func TestSchemaSnapshotScratch(t *testing.T) {
	base, name := os.Getenv("SCHEMAGEN_DSN"), os.Getenv("SCHEMAGEN_DB")
	if base == "" || name == "" {
		t.Skip("not invoked by scripts/schema-snapshot.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("opening %s: %v", base, err)
	}
	defer admin.Close()

	// The name comes from the script, never from a caller, so interpolating it
	// is safe — Postgres has no placeholder form for an identifier.
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`"`); err != nil {
		t.Fatalf("dropping %s: %v", name, err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing DSN: %v", err)
	}
	u.Path = "/" + name

	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	defer conn.Close()

	if err := database.Migrate(ctx, conn); err != nil {
		t.Fatalf("migrating %s: %v", name, err)
	}
}

var (
	reSnapTable      = regexp.MustCompile(`(?m)^CREATE TABLE public\.(\w+) \($`)
	reSnapIndex      = regexp.MustCompile(`(?m)^CREATE (?:UNIQUE )?INDEX (\w+) ON `)
	reSnapConstraint = regexp.MustCompile(`(?:ADD )?CONSTRAINT (\w+) `)
	reSnapComment    = regexp.MustCompile(`(?m)^COMMENT ON (?:TABLE|COLUMN) ([\w.]+) IS `)
)

func snapshotTables(text string) []string {
	var out []string
	for _, m := range reSnapTable.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

func snapshotIndexes(text string) []string {
	var out []string
	for _, m := range reSnapIndex.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// Primary keys, unique constraints and foreign keys arrive as ADD CONSTRAINT
// after the table; CHECKs are inline inside it. Both spellings are matched,
// including the names Postgres invents for an unnamed inline CHECK.
func snapshotConstraints(text string) []string {
	var out []string
	for _, m := range reSnapConstraint.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// snapshotComments takes the object each COMMENT ON names, not the prose it
// carries. pg_dump writes the schema qualifier and the live catalog does not, so
// the prefix is stripped to compare like with like.
func snapshotComments(text string) []string {
	var out []string
	for _, m := range reSnapComment.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.TrimPrefix(m[1], "public."))
	}
	return out
}

// snapshotColumns walks each CREATE TABLE body and takes the first token of
// every line that declares a column. CONSTRAINT lines are skipped — they are
// covered by snapshotConstraints — as is the generated-column continuation,
// which is indented further than a declaration ever is.
func snapshotColumns(text string) []string {
	var out []string
	var table string
	for _, line := range strings.Split(text, "\n") {
		if m := reSnapTable.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		if table == "" {
			continue
		}
		if strings.HasPrefix(line, ");") {
			table = ""
			continue
		}
		if !strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "     ") {
			continue
		}
		field := strings.TrimSpace(line)
		if field == "" || strings.HasPrefix(field, "CONSTRAINT ") {
			continue
		}
		name := strings.SplitN(field, " ", 2)[0]
		if name == "" {
			continue
		}
		out = append(out, table+"."+strings.TrimSuffix(name, ","))
	}
	return out
}

func liveNames(t *testing.T, db *database.DB, query string) []string {
	t.Helper()
	rows, err := db.Conn().Query(query)
	if err != nil {
		t.Fatalf("reading the live schema: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning the live schema: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the live schema: %v", err)
	}
	return out
}

// goose's own bookkeeping table is not part of the schema the snapshot
// describes — it is how the snapshot's version is reached — so it is dropped
// from both sides rather than from one.
func withoutGoose(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.HasPrefix(n, "goose_db_version") {
			continue
		}
		out = append(out, n)
	}
	return out
}

func diffNames(inFile, inDB []string) (missingFromFile, extraInFile []string) {
	f, d := map[string]bool{}, map[string]bool{}
	for _, s := range inFile {
		f[s] = true
	}
	for _, s := range inDB {
		d[s] = true
	}
	for s := range d {
		if !f[s] {
			missingFromFile = append(missingFromFile, s)
		}
	}
	for s := range f {
		if !d[s] {
			extraInFile = append(extraInFile, s)
		}
	}
	sort.Strings(missingFromFile)
	sort.Strings(extraInFile)
	return
}
