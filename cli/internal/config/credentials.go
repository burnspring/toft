package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The credential, which lives in one file and never in a flag.
//
// `--token` puts a secret in ~/.bash_history and in every `ps` on a shared box,
// so there is no such flag and there will not be one. The only non-interactive
// ways in are `TOFT_TOKEN` in the environment, and
// `toft login --token-stdin` for the CI machine whose key a human minted in a
// browser.
//
// **This file is separate from config.toml and holds nothing but credentials.**
// A file with a secret in it gets different permissions, different backup
// advice and a different blast radius from a file people are meant to edit —
// and the moment the two are one file, somebody pastes theirs into a ticket.

// CredentialsFile is the on-disk shape:
//
//	default = "https://toft.corp"
//
//	[installs."https://toft.corp"]
//	alias = "prod"
//	token = "toftu_…"
//	user  = "ada@corp.com"
//
// The stored token is always an `toftu_` personal key — that is what `toft login`
// mints, and every token in this file acts as the person who signed in. A
// pipeline is the other case and does not use this file at all: it sets
// TOFT_TOKEN, and that may be an `toftd_` deploy key, which belongs to
// nobody and is bounded to creating and deploying apps. See IsDeployKey.
type CredentialsFile struct {
	Default  string                `toml:"default"`
	Installs map[string]Credential `toml:"installs"`

	// BrokeStaleLock reports that Save stepped over a lock left behind by a
	// killed `toft`. Surfaced rather than silent: a credentials file nobody can
	// write is worse than a race nobody hit, but stepping over another
	// process's lock should appear in the output the one time it matters.
	BrokeStaleLock bool

	// forgotten records the entries this process removed.
	//
	// Save re-reads the file inside the lock and merges, so that two `toft login`
	// runs cannot lose each other's entry — and a merge needs to tell "somebody
	// else wrote this while we waited" from "we deleted this". Without it a
	// union would quietly undo every --forget.
	forgotten map[string]bool

	// changed records the entries this process Set. Only those win the merge:
	// an entry merely loaded is the disk's, and writing back the copy this
	// process read replaced a key another `toft login` stored in between
	// (security review 2026-11-15, CL-7).
	changed map[string]bool
}

// Credential is one install's entry.
type Credential struct {
	Alias string `toml:"alias"`
	Token string `toml:"token"`
	User  string `toml:"user"`
	// ExpiresAt is when the install will stop accepting this key.
	//
	// Stored because the server already sends it and the client used to throw it
	// away, which made a ninety-day credential expire with no warning anywhere —
	// the first sign being a pipeline's exit 3 at whatever hour it runs.
	//
	// The zero value is *unknown*, not never: an entry written before this field
	// existed, or one stored by a probe that did not report an expiry. Nothing
	// is computed here — the CLI never decides a credential has expired, it
	// warns, and the install refuses.
	ExpiresAt time.Time `toml:"expires_at"`
	// Session is a signed-in browser session for the same install, stored
	// **beside** the key rather than in place of it: `toft login
	// --password-stdin` mints it, and only a command run with `--session`
	// presents it. Two credentials because they answer different questions — a
	// key operates apps and is refused every route that decides who may do what
	// (ADR §42, internal/api/keyroutes.go); a session is what an administrator's
	// browser holds, and reaches those routes because it is one. Keeping both
	// lets one machine be the tester and the administrator without either
	// overwriting the other.
	Session string `toml:"session,omitempty"`
	// SessionUser and SessionExpiresAt describe Session. A session lives hours,
	// not months, and the install's policy decides how many.
	SessionUser      string    `toml:"session_user,omitempty"`
	SessionExpiresAt time.Time `toml:"session_expires_at,omitempty"`
	// URL is filled in on read from the table key, so a caller holding one
	// entry knows which install it belongs to without carrying the key beside
	// it. Not written back — the key is the record.
	URL string `toml:"-"`
}

func credentialsPath() string {
	dir := userConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "credentials.toml")
}

// CredentialsPath is where the file lives, for messages that tell somebody
// which file to look at or delete.
func CredentialsPath() string { return credentialsPath() }

