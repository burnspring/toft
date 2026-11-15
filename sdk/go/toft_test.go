package toft

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// platform stands up a fake /api/v1/app/* and points the SDK at it. Every test
// that talks to the platform goes through here, so the contract header and the
// bearer token are asserted once for all of them rather than test by test.
func platform(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want the app token", got)
		}
		if got := r.Header.Get(ContractHeader); got != AppContractVersion {
			t.Errorf("%s = %q, want %q — a skew has to be loud", ContractHeader, got, AppContractVersion)
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	SetTransport(server.URL, "test-token")
	t.Cleanup(func() { SetTransport("", "") })
	App.Refresh()
	resetIdentityCache()
	return server
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatal(err)
	}
}

// Off-platform every platform call names the variable that is missing. There is
// deliberately no dry-run mode: an SDK that printed to a console instead of
// paging a channel is a trap that only springs in production.
func TestOffPlatformEveryCallNamesWhatIsMissing(t *testing.T) {
	SetTransport("", "")
	t.Setenv(EnvAPIURL, "")
	t.Setenv(EnvAppToken, "")

	ctx := context.Background()
	calls := map[string]func() error{
		"App.Self":     func() error { _, err := App.Self(ctx); return err },
		"Data.Get":     func() error { _, err := Data.Get(ctx); return err },
		"Notify.Slack": func() error { _, err := Notify.Slack(ctx, "hello"); return err },
		"Jobs.Start":   func() error { _, err := Jobs.Start(ctx, "reconcile", StartOptions{}); return err },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s off-platform returned no error", name)
			continue
		}
		if !strings.Contains(err.Error(), EnvAPIURL) {
			t.Errorf("%s said %q, which does not name %s", name, err, EnvAPIURL)
		}
		// Coded, and never retryable: a statusless error used to read as a
		// network failure, so an agent's loop would retry a missing variable.
		if e, ok := AsError(err); !ok || e.Code() != "off_platform" || e.Retryable() {
			t.Errorf("%s off-platform: code/retryable = %v, want off_platform and false", name, err)
		}
	}
}

// Inside a job the app token is *meant* to be missing, and the generic sentence
// above is then a lie: it tells an author whose job has just done its real work
// that they are not running on the platform, which sends them looking for a
// broken deploy. The absence is architectural — a job workload holds no app
// token — so the refusal has to say so, and name the two ways round it.
func TestInsideAJobTheRefusalExplainsTheAbsence(t *testing.T) {
	SetTransport("", "")
	t.Setenv(EnvAPIURL, "http://platform.invalid")
	t.Setenv(EnvAppToken, "")
	t.Setenv(EnvRunID, "80ced99a")
	t.Setenv(EnvJobName, "archive")

	_, err := Data.Get(context.Background())
	if err == nil {
		t.Fatal("Data.Get inside a job with no app token returned no error")
	}
	for _, want := range []string{"archive", "80ced99a", "deliberately", "WriteOutputJSON"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "only works in an app running on the platform") {
		t.Errorf("the refusal still claims this is not the platform:\n%s", err)
	}
}

// Outside a run the same absence keeps the sentence it always had.
func TestOutsideARunTheOffPlatformSentenceStands(t *testing.T) {
	SetTransport("", "")
	t.Setenv(EnvAPIURL, "http://platform.invalid")
	t.Setenv(EnvAppToken, "")
	t.Setenv(EnvRunID, "")

	_, err := Data.Get(context.Background())
	if err == nil || !strings.Contains(err.Error(), "only works in an app running on the platform") {
		t.Errorf("off-platform refusal = %v", err)
	}
}

// Scratch is the one call that does not need the platform: a path is a path, so
// app code runs unchanged in local development.
func TestScratchFallsBackToTheSystemTempDirectory(t *testing.T) {
	t.Setenv(EnvScratchDir, "")
	dir, err := ScratchDir("uploads")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Errorf("ScratchDir = %q, want it under %q", dir, os.TempDir())
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("ScratchDir did not create %q: %v", dir, err)
	}
}

