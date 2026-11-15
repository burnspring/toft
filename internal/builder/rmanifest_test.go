package builder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The manifest a Connect bundle actually ships, cut down to one package of each
// kind this has to decide about. Written out in full rather than built by a
// helper, because the shape — the version living under `description` and not
// beside `Source` — is the thing that would silently produce an empty lock.
const connectManifestJSON = `{
  "version": 1,
  "locale": "en_US",
  "platform": "4.5.2",
  "metadata": {"appmode": "shiny", "primary_rmd": null},
  "environment": {"r": {"requires": ">= 4.1.0"}},
  "packages": {
    "shiny": {
      "Source": "CRAN",
      "Repository": "https://cran.rstudio.com",
      "description": {"Package": "shiny", "Version": "1.14.0", "Config/needs/check": true}
    },
    "Biobase": {
      "Source": "Bioconductor",
      "Repository": "https://bioconductor.org/packages/3.20/bioc",
      "description": {"Package": "Biobase", "Version": "2.66.0"}
    },
    "EPATADA": {
      "Source": "github",
      "description": {
        "Package": "EPATADA", "Version": "0.0.1",
        "RemoteType": "github", "RemoteHost": "api.github.com",
        "RemoteUsername": "USEPA", "RemoteRepo": "EPATADA",
        "RemoteRef": "develop", "RemoteSha": "bb1e8b1a7ee5a1d4ac2bc9460bb4ce0494241ec6"
      }
    },
    "homegrown": {
      "Source": "local",
      "description": {"Package": "homegrown", "Version": "1.0.0"}
    },
    "nameless": {
      "Source": "CRAN",
      "description": {"Package": "nameless"}
    }
  },
  "files": {"app.R": {"checksum": "abc"}}
}`

func writeConnectManifest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLock(t *testing.T, dir string) renvLockFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "renv.lock"))
	if err != nil {
		t.Fatalf("no renv.lock was written: %v", err)
	}
	var lock renvLockFile
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatalf("the renv.lock written is not JSON: %v", err)
	}
	return lock
}

// The whole point: a bundle's packages reach renv, with their versions.
func TestAConnectManifestBecomesAnRenvLock(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, connectManifestJSON)

	report, err := adoptConnectManifest(dir, "")
	if err != nil {
		t.Fatalf("adopting the manifest: %v", err)
	}
	if report == nil {
		t.Fatal("a Connect R manifest was not recognised")
	}
	if !hasRenvLock(dir) {
		t.Fatal("the release still has no renv.lock, which is what every reader downstream asks for")
	}

	lock := readLock(t, dir)
	if lock.R.Version != "4.5.2" {
		t.Errorf("R version = %q, want the manifest's 4.5.2", lock.R.Version)
	}
	if got := lock.Packages["shiny"]; got.Version != "1.14.0" ||
		got.Source != "Repository" || got.Repository != "CRAN" {
		t.Errorf("shiny = %+v, want version 1.14.0 from Repository/CRAN", got)
	}
	// The URL the author's machine recorded must not become the record's
	// repository, or TOFT_R_REPOS would stop deciding where CRAN is.
	if strings.HasPrefix(lock.Packages["shiny"].Repository, "http") {
		t.Errorf("shiny's Repository is a URL (%q); it has to be the name renv resolves",
			lock.Packages["shiny"].Repository)
	}
	if got := lock.Packages["Biobase"]; got.Source != "Bioconductor" {
		t.Errorf("Biobase source = %q, want Bioconductor", got.Source)
	}
}

// A DESCRIPTION field whose value is not a string must not take the whole
// manifest down with it — this is why `description` is a named struct.
func TestANonStringDescriptionFieldDoesNotFailTheDecode(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, connectManifestJSON)
	if _, err := adoptConnectManifest(dir, ""); err != nil {
		t.Fatalf("adopting the manifest: %v", err)
	}
	if _, ok := readLock(t, dir).Packages["shiny"]; !ok {
		t.Error("shiny is missing, and it is the package carrying a boolean Config/ field")
	}
}

// A commit-pinned package keeps everything renv needs to fetch it, and is
// reported — it is the one class installed from somewhere the operator's
// repository setting does not name.
func TestACommitPinnedPackageKeepsItsRemoteFields(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, connectManifestJSON)

	report, err := adoptConnectManifest(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got := readLock(t, dir).Packages["EPATADA"]
	if got.Source != "GitHub" {
		t.Errorf("source = %q, want GitHub — renv reads this to pick the downloader", got.Source)
	}
	if got.RemoteSha != "bb1e8b1a7ee5a1d4ac2bc9460bb4ce0494241ec6" || got.RemoteUsername != "USEPA" ||
		got.RemoteRepo != "EPATADA" || got.RemoteRef != "develop" {
		t.Errorf("remote fields = %+v, want the manifest's username, repo, ref and commit", got)
	}
	if len(report.Remote) != 1 || report.Remote[0] != "EPATADA" {
		t.Errorf("Remote = %v, want the one commit-pinned package named", report.Remote)
	}
}

