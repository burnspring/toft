package autodeploy

import (
	"context"
	"errors"
	"testing"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/events"
	"toft/internal/gitconn"
	"toft/internal/runtime"
)

// remote stands in for the repository: its tip, and the app folder's tree there.
type remote struct {
	tip, tree string
	treeErr   error
	fetches   int
}

func (r *remote) install(t *testing.T) {
	t.Helper()
	savedLs, savedTree := lsRemote, subdirTree
	lsRemote = func(context.Context, string, string, string) (string, error) { return r.tip, nil }
	subdirTree = func(context.Context, string, string, string, string) (string, string, error) {
		r.fetches++
		return r.tip, r.tree, r.treeErr
	}
	t.Cleanup(func() { lsRemote, subdirTree = savedLs, savedTree })
}

// folderPoller is a poller over one auto-deploying app built from apps/one,
// whose last build saw commit "c1" with the folder at tree "t1".
func folderPoller(t *testing.T, subdir string) (*Poller, *database.DB, *database.App) {
	t.Helper()
	db := dbtest.New(t)
	owner := &database.User{ID: "owner", Login: "owner"}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		ID: "app-mono", Slug: "mono", Name: "mono", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/mono", Branch: "main", RepoSubdir: subdir,
		Status: database.AppStatusRunning, AutoDeploy: true,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	tree := ""
	if subdir != "" {
		tree = "t1"
	}
	if err := db.SetLastSeen(app.ID, "c1", tree); err != nil {
		t.Fatalf("SetLastSeen: %v", err)
	}
	bld := builder.New(db, runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil), nil, gitconn.NewResolver(db, nil), nil, builder.Options{})
	p := New(db, bld, nil, events.NewRecorder(db, nil, nil), 0, nil)
	fresh, err := db.GetApp(app.ID)
	if err != nil || fresh == nil {
		t.Fatalf("GetApp: %v", err)
	}
	return p, db, fresh
}

func outcome(t *testing.T, db *database.DB, appID string) (seen string, deployments int) {
	t.Helper()
	app, err := db.GetApp(appID)
	if err != nil || app == nil {
		t.Fatalf("GetApp: %v", err)
	}
	ds, err := db.ListDeployments(appID)
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	return app.LastSeenSHA, len(ds)
}

// A push outside the folder claims the commit without building; a push inside
// it builds. Get the first wrong and every app in a monorepo rebuilds on every
// push; get the second wrong and an owner's change silently never deploys.
func TestAPushOutsideTheFolderClaimsWithoutBuilding(t *testing.T) {
	cases := []struct {
		name      string
		subdir    string
		r         remote
		wantBuild bool
		fetches   int
	}{
		{"outside the folder", "apps/one", remote{tip: "c2", tree: "t1"}, false, 1},
		{"inside the folder", "apps/one", remote{tip: "c2", tree: "t2"}, true, 1},
		{"the folder is gone", "apps/one", remote{tip: "c2", tree: ""}, true, 1},
		{"the fetch fails", "apps/one", remote{tip: "c2", treeErr: errors.New("unreachable")}, true, 1},
		{"at the repository root", "", remote{tip: "c2", tree: "t1"}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, db, app := folderPoller(t, c.subdir)
			r := c.r
			r.install(t)

			p.check(app)

			seen, deployments := outcome(t, db, app.ID)
			if seen != "c2" {
				t.Errorf("last_seen_sha = %q, want the tip claimed", seen)
			}
			if built := deployments == 1; built != c.wantBuild {
				t.Errorf("deployments = %d, want a build: %v", deployments, c.wantBuild)
			}
			if r.fetches != c.fetches {
				t.Errorf("tree fetches = %d, want %d", r.fetches, c.fetches)
			}
		})
	}
}

// The tip moved again between ls-remote and the tree fetch: the tree read is
// of a commit the poller is not claiming, so it builds rather than guess.
func TestATipThatMovesUnderTheFetchBuilds(t *testing.T) {
	p, db, app := folderPoller(t, "apps/one")
	r := remote{tip: "c2", tree: "t1"}
	r.install(t)
	subdirTree = func(context.Context, string, string, string, string) (string, string, error) {
		return "c3", "t1", nil
	}

	p.check(app)
	if _, deployments := outcome(t, db, app.ID); deployments != 1 {
		t.Errorf("deployments = %d, want the build", deployments)
	}
}
