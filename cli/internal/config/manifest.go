package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// The two manifests, and why there are two.
//
// `toft.toml` is the repository's own file — the one that already declares
// background jobs and the static root — and `[app]` joins it rather than
// arriving as a second file in a second format beside it. An app slug is not a
// secret and belongs in version control with the code that deploys to it.
//
// `.toft.toml` is the uncommitted override, written by `toft link --local`,
// for the developer whose fork deploys somewhere private. It is read first and
// is otherwise identical.
//
// The builder ignores a table it does not know, which is what makes `[app]`
// safe to add to a file it also reads — and that is a tested contract on the
// server side (TestUnknownTablesAreIgnored), not an accident of the decoder.

const (
	// ManifestFile is the committed file, shared with the builder.
	ManifestFile = "toft.toml"
	// LocalManifestFile is the uncommitted override.
	LocalManifestFile = ".toft.toml"
)

// Manifest is the part of toft.toml this client reads. Everything else in
// the file — `[jobs]`, `[static]` — belongs to the platform, and is left alone
// here for the same reason the platform leaves `[app]` alone there.
type Manifest struct {
	App AppTable `toml:"app"`
}

// AppTable is `[app]`:
//
//	[app]
//	slug = "phase-test"
//
//	# a checkout that targets two installs says so, and neither is wrong:
//	[app.installs."https://toft.corp"]
//	slug = "phase-test"
//
//	[app.installs."https://staging.toft.corp"]
//	slug = "phase-test-stg"
type AppTable struct {
	Slug string `toml:"slug"`
	// Name is the display name `toft create` gives a new app when --name is
	// not passed. Read at create and nowhere else: after that the name is the
	// app's own setting, renamed in its settings page, and a deploy never
	// moves it — a file that overwrote a rename would be two sources of truth.
	Name string `toml:"name"`
	// Install pins this directory to one Toft. Rarely written by hand —
	// `toft link` writes it only when the directory would otherwise be ambiguous.
	Install  string                  `toml:"install"`
	Installs map[string]InstallEntry `toml:"installs"`
}

// InstallEntry is one `[app.installs."<url>"]` table.
type InstallEntry struct {
	Slug string `toml:"slug"`
}

// slugFor answers the per-install slug, matching the URL the way a person would
// rather than the way a byte comparison would: trailing slashes and case in the
// scheme and host are not a difference anybody means.
func (a AppTable) slugFor(installURL string) string {
	if installURL == "" || len(a.Installs) == 0 {
		return ""
	}
	want := NormalizeInstallURL(installURL)
	for url, entry := range a.Installs {
		if NormalizeInstallURL(url) == want {
			return entry.Slug
		}
	}
	return ""
}

// NormalizeInstallURL is the one spelling of an install's address.
//
// It exists because the same install is written four ways by four people —
// with and without a trailing slash, with the host in capitals, with and
// without the scheme — and every one of them is used as a map key: in the
// credentials file, in `[app.installs]`, and in the comparison between the two.
// A key that matches by accident is a key that stops matching by accident.
//
// **It supplies the scheme**, so `toft.corp` and `https://toft.corp`
// are one key rather than two. `https` is the only default that can be wrong in
// the safe direction: an install genuinely on plaintext has to type it, and
// `toft login` refuses that outside loopback.
//
// **It lowercases the scheme and the host and leaves the path alone.** A host is
// case-insensitive by RFC and a path is not, so lowercasing the whole string —
// which this did — normalized an install under a base path to a key that did not
// match itself.
//
// What it does not do is invent an address out of something that is not one: a
// bare word with no dot and no port is an alias, and turning `prod` into
// `https://prod` would convert "that alias names nothing" into a request to a
// host called prod. Those two are told apart by looksLikeHost and nowhere else.
func NormalizeInstallURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		// Empty stays empty. Callers compare against "" to mean "nothing was
		// set", and an empty string that normalized to a scheme would be an
		// install nobody named.
		return ""
	}

	i := strings.Index(raw, "://")
	if i < 0 {
		if !looksLikeHost(raw) {
			return strings.ToLower(raw)
		}
		raw = "https://" + raw
		i = len("https")
	}

	scheme, rest := strings.ToLower(raw[:i+3]), raw[i+3:]
	if j := strings.Index(rest, "/"); j >= 0 {
		return scheme + strings.ToLower(rest[:j]) + rest[j:]
	}
	return scheme + strings.ToLower(rest)
}

// looksLikeHost reports whether a string with no scheme is an address rather
// than an alias.
//
// A dot or a port is what every real install has and what no alias somebody
// types has; localhost is the one host with neither, and it is the one a
// developer types most.
func looksLikeHost(raw string) bool {
	if strings.ContainsAny(raw, ".:") {
		return true
	}
	host, _, _ := strings.Cut(raw, "/")
	return strings.EqualFold(host, "localhost")
}

// ReadManifest reads one manifest file. A missing file is (nil, nil): most
// directories have none, and that is not a condition to report.
func ReadManifest(path string) (*Manifest, error) {
	var m Manifest
	if err := decodeTOMLFile(path, &m); err != nil {
		return nil, &ManifestError{Path: path, Err: err}
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	return &m, nil
}

// ManifestError is a toft.toml (or .toft.toml) that exists and cannot be read.
// A type of its own so the caller can classify it: a file the developer wrote
// is a usage problem, exit 2 with code manifest_invalid, and never "the
// platform failed".
type ManifestError struct {
	Path string
	Err  error
}

func (e *ManifestError) Error() string { return e.Err.Error() }
func (e *ManifestError) Unwrap() error { return e.Err }

// decodeTOMLFile decodes a file that may not exist. A parse error is reported —
// a file somebody has written and the CLI silently ignores is worse than a
// refusal, because the refusal names the line.
func decodeTOMLFile(path string, into any) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if _, err := toml.Decode(string(data), into); err != nil {
		return fmt.Errorf("%s: %s", shownPath(path), tomlError(string(data), err))
	}
	return nil
}

// shownPath is the file as the person typed their way to it: relative to the
// working directory when it is under it, as the install's own `check` names
// it, rather than the absolute path the resolver walked.
func shownPath(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(wd, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// tomlError restates a TOML parse error with the line counted from the byte
// offset the library reports, rather than its own line counter — which has
// already stepped past the newline when the newline is what was unexpected,
// so an unterminated `[jobs.a` on the last line of a three-line file was
// reported at line 4, a line that does not exist.
func tomlError(data string, err error) string {
	var pe toml.ParseError
	if !errors.As(err, &pe) || pe.Position.Start < 0 || pe.Position.Start > len(data) {
		return err.Error()
	}
	line := 1 + strings.Count(data[:pe.Position.Start], "\n")
	if pe.LastKey == "" {
		return fmt.Sprintf("toml: line %d: %s", line, pe.Message)
	}
	return fmt.Sprintf("toml: line %d (last key %q): %s", line, pe.LastKey, pe.Message)
}