// A hostile session id cannot escape the scratch root, because the id is hashed
// before it becomes a directory name.
func TestASessionIdCannotTraverseOutOfScratch(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvScratchDir, root)

	dir, err := SessionScratchDir("../../etc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(root)) {
		t.Errorf("SessionScratchDir = %q, which is outside %q", dir, root)
	}
	if strings.Contains(dir, "..") {
		t.Errorf("SessionScratchDir = %q, which carries the caller's own segments", dir)
	}
}

// An empty session id would silently collapse every session into one directory,
// where one viewer's upload overwrites another's.
func TestAnEmptySessionIdIsRefused(t *testing.T) {
	if _, err := SessionScratchDir(""); err == nil {
		t.Error("an empty session id was accepted")
	}
}

// 128 bits, because the directory name is a hash of this string: anyone who can
// guess an id can name another viewer's directory.
func TestANewSessionIdIs128Bits(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil {
			t.Fatalf("NewSessionID = %q, which is not base64url: %v", id, err)
		}
		if len(raw) != 16 {
			t.Fatalf("NewSessionID decoded to %d bytes, want 16", len(raw))
		}
		if seen[id] {
			t.Fatalf("NewSessionID repeated %q", id)
		}
		seen[id] = true
	}
}

// Can takes a full capability or an integration name, because both are the
// obvious thing to type and guessing wrong should not be a silent false.
func TestCanAcceptsBothSpellingsAndReadsOnce(t *testing.T) {
	var reads int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reads, 1)
		writeJSON(t, w, 200, map[string]any{
			"id": "app_1", "slug": "recon", "mode": "isolated",
			"capabilities": []string{"email.send", "slack.post"},
		})
	})

	ctx := context.Background()
	for _, tc := range []struct {
		capability string
		want       bool
	}{
		{"email.send", true},
		{"email", true},
		{"slack", true},
		{"slack.file", false},
		{"storage", false},
	} {
		got, err := App.Can(ctx, tc.capability)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("Can(%q) = %v, want %v", tc.capability, got, tc.want)
		}
	}
	if reads != 1 {
		t.Errorf("the app view was fetched %d times, want once — it is read on hot paths", reads)
	}
}

// BasePath is the one value an app is told never to get wrong, and reporting ""
// as fact is how it got it wrong.
//
// The platform sends base_path; BASE_PATH in the environment is the fallback for
// an install older than the field. Without it the accessor answered "" — which
// is also the honest answer on an install that gives each app its own origin, so
// an app had nothing to check — and an app building its links from it emitted
// root-absolute URLs, a 404 on every link with nothing in any log saying why.
func TestBasePathFallsBackToTheInjectedVariable(t *testing.T) {
	for _, tc := range []struct {
		name, sent, env, want string
	}{
		{"the platform's answer wins", "/apps/reporter", "/apps/ignored", "/apps/reporter"},
		{"the variable answers where the platform is silent", "", "/apps/reporter", "/apps/reporter"},
		{"neither half says otherwise", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform(t, func(w http.ResponseWriter, r *http.Request) {
				body := map[string]any{"id": "app_1", "slug": "reporter"}
				if tc.sent != "" {
					body["base_path"] = tc.sent
				}
				writeJSON(t, w, 200, body)
			})
			t.Setenv(EnvBasePath, tc.env)
			App.Refresh()
			info, err := App.Self(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.BasePath != tc.want {
				t.Errorf("BasePath = %q, want %q", info.BasePath, tc.want)
			}
		})
	}
}