// LoadCredentials reads the file. A missing file is an empty set, not an error:
// somebody who has never run `toft login` is in a normal state, and the message
// they should get is "run toft login", from the command that needed it.
func LoadCredentials() (*CredentialsFile, error) {
	f := &CredentialsFile{Installs: map[string]Credential{}}
	if err := decodeTOMLFile(credentialsPath(), f); err != nil {
		return nil, err
	}
	if f.Installs == nil {
		f.Installs = map[string]Credential{}
	}
	for url, c := range f.Installs {
		c.URL = url
		f.Installs[url] = c
	}
	return f, nil
}

// Lookup finds an entry by URL or by alias.
//
// An alias is a convenience for the person with a staging install, and it is
// resolved here so that `--install staging` and `--install https://…` are the
// same code path — two lookups that could disagree about which install is meant
// would be a way to deploy to the wrong one.
func (f *CredentialsFile) Lookup(nameOrURL string) (Credential, bool, error) {
	if f == nil {
		return Credential{}, false, nil
	}
	want := NormalizeInstallURL(nameOrURL)
	for url, c := range f.Installs {
		if NormalizeInstallURL(url) == want {
			return c, true, nil
		}
	}

	// The alias arm collects every match rather than returning the first,
	// because returning the first is a lookup that disagrees with itself: the
	// runtime decides which entry a map yields, so the same alias resolves to
	// either install on either run. A deterministic tie-break — lowest URL,
	// first written — was the cheaper fix and is worse, because what it produces
	// is a deploy that *works*, against the wrong install.
	alias := strings.TrimSpace(nameOrURL)
	var matches []Credential
	for _, c := range f.Installs {
		if c.Alias != "" && strings.EqualFold(c.Alias, alias) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return Credential{}, false, nil
	case 1:
		return matches[0], true, nil
	}
	urls := make([]string, 0, len(matches))
	for _, c := range matches {
		urls = append(urls, c.URL)
	}
	sort.Strings(urls)
	return Credential{}, false, &AmbiguousAliasError{Alias: alias, URLs: urls}
}

