//go:build linux

package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// A service's identity is issued by the agent, per process (ADR §130).
//
// The platform's own host-run token is single-use, which a service cannot be:
// it restarts, and presents its identity again at every start and every token
// refresh. A platform-minted reusable token would be a credential valid for the
// release's life and at rest wherever it was handed over. This is valid exactly
// as long as the process it was issued to: the agent writes 32 random bytes
// beside each process it starts, and answers the platform's /identity/verify
// only while that process is alive. A copy outlives nothing.
//
// The file is root's and the app's group's to read, never the app's to write:
// id/<name> is root:<per-app group> 0710 and the token 0440, so root never
// writes into a tree an app owns.

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *Agent) identityDir(name string) string { return filepath.Join(a.cfg.Dir, "id", name) }

// issueIdentity writes a fresh token for one process of spec's workload and
// returns it with its path.
func (a *Agent) issueIdentity(spec Spec) (token, path string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)

	dir := a.identityDir(spec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, "token")
	tmp := path + ".new"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return "", "", err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}

	// Who may read it. As root: the per-app group, or the account itself when
	// the install has no such group. As anyone else there is nobody to hand
	// it to — the runner runs as the agent's own user.
	if os.Geteuid() == 0 && spec.UID >= 0 {
		switch {
		case spec.IdentityGID > 0:
			if err := os.Chown(dir, 0, spec.IdentityGID); err != nil {
				return "", "", err
			}
			if err := os.Chmod(dir, 0o710); err != nil {
				return "", "", err
			}
			if err := os.Chown(tmp, 0, spec.IdentityGID); err != nil {
				return "", "", err
			}
			if err := os.Chmod(tmp, 0o440); err != nil {
				return "", "", err
			}
		default:
			if err := os.Chmod(dir, 0o711); err != nil {
				return "", "", err
			}
			if err := os.Chown(tmp, spec.UID, -1); err != nil {
				return "", "", err
			}
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", "", err
	}
	return token, path, nil
}

func (a *Agent) rememberToken(token, name string) {
	a.mu.Lock()
	a.tokens[digest(token)] = name
	a.mu.Unlock()
}

func (a *Agent) forgetToken(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	delete(a.tokens, digest(token))
	a.mu.Unlock()
}

// verify answers who a token belongs to, while its process is alive.
func (a *Agent) verify(token string) (VerifyResponse, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name, ok := a.tokens[digest(token)]
	if !ok {
		return VerifyResponse{}, false
	}
	w := a.workloads[name]
	if w == nil || w.proc == nil || w.proc.token != token {
		return VerifyResponse{}, false
	}
	select {
	case <-w.proc.exited:
		return VerifyResponse{}, false
	default:
	}
	return VerifyResponse{AppID: w.spec.AppID, Workload: w.spec.Name, Kind: w.spec.Kind}, true
}

// removeIdentityFile removes the token file if it still holds token: a
// replacement's own token, written since, is left alone.
func (a *Agent) removeIdentityFile(name, token string) {
	path := filepath.Join(a.identityDir(name), "token")
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == token {
		_ = os.Remove(path)
	}
}

func (a *Agent) removeIdentity(name string) { _ = os.RemoveAll(a.identityDir(name)) }

// readIdentity is the token on disk for name, for an adopted process.
func (a *Agent) readIdentity(name string) string {
	b, err := os.ReadFile(filepath.Join(a.identityDir(name), "token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
