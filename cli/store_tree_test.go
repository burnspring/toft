package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

// fakeStoreInstall is the install and its bucket in one server: the API mints URLs
// pointing back at /bucket/, and /bucket/ stores bytes in a map. It enforces
// the two rules the tree commands lean on — If-None-Match on a write, and a
// trailing slash only ever arriving through the folder routes.
type fakeStoreInstall struct {
	mu      sync.Mutex
	objects map[string][]byte // store/name → bytes
	srv     *httptest.Server
	mints   []string
	stores  string // GET /api/v1/store's body; both stores at full when empty
}

func newFakeStoreInstall(t *testing.T) *fakeStoreInstall {
	t.Helper()
	f := &fakeStoreInstall{objects: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStoreInstall) session() *session {
	return &session{client: client.New(f.srv.URL, "tok", "toft-test")}
}

func (f *fakeStoreInstall) put(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = []byte(body)
}

func (f *fakeStoreInstall) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeStoreInstall) sign(w http.ResponseWriter, method, key string, headers map[string]string) {
	f.mu.Lock()
	f.mints = append(f.mints, method+" "+key)
	f.mu.Unlock()
	if headers == nil {
		headers = map[string]string{}
	}
	json.NewEncoder(w).Encode(signedRequest{URL: f.srv.URL + "/bucket/" + key, Method: method,
		Headers: headers, ExpiresAt: time.Now().Add(time.Hour)})
}

