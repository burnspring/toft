package staticserve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write creates a file, and every directory above it.
func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func serve(t *testing.T, dir string) *Handler {
	t.Helper()
	cfg, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func get(h *Handler, target string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- entry resolution --------------------------------------------------------

func TestEntryIsIndexHTMLWithoutCeremony(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<h1>hello</h1>")
	write(t, dir, "styles.css", "body{}")

	cfg, err := Resolve(dir)
	if err != nil {
		t.Fatalf("a folder with an index.html should just deploy: %v", err)
	}
	if cfg.Entry != "index.html" {
		t.Errorf("entry = %q, want index.html", cfg.Entry)
	}
	if cfg.RootRel != "." || cfg.Root != dir {
		t.Errorf("root = %q/%q, want the release directory", cfg.Root, cfg.RootRel)
	}
	if cfg.Files != 2 {
		t.Errorf("Files = %d, want 2", cfg.Files)
	}
}

func TestEntryIsTheOnlyHTMLFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "weekly-report.html", "<h1>report</h1>")

	cfg, err := Resolve(dir)
	if err != nil {
		t.Fatalf("one unambiguous document should serve: %v", err)
	}
	if cfg.Entry != "weekly-report.html" {
		t.Errorf("entry = %q", cfg.Entry)
	}
}

func TestSeveralDocumentsAndNoDeclarationIsARefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.html", "")
	write(t, dir, "b.html", "")

	_, err := Resolve(dir)
	if err == nil {
		t.Fatal("a root with two candidates and no index.html must not deploy")
	}
	// It names the candidates and the line to add, rather than picking one.
	for _, want := range []string{"a.html", "b.html", "[static]", "entry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %s", want, err)
		}
	}
}

func TestNoHTMLAtAllIsARefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "readme.txt", "hi")

	if _, err := Resolve(dir); err == nil {
		t.Fatal("a root with no HTML document must not deploy")
	}
}

func TestDeclaredEntryBeatsIndexHTML(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "wrong")
	write(t, dir, "report.html", "right")
	write(t, dir, ManifestFile, "[static]\nentry = \"report.html\"\n")

	cfg, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Entry != "report.html" {
		t.Errorf("entry = %q, want the declared one", cfg.Entry)
	}
}

func TestDeclaredEntryThatIsNotThereFailsByName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "")
	write(t, dir, ManifestFile, "[static]\nentry = \"missing.html\"\n")

	_, err := Resolve(dir)
	if err == nil || !strings.Contains(err.Error(), "missing.html") {
		t.Fatalf("want the missing file named, got %v", err)
	}
}

func TestDeclaredRootPublishesOnlyThatDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "src/secret.js", "// source")
	write(t, dir, "dist/index.html", "<h1>built</h1>")
	write(t, dir, ManifestFile, "[static]\nroot = \"dist\"\n")

	cfg, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootRel != "dist" {
		t.Errorf("root = %q, want dist", cfg.RootRel)
	}
	if cfg.Files != 1 {
		t.Errorf("Files = %d, want only the built file", cfg.Files)
	}

	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if rec := get(h, "/src/secret.js"); rec.Code != http.StatusNotFound {
		t.Errorf("a file outside the declared root answered %d", rec.Code)
	}
}

func TestDeclaredRootThatIsNotThereFailsByName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "")
	write(t, dir, ManifestFile, "[static]\nroot = \"dist\"\n")

	_, err := Resolve(dir)
	if err == nil || !strings.Contains(err.Error(), "dist") {
		t.Fatalf("want the missing directory named, got %v", err)
	}
}

func TestDeclarationRefusesAnEscapingRoot(t *testing.T) {
	for _, root := range []string{"/etc", "../elsewhere", "/"} {
		dir := t.TempDir()
		write(t, dir, ManifestFile, "[static]\nroot = \""+root+"\"\n")
		if _, err := ReadDeclaration(dir); err == nil {
			t.Errorf("root = %q was accepted", root)
		}
	}
}

func TestDeclarationIsAbsentWithoutAStaticTable(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ManifestFile, "[jobs.nightly]\ncommand = \"echo hi\"\n")

	d, err := ReadDeclaration(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Present {
		t.Error("a manifest with only [jobs] must not read as a static declaration")
	}
}

// --- what is refused ---------------------------------------------------------

