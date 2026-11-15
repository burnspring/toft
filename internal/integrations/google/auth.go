// Package google is the Drive and Chat transports and the service-account
// credential they share.
//
// # The credential
//
// A service account key: the JSON file Google Cloud hands you, pasted whole.
// **No browser OAuth**, for the reason the Microsoft trio has none and
// `auth_providers` has none — it is a redirect surface, a state store and a
// token-refresh path per provider, and every other credential-holding
// connection on this platform is pasted.
//
// A service account is also the *right* shape for this feature in a way a
// person's OAuth grant is not: an app writing a weekly report into a shared
// drive should not stop working the week the colleague who authorized it
// leaves. That is the same argument `git_connections` makes about a personal
// access token, and it is the reason a delegated grant would be the weaker
// option even if it were free.
//
// # What it can reach
//
// Whatever the organization shared with the service account's own address, and
// nothing else. That is the boundary, and it is one an administrator can
// inspect and change from Google's side without touching Toft — the same
// property Slack's "invite the bot" has, and the one Teams' workflow URL
// deliberately does not (see that package).
package google

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"toft/internal/appint"
	"toft/internal/integrations"
)

// Timeout bounds one call.
const Timeout = 60 * time.Second

// Scopes, one per transport. Drive's is the full `drive` scope, and the
// narrower `drive.file` was wrong rather than cautious: it reaches only files
// this OAuth client created or a person opened with it through a picker, so the
// folder an administrator shares with the service account — the scope of every
// connection of this kind — was invisible to the token, and so was every
// subfolder its people made. For a *service account* the full scope is not the
// organization's documents: it is what has been shared with the account's own
// address, which is the boundary the kind names. What that costs is stated in
// the kind's hint: use the account for this connection alone, because anything
// else shared with it is reachable by the same token.
const (
	ScopeDrive   = "https://www.googleapis.com/auth/drive"
	ScopeChatBot = "https://www.googleapis.com/auth/chat.bot"
)

// The hosts this package talks to. Unexported variables for the reason graph's
// are: a configurable endpoint is a way to aim a stored credential at an
// arbitrary server, and they are variables only so this package's own tests can
// stub them.
var (
	tokenHost  = "https://oauth2.googleapis.com/token"
	driveHost  = "https://www.googleapis.com"
	chatHost   = "https://chat.googleapis.com"
	httpClient = &http.Client{Timeout: Timeout}
)

// ServiceAccount is the half of the key file this package uses.
type ServiceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	// TokenURI is read and never dialled: every key Google issues carries
	// tokenHost, and honouring the field would let a pasted document aim the
	// signed assertion — and this box's network position — at any URL.
	TokenURI string `json:"token_uri"`
}

// From reads the service account key off a Target.
//
// The whole JSON document is one credential field, pasted as it comes out of
// Google Cloud. Splitting it into five form fields would be five chances for a
// PEM body to lose its newlines in a text input, which is the failure that
// produces "invalid signature" and no way to see why.
func From(t integrations.Target) (*ServiceAccount, error) {
	raw := strings.TrimSpace(t.Credential("service_account"))
	if raw == "" {
		return nil, errors.New("this connection has no service account key")
	}
	var sa ServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, errors.New(
			"that is not a service account key — paste the whole JSON file Google Cloud gave you")
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New(
			"that JSON has no client_email or private_key — it may be an OAuth client rather " +
				"than a service account key")
	}
	return &sa, nil
}

// Token mints an access token for one scope.
func (sa *ServiceAccount) Token(ctx context.Context, scope string) (string, error) {
	tok, err := sa.Mint(ctx, scope)
	return tok.Value, err
}

// tokens is the package's cache, one for every connection on the install.
var tokens = integrations.NewTokenCache()

// Mint returns an access token for one scope and when it expires, from the
// cache while it has more than integrations.TokenMargin left. The key carries
// the private key itself (digested with the rest), so a rotated key is a miss.
func (sa *ServiceAccount) Mint(ctx context.Context, scope string) (integrations.AccessToken, error) {
	key := integrations.TokenKey(sa.endpoint(), sa.ClientEmail, sa.PrivateKeyID, sa.PrivateKey, scope)
	return tokens.Get(ctx, key, func(ctx context.Context) (integrations.AccessToken, error) {
		return sa.mint(ctx, scope)
	})
}

// endpoint is the compiled-in token host, whatever the key file says.
func (sa *ServiceAccount) endpoint() string { return tokenHost }

// mint exchanges a signed assertion for a token, uncached.
func (sa *ServiceAccount) mint(ctx context.Context, scope string) (integrations.AccessToken, error) {
	fail := func(err error) (integrations.AccessToken, error) { return integrations.AccessToken{}, err }
	endpoint := sa.endpoint()
	assertion, err := sa.assertion(scope, endpoint)
	if err != nil {
		return fail(err)
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	issued := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		return fail(appint.Retryable(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var body struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode/100 != 2 || body.AccessToken == "" {
		msg := body.ErrorDescription
		if msg == "" {
			msg = body.Error
		}
		// Only the OAuth error fields are repeated, never the body itself:
		// a body is whatever the far side chose to put there.
		if msg == "" {
			msg = "it answered without an access token"
		}
		return fail(&APIError{Op: "the token endpoint", Status: resp.StatusCode, Message: msg})
	}
	return integrations.AccessToken{
		Value:     body.AccessToken,
		ExpiresAt: integrations.ExpiresIn(issued, body.ExpiresIn),
	}, nil
}

// assertion builds and signs the JWT Google exchanges for a token.
func (sa *ServiceAccount) assertion(scope, audience string) (string, error) {
	key, err := sa.signer()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	if sa.PrivateKeyID != "" {
		header["kid"] = sa.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": scope,
		"aud":   audience,
		"iat":   now.Unix(),
		// Ten minutes: Google's ceiling is an hour, and a shorter assertion is
		// a shorter window for one that leaks in a log.
		"exp": now.Add(10 * time.Minute).Unix(),
	}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)

	signing := b64(headerJSON) + "." + b64(claimsJSON)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the assertion: %w", err)
	}
	return signing + "." + b64(sig), nil
}

// signer parses the PEM private key out of the key file.
func (sa *ServiceAccount) signer() (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, errors.New(
			"the private key in that service account file is not readable — if it was pasted " +
				"through a form that lost its line breaks, paste the file again")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("that service account key is not an RSA key")
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the private key in that service account file could not be parsed")
	}
	return key, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// APIError is a failure Google reported.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Google refused %s: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("Google returned HTTP %d from %s", e.Status, e.Op)
}

// Retryable reports whether a second attempt could succeed. A quota or a server
// error could; "not found" and "permission denied" could not, and retrying them
// only doubles the log noise for a problem a person must fix.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status/100 == 5
}

// call is the shared request path: authorize, send, decode, and turn a refusal
// into Google's own words.
func call(ctx context.Context, token, method, endpoint, contentType string,
	body []byte, out any, op string) error {

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return appint.Retryable(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return appint.Retryable(err)
	}
	if resp.StatusCode/100 != 2 {
		var refusal struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &refusal)
		msg := refusal.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		apiErr := &APIError{Op: op, Status: resp.StatusCode, Message: msg}
		if apiErr.Retryable() {
			return appint.Retryable(apiErr)
		}
		return apiErr
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
