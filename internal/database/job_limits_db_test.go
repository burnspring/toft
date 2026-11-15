package database_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// An administrator's override on a job survives a redeploy: the deploy's
// upsert never names it. Silent if broken — the job would quietly drop back
// to the default.
func TestAJobOverrideSurvivesARedeploy(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "demo")
	def := seedJobDef(t, db, app.ID, "nightly", "")
	mem, cpu := 4096, 500
	if err := db.SetJobResources(app.ID, def.ID, &mem, &cpu, database.PrincipalTypeUser, "alice", "req", "127.0.0.1", "test"); err != nil {
		t.Fatal(err)
	}

	// The next deploy, from a manifest that knows nothing of the override.
	if err := db.ReconcileManifestJobs(app.ID, []database.JobDefinition{
		{Name: "nightly", Command: "python report.py --v2", TimeoutSec: 600},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetJobDefinitionByName(app.ID, "nightly")
	if err != nil || got == nil {
		t.Fatalf("definition: %v", err)
	}
	if got.Command != "python report.py --v2" {
		t.Errorf("the deploy did not apply: %q", got.Command)
	}
	if got.OverrideMemoryMB != 4096 || got.OverrideCPUMillicores != 500 {
		t.Errorf("overrides after a redeploy = %d MB, %dm; want 4096, 500", got.OverrideMemoryMB, got.OverrideCPUMillicores)
	}

	// 0 clears one, and leaves the other.
	zero := 0
	if err := db.SetJobResources(app.ID, def.ID, &zero, nil, database.PrincipalTypeUser, "alice", "req", "127.0.0.1", "test"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetJobDefinitionByName(app.ID, "nightly")
	if got.OverrideMemoryMB != 0 || got.OverrideCPUMillicores != 500 {
		t.Errorf("after clearing memory = %d MB, %dm", got.OverrideMemoryMB, got.OverrideCPUMillicores)
	}

	// A job removed from the manifest and added back starts over.
	if err := db.ReconcileManifestJobs(app.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileManifestJobs(app.ID, []database.JobDefinition{{Name: "nightly", Command: "x"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetJobDefinitionByName(app.ID, "nightly")
	if got.OverrideCPUMillicores != 0 {
		t.Errorf("a re-added job kept an override: %dm", got.OverrideCPUMillicores)
	}
}

// Migration 00076 keeps every job's memory: a declared 4096, an app's 2048
// override, and a job on neither at the install's app default. The default
// in the settings row is visible to SQL; one in the environment is resolved
// at the next start.
func TestTheJobLimitMigrationKeepsEveryJobsMemory(t *testing.T) {
	for _, c := range []struct {
		name       string
		installRow string // app_memory_limit_mb's row, "" for none
		envDefault int    // APP_MEMORY_LIMIT_MB at the next start
		wantPlain  int    // the job on neither, after the start; 0 is NULL
	}{
		{"install default in the settings row, at the job default", "1024", 512, 0},
		{"install default in the environment", "", 768, 768},
		{"install default in the environment, at the job default", "", 1024, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, dsn := scratchDatabase(t)
			ctx := context.Background()
			if err := database.MigrateUpToForTest(ctx, conn, 75); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				`INSERT INTO users (id, login) VALUES ('u1', 'alice')`,
				`INSERT INTO apps (id, slug, name, repo_url, owner_user_id) VALUES ('a1', 'one', 'one', '', 'u1'), ('a2', 'two', 'two', '', 'u1')`,
				`UPDATE apps SET memory_limit_mb = 2048 WHERE id = 'a2'`,
				`INSERT INTO job_definitions (id, app_id, name, command, memory_mb) VALUES
					('declared', 'a1', 'declared', 'x', 4096),
					('appwide', 'a2', 'appwide', 'x', 0),
					('plain', 'a1', 'plain', 'x', 0)`,
			} {
				if _, err := conn.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if c.installRow != "" {
				if _, err := conn.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('app_memory_limit_mb', $1)`, c.installRow); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.Migrate(ctx, conn); err != nil {
				t.Fatal(err)
			}
			db, err := database.Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.SeedJobMemoryOverrides(c.envDefault, 1024); err != nil {
				t.Fatal(err)
			}
			for id, want := range map[string]int{"declared": 4096, "appwide": 2048, "plain": c.wantPlain} {
				var got sql.NullInt64
				if err := conn.QueryRowContext(ctx, `SELECT override_memory_mb FROM job_definitions WHERE id = $1`, id).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if (want == 0 && got.Valid) || (want != 0 && got.Int64 != int64(want)) {
					t.Errorf("%s: override = %v, want %d (0 is NULL)", id, got, want)
				}
			}
		})
	}
}

// scratchDatabase is an empty database of its own, unmigrated, dropped after.
func scratchDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	base := os.Getenv(dbtest.EnvVar)
	if base == "" {
		t.Skipf("%s not set", dbtest.EnvVar)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	name := "toft_test_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
		admin.Close()
	})
	return conn, u.String()
}
