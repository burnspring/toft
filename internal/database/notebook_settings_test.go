package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The one build command whose clock is a setting.
//
// Every other command inherits a compiled-in ten minutes chosen for `npm ci`.
// A render's length is a property of the app's own work — a query, a model, a
// month of data — so a legitimate twenty-minute report has to be able to exist,
// and a render killed at a constant with a bare "signal: killed" is the failure
// this replaces.

func TestAFreshInstallRendersForTenMinutes(t *testing.T) {
	db := dbtest.New(t)

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if s.NotebookRenderTimeoutSecs != 600 {
		t.Errorf("default = %d, want 600 — exactly what a render inherited before it had a setting",
			s.NotebookRenderTimeoutSecs)
	}
	if want := 10 * time.Minute; s.NotebookRenderTimeout() != want {
		t.Errorf("NotebookRenderTimeout() = %v, want %v", s.NotebookRenderTimeout(), want)
	}
}

func TestTheRenderTimeoutRoundTrips(t *testing.T) {
	db := dbtest.New(t)

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	s.NotebookRenderTimeoutSecs = 1800
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}

	back, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if back.NotebookRenderTimeoutSecs != 1800 {
		t.Errorf("stored = %d, want 1800", back.NotebookRenderTimeoutSecs)
	}
}

// A row outside the range is read as the default rather than obeyed. The
// handler refuses an out-of-range value naming the range; this is the other
// half — a value that got into the table some other way must not become a
// build queue held open for a day.
func TestACorruptRenderTimeoutClampsToTheDefault(t *testing.T) {
	for _, stored := range []string{"5", "86400", "not a number", "0"} {
		t.Run(stored, func(t *testing.T) {
			db := dbtest.New(t)
			if _, err := db.PutSetting(database.SettingNotebookRenderTimeout, stored, database.BuiltinAdminID); err != nil {
				t.Fatalf("PutSetting: %v", err)
			}
			s, err := db.GetPlatformSettings()
			if err != nil {
				t.Fatalf("GetPlatformSettings: %v", err)
			}
			if s.NotebookRenderTimeoutSecs != 600 {
				t.Errorf("stored %q read back as %d, want the default 600",
					stored, s.NotebookRenderTimeoutSecs)
			}
		})
	}
}

// The bounds are one pair of numbers, read by the handler and by the clamp
// above. Two copies of a range is how a stored value comes to sit outside the
// range the form will accept.
func TestTheRenderTimeoutBounds(t *testing.T) {
	if database.MinNotebookRenderTimeoutSeconds != 60 || database.MaxNotebookRenderTimeoutSeconds != 3600 {
		t.Errorf("bounds = %d..%d, want 60..3600",
			database.MinNotebookRenderTimeoutSeconds, database.MaxNotebookRenderTimeoutSeconds)
	}
}
