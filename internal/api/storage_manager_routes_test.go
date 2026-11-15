package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Three routes write storage_mappings, and the list is the specification.
//
// The model is TestADeployKeyCannotReachCredentialsOrAuthorization: naming the
// enforcement points in a comment is a promise, and a walk of the real router
// is the mechanism. A fourth writer added without a decision is the same class
// of bug as a handler that forgets to authorize — the mapping would be written
// by whatever gate that route happened to have, and the ceiling and the stamp
// would both be somebody's else problem.
func TestOnlyThreeRoutesWriteStorageMappings(t *testing.T) {
	want := map[string]bool{
		// The administrator's, gated by requireStorageAdmin. Stamps `admin`.
		"PUT /api/v1/admin/storage/{id}/mappings/{appID}":    true,
		"DELETE /api/v1/admin/storage/{id}/mappings/{appID}": true,
		// The delegate's, gated by AuthorizeAttach — management on the resource
		// and ActionEditContent on the app, both. Stamps the delegation.
		"PUT /api/v1/apps/{appID}/storage/{resourceID}":    true,
		"DELETE /api/v1/apps/{appID}/storage/{resourceID}": true,
		// The revocation, which collects mappings rather than writing one.
		"DELETE /api/v1/admin/storage/{id}/managers/{subject}": true,
		// The same revocation, once per resource a default delegated, when a
		// default is removed with revoke_existing (ADR §149). Gated by
		// requireStorageAdmin; it writes no mapping of its own.
		"DELETE /api/v1/admin/storage/defaults/{subject}": true,
	}

	writers := storageMappingWriters(t)
	found := map[string]bool{}
	var extra []string
	for _, route := range routesCalling(t, writers) {
		found[route] = true
		if !want[route] {
			extra = append(extra, route)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("%d route(s) reach a storage_mappings writer and are not one of the three "+
			"enforcement points:\n\t%s\nIf that is deliberate, it belongs in AuthorizeAttach and "+
			"in docs/invariants.md § App storage, not in a new handler.",
			len(extra), strings.Join(extra, "\n\t"))
	}
	// The other direction, so the walk cannot pass by finding nothing.
	var missing []string
	for route := range want {
		if !found[route] {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d expected writer route(s) were not found on the router:\n\t%s",
			len(missing), strings.Join(missing, "\n\t"))
	}
}

// storageMappingWriters is the set of handler names whose body reaches a write
// of storage_mappings. Derived from the source rather than listed, so a fourth
// database function that writes the table is caught too.
func storageMappingWriters(t *testing.T) map[string]bool {
	t.Helper()
	// The database functions that mutate the table, from the package that owns
	// it. Anything calling one of these is a writer.
	dbWriters := []string{
		"PutStorageMapping", "DeleteStorageMapping", "DeleteStorageManagerAndMappings",
	}
	assertDBWritersAreComplete(t, dbWriters)

	handlerRe := regexp.MustCompile(`(?m)^func \(s \*Server\) (handle\w+)\(`)
	out := map[string]bool{}
	for _, path := range goFilesIn(t, ".") {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		text := string(src)
		locs := handlerRe.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range locs {
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := text[loc[1]:end]
			for _, w := range dbWriters {
				if strings.Contains(body, "s.db."+w+"(") {
					out[text[loc[2]:loc[3]]] = true
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no handler writing storage_mappings — this test is no longer testing anything")
	}
	return out
}

// assertDBWritersAreComplete fails when internal/database grows a function that
// mutates storage_mappings and is not in the list above. Without it, the walk
// would quietly stop covering a new writer.
func assertDBWritersAreComplete(t *testing.T, known []string) {
	t.Helper()
	mutation := regexp.MustCompile(`(?i)(INSERT INTO|UPDATE|DELETE FROM)\s+storage_mappings`)
	funcRe := regexp.MustCompile(`(?m)^func (?:\(db \*DB\) )?(\w+)\(`)
	for _, path := range goFilesIn(t, "../database") {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		text := string(src)
		locs := funcRe.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range locs {
			end := len(text)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			name := text[loc[2]:loc[3]]
			if !mutation.MatchString(text[loc[1]:end]) {
				continue
			}
			found := false
			for _, k := range known {
				if k == name {
					found = true
				}
			}
			// DeleteStorageManagersForSubject is the deleted-user and
			// deleted-group sweep. It writes the table and is deliberately not
			// a route: it is reached from DeleteUser and DeleteGroup, which
			// have their own gates, and it restarts nothing.
			//
			// ReconcileManifestStorage is the deploy's flip writing toft.toml's
			// [[storage]] — reached from the builder through
			// builder.StoragePlanner, never from a route, and judged by the
			// plan (validateAttach, the owner's and the deployer's
			// delegations) before it runs.
			//
			// restampManifestStorageTx is a transfer moving the stamps of
			// manifest mappings to the new owner: no mapping is added or
			// widened, and the transfer's own gate is ActionTransferOwnership.
			if !found && name != "DeleteStorageManagersForSubject" && name != "ReconcileManifestStorage" &&
				name != "restampManifestStorageTx" {
				t.Errorf("internal/database.%s mutates storage_mappings and is not in the writer "+
					"list this test walks — add it, or the router walk stops covering it", name)
			}
		}
	}
}

// routesCalling walks the real router and returns the routes whose handler is
// one of the named functions. handlerName is what `toft routes` already
// uses to recover a method value's name, so this test and the route inventory
// agree about what a handler is called.
func routesCalling(t *testing.T, handlers map[string]bool) []string {
	t.Helper()
	router := routerForWalk(t)

	var out []string
	if err := chi.Walk(router, func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		if !handlers[handlerName(h)] {
			return nil
		}
		out = append(out, method+" "+normalizeRoutePattern(route))
		return nil
	}); err != nil {
		t.Fatalf("walking router: %v", err)
	}
	return out
}

func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}
