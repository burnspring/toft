package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// The two cross-field rules are the only way these switches can do harm, so
// they are refused when saved rather than clamped — the same shape as half a
// telemetry pipeline being refused at startup.

func TestBlockingWithNoDeployScanIsRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy":         database.DependencyPolicyBlock,
		"dependency_scan_on_deploy": false,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	// The refusal names both halves: the gate would have nothing to evaluate
	// and every deploy would pass while the page said it was blocking.
	body := rec.Body.String()
	for _, want := range []string{"dependency_scan_on_deploy", "nothing to evaluate", "warn"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal should mention %q: %s", want, body)
		}
	}

	// Nothing was stored, either half of it.
	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if settings.DependencyPolicy != database.DependencyPolicyOff {
		t.Errorf("policy = %q, want the save refused whole", settings.DependencyPolicy)
	}
}

func TestBlockingWithBuildScriptsIsRefused(t *testing.T) {
	srv := newTestServer(t)
	// An operator who has both believes they have a control they do not have:
	// with lifecycle scripts on, `npm ci` runs arbitrary code from every
	// package *after* the gate approved it by name.
	srv.cfg.AllowBuildScripts = true
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy": database.DependencyPolicyBlock,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "BUILD_ALLOW_SCRIPTS") {
		t.Errorf("the refusal should name the variable: %s", rec.Body.String())
	}

	// `warn` is fine with scripts on — nothing is being claimed as a control.
	rec = putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy": database.DependencyPolicyWarn,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("warn should be accepted: %d %s", rec.Code, rec.Body.String())
	}
}

// An unknown value is refused by name rather than clamped: an administrator
// who typed something meant something.
func TestDependencyModeAndThresholdAreRefusedByName(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{"dependency_policy": "enforce"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "warn") {
		t.Errorf("an unknown mode should be refused naming the three: %d %s", rec.Code, rec.Body.String())
	}

	rec = putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_block_severity": "severe",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown threshold should be refused: %d", rec.Code)
	}
	// And the refusal states the rule an administrator will otherwise trip
	// over: an unscored advisory never meets any threshold.
	if !strings.Contains(rec.Body.String(), "nobody scored") {
		t.Errorf("the refusal should say what unknown severities do: %s", rec.Body.String())
	}
}

// A URL this server fetches on its own initiative is server-side input
// whoever typed it.
func TestFeedEndpointMustBeHTTPS(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_feed_endpoint": "http://osv.example.com",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "https") {
		t.Errorf("http should be refused: %d %s", rec.Code, rec.Body.String())
	}

	rec = putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_feed_endpoint": "https://user:pass@osv.example.com",
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "credentials") {
		t.Errorf("credentials in the URL should be refused: %d %s", rec.Code, rec.Body.String())
	}
}

// Going straight from off to blocking is accepted and warned about — `warn`
// exists because `block` is unadoptable without it, and the copy beside the
// switch has to say so.
func TestGoingStraightToBlockingWarns(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy": database.DependencyPolicyBlock,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !strings.Contains(out.Warning, "warn") {
		t.Errorf("warning = %q, want it to recommend warn first", out.Warning)
	}
}

// Everything under the mode is stored and read back, so an administrator's
// saved value is the one that applies.
func TestDependencySettingsRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy":              database.DependencyPolicyWarn,
		"dependency_watch_enabled":       false,
		"dependency_block_malware":       false,
		"dependency_block_severity":      "high",
		"dependency_block_only_fixable":  false,
		"dependency_feed_endpoint":       "https://mirror.example.com",
		"dependency_feed_interval_hours": 12,
		"dependency_feed_max_age_hours":  168,
		"dependency_waiver_max_days":     0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}

	got, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if got.DependencyPolicy != database.DependencyPolicyWarn ||
		got.DependencyWatchEnabled ||
		got.DependencyBlockMalware ||
		got.DependencyBlockSeverity != "high" ||
		got.DependencyBlockOnlyFixable ||
		got.DependencyFeedEndpoint != "https://mirror.example.com" ||
		got.DependencyFeedIntervalHrs != 12 ||
		got.DependencyFeedMaxAgeHours != 168 {
		t.Fatalf("settings = %+v", got)
	}
	// Zero is a real value here — waivers off — and must not be read as
	// "unset" and quietly restored to the default.
	if got.DependencyWaiverMaxDays != 0 {
		t.Errorf("waiver_max_days = %d, want 0 to mean waivers are off", got.DependencyWaiverMaxDays)
	}
}