// Save writes the file with 0600 permissions, creating the directory.
//
// Written by hand rather than through an encoder, for two reasons that both
// matter more than the twenty lines it costs: the file has comments in it that
// explain what it is to whoever opens it, and a round-trip through a marshaller
// would silently drop a key a newer `toft` had written, which on a shared home
// directory is somebody else's login.
func (f *CredentialsFile) Save() error {
	path := credentialsPath()
	if path == "" {
		return fmt.Errorf("no home directory to store credentials in — set XDG_CONFIG_HOME")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Asserted rather than assumed: MkdirAll does nothing to a directory that
	// already exists, so a ~/.config/toft that predates `toft` keeps whatever
	// mode it was made with. The file is 0600 either way, so this is
	// enumeration rather than exposure — but the same reasoning that keeps the
	// file's mode off a second syscall covers the directory it sits in.
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}

	// One writer at a time. Two `toft login` runs in parallel used to lose one
	// entry — whole-file read-modify-write, and the loser found out days later
	// in a different session — and two renames of the same fixed temporary name
	// could interleave. The hand-written encoder below exists so a newer `toft`'s
	// unknown key is not dropped from a shared home directory; concurrency
	// undid that guarantee by another route.
	release, brokeStale, err := lock(path + ".lock")
	if err != nil {
		return err
	}
	defer release()
	f.BrokeStaleLock = brokeStale

	// Re-read *inside* the lock, so the mutation lands on what is on disk rather
	// than on what was read before the wait.
	if err := f.mergeWithDisk(); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("# Toft CLI credentials. Written by `toft login`.\n")
	b.WriteString("#\n")
	b.WriteString("# Every token here acts as you: it can deploy, roll back and read anything\n")
	b.WriteString("# you can. Revoke one from Account -> API keys on the install it belongs to.\n")
	b.WriteString("# A session (toft login --password-stdin) is a signed-in browser's; it ends\n")
	b.WriteString("# when it expires, or with Sign out everywhere.\n")
	b.WriteString("# Nothing but credentials lives in this file; settings are in config.toml.\n\n")
	if f.Default != "" {
		fmt.Fprintf(&b, "default = %s\n\n", tomlString(f.Default))
	}

	urls := make([]string, 0, len(f.Installs))
	for url := range f.Installs {
		urls = append(urls, url)
	}
	sort.Strings(urls)

	// Refused here so the state is unreachable going forward, and Lookup's
	// refusal is left to deal only with files that already contain one.
	if err := f.checkAliases(urls); err != nil {
		return err
	}

	for _, url := range urls {
		c := f.Installs[url]
		fmt.Fprintf(&b, "[installs.%s]\n", tomlString(url))
		if c.Alias != "" {
			fmt.Fprintf(&b, "alias = %s\n", tomlString(c.Alias))
		}
		fmt.Fprintf(&b, "token = %s\n", tomlString(c.Token))
		if c.User != "" {
			fmt.Fprintf(&b, "user = %s\n", tomlString(c.User))
		}
		if !c.ExpiresAt.IsZero() {
			// A TOML offset date-time, unquoted, which is what decodes back
			// into a time.Time.
			fmt.Fprintf(&b, "expires_at = %s\n", c.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if c.Session != "" {
			fmt.Fprintf(&b, "session = %s\n", tomlString(c.Session))
			if c.SessionUser != "" {
				fmt.Fprintf(&b, "session_user = %s\n", tomlString(c.SessionUser))
			}
			if !c.SessionExpiresAt.IsZero() {
				fmt.Fprintf(&b, "session_expires_at = %s\n", c.SessionExpiresAt.UTC().Format(time.RFC3339))
			}
		}
		b.WriteString("\n")
	}

	return writePrivate(path, []byte(b.String()))
}

// tomlString is s as a TOML basic string. Go's %q is not one: it spells BEL
// and VT as \a and \v, which TOML does not have, so a user name the install
// answered with one made the whole file unreadable for every install, and an
// invalid UTF-8 byte as \xff, which TOML reads back as U+00FF (security review
// 2026-11-15, CL-5).
func tomlString(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// mergeWithDisk folds what is on disk into this file, for a writer that has been
// waiting on the lock.
//
// The entries this process Set win, every other entry is the disk's (an entry
// only in memory is kept), and entries this process forgot stay forgotten. That last clause is the whole reason `forgotten`
// exists: a plain union would make --forget a no-op whenever anything else had
// written the file.
func (f *CredentialsFile) mergeWithDisk() error {
	onDisk, err := LoadCredentials()
	if err != nil {
		return err
	}
	if f.Installs == nil {
		f.Installs = map[string]Credential{}
	}
	for url, c := range onDisk.Installs {
		if f.forgotten[url] {
			continue
		}
		if !f.changed[url] {
			f.Installs[url] = c
		}
	}
	// A default this process did not set is still somebody's default, unless it
	// names an install that is no longer here.
	if f.Default == "" && onDisk.Default != "" && !f.forgotten[NormalizeInstallURL(onDisk.Default)] {
		f.Default = onDisk.Default
	}
	if _, ok := f.Installs[NormalizeInstallURL(f.Default)]; !ok {
		f.Default = soleInstall(f.Installs)
	}
	f.forgotten = nil
	f.changed = nil
	return nil
}

// checkAliases refuses a file in which one alias names two installs.
func (f *CredentialsFile) checkAliases(urls []string) error {
	seen := map[string]string{}
	for _, url := range urls {
		alias := strings.ToLower(strings.TrimSpace(f.Installs[url].Alias))
		if alias == "" {
			continue
		}
		if other, ok := seen[alias]; ok {
			return &AmbiguousAliasError{Alias: alias, URLs: []string{other, url}}
		}
		seen[alias] = url
	}
	return nil
}

// AmbiguousAliasError is one alias naming two installs.
//
// It is a refusal rather than a tie-break because the failure a tie-break
// produces is a command that succeeds against the install nobody meant.
type AmbiguousAliasError struct {
	Alias string
	URLs  []string
}

// Code is the refusal's code in a --json document.
func (e *AmbiguousAliasError) Code() string { return "ambiguous_alias" }

func (e *AmbiguousAliasError) Error() string {
	return fmt.Sprintf("the alias %q names both %s in %s — rename or forget one of them, "+
		"or pass the URL instead", e.Alias, strings.Join(e.URLs, " and "), credentialsPath())
}

// Set records a credential and makes it the default when there is no other.
func (f *CredentialsFile) Set(c Credential) {
	url := NormalizeInstallURL(c.URL)
	c.URL = url
	if f.Installs == nil {
		f.Installs = map[string]Credential{}
	}
	f.Installs[url] = c
	if f.changed == nil {
		f.changed = map[string]bool{}
	}
	f.changed[url] = true
	if f.Default == "" || len(f.Installs) == 1 {
		f.Default = url
	}
}

// Forget drops an entry, and moves the default off it when it was the default.
// A default pointing at an install somebody has just signed out of is a command
// that fails for a reason nobody can see.
func (f *CredentialsFile) Forget(nameOrURL string) (bool, error) {
	c, ok, err := f.Lookup(nameOrURL)
	if err != nil {
		// An ambiguous alias is not "no such credential", and saying so would
		// send somebody to look for an entry that is there twice.
		return false, err
	}
	if !ok {
		return false, nil
	}
	delete(f.Installs, c.URL)
	if f.forgotten == nil {
		f.forgotten = map[string]bool{}
	}
	f.forgotten[c.URL] = true
	if NormalizeInstallURL(f.Default) == c.URL {
		f.Default = soleInstall(f.Installs)
	}
	return true, nil
}

// ForgetAll drops every stored credential.
//
// A method rather than two assignments at the call site, because a caller that
// empties the map itself leaves Save's merge with nothing to distinguish "this
// process deleted them" from "this process has not read them yet" — and the
// merge would put them all back.
func (f *CredentialsFile) ForgetAll() {
	if f.forgotten == nil {
		f.forgotten = map[string]bool{}
	}
	for url := range f.Installs {
		f.forgotten[url] = true
	}
	f.Installs = map[string]Credential{}
	f.Default = ""
}

// Session is the resolved answer to "which install, with which credential" —
// everything a command needs before it can make a call.
type Session struct {
	InstallURL string
	Token      string
	User       string
	// ExpiresAt is the stored credential's expiry, zero when unknown — an
	// environment token has none, and neither has an entry written before the
	// field existed.
	ExpiresAt time.Time
	// Origin says where the install came from, for a refusal that can be acted
	// on: "--install", "TOFT_URL", or the credentials file's default.
	Origin string
	// Cookie says Token is a session, presented as the platform's session
	// cookie rather than as a bearer key. Set only by ResolveAdminSession.
	Cookie bool
}

// DeployKeyPrefix marks the credential class that belongs to nobody.
//
// The CLI needs to recognise it for exactly one reason: a handful of commands
// ask "who am I", and a deploy key has no answer. Nothing here authenticates or
// authorizes on the prefix — the install does that against a stored hash — so
// this is a hint for better copy and never a check.
const DeployKeyPrefix = "toftd_"

// IsDeployKey reports whether this session is authenticated by a deploy key.
func (s *Session) IsDeployKey() bool { return IsDeployKeyToken(s.Token) }

// IsDeployKeyToken is the same question about a token that is not in a session
// yet — `toft login --token-stdin` has to know the class before it has anywhere
// to put it. One definition, so the two answers cannot drift apart.
func IsDeployKeyToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), DeployKeyPrefix)
}