// The platform's reply to a PUT is metadata only, so a handle built from it
// alone carried a nil Data — the value Empty reports as "never written" — for a
// document that had just been written.
func TestAWriteReturnsWhatItWrote(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, 200, map[string]any{"data": map[string]any{"runs": 1.0}, "version": 1})
		case http.MethodPut:
			writeJSON(t, w, 200, map[string]any{"version": 2, "bytes": 12})
		}
	})

	ctx := context.Background()
	saved, err := Data.Set(ctx, map[string]any{"period": "2026-03"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Empty() {
		t.Error("a document that was just written read back as never-written")
	}
	if saved.Data["period"] != "2026-03" {
		t.Errorf("Data = %v, want the document that was written", saved.Data)
	}

	updated, err := Data.Update(ctx, func(s map[string]any) (map[string]any, error) {
		runs, _ := s["runs"].(float64)
		s["runs"] = runs + 1
		return s, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := updated.Data["runs"].(float64); got != 2 {
		t.Errorf("Update returned data %v, want the mutated document", updated.Data)
	}
}

// Never-written, cleared and an empty document are three tellable-apart states.
func TestNeverWrittenAndAnEmptyDocumentAreDifferent(t *testing.T) {
	var stored *map[string]any
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			body := map[string]any{"version": 0}
			if stored != nil {
				body = map[string]any{"data": *stored, "version": 3, "updated_at": "2026-08-24T10:00:00Z"}
			}
			writeJSON(t, w, 200, body)
		case http.MethodPut:
			var document map[string]any
			json.NewDecoder(r.Body).Decode(&document)
			stored = &document
			writeJSON(t, w, 200, map[string]any{"data": document, "version": 3})
		case http.MethodDelete:
			stored = nil
			writeJSON(t, w, 200, map[string]any{"deleted": true})
		}
	})

	ctx := context.Background()
	first, err := Data.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Empty() {
		t.Error("a never-written document did not read as empty")
	}

	if _, err := Data.Set(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	written, err := Data.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if written.Empty() {
		t.Error("an empty document read back as never-written")
	}
	if written.UpdatedAt.IsZero() {
		t.Error("the timestamp did not survive the read")
	}

	if deleted, err := Data.Clear(ctx); err != nil || !deleted {
		t.Fatalf("Clear = %v, %v", deleted, err)
	}
	if cleared, _ := Data.Read(ctx); !cleared.Empty() {
		t.Error("a cleared document did not read as empty")
	}
}

// A nil map would serialize as null and quietly wipe the app's settings, which
// is a wrong request rather than a refused one.
func TestANilDocumentIsRefusedLocally(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a nil document reached the platform")
	})
	if _, err := Data.Set(context.Background(), nil); err == nil {
		t.Error("a nil document was accepted")
	}
}

// Update re-reads and re-applies under a conflict, so the other writer's change
// survives instead of being overwritten.
func TestUpdateRetriesAConflictAndKeepsTheOtherWritersChange(t *testing.T) {
	version := 1
	document := map[string]any{"runs": float64(7)}
	var conflicts int
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, 200, map[string]any{"data": document, "version": version})
		case http.MethodPut:
			if conflicts == 0 {
				conflicts++
				// Somebody else got there first, and their change is in the
				// document the next read returns.
				version = 2
				document = map[string]any{"runs": float64(9)}
				writeJSON(t, w, 409, map[string]any{"code": "version_conflict", "version": version, "error": "stale"})
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			document = body
			version++
			writeJSON(t, w, 200, map[string]any{"data": document, "version": version})
		}
	})

	doc, err := Data.Update(context.Background(), func(s map[string]any) (map[string]any, error) {
		runs, _ := s["runs"].(float64)
		s["runs"] = runs + 1
		return s, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if runs, _ := doc.Data["runs"].(float64); runs != 10 {
		t.Errorf("runs = %v, want 10 — the other writer's 9 should have been the base", doc.Data["runs"])
	}
	if conflicts != 1 {
		t.Errorf("conflicts = %d, want the first write to have been refused", conflicts)
	}
}

// Several writers at once spend the app's burst between them, and the update is
// the write that must not be lost to it: a rate-limit refusal is waited out.
func TestUpdateWaitsOutTheRateLimit(t *testing.T) {
	limited := 0
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, 200, map[string]any{"data": map[string]any{"runs": 1}, "version": 1})
		case http.MethodPut:
			// More refusals than the update has attempts: a drained limiter
			// is waited out, and the waits do not spend them (rerun #3).
			if limited < DefaultUpdateAttempts+3 {
				limited++
				w.Header().Set("Retry-After", "0")
				writeJSON(t, w, 429, map[string]any{"code": "rate_limited", "retryable": true, "error": "slow down"})
				return
			}
			writeJSON(t, w, 200, map[string]any{"version": 2})
		}
	})

	doc, err := Data.Update(context.Background(), func(s map[string]any) (map[string]any, error) {
		s["runs"] = s["runs"].(float64) + 1
		return s, nil
	})
	if err != nil {
		t.Fatalf("Update under the rate limit = %v, want it waited out", err)
	}
	if doc.Version != 2 {
		t.Errorf("Version = %d, want 2", doc.Version)
	}
}

