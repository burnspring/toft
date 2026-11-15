package api

import (
	"context"
	"crypto"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/database"
)

// RStudio's "Add account" sign-in: rsconnect's token flow, on `toft login`'s
// approval (ADR §102).
//
// rsconnect makes an RSA key pair, registers the public half with
// POST /__api__/tokens, opens the claim URL in a browser, and then polls
// /users/current — **signed** — until somebody approves. Every later request is
// signed the same way rather than carrying a secret.
//
// Two decisions keep this from being a new kind of credential:
//
//   - **The approval is `toft login`'s.** Registering a token creates an ordinary
//     device code and the claim URL is /cli?code=…, so the person decides on the
//     page that already decides, in a browser session, under the per-user key
//     cap, with the same audit row. A key cannot approve anything there.
//   - **What the approval creates is an ordinary personal key.** The token row
//     binds the public key to it, and a verified request is handed to
//     apiKeyAuth as that key's hash — so disabling, expiry, the owner's lock,
//     scope, the route allow-list, usage counting and revocation are the key's,
//     made by the code that already makes them. The key's own secret is
//     generated and discarded: possession of the private key is the proof.
//
// What the protocol fixes and this cannot change: the signature is RSA PKCS#1
// v1.5 over SHA-1, and a signed request can be replayed until its Date falls
// outside the window below. Both are Connect's, stated rather than designed
// around.

// connectSignatureWindow is how far a signed request's Date may be from now.
// It bounds replay; it has to absorb a laptop's clock drift.
const connectSignatureWindow = 5 * time.Minute

// connectSpoolLimit caps a signed body, which is read in full before its
// checksum can be compared: the upload routes' 100MB and a little slack for a
// JSON request.
const connectSpoolLimit = 100<<20 + 1<<20

// connectTokenIDRe is the shape rsconnect gives a token: "T" and sixteen random
// bytes as hex. Anything else is not a token it made.
var connectTokenIDRe = regexp.MustCompile(`^T[0-9a-fA-F]{16,64}$`)

type connectSignedKeyCtx struct{}

// connectSignedKeyHash is the personal key a verified signed request
// authenticates as, for apiKeyAuth.
func connectSignedKeyHash(ctx context.Context) (string, bool) {
	h, ok := ctx.Value(connectSignedKeyCtx{}).(string)
	return h, ok && h != ""
}

// connectAPIRewrite moves a Connect request onto the platform API before
// anything routes it, and verifies it first when rsconnect signed it.
//
// Two rewrites, and only on this prefix. The path, so the request lands in the
// /api/v1 group and meets its middleware. And the credential: Connect clients
// send `Authorization: Key <secret>`, which no authenticator here reads, so it
// becomes the `Bearer` form both key classes accept; nothing outside /__api__
// ever accepts the `Key` spelling. A signed request is verified here rather
// than further in because the signature covers the path **as the client sent
// it**, which the rewrite is about to change.
func (s *Server) connectAPIRewrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, connectAPIPrefix)
		if !ok || (rest != "" && rest[0] != '/') {
			next.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = connectRoutePrefix + rest
		r2.URL.RawPath = ""
		if key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Key "); ok {
			r2.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
		}

		if isConnectSigned(r) {
			hash, body, ok := s.verifyConnectSignature(w, r)
			if !ok {
				return
			}
			if body != nil {
				defer body.cleanup()
				r2.Body = body
				r2.ContentLength = body.size
			}
			r2 = r2.WithContext(context.WithValue(r2.Context(), connectSignedKeyCtx{}, hash))
		}
		next.ServeHTTP(w, r2)
	})
}

// isConnectSigned reports a request rsconnect signed. Its unauthenticated calls
// carry `X-Auth-Token: anonymous-access` and no signature, and are not.
func isConnectSigned(r *http.Request) bool {
	return r.Header.Get("X-Auth-Signature") != "" && r.Header.Get("X-Auth-Token") != "" &&
		r.Header.Get("X-Auth-Token") != "anonymous-access"
}