// The two environment variables that carry a credential. Spelled out in one
// place, so a message that names one and the code that reads it cannot drift.
const (
	// EnvTokenVar holds the token itself.
	EnvTokenVar = "TOFT_TOKEN"
	// EnvTokenFileVar names a file holding the token.
	//
	// The argument for a second door is that the alternative is worse for the
	// run that most needs it. An environment variable is readable through
	// /proc, inherited by every child, and printed by every diagnostic that
	// dumps the environment; a mounted file is how every secret store in a
	// container hands something over, and **the path is not sensitive**.
	EnvTokenFileVar = "TOFT_TOKEN_FILE"
)

// environmentToken reads the credential the environment supplies, if any.
//
// Most specific first, the same shape as the existing order: the token itself,
// then a file naming it, then nothing and the credentials file answers.
//
// Setting both is a refusal rather than a precedence. A job that exports two
// tokens does not know which one it is using, and picking one for it would make
// that true silently.
func environmentToken() (string, string, error) {
	token := strings.TrimSpace(os.Getenv(EnvTokenVar))
	path := strings.TrimSpace(os.Getenv(EnvTokenFileVar))

	switch {
	case token != "" && path != "":
		return "", "", &EnvCredentialError{msg: fmt.Sprintf(
			"%s and %s are both set — a job holding two tokens does not know which one it used. Unset one",
			EnvTokenVar, EnvTokenFileVar), code: "ambiguous_credential"}
	case token != "":
		return token, EnvTokenVar, nil
	case path == "":
		return "", "", nil
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// A secret that was not mounted and an empty secret are different
		// mistakes, and they get different sentences: one is a volume that did
		// not arrive, the other is a store that handed over nothing.
		return "", "", &EnvCredentialError{msg: fmt.Sprintf(
			"%s names %s and there is no file there — the secret was not mounted",
			EnvTokenFileVar, path), code: "token_file_unreadable"}
	}
	if err != nil {
		return "", "", &EnvCredentialError{msg: fmt.Sprintf("reading the token named by %s: %v", EnvTokenFileVar, err),
			code: "token_file_unreadable"}
	}
	// Trimmed exactly as --token-stdin trims: a secret store that ends its file
	// with a newline has not made a different token.
	if token = strings.TrimSpace(string(data)); token == "" {
		return "", "", &EnvCredentialError{msg: fmt.Sprintf(
			"%s names %s and it is empty", EnvTokenFileVar, path), code: "token_file_unreadable"}
	}
	return token, EnvTokenFileVar, nil
}

