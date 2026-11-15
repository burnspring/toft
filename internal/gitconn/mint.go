package gitconn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"toft/internal/database"
	"toft/internal/safefetch"
)

// The `app` method: sign a JWT, exchange it for an installation token, and use
// that.
//
// **What is at rest is a signing key, worth nothing unless the host agrees to
// mint** — which is the whole difference from the `token` method, where the
// ciphertext opening is repository read access. The mint call accepts a
// repository narrowing, so the token this asks for covers **only the repository
// about to be cloned**: a credential lifted off a build host in the seconds it
// exists reaches one repository for one hour.

const (
	// appJWTLife is how long the signing JWT is good for. GitHub caps it at ten
	// minutes; a minute is all that is needed and leaves room for clock skew.
	appJWTLife = 60 * time.Second
	// mintSkew backdates `iat` so a host clock a few seconds behind does not
	// refuse a freshly minted assertion.
	mintSkew = 30 * time.Second
	// cacheMargin retires a minted token before the host does, so a clone never
	// starts with one that expires mid-fetch.
	cacheMargin = 2 * time.Minute
)

// installationToken returns a token for this connection, narrowed to repo when
// one is given.
//
// The cache is keyed by connection **and** repository, because the narrowing is
// per repository and handing one repository's token to another would quietly
// widen every clone. It is an optimisation and never a correctness mechanism:
// several nodes minting the same token concurrently is fine — GitHub issues one
// per request and they coexist — so nothing here holds a lock across the
// network call.
func (r *Resolver) installationToken(ctx context.Context, conn *database.GitConnection, repo string) (string, error) {
	key := conn.ID + "\n" + repo
	now := time.Now().UTC()

	r.mu.Lock()
	if cached, ok := r.minted[key]; ok && cached.expires.After(now.Add(cacheMargin)) {
		r.mu.Unlock()
		return cached.token, nil
	}
	r.mu.Unlock()

	assertion, err := r.appAssertion(ctx, conn)
	if err != nil {
		return "", err
	}
	token, expires, err := r.exchangeAssertion(ctx, conn, assertion, repo)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	r.minted[key] = mintedToken{token: token, expires: expires}
	r.mu.Unlock()
	return token, nil
}

// appAssertion signs the JWT that proves this install holds the App's key.
func (r *Resolver) appAssertion(ctx context.Context, conn *database.GitConnection) (string, error) {
	pem, err := r.enc.DecryptContext(ctx, conn.EncryptedCloneSecret())
	if err != nil {
		return "", fmt.Errorf("reading the App's private key: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pem))
	if err != nil {
		return "", fmt.Errorf("the stored value is not an RSA private key: %w", err)
	}
	now := time.Now().UTC()
	claims := jwt.RegisteredClaims{
		Issuer:    conn.GitHubAppID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-mintSkew)),
		ExpiresAt: jwt.NewNumericDate(now.Add(appJWTLife)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}

// exchangeAssertion trades the JWT for an installation token, narrowed to repo
// when one is given.
func (r *Resolver) exchangeAssertion(ctx context.Context, conn *database.GitConnection, assertion, repo string) (string, time.Time, error) {
	if conn.InstallationID == "" {
		return "", time.Time{}, fmt.Errorf("the %s connection has no installation id", conn.DisplayName)
	}
	endpoint := apiBase(conn) + "/app/installations/" + conn.InstallationID + "/access_tokens"

	payload := []byte("{}")
	if repo != "" {
		// **Narrowed to the repository being cloned.** GitHub accepts a
		// repository list on the mint, and asking for one is the difference
		// between a credential that reaches this app's code for an hour and one
		// that reaches the whole installation.
		if _, name, ok := splitRepo(repo); ok {
			body, err := json.Marshal(map[string]any{"repositories": []string{name}})
			if err != nil {
				return "", time.Time{}, err
			}
			payload = body
		}
	}

	client := r.clientFor(conn)
	if err := client.CheckConfigured(endpoint); err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Toft")
	req.Header.Set("Authorization", "Bearer "+assertion)

	resp, err := client.For(endpoint).Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The host's status and never its body: a body echoed into an error is
		// how a credential ends up in a log.
		return "", time.Time{}, fmt.Errorf("%s refused to mint a token: %s", conn.DisplayName, resp.Status)
	}
	raw, err := safefetch.ReadLimited(resp.Body, maxHostResponse, "the mint response")
	if err != nil {
		return "", time.Time{}, err
	}
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &minted); err != nil {
		return "", time.Time{}, err
	}
	if minted.Token == "" {
		return "", time.Time{}, fmt.Errorf("%s minted an empty token", conn.DisplayName)
	}
	if minted.ExpiresAt.IsZero() {
		minted.ExpiresAt = time.Now().UTC().Add(time.Hour)
	}
	return minted.Token, minted.ExpiresAt.UTC(), nil
}

// splitRepo pulls owner and name out of a clone URL for the mint's narrowing.
func splitRepo(repoURL string) (owner, name string, ok bool) {
	repo, err := ParseRepo(repoURL)
	if err != nil {
		return "", "", false
	}
	return repo.Owner, repo.Name, true
}