// **The lock threshold is not the block threshold**, and the refusal has to say
// so: nothing else on this page takes a running app off the air.
func TestLockThresholdIsRefusedByNameAndHasNoModerate(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	for _, bad := range []string{"moderate", "low", "always"} {
		rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
			"dependency_lock_on": bad,
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("dependency_lock_on=%q was accepted: %d %s", bad, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "malware") {
			t.Errorf("the refusal should name the recommended value: %s", rec.Body.String())
		}
	}
}

// The warning carries §39's own argument rather than a softened version: this
// is the setting that reverses a committed decision.
func TestSwitchingTheLockOnWarnsInTheProductsOwnWords(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	// The default threshold is malware, so switching the policy on is the save
	// that arms it — and it is said there, without the select ever opening.
	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy": database.DependencyPolicyWarn,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	// The two facts an administrator cannot be allowed to discover afterwards:
	// it stops apps with nobody present, and an unlock is not the end of it.
	for _, want := range []string{"stopped", "waiver"} {
		if !strings.Contains(out.Warning, want) {
			t.Errorf("warning should mention %q: %q", want, out.Warning)
		}
	}

	// Above malware the copy has to argue against itself, because that is the
	// half of §39 whose reasoning is unchanged.
	rec = putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_lock_on": database.DependencyLockCritical,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.Contains(out.Warning, "3am") {
		t.Errorf("the vulnerability thresholds should carry the argument against them: %q", out.Warning)
	}
}

// Locking happens on a scheduled pass, so *setting* a threshold with the watch
// off is a control that reads as one and never fires. Refused, naming both
// halves — and the opposite direction is not, because taking a consequence away
// is safe. It is warned about instead, because it must not be silent.
func TestSettingALockThresholdWithTheWatchOffIsRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy":        database.DependencyPolicyWarn,
		"dependency_lock_on":       database.DependencyLockMalware,
		"dependency_watch_enabled": false,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"dependency_watch_enabled", "dependency_lock_on"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the refusal should name %q: %s", want, rec.Body.String())
		}
	}
}

func TestTurningTheWatchOffUnderALockThresholdIsAllowedAndSaidOutLoud(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	if rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_policy":  database.DependencyPolicyWarn,
		"dependency_lock_on": database.DependencyLockMalware,
	}); rec.Code != http.StatusOK {
		t.Fatalf("setting the threshold: %d %s", rec.Code, rec.Body.String())
	}

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_watch_enabled": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — this only takes a consequence away\n%s",
			rec.Code, rec.Body.String())
	}
	var out struct {
		Warning string `json:"warning"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// It must not be silent: the threshold is still set and comes back with the
	// watch, and what is already locked stays locked.
	for _, want := range []string{"malware", "stay locked"} {
		if !strings.Contains(out.Warning, want) {
			t.Errorf("warning should mention %q: %q", want, out.Warning)
		}
	}
}

// The two new dials are the watch's own, and both are refused out of range
// rather than clamped.
func TestScanIntervalAndHistoryBounds(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	for _, tc := range []struct {
		key   string
		value int
		want  string
	}{
		{"dependency_scan_interval_hours", 0, "not the feed"},
		{"dependency_scan_interval_hours", 200, "not the feed"},
		{"dependency_scan_history_kept", 0, "a count"},
		{"dependency_scan_history_kept", 400, "a count"},
	} {
		rec := putSettingsAs(t, srv, router, "admin1", map[string]any{tc.key: tc.value})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s=%d was accepted: %d", tc.key, tc.value, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.key) {
			t.Errorf("the refusal should name the setting: %s", rec.Body.String())
		}
	}

	rec := putSettingsAs(t, srv, router, "admin1", map[string]any{
		"dependency_scan_interval_hours": 24,
		"dependency_scan_history_kept":   7,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	got, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if got.DependencyScanIntervalHrs != 24 || got.DependencyScanHistoryKept != 7 {
		t.Errorf("settings = %d/%d, want 24/7",
			got.DependencyScanIntervalHrs, got.DependencyScanHistoryKept)
	}
}
