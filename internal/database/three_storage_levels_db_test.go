package database_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// Migration 00074 over every (kind, path, old level) a database could hold.
//
// **A test whose failure would be silent.** A wrong promotion here is a
// permission nobody asked for, on a row that looks like every other. The
// schema is put back as it stood before the migration — four tokens in the
// CHECKs, the old triggers bypassed while seeding — and the Up block is read
// from the migration file itself and run whole, so this tests what ran rather
// than a copy of it.
func TestTheMigrationMergesOverwriteIntoFull(t *testing.T) {
	db := dbtest.New(t)
	conn := db.Conn().SQL()
	app := mkApp(t, db, "lvl")
	vol := mkFileStore(t, db, "share", "share")
	mp := mkResource(t, db, "drop", "drop", database.StorageKindStore)
	blob := mkResource(t, db, "azure", "azure", database.StorageKindStore)
	if _, err := conn.Exec(`UPDATE storage_resources SET backing='blob' WHERE id=$1`, blob.ID); err != nil {
		t.Fatal(err)
	}
	mkUserIn(t, db, "lvl-del", database.SystemRolePublisher, nil)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, c := range []struct{ table, col string }{
		{"storage_managers", "max_access"}, {"storage_mappings", "access"}, {"store_grants", "access"},
	} {
		exec(`ALTER TABLE ` + c.table + ` DROP CONSTRAINT ` + c.table + `_access_check`)
		exec(`ALTER TABLE ` + c.table + ` ADD CONSTRAINT ` + c.table + `_access_check CHECK (` + c.col +
			` = ANY (ARRAY['read','write','overwrite','full']))`)
		exec(`ALTER TABLE ` + c.table + ` DISABLE TRIGGER USER`)
	}

	type row struct{ table, id, kind, before, want, app string }
	var rows []row
	n := 0
	for _, res := range []*database.StorageResource{vol, mp, blob} {
		for _, level := range []string{"read", "write", "overwrite", "full"} {
			n++
			id := func(p string) string { return p + "-" + res.Slug + "-" + level }

			// A delegate's ceiling, and a mapping made under it, which is the
			// pair the ceilings-first order exists for.
			mkUserIn(t, db, "lvl-del-"+id(""), database.SystemRolePublisher, nil)
			exec(`INSERT INTO storage_managers (id, resource_id, subject_type, subject_id, max_access)
			      VALUES ($1, $2, 'user', $3, $4)`, id("sm"), res.ID, "lvl-del-"+id(""), level)
			exec(`INSERT INTO storage_mappings (id, resource_id, app_id, path, access, authority, folder)
			      VALUES ($1, $2, $3, $4, $5, $6, $7)`, id("map"), res.ID, app.ID,
				"/data/"+id(""), level, "user:lvl-del-"+id(""), level)
			// One subject, a folder per level, so the four grants are four rows.
			exec(`INSERT INTO store_grants (id, resource_id, subject_type, subject_id, access, folder)
			      VALUES ($1, $2, 'user', 'lvl-del', $3, $4)`, id("gr"), res.ID, level, level)

			promote := level == "overwrite"
			// On a volume a mapping (and the ceiling that caps it) at `write`
			// mounted read-write; its grant at `write` is create-only, and stays.
			renamed := res.Kind == database.StorageKindVolume && level == "write"
			want := func(p bool) string {
				if p {
					return "full"
				}
				return level
			}
			rows = append(rows,
				row{"storage_managers", id("sm"), res.Kind, level, want(promote || renamed), ""},
				row{"storage_mappings", id("map"), res.Kind, level, want(promote || renamed), app.ID},
				row{"store_grants", id("gr"), res.Kind, level, want(promote), ""},
			)
		}
	}
	for _, table := range []string{"storage_managers", "storage_mappings", "store_grants"} {
		exec(`ALTER TABLE ` + table + ` ENABLE TRIGGER USER`)
	}

	raw, err := os.ReadFile("migrations/00074_three_storage_levels.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(up); err != nil {
		_ = tx.Rollback()
		t.Fatalf("the migration: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	col := map[string]string{"storage_managers": "max_access", "storage_mappings": "access", "store_grants": "access"}
	target := map[string]string{"storage_managers": "storage_manager", "storage_mappings": "storage_mapping", "store_grants": "store_grant"}
	for _, r := range rows {
		var got string
		if err := conn.QueryRow(`SELECT `+col[r.table]+` FROM `+r.table+` WHERE id=$1`, r.id).Scan(&got); err != nil {
			t.Fatalf("%s %s: %v", r.table, r.id, err)
		}
		if got != r.want {
			t.Errorf("%s on a %s at %s = %q, want %q", r.table, r.kind, r.before, got, r.want)
		}

		var before, after []byte
		var appID string
		var count int
		err := conn.QueryRow(`SELECT count(*), COALESCE(max(before::text),''), COALESCE(max(after::text),''), COALESCE(max(app_id),'')
		    FROM audit_log WHERE action='storage.level_promoted' AND target_type=$1 AND target_id=$2`,
			target[r.table], r.id).Scan(&count, &before, &after, &appID)
		if err != nil {
			t.Fatal(err)
		}
		changed := r.want != r.before
		if changed != (count == 1) || count > 1 {
			t.Errorf("%s on a %s at %s: %d audit rows, want %d", r.table, r.kind, r.before, count,
				map[bool]int{true: 1, false: 0}[changed])
			continue
		}
		if !changed {
			continue
		}
		var b, a map[string]string
		_ = json.Unmarshal(before, &b)
		_ = json.Unmarshal(after, &a)
		if b["access"] != r.before || a["access"] != "full" {
			t.Errorf("%s %s: audit before %s after %s", r.table, r.id, before, after)
		}
		if appID != r.app {
			t.Errorf("%s %s: audit app_id = %q, want %q — a mapping's promotion is its app's history",
				r.table, r.id, appID, r.app)
		}
	}

	// The old token is gone from the schema, not merely from the data.
	if _, err := conn.Exec(`UPDATE store_grants SET access='overwrite' WHERE id=$1`, "gr-drop-read"); err == nil {
		t.Error("store_grants still accepts overwrite after the migration")
	}
	if n != 12 {
		t.Fatalf("seeded %d cases, want 12", n)
	}
}

// A fresh install has nothing to promote, so the migration writes no rows.
func TestAFreshInstallHasNoPromotionRows(t *testing.T) {
	db := dbtest.New(t)
	var n int
	if err := db.Conn().SQL().QueryRow(`SELECT count(*) FROM audit_log WHERE action='storage.level_promoted'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a fresh install has %d storage.level_promoted rows, want none", n)
	}
}

// The trigger ranks the ladder as Go does. The authority trigger's CASEs and
// storeAccessRank are two spellings of one order, and one that drifted would
// let a delegate attach above their ceiling — or refuse them at it. Inserted
// raw, so the trigger is what answers and not the Go check above it.
func TestTheTriggerRanksTheLadderAsGoDoes(t *testing.T) {
	db := dbtest.New(t)
	conn := db.Conn().SQL()
	app := mkApp(t, db, "rank")
	res := mkResource(t, db, "ranked", "ranked", database.StorageKindStore)
	ladder := []string{runtime.AccessRead, runtime.AccessWrite, runtime.AccessFull}
	for _, ceiling := range ladder {
		subject := "rank-" + ceiling
		mkUserIn(t, db, subject, database.SystemRolePublisher, nil)
		putManager(t, db, res.ID, database.StoreSubjectUser, subject, ceiling)
		for _, level := range ladder {
			_, err := conn.Exec(`INSERT INTO storage_mappings (id, resource_id, app_id, path, access, authority, folder)
			    VALUES ($1, $2, $3, $4, $5, $6, $7)`, subject+"-"+level, res.ID, app.ID,
				"/data/"+subject+"-"+level, level, "user:"+subject, subject+"-"+level)
			if want := database.StoreAccessAtLeast(ceiling, level); (err == nil) != want {
				t.Errorf("a delegate at %s attaching at %s: trigger said %v, Go says allowed=%v", ceiling, level, err, want)
			}
		}
	}
}