// EnvCredentialError is a credential the environment described and could not
// deliver. Exit 2: it is a statement about how the run was configured, and no
// install has been asked anything.
type EnvCredentialError struct{ msg, code string }

func (e *EnvCredentialError) Error() string { return e.msg }

// Code is the refusal's code in a --json document.
func (e *EnvCredentialError) Code() string { return e.code }

// ResolveSession walks the credential rule.
//
// **A credential and an address are one statement or they are nothing.** An
// environment token is used when, and only when, the install was resolved from
// the environment too — every other combination is a refusal, and this is the
// function that decides it, once, for every command.
//
// The rule replaces a precedence that was not one. TOFT_TOKEN used to beat
// the stored credential for whatever install happened to resolve, from any
// layer: a flag, a committed manifest, the stored default. So a developer
// holding a production token in their shell and running `toft deploy --install
// staging` sent the production credential to staging, and so did a
// toft.toml committed by somebody else naming an install this machine had
// never seen. The request then succeeded or failed on the far end's terms, with
// the token already on the wire and the output identical either way.
//
// `--install` does not complete the pair. It names an install; it does not name
// a credential, and the developer typing `--install staging` on a machine that
// exported a production token is the exact case this exists to catch. A rule
// with an exception in it is not a rule.
//
// Warning instead of refusing was rejected: by the time the warning prints the
// token has been sent, and in the run where it matters — unattended — nobody
// reads stderr until something has already failed.
func ResolveSession(install Value) (*Session, error) {
	envToken, envTokenVar, err := environmentToken()
	if err != nil {
		return nil, err
	}

	creds, err := LoadCredentials()
	if err != nil {
		return nil, err
	}

	url := install.Value
	origin := install.Origin
	if url == "" {
		if creds.Default == "" {
			// A token with no address on a machine that has never signed in:
			// not "sign in", which is not what the pipeline is missing, but
			// the half of the pair it left out.
			if envToken != "" {
				return nil, &EnvCredentialError{msg: fmt.Sprintf("%s is set and no install is named — set %s "+
					"to the install that token belongs to", envTokenVar, envVar[KeyInstall]), code: "missing_install"}
			}
			return nil, ErrNoInstall
		}
		url, origin = creds.Default, "the default in "+credentialsPath()
	}

	// An alias resolves to its URL before anything else uses it: the value goes
	// into HTTP requests and into `[app.installs."<url>"]` lookups, and "staging"
	// is neither. Nothing here talks to an install, so doing it before the
	// refusal below costs nothing and lets that refusal name a URL rather than
	// a word somebody then has to look up.
	c, found, err := creds.Lookup(url)
	if err != nil {
		return nil, err
	}
	if found {
		url = c.URL
	} else {
		url = NormalizeInstallURL(url)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			// Exit 2 invalid_url, as `toft login` says it: nothing was asked
			// of any install (L4, 2026-11-04 black-box list).
			return nil, &EnvCredentialError{msg: fmt.Sprintf("%q is not a URL and names no install in %s — "+
				"write it as https://<host>", install.Value, credentialsPath()), code: "invalid_url"}
		}
	}

	if envToken != "" {
		// The same install named twice is not a conflict: `--install X` with
		// TOFT_URL=X said one thing in two places (review #9).
		if origin != envVar[KeyInstall] && !sameInstall(creds, os.Getenv(envVar[KeyInstall]), url) {
			return nil, &AmbiguousCredentialError{Install: url, Origin: origin, TokenVar: envTokenVar,
				EnvInstall: os.Getenv(envVar[KeyInstall])}
		}
		// Both halves came from the environment, so the pipeline said which
		// install its token belongs to. That still beats a stored credential
		// for the same install: it is the documented precedence, and the job
		// that set both said which one it meant.
		return &Session{InstallURL: url, Token: envToken, User: c.User, Origin: origin}, nil
	}

	if !found || c.Token == "" {
		return nil, &NotSignedInError{Install: url}
	}
	return &Session{InstallURL: url, Token: c.Token, User: c.User, Origin: origin, ExpiresAt: c.ExpiresAt}, nil
}

