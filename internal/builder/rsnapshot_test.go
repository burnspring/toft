package builder

import (
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestP3MRepoAcceptsOnlyPositsBinaryRepositories(t *testing.T) {
	origin, prefix, ok := p3mRepo("https://p3m.dev/cran/__linux__/trixie/latest")
	if !ok || origin != "https://p3m.dev" || prefix != "https://p3m.dev/cran/__linux__/trixie/" {
		t.Errorf("p3mRepo(latest) = %q, %q, %v", origin, prefix, ok)
	}
	for _, refused := range []string{
		"https://cran.internal.example/cran/__linux__/trixie/latest",  // an operator's mirror
		"http://p3m.dev/cran/__linux__/trixie/latest",                 // not https
		"https://p3m.dev/cran/latest",                                 // source, not a binary repository
		"https://p3m.dev/cran/__linux__/trixie/latest;X=https://evil", // already a list
		"",
	} {
		if _, _, ok := p3mRepo(refused); ok {
			t.Errorf("p3mRepo(%q) was accepted", refused)
		}
	}
}

// The day before the next change is picked, and one date covers every window
// that holds it.
func TestCoverDatesPicksTheFewestDatesInsideEveryWindow(t *testing.T) {
	ws := []window{
		{Package: "Rcpp", From: "2026-01-12", Until: "2026-04-17"},
		{Package: "S7", From: "2026-02-01", Until: "2026-05-01"},
		{Package: "old", From: "2024-03-01", Until: "2024-06-01"},
	}
	got := coverDates(ws, 6)
	want := []string{"2024-05-31", "2026-04-16"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("coverDates = %v, want %v", got, want)
	}
	if got := coverDates(ws, 1); len(got) != 1 {
		t.Errorf("coverDates with limit 1 = %v", got)
	}
}

func TestCurrentWindowRunsFromAVersionToTheNextChange(t *testing.T) {
	h := []snapshotEntry{{"2026-07-06", "1.1.2"}, {"2026-01-12", "1.1.1"}, {"2026-04-17", "1.1.1-1"}}
	w, ok := currentWindow("Rcpp", "1.1.1", h)
	if !ok || w.From != "2026-01-12" || w.Until != "2026-04-17" {
		t.Errorf("currentWindow = %+v, %v", w, ok)
	}
	if _, ok := currentWindow("Rcpp", "0.1", h); ok {
		t.Error("a version P3M never recorded got a window")
	}
}

// End to end against a fake P3M: a pin behind the index gets a dated snapshot,
// a current pin does not cause one, and a P3M that cannot answer changes nothing.
func TestPlanRestoreReposAddsASnapshotForAPinTheRepositoryHasMovedPast(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(`{"Packages":{
		"Rcpp":{"Package":"Rcpp","Version":"1.1.1","Source":"Repository"},
		"jsonlite":{"Package":"jsonlite","Version":"2.0.0","Source":"Repository"},
		"mine":{"Package":"mine","Version":"0.1","Source":"GitHub"}}}`), 0o644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/src/contrib/PACKAGES.gz"):
			gz := gzip.NewWriter(w)
			fmt.Fprint(gz, "Package: Rcpp\nVersion: 1.1.2\n\nPackage: jsonlite\nVersion: 2.0.0\n\nPackage: mine\nVersion: 9\n")
			gz.Close()
		case r.URL.Path == "/__api__/repos/cran/packages/Rcpp/history":
			fmt.Fprint(w, `{"pages":1,"snapshots":[
				{"date":"2026-07-06","latest_version":{"version":"1.1.2"}},
				{"date":"2026-01-12","latest_version":{"version":"1.1.1"}},
				{"date":"2025-12-09","latest_version":{"version":"1.1.0"}}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := p3mClient{http: srv.Client(), origin: srv.URL}
	repos := srv.URL + "/cran/__linux__/trixie/latest"

	var said []string
	got := planRestoreRepos(dir, repos, "https://p3m.dev/cran/__linux__/trixie/", c, func(s string) { said = append(said, s) })
	want := "CRAN=" + repos + ";P3M-2026-07-05=https://p3m.dev/cran/__linux__/trixie/2026-07-05"
	if got != want {
		t.Errorf("planRestoreRepos =\n %q\nwant\n %q", got, want)
	}
	if !strings.Contains(strings.Join(said, "\n"), "2026-07-05") {
		t.Errorf("the build log does not name the snapshot: %v", said)
	}

	srv.Close()
	if got := planRestoreRepos(dir, repos, "https://p3m.dev/cran/__linux__/trixie/", c, func(string) {}); got != repos {
		t.Errorf("with P3M unreachable the repositories changed to %q", got)
	}
}
