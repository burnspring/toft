// Package bundle turns a working directory into the archive `toft deploy` sends.
//
// The interesting half is what it leaves out, and who decides that.
//
// **`.gitignore` decides, and git is never shelled out to.** `git check-ignore`
// would be authoritative and is available in most of these directories — but not
// all of them, because a plain folder that is not a repository has to bundle
// too, and a CLI whose exclusion rules change depending on whether you have run
// `git init` is a support case nobody will enjoy. One implementation, both
// cases, with a table test.
package bundle

import (
	"bufio"
	"errors"
	"os"
	"path"
	"strings"
	"syscall"
)

// A pattern from one ignore file, compiled once.
//
// This implements the subset of gitignore syntax that appears in real
// repositories: leading `!` to negate, a trailing `/` to match directories
// only, a leading or embedded `/` to anchor to the file's own directory, and
// `*`, `?`, `**` inside a segment. What it does not implement — character
// classes — is rare enough in a `.gitignore` that guessing wrong about it would
// be a worse outcome than the occasional over-inclusion, and over-inclusion is
// the direction this errs in deliberately: a file that ships and should not have
// is visible in the build, while a file that was silently dropped is a build
// failure nobody can read.
type pattern struct {
	// base is the directory the pattern's file lived in, relative to the tree
	// root, with no trailing slash. "" for the root .gitignore.
	base string
	// glob is the pattern with its markers stripped.
	glob string
	// negate is `!` — the pattern un-ignores what an earlier one ignored.
	negate bool
	// dirOnly is a trailing `/` — matches a directory and everything in it,
	// never a file of that name.
	dirOnly bool
	// anchored is a `/` anywhere but the end: the pattern is relative to base
	// rather than matching at any depth.
	anchored bool
}

// Matcher answers "is this path excluded", from a stack of ignore files.
//
// **Order is the semantics.** Patterns are consulted in the order they were
// read — root file first, then deeper files, and within a file top to bottom —
// and the *last* one that matches wins, which is what makes `!` work. A matcher
// that returned on the first hit would silently ignore every negation in the
// tree.
type Matcher struct {
	patterns []pattern
}

// Add reads one ignore file's worth of lines, attributed to the directory it
// came from.
func (m *Matcher) Add(baseDir string, lines []string) {
	baseDir = strings.Trim(baseDir, "/")
	for _, raw := range lines {
		if p, ok := compile(baseDir, raw); ok {
			m.patterns = append(m.patterns, p)
		}
	}
}

// AddFile reads an ignore file if it is there. A missing file is not an error:
// most directories have none.
func (m *Matcher) AddFile(baseDir, path string) error {
	f, err := os.Open(path)
	// ENOTDIR is "absent" too: in a git worktree or a submodule `.git` is a
	// file, and `.git/info/exclude` under it fails with "not a directory" on
	// Unix, which os.IsNotExist does not count (security review 2026-11-14,
	// CS-2).
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	m.Add(baseDir, lines)
	return nil
}

func compile(base, raw string) (pattern, bool) {
	line := strings.TrimRight(raw, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}
	p := pattern{base: base}
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	// An escaped leading `#` or `!` — rare, and cheap to get right.
	if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if line == "" {
		return pattern{}, false
	}
	// A slash anywhere but the trailing position anchors the pattern to the
	// directory its file lives in. `build/output` matches only there; `build`
	// matches at any depth.
	if strings.Contains(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	p.glob = line
	return p, true
}

// Excluded reports whether a path is ignored. rel is slash-separated and
// relative to the tree root; isDir says whether it is a directory, which several
// pattern forms depend on.
func (m *Matcher) Excluded(rel string, isDir bool) bool {
	excluded := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			// A `build/` pattern does not match a *file* called build — but it
			// does match everything under a directory called build, and that is
			// handled by the walk pruning the directory rather than by testing
			// each child against a directory-only pattern.
			continue
		}
		if p.matches(rel, isDir) {
			excluded = !p.negate
		}
	}
	return excluded
}

func (p pattern) matches(rel string, isDir bool) bool {
	// A pattern only speaks about paths inside its own directory.
	if p.base != "" {
		if !strings.HasPrefix(rel, p.base+"/") {
			return false
		}
		rel = rel[len(p.base)+1:]
	}
	if p.anchored {
		// Segment by segment, always: an anchored one-segment glob — `/*`,
		// `/*.json` — went through the single-segment matcher with the whole
		// relative path, whose `*` crosses a slash, so the whitelist
		// `/*` + `!/src` dropped everything under src/ (code review
		// 2026-11-11, M2-1). In git a leading slash means "at this level".
		return pathMatch(strings.Split(p.glob, "/"), strings.Split(rel, "/"))
	}
	// Unanchored, which by the rule in compile means the pattern has no slash
	// in it at all: it matches a name at any depth. `**/foo` is not this case —
	// it contains a slash, so it is anchored, and its leading `**` is what
	// gives it the depth.
	return globMatch(p.glob, path.Base(rel))
}

// globMatch is gitignore's glob, not filepath.Match's.
//
// The difference that matters: `*` does not cross a `/`, and `**` does. Go's
// filepath.Match has no `**` at all and treats `*` as crossing nothing, which is
// right for one segment and silently wrong for `dist/**/*.map`.
func globMatch(pat, name string) bool {
	if pat == "**" {
		return true
	}
	if !strings.Contains(pat, "/") {
		return segmentMatch(pat, name)
	}
	return pathMatch(strings.Split(pat, "/"), strings.Split(name, "/"))
}

// pathMatch walks pattern segments against name segments, with `**` able to
// swallow any number of them.
func pathMatch(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			// A trailing `**` is everything *inside* the directory, not the
			// directory itself: `data/**` + `!data/keep` must keep data/keep,
			// which it cannot if the walk never enters data/ (code review
			// 2026-11-11, M2-5). git's rule, checked with check-ignore.
			return len(name) > 0
		}
		for i := 0; i <= len(name); i++ {
			if pathMatch(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 || !segmentMatch(pat[0], name[0]) {
		return false
	}
	return pathMatch(pat[1:], name[1:])
}

// segmentMatch is `*` and `?` within one path segment.
func segmentMatch(pat, name string) bool {
	// Walk both, remembering the last `*` so a failed match can backtrack to
	// it. Iterative rather than recursive: a pathological pattern against a
	// long name is somebody's file list, not an attack, but it should still
	// not be quadratic in the stack.
	var (
		p, n      int
		star      = -1
		starMatch int
	)
	for n < len(name) {
		switch {
		case p < len(pat) && (pat[p] == name[n] || pat[p] == '?'):
			p++
			n++
		case p < len(pat) && pat[p] == '*':
			star, starMatch = p, n
			p++
		case star >= 0:
			p = star + 1
			starMatch++
			n = starMatch
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}