// connectUnauthorized is every refusal of a signed request. 401 is also what
// rsconnect reads as "not approved yet" and keeps polling on, so the pending
// case shares it; the sentence says which it was.
func connectUnauthorized(w http.ResponseWriter, msg string) {
	connectError(w, http.StatusUnauthorized, msg)
}

// verifyConnectSignature checks a signed request and answers the personal key
// it authenticates as, claiming that key first if the token has just been
// approved. A refusal has been written when ok is false.
func (s *Server) verifyConnectSignature(w http.ResponseWriter, r *http.Request) (string, *spooledBody, bool) {
	tokenID := r.Header.Get("X-Auth-Token")
	if !connectTokenIDRe.MatchString(tokenID) {
		connectUnauthorized(w, "unknown token — add the account again")
		return "", nil, false
	}
	tok, err := s.db.GetConnectToken(tokenID)
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return "", nil, false
	}
	if tok == nil {
		connectUnauthorized(w, "unknown token — add the account again")
		return "", nil, false
	}
	pub, err := parseConnectPublicKey(tok.PublicKey)
	if err != nil {
		connectUnauthorized(w, "unknown token — add the account again")
		return "", nil, false
	}

	date := r.Header.Get("Date")
	at, err := http.ParseTime(date)
	if err != nil || time.Since(at).Abs() > connectSignatureWindow {
		connectUnauthorized(w, "the request's Date is missing or more than five minutes from this "+
			"server's clock — check the clock on the machine running RStudio")
		return "", nil, false
	}

	// The signature is over the *header* checksum, so it is checked before a
	// byte of the body is read. It proves only that the caller holds the key
	// the token was registered with — and anybody can register a token with a
	// key of their own choosing. What keeps an anonymous caller from filling
	// the apps volume is the approval check that follows it: a token nobody
	// has approved is refused before the body is spooled. Then the body has to
	// match the checksum, or a valid signature would carry any body at all.
	checksum := r.Header.Get("X-Content-Checksum")
	if !verifyConnectRequest(pub, r.Method, r.URL.EscapedPath(), date, checksum, r.Header.Get("X-Auth-Signature")) {
		connectUnauthorized(w, "the request signature does not verify — add the account again")
		return "", nil, false
	}
	if tok.KeyHash == "" {
		if _, msg := s.connectTokenApproval(tok); msg != "" {
			connectUnauthorized(w, msg)
			return "", nil, false
		}
	}
	body, sum, err := s.spoolConnectBody(r)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			connectError(w, http.StatusRequestEntityTooLarge, "the request body is larger than 100MB")
		} else {
			connectError(w, http.StatusBadRequest, "the request body did not arrive")
		}
		return "", nil, false
	}
	fail := func(msg string) (string, *spooledBody, bool) {
		if body != nil {
			body.cleanup()
		}
		connectUnauthorized(w, msg)
		return "", nil, false
	}
	if checksum != sum {
		return fail("the request body does not match its checksum")
	}

	if tok.KeyHash != "" {
		return tok.KeyHash, body, true
	}
	hash, msg := s.claimConnectToken(tok)
	if hash == "" {
		return fail(msg)
	}
	return hash, body, true
}

// connectCanonicalRequest is what rsconnect signs: the method, the path as sent
// with any query removed, the Date header and the body's checksum, one per line.
func connectCanonicalRequest(method, path, date, checksum string) string {
	path, _, _ = strings.Cut(path, "?")
	return method + "\n" + path + "\n" + date + "\n" + checksum
}

// verifyConnectRequest checks an rsconnect signature: RSA PKCS#1 v1.5 over the
// SHA-1 of the canonical request, base64. The private-key form carries no
// "; version=" suffix, but one is ignored rather than refused.
func verifyConnectRequest(pub *rsa.PublicKey, method, path, date, checksum, signature string) bool {
	sigB64, _, _ := strings.Cut(signature, ";")
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return false
	}
	digest := sha1.Sum([]byte(connectCanonicalRequest(method, path, date, checksum)))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA1, digest[:], sig) == nil
}

