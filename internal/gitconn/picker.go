package gitconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/database"
	"toft/internal/safefetch"
)

// The picker credential: a person's own token against the host.
//
// **It is a different type from `Credential`, and there is no conversion
// between them.** Listing repositories with the *clone* credential is the
// shortcut this whole design exists to refuse: an install-wide credential sees
// everything the organization exposed, so any signed-in developer would get a
// directory of the company's private repositories, and nothing in the product
// would look wrong. The picker shows a person what they can already see.
//
// The two halves each answer a question the other cannot. The install's
// credential answers *what may this platform clone*, which must not die with
// the person who set it up. A person's answers *what may this developer be
// shown*, which must not be the estate.
//
// **The picker exists only where the connection can sign a person in.** A
// GitHub App is an OAuth client — the same artifact doing a second job at no
// extra cost to the administrator — and a pasted token is not. That is the whole
// rule: no optional field, nothing else to register, and a token-backed
// connection is not a degraded one, it is the same feature with a URL field
// where the list would be.

// pickerToken is one person's access token against one connection. Its own
// type so it cannot be passed where a clone credential is expected, or
// returned where one is.
type pickerToken string

// ErrNoPickerIdentity means this person has not signed in to the host yet.
var ErrNoPickerIdentity = errors.New("no host identity for this person")

// AuthorizeURL is where the browser is sent to sign this person in to the host,
// and the state the callback must match.
func (r *Resolver) AuthorizeURL(conn *database.GitConnection, redirectURI string) (string, string, error) {
	if !conn.HasPicker() {
		return "", "", fmt.Errorf("the %s connection has no repository picker", conn.DisplayName)
	}
	state := uuid.NewString()
	q := url.Values{}
	q.Set("client_id", conn.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	// **Read, and only read.** The platform never pushes, opens a pull request,
	// writes a commit status or creates a repository; a connection that can
	// write is a different security review.
	q.Set("scope", "read:user")
	return strings.TrimRight(conn.BaseURL, "/") + "/login/oauth/authorize?" + q.Encode(), state, nil
}

// ExchangeCode turns the callback's code into this person's stored token.
func (r *Resolver) ExchangeCode(ctx context.Context, conn *database.GitConnection, userID, code, redirectURI string) error {
	secret, err := r.enc.DecryptContext(ctx, conn.EncryptedClientSecret())
	if err != nil {
		return fmt.Errorf("reading the connection's OAuth secret: %w", err)
	}
	endpoint := strings.TrimRight(conn.BaseURL, "/") + "/login/oauth/access_token"

	form := url.Values{}
	form.Set("client_id", conn.ClientID)
	form.Set("client_secret", secret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	client := r.clientFor(conn)
	if err := client.CheckConfigured(endpoint); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Toft")

	resp, err := client.For(endpoint).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s refused the sign-in: %s", conn.DisplayName, resp.Status)
	}
	raw, err := safefetch.ReadLimited(resp.Body, maxHostResponse, "the sign-in response")
	if err != nil {
		return err
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	if body.AccessToken == "" {
		// The host's own error code, which names the problem without carrying
		// anything secret.
		if body.Error != "" {
			return fmt.Errorf("%s refused the sign-in: %s", conn.DisplayName, body.Error)
		}
		return fmt.Errorf("%s returned no access token", conn.DisplayName)
	}

	ciphertext, err := r.enc.EncryptContext(ctx, body.AccessToken)
	if err != nil {
		return err
	}
	var expires *time.Time
	if body.ExpiresIn > 0 {
		t := time.Now().UTC().Add(time.Duration(body.ExpiresIn) * time.Second)
		expires = &t
	}
	return r.db.PutGitConnectionIdentity(conn.ID, userID, ciphertext, expires)
}

// pickerTokenFor reads this person's stored token, or ErrNoPickerIdentity.
//
// **The only caller is the repository listing.** Nothing else may reach it, and
// nothing turns it into a `Credential`.
func (r *Resolver) pickerTokenFor(ctx context.Context, connectionID, userID string) (pickerToken, error) {
	identity, err := r.db.GetGitConnectionIdentity(connectionID, userID)
	if err != nil {
		return "", err
	}
	if identity == nil {
		return "", ErrNoPickerIdentity
	}
	if identity.ExpiresAt != nil && identity.ExpiresAt.Before(time.Now().UTC()) {
		return "", ErrNoPickerIdentity
	}
	token, err := r.enc.DecryptContext(ctx, identity.EncryptedToken())
	if err != nil {
		return "", ErrNoPickerIdentity
	}
	return pickerToken(token), nil
}

// ListRepositoriesFor is the picker's list: what **this person** can see,
// through their own credential.
// hasMore says the list was cut short (SearchRepositories).
func (r *Resolver) ListRepositoriesFor(ctx context.Context, conn *database.GitConnection, userID, query string, page int) ([]Repository, bool, error) {
	token, err := r.pickerTokenFor(ctx, conn.ID, userID)
	if err != nil {
		return nil, false, err
	}
	return r.SearchRepositories(ctx, conn, token, query, page)
}
