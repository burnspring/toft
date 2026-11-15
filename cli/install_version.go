package main

import (
	"strconv"
	"strings"
	"time"
)

// nowSince exists so status.go's roughAge can be reused from rollback without
// importing time there for one call.
func nowSince(t time.Time) time.Duration { return time.Since(t) }

// upgradeCommand is the whole upgrade: the installer downloads the newest
// release and renames it over this binary. It is written out here rather than
// derived because there is nothing to derive it from — a client that has not
// been upgraded knows only where it came from — and deploy/cli/cli_test.go
// checks this string against the repository publish-cli.sh publishes to, which
// is the same guard install.sh and the README are under.
const upgradeCommand = "curl -fsSL https://raw.githubusercontent.com/" + githubOwner + "/toft-cli/main/install.sh | sh"

// githubOwner is the server's config.GitHubOwner, restated because this is a
// separate module and cannot import it; internal/config/owner_test.go holds the
// two equal.
const githubOwner = "burnspring"

// noteInstallVersion says one dimmed line when the install is newer than this
// binary.
//
// A laptop's `toft` is older than the install it talks to more often than not,
// and that is fine by design: the step vocabulary is built so adding a step is
// safe, and an unknown step renders as its key. What is not fine is somebody
// watching a deploy full of words their client does not label and having no
// idea why. Once, and never a refusal — a client that stopped working because
// the server moved would be the failure this design exists to avoid.
//
// It names the upgrade because the line is otherwise a fact with nothing to do
// about it: this is the one moment somebody knows their client is behind, and
// sending them to a README to find a command they could have been given is how
// a courtesy turns into a nag. The command is on its own line to be copied.
//
// It reads the unauthenticated config probe — session.config(), the same read
// the app's URL comes from and the same one only-once — so it costs nothing and
// works before there is a credential. A failure there is silence: this is a
// courtesy, and a courtesy that could fail a command would not be one.
func (e *env) noteInstallVersion(s *session) {
	cfg := s.config()
	if cfg.Version == "" || !newerThan(cfg.Version, version) {
		return
	}
	e.out.Dim("%s is running Toft %s; this toft is %s. Anything it does not "+
		"recognise is shown by its key. To upgrade:\n    %s",
		s.InstallURL, cfg.Version, version, upgradeCommand)
}

// newerThan compares two dotted version strings, numerically per component.
//
// Deliberately small: it exists to decide whether to print one dimmed line. A
// version it cannot parse compares as not-newer, so an install running
// something unusual gets silence rather than a line about nothing.
func newerThan(a, b string) bool {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := 0, 0
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func versionParts(v string) []int {
	// Anything after a dash is a pre-release marker and is not compared: 0.2.0
	// and 0.2.0-rc1 are the same install as far as this line is concerned.
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, part := range strings.Split(strings.TrimSpace(v), ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}
