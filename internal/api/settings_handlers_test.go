package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/config"
	"toft/internal/database"
)

func putSettingsAs(t *testing.T, srv *Server, router http.Handler, who string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/system/settings", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// What an install is capable of is infrastructure detail; a non-admin gets 404,
// not 403.
func TestSettingsRoutes_NonAdmin_Return404(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "viewer", database.SystemRoleViewer)

	if rec := getAs(t, srv, router, "viewer", "/api/v1/system/settings"); rec.Code != http.StatusNotFound {
		t.Errorf("GET as viewer = %d, want 404", rec.Code)
	}
	if rec := putSettingsAs(t, srv, router, "viewer", map[string]string{"environment_tag": "stage"}); rec.Code != http.StatusNotFound {
		t.Errorf("PUT as viewer = %d, want 404", rec.Code)
	}
}

// The posture Admin → Info reads. Derived from configuration on every request
// rather than stored, so the page cannot describe an install differently from
// the startup warning — and the wire values are asserted here because the SPA
// switches on exactly these three.
func TestSettingsReportsWhichAccountAppsRunAs(t *testing.T) {
	cases := []struct {
		name      string
		runAsUser string
		perApp    bool
		want      string
	}{
		{"nothing configured", "", false, "platform"},
		{"one shared account", "toft-apps", false, "shared"},
		{"an account per app", "", true, "per-app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			router := srv.Router(http.NotFoundHandler())
			seedUser(t, srv.db, "root", database.SystemRoleAdmin)
			srv.cfg.AppRunAsUser, srv.cfg.AppUserPerApp = tc.runAsUser, tc.perApp

			rec := getAs(t, srv, router, "root", "/api/v1/system/settings")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
			}
			var got settingsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.AppAccounts != tc.want {
				t.Errorf("app_accounts = %q, want %q", got.AppAccounts, tc.want)
			}
		})
	}
}

// Both addresses an install is reached at come from its configuration, and the
// Settings page shows each beside the field that does or does not move it:
// BASE_URL under the override, APPS_BASE_URL as the origin apps' own links
// start from. A Terraform variable, a chart value or a compose .env line is
// what an administrator changes; this is where the change shows.
func TestSettingsReportBothConfiguredAddresses(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	srv.cfg.BaseURL, srv.cfg.AppsBaseURL = "https://trytoft.example/", "https://applications.trytoft.example/"

	rec := getAs(t, srv, router, "root", "/api/v1/system/settings")
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%d %s)", err, rec.Code, rec.Body.String())
	}
	if got.EnvBaseURL != "https://trytoft.example" || got.AppsBaseURL != "https://applications.trytoft.example" {
		t.Errorf("env_base_url = %q, apps_base_url = %q", got.EnvBaseURL, got.AppsBaseURL)
	}
}

