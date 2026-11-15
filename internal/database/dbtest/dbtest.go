// Package dbtest provisions isolated Postgres databases for tests.
//
// Each call to New creates a fresh database, migrates it, and drops it when the
// test finishes, so tests never see each other's rows and can run in parallel.
//
// Tests that need a database skip when TEST_DATABASE_URL is unset — that keeps
// `go test ./...` green on a machine with no Postgres. Bring one up with:
//
//	docker compose up -d
//	export TEST_DATABASE_URL='postgres://toft:toft@localhost:5432/toft?sslmode=disable'
package dbtest

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"toft/internal/database"
	"toft/internal/password"
)

// EnvVar names the connection string used to provision test databases.
const EnvVar = "TEST_DATABASE_URL"

// Every test database bootstraps the built-in administrator's password, which at
// production cost is a quarter of a second per database — a minute across a
// suite that provisions a few hundred. The stored hash carries its own iteration
// count, so a cheap one verifies exactly like an expensive one; nothing under
// test behaves differently.
func init() {
	password.SetIterationsForTesting(1)
}

// New returns a migrated, empty database dedicated to this test.
func New(t *testing.T, opts ...database.Option) *database.DB {
	t.Helper()
	db, _ := provision(t, false, opts...)
	return db
}

// NewCounting is New with a driver that counts every round trip to Postgres,
// for the tests whose subject is *how many queries* a reader makes rather than
// what it answers.
//
// Counting at the driver rather than from pg_stat_database is the whole point.
// The server's counters are accumulated per backend and reported when that
// backend next goes idle, so out of a connection pool they see one connection's
// work and miss the rest — which is exactly the shape of an N+1, and made an
// earlier version of TestEstateReachIsOneQuery pass against a query deliberately
// sabotaged to run one statement per app.
//
// The returned function reports the count so far; take it before and after.
func NewCounting(t *testing.T, opts ...database.Option) (*database.DB, func() int64) {
	t.Helper()
	return provision(t, true, opts...)
}

func provision(t *testing.T, counting bool, opts ...database.Option) (*database.DB, func() int64) {
	t.Helper()

	baseDSN := os.Getenv(EnvVar)
	if baseDSN == "" {
		t.Skipf("%s not set — skipping database test (see package docs)", EnvVar)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The admin connection only exists to CREATE/DROP the per-test database;
	// it cannot be the same connection, since Postgres refuses to drop a
	// database that has an open session on it.
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("opening %s: %v", EnvVar, err)
	}
	defer admin.Close()

	if err := admin.PingContext(ctx); err != nil {
		// Set but unreachable is a real failure, not a reason to skip: the
		// author asked for these tests to run.
		t.Fatalf("connecting to %s (%s): %v", EnvVar, redact(baseDSN), err)
	}

	name := "toft_test_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
	// The name is generated here, never caller-supplied, so interpolating it
	// is safe — Postgres has no placeholder form for an identifier.
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("creating test database %s: %v", name, err)
	}

	dsn, err := swapDatabase(baseDSN, name)
	if err != nil {
		t.Fatalf("building test DSN: %v", err)
	}

	queries := func() int64 { return 0 }
	if counting {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parsing the test DSN: %v", err)
		}
		tracer := &queryCounter{}
		cfg.Tracer = tracer
		// RegisterConnConfig hands back a key that stdlib's driver resolves
		// back to this config, so database.Open's hard-coded sql.Open("pgx",
		// …) picks up the tracer with no seam in the production package.
		dsn = stdlib.RegisterConnConfig(cfg)
		queries = tracer.count
	}

	db, err := database.Open(ctx, dsn, opts...)
	if err != nil {
		// Best-effort cleanup; the CREATE succeeded, so the database exists.
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+name+`"`)
		t.Fatalf("opening test database: %v", err)
	}

	t.Cleanup(func() {
		// Every database-backed test is the audit-scope check: a writer that
		// forgets its app, or files an install act under one, fails the test
		// that drove it, naming the action (internal/database/audit_scope.go).
		if bad, err := db.AuditScopeViolations(); err != nil {
			t.Logf("cleanup: reading the audit scope: %v", err)
		} else if len(bad) > 0 {
			t.Errorf("audit rows written against their scope: %s", strings.Join(bad, "; "))
		}
		db.Close()

		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()

		dropper, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Logf("cleanup: reopening admin connection: %v", err)
			return
		}
		defer dropper.Close()

		if _, err := dropper.ExecContext(dropCtx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Logf("cleanup: dropping %s: %v", name, err)
		}
	})

	return db, queries
}

// queryCounter counts every statement the driver sends, across every pooled
// connection. TraceQueryStart fires once per Query, QueryRow and Exec.
type queryCounter struct{ n atomic.Int64 }

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	q.n.Add(1)
	return ctx
}

func (q *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (q *queryCounter) count() int64 { return q.n.Load() }

// swapDatabase rewrites the database name in a Postgres URL, preserving
// credentials, host, and query parameters such as sslmode.
func swapDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// redact strips the password so a connection failure can be logged safely.
func redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<unparseable dsn>"
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}
