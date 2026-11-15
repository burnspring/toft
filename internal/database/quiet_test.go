package database

import (
	"testing"
	"time"
)

// The quiet verdict is a pure function so it can be tested exactly like
// ResolveRetention — no database, no fixtures, and every branch of the policy
// stated as a case somebody can read.

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestQuietBasis(t *testing.T) {
	now := day("2026-08-16")

	cases := []struct {
		name      string
		activity  AppActivity
		wantDays  int
		wantBasis string
		wantLast  string
	}{
		{
			// The ordinary case: somebody used it, then stopped.
			name: "a visit is the most recent evidence",
			activity: AppActivity{
				CreatedAt:  day("2025-01-01"),
				LastDeploy: day("2026-01-10"),
				LastDay:    day("2026-05-18"),
			},
			wantDays: 90, wantBasis: QuietBasisVisit, wantLast: "2026-05-18",
		},
		{
			// The case that makes this more than "ORDER BY last visit": an app
			// deployed last week with no traffic is not an app abandoned in
			// January. Somebody is clearly still working on it.
			name: "a fresh deploy outranks an old visit",
			activity: AppActivity{
				CreatedAt:  day("2024-06-01"),
				LastDay:    day("2025-02-02"),
				LastDeploy: day("2026-08-09"),
			},
			wantDays: 7, wantBasis: QuietBasisDeploy, wantLast: "2025-02-02",
		},
		{
			// Deployed and never opened. The clock runs from the deploy, and
			// the empty LastDay is what lets the UI say "never opened" rather
			// than inventing a visit.
			name: "never visited falls back to the deploy",
			activity: AppActivity{
				CreatedAt:  day("2026-03-01"),
				LastDeploy: day("2026-03-02"),
			},
			wantDays: 167, wantBasis: QuietBasisDeploy, wantLast: "",
		},
		{
			// Created, never successfully deployed, never opened. This app has
			// never worked, and saying so is different from saying nobody uses
			// it.
			name: "no traffic and no successful deploy falls back to creation",
			activity: AppActivity{
				CreatedAt: day("2026-07-17"),
			},
			wantDays: 30, wantBasis: QuietBasisCreated, wantLast: "",
		},
		{
			// Used today. Not quiet at all, and the count must be zero rather
			// than a negative number or a one.
			name: "used today is zero days quiet",
			activity: AppActivity{
				CreatedAt: day("2025-01-01"),
				LastDay:   day("2026-08-16"),
			},
			wantDays: 0, wantBasis: QuietBasisVisit, wantLast: "2026-08-16",
		},
		{
			// A clock in the future must not produce a negative day count — a
			// deploy row finished a moment after `now` is a clock skew, not a
			// reason to render "-1 days".
			name: "evidence in the future clamps to zero",
			activity: AppActivity{
				CreatedAt:  day("2026-01-01"),
				LastDeploy: day("2026-09-01"),
			},
			wantDays: 0, wantBasis: QuietBasisDeploy, wantLast: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.activity.Quiet(now)
			if got.QuietDays != tc.wantDays {
				t.Errorf("QuietDays = %d, want %d (since %s)", got.QuietDays, tc.wantDays, got.QuietSince)
			}
			if got.QuietBasis != tc.wantBasis {
				t.Errorf("QuietBasis = %q, want %q", got.QuietBasis, tc.wantBasis)
			}
			if got.LastDay != tc.wantLast {
				t.Errorf("LastDay = %q, want %q", got.LastDay, tc.wantLast)
			}
		})
	}
}

// Truncation, not rounding. An app last opened 47 hours ago has been quiet for
// one day; a list that rounds it to two is a list an administrator stops
// trusting the first time they check a row by hand.
func TestQuietDaysTruncate(t *testing.T) {
	now := time.Date(2026, 8, 16, 23, 0, 0, 0, time.UTC)
	a := AppActivity{
		CreatedAt:  day("2020-01-01"),
		LastDeploy: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), // 47 hours earlier
	}
	if got := a.Quiet(now).QuietDays; got != 1 {
		t.Errorf("47 hours quiet reported as %d days, want 1", got)
	}
}
