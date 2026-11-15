package toolchain

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sha = "--hash=sha256:70761cfe03c773ceb22aa2f671b4757976145175cdfca038c02654d061d6dcc6"

// What both compile tools write is a lock: their output, generated once with
// --generate-hashes from `flask==3.1.0` and `werkzeug`, and committed.
func TestTheCompileToolsOutputIsHashed(t *testing.T) {
	for _, file := range []string{"requirements-uv.txt", "requirements-pip-compile.txt"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join("testdata", file)
			hashed, err := HashedRequirements(path)
			if err != nil {
				t.Fatal(err)
			}
			if !hashed {
				t.Fatal("a compiled, hashed file is a lock")
			}
			r, err := ReadRequirements(path)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			via := map[string][]string{}
			for _, req := range r.Requirements {
				got[req.Name] = req.Version
				via[req.Name] = req.Via
			}
			want := map[string]string{
				"blinker": "1.9.0", "click": "8.5.0", "flask": "3.1.0", "itsdangerous": "2.2.0",
				"jinja2": "3.1.6", "markupsafe": "3.0.4", "werkzeug": "3.1.9",
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("requirements = %v, want %v", got, want)
			}
			// Both spellings of `# via`: one line, and the indented list.
			for name, w := range map[string][]string{
				"flask":      {"-r two.in"},
				"blinker":    {"flask"},
				"markupsafe": {"jinja2", "werkzeug"},
				"werkzeug":   {"-r two.in", "flask"},
			} {
				if !reflect.DeepEqual(via[name], w) {
					t.Errorf("%s via = %q, want %q", name, via[name], w)
				}
			}
		})
	}
}

func TestHashedRequirements(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		hashed     bool
	}{
		{"the hash on the same line", "requests==2.32.3 " + sha + "\n", true},
		{"the hash on a continuation", "requests==2.32.3 \\\n    " + sha + "\n", true},
		{"the two-token spelling", "requests==2.32.3 --hash sha256:abc\n", true},
		// A universal lock carries them; the package is listed either way.
		{"a marker", "tomli==2.0.1 ; python_version < \"3.11\" \\\n    " + sha + "\n", true},
		{"extras", "uvicorn[standard]==0.27.0 \\\n    " + sha + "\n", true},
		{"comments and blank lines", "# header\n\nrequests==2.32.3 " + sha + "  # pinned\n", true},

		{"empty", "", false},
		{"only comments", "# nothing\n", false},
		{"no hashes", "requests==2.32.3\n", false},
		{"hashes on some lines", "requests==2.32.3 " + sha + "\nurllib3==2.2.3\n", false},
		{"a range", "requests>=2.32 " + sha + "\n", false},
		{"a wildcard", "requests==2.* " + sha + "\n", false},
		{"arbitrary equality", "requests===2.32.3 " + sha + "\n", false},
		{"an include", "-r base.txt\nrequests==2.32.3 " + sha + "\n", false},
		{"a constraint", "-c constraints.txt\nrequests==2.32.3 " + sha + "\n", false},
		{"an editable", "-e .\nrequests==2.32.3 " + sha + "\n", false},
		{"an index", "--index-url https://pypi.corp.example/simple\nrequests==2.32.3 " + sha + "\n", false},
		{"an extra index", "--extra-index-url https://pypi.corp.example/simple\nrequests==2.32.3 " + sha + "\n", false},
		{"find-links", "-f https://example.com/wheels\nrequests==2.32.3 " + sha + "\n", false},
		{"a URL", "pkg @ https://example.com/pkg-1.0.whl " + sha + "\n", false},
		{"a per-requirement option", "requests==2.32.3 " + sha + " --config-settings x=y\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "requirements.txt")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := HashedRequirements(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.hashed {
				t.Errorf("HashedRequirements = %v, want %v", got, tc.hashed)
			}
		})
	}
}

func TestHashedRequirementsWithNoFileIsNotHashed(t *testing.T) {
	got, err := HashedRequirements(filepath.Join(t.TempDir(), "requirements.txt"))
	if err != nil || got {
		t.Errorf("HashedRequirements = %v, %v; want false, nil", got, err)
	}
}

// A hash is decoration to a pin, and a continuation is one line. Reading the
// `\` as a directive made every compiled file "not pinned" (D8).
func TestAHashedContinuationIsPinned(t *testing.T) {
	r := ParseRequirements("certifi==2024.8.30 \\\n    " + sha + " \\\n    " + sha + "\n    # via requests\n")
	if !r.Pinned() {
		t.Errorf("a pinned, hashed file is pinned: %+v", r)
	}
	if len(r.Requirements) != 1 || r.Requirements[0].Version != "2024.8.30" {
		t.Errorf("requirements = %+v", r.Requirements)
	}
}

// pip's comment rule: a `#` after whitespace, so a URL fragment is not one.
func TestAURLFragmentIsNotAComment(t *testing.T) {
	r := ParseRequirements("pkg @ https://example.com/pkg.zip#egg=pkg\n")
	if len(r.Options) != 1 || r.Options[0] != "pkg @ https://example.com/pkg.zip#egg=pkg" {
		t.Errorf("options = %q", r.Options)
	}
}

// A direct reference compiles to `name @ url`, which is an option, not a
// requirement: the resolver reads Options to name it as a gap (code review
// 2026-11-11, E-5).
func TestADirectReferenceIsAnOptionNotARequirement(t *testing.T) {
	r := ParseRequirements("requests @ https://files.pythonhosted.org/x/requests-2.19.0-py2.py3-none-any.whl\nflask==3.0.0\n")
	if len(r.Requirements) != 1 || r.Requirements[0].Name != "flask" {
		t.Fatalf("requirements = %+v", r.Requirements)
	}
	if len(r.Options) != 1 || !strings.HasPrefix(r.Options[0], "requests @ https://") {
		t.Fatalf("options = %q, want the direct reference", r.Options)
	}
}