// sameInstall reports whether named — an alias or a URL — is url.
func sameInstall(creds *CredentialsFile, named, url string) bool {
	if named == "" {
		return false
	}
	if c, found, err := creds.Lookup(named); err == nil && found {
		return c.URL == url
	}
	return NormalizeInstallURL(named) == url
}

// ResolveAdminSession is ResolveSession for a command run with `--session`: the
// same install resolution, answered by the stored session instead of the key.
//
// **The environment's token is never the answer here**, and neither is the key:
// a run that asked for the session and silently got the key would reach the
// governance routes as a key, be refused `session_required`, and send somebody
// looking for a permission problem that is really a missing sign-in.
func ResolveAdminSession(install Value) (*Session, error) {
	creds, err := LoadCredentials()
	if err != nil {
		return nil, err
	}
	url := install.Value
	origin := install.Origin
	if url == "" {
		if creds.Default == "" {
			return nil, ErrNoInstall
		}
		url, origin = creds.Default, "the default in "+credentialsPath()
	}
	c, found, err := creds.Lookup(url)
	if err != nil {
		return nil, err
	}
	if found {
		url = c.URL
	} else {
		url = NormalizeInstallURL(url)
	}
	if !found || c.Session == "" {
		return nil, &NoAdminSessionError{Install: url}
	}
	if !c.SessionExpiresAt.IsZero() && !c.SessionExpiresAt.After(time.Now()) {
		return nil, &NoAdminSessionError{Install: url, Expired: c.SessionExpiresAt}
	}
	return &Session{InstallURL: url, Token: c.Session, User: c.SessionUser, Origin: origin,
		ExpiresAt: c.SessionExpiresAt, Cookie: true}, nil
}

// RenewAdminSession stores a session the install renewed mid-command, so the
// next invocation presents the live token rather than the one it replaced.
// Best effort: a failed write costs a sign-in later, never this command.
func RenewAdminSession(installURL, token string, expires time.Time) {
	creds, err := LoadCredentials()
	if err != nil {
		return
	}
	c, found, _ := creds.Lookup(installURL)
	if !found {
		return
	}
	c.Session = token
	if !expires.IsZero() {
		c.SessionExpiresAt = expires
	}
	creds.Set(c)
	_ = creds.Save()
}

// NoAdminSessionError is `--session` with no session stored, or one past its
// expiry. Said client-side because the CLI wrote the expiry itself from the
// cookie the install set — this is the one credential whose lifetime it knows.
type NoAdminSessionError struct {
	Install string
	Expired time.Time
}

func (e *NoAdminSessionError) Error() string {
	if !e.Expired.IsZero() {
		return fmt.Sprintf("the session for %s expired at %s — sign in again with "+
			"`toft login --password-stdin %s`", e.Install, e.Expired.UTC().Format("15:04 MST 2 Jan"), e.Install)
	}
	return fmt.Sprintf("no session is stored for %s — `toft login --password-stdin %s` signs in "+
		"with a password and stores one beside the key", e.Install, e.Install)
}

// ExpiryWarning is the one line a command says when its credential is nearly
// out, and "" when it is not.
//
// **Fourteen days, because the unit that has to see this is not a day, it is a
// run.** A weekly pipeline gets two chances at fourteen days and one at seven,
// and one chance is a coin flip against a job that was skipped that week. The
// CLI cannot know a job's cadence, and a configurable threshold is a setting
// nobody sets.
//
// An expired credential is not a warning: no client-side check gates the
// request, the install answers 401 key_expired, and that is exit 3. Warning
// about it here would be the CLI deciding something only the install decides.
const ExpiryWarningWindow = 14 * 24 * time.Hour

