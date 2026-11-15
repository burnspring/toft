package staticserve

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Static is a strip-prefix framework: the proxy removes /apps/{slug} and the
// file server sees /. So `href="styles.css"`, `./img/x.png` and `../shared/a.css`
// all work, because the browser resolves them against /apps/{slug}/… — and
// `href="/styles.css"` does not, because it resolves to the platform's own
// origin and 404s there.
//
// Every strip-prefix framework has this constraint; static is the only one
// where the app author has no code in which to honour BASE_PATH. So the
// platform **warns and does not rewrite**. Injecting a `<base href>` or
// string-rewriting the body is what LegacyPrefixRewrite already does and
// already says is a mistake: it changes the app's bytes, silently re-anchors
// URLs the author wrote deliberately, and interacts badly with in-page anchors.
// A lint, in the log, at the moment the author is looking at the log, is the
// honest version.

// lintMaxFiles bounds how much of a documentation build is read for the lint. A
// site with more HTML than this has the same problem on its first files as on
// its last, and the lint's job is to name it once.
const lintMaxFiles = 200

// lintMaxBytes bounds one file. Enough for any hand-written page; a generated
// document larger than this is scanned as far as it goes.
const lintMaxBytes = 1 << 20

// lintMaxReported is how many offending URLs are listed. The fix is the same
// for all of them, so the list is evidence rather than a work item.
const lintMaxReported = 5

// rootAbsoluteURL matches src=/href= pointing at the origin root. The negative
// lookahead a Go regexp cannot express — excluding `//host/path`, which is
// protocol-relative and not this problem — is applied in the loop below.
var rootAbsoluteURL = regexp.MustCompile(`(?i)\b(src|href)\s*=\s*["'](/[^"']*)["']`)

// Lint reports root-absolute asset URLs in the HTML a root would publish, as
// finished log lines. Empty means nothing to say.
//
// The root is rootRel inside releaseDir (Config.RootRel), opened through the
// release's os.Root: the lint runs as the platform over a tree the app owns,
// and only regular files are read, so a `peek.html` linked to a host file
// prints nothing of it.
func Lint(releaseDir, rootRel string) []string {
	root, err := openDocRoot(releaseDir, rootRel)
	if err != nil {
		return nil
	}
	defer root.Close()

	type hit struct{ file, url string }
	var hits []hit
	scanned, truncated := 0, false

	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if Refused(d.Name()) && rel != "." {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !isHTML(d.Name()) {
			return nil
		}
		if scanned >= lintMaxFiles {
			truncated = true
			return fs.SkipAll
		}
		scanned++

		for _, u := range rootAbsoluteURLs(root, rel) {
			hits = append(hits, hit{file: filepath.ToSlash(rel), url: u})
		}
		return nil
	})

	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].url < hits[j].url
	})

	out := []string{fmt.Sprintf("    Warning: %s to a root-absolute URL, which will not resolve here.",
		plural(len(hits), "1 link or asset points", fmt.Sprintf("%d links or assets point", len(hits)))),
	}
	for i, h := range hits {
		if i == lintMaxReported {
			out = append(out, fmt.Sprintf("      … and %d more", len(hits)-lintMaxReported))
			break
		}
		out = append(out, fmt.Sprintf("      %s: %s", h.file, h.url))
	}
	out = append(out,
		"    This app is served under a prefix, so \"/styles.css\" asks the platform for",
		"    /styles.css rather than the app for it. Make these relative "+
			"(\"styles.css\", \"./img/x.png\").")
	if truncated {
		out = append(out, fmt.Sprintf("      (only the first %d HTML files were checked)", lintMaxFiles))
	}
	return out
}

func rootAbsoluteURLs(root *os.Root, rel string) []string {
	// Not following a link, and judged by the descriptor: an entry swapped
	// since the listing is refused or found not to be a regular file.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(f, lintMaxBytes))
	if err != nil {
		return nil
	}

	var out []string
	seen := map[string]bool{}
	for _, m := range rootAbsoluteURL.FindAllStringSubmatch(string(data), -1) {
		url := m[2]
		// Protocol-relative (`//cdn.example/x.js`) is an absolute URL to another
		// origin, which is a deliberate choice and not this mistake.
		if strings.HasPrefix(url, "//") || seen[url] {
			continue
		}
		seen[url] = true
		out = append(out, url)
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
