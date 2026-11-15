// Package sources decides which git repositories this install will deploy
// from.
//
// It is deliberately pure: no database, no HTTP, no logging. The policy is a
// mode and a list of patterns, and every enforcement point on the platform
// answers the question by calling Policy.Allows — one resolver rather than
// five opinions, for the reason ResolveRetention and ResolveAttachment are
// each one function. Two call sites that could disagree about whether a source
// is allowed is a bug nobody finds until it matters.
//
// What this buys, and what it does not, is worth stating where the code lives
// rather than only in a document. It stops a well-meaning employee deploying an
// unvetted repository, a lookalike standing in for the real one, and an app
// quietly re-pointed at a fork after review. It does not stop a malicious
// insider, who pushes to an allowed repository; it does not stop the build,
// which runs arbitrary code out of an allowed repository and can fetch whatever
// it likes; and it does not stop dependency compromise, because npm and uv
// still reach public registries. **This constrains the provenance of an app,
// not its behavior**, and no copy anywhere may promise otherwise.
package sources

import (
	"fmt"
	"net/url"
	"strings"
)

// The three values the policy takes.
//
// ModeNone is a distinct value rather than "ModeAllowlist with an empty list"
// because an empty list is ambiguous between *not configured yet* and
// *deliberately nothing*, and the two want different copy and different audit
// entries. An empty list under ModeAllowlist is a valid, saveable state that
// warns the administrator that nothing can currently be deployed from git.
const (
	ModeAny       = "any"
	ModeAllowlist = "allowlist"
	ModeNone      = "none"
)

// ValidMode reports whether a value is one of the three.
func ValidMode(mode string) bool {
	switch mode {
	case ModeAny, ModeAllowlist, ModeNone:
		return true
	}
	return false
}

// Candidate is a repository URL reduced to the two things matching compares:
// a host, and the path split into segments. Both are already lowercased and
// stripped of the decorations that would otherwise let two spellings of one
// repository disagree about whether they are allowed.
type Candidate struct {
	// Host includes an explicit port when there is one that is not :443, since
	// a host on a second port is a different service.
	Host string
	// Segments is the path, split on "/", with any trailing ".git" removed.
	Segments []string
}

// String renders the candidate the way a refusal names it: host and path, no
// scheme. This is what an administrator would copy into a pattern.
func (c Candidate) String() string {
	if len(c.Segments) == 0 {
		return c.Host
	}
	return c.Host + "/" + strings.Join(c.Segments, "/")
}

// Normalize reduces a repository URL to a Candidate.
//
// The bypasses all live here, so each step is deliberate:
//
//   - the host is lowercased, a trailing "." is stripped (a fully-qualified
//     "github.com." is the same host), and an explicit ":443" is stripped
//     because it is the default for https and would otherwise be a spelling
//     that no pattern matches;
//   - a **non-ASCII host is refused outright**, which removes IDN homograph
//     attacks (a Cyrillic і in "github.com") without taking a punycode
//     dependency. The cost is an internationalized-domain git host, which is
//     not a real case; if one ever appears, x/net/idna normalization replaces
//     this rule;
//   - a trailing ".git" is stripped once, case-insensitively, because
//     "acme/x.git" and "acme/x" are the same repository to every host;
//   - segments are lowercased, because GitHub, GitHub Enterprise, GitLab and
//     Bitbucket all treat owner and repository paths case-insensitively and not
//     folding case would make "github.com/Acme/x" a bypass of
//     "github.com/acme/*". The residual risk — one entry admitting two
//     organizations on a hypothetical host that *did* distinguish them — is
//     recorded rather than defended against.
func Normalize(raw string) (Candidate, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Candidate{}, fmt.Errorf("repository URL is required")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return Candidate{}, fmt.Errorf("malformed repository URL")
	}
	if strings.ToLower(u.Scheme) != "https" {
		return Candidate{}, fmt.Errorf("only https:// repository URLs can be checked against the source policy")
	}
	if u.User != nil {
		return Candidate{}, fmt.Errorf("credentials in the repository URL are not supported")
	}

	host, err := normalizeHost(u.Host)
	if err != nil {
		return Candidate{}, err
	}

	segs, err := splitPath(u.Path)
	if err != nil {
		return Candidate{}, err
	}
	return Candidate{Host: host, Segments: segs}, nil
}

// normalizeHost is shared by candidates and patterns so the two can never
// disagree about what a host is.
func normalizeHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return "", fmt.Errorf("repository URL has no host")
	}
	if !isASCII(host) {
		// Named, not silently refused: the person reading this typed a host
		// that looks right and needs to be told why it is not.
		return "", fmt.Errorf("the host %q is not plain ASCII — internationalized domain names are "+
			"refused because two of them can look identical", raw)
	}
	host = strings.TrimSuffix(host, ".")
	host = strings.TrimSuffix(host, ":443")
	if host == "" {
		return "", fmt.Errorf("repository URL has no host")
	}
	if strings.Contains(host, "*") {
		return "", fmt.Errorf("'*' is not allowed in a host — a pattern is a host plus an optional path, " +
			"and only the final path segment may be '*'")
	}
	return host, nil
}

