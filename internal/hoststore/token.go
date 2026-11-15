// Package hoststore is the data plane of a storage resource that lives under a
// host root: a directory on the platform's own machine, reached by people over
// URLs the platform signs for itself and then serves.
//
// **It speaks the protocol a bucket already speaks.** Every person-facing store
// route mints through artifact.Signer, and the CLI, the Files page and the SDKs
// follow whatever URL, method and headers they are handed. So this is a new
// backend behind the same interface rather than a new protocol: a URL on the
// platform's own origin, `/store-data/{token}`, where the token is the whole
// credential.
//
// A bucket's bytes never touch the platform, because the object store can
// enforce a signature on its own. A disk mounted on this host has no other
// server that could, so here the platform is the enforcer, and three things a
// bucket gave for free are this package's to keep:
//
//   - **Containment** is an os.Root at the folder whose grant authorized the
//     request (Binding.Scope), so a symlink an app planted inside a granted
//     folder cannot reach a sibling the grant does not cover.
//   - **The ladder**: `write` is create-only (a temp file linked into place,
//     which fails on an existing name), `full` renames over and unlinks. A
//     write never goes through an existing inode.
//   - **Ownership**: what is written is the gate's — `root:<gate>`, files
//     `0660`, folders `2770` — at the number the database recorded, so the apps
//     on the gate can edit and delete what a person uploaded.
package hoststore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Claims is what a token says, and every field is covered by its MAC.
type Claims struct {
	// Resource is the resource id, for the audit. Root and Slug locate the
	// directory: the root by name, resolved against this install's configured
	// roots at serve time, so a token never carries a filesystem path.
	Resource string `json:"r"`
	Root     string `json:"o"`
	Slug     string `json:"s"`
	// Key is the object's name relative to the resource directory. A trailing
	// slash is a folder.
	Key string `json:"k"`
	// Method is the one HTTP method the token admits.
	Method string `json:"m"`
	// Scope is the folder the containment root is opened at — the grant that
	// authorized the level. Empty is the resource directory.
	Scope string `json:"c,omitempty"`
	// Expires is a Unix time, checked when the request starts.
	Expires int64 `json:"e"`
	// Subject is who minted it, for the per-subject concurrency cap and the
	// audit of a completed write.
	Subject string `json:"p,omitempty"`
	// The PUT's conditions: the declared length and both digests (base64), and
	// whether an existing name refuses the write.
	Length      int64  `json:"n,omitempty"`
	MD5         string `json:"d,omitempty"`
	SHA256      string `json:"h,omitempty"`
	IfNoneMatch bool   `json:"x,omitempty"`
	// GID is the gate's recorded number, which a PUT's file and folders take.
	GID int `json:"g,omitempty"`
	// AttachAs names the saved file on a download.
	AttachAs string `json:"a,omitempty"`
}

// ErrBadToken covers every way a token can fail to verify. One error, so the
// handler's answer cannot say which part was wrong.
var ErrBadToken = errors.New("this link is not valid")

// ErrExpired is the one failure worth naming: it is the ordinary one, and the
// fix is the caller's — mint another.
var ErrExpired = errors.New("this link has expired — mint another")

// sign renders claims as `<payload>.<mac>`, both base64url.
func sign(key []byte, c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + base64.RawURLEncoding.EncodeToString(macOf(key, p)), nil
}

// verify checks the MAC before it parses anything, then the expiry.
func verify(key []byte, token string, now time.Time) (Claims, error) {
	p, m, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, ErrBadToken
	}
	got, err := base64.RawURLEncoding.DecodeString(m)
	if err != nil || !hmac.Equal(got, macOf(key, p)) {
		return Claims{}, ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, ErrBadToken
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, ErrBadToken
	}
	if now.Unix() > c.Expires {
		return Claims{}, ErrExpired
	}
	return c, nil
}

func macOf(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("toft-host-store\x00"))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// DeriveKey is the data plane's key, from the platform's signing secret under
// a label of its own — derived, never stored, the way the role key is (ADR
// §56), so nothing else signed with the secret is a valid token here.
func DeriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("toft-host-store-key"))
	return mac.Sum(nil)
}