func TestUpdateSettings_EnvironmentTag(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	// A valid tag persists and is echoed back by GET.
	if rec := putSettingsAs(t, srv, router, "root", map[string]string{"environment_tag": "stage"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT valid tag = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := getAs(t, srv, router, "root", "/api/v1/system/settings")
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.EnvironmentTag != "stage" {
		t.Fatalf("environment_tag = %q, want %q", got.EnvironmentTag, "stage")
	}

	// The tag names a file, so anything unsafe as a filename component is
	// refused — with an explanation, not a bare "invalid".
	for _, bad := range []string{"Prod", "a.b", "-x", "x-", strings.Repeat("a", 33)} {
		rec := putSettingsAs(t, srv, router, "root", map[string]string{"environment_tag": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %q = %d, want 400", bad, rec.Code)
		}
	}

	// Empty turns the feature off.
	if rec := putSettingsAs(t, srv, router, "root", map[string]string{"environment_tag": ""}); rec.Code != http.StatusOK {
		t.Fatalf("PUT empty tag = %d body=%s", rec.Code, rec.Body.String())
	}
	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.EnvironmentTag != "" {
		t.Fatalf("environment_tag after clear = %q, want empty", settings.EnvironmentTag)
	}
}

func TestUpdateSettings_IdleSleepMinutes(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	// Off until an administrator says otherwise: an upgrade must not start
	// stopping people's apps because a new setting appeared.
	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.IdleSleepMinutes != 0 {
		t.Fatalf("default idle_sleep_minutes = %d, want 0", settings.IdleSleepMinutes)
	}

	if rec := putSettingsAs(t, srv, router, "root", map[string]int{"idle_sleep_minutes": 30}); rec.Code != http.StatusOK {
		t.Fatalf("PUT 30 = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := getAs(t, srv, router, "root", "/api/v1/system/settings")
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.IdleSleepMinutes != 30 {
		t.Fatalf("idle_sleep_minutes = %d, want 30", got.IdleSleepMinutes)
	}

	// A timeout shorter than a slow framework's own startup would thrash the
	// app rather than save anything, so the floor is refused outright.
	for _, bad := range []int{1, 4, -5, maxIdleSleepMinutes + 1} {
		rec := putSettingsAs(t, srv, router, "root", map[string]int{"idle_sleep_minutes": bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %d = %d, want 400", bad, rec.Code)
		}
	}

	// 0 is the way to turn it off, and is not subject to the floor.
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{"idle_sleep_minutes": 0}); rec.Code != http.StatusOK {
		t.Fatalf("PUT 0 = %d body=%s", rec.Code, rec.Body.String())
	}
	settings, err = srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.IdleSleepMinutes != 0 {
		t.Fatalf("idle_sleep_minutes after clear = %d, want 0", settings.IdleSleepMinutes)
	}

	// An unsent field means "leave alone" — the PUT is a partial patch, and the
	// settings form sends one field at a time.
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{"idle_sleep_minutes": 60}); rec.Code != http.StatusOK {
		t.Fatalf("PUT 60 = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := putSettingsAs(t, srv, router, "root", map[string]string{"environment_tag": "stage"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT unrelated field = %d body=%s", rec.Code, rec.Body.String())
	}
	settings, err = srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.IdleSleepMinutes != 60 {
		t.Fatalf("idle_sleep_minutes = %d after an unrelated update, want 60", settings.IdleSleepMinutes)
	}
}

// The isolated timeout is a setting of its own: off by default, the same
// bounds as the shared one, and neither write touches the other.
func TestUpdateSettings_IsolatedIdleSleepMinutes(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.IsolatedIdleSleepMinutes != 0 {
		t.Fatalf("default isolated_idle_sleep_minutes = %d, want 0", settings.IsolatedIdleSleepMinutes)
	}

	if rec := putSettingsAs(t, srv, router, "root", map[string]int{"isolated_idle_sleep_minutes": 120}); rec.Code != http.StatusOK {
		t.Fatalf("PUT 120 = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := getAs(t, srv, router, "root", "/api/v1/system/settings")
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.IsolatedIdleSleepMinutes != 120 || got.IdleSleepMinutes != 0 {
		t.Fatalf("isolated = %d, shared = %d; want 120 and 0 — one setting moved the other",
			got.IsolatedIdleSleepMinutes, got.IdleSleepMinutes)
	}

	for _, bad := range []int{1, 4, -5, maxIdleSleepMinutes + 1} {
		if rec := putSettingsAs(t, srv, router, "root", map[string]int{"isolated_idle_sleep_minutes": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %d = %d, want 400", bad, rec.Code)
		}
	}

	if rec := putSettingsAs(t, srv, router, "root", map[string]int{"idle_sleep_minutes": 30}); rec.Code != http.StatusOK {
		t.Fatalf("PUT shared 30 = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{"isolated_idle_sleep_minutes": 0}); rec.Code != http.StatusOK {
		t.Fatalf("PUT isolated 0 = %d body=%s", rec.Code, rec.Body.String())
	}
	settings, err = srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.IsolatedIdleSleepMinutes != 0 || settings.IdleSleepMinutes != 30 {
		t.Fatalf("isolated = %d, shared = %d; want 0 and 30",
			settings.IsolatedIdleSleepMinutes, settings.IdleSleepMinutes)
	}
}

// The window is off by default with a night already filled in, refuses a time
// that is not HH:MM, and refuses a window whose ends meet.
func TestUpdateSettings_IsolatedSleepWindow(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.IsolatedSleepWindowEnabled || settings.IsolatedSleepWindowStart != "19:00" ||
		settings.IsolatedSleepWindowEnd != "07:00" {
		t.Fatalf("defaults = %v %q–%q, want off 19:00–07:00", settings.IsolatedSleepWindowEnabled,
			settings.IsolatedSleepWindowStart, settings.IsolatedSleepWindowEnd)
	}

	ok := map[string]any{"isolated_sleep_window_enabled": true,
		"isolated_sleep_window_start": "22:30", "isolated_sleep_window_end": "06:00"}
	if rec := putSettingsAs(t, srv, router, "root", ok); rec.Code != http.StatusOK {
		t.Fatalf("PUT window = %d body=%s", rec.Code, rec.Body.String())
	}
	for _, bad := range []map[string]any{
		{"isolated_sleep_window_start": "7pm"},
		{"isolated_sleep_window_end": "24:00"},
		{"isolated_sleep_window_end": "22:30"}, // meets the stored start
	} {
		if rec := putSettingsAs(t, srv, router, "root", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %v = %d, want 400", bad, rec.Code)
		}
	}
	settings, err = srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.IsolatedSleepWindowEnabled || settings.IsolatedSleepWindowStart != "22:30" ||
		settings.IsolatedSleepWindowEnd != "06:00" {
		t.Fatalf("stored = %v %q–%q, want on 22:30–06:00", settings.IsolatedSleepWindowEnabled,
			settings.IsolatedSleepWindowStart, settings.IsolatedSleepWindowEnd)
	}
}

// --- The retention and automation caps ---

// Every bound refuses out of range and says what the range is. A cap an
// administrator meets only as a refusal was the problem these settings solve;
// a refusal that does not name the range would be the same problem again.
func TestUpdateSettings_RetentionBounds(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	cases := []struct {
		field string
		bad   int
		good  int
		want  string
	}{
		{"job_log_retention_days", 0, 7, "between 1 and 365"},
		{"job_run_retention_days", 0, 120, "between 1 and 3650"},
		// A week is the floor, so a day is refused here where it is accepted
		// for a job's logs — the table this sweeps is what answers "did it fail
		// over the weekend".
		{"app_event_retention_days", 1, 180, "between 7 and 3650"},
		{"app_event_retention_days", 9999, 180, "between 7 and 3650"},
	}
	for _, c := range cases {
		rec := putSettingsAs(t, srv, router, "root", map[string]int{c.field: c.bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s=%d: status %d, want 400: %s", c.field, c.bad, rec.Code, rec.Body)
			continue
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s=%d: %s, want a message naming the range %q", c.field, c.bad, rec.Body, c.want)
		}

		if rec := putSettingsAs(t, srv, router, "root", map[string]int{c.field: c.good}); rec.Code != http.StatusOK {
			t.Errorf("%s=%d: status %d, want 200: %s", c.field, c.good, rec.Code, rec.Body)
		}
	}

	// The whole set survives the round trip, rather than the last write winning.
	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if settings.JobLogRetentionDays != 7 || settings.JobRunRetentionDays != 120 ||
		settings.AppEventRetentionDays != 180 {
		t.Errorf("stored = %+v, want the three values saved above", settings)
	}
	// The sweep reads a duration rather than the raw figure, and reading the
	// wrong unit is the failure this catches: 180 hours would prune two thirds
	// of the events an administrator asked to keep.
	if got := settings.AppEventRetention(); got != 180*24*time.Hour {
		t.Errorf("AppEventRetention() = %v, want 4320h", got)
	}
}

// Rows outlive logs. Both figures are individually valid, so this is refused
// rather than clamped: clamping would move a number the administrator typed.
func TestUpdateSettings_JobRetentionInversionIsRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "root", map[string]int{
		"job_log_retention_days": 30, "job_run_retention_days": 7,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "cannot be less than") {
		t.Errorf("%s, want an explanation of the ordering", rec.Body)
	}

	// Nothing was stored: a refused write is a refused write.
	settings, _ := srv.db.GetPlatformSettings()
	if settings.JobLogRetentionDays != 14 || settings.JobRunRetentionDays != 90 {
		t.Errorf("settings = %+v, want the defaults untouched", settings)
	}

	// The same pair the right way round is accepted.
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{
		"job_log_retention_days": 7, "job_run_retention_days": 30,
	}); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}

	// And raising the log figure past a stored row figure is refused too — the
	// invariant is about the pair, not about one request's two fields.
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{
		"job_log_retention_days": 200,
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 when only one half is sent: %s", rec.Code, rec.Body)
	}
}

// Every change lands in audit_log, because "who shortened retention, and when"
// is exactly what that table is for.
func TestUpdateSettings_RetentionCapsAreAudited(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	// Both halves of the pair, because rows outlive logs: dropping run history
	// to five days while the log clock still says fourteen is the inversion the
	// handler refuses.
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{
		"job_run_retention_days": 5, "job_log_retention_days": 3,
	}); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	var before, after string
	err := srv.db.Conn().QueryRow(
		`SELECT before, after FROM audit_log WHERE action = 'platform_settings.update'
		 ORDER BY occurred_at DESC LIMIT 1`).Scan(&before, &after)
	if err != nil {
		t.Fatalf("no platform_settings.update entry was written: %v", err)
	}
	// Parsed rather than substring-matched: audit_log.before/after are JSONB, so
	// Postgres owns the key order and the spacing and a literal match would be
	// asserting the formatter.
	var newVals, oldVals map[string]any
	if err := json.Unmarshal([]byte(after), &newVals); err != nil {
		t.Fatalf("audit after is not JSON: %v (%s)", err, after)
	}
	if err := json.Unmarshal([]byte(before), &oldVals); err != nil {
		t.Fatalf("audit before is not JSON: %v (%s)", err, before)
	}
	if newVals["job_run_retention_days"] != float64(5) {
		t.Errorf("audit entry %s does not carry the new value", after)
	}
	if oldVals["job_run_retention_days"] != float64(90) {
		t.Errorf("audit entry %s does not carry the old value", before)
	}
}

// A corrupt or out-of-range stored row reads back as the default rather than as
// itself. An app allowed minus five runs at once is the failure this clamp
// stands in front of.
func TestPlatformSettings_ClampACorruptRow(t *testing.T) {
	srv := newTestServer(t)
	for key, value := range map[string]string{
		database.SettingJobConcurrency:      "-5",
		database.SettingJobLogRetentionDays: "not a number",
	} {
		if _, err := srv.db.PutSetting(key, value, database.BuiltinAdminID); err != nil {
			t.Fatalf("writing %s: %v", key, err)
		}
	}

	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	d := database.DefaultPlatformSettings()
	if settings.JobConcurrency != 1 {
		t.Errorf("concurrency = %d, want it raised to 1", settings.JobConcurrency)
	}
	if settings.JobLogRetentionDays != d.JobLogRetentionDays {
		t.Errorf("log retention = %d, want the default %d", settings.JobLogRetentionDays, d.JobLogRetentionDays)
	}
}

// An install that *names* a backend it does not *have* must not advertise the
// capability.
//
// `ECS_CLUSTER` is the one configuration where the two answers diverge: the ECS
// backend is not implemented (openIsolatedBackend warns and returns nil; ADR §26,
// docs/work-remaining.md §2), so config says isolated is possible and no
// runtime exists to place anything into. Asking the configuration produced an
// install that offered jobs and isolated mode on every screen and refused both
// at the moment somebody used them.
//
// internal/jobs already derives this from `s.backend != nil` and fails closed,
// which is why the failure was a confusing refusal rather than a lost run. This
// pins the API layer to the same answer.
func TestANamedButUnimplementedBackendIsNotACapability(t *testing.T) {
	srv := newTestServer(t)
	// What an ECS install looks like: the cluster is configured, and
	// newTestServer's runtimes hold no isolated backend because none exists.
	srv.cfg.ECSCluster = "arn:aws:ecs:us-east-1:1234:cluster/toft"
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	// The topology is still reported honestly — an operator should not be able
	// to mislabel their install, and this is not the lie being fixed.
	if got := srv.topology(); got != "ecs" {
		t.Errorf("topology = %q, want ecs", got)
	}

	caps := srv.capabilities()
	if caps.IsolatedPossible {
		t.Error("isolated mode was offered with no backend to place a workload into")
	}
	if caps.BackgroundJobsAvailable() {
		t.Error("background jobs were offered with no backend to run them on")
	}

	// And an administrator cannot switch on what the install cannot do: a
	// stored "enabled" with no implementation behind it is a lie the UI would
	// have to explain forever after.
	rec := putSettingsAs(t, srv, router, "root", map[string]any{"isolated_execution_enabled": true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("enabling isolated mode: status = %d, want 422", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "no isolated backend") {
		t.Errorf("the refusal should name what is missing, got %q", body.Error)
	}
}

// The job limits: each bounded, and the default may not sit above the ceiling
// — a job that declares nothing would be refused for it.
func TestUpdateSettings_JobLimits(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	if rec := putSettingsAs(t, srv, router, "root", map[string]int{
		"max_job_timeout_seconds": 900, "default_job_timeout_seconds": 600,
		"job_memory_limit_mb": 2048, "max_queued_runs_per_app": 10,
	}); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	settings, _ := srv.db.GetPlatformSettings()
	if settings.MaxJobTimeoutSecs != 900 || settings.DefaultJobTimeoutSecs != 600 ||
		settings.JobMemoryLimitMB != 2048 || settings.MaxQueuedRunsPerApp != 10 {
		t.Errorf("stored = %+v", settings)
	}

	rec := putSettingsAs(t, srv, router, "root", map[string]int{"default_job_timeout_seconds": 1200})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "max_job_timeout_seconds") {
		t.Errorf("a default above the ceiling: %d %s, want 400 naming the ceiling", rec.Code, rec.Body)
	}
	rec = putSettingsAs(t, srv, router, "root", map[string]int{"max_queued_runs_per_app": 0})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a zero queue cap: %d, want 400", rec.Code)
	}
}

// Jobs are on by default: they are the one automation concept, and an install
// with them off could not schedule anything.
func TestJobsAreOnByDefault(t *testing.T) {
	if !database.DefaultPlatformSettings().JobsEnabled {
		t.Error("background_jobs_enabled defaults off")
	}
}

// The session policy: two numbers, one of which has a meaningful zero and one
// of which does not.
func TestUpdateSettings_SessionPolicy(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	// Out of range on either is refused with the message, not clamped in
	// silence: the form mirrors these bounds, so a request that reaches here
	// out of range is a script and deserves to be told why.
	for _, body := range []map[string]int{
		{"session_absolute_hours": 0},
		{"session_absolute_hours": 25},
		{"session_idle_minutes": 1},
		{"session_idle_minutes": 10000},
	} {
		if rec := putSettingsAs(t, srv, router, "root", body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %v = %d, want 400 (body %s)", body, rec.Code, rec.Body.String())
		}
	}

	// 0 on the idle timeout is the escape hatch and must be accepted.
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{
		"session_absolute_hours": 12, "session_idle_minutes": 0,
	}); rec.Code != http.StatusOK {
		t.Fatalf("PUT the policy = %d body=%s", rec.Code, rec.Body.String())
	}

	rec := getAs(t, srv, router, "root", "/api/v1/system/settings")
	var got settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionAbsoluteHours != 12 || got.SessionIdleMinutes != 0 {
		t.Fatalf("policy = %dh/%dm, want 12h/0m", got.SessionAbsoluteHours, got.SessionIdleMinutes)
	}
	// And the platform is now enforcing what it was told, with no restart.
	if p := srv.sessionPolicy(); p.Absolute != 12*time.Hour || p.Idle != 0 {
		t.Fatalf("enforced policy = %v/%v, want 12h/0", p.Absolute, p.Idle)
	}
}

// Shortening the absolute lifetime ends sessions that are already open,
// including the administrator's own — so it says so rather than letting them
// discover it.
func TestUpdateSettings_ShorteningTheLifetimeWarns(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "root", map[string]int{"session_absolute_hours": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Warning, "already open") {
		t.Errorf("warning = %q, want it to say outstanding sessions are held to this too", body.Warning)
	}

	// Lengthening is not a warning: nothing is signed out by it.
	rec = putSettingsAs(t, srv, router, "root", map[string]int{"session_absolute_hours": 12})
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Contains(body.Warning, "already open") {
		t.Errorf("lengthening the lifetime warned about signing people out: %q", body.Warning)
	}
}

// --- The memory ceiling's unit ---

// The wire carries gigabytes and the row carries mebibytes, and the round trip
// is lossless at every value anybody types.
//
// The echo is the half that matters: a field that accepted 0.333 and then went
// on showing 0.333 would be claiming a precision the install does not hold, and
// the number actually in force would be 341 MB.
func TestTheMemoryCeilingRoundTripsThroughGigabytes(t *testing.T) {
	cases := []struct {
		name     string
		sent     float64
		wantMB   int
		wantEcho float64
	}{
		{"half a gigabyte is the 512 the install has always had", 0.5, 512, 0.5},
		{"a whole one", 1, 1024, 1},
		{"a fraction nobody can store exactly is rounded and said so", 0.333, 341, 341.0 / 1024},
		{"zero is the operator's number, never uncapped", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			router := srv.Router(http.NotFoundHandler())
			seedUser(t, srv.db, "root", database.SystemRoleAdmin)

			rec := putSettingsAs(t, srv, router, "root",
				map[string]any{"app_memory_limit_gb": tc.sent})
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
			}
			var wrote struct {
				Settings struct {
					AppMemoryLimitGB float64 `json:"app_memory_limit_gb"`
				} `json:"settings"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wrote); err != nil {
				t.Fatal(err)
			}
			if got := wrote.Settings.AppMemoryLimitGB; got != tc.wantEcho {
				t.Errorf("the write echoed %v GB, want %v", got, tc.wantEcho)
			}

			// What is stored is what a workload will be started with.
			s, err := srv.db.GetPlatformSettings()
			if err != nil {
				t.Fatal(err)
			}
			if s.AppMemoryLimitMB != tc.wantMB {
				t.Errorf("stored %d MB, want %d", s.AppMemoryLimitMB, tc.wantMB)
			}

			// And the read agrees with the write.
			read := getAs(t, srv, router, "root", "/api/v1/system/settings")
			var got struct {
				AppMemoryLimitGB float64 `json:"app_memory_limit_gb"`
			}
			if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.AppMemoryLimitGB != tc.wantEcho {
				t.Errorf("GET reported %v GB, want %v", got.AppMemoryLimitGB, tc.wantEcho)
			}
		})
	}
}

// The floor, checked before the rounding: 0.0624 GB rounds to exactly 64 MB,
// and accepting it would make the refusal's own number wrong.
func TestACeilingBelowTheFloorIsRefusedInBothUnits(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "root", map[string]any{"app_memory_limit_gb": 0.0624})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT 0.0624 GB = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"0.0625 GB", "64 MB", "interpreter"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the refusal should say %q — the person reading it typed gigabytes and the "+
				"reason is about megabytes; got %s", want, rec.Body.String())
		}
	}

	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_memory_limit_gb": 0.0625}); rec.Code != http.StatusOK {
		t.Errorf("PUT 0.0625 GB = %d, want 200 — the floor itself is allowed: %s", rec.Code, rec.Body.String())
	}
}

// The environment's figure travels beside the stored one, because a field whose
// zero defers to a number the page cannot show is a field nobody can reason
// about.
func TestTheSettingsDocumentCarriesTheEnvironmentsCeiling(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.AppMemoryLimitMB = 2048
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	var got struct {
		EnvAppMemoryLimitGB float64 `json:"env_app_memory_limit_gb"`
	}
	rec := getAs(t, srv, router, "root", "/api/v1/system/settings")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.EnvAppMemoryLimitGB != 2 {
		t.Errorf("env ceiling = %v GB, want 2", got.EnvAppMemoryLimitGB)
	}
}

// Megabytes is the right unit for a 256 MB cap and gigabytes for a 16 GB one,
// so the document carries both and a write may name either. What it may not do
// is name both: that is one number written twice, and the platform will not
// pick.
func TestTheCeilingMayBeWrittenInEitherUnitButNotBoth(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "root", map[string]any{"app_memory_limit_mb": 256})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 256 MB = %d: %s", rec.Code, rec.Body.String())
	}

	// Both renderings come back, and they are the same number.
	var got struct {
		AppMemoryLimitMB int     `json:"app_memory_limit_mb"`
		AppMemoryLimitGB float64 `json:"app_memory_limit_gb"`
	}
	read := getAs(t, srv, router, "root", "/api/v1/system/settings")
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AppMemoryLimitMB != 256 || got.AppMemoryLimitGB != 0.25 {
		t.Errorf("read back %d MB / %v GB, want 256 and 0.25 — two renderings of one row",
			got.AppMemoryLimitMB, got.AppMemoryLimitGB)
	}

	both := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_memory_limit_mb": 512, "app_memory_limit_gb": 1})
	if both.Code != http.StatusBadRequest {
		t.Fatalf("PUT with both units = %d, want 400: %s", both.Code, both.Body.String())
	}

	// And the refused write changed nothing.
	s, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.AppMemoryLimitMB != 256 {
		t.Errorf("a refused write stored %d MB", s.AppMemoryLimitMB)
	}
}

// --- One app's own ceiling ---

// The install's ceiling is arithmetic about a machine; an app's is about one
// piece of work. An administrator may raise one app's without raising the box's,
// and the write is audited.
func TestAnAdministratorMaySetOneAppsMemoryCeiling(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := seedAppOwnedBy(t, srv, "reports", "owner")

	rec := patchAs(t, srv, router, "root", "/api/v1/apps/"+app.ID,
		jsonBody(t, map[string]any{"memory_limit_gb": 4}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH as admin = %d: %s", rec.Code, rec.Body.String())
	}

	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 4096 {
		t.Errorf("stored %d MB, want 4096 — 4 GB is what both backends count in", got.MemoryLimitMB)
	}

	// The install's dial is untouched: raising one app's ceiling must not raise
	// the estate's.
	s, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.AppMemoryLimitMB != 0 {
		t.Errorf("the install's dial moved to %d MB", s.AppMemoryLimitMB)
	}

	// And a decision the platform made about somebody else's app left a record.
	var n int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE target_id = ? AND action = 'app.memory_limit.change'`,
		app.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the change wrote %d audit rows, want 1 — a platform decision about "+
			"somebody else's app that leaves no record is what the audit log exists to prevent", n)
	}
}

// A ceiling an app can raise for itself is not a ceiling. The owner may read it
// and may not write it.
func TestAnOwnerMayNotRaiseTheirOwnAppsCeiling(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := seedAppOwnedBy(t, srv, "reports", "owner")

	rec := patchAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID,
		jsonBody(t, map[string]any{"memory_limit_mb": 4096}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PATCH as owner = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 0 {
		t.Errorf("a refused write stored %d MB", got.MemoryLimitMB)
	}
}

// Zero on the app hands the number back to the install's dial rather than
// uncapping the workload — the same rule the settings row follows, and the one
// that makes an unset value safe.
func TestAnAppsZeroCeilingMeansTheInstallsNumber(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	app := seedAppOwnedBy(t, srv, "reports", "root")

	if rec := patchAs(t, srv, router, "root", "/api/v1/apps/"+app.ID,
		jsonBody(t, map[string]any{"memory_limit_mb": 2048})); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := patchAs(t, srv, router, "root", "/api/v1/apps/"+app.ID,
		jsonBody(t, map[string]any{"memory_limit_mb": 0})); rec.Code != http.StatusOK {
		t.Fatalf("clearing = %d: %s", rec.Code, rec.Body.String())
	}

	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB != 0 {
		t.Errorf("stored %d MB, want 0", got.MemoryLimitMB)
	}

	// And the resolver reads the install's number for it.
	env := config.Envelope{AppMemoryLimitMB: 512}
	if n := env.ResourcesForApp(got.MemoryLimitMB, 0).MemoryMB.Value; n != 512 {
		t.Errorf("an app with no ceiling resolved to %d, want the install's 512", n)
	}
}

// Below the floor is refused for an app exactly as it is for the install, and a
// write naming both units is refused rather than resolved by precedence.
func TestAnAppsCeilingIsBoundedAndSingleUnit(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	app := seedAppOwnedBy(t, srv, "reports", "root")

	if rec := patchAs(t, srv, router, "root", "/api/v1/apps/"+app.ID,
		jsonBody(t, map[string]any{"memory_limit_mb": 8})); rec.Code != http.StatusBadRequest {
		t.Errorf("8 MB = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := patchAs(t, srv, router, "root", "/api/v1/apps/"+app.ID,
		jsonBody(t, map[string]any{"memory_limit_mb": 512, "memory_limit_gb": 1})); rec.Code != http.StatusBadRequest {
		t.Errorf("both units = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// seedAppOwnedBy is one app with a named owner, for the cases about what may be
// decided *about* an app rather than by it.
func seedAppOwnedBy(t *testing.T, srv *Server, slug, owner string) *database.App {
	t.Helper()
	app := &database.App{
		ID: "app-" + slug, Slug: slug, Name: slug, OwnerID: owner,
		Framework: "python",
		Status:    database.AppStatusStopped, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return app
}

// jsonBody is patchAs's string body, written the way every other case here
// writes a payload.
func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- The rest of the dials ---

// A written level reaches this process's logger with no restart, and the level
// this process is *saying* is reported back — which is the half that fails
// quietly, because the write's own effect is visible immediately and the
// reported figure is only checked by whoever is confused later.
func TestTheLogLevelTakesEffectWithoutARestart(t *testing.T) {
	srv := newTestServer(t)
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	srv.logLevel = lv
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "root", map[string]any{"log_level": "debug"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if lv.Level() != slog.LevelDebug {
		t.Errorf("the process is still at %v — the level is the one dial that may take effect "+
			"now, because it reaches no workload at all", lv.Level())
	}

	var got struct {
		EffectiveLogLevel string `json:"effective_log_level"`
		EnvLogLevel       string `json:"env_log_level"`
		LogFormat         string `json:"log_format"`
	}
	read := getAs(t, srv, router, "root", "/api/v1/system/settings")
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.EffectiveLogLevel != "debug" {
		t.Errorf("effective level = %q, want debug", got.EffectiveLogLevel)
	}
	// The format is the half that stays in the environment, and the page has to
	// say which one is in force or an administrator is left wondering where it
	// went.
	if got.LogFormat == "" {
		t.Error("the document does not name the log format, which is configuration and not a dial")
	}

	// Cleared: back to the environment's level, not to nothing.
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"log_level": ""}); rec.Code != http.StatusOK {
		t.Fatalf("clearing = %d: %s", rec.Code, rec.Body.String())
	}
	if lv.Level() != srv.cfg.LogLevel {
		t.Errorf("cleared level = %v, want the environment's %v", lv.Level(), srv.cfg.LogLevel)
	}

	// And a word nobody can parse is refused rather than quietly ignored.
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"log_level": "verbose"}); rec.Code != http.StatusBadRequest {
		t.Errorf("log_level=verbose = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// Build concurrency cannot exceed the pool this process's workers live in, and
// the refusal names the number rather than clamping silently — a field that
// accepts 20 and behaves like 4 is a field that lies.
func TestBuildConcurrencyIsBoundedByThePoolAndSaysSo(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	var got struct {
		Ceiling int `json:"max_concurrent_builds_ceiling"`
	}
	read := getAs(t, srv, router, "root", "/api/v1/system/settings")
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Ceiling < 1 {
		t.Fatalf("the document does not name the pool the dial cannot exceed: %d", got.Ceiling)
	}

	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"max_concurrent_builds": got.Ceiling}); rec.Code != http.StatusOK {
		t.Errorf("the ceiling itself = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"max_concurrent_builds": got.Ceiling + 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("above the ceiling = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "MAX_CONCURRENT_BUILDS") {
		t.Errorf("the refusal should name the variable that sets the pool: %s", rec.Body.String())
	}
}

// A CPU limit below the request is refused for the stored pair exactly as it is
// for the environment's: the cluster would refuse every pod, and finding that
// out at the next deploy is the failure config.Load already prevents.
func TestAStoredCPULimitBelowTheRequestIsRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_cpu_request_cores": 1}); rec.Code != http.StatusOK {
		t.Fatalf("request = %d: %s", rec.Code, rec.Body.String())
	}
	rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_cpu_limit_millicores": 500})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit below request = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// And the request round-trips in both units.
	var got struct {
		Millicores int     `json:"app_cpu_request_millicores"`
		Cores      float64 `json:"app_cpu_request_cores"`
	}
	read := getAs(t, srv, router, "root", "/api/v1/system/settings")
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Millicores != 1000 || got.Cores != 1 {
		t.Errorf("read back %dm / %v cores, want 1000 and 1", got.Millicores, got.Cores)
	}
}

// Turning auto-deploy polling off stays where it has always been. A stored zero
// means the operator's number, like every other dial, and the document says
// whether a poller exists at all so the field can render disabled rather than
// silently doing nothing.
func TestTheStoredPollZeroIsTheOperatorsNumber(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"auto_deploy_poll_seconds": 0}); rec.Code != http.StatusOK {
		t.Fatalf("zero = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"auto_deploy_poll_seconds": 1}); rec.Code != http.StatusBadRequest {
		t.Errorf("one second = %d, want 400 — a poller that asks a git host every second is a "+
			"self-inflicted rate limit", rec.Code)
	}

	var got struct {
		Running bool `json:"auto_deploy_poller_running"`
	}
	read := getAs(t, srv, router, "root", "/api/v1/system/settings")
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Running != (srv.cfg.AutoDeployPollSeconds > 0) {
		t.Errorf("poller running = %v, want %v — storing a number must not imply a poller exists",
			got.Running, srv.cfg.AutoDeployPollSeconds > 0)
	}
}

// The echo after a write carries the CPU dials it just stored. settingsWritten
// declared the four fields precisely to round-trip them and assigned only the
// memory pair, so the SPA's cache saw the dial flash to zero before its
// refetch corrected it.
func TestTheSettingsEchoCarriesTheCPUDials(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_cpu_request_millicores": 250, "app_cpu_limit_cores": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("write = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Settings struct {
			RequestMillicores int     `json:"app_cpu_request_millicores"`
			RequestCores      float64 `json:"app_cpu_request_cores"`
			LimitMillicores   int     `json:"app_cpu_limit_millicores"`
			LimitCores        float64 `json:"app_cpu_limit_cores"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Settings.RequestMillicores != 250 || out.Settings.RequestCores != 0.25 ||
		out.Settings.LimitMillicores != 2000 || out.Settings.LimitCores != 2 {
		t.Errorf("echo = %+v, want 250m/0.25 and 2000m/2", out.Settings)
	}
}

// A write from a page that read an older document is refused. Two
// administrators saving from two pages loaded a moment apart used to have the
// second overwrite the first's change with the value the second page still
// held, and the second's audit row recorded a change they never made.
func TestASaveFromAStalePageIsRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	read := getAs(t, srv, router, "root", "/api/v1/system/settings")
	var doc struct {
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}

	// Somebody else saves first.
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"max_concurrent_builds": 2}); rec.Code != http.StatusOK {
		t.Fatalf("first save = %d: %s", rec.Code, rec.Body.String())
	}

	// The stale page's save carries the updated_at it read.
	rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"max_concurrent_builds": 3, "updated_at": doc.UpdatedAt})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "settings_changed") {
		t.Fatalf("stale save = %d %s, want 409 settings_changed", rec.Code, rec.Body.String())
	}

	// Reloaded, the same save goes through, and the echo carries the new
	// updated_at for the next one.
	read = getAs(t, srv, router, "root", "/api/v1/system/settings")
	if err := json.Unmarshal(read.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	rec = putSettingsAs(t, srv, router, "root",
		map[string]any{"max_concurrent_builds": 3, "updated_at": doc.UpdatedAt})
	if rec.Code != http.StatusOK {
		t.Fatalf("save after reload = %d: %s", rec.Code, rec.Body.String())
	}
	var echo struct {
		Settings struct {
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &echo); err != nil {
		t.Fatal(err)
	}
	if !echo.Settings.UpdatedAt.After(doc.UpdatedAt) {
		t.Errorf("the echo's updated_at (%v) did not move past the one read (%v)", echo.Settings.UpdatedAt, doc.UpdatedAt)
	}
}

// API-SET-1: a save with no updated_at was unconditional, and the document it
// writes back whole was read at the top of the handler — a value saved on its
// own page in between was put back to the old one. The handler's own read is
// now the precondition: the save is refused, the value survives.
func TestASaveWithoutUpdatedAtNeverRevertsAConcurrentWrite(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	afterSettingsReadForTest = func() {
		time.Sleep(2 * time.Millisecond) // past the millisecond the read saw
		if _, err := srv.db.PutSetting(database.SettingSecurityTxtLanguages, "en, fr", ""); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	rec := putSettingsAs(t, srv, router, "root", map[string]any{"idle_sleep_minutes": 30})
	afterSettingsReadForTest = nil
	if rec.Code != http.StatusConflict {
		t.Fatalf("save across a concurrent write = %d %s, want 409", rec.Code, rec.Body.String())
	}
	got, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.SecurityTxtLanguages != "en, fr" {
		t.Fatalf("security.txt languages = %q: the concurrent write was reverted", got.SecurityTxtLanguages)
	}

	// With nothing in the window, the same script's save goes through.
	if rec := putSettingsAs(t, srv, router, "root", map[string]any{"idle_sleep_minutes": 30}); rec.Code != http.StatusOK {
		t.Fatalf("plain save = %d: %s", rec.Code, rec.Body.String())
	}
}

// The access record's dial: 0 keeps everything and is the default; otherwise a
// year to ten, refused below the floor rather than clamped, with the floor's
// reason in the refusal.
func TestUpdateSettings_AccessRecordRetention(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	if d := database.DefaultPlatformSettings().AccessRecordRetentionDays; d != 0 {
		t.Fatalf("default = %d, want 0 — an upgrade must delete nothing", d)
	}
	for _, n := range []int{1, 364, 3651} {
		rec := putSettingsAs(t, srv, router, "root", map[string]int{"access_record_retention_days": n})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "365") {
			t.Errorf("%d = %d %s, want 400 naming the floor", n, rec.Code, rec.Body.String())
		}
	}
	for _, n := range []int{365, 3650, 0} {
		if rec := putSettingsAs(t, srv, router, "root", map[string]int{"access_record_retention_days": n}); rec.Code != http.StatusOK {
			t.Errorf("%d = %d %s, want 200", n, rec.Code, rec.Body.String())
		}
	}
}

// A quiet threshold past the retained window is refused: there, quiet and
// unrecorded cannot be told apart.
func TestQuietAppsRefusesAThresholdPastRetention(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	if rec := putSettingsAs(t, srv, router, "root", map[string]int{"access_record_retention_days": 365}); rec.Code != http.StatusOK {
		t.Fatalf("setting retention: %d %s", rec.Code, rec.Body.String())
	}
	if rec := getAs(t, srv, router, "root", "/api/v1/system/quiet-apps?days=400"); rec.Code != http.StatusBadRequest {
		t.Errorf("days=400 on a 365-day install = %d, want 400", rec.Code)
	}
	if rec := getAs(t, srv, router, "root", "/api/v1/system/quiet-apps?days=365"); rec.Code != http.StatusOK {
		t.Errorf("days=365 = %d, want 200", rec.Code)
	}
}

// A job limit above the machine the platform reports is saved and warned
// about, never refused — on a cluster a node may be larger than this machine,
// and there the backend reports none (porting feedback 2026-11-03).
func TestUpdateSettings_JobLimitAboveTheMachineWarns(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	m, err := srv.rts.SystemMetrics()
	if err != nil || m == nil || m.MemoryTotal == 0 || m.CPUCount == 0 {
		t.Skip("this backend reports no machine")
	}
	hostMB := int(m.MemoryTotal >> 20)
	rec := putSettingsAs(t, srv, router, "root", map[string]int{
		"job_memory_limit_mb": hostMB + 1024, "job_cpu_limit_millicores": (m.CPUCount + 1) * 1000})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct{ Warning string }
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Warning, "more than this machine has") || !strings.Contains(body.Warning, "cores") {
		t.Errorf("warning = %q", body.Warning)
	}
	rec = putSettingsAs(t, srv, router, "root", map[string]int{"job_memory_limit_mb": 1024})
	json.Unmarshal(rec.Body.Bytes(), &body)
	if strings.Contains(body.Warning, "machine") {
		t.Errorf("a limit inside the machine warned: %q", body.Warning)
	}
}

// A job's default CPU limit is held to the same rule as the app's: a job pod
// pairs it with the install's request, and saved below it every run was refused
// by the cluster (code review 2026-11-11, H-5).
func TestAStoredJobCPULimitBelowTheRequestIsRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_cpu_request_millicores": 500}); rec.Code != http.StatusOK {
		t.Fatalf("request = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"job_cpu_limit_millicores": 250}); rec.Code != http.StatusBadRequest {
		t.Fatalf("job limit below request = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"job_cpu_limit_millicores": 500}); rec.Code != http.StatusOK {
		t.Fatalf("job limit at the request = %d: %s", rec.Code, rec.Body.String())
	}
}

// With no job limit stored a job pod is capped at the install's own default,
// so raising the request past that default is the same refusal (code review
// 2026-11-11, H-5, as the e2e case found it).
func TestARequestAboveTheDefaultJobCPULimitIsRefused(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.JobCPULimitMillicores = 2000
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_cpu_request_millicores": 2500}); rec.Code != http.StatusBadRequest {
		t.Fatalf("request above the default job limit = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := putSettingsAs(t, srv, router, "root",
		map[string]any{"app_cpu_request_millicores": 2500, "job_cpu_limit_millicores": 3000}); rec.Code != http.StatusOK {
		t.Fatalf("with a job limit to match = %d: %s", rec.Code, rec.Body.String())
	}
}