// splitPath applies steps 3 and 4 of the normalization: strip the slashes and
// one trailing ".git", split, and refuse the segments that would make a prefix
// comparison meaningless.
func splitPath(path string) ([]string, error) {
	// One slash off each end, not all of them: a doubled slash anywhere —
	// including at the front, where url.Parse leaves "//acme/x" — has to reach
	// the empty-segment check below rather than being tidied away. Two spellings
	// of one path that normalize differently is exactly how a prefix comparison
	// stops meaning what it says.
	p := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(path), "/"), "/")
	if p == "" {
		return nil, nil
	}
	if i := strings.LastIndex(strings.ToLower(p), ".git"); i >= 0 && i == len(p)-4 {
		p = p[:i]
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return nil, nil
	}

	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch s {
		case "":
			return nil, fmt.Errorf("the path has an empty segment")
		case ".", "..":
			return nil, fmt.Errorf("the path segment %q is not allowed", s)
		}
	}
	for i := range segs {
		segs[i] = strings.ToLower(segs[i])
	}
	return segs, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// Pattern is one allowlist entry: a host, an optional path prefix, and whether
// the prefix names a subtree or exactly one repository.
//
// The trailing "/*" is the whole grammar, and it exists in that shape to
// resolve a real ambiguity: on GitLab, "gitlab.internal/dept/team" could be a
// repository named team in group dept, or a subgroup holding repositories. The
// star says which.
type Pattern struct {
	Host string
	// Prefix is the path, already normalized. Empty for a bare host.
	Prefix []string
	// Subtree is true for a pattern written with a trailing "/*": this prefix
	// and everything beneath it. False means exactly this repository.
	Subtree bool
}

// ParsePattern reads an administrator's entry and returns it in the one
// canonical form that is stored and displayed.
//
// A bare host is canonicalized to "host/*", so a stored pattern is never
// ambiguous about which of the two forms it is. A "*" anywhere but as the final
// path segment is refused with a message naming the rule rather than treated as
// a literal — silently matching a repository actually named "*" would be a
// pattern that looks like a subtree and is not.
func ParsePattern(raw string) (Pattern, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Pattern{}, fmt.Errorf("a pattern cannot be empty")
	}
	// Pasting the URL from the browser is the obvious mistake, and stripping
	// the scheme is a canonicalization rather than a guess. Any other scheme is
	// refused: sources are https-only in every mode, and an entry written as
	// ssh:// would look like it permitted something it does not.
	if i := strings.Index(s, "://"); i >= 0 {
		if !strings.EqualFold(s[:i], "https") {
			return Pattern{}, fmt.Errorf("a pattern is a host and an optional path, not a URL — "+
				"write %q without the %s:// prefix", s[i+3:], s[:i])
		}
		s = s[i+3:]
	}
	if strings.ContainsAny(s, "?#@") {
		return Pattern{}, fmt.Errorf("a pattern is a host and an optional path — it cannot carry " +
			"credentials, a query or a fragment")
	}

	hostPart, pathPart, _ := strings.Cut(s, "/")
	host, err := normalizeHost(hostPart)
	if err != nil {
		return Pattern{}, err
	}

	p := Pattern{Host: host}
	pathPart = strings.Trim(pathPart, "/")
	if pathPart == "" {
		// A bare host means the whole host. Canonicalized on the way in so the
		// stored form always says which of the two kinds it is.
		p.Subtree = true
		return p, nil
	}

	segs := strings.Split(pathPart, "/")
	if last := segs[len(segs)-1]; last == "*" {
		p.Subtree = true
		segs = segs[:len(segs)-1]
	}
	for _, s := range segs {
		if strings.Contains(s, "*") {
			return Pattern{}, fmt.Errorf("'*' is only allowed as the final path segment — write " +
				"'host/org/*' for everything under an organization, or 'host/org/repo' for one repository")
		}
	}
	// The remaining segments go through the same normalization a candidate
	// does, so a pattern and the URL it is meant to match cannot disagree about
	// case or about a trailing .git.
	prefix, err := splitPath(strings.Join(segs, "/"))
	if err != nil {
		return Pattern{}, err
	}
	p.Prefix = prefix
	if len(p.Prefix) == 0 && !p.Subtree {
		// "host/" with nothing after it. Reachable only by writing the slash,
		// and it means the same thing as the bare host.
		p.Subtree = true
	}
	return p, nil
}

// String renders the canonical form: what is stored, and what is displayed.
func (p Pattern) String() string {
	out := p.Host
	if len(p.Prefix) > 0 {
		out += "/" + strings.Join(p.Prefix, "/")
	}
	if p.Subtree {
		out += "/*"
	}
	return out
}

