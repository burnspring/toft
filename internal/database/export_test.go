package database

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// UpsertJobDefinitionForTest writes one definition, keyed by (app, name),
// without touching the app's others. Only tests do this: in the product a
// deploy reconciles every definition at once.
func UpsertJobDefinitionForTest(db *DB, d *JobDefinition) error {
	now := time.Now().UTC()
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	d.ParamsSpec = jobJSONOrEmpty(d.ParamsSpec)
	d.Overlap = overlapOrQueue(d.Overlap)
	d.Notify = notifyOrNone(d.Notify)
	return db.conn.QueryRow(`
		INSERT INTO job_definitions (`+jobDefColumns+`, override_memory_mb, override_cpu_millicores)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id, name) DO UPDATE SET
			command=excluded.command, args=excluded.args, schedule=excluded.schedule,
			params_spec=excluded.params_spec, timeout_sec=excluded.timeout_sec,
			overlap=excluded.overlap, notify=excluded.notify,
			enabled=excluded.enabled, next_run_at=excluded.next_run_at,
			expect_within_sec=excluded.expect_within_sec, updated_at=excluded.updated_at,
			override_memory_mb=excluded.override_memory_mb,
			override_cpu_millicores=excluded.override_cpu_millicores
		RETURNING id, created_at, enabled_at`,
		d.ID, d.AppID, d.Name, d.Command, argsOrNull(d.Args), nullIfEmpty(d.Schedule),
		[]byte(d.ParamsSpec), d.TimeoutSec, d.Overlap, d.Notify,
		d.Enabled, d.NextRunAt, now, now, d.ExpectWithinSec, now,
		nullIfZero(d.OverrideMemoryMB), nullIfZero(d.OverrideCPUMillicores)).Scan(&d.ID, &d.CreatedAt, &d.EnabledAt)
}

// SetAccessRecordBatchForTest shrinks the access-record sweep's batch, so a
// test can watch a pass stop part-way with a handful of rows. It returns the
// restore.
func SetAccessRecordBatchForTest(n int) func() {
	old := accessRecordBatch
	accessRecordBatch = n
	return func() { accessRecordBatch = old }
}

// StepAccessRecordClockForTest makes the access-record sweep's budget clock
// advance one second each time it is read, so a pass's budget is a number of
// statements. It returns the restore.
func StepAccessRecordClockForTest() func() {
	old := accessRecordClock
	t := time.Now()
	accessRecordClock = func() time.Time {
		t = t.Add(time.Second)
		return t
	}
	return func() { accessRecordClock = old }
}

// SetRequestPolicyTTLForTest shortens the request-policy cache's lifetime and
// returns the restore.
func SetRequestPolicyTTLForTest(d time.Duration) func() {
	was := requestPolicyTTL
	requestPolicyTTL = d
	return func() { requestPolicyTTL = was }
}

// MigrateUpToForTest applies the migrations up to version and no further, so
// a test can write rows a later migration has to carry.
func MigrateUpToForTest(ctx context.Context, conn *sql.DB, version int64) error {
	p, err := newProvider(conn)
	if err != nil {
		return err
	}
	_, err = p.UpTo(ctx, version)
	return err
}