// More than thirty refusals are waited out — the bound is a deadline, as
// Python's and Node's are — and past the deadline the refusal is returned.
func TestUpdateWaitsOutRefusalsUpToItsDeadline(t *testing.T) {
	limited, refuseAll := 0, false
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, 200, map[string]any{"data": map[string]any{}, "version": 1})
		case http.MethodPut:
			if refuseAll || limited < 35 {
				limited++
				w.Header().Set("Retry-After", "0")
				writeJSON(t, w, 429, map[string]any{"code": "rate_limited", "retryable": true, "error": "slow down"})
				return
			}
			writeJSON(t, w, 200, map[string]any{"version": 2})
		}
	})
	mutate := func(s map[string]any) (map[string]any, error) { return s, nil }
	if _, err := Data.Update(context.Background(), mutate); err != nil {
		t.Fatalf("35 refusals: %v, want them waited out", err)
	}

	refuseAll = true
	old := RateLimitTimeout
	RateLimitTimeout = 200 * time.Millisecond
	defer func() { RateLimitTimeout = old }()
	_, err := Data.Update(context.Background(), mutate)
	if e, ok := AsError(err); !ok || e.Code() != "rate_limited" {
		t.Fatalf("past the deadline: %v, want rate_limited", err)
	}
}

// An update that runs out of attempts carries the last conflict's code, as
// Node's and Python's do: Go's give-up answered Code() "" (trytoft rerun #3).
func TestAnUpdateThatGivesUpIsCoded(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, 200, map[string]any{"data": map[string]any{}, "version": 1})
		case http.MethodPut:
			writeJSON(t, w, 412, map[string]any{"code": "version_conflict", "retryable": true, "version": 2, "error": "stale"})
		}
	})
	_, err := Data.Update(context.Background(), func(s map[string]any) (map[string]any, error) { return s, nil })
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("Update err = %v, want an *Error", err)
	}
	if e.Code() != "version_conflict" || !e.Retryable() {
		t.Errorf("give-up Code() = %q, Retryable() = %v; want version_conflict, true", e.Code(), e.Retryable())
	}
}

// A conflict app code drives itself has a type, so it can be caught rather than
// matched on a status other refusals also use.
func TestAConflictIsTyped(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 409, map[string]any{"code": "version_conflict", "version": 12, "error": "stale"})
	})
	_, err := Data.SetIf(context.Background(), map[string]any{"a": 1}, 3)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("SetIf err = %v, want a VersionConflictError", err)
	}
	if conflict.Version != 12 {
		t.Errorf("Version = %d, want the stored 12 so recovery is one read", conflict.Version)
	}
	if platformErr, ok := AsError(err); !ok || platformErr.Status != 409 {
		t.Error("errors.As did not reach the platform error underneath")
	}
}