func (s *Session) ExpiryWarning(now time.Time) string {
	// A session's life is hours by design, and it says so itself when it ends.
	if s.Cookie || s.ExpiresAt.IsZero() || !s.ExpiresAt.After(now) {
		return ""
	}
	if s.ExpiresAt.Sub(now) > ExpiryWarningWindow {
		return ""
	}
	// Both doors, because the file does not record which one this credential
	// came in by, and `toft login <url>` alone is the device flow — a browser
	// and a person, the wrong door for the CI runner or agent that holds a key
	// (docs/bugs 2026-10-28 D20). A deploy key has only the one door.
	when := s.ExpiresAt.UTC().Format("2 January 2006")
	stdin := fmt.Sprintf("printf '%%s' \"$KEY\" | toft login --token-stdin %s", s.InstallURL)
	if s.IsDeployKey() {
		return fmt.Sprintf("The deploy key for %s expires on %s — mint a new one and `%s` to replace it.",
			s.InstallURL, when, stdin)
	}
	return fmt.Sprintf("The credential for %s expires on %s — sign in again with `toft login %s` "+
		"(a browser), or, unattended, mint a new key and `%s`.", s.InstallURL, when, s.InstallURL, stdin)
}

// AmbiguousCredentialError is a token in the environment with no install named
// there — a secret with no address attached.
//
// Exit 2 rather than 3, and deliberately: nothing was refused by an install and
// nothing was sent to one. It is a statement about how the command was invoked,
// the same class as "no app linked".
type AmbiguousCredentialError struct {
	// Install is the install this command was going to talk to.
	Install string
	// Origin is the layer that named it — "--install", a manifest, the stored
	// default — which is half of what makes the refusal actionable.
	Origin string
	// TokenVar is which door the token came through, so the fix names the
	// variable that is actually set rather than the one that usually is.
	TokenVar string
	// EnvInstall is TOFT_URL when it is set too. Then the environment *did*
	// say which install the token belongs to, and the conflict is that a
	// flag or a manifest also named one: telling the user to set the variable
	// they had already set, to the value it already held, was the old answer.
	EnvInstall string
}

// Code is the refusal's code in a --json document.
func (e *AmbiguousCredentialError) Code() string { return "ambiguous_credential" }

func (e *AmbiguousCredentialError) Error() string {
	tokenVar := e.TokenVar
	if tokenVar == "" {
		tokenVar = EnvTokenVar
	}
	if e.EnvInstall != "" {
		// "drop toft.toml" was the advice when a manifest named the install;
		// what is dropped is the install it names, not the file (review #9).
		drop := "drop " + e.Origin
		if e.Origin != "--install" {
			drop = "remove the install " + e.Origin + " names"
		}
		return fmt.Sprintf("%s and %s name the install in the environment, and %s names one as well "+
			"(%s) — the pair is the whole statement, so %s, or unset %s and %s",
			tokenVar, envVar[KeyInstall], e.Origin, e.Install, drop, tokenVar, envVar[KeyInstall])
	}
	return fmt.Sprintf("%s is set, but this command targets %s (from %s), and the environment "+
		"does not say which install that token belongs to — set %s=%s, or unset %s",
		tokenVar, e.Install, e.Origin, envVar[KeyInstall], e.Install, tokenVar)
}

// ErrNoInstall is "this machine has never signed in anywhere".
var ErrNoInstall = fmt.Errorf("no Toft install is configured — run `toft login <url>`")

// NotSignedInError is "this machine knows the install and has no credential for
// it", which is a different fix from not knowing the install at all.
type NotSignedInError struct{ Install string }

func (e *NotSignedInError) Error() string {
	return fmt.Sprintf("not signed in to %s — run `toft login %s`", e.Install, e.Install)
}

// soleInstall is the one install left, when exactly one is — the default a
// person would choose themselves — and "" otherwise. Picking from a map of
// two or more picked whichever the runtime yielded, so forgetting the default
// among three installs made the next deploy go to staging or prod at random
// (code review 2026-11-11, M-2); with no default, ResolveSession asks.
func soleInstall(installs map[string]Credential) string {
	if len(installs) != 1 {
		return ""
	}
	for url := range installs {
		return url
	}
	return ""
}