func (f *fakeStoreInstall) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/bucket/") {
		key := strings.TrimPrefix(path, "/bucket/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if _, exists := f.objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.objects[key] = body
		case http.MethodGet:
			body, ok := f.objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(body)
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}

	if path == "/api/v1/store" {
		w.Header().Set("Content-Type", "application/json")
		if f.stores != "" {
			io.WriteString(w, f.stores)
			return
		}
		io.WriteString(w, `{"stores":[{"slug":"reports","access":"full"},{"slug":"drop","access":"full"}]}`)
		return
	}
	rest := strings.TrimPrefix(path, "/api/v1/store/")
	store, rest, _ := strings.Cut(rest, "/")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case rest == "objects" && r.Method == http.MethodGet:
		// One level, folders as prefixes, unless flat=1 — as the install
		// answers.
		prefix := store + "/" + r.URL.Query().Get("path")
		flat := r.URL.Query().Get("flat") == "1"
		page := storeObjectPage{Objects: []storeObject{}}
		seen := map[string]bool{}
		f.mu.Lock()
		for k, v := range f.objects {
			if !strings.HasPrefix(k, prefix) || k == prefix {
				continue
			}
			rel := strings.TrimPrefix(k, prefix)
			if dir, _, nested := strings.Cut(rel, "/"); !flat && nested {
				if !seen[dir] {
					seen[dir] = true
					page.Prefixes = append(page.Prefixes, dir+"/")
				}
				continue
			}
			page.Objects = append(page.Objects, storeObject{Name: rel, Size: int64(len(v))})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(page)
	case rest == "objects" && r.Method == http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if strings.HasSuffix(req.Name, "/") {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"a name must not end in a slash"}`)
			return
		}
		f.sign(w, http.MethodPut, store+"/"+req.Name, map[string]string{"If-None-Match": "*"})
	case strings.HasPrefix(rest, "objects/"):
		name := strings.TrimPrefix(rest, "objects/")
		method := http.MethodGet
		if r.Method == http.MethodDelete {
			method = http.MethodDelete
		}
		f.sign(w, method, store+"/"+name, nil)
	case rest == "folders" && r.Method == http.MethodPost:
		var req struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.sign(w, http.MethodPut, store+"/"+strings.TrimSuffix(req.Path, "/")+"/", map[string]string{"If-None-Match": "*"})
	case strings.HasPrefix(rest, "folders/") && r.Method == http.MethodDelete:
		f.sign(w, http.MethodDelete, store+"/"+strings.TrimSuffix(strings.TrimPrefix(rest, "folders/"), "/")+"/", nil)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func treeEnv() *env {
	e, _ := testEnv()
	e.ctx = context.Background()
	return e
}

func copyTree(t *testing.T, f *fakeStoreInstall, from, to string, recursive, force, move bool) error {
	t.Helper()
	e := treeEnv()
	s := f.session()
	ops, p, err := planCopy(s, from, to, recursive, force, move)
	if err != nil {
		return err
	}
	if err := runOps(e, s, &globalFlags{}, ops, 3, "copied"); err != nil {
		return err
	}
	return finishMove(e, s, p)
}

// A tree goes up and comes back down as the same tree — nested files, an empty
// file, and an empty directory, which only survives because of its marker.
func TestATreeRoundTrips(t *testing.T) {
	f := newFakeStoreInstall(t)
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.MkdirAll(filepath.Join(src, "empty"), 0o755)
	os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644)
	os.WriteFile(filepath.Join(src, "a", "b", "deep.csv"), []byte("1,2"), 0o644)
	os.WriteFile(filepath.Join(src, "a", "__init__.py"), nil, 0o644)

	if err := copyTree(t, f, src, "toft://reports/2026/", true, false, false); err != nil {
		t.Fatalf("upload: %v", err)
	}
	want := []string{"reports/2026/a/__init__.py", "reports/2026/a/b/deep.csv",
		"reports/2026/empty/", "reports/2026/top.txt"}
	if got := f.keys(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("keys = %v, want %v", got, want)
	}

	dst := filepath.Join(t.TempDir(), "back")
	if err := copyTree(t, f, "toft://reports/2026", dst, true, false, false); err != nil {
		t.Fatalf("download: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "b", "deep.csv")); string(b) != "1,2" {
		t.Errorf("deep.csv = %q", b)
	}
	if st, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !st.IsDir() {
		t.Errorf("the empty folder did not come back as a directory: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dst, "a", "__init__.py")); err != nil || st.Size() != 0 {
		t.Errorf("the empty file did not come back: %v", err)
	}

	// A second download over the first refuses before anything moves.
	if err := copyTree(t, f, "toft://reports/2026/", dst, true, false, false); err == nil ||
		!strings.Contains(err.Error(), "--force") {
		t.Errorf("downloading over existing files = %v, want a refusal naming --force", err)
	}
	if err := copyTree(t, f, "toft://reports/2026/", dst, true, true, false); err != nil {
		t.Errorf("--force: %v", err)
	}
}

// Two names one case-insensitive disk holds as one file are refused before
// anything is fetched or deleted (security review 2026-11-15, CL-2).
func TestRecursiveDownloadRefusesNamesThatCollide(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/q1/Report.csv", "upper")
	f.put("reports/q1/report.csv", "lower")
	defer func(was bool) { foldsLocalNames = was }(foldsLocalNames)
	foldsLocalNames = true

	err := copyTree(t, f, "toft://reports/q1/", t.TempDir(), true, false, true)
	var ue *usageError
	if !errors.As(err, &ue) || ue.code != "name_collision" ||
		!strings.Contains(err.Error(), "Report.csv") || !strings.Contains(err.Error(), "report.csv") {
		t.Fatalf("err = %v, want name_collision naming both", err)
	}
	for _, m := range f.mints {
		if strings.HasPrefix(m, "GET ") || strings.HasPrefix(m, "DELETE ") {
			t.Errorf("an object was touched: %v", f.mints)
		}
	}
	if len(f.keys()) != 2 {
		t.Errorf("objects = %v, want both kept", f.keys())
	}

	// On a disk that tells them apart, the copy is legitimate.
	foldsLocalNames = false
	if err := copyTree(t, f, "toft://reports/q1/", t.TempDir(), true, false, false); err != nil {
		t.Errorf("case-sensitive disk: %v", err)
	}
}

// A destination written as a folder takes the source's own name.
func TestASingleFileLandsUnderAFolderDestination(t *testing.T) {
	f := newFakeStoreInstall(t)
	file := filepath.Join(t.TempDir(), "q1.csv")
	os.WriteFile(file, []byte("x"), 0o644)
	for _, tc := range []struct{ to, key string }{
		{"toft://reports/", "reports/q1.csv"},
		{"toft://reports/finance/", "reports/finance/q1.csv"},
		{"toft://reports/finance/renamed.csv", "reports/finance/renamed.csv"},
	} {
		if err := copyTree(t, f, file, tc.to, false, false, false); err != nil {
			t.Fatalf("%s: %v", tc.to, err)
		}
		if _, ok := f.objects[tc.key]; !ok {
			t.Errorf("cp to %s did not write %s: %v", tc.to, tc.key, f.keys())
		}
	}
}

// A rename is copy then delete, and the delete waits for the copy.
func TestMoveRenamesAndOnlyThenDeletes(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/draft.csv", "data")
	if err := copyTree(t, f, "toft://reports/draft.csv", "toft://reports/final.csv", false, false, true); err != nil {
		t.Fatalf("mv: %v", err)
	}
	if got := f.keys(); len(got) != 1 || got[0] != "reports/final.csv" {
		t.Fatalf("keys = %v, want only the new name", got)
	}

	// The copy refused (write access, name taken): the source stays.
	f.put("reports/a.csv", "a")
	f.put("reports/b.csv", "b")
	err := copyTree(t, f, "toft://reports/a.csv", "toft://reports/b.csv", false, false, true)
	if err == nil {
		t.Fatal("moving onto an existing name at write access succeeded")
	}
	if string(f.objects["reports/a.csv"]) != "a" || string(f.objects["reports/b.csv"]) != "b" {
		t.Errorf("a refused move changed something: %v", f.keys())
	}
}

// A move onto itself would copy the object, then delete the only copy.
func TestMoveOntoItselfIsRefused(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/a.csv", "a")
	for _, to := range []string{"toft://reports/a.csv", "toft://reports/"} {
		if err := copyTree(t, f, "toft://reports/a.csv", to, false, false, true); err == nil {
			t.Errorf("mv onto %s was not refused", to)
		}
	}
	if err := copyTree(t, f, "toft://reports/x/", "toft://reports/x", true, false, true); err == nil {
		t.Error("mv -r of a folder onto itself was not refused")
	}
	if string(f.objects["reports/a.csv"]) != "a" {
		t.Error("the object is gone")
	}
}

// A recursive move or copy of a folder into itself, or onto its own parent, is
// refused up front: the plan would write over objects later ops still read,
// and a/b/x was destroyed while "moved" was printed (code review 2026-11-11,
// M-1).
func TestMoveIntoItsOwnSubfolderIsRefused(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("a/x", "top")
	f.put("a/b/x", "nested")
	for _, pair := range [][2]string{
		{"toft://a/b/", "toft://a/"},
		{"toft://a/", "toft://a/b/"},
		{"toft://a/b/c/", "toft://a/b/"},
	} {
		for _, move := range []bool{true, false} {
			err := copyTree(t, f, pair[0], pair[1], true, false, move)
			if err == nil || !strings.Contains(err.Error(), "one inside the other") {
				t.Errorf("%s -> %s (move=%v): err = %v", pair[0], pair[1], move, err)
			}
		}
	}
	if string(f.objects["a/b/x"]) != "nested" || string(f.objects["a/x"]) != "top" {
		t.Error("an object was changed by a refused plan")
	}
	if got := nestedFolders("reports/2026", "archive/2026"); got {
		t.Error("siblings are not nested")
	}
}

// Moving a folder carries its markers and removes the source folder's own.
func TestMoveAFolderTakesItsMarkers(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/old/", "")
	f.put("reports/old/x.csv", "x")
	f.put("reports/old/empty/", "")
	if err := copyTree(t, f, "toft://reports/old/", "toft://reports/new/", true, false, true); err != nil {
		t.Fatalf("mv -r: %v", err)
	}
	want := "reports/new/empty/ reports/new/x.csv"
	if got := strings.Join(f.keys(), " "); got != want {
		t.Errorf("keys = %s, want %s", got, want)
	}
}

// rm -r deletes everything under the folder, markers included, and the
// folder's own marker last.
func TestRemoveTreeLeavesNothingBehind(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/2025/", "")
	f.put("reports/2025/a.csv", "a")
	f.put("reports/2025/q1/", "")
	f.put("reports/2025/q1/b.csv", "b")
	f.put("reports/2026/c.csv", "c")

	e := treeEnv()
	ref, _ := parseRemote("reports/2025/")
	if err := removeTree(e, f.session(), &globalFlags{yes: true}, ref, 2); err != nil {
		t.Fatalf("rm -r: %v", err)
	}
	if got := f.keys(); len(got) != 1 || got[0] != "reports/2026/c.csv" {
		t.Errorf("keys = %v, want only the sibling folder's object", got)
	}
}

// rm -r is confirmed, and refuses rather than waits when nobody can answer.
func TestRemoveTreeNeedsYesWhenUnattended(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/2025/a.csv", "a")
	ref, _ := parseRemote("reports/2025")
	if err := removeTree(treeEnv(), f.session(), &globalFlags{}, ref, 1); err == nil {
		t.Fatal("rm -r went ahead without --yes")
	}
	if len(f.keys()) != 1 {
		t.Error("something was deleted before the confirmation")
	}
}

// mkdir writes a marker, and a second mkdir is the folder existing, not a
// failure — at write access that second PUT is the store's 412.
func TestMkdirIsIdempotent(t *testing.T) {
	f := newFakeStoreInstall(t)
	e := treeEnv()
	existed, err := createMarker(e, f.session(), "reports", "2027")
	if err != nil || existed {
		t.Fatalf("first mkdir = (%v, %v)", existed, err)
	}
	existed, err = createMarker(e, f.session(), "reports", "2027")
	if err != nil || !existed {
		t.Fatalf("second mkdir = (%v, %v), want it reported as already there", existed, err)
	}
	if got := f.keys(); len(got) != 1 || got[0] != "reports/2027/" {
		t.Errorf("keys = %v", got)
	}
}

func TestParseRemoteTakesEverySpelling(t *testing.T) {
	for _, tc := range []struct {
		in          string
		store, path string
		dir         bool
	}{
		{"toft://reports", "reports", "", true},
		{"toft://reports/", "reports", "", true},
		{"toft://reports/a/b", "reports", "a/b", false},
		{"toft://reports/a/b/", "reports", "a/b", true},
		{"reports//a//b/", "reports", "a/b", true},
	} {
		got, err := parseRemote(tc.in)
		if err != nil || got.store != tc.store || got.path != tc.path || got.dir != tc.dir {
			t.Errorf("parseRemote(%q) = %+v, %v; want {%s %s %v}", tc.in, got, err, tc.store, tc.path, tc.dir)
		}
	}
	if _, err := parseRemote("toft:///x"); err == nil {
		t.Error("a ref with no store was accepted")
	}
}

// storeRun runs one `toft store` command line against the fake, through
// report, and answers the exit code and everything written.
func storeRun(t *testing.T, f *fakeStoreInstall, args ...string) (int, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("TOFT_URL", f.srv.URL)
	t.Setenv("TOFT_TOKEN", "toftu_test")
	e, buf := testEnv()
	e.ctx = context.Background()
	e.json = wantsJSON(args)
	code := report(e, cmdStore(e, args))
	return code, buf.String()
}

// rm deletes something or says why not — never "deleted" over nothing, which
// is what an object store's 204 for a missing key made it print. And it says
// so before it asks: unattended and without --yes, the refusal is about the
// name, not the flag.
func TestRemoveResolvesTheNameBeforeAskingOrDeleting(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/fold/x.txt", "x")
	f.put("reports/keep.txt", "k")

	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"rm", "--yes", "toft://reports/nothere.txt", "--json"}, `"code":"not_found"`},
		{[]string{"rm", "toft://reports/nothere.txt", "--json"}, `"code":"not_found"`},
		{[]string{"rm", "reports/fold", "--yes", "--json"}, `"code":"is_a_folder"`},
		{[]string{"rm", "reports/fold", "--json"}, `"code":"is_a_folder"`},
		{[]string{"rm", "-r", "--yes", "reports/nothing/", "--json"}, `"code":"not_found"`},
	} {
		code, out := storeRun(t, f, tc.args...)
		if code != exitUsage || !strings.Contains(out, tc.code) {
			t.Errorf("%v = exit %d:\n%s\nwant 2 with %s", tc.args, code, out, tc.code)
		}
	}
	if got := strings.Join(f.keys(), " "); got != "reports/fold/x.txt reports/keep.txt" {
		t.Errorf("a refused rm deleted something: %s", got)
	}

	code, out := storeRun(t, f, "rm", "reports/keep.txt", "--yes")
	if code != exitOK || !strings.Contains(out, "deleted  toft://reports/keep.txt") {
		t.Errorf("rm of a real object = %d:\n%s", code, out)
	}
	// -r of a file is the file.
	f.put("reports/one.txt", "1")
	if code, out := storeRun(t, f, "rm", "-r", "--yes", "reports/one.txt"); code != exitOK {
		t.Errorf("rm -r of an object = %d:\n%s", code, out)
	}
	if _, ok := f.objects["reports/one.txt"]; ok {
		t.Error("rm -r of an object left it")
	}
}

// The rm -r confirmation counts what the deletes will walk, markers included.
func TestRemoveTreeAsksAboutWhatItWillDelete(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/td/a.txt", "aa")
	f.put("reports/td/b.txt", "bb")
	f.put("reports/td/emptydir/", "")
	_, out := storeRun(t, f, "rm", "-r", "reports/td")
	if !strings.Contains(out, "delete 3 object(s)") {
		t.Errorf("the question does not count the marker:\n%s", out)
	}
	code, out := storeRun(t, f, "rm", "-r", "reports/td", "--yes")
	if code != exitOK || strings.Count(out, "deleted  toft://reports/td/") != 3 {
		t.Errorf("rm -r = %d, want three canonical lines:\n%s", code, out)
	}
	if strings.Contains(out, "without --yes") {
		t.Errorf("unexpected refusal:\n%s", out)
	}
}

// A refusal for --yes reads as one: the clause about the store keeping no copy
// does not end up attached to the flag.
func TestConfirmationsReadCorrectlyWhenRefused(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/x.txt", "x")
	_, out := storeRun(t, f, "rm", "reports/x.txt")
	if !strings.Contains(out, "delete toft://reports/x.txt (the store keeps no copy) without --yes") {
		t.Errorf("rm refusal reads:\n%s", out)
	}
}

// mkdir of a folder that is there says so and writes nothing, whatever the
// level — at full the marker's PUT is unconditional and cannot tell.
func TestMkdirOfAnExistingFolderSaysExists(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/td/emptydir/", "")
	f.put("reports/td/full/x.txt", "x")
	code, out := storeRun(t, f, "mkdir", "reports/td/emptydir", "toft://reports/td/full/", "reports/td/new")
	if code != exitOK {
		t.Fatalf("mkdir = %d:\n%s", code, out)
	}
	for _, want := range []string{"exists  toft://reports/td/emptydir/", "exists  toft://reports/td/full/",
		"created  toft://reports/td/new/"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, m := range f.mints {
		if strings.Contains(m, "emptydir") || strings.Contains(m, "full/") {
			t.Errorf("an existing folder was written: %s", m)
		}
	}
}

// get: a missing object is exit 2 in Toft's words, a local file is replaced
// only with --force, and flags may follow the operands.
func TestGetMissingClobberAndForce(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/a.txt", "new")
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.txt")

	code, out := storeRun(t, f, "get", "reports", "nope.txt", "--json")
	if code != exitUsage || !strings.Contains(out, `"code":"not_found"`) || strings.Contains(out, "answered 404") {
		t.Errorf("get of a missing object = %d:\n%s", code, out)
	}

	os.WriteFile(dest, []byte("old"), 0o644)
	code, out = storeRun(t, f, "get", "toft://reports/a.txt", "-o", dest)
	if code != exitUsage || !strings.Contains(out, "--force") {
		t.Errorf("get over a local file = %d, want 2 naming --force:\n%s", code, out)
	}
	if code, out = storeRun(t, f, "get", "reports", "a.txt", "-o", dest, "--force"); code != exitOK {
		t.Errorf("get --force = %d:\n%s", code, out)
	}
	if b, _ := os.ReadFile(dest); string(b) != "new" {
		t.Errorf("the file = %q, want the object's bytes", b)
	}
}

// -o - is the bytes on stdout, with no file called "-" left behind — and
// refused beside --json, which would put a document in the same stream.
func TestGetToStdout(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/a.txt", "the bytes")
	wd, _ := os.Getwd()
	dir := t.TempDir()
	os.Chdir(dir)
	defer os.Chdir(wd)

	code, out := storeRun(t, f, "get", "reports", "a.txt", "-o", "-")
	if code != exitOK || !strings.Contains(out, "the bytes") || strings.Contains(out, "downloaded") {
		t.Errorf("get -o - = %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "-")); err == nil {
		t.Error(`a file called "-" was written`)
	}
	if code, _ := storeRun(t, f, "get", "reports", "a.txt", "-o", "-", "--json"); code != exitUsage {
		t.Errorf("-o - with --json = %d, want 2", code)
	}
}

// put takes its flags after the operands, and prints the path ls would.
func TestPutTakesFlagsAfterOperands(t *testing.T) {
	f := newFakeStoreInstall(t)
	file := filepath.Join(t.TempDir(), "local.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	code, out := storeRun(t, f, "put", "reports", file, "--as", "in/renamed.txt")
	if code != exitOK || !strings.Contains(out, "uploaded  toft://reports/in/renamed.txt") {
		t.Errorf("put … --as = %d:\n%s", code, out)
	}
	if _, ok := f.objects["reports/in/renamed.txt"]; !ok {
		t.Errorf("keys = %v", f.keys())
	}
}

// ls --json is one shape, -r or not, and an empty store is empty arrays.
func TestListJSONIsOneShape(t *testing.T) {
	f := newFakeStoreInstall(t)
	for _, args := range [][]string{{"ls", "--json", "drop"}, {"ls", "-r", "drop", "--json"}} {
		code, out := storeRun(t, f, args...)
		if code != exitOK || strings.TrimSpace(out) != `{"folders":[],"objects":[]}` {
			t.Errorf("%v = %d %q", args, code, out)
		}
	}
	f.put("reports/a/b.txt", "b")
	f.put("reports/a/c/", "")
	_, out := storeRun(t, f, "ls", "-r", "reports/a", "--json")
	var doc struct {
		Folders []string      `json:"folders"`
		Objects []storeObject `json:"objects"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Folders) != 1 || len(doc.Objects) != 1 {
		t.Errorf("ls -r --json = %s (%v)", out, err)
	}
	_, out = storeRun(t, f, "ls", "-r", "reports/empty")
	if !strings.Contains(out, "Nothing in toft://reports/empty/ yet.") {
		t.Errorf("empty message = %q", out)
	}
}