// What cannot be restored is left out *and said*, by name. A silent drop is a
// deploy that succeeds and an app that dies at library().
func TestWhatCannotBeRestoredIsNamedRatherThanDropped(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, connectManifestJSON)

	report, err := adoptConnectManifest(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	lock := readLock(t, dir)
	for _, name := range []string{"homegrown", "nameless"} {
		if _, ok := lock.Packages[name]; ok {
			t.Errorf("%s was written into the lock; renv cannot restore it", name)
		}
	}
	if len(report.Skipped) != 2 {
		t.Fatalf("Skipped = %v, want both", report.Skipped)
	}
	joined := strings.Join(report.Skipped, " ")
	if !strings.Contains(joined, "homegrown") || !strings.Contains(joined, "nameless") {
		t.Errorf("Skipped = %v, want each named", report.Skipped)
	}
	if !strings.Contains(joined, "no version recorded") {
		t.Errorf("Skipped = %v, want the versionless one to say why", report.Skipped)
	}
	if report.Converted != 3 {
		t.Errorf("Converted = %d, want the three that can be restored", report.Converted)
	}
}

// The author's own lock is the author's statement, and a bundle carrying both
// was snapshotted twice.
func TestACommittedLockIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, connectManifestJSON)
	const theirs = `{"R":{"Version":"4.4.1"},"Packages":{}}`
	if err := os.WriteFile(filepath.Join(dir, "renv.lock"), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := adoptConnectManifest(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if report != nil {
		t.Error("the manifest was adopted over a committed renv.lock")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "renv.lock"))
	if string(data) != theirs {
		t.Errorf("renv.lock = %s, want the author's file untouched", data)
	}
}

// `manifest.json` is not a name this platform owns, and reading somebody else's
// as a dependency declaration is worse than reading none.
func TestSomebodyElsesManifestIsNotAdopted(t *testing.T) {
	notOurs := map[string]string{
		"a progressive web app's": `{"name":"Dashboard","short_name":"Dash","icons":[],"start_url":"/"}`,
		"a Python bundle's":       `{"version":1,"platform":"3.12.4","environment":{"python":{"version":"3.12.4"}},"files":{}}`,
		"not JSON at all":         `<?xml version="1.0"?><manifest/>`,
		"an empty package set":    `{"version":1,"platform":"4.5.2","environment":{"r":{}},"packages":{}}`,
	}
	for what, body := range notOurs {
		t.Run(what, func(t *testing.T) {
			dir := t.TempDir()
			writeConnectManifest(t, dir, body)
			report, err := adoptConnectManifest(dir, "")
			if err != nil {
				t.Fatalf("reading %s manifest returned an error: %v", what, err)
			}
			if report != nil {
				t.Errorf("%s manifest was adopted as an R declaration", what)
			}
			if hasRenvLock(dir) {
				t.Errorf("%s manifest produced an renv.lock", what)
			}
		})
	}
}

// An R bundle that predates `environment.r` is still recognised by its
// platform, and a word there is not a version.
func TestTheRSignalIsEitherEnvironmentOrAnRShapedPlatform(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, `{"version":1,"platform":"4.5.2","packages":{
	  "shiny":{"Source":"CRAN","description":{"Package":"shiny","Version":"1.14.0"}}}}`)
	if report, _ := adoptConnectManifest(dir, ""); report == nil {
		t.Error("a bundle with no environment.r but an R-shaped platform was not recognised")
	}

	other := t.TempDir()
	writeConnectManifest(t, other, `{"version":1,"platform":"jvm","packages":{
	  "shiny":{"Source":"CRAN","description":{"Package":"shiny","Version":"1.14.0"}}}}`)
	if report, _ := adoptConnectManifest(other, ""); report != nil {
		t.Error("a manifest with neither R signal was adopted")
	}
}

// The lock this writes has to be the one `internal/deps` can enumerate, or a
// bundle would install packages the inventory says it does not have.
func TestTheWrittenLockIsTheShapeTheInventoryReads(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, connectManifestJSON)
	if _, err := adoptConnectManifest(dir, ""); err != nil {
		t.Fatal(err)
	}

	// The same fields deps.renvLock names, asked for the same way.
	var lock struct {
		Packages map[string]struct {
			Package    string `json:"Package"`
			Version    string `json:"Version"`
			Source     string `json:"Source"`
			Repository string `json:"Repository"`
		} `json:"Packages"`
	}
	data, _ := os.ReadFile(filepath.Join(dir, "renv.lock"))
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatalf("the inventory's reader could not parse it: %v", err)
	}
	if len(lock.Packages) != 3 {
		t.Fatalf("the inventory would see %d packages, want 3", len(lock.Packages))
	}
	for name, p := range lock.Packages {
		if p.Package == "" || p.Version == "" {
			t.Errorf("%s = %+v; an entry with no name or version is one no advisory can match",
				name, p)
		}
	}
}