// A miss names what is mounted: "no such resource" on its own sends somebody to
// their administrator to ask a question the process could already answer.
func TestAStorageMissNamesWhatIsMounted(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"reports","kind":"store","path":"/data/reports","access":"write"},
	                        {"name":"scratchpad","kind":"volume","path":"/data/vol","access":"read"}]`)

	_, err := Storage("ledgers")
	if err == nil || !strings.Contains(err.Error(), "reports") || !strings.Contains(err.Error(), "scratchpad") {
		t.Errorf("Storage miss said %v, want the mounted names", err)
	}

	if _, err := Store("scratchpad"); err == nil || !strings.Contains(err.Error(), "volume") {
		t.Errorf("Store on a volume said %v, want it named as the wrong kind", err)
	}

	reports, err := Store("reports")
	if err != nil {
		t.Fatal(err)
	}
	if reports.Path != "/data/reports" || reports.ReadOnly() {
		t.Errorf("Store(reports) = %+v", reports)
	}
}

// One resource, mapped twice. A mapping's identity is (resource, app, folder),
// so this is a shape an administrator can make on purpose — a read corpus and a
// write drop, at two paths in one workload — and both arrive under one name.
// The same scenarios the Python and Node SDKs pass.
const twoMappings = `[{"name":"drop","kind":"store","path":"/data/backup","access":"write","folder":"backup"},
                      {"name":"drop","kind":"store","path":"/data/drop","access":"read"}]`

func TestTheFolderReachesAppCode(t *testing.T) {
	t.Setenv(EnvStorage, twoMappings)

	mounts, err := Mounts()
	if err != nil {
		t.Fatal(err)
	}
	byFolder := map[string]Mount{}
	for _, mount := range mounts {
		byFolder[mount.Folder] = mount
	}
	if len(byFolder) != 2 {
		t.Fatalf("two mappings of one resource collapsed to %d: %+v", len(byFolder), mounts)
	}
	if got := byFolder["backup"]; got.Path != "/data/backup" || got.Access != "write" {
		t.Errorf("the backup mapping is %+v", got)
	}
}

// Returning the first match made the choice by the alphabetical order of paths
// an administrator chose — the coupling TOFT_STORAGE exists to prevent,
// arriving through the accessor. So the refusal names both folders and both
// levels, the level being usually why there are two.
func TestAnAmbiguousNameIsRefusedRatherThanResolved(t *testing.T) {
	t.Setenv(EnvStorage, twoMappings)

	_, err := Storage("drop")
	if err == nil {
		t.Fatal("Storage answered a name that is mounted twice")
	}
	for _, want := range []string{`folder "backup" at write`, `folder "" at read`, "StorageIn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q, want it to carry %q", err, want)
		}
	}
}

func TestNamingTheFolderAnswers(t *testing.T) {
	t.Setenv(EnvStorage, twoMappings)

	for _, c := range []struct{ folder, path string }{{"backup", "/data/backup"}, {"", "/data/drop"}} {
		mount, err := StorageIn("drop", c.folder)
		if err != nil {
			t.Fatalf("StorageIn(drop, %q): %v", c.folder, err)
		}
		if mount.Path != c.path {
			t.Errorf("StorageIn(drop, %q).Path = %q, want %q", c.folder, mount.Path, c.path)
		}
	}
	if mount, err := StoreIn("drop", "backup"); err != nil || mount.Path != "/data/backup" {
		t.Errorf("StoreIn(drop, backup) = %+v, %v", mount, err)
	}
}

func TestAFolderThatIsNotMountedNamesTheOnesThatAre(t *testing.T) {
	t.Setenv(EnvStorage, twoMappings)

	_, err := StorageIn("drop", "archive")
	if err == nil || !strings.Contains(err.Error(), `not at folder "archive"`) ||
		!strings.Contains(err.Error(), `folder "backup" at write`) {
		t.Errorf("the refusal is %v, want the folder named and the mounted ones listed", err)
	}
}

// The old-install case: every mount has an empty folder, so nothing is
// ambiguous and the fix cannot make a working install fail.
func TestAnInstallThatNamesNoFolderIsUnchanged(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"reports","kind":"volume","path":"/data/r","access":"full"}]`)

	mount, err := Storage("reports")
	if err != nil {
		t.Fatal(err)
	}
	if mount.Folder != "" || mount.Path != "/data/r" {
		t.Errorf("Storage(reports) = %+v", mount)
	}
	// `full` is the only level above read a volume has, and it is writable.
	if mount.ReadOnly() {
		t.Errorf("a volume at full reads as read-only: %+v", mount)
	}
}

