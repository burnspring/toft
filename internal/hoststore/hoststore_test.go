package hoststore

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"

	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"toft/internal/artifact"
	"toft/internal/runtime"
)

type rig struct {
	t      *testing.T
	store  *Store
	dir    string // the resource directory
	audits []int64
	mu     sync.Mutex
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "share"), 0o770); err != nil {
		t.Fatal(err)
	}
	g := &rig{t: t, dir: filepath.Join(root, "share")}
	g.store = New(DeriveKey([]byte("secret")), "https://toft.example",
		[]runtime.StorageRoot{{Name: "data", Path: root}}, func(_ Claims, n int64) {
			g.mu.Lock()
			g.audits = append(g.audits, n)
			g.mu.Unlock()
		})
	return g
}

func (g *rig) signer(scope string) artifact.Signer {
	return g.store.For(Binding{Resource: "res-1", Root: "data", Slug: "share", Scope: scope,
		Subject: "user-1", GID: os.Getgid()})
}

func (g *rig) do(signed *artifact.SignedRequest, body []byte) *httptest.ResponseRecorder {
	g.t.Helper()
	u, err := url.Parse(signed.URL)
	if err != nil {
		g.t.Fatal(err)
	}
	req := httptest.NewRequest(signed.Method, u.Path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	g.store.ServeHTTP(rec, req)
	return rec
}

func conditions(body []byte, ifNoneMatch bool) artifact.PutConditions {
	m, s := md5.Sum(body), sha256.Sum256(body)
	return artifact.PutConditions{IfNoneMatch: ifNoneMatch, ContentLength: int64(len(body)),
		ContentMD5: base64.StdEncoding.EncodeToString(m[:]), ChecksumSHA256: base64.StdEncoding.EncodeToString(s[:])}
}

func (g *rig) put(scope, key string, body []byte, ifNoneMatch bool) *httptest.ResponseRecorder {
	g.t.Helper()
	signed, err := g.signer(scope).PresignPut(context.Background(), "", key, time.Hour, conditions(body, ifNoneMatch))
	if err != nil {
		g.t.Fatal(err)
	}
	return g.do(signed, body)
}

func (g *rig) get(scope, key string) *httptest.ResponseRecorder {
	g.t.Helper()
	signed, err := g.signer(scope).PresignGet(context.Background(), "", key, time.Hour, "")
	if err != nil {
		g.t.Fatal(err)
	}
	return g.do(signed, nil)
}

// The scope is a root, not a string check. An app on teams/a plants a link to
// its sibling; a person granted teams/a is refused through it, and so is a
// person granted the whole resource: a link below the scope is never
// followed, even one that stays inside it (AS-13). A prefix comparison on the
// key would pass the first, which is the escape.
func TestTheScopeHoldsAgainstAPlantedSymlink(t *testing.T) {
	g := newRig(t)
	if rec := g.put("", "teams/b/secret", []byte("b's"), true); rec.Code != http.StatusOK {
		t.Fatalf("seeding b: %d %s", rec.Code, rec.Body.String())
	}
	if err := os.MkdirAll(filepath.Join(g.dir, "teams", "a"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../b", filepath.Join(g.dir, "teams", "a", "x")); err != nil {
		t.Fatal(err)
	}
	if rec := g.get("teams/a", "teams/a/x/secret"); rec.Code != http.StatusForbidden {
		t.Errorf("through the link from teams/a = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := g.get("", "teams/a/x/secret"); rec.Code != http.StatusForbidden {
		t.Errorf("through the link from the root = %d %q, want 403", rec.Code, rec.Body.String())
	}
}

// The scope folder itself is never a link: an app on a whole resource could
// otherwise point a granted folder at one the grant does not cover.
func TestAGrantedFolderThatIsALinkIsRefused(t *testing.T) {
	g := newRig(t)
	if rec := g.put("", "finance/q1", []byte("money"), true); rec.Code != http.StatusOK {
		t.Fatalf("seeding: %d %s", rec.Code, rec.Body.String())
	}
	if err := os.Symlink("finance", filepath.Join(g.dir, "reports")); err != nil {
		t.Fatal(err)
	}
	if rec := g.get("reports", "reports/q1"); rec.Code != http.StatusForbidden {
		t.Errorf("a linked scope = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := g.put("reports", "reports/new", []byte("x"), true); rec.Code != http.StatusForbidden {
		t.Errorf("a write through a linked scope = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "finance", "new")); err == nil {
		t.Error("the write landed in the folder the link pointed at")
	}
}

// The same refusal when the folder is a real one at the check and a link by the
// open: an app on a whole resource can rename entries there, and a person whose
// grant covers only reports/ must not be served finance/ (KUBE-4). The swap is
// made in exactly that gap.
func TestAGrantedFolderSwappedForALinkAfterTheCheckIsRefused(t *testing.T) {
	g := newRig(t)
	if rec := g.put("", "finance/q1", []byte("money"), true); rec.Code != http.StatusOK {
		t.Fatalf("seeding: %d %s", rec.Code, rec.Body.String())
	}
	if rec := g.put("", "reports/r1", []byte("mine"), true); rec.Code != http.StatusOK {
		t.Fatalf("seeding: %d %s", rec.Code, rec.Body.String())
	}
	betweenCheckAndOpen = func(_ *os.Root, comp string) {
		if comp != "reports" {
			return
		}
		reports := filepath.Join(g.dir, "reports")
		if err := os.Rename(reports, reports+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("finance", reports); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { betweenCheckAndOpen = nil }()
	rec := g.get("reports", "reports/q1")
	if rec.Code != http.StatusForbidden {
		t.Errorf("a scope swapped for a link = %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// `write` never overwrites, even racing itself: of two create-only PUTs to
// one name exactly one lands and the other is the 412 the CLI already reads.
func TestWriteNeverOverwritesEvenRacing(t *testing.T) {
	g := newRig(t)
	codes := make([]int, maxConcurrent)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = g.put("", "report.csv", []byte(strings.Repeat("x", i+1)), true).Code
		}(i)
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			won++
		case http.StatusPreconditionFailed:
		default:
			t.Fatalf("a racing create answered %d", c)
		}
	}
	if won != 1 {
		t.Fatalf("%d creates landed on one name, want 1 (%v)", won, codes)
	}
	if left := temps(t, g.dir); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// A body that is not the one the link was signed for changes nothing: the old
// object stays, and no temp file is left beside it.
func TestAFailedPutChangesNothing(t *testing.T) {
	g := newRig(t)
	g.put("", "report.csv", []byte("old"), true)
	signed, _ := g.signer("").PresignPut(context.Background(), "", "report.csv", time.Hour,
		conditions([]byte("new"), false))
	if rec := g.do(signed, []byte("NEW")); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "digest") {
		t.Errorf("a mismatched body = %d %s, want 400 naming the digest", rec.Code, rec.Body.String())
	}
	if rec := g.do(signed, []byte("ne")); rec.Code != http.StatusBadRequest {
		t.Errorf("a short body = %d, want 400", rec.Code)
	}
	if b, _ := os.ReadFile(filepath.Join(g.dir, "report.csv")); string(b) != "old" {
		t.Errorf("the object is now %q", b)
	}
	if left := temps(t, g.dir); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
	if rec := g.do(signed, []byte("new")); rec.Code != http.StatusOK {
		t.Errorf("overwrite = %d %s", rec.Code, rec.Body.String())
	}
	if b, _ := os.ReadFile(filepath.Join(g.dir, "report.csv")); string(b) != "new" {
		t.Errorf("after overwrite the object is %q", b)
	}
}

// What the platform writes is the gate's: the recorded group, a file 0660 and
// a folder 2770, whatever the platform's own umask is — so the apps on the
// gate can edit and delete what a person uploaded.
func TestWhatIsWrittenIsTheGates(t *testing.T) {
	g := newRig(t)
	if rec := g.put("", "a/b/file", []byte("x"), true); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	check := func(p string, want os.FileMode) {
		info, err := os.Stat(filepath.Join(g.dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (os.ModePerm | os.ModeSetgid); got != want {
			t.Errorf("%s is %v, want %v", p, got, want)
		}
		if gid := int(info.Sys().(*syscall.Stat_t).Gid); gid != os.Getgid() {
			t.Errorf("%s is group %d, want the gate's %d", p, gid, os.Getgid())
		}
	}
	check("a", gatedFolderMode)
	check("a/b", gatedFolderMode)
	check("a/b/file", gatedFileMode)
	if len(g.audits) != 1 || g.audits[0] != 1 {
		t.Errorf("completed writes audited = %v, want one of 1 byte", g.audits)
	}
}

// A file with a second name is not served: the walk cannot see where the
// other name lives, and protected_hardlinks is the only thing stopping an app
// from making it.
func TestAHardLinkedFileIsNotServed(t *testing.T) {
	g := newRig(t)
	g.put("", "one", []byte("x"), true)
	if err := os.Link(filepath.Join(g.dir, "one"), filepath.Join(g.dir, "two")); err != nil {
		t.Fatal(err)
	}
	if rec := g.get("", "one"); rec.Code != http.StatusForbidden {
		t.Errorf("a file with two links = %d, want 403", rec.Code)
	}
}

// A download is never something a browser renders: this is the platform's own
// origin, and an uploaded page served with its own type would run as it.
func TestADownloadIsOpaqueBytesInASandbox(t *testing.T) {
	g := newRig(t)
	g.put("", "page.html", []byte("<script>alert(1)</script>"), true)
	rec := g.get("", "page.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("download headers = %v", h)
	}
}

// The token is the credential, whole: an edited one, an expired one and one
// used with another method are all refused.
func TestTheTokenIsTheWholeCredential(t *testing.T) {
	g := newRig(t)
	g.put("", "f", []byte("x"), true)
	signed, _ := g.signer("").PresignGet(context.Background(), "", "f", time.Hour, "")
	tampered := *signed
	tampered.URL = strings.Replace(signed.URL, Route, Route+"A", 1)
	if rec := g.do(&tampered, nil); rec.Code != http.StatusForbidden {
		t.Errorf("an edited token = %d, want 403", rec.Code)
	}
	wrong := *signed
	wrong.Method = http.MethodDelete
	if rec := g.do(&wrong, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("a GET token used to DELETE = %d, want 405", rec.Code)
	}
	g.store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if rec := g.do(signed, nil); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("an expired token = %d %s, want 403 naming the expiry", rec.Code, rec.Body.String())
	}
}

// A listing reads as S3's: names relative to the listed prefix, folders as
// prefixes one level down, and flat with a marker for every folder. Temp
// files never show, and the cursor pages by name.
func TestAListingReadsAsABucketsDoes(t *testing.T) {
	g := newRig(t)
	for _, k := range []string{"r/a.csv", "r/b.csv", "r/sub/c.csv"} {
		g.put("", k, []byte("x"), true)
	}
	if err := os.WriteFile(filepath.Join(g.dir, "r", tempPrefix+"x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := g.signer("")
	page, err := s.ListObjects(context.Background(), "", "r/", "", "/", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if names(page) != "a.csv b.csv | sub/" {
		t.Errorf("delimited = %q", names(page))
	}
	page, _ = s.ListObjects(context.Background(), "", "r/", "", "", 1000)
	if names(page) != "a.csv b.csv sub/ sub/c.csv | " {
		t.Errorf("flat = %q", names(page))
	}
	page, _ = s.ListObjects(context.Background(), "", "r/", "", "", 2)
	if names(page) != "a.csv b.csv | " || page.NextCursor != "b.csv" {
		t.Errorf("page one = %q cursor %q", names(page), page.NextCursor)
	}
	page, _ = s.ListObjects(context.Background(), "", "r/", "b.csv", "", 2)
	if names(page) != "sub/ sub/c.csv | " {
		t.Errorf("page two = %q", names(page))
	}
	page, _ = s.ListObjects(context.Background(), "", "nothing/", "", "/", 10)
	if len(page.Objects)+len(page.Prefixes) != 0 {
		t.Errorf("a folder that does not exist listed %q", names(page))
	}
	// A file listed as a folder is empty, as a prefix naming an object is on
	// S3 — not an error the API turns into a 502 (M1, 2026-11-04).
	for _, delim := range []string{"/", ""} {
		page, err = s.ListObjects(context.Background(), "", "r/a.csv/", "", delim, 10)
		if err != nil || len(page.Objects)+len(page.Prefixes) != 0 {
			t.Errorf("a file listed as a folder (delimiter %q) = %v, %v", delim, page, err)
		}
	}
}

// A folder marker is an empty-only rmdir: a folder with something in it stays.
func TestDeletingAFolderRemovesOnlyAnEmptyOne(t *testing.T) {
	g := newRig(t)
	g.put("", "a/f", []byte("x"), true)
	del := func(key string) int {
		signed, _ := g.signer("").PresignDelete(context.Background(), "", key, time.Hour)
		return g.do(signed, nil).Code
	}
	if c := del("a/"); c != http.StatusNoContent {
		t.Errorf("rm a non-empty folder = %d", c)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "a", "f")); err != nil {
		t.Error("a non-empty folder's contents went")
	}
	if c := del("a/f"); c != http.StatusNoContent {
		t.Errorf("rm a file = %d", c)
	}
	if c := del("a/"); c != http.StatusNoContent {
		t.Errorf("rm an empty folder = %d", c)
	}
	if _, err := os.Stat(filepath.Join(g.dir, "a")); err == nil {
		t.Error("the empty folder is still there")
	}
}

// One subject has at most four transfers open at once.
func TestASubjectHoldsAtMostFourTransfers(t *testing.T) {
	g := newRig(t)
	var releases []func()
	for i := 0; i < maxConcurrent; i++ {
		rel, ok := g.store.admit("user-1")
		if !ok {
			t.Fatalf("transfer %d refused", i+1)
		}
		releases = append(releases, rel)
	}
	if _, ok := g.store.admit("user-1"); ok {
		t.Error("a fifth transfer was admitted")
	}
	if _, ok := g.store.admit("user-2"); !ok {
		t.Error("another subject was refused")
	}
	releases[0]()
	if _, ok := g.store.admit("user-1"); !ok {
		t.Error("a slot was not given back")
	}
}

func names(p *artifact.ObjectPage) string {
	var o, f []string
	for _, x := range p.Objects {
		o = append(o, x.Name)
	}
	f = append(f, p.Prefixes...)
	return strings.Join(o, " ") + " | " + strings.Join(f, " ")
}

func temps(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), tempPrefix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// HasObject answers "would a GET of this be served": a regular file inside the
// scope. A folder, a missing name, a name outside the scope and a scope folder
// nothing has been written to are all no — never an error a storage link would
// read as the store not answering.
func TestHasObjectIsARegularFileInsideTheScope(t *testing.T) {
	g := newRig(t)
	if err := os.MkdirAll(filepath.Join(g.dir, "teams", "a"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.dir, "teams", "a", "r.pdf"), []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		scope, key string
		want       bool
	}{
		{"", "teams/a/r.pdf", true},
		{"teams/a", "teams/a/r.pdf", true},
		{"", "teams/a", false},
		{"", "teams/a/missing.pdf", false},
		{"teams/b", "teams/a/r.pdf", false},
		{"teams/b", "teams/b/r.pdf", false},
		{"", "teams/a/r.pdf/under-a-file", false},
	} {
		got, err := g.signer(c.scope).HasObject(ctx, "", c.key)
		if err != nil || got != c.want {
			t.Errorf("HasObject(scope %q, %q) = %v, %v; want %v", c.scope, c.key, got, err, c.want)
		}
	}
}

// A flat listing at the scope root keeps a dot-file's own dot: from "." the
// walk yields ".env", and trimming "." as a string made it "env" (AS-5).
func TestAFlatListAtTheRootKeepsDotNames(t *testing.T) {
	g := newRig(t)
	for _, k := range []string{".env", ".cache/x", "env", "team/.env", "team/env"} {
		if c := g.put("", k, []byte("x"), true).Code; c >= 300 {
			t.Fatalf("put %s = %d", k, c)
		}
	}
	page, err := g.signer("").ListObjects(context.Background(), "", "", "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(page); got != ".cache/ .cache/x .env env team/ team/.env team/env | " {
		t.Errorf("flat at the root = %q", got)
	}
	// A prefix equal to a non-empty scope starts at "." too.
	page, err = g.signer("team").ListObjects(context.Background(), "", "team/", "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(page); got != ".env env | " {
		t.Errorf("flat at the scope = %q", got)
	}
}

// A FIFO an app planted answers 404 at once rather than hanging the open, and
// the person's transfer slots all come back (AS-8).
func TestAFIFOIsNotFoundAndHoldsNoSlot(t *testing.T) {
	g := newRig(t)
	if err := syscall.Mkfifo(filepath.Join(g.dir, "pipe"), 0o660); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxConcurrent; i++ {
		done := make(chan int, 1)
		go func() { done <- g.get("", "pipe").Code }()
		select {
		case code := <-done:
			if code != http.StatusNotFound {
				t.Fatalf("GET of a FIFO = %d, want 404", code)
			}
		case <-time.After(time.Second):
			t.Fatal("GET of a FIFO hung")
		}
	}
	rel, ok := g.store.admit("user-1")
	if !ok {
		t.Fatal("a transfer after the FIFO GETs was refused")
	}
	rel()
}

// An upload's temp file a killed process left is swept by a listing once it is
// a day old, and a fresh one — an upload in flight — stays. A folder holding
// only a stale temp can be deleted (AS-11).
func TestTempSweepRemovesOnlyStaleUploads(t *testing.T) {
	g := newRig(t)
	g.put("", "r/a.csv", []byte("x"), true)
	stale, fresh := filepath.Join(g.dir, "r", tempPrefix+"stale"), filepath.Join(g.dir, "r", tempPrefix+"fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTemp)
	os.Chtimes(stale, old, old)

	for _, delim := range []string{"/", ""} {
		if _, err := g.signer("").ListObjects(context.Background(), "", "r/", "", delim, 100); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a stale temp survived a listing: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("an upload in flight was swept: %v", err)
	}

	// A folder holding only a stale temp.
	if err := os.MkdirAll(filepath.Join(g.dir, "empty"), 0o770); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(g.dir, "empty", tempPrefix+"x")
	os.WriteFile(left, []byte("partial"), 0o600)
	os.Chtimes(left, old, old)
	signed, _ := g.signer("").PresignDelete(context.Background(), "", "empty/", time.Hour)
	g.do(signed, nil)
	if _, err := os.Stat(filepath.Join(g.dir, "empty")); !os.IsNotExist(err) {
		t.Errorf("a folder holding only a stale temp was not removed: %v", err)
	}
}