// parseConnectPublicKey reads the DER rsconnect registers — a
// SubjectPublicKeyInfo from openssl's write_der, or a bare PKCS#1 key — and
// refuses anything that is not RSA of at least 2048 bits.
func parseConnectPublicKey(der []byte) (*rsa.PublicKey, error) {
	var pub *rsa.PublicKey
	if k, err := x509.ParsePKIXPublicKey(der); err == nil {
		rk, ok := k.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		pub = rk
	} else if rk, err := x509.ParsePKCS1PublicKey(der); err == nil {
		pub = rk
	} else {
		return nil, errors.New("not a public key")
	}
	if pub.N.BitLen() < 2048 {
		return nil, errors.New("key shorter than 2048 bits")
	}
	return pub, nil
}

// spooledBody is a signed request's body, read in full so its checksum could be
// compared, and handed on to the handler from disk.
type spooledBody struct {
	*os.File
	size int64
}

func (b *spooledBody) cleanup() {
	b.File.Close()
	os.Remove(b.File.Name())
}

// spoolConnectBody reads a request's body and returns rsconnect's checksum of
// it — the MD5, base64 — with the bytes ready to be read again. A request with
// no body is the checksum of nothing, which is what rsconnect signs for a GET.
//
// The bytes go to the apps directory rather than the system temp, which on
// some hosts is a small tmpfs and a bundle is up to 100MB.
func (s *Server) spoolConnectBody(r *http.Request) (*spooledBody, string, error) {
	h := md5.New()
	if r.Body == nil || r.Body == http.NoBody {
		return nil, base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
	}
	dir := filepath.Join(s.cfg.AppsDir, ".connect-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	f, err := os.CreateTemp(dir, "body-*")
	if err != nil {
		return nil, "", err
	}
	n, err := io.Copy(io.MultiWriter(f, h), http.MaxBytesReader(nil, r.Body, connectSpoolLimit))
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, "", err
	}
	if n == 0 {
		f.Close()
		os.Remove(f.Name())
		return nil, base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
	}
	return &spooledBody{File: f, size: n}, base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// connectTokenApproval reads the device code behind a token not yet claimed:
// the approved code, or the sentence that refuses the request. A code denied,
// expired or gone takes its token with it.
func (s *Server) connectTokenApproval(tok *database.ConnectToken) (*database.DeviceCode, string) {
	d, err := s.db.PollDeviceCode(tok.DeviceCodeHash)
	if err != nil || d == nil {
		if d == nil && err == nil {
			s.db.DeleteConnectToken(tok.TokenID, tok.DeviceCodeHash)
		}
		return nil, "this sign-in is no longer waiting — add the account again"
	}
	switch deviceStatus(d, time.Now().UTC()) {
	case deviceStatusPending:
		return nil, "waiting for this sign-in to be approved in the browser"
	case deviceStatusDenied, deviceStatusExpired:
		if err := s.db.DeleteConnectToken(tok.TokenID, tok.DeviceCodeHash); err != nil {
			log.Printf("connect: dropping an unapproved token: %v", err)
		}
		return nil, "this sign-in was denied or expired — add the account again"
	}
	return d, ""
}

// claimConnectToken is the first signed request after an approval: it creates
// the personal key the token will authenticate as from now on. The empty hash
// means not yet, or never, and the sentence says which.
func (s *Server) claimConnectToken(tok *database.ConnectToken) (string, string) {
	d, msg := s.connectTokenApproval(tok)
	if msg != "" {
		return "", msg
	}

	owner, err := s.db.GetUser(d.ApprovedUserID)
	if err != nil || owner == nil {
		return "", "the account that approved this sign-in no longer exists"
	}
	name, err := s.uniqueKeyName(owner.ID, "RStudio (rsconnect)")
	if err != nil {
		return "", "internal error"
	}
	// The secret is generated for the row's shape and never kept: nobody will
	// present it. The token's private key is what proves possession.
	secret, err := generateUserAPIKeySecret()
	if err != nil {
		return "", "internal error"
	}
	expires, err := s.signInKeyExpiry()
	if err != nil {
		return "", "internal error"
	}
	key := database.UserAPIKey{
		ID:        uuid.New().String(),
		UserID:    owner.ID,
		Name:      name,
		KeyHash:   hashAPIKey(secret),
		KeyPrefix: keyDisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expires,
	}
	audit := database.AuditEntry{
		ActorType:  database.PrincipalTypeUser,
		ActorID:    owner.ID,
		Action:     "user.api_key_create",
		TargetType: "user",
		TargetID:   owner.ID,
		After: jsonValStr(map[string]interface{}{
			"key_id": key.ID, "name": key.Name, "key_prefix": key.KeyPrefix,
			"expires_at": key.ExpiresAt, "via": "rsconnect", "user_code": d.UserCode,
		}),
	}
	claimed, err := s.db.ClaimConnectToken(tok.TokenID, tok.DeviceCodeHash, &key, audit)
	if err != nil {
		log.Printf("connect: claiming a token: %v", err)
		return "", "internal error"
	}
	if !claimed {
		// A concurrent poll claimed it first; the row now names that key.
		if again, err := s.db.GetConnectToken(tok.TokenID); err == nil && again != nil && again.KeyHash != "" {
			return again.KeyHash, ""
		}
		return "", "this sign-in is no longer waiting — add the account again"
	}
	return key.KeyHash, ""
}

// handleConnectAddToken registers rsconnect's public key and starts the
// approval: an `toft login` device code, marked as rsconnect's and stamped with
// the address that asked, whose page is the claim URL rsconnect opens.
//
// Unauthenticated by necessity, like the device flow's start — the client has
// nothing to present yet — and absent where personal keys are off. A key is
// refused here by the route guard: no credential starts a credential.
func (s *Server) handleConnectAddToken(w http.ResponseWriter, r *http.Request) {
	if s.personalKeysAbsent(w) {
		return
	}
	var req struct {
		Token     string `json:"token"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		connectError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !connectTokenIDRe.MatchString(req.Token) {
		connectError(w, http.StatusBadRequest, "token is not in the form rsconnect generates")
		return
	}
	der, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err == nil {
		_, err = parseConnectPublicKey(der)
	}
	if err != nil {
		connectError(w, http.StatusBadRequest, "public_key must be a base64 DER RSA key of at least 2048 bits")
		return
	}
	if existing, err := s.db.GetConnectToken(req.Token); err != nil || existing != nil {
		connectError(w, http.StatusConflict, "that token is already registered")
		return
	}

	userCode, err := generateUserCode()
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	deviceCode, err := generateDeviceCode()
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := time.Now().UTC()
	d := &database.DeviceCode{
		UserCode:      userCode,
		ClientVersion: sanitizeDeviceDisplay(r.UserAgent()),
		Client:        database.DeviceClientRSConnect,
		RequestedFrom: clientIP(r),
		CreatedAt:     now,
		ExpiresAt:     now.Add(deviceCodeTTL),
	}
	tok := &database.ConnectToken{TokenID: req.Token, PublicKey: der}
	if err := s.db.CreateConnectToken(tok, d, hashAPIKey(deviceCode)); errors.Is(err, database.ErrTooManyPendingCodes) {
		w.Header().Set("Retry-After", strconv.Itoa(int(deviceCodeTTL.Seconds())))
		connectError(w, http.StatusTooManyRequests,
			"too many sign-in requests are waiting to be approved — try again in a few minutes")
		return
	} else if err != nil {
		log.Printf("connect: registering a token: %v", err)
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":           req.Token,
		"token_claim_url": s.installOrigin(r) + "/cli?code=" + userCode,
	})
}
