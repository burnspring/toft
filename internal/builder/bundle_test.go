package builder

import (
	"testing"

	"toft/internal/database"
)

// What a deploy is allowed to say about a commit, by where its code came from.
//
// The case this table exists for is the new one: a bundle pushed at an app that
// has a repository. Everything about that app says "git" — its repo_url, its
// branch, its auto-deploy switch — and the code being built came off somebody's
// laptop. Both writes below would otherwise take the app's word for it and
// record something that is not true of these files.
func TestWhatABundleMayNotClaim(t *testing.T) {
	gitApp := &database.App{RepoURL: "https://github.com/example/thing"}
	zipApp := &database.App{RepoURL: UploadScheme + "thing.zip"}

	cases := []struct {
		name    string
		job     BuildJob
		stamps  bool
		advance bool
	}{
		{
			name:    "a clone of the branch tip",
			job:     BuildJob{App: gitApp, Deployment: &database.Deployment{CommitSHA: "a91f3c2"}},
			stamps:  true,
			advance: true,
		},
		{
			name: "a bundle pushed at that same app",
			job: BuildJob{App: gitApp, Deployment: &database.Deployment{},
				SkipClone: true, FromBundle: true},
			stamps:  false,
			advance: false,
		},
		{
			// The belt-and-suspenders case. If a hand-built archive carried a
			// .git and something stamped a SHA anyway, the poller must still not
			// be told that commit has shipped — auto-deploy would skip the real
			// one.
			name: "a bundle that smuggled in a commit",
			job: BuildJob{App: gitApp, Deployment: &database.Deployment{CommitSHA: "a91f3c2"},
				SkipClone: true, FromBundle: true},
			stamps:  false,
			advance: false,
		},
		{
			name:    "an uploaded app's redeploy, which reuses the current release",
			job:     BuildJob{App: zipApp, Deployment: &database.Deployment{}, SkipClone: true},
			stamps:  true,
			advance: false, // no remote, so no poller to tell
		},
		{
			name: "a rollback",
			job: BuildJob{App: gitApp, Deployment: &database.Deployment{CommitSHA: "a91f3c2"},
				SourceDeployID: "older"},
			stamps:  true,
			advance: false, // rewinding last_seen_sha would undo the promote
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.job.stampsCommit(); got != tc.stamps {
				t.Errorf("stampsCommit() = %v, want %v", got, tc.stamps)
			}
			if got := tc.job.advancesPoller(); got != tc.advance {
				t.Errorf("advancesPoller() = %v, want %v", got, tc.advance)
			}
		})
	}
}