// A Python bundle's platform is `3.12.4`, which is the same shape as an R
// one — so `environment.python` is what tells them apart, and it decides even
// when the bundle records packages.
func TestAPythonBundleIsNotReadAsRWhateverItsPlatformLooksLike(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, `{"version":1,"platform":"3.12.4",
	  "environment":{"python":{"version":"3.12.4"}},
	  "packages":{"shiny":{"Source":"CRAN","description":{"Package":"shiny","Version":"1.14.0"}}}}`)
	if report, _ := adoptConnectManifest(dir, ""); report != nil {
		t.Error("a Python bundle was adopted as an R declaration")
	}
	if hasRenvLock(dir) {
		t.Error("a Python bundle produced an renv.lock")
	}
}

// A version the repository does not have loses its pin rather than the deploy.
//
// The failure was a Connect host running r2u: rsconnect recorded Debian's
// version of Rcpp (`1.1.1-1`) into the manifest, CRAN has never published that
// string, and renv marked the download ✖, reported the other 62 as successful,
// and died sixty lines later at "there is no package called 'Rcpp'" while
// installing something else. Nothing in that sequence names the manifest.
func TestAVersionTheRepositoryLacksIsUnpinnedAndReported(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if strings.Contains(r.URL.Path, "Rcpp_1.1.1-1.tar.gz") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lock := renvLockFile{Packages: map[string]renvLockPackage{
		"Rcpp":  {Package: "Rcpp", Version: "1.1.1-1", Source: "Repository", Repository: "CRAN"},
		"later": {Package: "later", Version: "1.4.8", Source: "Repository", Repository: "CRAN"},
		// A commit-pinned record is fetched from its host; there is no version
		// in this repository to look for and it must not be dropped.
		"rCharts": {Package: "rCharts", Version: "0.4.5", Source: "GitHub", RemoteSha: "abc"},
	}}
	report := &connectReport{Converted: 3}
	reconcileVersions(&lock, srv.URL, report)

	if _, ok := lock.Packages["Rcpp"]; ok {
		t.Error("Rcpp kept a version the repository does not have, so the restore will fail on it")
	}
	if _, ok := lock.Packages["later"]; !ok {
		t.Error("later was dropped, and the repository has it")
	}
	if _, ok := lock.Packages["rCharts"]; !ok {
		t.Error("a commit-pinned record was dropped by a check that does not apply to it")
	}
	if len(report.Unpinned) != 1 || !strings.Contains(report.Unpinned[0], "1.1.1-1") {
		t.Errorf("Unpinned = %v, want one entry naming the version that was dropped", report.Unpinned)
	}
	// Its own bucket: the package is still installed, so Skipped's copy — "left
	// out, and the app will fail if it uses it" — would say the opposite thing.
	if len(report.Skipped) != 0 {
		t.Errorf("Skipped = %v, want the unpinned package reported separately", report.Skipped)
	}
	if report.Converted != 2 {
		t.Errorf("Converted = %d, want 2", report.Converted)
	}
}

// Anything that is not a clean 404 leaves the pin alone. Dropping pins because
// a proxy hiccuped would turn a network blip into a silently different
// dependency tree, which is worse than the failure this exists to fix.
func TestAnUnreachableRepositoryChangesNoPin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	for name, repos := range map[string]string{
		"a repository answering 500":         srv.URL,
		"a repository that does not resolve": "http://127.0.0.1:1",
		// An install that configured none checks nothing at all.
		"no repository configured": "",
	} {
		lock := renvLockFile{Packages: map[string]renvLockPackage{
			"Rcpp": {Package: "Rcpp", Version: "1.1.1-1", Source: "Repository", Repository: "CRAN"},
		}}
		report := &connectReport{Converted: 1}
		reconcileVersions(&lock, repos, report)
		if _, ok := lock.Packages["Rcpp"]; !ok {
			t.Errorf("%s: the pin was dropped on something that is not a 404", name)
		}
		if len(report.Unpinned) != 0 {
			t.Errorf("%s: reported %v", name, report.Unpinned)
		}
	}
}

// A committed renv.lock that is a link to a path that does not exist is the
// author's lock, never a name the platform writes through: Stat read it as
// absent and the platform, as root before the handover, created the file it
// pointed at (security review 2026-11-10, E-4).
func TestADanglingRenvLockLinkIsNeverWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	writeConnectManifest(t, dir, connectManifestJSON)
	target := filepath.Join(t.TempDir(), "planted")
	if err := os.Symlink(target, filepath.Join(dir, "renv.lock")); err != nil {
		t.Fatal(err)
	}
	if report, err := adoptConnectManifest(dir, ""); err != nil || report != nil {
		t.Fatalf("adopt = %v, %v; want nothing done", report, err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("the platform created the file a committed link pointed at")
	}
	// And the write itself refuses a name that exists as a link.
	if err := createInRelease(dir, "renv.lock", []byte("{}")); err == nil {
		t.Fatal("createInRelease wrote through a dangling link")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("createInRelease created the link's target")
	}
}