// The install's refusals land on the contract's codes: a name it would not
// take is 2, the partition refusal is 3 (and so is the 409 an older install
// sent for it).
func TestStoreRefusalsExitOnTheContract(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusBadRequest, `{"error":"a name must not contain \"..\" segments","code":"bad_request"}`, exitUsage},
		{http.StatusForbidden, `{"error":"\"teams\" is partitioned","code":"outside_mapped_folders"}`, exitAuth},
		{http.StatusConflict, `{"error":"this is outside every folder an app is mapped to"}`, exitAuth},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		s := &session{client: client.New(srv.URL, "tok", "toft-test")}
		e, _ := testEnv()
		e.ctx = context.Background()
		_, err := downloadObject(e, s, "teams", "../../etc/passwd", filepath.Join(t.TempDir(), "x"), false)
		if got := report(e, err); got != tc.want {
			t.Errorf("%d = exit %d, want %d (%v)", tc.status, got, tc.want, err)
		}
		srv.Close()
	}
}

// A move needs `full` where it removes and `write` where it lands. Known short
// of either, it is refused before anything is copied — not copied and then
// left behind as two objects.
func TestMoveIsRefusedUpFrontWithoutFullAtTheSource(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.stores = `{"stores":[{"slug":"drop","access":"write"},` +
		`{"slug":"drop","folder":"mine","access":"full"},{"slug":"reports","access":"read"}]}`
	s := f.session()
	e := treeEnv()

	err := checkMoveAccess(s, "toft://drop/a.txt", "toft://drop/b.txt")
	if err == nil || !strings.Contains(err.Error(), "nothing was moved") {
		t.Fatalf("a move at write was not refused: %v", err)
	}
	if got := report(e, err); got != exitAuth {
		t.Errorf("exit %d, want %d", got, exitAuth)
	}
	if len(f.mints) != 0 {
		t.Errorf("something was signed before the refusal: %v", f.mints)
	}

	// A folder granted at full is full, whatever the store's own row says.
	if err := checkMoveAccess(s, "toft://drop/mine/a.txt", "./a.txt"); err != nil {
		t.Errorf("full on the folder was refused: %v", err)
	}
	// And the destination needs write.
	if err := checkMoveAccess(s, "toft://drop/mine/a.txt", "toft://reports/a.txt"); err == nil {
		t.Error("a destination at read was not refused")
	}
	// A store no row covers is the install's to answer.
	if err := checkMoveAccess(s, "toft://other/a.txt", "./a.txt"); err != nil {
		t.Errorf("an unlisted store was judged here: %v", err)
	}
}

