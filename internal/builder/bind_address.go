package builder

import (
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// The bind-address warning is advice for an app that picks its own address,
// and it was printed to every Go, Node and generic Python app — including the
// ones that read HOST and PORT exactly as it asks, whose authors then went
// looking for what they had done wrong (porting feedback 2026-11-03).
//
// **A reading, not a proof.** It asks whether the source names both variables
// the way each language reads the environment; an app that reads HOST and then
// binds something else passes, which is why what replaces the warning states
// what was seen rather than that the app is correct.

var (
	hostRead = regexp.MustCompile(`["']HOST["']|env\.HOST\b|\bHOST\s*=\s*os\.`)
	portRead = regexp.MustCompile(`["']PORT["']|env\.PORT\b`)
)

// Bounds on the walk: a tree larger than this is not read to the end, and an
// unfinished reading is the warning, never the all-clear.
const (
	bindScanMaxFiles = 4000
	bindScanMaxBytes = 1 << 20
)

// readsHostAndPort reports whether the source under dir, in files with one of
// exts, reads both HOST and PORT from the environment.
func readsHostAndPort(dir string, exts ...string) bool {
	want := map[string]bool{}
	for _, e := range exts {
		want[e] = true
	}
	var host, port bool
	files := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != dir && (strings.HasPrefix(name, ".") || name == "node_modules" ||
				name == "vendor" || name == "testdata" || name == "__pycache__" || name == "venv") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !want[filepath.Ext(path)] || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if files++; files > bindScanMaxFiles {
			return filepath.SkipAll
		}
		// Through the release's root, non-blocking and checked on the
		// descriptor: this runs after the app's build, in a tree its account
		// can rewrite between the walk and the open (security review
		// 2026-11-10, E-6).
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		f, err := openInRelease(dir, rel)
		if err != nil {
			return nil
		}
		raw, _ := io.ReadAll(io.LimitReader(f, bindScanMaxBytes))
		f.Close()
		host = host || hostRead.Match(raw)
		port = port || portRead.Match(raw)
		if host && port {
			return filepath.SkipAll
		}
		return nil
	})
	return host && port
}

// readsHostAndPortLine is what the build log says in the warning's place.
const readsHostAndPortLine = "    Reads HOST and PORT from the environment, as a service here has to."
