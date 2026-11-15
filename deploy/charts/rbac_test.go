package charts

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// **Every Kubernetes verb the backend calls is one both RBAC sources grant.**
//
// This is the guard for a failure nothing else can see. A fake clientset
// enforces no RBAC, so every unit test in `internal/kube` passes against
// permissions the real cluster refuses; `helm template` renders a Role happily
// whatever is in it; and a first-ever create of anything succeeds, because the
// call that needs the missing verb is on the *second* path. The result is a
// binary that works on the install it was built on and fails on the next one.
//
// It has already happened, in the commit this test arrived with. Two guards
// were shipped reading a claim and a volume back — the only way to tell a live
// name from one still being deleted — while both Roles granted `create` and
// `delete` and no `get`. Every re-provision then failed with a forbidden,
// including the ordinary one that had worked for a year, and the storage
// resource was left recorded with no binding anywhere: the same unbacked state
// the read-back was added to prevent, by a different road.
//
// Textual on both sides, for `appself_contract_test.go`'s reason: reaching for
// a Kubernetes API type would mean building the client set, which is heavier
// than the failure warrants, and the failure is not subtle.
func TestEveryVerbTheBackendCallsIsGrantedByBothRBACSources(t *testing.T) {
	used := verbsTheBackendCalls(t)
	if len(used) < 10 {
		t.Fatalf("only %d client calls found in internal/kube; the pattern has stopped "+
			"matching and this test is comparing against nothing", len(used))
	}

	for _, src := range []struct{ name, path string }{
		{"the chart's Role", filepath.Join("toft", "templates", "rbac.yaml")},
		{"this box's manifests", filepath.Join("..", "k8s", "10-rbac.yaml")},
	} {
		if _, err := os.Stat(src.path); os.IsNotExist(err) && strings.HasPrefix(src.path, "..") {
			continue // the public source snapshot carries the chart, not this box's manifests
		}
		granted := verbsGranted(t, src.path)
		for _, need := range sortedPairs(used) {
			if granted[need] {
				continue
			}
			t.Errorf("%s does not grant %q on %q, which internal/kube calls.\n"+
				"    A missing verb is invisible until the path that needs it runs on a real "+
				"cluster, and then it is a forbidden in the middle of an operation that has "+
				"already changed something.", src.name, need.verb, need.resource)
		}
	}
}

type rule struct{ resource, verb string }

// The Go client's type name, as RBAC spells the resource. Only the ones the
// backend touches: a name not in here is a resource nothing calls yet, and it
// arrives with the first call that needs it.
var rbacResource = map[string]string{
	"Pods":                   "pods",
	"Services":               "services",
	"NetworkPolicies":        "networkpolicies",
	"Jobs":                   "jobs",
	"PersistentVolumeClaims": "persistentvolumeclaims",
	"PersistentVolumes":      "persistentvolumes",
	"StorageClasses":         "storageclasses",
}

// A client method, as RBAC spells the verb. `GetLogs` is the odd one and the
// reason this is a map rather than strings.ToLower: it is `get` on the
// `pods/log` **subresource**, which is a grant of its own.
var rbacVerb = map[string]string{
	"Create": "create",
	"Get":    "get",
	"List":   "list",
	"Delete": "delete",
	"Update": "update",
	"Patch":  "patch",
	"Watch":  "watch",
}

var clientCall = regexp.MustCompile(`client\.(?:CoreV1|BatchV1|NetworkingV1|StorageV1)\(\)\.\s*(\w+)\([^)]*\)\.\s*(\w+)\(`)
var whitespace = regexp.MustCompile(`\s+`)

func verbsTheBackendCalls(t *testing.T) map[rule]bool {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "kube")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	used := map[rule]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		// Collapsed, because a client call is routinely wrapped across lines.
		src := whitespace.ReplaceAllString(string(raw), " ")
		for _, m := range clientCall.FindAllStringSubmatch(src, -1) {
			resource, ok := rbacResource[m[1]]
			if !ok {
				t.Errorf("internal/kube calls %s, which this test cannot spell as an RBAC "+
					"resource — add it to rbacResource, and to both RBAC sources", m[1])
				continue
			}
			if m[2] == "GetLogs" {
				used[rule{resource + "/log", "get"}] = true
				continue
			}
			verb, ok := rbacVerb[m[2]]
			if !ok {
				t.Errorf("internal/kube calls %s.%s, whose RBAC verb this test does not know — "+
					"add it to rbacVerb", m[1], m[2])
				continue
			}
			used[rule{resource, verb}] = true
		}
	}
	return used
}

var resourcesLine = regexp.MustCompile(`resources:\s*\[([^\]]*)\]`)
var verbsLine = regexp.MustCompile(`verbs:\s*\[([^\]]*)\]`)

// verbsGranted reads the rules out of an RBAC document.
//
// A `resources:` line and the `verbs:` line under it, which is the shape both
// files are written in. Whether the grant is in the namespaced Role or the
// ClusterRole is deliberately not checked: this test answers "is the verb
// granted at all", and a cluster-scoped resource granted in a namespaced Role
// is a different mistake, one an install fails loudly on.
func verbsGranted(t *testing.T, path string) map[rule]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	granted := map[rule]bool{}
	lines := strings.Split(string(raw), "\n")
	var pending []string
	for _, line := range lines {
		if m := resourcesLine.FindStringSubmatch(line); m != nil {
			pending = splitList(m[1])
			continue
		}
		if m := verbsLine.FindStringSubmatch(line); m != nil {
			for _, r := range pending {
				for _, v := range splitList(m[1]) {
					granted[rule{r, v}] = true
				}
			}
			pending = nil
		}
	}
	if len(granted) == 0 {
		t.Fatalf("%s yielded no rules; the shape this reads has changed", path)
	}
	return granted
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedPairs(m map[rule]bool) []rule {
	out := make([]rule, 0, len(m))
	for r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].resource != out[j].resource {
			return out[i].resource < out[j].resource
		}
		return out[i].verb < out[j].verb
	})
	return out
}