// No mapped storage at all is a normal state app code branches on, not an error.
func TestNoMappedStorageIsNotAnError(t *testing.T) {
	t.Setenv(EnvStorage, "")
	mounts, err := Mounts()
	if err != nil || len(mounts) != 0 {
		t.Errorf("Mounts = %v, %v — want empty and no error", mounts, err)
	}
}

// The name is the caller's and the location is not, so a traversal segment would
// write into the app's own filesystem rather than into the storage it was given.
func TestPutRefusesANameThatWouldLandOutsideTheMount(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src.txt")
	os.WriteFile(source, []byte("rows"), 0o644)
	mount := Mount{Name: "reports", Kind: "store", Path: filepath.Join(root, "mount")}

	for _, name := range []string{"../escape.txt", "/absolute.txt", `windows\path.txt`, "a//b.txt", "nested/../../out.txt"} {
		if _, err := mount.Put(source, name); err == nil {
			t.Errorf("Put(%q) was accepted", name)
		}
	}

	landed, err := mount.Put(source, "2026/08.txt")
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(landed); string(body) != "rows" {
		t.Errorf("the file did not land at %s", landed)
	}

	// An empty name is not a refusal: it means the file's own name, which is
	// what the one-argument spelling in the other two SDKs does.
	own, err := mount.Put(source, "")
	if err != nil || filepath.Base(own) != "src.txt" {
		t.Errorf("Put with no name = %q, %v — want the source's own basename", own, err)
	}
}

// The mount is the enforcement; this check is only a better message.
func TestPutRefusesAReadOnlyMountWithTheReason(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src.txt")
	os.WriteFile(source, []byte("rows"), 0o644)

	mount := Mount{Name: "reports", Path: filepath.Join(root, "mount"), Access: "read"}
	_, err := mount.Put(source, "out.txt")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("Put on a read-only mount said %v", err)
	}
}

// Inside a job, which holds no app token, the transport sends with the run's
// own credential (ADR §108); the app's token wins wherever there is one.
func TestAJobSendsWithItsRunToken(t *testing.T) {
	t.Setenv("TOFT_APP_TOKEN", "")
	t.Setenv("TOFT_RUN_ID", "run-1")
	t.Setenv("TOFT_JOB_TOKEN", "jwt")
	if tok, err := appOrJobToken(); err != nil || tok != "jwt" {
		t.Fatalf("token = %q, %v; want the run token", tok, err)
	}
	t.Setenv("TOFT_APP_TOKEN", "tofta_x")
	if tok, _ := appOrJobToken(); tok != "tofta_x" {
		t.Errorf("token = %q; the app's own token wins", tok)
	}
}

