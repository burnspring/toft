package sdk_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"toft/internal/runner"
	"toft/internal/runtime"
)

// **Every key the platform puts in `TOFT_STORAGE` is a key every SDK
// reads, and the comparison runs both ways.**
//
// The sibling guard in appself_contract_test.go pins the fields an SDK reads off
// `/api/v1/app/self`, and it says in its own words that it runs one way only: a
// key the platform sends and nobody reads is a field waiting for a reader. That
// is right there and wrong here, and the difference is worth stating because it
// is the whole reason this file exists.
//
// `/api/v1/app/self` is a wide payload with several readers — the SPA, the CLI,
// four SDKs — so a key no SDK wants is ordinary. `TOFT_STORAGE` has exactly
// one reader per language and it is the *whole* of what an app knows about its
// storage: there is no second route to ask, no field to fall back to, and no way
// for app code to see a key its SDK dropped. **A field nobody reads here is not
// waiting for a reader; it is a mount nobody can address.**
//
// Which is what happened. `folder` was added to the descriptor because two
// mounts of one resource share a name and the folder is what tells them apart —
// the reason is written on `runtime.Mount` itself — and no SDK carried the
// field. `storage(name)` returns the first match over an ordering by path, so an
// administrator using a documented capability made one of an app's two mounts
// unreachable, silently, in every language.
//
// The platform side is read by reflection rather than by a regex over the
// source, because the struct is in this module and a tag is a better oracle than
// a line. The SDK side is textual for the reason the other guards are: reaching
// for four language runtimes is heavier than the failure warrants.
func TestEveryMountKeyThePlatformSendsEverySDKReads(t *testing.T) {
	sent := descriptorKeys()
	if len(sent) == 0 {
		t.Fatal("runtime.MountDescriptor has no json tags — it was rewritten, and this guard now compares nothing")
	}

	for lang, r := range mountReaders {
		read, ok := r.keys(t)
		if !ok {
			continue
		}
		if len(read) == 0 {
			t.Errorf("%s: nothing matched %s inside %s — the reader was rewritten, and this "+
				"guard now says every key is missing", lang, r.key, r.file)
			continue
		}
		for _, key := range sent {
			if !contains(read, key) {
				t.Errorf("the platform sends %q in %s and the %s SDK never reads it. "+
					"An app in %s cannot see that field at all — and if it is what tells two "+
					"mounts of one resource apart, one of them is unaddressable",
					key, runner.EnvStorage, lang, lang)
			}
		}
		for _, key := range read {
			if !contains(sent, key) {
				t.Errorf("the %s SDK reads %q out of %s and the platform never sends it — "+
					"so it is always the zero value, reported as fact", lang, key, runner.EnvStorage)
			}
		}
	}
}

// descriptorKeys is the wire, from the struct that renders it.
func descriptorKeys() []string {
	var out []string
	tt := reflect.TypeOf(runtime.MountDescriptor{})
	for i := 0; i < tt.NumField(); i++ {
		tag := tt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	sort.Strings(out)
	return out
}

// reader is where one language turns the variable into its own type, and the
// spelling it uses to pull a key out.
//
// A region rather than the whole file, so an unrelated `raw.name` elsewhere
// cannot make a missing field look present. A region that stops matching is a
// hard failure rather than an empty set, for the same reason.
type reader struct {
	file   string
	from   string
	to     string
	key    *regexp.Regexp
	ignore []string
}

var mountReaders = map[string]reader{
	"python": {
		file: filepath.Join("python", "toft", "resources.py"),
		from: "def mounts()", to: "\ndef storage(",
		key: regexp.MustCompile(`entry\.get\("([a-z_]+)"`),
	},
	"node": {
		file: filepath.Join("node", "app.js"),
		from: "export class Mount {", to: "\n  /** Whether this mount",
		key: regexp.MustCompile(`raw\.([a-zA-Z_]+)`),
	},
	"go": {
		file: filepath.Join("go", "storage.go"),
		from: "func Mounts()", to: "\n// Storage resolves",
		key: regexp.MustCompile(`json:"([a-z]+)"`),
	},
	"r": {
		file: filepath.Join("r", "R", "storage.R"),
		from: "mounts <- function", to: "\nstorage <- function",
		key: regexp.MustCompile(`entry\[\["([a-z_]+)"\]\]`),
	},
}

// keys reports false when it could not look, so one language's absence does not
// hide the other three — the failure that made this guard necessary was a
// missing field in three SDKs at once.
func (r reader) keys(t *testing.T) ([]string, bool) {
	t.Helper()
	raw, err := os.ReadFile(r.file)
	if err != nil {
		t.Errorf("reading %s: %v", r.file, err)
		return nil, false
	}
	text := string(raw)
	start := strings.Index(text, r.from)
	if start < 0 {
		t.Errorf("%s: no %q — the reader was renamed and this guard is reading the wrong lines", r.file, r.from)
		return nil, false
	}
	rest := text[start:]
	if end := strings.Index(rest, r.to); end > 0 {
		rest = rest[:end]
	}
	var out []string
	for _, m := range r.key.FindAllStringSubmatch(rest, -1) {
		if !contains(out, m[1]) && !contains(r.ignore, m[1]) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out, true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