// Matches reports whether one candidate is covered by this pattern.
//
// Hosts must be **equal** — never a suffix, never "contains", never "ends with
// a dot and then the pattern", because github.com.evil.internal must not match
// github.com. The path comparison is over parsed segments for the same reason:
// acme-evil must not match acme.
//
// The strict length inequality on a subtree is deliberate: github.com/acme/*
// matches acme/dashboard and acme/team/dashboard, but not the bare organization
// root github.com/acme, which is not a repository anyway.
func (p Pattern) Matches(c Candidate) bool {
	if p.Host != c.Host {
		return false
	}
	if !p.Subtree {
		return equalSegments(c.Segments, p.Prefix)
	}
	if len(c.Segments) <= len(p.Prefix) {
		return false
	}
	return equalSegments(c.Segments[:len(p.Prefix)], p.Prefix)
}

func equalSegments(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Policy is the resolved answer for the whole install: the mode, and the
// patterns that apply under ModeAllowlist.
type Policy struct {
	Mode     string    `json:"mode"`
	Patterns []Pattern `json:"-"`
}

// Restricted reports whether the policy is anything other than "any". It is the
// question the gallery and the settings copy ask — not "is there a list", since
// ModeNone has no list and restricts the most.
func (p Policy) Restricted() bool { return p.Mode != ModeAny }

// PatternStrings is the canonical list, for display and for a refusal.
func (p Policy) PatternStrings() []string {
	out := make([]string, 0, len(p.Patterns))
	for _, pat := range p.Patterns {
		out = append(out, pat.String())
	}
	return out
}

// Refusal is a source this install will not deploy from, and why.
//
// It is an error so that every enforcement point can return it unchanged, and
// it carries the patterns because a refusal that does not say what *would* work
// sends the developer to guess or to Slack. Internal hostnames are mild
// disclosure to signed-in staff on an internal platform, and the alternative
// fails the standing rule that a refusal a person can act on says why.
type Refusal struct {
	// Source is the normalized "host/path" when the URL could be read, and the
	// raw string when it could not.
	Source string
	Mode   string
	// Allowed is the canonical pattern list, empty under ModeNone.
	Allowed []string
	// Malformed is set when the URL could not be normalized at all — a
	// different sentence, because the install is not refusing a repository so
	// much as failing to understand one.
	Malformed string
}

func (r *Refusal) Error() string { return r.Message() }

// Message is the sentence shown wherever a source is refused: New App, the
// app's deploy control, the API's error field, and the first line of a build
// log. One wording, defined once, because four wordings of one rule is how a
// platform ends up sounding like it has four rules.
func (r *Refusal) Message() string {
	if r.Malformed != "" {
		return fmt.Sprintf("%s cannot be checked against this install's source policy: %s",
			r.Source, r.Malformed)
	}
	head := fmt.Sprintf("Administrators have blocked deploying from %s.", r.Source)
	switch {
	case r.Mode == ModeNone:
		return head + " This install does not deploy from git repositories."
	case len(r.Allowed) == 0:
		return head + " This install has an approved-source list and nothing is on it yet — " +
			"ask an administrator to add the repository."
	default:
		return head + " This install deploys only from approved sources: " + strings.Join(r.Allowed, ", ")
	}
}

// Allows reports whether a repository URL may be deployed, returning a *Refusal
// when it may not.
//
// The mode ordering matters. A non-ASCII host is refused in **every** mode,
// including "any", because a homograph is a hazard regardless of policy. Every
// other normalization problem is only consulted once there is a list to match
// against: under "any" the platform's existing validation and git itself are
// what answer a malformed URL, and this function is not the place to start
// refusing URLs that have always been accepted.
func (p Policy) Allows(rawURL string) error {
	c, err := Normalize(rawURL)
	if err != nil {
		if strings.Contains(err.Error(), "plain ASCII") {
			return &Refusal{Source: strings.TrimSpace(rawURL), Mode: p.Mode, Malformed: err.Error()}
		}
		if p.Mode == ModeAny {
			return nil
		}
		return &Refusal{Source: strings.TrimSpace(rawURL), Mode: p.Mode,
			Allowed: p.PatternStrings(), Malformed: err.Error()}
	}

	switch p.Mode {
	case ModeAny:
		return nil
	case ModeNone:
		return &Refusal{Source: c.String(), Mode: p.Mode}
	}

	for _, pat := range p.Patterns {
		if pat.Matches(c) {
			return nil
		}
	}
	return &Refusal{Source: c.String(), Mode: p.Mode, Allowed: p.PatternStrings()}
}

// Matches is Allows as a boolean, for the counting and preview paths where the
// sentence is not wanted.
func (p Policy) Matches(rawURL string) bool { return p.Allows(rawURL) == nil }