func TestTheReleaseNeverPublishesItsOwnPlumbing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<h1>site</h1>")
	write(t, dir, ".git/config", "[remote \"origin\"]\n url = git@github.com:acme/private.git")
	write(t, dir, ".git/objects/pack/pack-abc.pack", "binary")
	write(t, dir, ".env", "DATABASE_URL=postgres://user:pw@db/app")
	write(t, dir, ".env.production", "SECRET=1")
	write(t, dir, ManifestFile, "[static]\n")
	write(t, dir, "ecosystem.config.json", "{}")
	write(t, dir, "docs/.hidden/notes.txt", "private")

	h := serve(t, dir)
	for _, path := range []string{
		"/.git/config",
		"/.git/objects/pack/pack-abc.pack",
		"/.env",
		"/.env.production",
		"/" + ManifestFile,
		"/ecosystem.config.json",
		"/docs/.hidden/notes.txt",
		"/.git/",
	} {
		rec := get(h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "origin") || strings.Contains(rec.Body.String(), "postgres") {
			t.Errorf("GET %s leaked the file's contents", path)
		}
	}
}

func TestASymlinkOutOfTheTreeIs404(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<h1>site</h1>")

	outside := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(outside, []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("/etc", filepath.Join(dir, "etc")); err != nil {
		t.Fatal(err)
	}

	h := serve(t, dir)
	for _, path := range []string{"/link", "/etc/passwd"} {
		rec := get(h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "root:x") {
			t.Errorf("GET %s followed the symlink out of the tree", path)
		}
	}
}

func TestTraversalOutOfTheRootIs404(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dist/index.html", "<h1>built</h1>")
	write(t, dir, "secret.txt", "no")
	write(t, dir, ManifestFile, "[static]\nroot = \"dist\"\n")

	h := serve(t, dir)
	for _, path := range []string{"/../secret.txt", "/a/../../secret.txt"} {
		if rec := get(h, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", path, rec.Code)
		}
	}
}

// --- serving -----------------------------------------------------------------

func TestEntryIsServedAtTheMountPathAndAtItsOwnURL(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report.html", "<h1>the report</h1>")
	write(t, dir, ManifestFile, "[static]\nentry = \"report.html\"\n")

	h := serve(t, dir)

	rec := get(h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / answered %d, want the entry document", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "the report") {
		t.Error("GET / did not serve the entry document")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("the entry document was redirected to %q; it must be served at the directory path", loc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}

	if rec := get(h, "/report.html"); rec.Code != http.StatusOK {
		t.Errorf("the entry's own URL answered %d", rec.Code)
	}
}

func TestASubdirectoryWithoutItsSlashRedirects(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "root")
	write(t, dir, "guide/index.html", "guide")

	h := serve(t, dir)

	rec := get(h, "/guide")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /guide answered %d, want a redirect", rec.Code)
	}
	// Root-relative on purpose: the proxy's rewriteLocation is what puts the
	// app's own prefix back on it.
	if loc := rec.Header().Get("Location"); loc != "/guide/" {
		t.Errorf("Location = %q, want /guide/", loc)
	}

	if rec := get(h, "/guide/"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "guide") {
		t.Errorf("GET /guide/ answered %d", rec.Code)
	}
}

func TestADirectoryWithNoIndexIsNotListed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "root")
	write(t, dir, "assets/secret-name.js", "x")

	h := serve(t, dir)
	rec := get(h, "/assets/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /assets/ answered %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret-name.js") {
		t.Error("a directory listing published the names of files nobody chose to publish")
	}
}

func TestConditionalRequestsAndRangeComeFree(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "0123456789")

	h := serve(t, dir)

	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	req.Header.Set("Range", "bytes=2-4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("a Range request answered %d", rec.Code)
	}
	if rec.Body.String() != "234" {
		t.Errorf("range body = %q", rec.Body.String())
	}

	full := get(h, "/index.html")
	req = httptest.NewRequest(http.MethodGet, "/index.html", nil)
	req.Header.Set("If-Modified-Since", full.Header().Get("Last-Modified"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("a conditional request answered %d, want 304", rec.Code)
	}
}

func TestEveryFileIsRevalidatedBeforeReuse(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<p>hi</p>")
	write(t, dir, "app.js", "x")
	write(t, dir, NotFoundDocument, "gone")

	h := serve(t, dir)
	for _, target := range []string{"/", "/app.js", "/missing.css"} {
		if got := get(h, target).Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", target, got)
		}
	}

	full := get(h, "/app.js")
	etag := full.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if rec := get(h, "/app.js", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Errorf("a matching If-None-Match answered %d, want 304", rec.Code)
	}
}