// Replacing an object at `write` is a refusal by level, exit 3 — as `rm` is.
func TestOverwriteAtWriteExitsAsARefusal(t *testing.T) {
	e := treeEnv()
	err := objectStoreRefusal(&http.Response{StatusCode: http.StatusPreconditionFailed,
		Body: io.NopCloser(strings.NewReader(""))})
	if got := report(e, err); got != exitAuth {
		t.Errorf("exit %d, want %d", got, exitAuth)
	}
}

// An empty folder downloaded is a directory, and its line says so.
func TestALocalFolderLineEndsInASlash(t *testing.T) {
	o := op{kind: opMkdirLocal, srcStore: "drop", src: "tree/empty/", dst: filepath.Join("out", "empty")}
	if got := o.describe(); !strings.HasSuffix(got, string(filepath.Separator)) {
		t.Errorf("describe = %q", got)
	}
}

// A batch where nothing succeeded and every item was refused for one reason is
// that refusal — `cp` of one file outside a partition's folders exits as `put`
// does — while a mixed batch, or one with a success in it, is still the count.
func TestABatchRefusedOneWayIsThatRefusal(t *testing.T) {
	outside := func() error {
		return &client.Error{Status: http.StatusForbidden, Code: "outside_mapped_folders", Message: "outside"}
	}
	err := batchError([]error{outside()}, 1)
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "outside_mapped_folders" {
		t.Errorf("one refused item = %v, want the refusal itself", err)
	}
	if _, ok := batchError([]error{outside(), outside()}, 2).(*client.Error); !ok {
		t.Error("two items refused the same way should be the refusal")
	}
	mixed := []error{outside(), &client.Error{Status: http.StatusConflict, Code: "already_exists"}}
	if _, ok := batchError(mixed, 2).(*partialError); !ok {
		t.Error("two different refusals should be the count")
	}
	if _, ok := batchError([]error{outside(), nil}, 1).(*partialError); !ok {
		t.Error("a batch with a success in it should be the count")
	}
}

