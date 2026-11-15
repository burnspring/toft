package database

import "testing"

// The platform cap and the app's own setting meet in ResolveRetention, and
// almost every way of getting this feature wrong is a way of getting this
// function wrong: clamping the stored value, treating the built-in default as
// something the owner asked for, or letting a keep of zero through.

func TestResolveRetention(t *testing.T) {
	cases := []struct {
		name         string
		autoDelete   bool
		kept         int
		max          int
		wantKeep     int
		wantConflict bool
	}{
		{"nothing set anywhere is the built-in default", true, 0, 0, DefaultReleasesKept, false},
		{"an owner's figure is honoured when there is no cap", true, 10, 0, 10, false},
		{"the cap lowers the number actually applied", true, 10, 5, 5, true},
		{"under the cap is not a conflict", true, 3, 5, 3, false},
		{"exactly at the cap is not a conflict", true, 5, 5, 5, false},
		// The owner never asked for three; being handed less than a default
		// they never chose is the platform working, not a disagreement to
		// report on their settings page.
		{"a cap below the default is not a conflict", true, 0, 2, 2, false},
		{"cleanup off does not change the resolved number", false, 10, 5, 5, true},
		// A keep of zero would delete the release being deployed.
		{"a cap of one still keeps one", true, 4, 1, 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{AutoDeleteReleases: tc.autoDelete, ReleasesKept: tc.kept}
			r := ResolveRetention(app, PlatformSettings{MaxVersions: tc.max})

			if r.Keep != tc.wantKeep {
				t.Errorf("Keep = %d, want %d", r.Keep, tc.wantKeep)
			}
			if r.Conflict != tc.wantConflict {
				t.Errorf("Conflict = %v, want %v", r.Conflict, tc.wantConflict)
			}
			if r.AutoDelete != tc.autoDelete {
				t.Errorf("AutoDelete = %v, want %v", r.AutoDelete, tc.autoDelete)
			}
			// The whole point of storing the owner's figure unclamped: an
			// administrator who raises the cap later restores what they asked
			// for, and that only works if it survived being over the cap.
			if r.Requested != tc.kept {
				t.Errorf("Requested = %d, want the stored %d — a clamped value cannot be restored", r.Requested, tc.kept)
			}
		})
	}
}

func TestRetentionBlocksDeploy(t *testing.T) {
	cases := []struct {
		name       string
		autoDelete bool
		max        int
		versions   int
		want       bool
	}{
		{"no cap never blocks", false, 0, 100, false},
		// The shape of the whole feature: an app that prunes itself cannot
		// reach the cap, so it is never the one refused.
		{"cleanup on never blocks", true, 5, 100, false},
		{"under the cap deploys", false, 5, 4, false},
		{"at the cap blocks", false, 5, 5, true},
		{"over the cap blocks", false, 5, 9, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Retention{AutoDelete: tc.autoDelete, Max: tc.max}
			if got := r.BlocksDeploy(tc.versions); got != tc.want {
				t.Errorf("BlocksDeploy(%d) = %v, want %v", tc.versions, got, tc.want)
			}
			// The message and the decision must agree — they are read from
			// three different paths and shown on a fourth.
			if why := r.BlockReason(tc.versions); (why != "") != tc.want {
				t.Errorf("BlockReason(%d) = %q, but BlocksDeploy said %v", tc.versions, why, tc.want)
			}
		})
	}
}

// An uncounted app is never blocked. -1 means nobody counted, which happens
// whenever the count could not change the answer; reading it as a number would
// block every app on the install.
func TestRetentionUncountedNeverBlocks(t *testing.T) {
	r := Retention{AutoDelete: false, Max: 1}
	if r.BlockReason(-1) != "" {
		t.Error("an app whose versions were never counted was blocked")
	}
}
