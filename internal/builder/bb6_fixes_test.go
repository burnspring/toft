package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/config"
)

// BB-065, sixth 2026-11-03 black-box list: toft check lists every problem a
// deploy would refuse, not the first of each section — a bad cron beside a bad
// zone, [runtime] and the notebook engine beside a missing notebook, and
// [static] beside [jobs] behind an [env] problem.
func TestCheckListsEveryProblemNotTheFirst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
		files    map[string]string
		want     []string
	}{
		{"cron and zone", "[jobs.a]\ncommand = \"true\"\nschedule = \"* * *\"\ntimezone = \"Mars/Base\"\n",
			map[string]string{"app.py": "print(1)\n", "requirements.txt": ""},
			[]string{"invalid_timezone", "invalid_schedule"}},
		{"runtime beside a missing notebook", "[runtime]\npython = \"3.13.18\"\n[notebook]\nengine = \"nbconvert\"\n",
			nil, []string{"unsupported_framework", "invalid_runtime"}},
		{"engine beside a missing notebook", "[notebook]\nengine = \"latex\"\n",
			nil, []string{"unsupported_framework", "invalid_notebook_engine"}},
		{"static and jobs behind env", "[static]\n[jobs.a]\ncommand = \"true\"\n[env]\nA = 5\n",
			map[string]string{"index.html": "<p>hi</p>\n"},
			[]string{"env_entry_invalid", "static_and_jobs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeManifest(t, tc.manifest)
			for name, body := range tc.files {
				os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
			}
			codes := map[string]bool{}
			var got []string
			for _, p := range RunPreflight(dir, "shared", nil, config.Envelope{}).Problems {
				code := p.Code
				if strings.Contains(p.Message, "a static app runs no commands") {
					code = "static_and_jobs"
				}
				codes[code] = true
				got = append(got, p.Code+": "+p.Message)
			}
			for _, want := range tc.want {
				if !codes[want] {
					t.Errorf("missing %s in %q", want, got)
				}
			}
		})
	}
}

// 2026-11-03 evening drills: a pin the install lacks and a malformed one are
// both said beside a missing notebook, whatever the [notebook] engine says.
func TestCheckSaysRuntimeBesideAMissingNotebook(t *testing.T) {
	for _, tc := range []struct{ name, manifest, want string }{
		{"absent version, quarto engine", "[runtime]\npython = \"3.99\"\n[notebook]\nengine = \"quarto\"\n", "runtime_version"},
		{"patch pin, bogus engine", "[runtime]\npython = \"3.11.2\"\n[notebook]\nengine = \"bogus\"\n", "invalid_runtime"},
		{"absent version, bogus engine", "[runtime]\npython = \"3.99\"\n[notebook]\nengine = \"bogus\"\n", "runtime_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeManifest(t, tc.manifest)
			var got []string
			found := false
			for _, p := range RunPreflight(dir, "shared", nil, config.Envelope{}).Problems {
				got = append(got, p.Code)
				found = found || p.Code == tc.want
			}
			if !found {
				t.Errorf("missing %s in %q", tc.want, got)
			}
		})
	}
}