// The object store's 412 at `write` is this client's own reading of it, not
// an install refusal with a code — and a `cp` of one file onto an existing name
// still has to exit 3 already_exists, as `put` does (the trytoft drills).
func TestABatchOfExistingNamesIsThatRefusal(t *testing.T) {
	exists := func() error {
		return fmt.Errorf("a.csv → toft://drop/a.csv: %w", objectStoreRefusal(&http.Response{
			StatusCode: http.StatusPreconditionFailed, Body: io.NopCloser(strings.NewReader(""))}))
	}
	e, _ := testEnv()
	e.json = true
	if got := report(e, batchError([]error{exists()}, 1)); got != exitAuth {
		t.Errorf("one existing name exited %d, want %d", got, exitAuth)
	}
	if _, ok := batchError([]error{exists(), errors.New("boom")}, 2).(*partialError); !ok {
		t.Error("an existing name beside another failure should be the count")
	}
}

// -q drops narration, never the reason an item failed: the summary says "each
// is named above", and under -q it has to be.
func TestQuietKeepsTheFailureReason(t *testing.T) {
	out := ui.New(false, false, true)
	var stderr bytes.Buffer
	out.Stderr = &stderr
	out.Warn("narration")
	out.Failed("x.txt: outside every folder")
	if strings.Contains(stderr.String(), "narration") || !strings.Contains(stderr.String(), "outside every folder") {
		t.Errorf("stderr under -q = %q", stderr.String())
	}
}