// A rollback serves files older than the browser's copy, so a date alone
// would answer 304 to it. Another serve-static process is another release, and
// its validators never match this one's.
func TestAnotherReleaseNeverMatchesThisOnesValidator(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<p>hi</p>")

	etag := get(serve(t, dir), "/").Header().Get("ETag")
	rec := get(serve(t, dir), "/",
		"If-None-Match", etag,
		"If-Modified-Since", "Mon, 01 Jan 2101 00:00:00 GMT")
	if rec.Code != http.StatusOK {
		t.Errorf("another release answered %d to this one's validators, want 200", rec.Code)
	}
}

func TestOnlyGETAndHEAD(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "site")

	h := serve(t, dir)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST answered %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Error("a 405 with no Allow header")
	}
}

// --- 404 and fallback --------------------------------------------------------

func TestTheAppsOwn404IsTheBody(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "site")
	write(t, dir, "404.html", "<h1>nothing here</h1>")

	h := serve(t, dir)
	rec := get(h, "/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("answered %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nothing here") {
		t.Error("the app's own 404 document was not used")
	}
}

func TestFallbackIsOffByDefault(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<div id=root></div>")

	h := serve(t, dir)
	rec := get(h, "/dashboard", "Accept", "text/html")
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unmatched route answered %d with no [static] fallback", rec.Code)
	}
}

func TestFallbackIsNarrowWhenItIsOn(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<div id=root></div>")
	write(t, dir, ManifestFile, "[static]\nfallback = true\n")

	h := serve(t, dir)

	rec := get(h, "/dashboard/settings", "Accept", "text/html,application/xhtml+xml")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "id=root") {
		t.Errorf("a client route answered %d, want the entry document", rec.Code)
	}

	// The whole point of the two conditions: a missing asset must not arrive as
	// an HTML document where JavaScript was expected.
	if rec := get(h, "/assets/app.js", "Accept", "text/html"); rec.Code != http.StatusNotFound {
		t.Errorf("a missing asset answered %d, want 404", rec.Code)
	}
	if rec := get(h, "/api/thing", "Accept", "application/json"); rec.Code != http.StatusNotFound {
		t.Errorf("a non-HTML request answered %d, want 404", rec.Code)
	}

	// A caller that does not say what it wants -- curl's */*, toft fetch's
	// nothing at all -- gets the route, as a browser does. Only text/html used
	// to count, so every client route was a 404 to anything but a browser.
	for _, accept := range []string{"*/*", ""} {
		rec := get(h, "/deep/a/b", "Accept", accept)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "id=root") {
			t.Errorf("Accept %q on a client route answered %d, want the entry document", accept, rec.Code)
		}
	}
	// A write is never answered with the entry page.
	req := httptest.NewRequest(http.MethodPost, "/dashboard", nil)
	req.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Errorf("a POST to a client route answered 200 with the entry document")
	}
}

// --- the lint ----------------------------------------------------------------

func TestLintNamesRootAbsoluteURLsAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", `<link href="/styles.css"><script src="./app.js"></script>`+
		`<img src="//cdn.example/x.png"><a href="https://example.com">out</a>`+
		`<a href="../shared/a.css">up</a>`)

	lines := strings.Join(Lint(dir, "."), "\n")
	if !strings.Contains(lines, "/styles.css") {
		t.Errorf("the root-absolute URL was not reported: %s", lines)
	}
	for _, ok := range []string{"./app.js", "//cdn.example", "https://example.com", "../shared"} {
		if strings.Contains(lines, ok) {
			t.Errorf("%q was reported and should not have been", ok)
		}
	}
}

func TestLintIsSilentOnACleanSite(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", `<link href="styles.css"><script src="./app.js"></script>`)
	if got := Lint(dir, "."); got != nil {
		t.Errorf("Lint = %v, want nothing to say", got)
	}
}

// BB-014: `root = "."` is the repository itself, not a refusal.
func TestARootOfDotIsTheRepository(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "toft.toml", "[static]\nroot = \".\"\n")
	write(t, dir, "index.html", "<h1>hi</h1>")
	d, err := ReadDeclaration(dir)
	if err != nil {
		t.Fatalf("root = \".\" was refused: %v", err)
	}
	if !d.Present || d.Root != "" {
		t.Fatalf("declaration = %+v, want present with the repository as root", d)
	}
	cfg, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootRel != "." || cfg.Entry != "index.html" {
		t.Errorf("resolved %+v", cfg)
	}
}

// BB-015: a missing directory to serve, with no [static] declared, is not
// blamed on `[static] root`.
func TestAMissingServeDirIsNotBlamedOnStatic(t *testing.T) {
	_, err := Resolve(filepath.Join(t.TempDir(), "toft-render"))
	if err == nil || strings.Contains(err.Error(), "[static]") {
		t.Fatalf("err = %v, want one that does not name [static]", err)
	}
}
