package builder

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The two ends of the same rule, and a guard rather than shared code.
//
// `toft` is a second Go module and cannot import `toft/internal/...`, so
// what an upload loses on arrival and what `toft deploy` never sends are stated
// once on each side. This is the pattern `deploy/compose/compose_test.go`
// already uses on the published compose files, and it exists for the same
// reason: two lists that are supposed to agree, in two places, with nothing
// checking, is how the second one goes stale — and the way that failure is met
// is somebody's deploy behaving differently depending on which door they used.
//
// **The comparison deliberately covers the dependency and VCS half only.**
// `toft deploy` also drops `dist`, `build`, `out`, `.next`, `.nuxt` and
// `.svelte-kit` when the tree root has a `package.json`, and the server drops
// none of them ever — see ExcludedDirs. That is a decision, not drift, so a
// test demanding whole-list equality would be asserting something the design
// says is false, and the first person to meet it would "fix" it by making the
// server drop a static site's deliverable.
const cliBundlePath = "../../cli/internal/bundle/bundle.go"

// mapLiteralPattern pulls the quoted keys out of one
// `var name = map[string]bool{…}` block. Reading the source as text rather than
// importing it is the whole point: the import is what the module boundary
// forbids.
const mapLiteralPattern = `(?s)var\s+%s\s*=\s*map\[string\]bool\{(.*?)\n\}`

var quotedRe = regexp.MustCompile(`"([^"]+)"`)

func cliSet(t *testing.T, varName string) []string {
	t.Helper()
	src, err := os.ReadFile(cliBundlePath)
	if err != nil {
		t.Fatalf("reading the CLI's bundle rules: %v", err)
	}
	re := regexp.MustCompile(fmt.Sprintf(mapLiteralPattern, regexp.QuoteMeta(varName)))
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no `var %s = map[string]bool{…}` found — has the CLI's "+
			"bundle rule changed shape? This guard reads it as text, so it "+
			"fails loudly rather than quietly checking nothing.", cliBundlePath, varName)
	}
	var out []string
	for _, k := range quotedRe.FindAllSubmatch(m[1], -1) {
		out = append(out, string(k[1]))
	}
	sort.Strings(out)
	return out
}

func serverSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestTheTwoModulesAgreeOnWhatIsNotSource(t *testing.T) {
	cli := append(cliSet(t, "alwaysExcludedDeps"), cliSet(t, "alwaysExcludedVCS")...)
	sort.Strings(cli)

	if got := strings.Join(serverSet(ExcludedDirs), " "); got != strings.Join(cli, " ") {
		t.Errorf("the two ends of the rule have drifted.\n"+
			"  server (builder.ExcludedDirs): %s\n"+
			"  cli    (alwaysExcludedDeps + alwaysExcludedVCS): %s\n"+
			"Whichever side is right, both have to say it: an archive that "+
			"loses a directory when uploaded and keeps it when pushed is a "+
			"platform with two answers.", got, strings.Join(cli, " "))
	}
}

func TestTheTwoModulesAgreeOnPerFileLitter(t *testing.T) {
	cli := cliSet(t, "alwaysExcludedFiles")
	if got := strings.Join(serverSet(ExcludedFiles), " "); got != strings.Join(cli, " ") {
		t.Errorf("per-file exclusions have drifted.\n  server: %s\n  cli: %s",
			got, strings.Join(cli, " "))
	}
}

// The deliberate difference, asserted so that closing it is a decision somebody
// makes rather than a test somebody satisfies. If the CLI ever stops dropping
// build output this fails, and the right response is to read ExcludedDirs.
func TestBuildOutputIsTheCLIsAloneAndOnPurpose(t *testing.T) {
	output := cliSet(t, "alwaysExcludedOutput")
	if len(output) == 0 {
		t.Fatal("the CLI no longer names build output — the asymmetry this file " +
			"documents has gone, and the server's list should be reconsidered")
	}
	for _, name := range output {
		if ExcludedDirs[name] {
			t.Errorf("%q is build output and the server must never remove it: on the "+
				"way in, a `dist/` may be the only copy that exists, and dropping it "+
				"deploys an empty app whose build log says every step succeeded", name)
		}
	}
}