// **Every item of a --json batch carries its refusal's code**, this client's
// own reading of the object store included: `already_exists` had none while
// `outside_mapped_folders` beside it did (drills rerun 2026-10-22 #9). And the
// batch that is only that refusal is exit 3 `kind: refused` — a refusal of
// state, as the install's own 409s at exit 3 are, not `auth`.
func TestABatchItemThatExistsCarriesItsCode(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("drop/up/a.txt", "old")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)

	code, out := storeRun(t, f, "cp", "-r", dir+"/", "toft://drop/up/", "--json")
	if code != exitFailed {
		t.Fatalf("exit %d, want %d (one of two failed):\n%s", code, exitFailed, out)
	}
	var items []opResult
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, `{"from"`) {
			var it opResult
			if err := json.Unmarshal([]byte(line), &it); err != nil {
				t.Fatalf("an item does not parse: %v\n%s", err, out)
			}
			items = append(items, it)
		}
	}
	found := false
	for _, it := range items {
		if it.Error != "" {
			found = true
			if it.Code != "already_exists" {
				t.Errorf("item %+v carries no already_exists code", it)
			}
		}
	}
	if !found {
		t.Fatalf("no item failed:\n%s", out)
	}

	code, out = storeRun(t, f, "cp", filepath.Join(dir, "a.txt"), "toft://drop/up/a.txt", "--json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var doc map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil {
		t.Fatalf("the last line is not the refusal document: %q", lines[len(lines)-1])
	}
	if code != exitAuth || doc["kind"] != "refused" || doc["code"] != "already_exists" {
		t.Errorf("exit %d, document %v", code, doc)
	}
}