// A refusal this SDK makes before any request is coded and never retryable.
// Without a body the status rule reads a statusless error as a network
// failure: code "" and retryable true, which is the answer an agent's loop
// retries forever.
func TestLocalRefusalsAreCoded(t *testing.T) {
	ctx := context.Background()
	calls := map[string]struct {
		code string
		call func() error
	}{
		"ViewerFor with no header": {"no_viewer", func() error { _, err := ViewerFor(http.Header{}); return err }},
		"SessionScratchDir(\"\")":  {"bad_request", func() error { _, err := SessionScratchDir(""); return err }},
		"Email with no recipient": {"no_recipient", func() error {
			_, err := Notify.Email(ctx, EmailMessage{Subject: "s", Text: "t"})
			return err
		}},
		"Status with no id": {"bad_request", func() error { _, err := Notify.Status(ctx, ""); return err }},
		"Data.Set(nil)":     {"bad_request", func() error { _, err := Data.Set(ctx, nil); return err }},
		"Start with no job": {"bad_request", func() error {
			_, err := Jobs.Start(ctx, " ", StartOptions{})
			return err
		}},
		"Get with no id": {"bad_request", func() error { _, err := Jobs.Get(ctx, ""); return err }},
		"WriteOutput with no location": {"no_output", func() error {
			return (&Run{}).WriteOutput(ctx, []byte("x"))
		}},
	}
	for name, c := range calls {
		err := c.call()
		e, ok := AsError(err)
		if !ok || e.Code() != c.code || e.Retryable() {
			t.Errorf("%s: %v — want code %q and not retryable", name, err, c.code)
		}
	}
}

// A run's output is fetched from the store on the transfer bound, not the
// platform round trip's: the presigned GET once inherited the 30-second
// context the redirect was read under, so a large output timed out at 30 s
// whatever uploadTimeout said.
func TestOutputDownloadIsNotCappedByThePlatformRoundTrip(t *testing.T) {
	saved := defaultTimeout
	defaultTimeout = 100 * time.Millisecond
	t.Cleanup(func() { defaultTimeout = saved })

	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the store fetch carried the app token")
		}
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("the whole report"))
	}))
	t.Cleanup(store.Close)
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, store.URL+"/object?sig=x", http.StatusFound)
	})

	got, err := (JobRun{ID: "run-1"}).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v — the store fetch was cut at the platform round trip's bound", err)
	}
	if string(got) != "the whole report" {
		t.Errorf("Output = %q", got)
	}
}

// A caller-supplied id is one path segment, escaped, never a route of its own:
// "a/../../data" reaches the sends route with the id it was, not the data route.
func TestCallerSuppliedIDsAreEscapedIntoOneSegment(t *testing.T) {
	var paths []string
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		writeJSON(t, w, http.StatusOK, map[string]any{"id": "x", "status": "sent"})
	})
	ctx := context.Background()
	_, _ = Notify.Status(ctx, "a/../../data?x=1")
	_, _ = (JobRun{ID: "r/../../data"}).Logs(ctx, 0)
	_ = (JobRun{ID: "r/../../data"}).Cancel(ctx)
	for _, p := range paths {
		if strings.Contains(p, "/../") || strings.HasSuffix(p, "/data") || strings.Contains(p, "?") {
			t.Errorf("an id escaped its segment: %s", p)
		}
	}
	if len(paths) != 3 {
		t.Fatalf("requests = %v", paths)
	}
}

// Tail reads every page of a run's log, not one per poll: the route answers a
// page at a time with no "more" marker, and a done run's last read dropped all
// but its next thousand lines — the end, where a failure explains itself.
func TestTailReadsEveryPage(t *testing.T) {
	const total, page = 2500, 1000
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			var after int
			fmt.Sscanf(r.URL.Query().Get("after"), "%d", &after)
			lines := []LogLine{}
			for seq := after + 1; seq <= total && len(lines) < page; seq++ {
				lines = append(lines, LogLine{Seq: seq, Line: fmt.Sprintf("line %d", seq)})
			}
			writeJSON(t, w, 200, map[string]any{"lines": lines})
			return
		}
		writeJSON(t, w, 200, map[string]any{"id": "r1", "status": "succeeded"})
	})
	seen := 0
	err := JobRun{ID: "r1", Status: RunRunning}.Tail(context.Background(), func(l LogLine) bool {
		seen++
		if l.Seq != seen {
			t.Fatalf("line %d has seq %d", seen, l.Seq)
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != total {
		t.Errorf("Tail visited %d lines, want %d", seen, total)
	}
}
