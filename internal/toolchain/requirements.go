package toolchain

import (
	"errors"
	"io/fs"
	"strings"
)

// A requirements.txt, read the way pip reads one, and the one rule that makes
// it Python's lockfile.
//
// **A hashed file is the lock Python already has.** `uv pip compile
// --generate-hashes` and `pip-compile --generate-hashes` both write every
// package the install needs, each pinned with `==` and carrying the hash of
// every distribution it may install. pip turns hash-checking on by itself for
// such a file and then refuses anything unlisted; uv does not — it installs an
// unlisted dependency unpinned and unhashed unless `--require-hashes` is passed.
// So the file is a lock on this platform only because the build passes that
// flag, and the scan may read it as the inventory only because the build does.
// HashedRequirements is the one rule both of them, and the runner, ask (ADR §39).

// Requirement is one requirement of a requirements.txt, its continuation lines
// joined.
type Requirement struct {
	// Name is the package as written, without extras or marker; normalizing
	// it is the caller's.
	Name string
	// Version is the exact `==` pin, and empty for anything else: a range, a
	// wildcard, arbitrary equality or no specifier at all.
	Version string
	// Hashed is a requirement carrying at least one --hash.
	Hashed bool
	// Via is the compile tools' `# via` annotation, one entry per source: a
	// package that depends on this one, or `-r requirements.in` for the file
	// that named it.
	Via []string
}

// Requirements is a requirements.txt.
type Requirements struct {
	Requirements []Requirement
	// Options is everything that is neither a requirement nor a --hash: an
	// include, a constraint, an editable install, a URL or local path, an
	// index. Each means the file is not the whole answer.
	Options []string
}

// Hashed is whether the file is locked: at least one requirement, every one
// pinned with `==` and hashed, and no option anywhere. Markers do not stop it —
// a marker-gated package is listed whether or not this interpreter installs it,
// which over-lists and never under-lists.
func (r Requirements) Hashed() bool {
	if len(r.Requirements) == 0 || len(r.Options) > 0 {
		return false
	}
	for _, req := range r.Requirements {
		if req.Name == "" || req.Version == "" || !req.Hashed {
			return false
		}
	}
	return true
}

// Pinned is whether every requirement is an exact `==` pin and nothing else is
// in the file. Hashes are decoration here.
func (r Requirements) Pinned() bool {
	if len(r.Requirements) == 0 || len(r.Options) > 0 {
		return false
	}
	for _, req := range r.Requirements {
		if req.Name == "" || req.Version == "" {
			return false
		}
	}
	return true
}

// Annotated is whether any requirement carries a `# via`.
func (r Requirements) Annotated() bool {
	for _, req := range r.Requirements {
		if len(req.Via) > 0 {
			return true
		}
	}
	return false
}

// HashedRequirements is whether the requirements.txt at path is fully pinned
// and hashed, which is what makes it a lock (see Requirements.Hashed). A file
// that is not there is not hashed.
func HashedRequirements(path string) (bool, error) {
	r, err := ReadRequirements(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.Hashed(), nil
}

// ReadRequirements reads the requirements.txt at path.
func ReadRequirements(path string) (Requirements, error) {
	raw, err := ReadRegularFile(path, MaxManifestBytes)
	if err != nil {
		return Requirements{}, err
	}
	return ParseRequirements(string(raw)), nil
}

// ParseRequirements reads a requirements.txt's text: backslash continuations
// joined, comments stripped except for the `# via` annotations, and each line
// split into its requirement and its options as pip splits it — at the first
// token that starts with a dash.
func ParseRequirements(raw string) Requirements {
	var out Requirements
	last := -1        // the requirement a `# via` belongs to
	viaBlock := false // inside a multi-line `# via` list

	for _, line := range logicalLines(raw) {
		content, comment, hasComment := splitComment(line)
		content = strings.TrimSpace(content)

		if content != "" {
			viaBlock = false
			if req, ok := parseRequirementLine(content, &out.Options); ok {
				out.Requirements = append(out.Requirements, req)
				last = len(out.Requirements) - 1
			}
		}
		if !hasComment || last < 0 {
			continue
		}
		text := strings.TrimSpace(comment)
		switch {
		case text == "via":
			viaBlock = true
		case strings.HasPrefix(text, "via "):
			viaBlock = false
			for _, v := range strings.Split(text[len("via "):], ",") {
				if v = strings.TrimSpace(v); v != "" {
					out.Requirements[last].Via = append(out.Requirements[last].Via, v)
				}
			}
		case viaBlock && content == "" && text != "" && strings.HasPrefix(comment, " "):
			out.Requirements[last].Via = append(out.Requirements[last].Via, text)
		default:
			viaBlock = false
		}
	}
	return out
}

// logicalLines joins backslash continuations. A comment line ends a
// continuation rather than joining it, as pip's join does, so a hash list's
// trailing `# via` stays a line of its own.
func logicalLines(raw string) []string {
	var out []string
	var cur strings.Builder
	open := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			if open {
				out = append(out, cur.String())
				cur.Reset()
				open = false
			}
			out = append(out, line)
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			open = true
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
		open = false
	}
	if open {
		out = append(out, cur.String())
	}
	return out
}

// splitComment splits a line at its comment: a `#` at the start or after
// whitespace, which is pip's rule — so a URL's `#egg=` fragment is not one.
func splitComment(line string) (content, comment string, ok bool) {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i], line[i+1:], true
		}
	}
	return line, "", false
}

// parseRequirementLine reads one logical line with content. A line that is
// not a requirement — an option, a URL, a path — is recorded in options and
// returns false.
func parseRequirementLine(content string, options *[]string) (Requirement, bool) {
	fields := strings.Fields(content)
	split := len(fields)
	for i, f := range fields {
		if strings.HasPrefix(f, "-") {
			split = i
			break
		}
	}
	spec := strings.Join(fields[:split], " ")
	var req Requirement
	for i := split; i < len(fields); i++ {
		f := fields[i]
		switch {
		case strings.HasPrefix(f, "--hash="):
			req.Hashed = true
		case f == "--hash":
			req.Hashed = true
			i++ // its value
		case strings.HasPrefix(f, "-"):
			*options = append(*options, strings.Join(fields[i:], " "))
			i = len(fields)
		}
	}
	if spec == "" {
		return Requirement{}, false
	}
	// A direct reference or a local path names no index release.
	if strings.Contains(spec, "://") || strings.Contains(spec, "@") ||
		strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
		*options = append(*options, spec)
		return Requirement{}, false
	}
	if i := strings.IndexByte(spec, ';'); i >= 0 {
		spec = strings.TrimSpace(spec[:i])
	}
	name := spec
	op := ""
	if i := strings.IndexAny(spec, "<>=!~"); i >= 0 {
		name, op = strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i:])
	}
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	req.Name = name
	if v, ok := strings.CutPrefix(op, "=="); ok && !strings.HasPrefix(v, "=") {
		v = strings.TrimSpace(v)
		if v != "" && !strings.ContainsAny(v, "*,<>!~= ") {
			req.Version = v
		}
	}
	return req, true
}