// `toft store mv` from a store held below `full` is refused before anything is
// copied, exit 3, and now with a code: `insufficient_access`, `kind: refused`
// — the name the app SDKs give the same refusal.
func TestAMoveBelowFullIsACodedRefusal(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.stores = `{"stores":[{"slug":"drop","access":"write"}]}`
	f.put("drop/a.txt", "a")
	code, out := storeRun(t, f, "mv", "toft://drop/a.txt", "toft://drop/b.txt", "--json")
	if code != exitAuth || !strings.Contains(out, `"code":"insufficient_access"`) || !strings.Contains(out, `"kind":"refused"`) {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if len(f.mints) != 0 {
		t.Errorf("something was signed before the refusal: %v", f.mints)
	}
}

// accessRank is the server's storeAccessRank, which a separate module cannot
// import — so this reads it from the source beside it. A ladder that drifted
// would refuse a move the install allows, or start one it will not finish.
func TestAccessRankIsTheServersLadder(t *testing.T) {
	src, err := os.ReadFile("../internal/database/store_grants.go")
	if err != nil {
		t.Skipf("the server's source is not beside this module: %v", err)
	}
	m := regexp.MustCompile(`(?s)var storeAccessRank = map\[string\]int\{(.*?)\}`).FindSubmatch(src)
	if m == nil {
		t.Fatal("storeAccessRank not found in internal/database/store_grants.go")
	}
	server := map[string]int{}
	for _, pair := range regexp.MustCompile(`"(\w+)":\s*(\d+)`).FindAllSubmatch(m[1], -1) {
		n, _ := strconv.Atoi(string(pair[2]))
		server[string(pair[1])] = n
	}
	if !reflect.DeepEqual(server, accessRank) {
		t.Errorf("the CLI ranks %v, the server ranks %v", accessRank, server)
	}
}

// get takes the path ls prints with where to put it, in cp's order: a first
// word with a path after its store is the object, and the second is the
// destination — a folder it goes into, or a file. `get <store> <name>` keeps
// its meaning (drills 2026-10-28 #12, where the path was read as a store's
// name).
func TestGetTakesAnObjectPathAndADestination(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/backup/a.tar.gz", "the bytes")
	f.put("reports/b.txt", "bee")
	dir := t.TempDir()

	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"store/path into an existing folder", []string{"get", "reports/backup/a.tar.gz", dir}, filepath.Join(dir, "a.tar.gz")},
		{"toft:// into a folder not yet made", []string{"get", "toft://reports/backup/a.tar.gz", filepath.Join(dir, "new") + "/"},
			filepath.Join(dir, "new", "a.tar.gz")},
		{"store/path to a file", []string{"get", "reports/b.txt", filepath.Join(dir, "renamed.txt")}, filepath.Join(dir, "renamed.txt")},
		{"store and name, -o", []string{"get", "reports", "backup/a.tar.gz", "-o", filepath.Join(dir, "o.tar.gz")}, filepath.Join(dir, "o.tar.gz")},
		{"toft://store/ and name", []string{"get", "toft://reports/", "b.txt", "-o", filepath.Join(dir, "slash.txt")}, filepath.Join(dir, "slash.txt")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code, out := storeRun(t, f, c.args...); code != exitOK {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if _, err := os.Stat(c.want); err != nil {
				t.Errorf("nothing at %s: %v", c.want, err)
			}
		})
	}

	// The destination given twice is a usage refusal, nothing written.
	code, out := storeRun(t, f, "get", "reports/b.txt", filepath.Join(dir, "x.txt"), "-o", filepath.Join(dir, "y.txt"))
	if code != exitUsage || !strings.Contains(out, "once") {
		t.Errorf("two destinations = %d:\n%s", code, out)
	}
}

// The install's hint survives the client's restatement of
// outside_mapped_folders as kind refused: printed under the sentence, and in
// the --json document (drills 2026-11-02 — it had been dropped).
func TestOutsideMappedFoldersKeepsTheInstallsHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"\"teams\" is partitioned","code":"outside_mapped_folders",`+
			`"hint":"write inside a folder an app is mapped to"}`)
	}))
	defer srv.Close()
	s := &session{client: client.New(srv.URL, "tok", "toft-test")}
	for _, asJSON := range []bool{false, true} {
		e, buf := testEnv()
		e.ctx = context.Background()
		e.json = asJSON
		_, err := downloadObject(e, s, "teams", "x", filepath.Join(t.TempDir(), "x"), false)
		if got := report(e, err); got != exitAuth {
			t.Fatalf("exit %d, want %d", got, exitAuth)
		}
		out := buf.String()
		if !strings.Contains(strings.ToLower(out), "write inside a folder an app is mapped to") {
			t.Errorf("json=%v: the hint is missing:\n%s", asJSON, out)
		}
		if asJSON && (!strings.Contains(out, `"kind":"refused"`) || !strings.Contains(out, `"code":"outside_mapped_folders"`)) {
			t.Errorf("the document lost its kind or code:\n%s", out)
		}
	}
}

// An op the run never reached — the context was cancelled first — is not a
// result row: a zero row read as a success under --json (code review
// 2026-11-11, M-4).
func TestUnreachedOpsAreNotResultRows(t *testing.T) {
	got := ranOnly([]opResult{{From: "a", To: "b"}, {}, {Error: "boom"}, {}})
	if len(got) != 2 || got[0].From != "a" || got[1].Error != "boom" {
		t.Fatalf("ranOnly = %+v, want the two rows that ran", got)
	}
}

// A symlink to a directory is copied as the directory it points at: Stat said
// "directory", WalkDir did not descend the link, and `cp -r out` of a linked
// folder planned nothing and exited 0 (code review 2026-11-11, M-3).
func TestPlanUploadFollowsASymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	os.WriteFile(filepath.Join(real, "a.csv"), []byte("1"), 0o644)
	os.MkdirAll(filepath.Join(real, "sub"), 0o755)
	os.WriteFile(filepath.Join(real, "sub", "b.csv"), []byte("2"), 0o644)
	link := filepath.Join(t.TempDir(), "out")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	ops, _, err := planUpload(link, remoteRef{store: "reports", path: "in", dir: true}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("planned %d op(s) through the link, want 2: %+v", len(ops), ops)
	}
}
