// Package graph is the Microsoft Graph client the SharePoint and OneDrive
// transports share.
//
// It exists because those two are the same mechanism twice: the same tenant
// credential, the same token endpoint, the same drive API, and the same
// upload-session dance above 4 MiB. What differs is which drive — a site's
// default document library, or one person's OneDrive — which is two lines, not
// two packages.
//
// # What this is not
//
// It is not an SDK, and it deliberately brings no dependency. Microsoft's own
// client library is a transitive tree to PUT a file, exactly as a provider's
// mail library would have been for the sending-API transport, and the surface
// used here is four endpoints.
//
// # The credential
//
// Client credentials against Entra: a tenant, an application id and a secret an
// administrator pasted. **There is no browser OAuth**, for the reason
// `auth_providers` and Slack have none — it is a redirect surface, a state
// store and a token-refresh path per provider, and every other
// credential-holding connection on this platform is pasted. Worth doing later,
// and it changes no decision here.
//
// The token is cached per credential and scope until ten minutes before it
// expires (integrations.TokenCache). The key carries a digest of the secret, so
// a rotated secret is a miss rather than a second place the old one keeps
// working; and an app handed a token through /app/tokens holds the one the
// transports use, which is the same reach by construction.
package graph

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"toft/internal/appint"
	"toft/internal/integrations"
)

// The two hosts this package talks to.
//
// **Unexported variables, not settings.** A configurable API base is a way to
// aim a stored credential at an arbitrary server, which is a worse thing to
// hand an administrator than a provider they cannot change — the same rule
// slack.apiBase and mail's Resend endpoint keep. They are variables rather than
// constants only so this package's own tests can point them at a stub, which is
// a door nothing outside the package can open.
var (
	loginHost = "https://login.microsoftonline.com"
	apiBase   = "https://graph.microsoft.com/v1.0"
)

const (
	// Timeout bounds one call.
	Timeout = 60 * time.Second

	// SimpleUploadLimit is Graph's ceiling for a single PUT. Above it a file
	// goes through an upload session, which is why the 25 MiB limit this
	// product sets belongs to the change that sets it rather than to a later
	// one that discovers it in production.
	SimpleUploadLimit = 4 << 20
	// ChunkSize is one slice of an upload session. Graph requires a multiple of
	// 320 KiB for every chunk but the last.
	ChunkSize = 320 * 1024 * 10
)

// httpClient is shared: one connection pool, and the same timeout the other
// transports use. A variable so a test can substitute one that trusts a stub's
// certificate.
var httpClient = &http.Client{Timeout: Timeout}

// Credentials are what an administrator pasted.
type Credentials struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

// From reads the credential fields off a Target. One function so the three
// transports cannot disagree about what the fields are called.
func From(t integrations.Target) Credentials {
	return Credentials{
		TenantID:     t.Config("tenant_id"),
		ClientID:     t.Config("client_id"),
		ClientSecret: t.Credential("client_secret"),
	}
}

// Valid reports whether all three are present, which is the only thing this
// side can check without asking Entra.
func (c Credentials) Valid() error {
	switch {
	case c.TenantID == "":
		return fmt.Errorf("this connection has no directory (tenant) id")
	case c.ClientID == "":
		return fmt.Errorf("this connection has no application (client) id")
	case c.ClientSecret == "":
		return fmt.Errorf("this connection has no client secret")
	}
	return nil
}

// APIError is a failure Graph itself reported, carrying enough to decide
// whether a second attempt could help.
//
// It answers Retryable() rather than being wrapped by the caller so that the
// judgement lives with the transport that knows Graph's vocabulary — the same
// shape slack.APIError has, and the reason neither package imports appint for
// anything but the marker.
type APIError struct {
	Op      string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Microsoft Graph refused %s: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("Microsoft Graph returned HTTP %d from %s", e.Status, e.Op)
}

// Retryable reports whether a second attempt could succeed. A throttle or a
// server error could; "itemNotFound" and "accessDenied" could not, and retrying
// them only doubles the log noise for a problem a person must fix.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status/100 == 5
}

// GraphScope is what every Graph call in this package asks for.
const GraphScope = "https://graph.microsoft.com/.default"

// Token mints an application token for the tenant, for Graph.
func Token(ctx context.Context, c Credentials) (string, error) {
	return TokenFor(ctx, c, GraphScope)
}

// TokenFor mints an application token for the tenant, for one resource.
//
// The same client-credentials grant against the same login host serves Graph
// and the Bot Connector (`https://api.botframework.com/.default`); only the
// scope differs, so the host stays one unexported variable rather than a copy in
// each package that needs a token.
func TokenFor(ctx context.Context, c Credentials, scope string) (string, error) {
	tok, err := Mint(ctx, c, scope)
	return tok.Value, err
}

// tokens is the package's cache, one for every connection on the install.
var tokens = integrations.NewTokenCache()

// Mint returns an application token for one resource and when it expires,
// from the cache while it has more than integrations.TokenMargin left.
func Mint(ctx context.Context, c Credentials, scope string) (integrations.AccessToken, error) {
	if err := c.Valid(); err != nil {
		return integrations.AccessToken{}, err
	}
	key := integrations.TokenKey(loginHost, c.TenantID, c.ClientID, c.ClientSecret, scope)
	return tokens.Get(ctx, key, func(ctx context.Context) (integrations.AccessToken, error) {
		return mint(ctx, c, scope)
	})
}

// mint asks Entra for a token, uncached.
func mint(ctx context.Context, c Credentials, scope string) (integrations.AccessToken, error) {
	fail := func(err error) (integrations.AccessToken, error) { return integrations.AccessToken{}, err }
	form := url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"scope":         {scope},
		"grant_type":    {"client_credentials"},
	}
	endpoint := loginHost + "/" + url.PathEscape(c.TenantID) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// The clock starts before the request, so the expiry recorded is never
	// later than Entra's own.
	issued := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		return fail(appint.Retryable(err))
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode/100 != 2 || body.AccessToken == "" {
		msg := body.ErrorDescription
		if msg == "" {
			msg = body.Error
		}
		// A refusal with no OAuth error fields says only its status: a token
		// refusal repeats its OAuth error fields, never its body.
		//
		// Entra's own words, first line only: the description is a paragraph
		// with a correlation id and a stack of URLs, and the first line is the
		// part an administrator acts on.
		if i := strings.IndexAny(msg, "\r\n"); i > 0 {
			msg = msg[:i]
		}
		return fail(&APIError{Op: "the token endpoint", Status: resp.StatusCode, Message: msg})
	}
	return integrations.AccessToken{
		Value:     body.AccessToken,
		ExpiresAt: integrations.ExpiresIn(issued, body.ExpiresIn),
	}, nil
}

// Resource is what a Graph token is for, as an app's SDK names it.
const Resource = "https://graph.microsoft.com"

// FileRoles are the application permissions a Graph token may carry and still
// be handed to an app. **The list is the specification**: each one reaches only
// what a tenant administrator granted per site, list or folder, so the grant is
// the fence. Anything else — Mail.Send, Sites.Read.All, Files.ReadWrite.All —
// reaches the tenant, and a token carrying it is refused on every mint, because
// a permission can be added to the Entra app at any time without Toft hearing.
var FileRoles = []string{
	"Sites.Selected",
	"Files.SelectedOperations.Selected",
	"ListItems.SelectedOperations.Selected",
	"Lists.SelectedOperations.Selected",
}

// TokenRoles reads the `roles` claim off a Graph access token.
//
// Read, not verified: the token is Toft's own, minted a moment ago from the
// tenant's login host over TLS, and what is being asked is what it would let an
// app do, not whether to trust it.
func TokenRoles(token string) ([]string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("Entra returned a token Toft cannot read the permissions of")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("Entra returned a token Toft cannot read the permissions of")
	}
	var claims struct {
		Roles []string `json:"roles"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, errors.New("Entra returned a token Toft cannot read the permissions of")
	}
	return claims.Roles, nil
}

// CheckFileRoles refuses a token carrying any role outside FileRoles, with a
// *integrations.TokenTooWideError naming them. A token with no roles reaches
// nothing and passes.
func CheckFileRoles(token string) error {
	roles, err := TokenRoles(token)
	if err != nil {
		return err
	}
	var wide []string
	for _, r := range roles {
		if !slices.Contains(FileRoles, r) {
			wide = append(wide, r)
		}
	}
	if len(wide) > 0 {
		return &integrations.TokenTooWideError{Roles: wide}
	}
	return nil
}

// Client is an authenticated Graph session for one call chain.
type Client struct {
	token string
}

// Connect mints a token and returns a client.
func Connect(ctx context.Context, c Credentials) (*Client, error) {
	token, err := Token(ctx, c)
	if err != nil {
		return nil, err
	}
	return &Client{token: token}, nil
}

// Get reads one resource into out.
func (c *Client) Get(ctx context.Context, op, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, op, out)
}

func (c *Client) do(req *http.Request, op string, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
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
		return newAPIError(op, resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func newAPIError(op string, status int, raw []byte) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := body.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	err := &APIError{Op: op, Status: status, Code: body.Error.Code, Message: msg}
	if err.Retryable() {
		return appint.Retryable(err)
	}
	return err
}

// ------------------------------------------------------------------- drives

// Item is what a drive returns about a file or folder. Only the fields this
// product records are modelled: a general Graph type would be a lot of surface
// for four endpoints.
type Item struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	WebURL string `json:"webUrl"`
	Size   int64  `json:"size"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder,omitempty"`
}

type itemList struct {
	Value    []Item `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// folderPageCap bounds the paging of a folder menu, as slack's channelPageCap
// does: an unbounded loop against someone else's API is how a settings page
// hangs.
const folderPageCap = 10

// Site is a SharePoint site, as Graph resolves it from a hostname and a path.
type Site struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
	WebURL      string `json:"webUrl"`
}

// ResolveSite turns `contoso.sharepoint.com/sites/finance` into a site.
//
// The site is *pasted* rather than picked from a list, and that is a property
// of how Microsoft grants this rather than a shortcut. `Sites.Selected` is
// granted per site by a tenant administrator and offers no "sites I may touch"
// endpoint, so there is nothing to enumerate — which is why the SharePoint
// kind's scope is welded and why this read is also the proof that the grant
// exists.
func (c *Client) ResolveSite(ctx context.Context, spec string) (*Site, error) {
	host, path := splitSiteSpec(spec)
	if host == "" {
		return nil, fmt.Errorf(
			"a SharePoint site looks like contoso.sharepoint.com/sites/finance")
	}
	target := "/sites/" + url.PathEscape(host)
	if path != "" {
		target += ":/" + path + ":"
	}
	var site Site
	if err := c.Get(ctx, "the site", target, &site); err != nil {
		return nil, err
	}
	return &site, nil
}

// splitSiteSpec accepts what people actually paste: a bare host and path, or
// the whole URL out of a browser's address bar.
func splitSiteSpec(spec string) (host, path string) {
	spec = strings.TrimSpace(spec)
	spec = strings.TrimPrefix(spec, "https://")
	spec = strings.TrimPrefix(spec, "http://")
	spec = strings.Trim(spec, "/")
	if spec == "" {
		return "", ""
	}
	host, path, _ = strings.Cut(spec, "/")
	return host, strings.Trim(path, "/")
}

// SiteDrive is a site's default document library.
//
// One library and not a picker: "the default document library" is what people
// mean by "the site's files", and a second axis of choice on a scope that is
// already welded would be a second thing an administrator has to get right for
// no gain that anybody asked for.
func (c *Client) SiteDrive(ctx context.Context, siteID string) (string, error) {
	var drive struct {
		ID string `json:"id"`
	}
	if err := c.Get(ctx, "the site's document library",
		"/sites/"+url.PathEscape(siteID)+"/drive", &drive); err != nil {
		return "", err
	}
	return drive.ID, nil
}

// UserDrive is one person's OneDrive, by user principal name or object id.
func (c *Client) UserDrive(ctx context.Context, user string) (id, label string, err error) {
	var drive struct {
		ID    string `json:"id"`
		Owner struct {
			User struct {
				DisplayName string `json:"displayName"`
				Email       string `json:"email"`
			} `json:"user"`
		} `json:"owner"`
	}
	if err := c.Get(ctx, "that user's OneDrive",
		"/users/"+url.PathEscape(user)+"/drive", &drive); err != nil {
		return "", "", err
	}
	label = drive.Owner.User.DisplayName
	if label == "" {
		label = user
	}
	return drive.ID, label, nil
}

// Folders lists the folders directly under a drive's root, for the destination
// picker.
//
// One level, deliberately. A recursive tree of an organization's document
// library is a lot of requests and a menu nobody can read, and the useful
// choice is "which of the folders at the top of this library" — a nested one is
// a case for a second connection scoped to the site that owns it.
//
// It pages: the menu is also the validator, so a folder past the first page
// would otherwise be one nobody could choose. A next link is followed only
// when it is under apiBase — it carries the bearer token, and Graph's answer
// must not be what decides which host receives it.
func (c *Client) Folders(ctx context.Context, driveID string) ([]Item, error) {
	path := "/drives/" + url.PathEscape(driveID) + "/root/children?$select=id,name,webUrl,folder&$top=200"
	var out []Item
	for page := 0; page < folderPageCap && path != ""; page++ {
		var list itemList
		if err := c.Get(ctx, "the library's folders", path, &list); err != nil {
			return nil, err
		}
		for _, item := range list.Value {
			if item.Folder != nil {
				out = append(out, item)
			}
		}
		path = ""
		if list.NextLink != "" {
			rest, ok := strings.CutPrefix(list.NextLink, apiBase+"/")
			if !ok {
				return nil, errors.New("Graph answered the library's folders with a next page on another host")
			}
			path = "/" + rest
		}
	}
	return out, nil
}

// Upload puts one file into a drive, under a folder, and **never replaces**.
//
// `conflictBehavior: rename` on every path, and it is not configurable. An app
// cannot overwrite a file — one it did not write, or one it did — which is what
// keeps ADR §25's compromise story at *leaks its own data* rather than letting
// it become *edits the organization's*. What that costs is that a daily report
// under a fixed name accumulates `report 1.csv`, `report 2.csv`; that is
// visible, recoverable and somebody's filing problem, which overwriting is not.
func (c *Client) Upload(ctx context.Context, driveID, folderID, name string,
	content []byte, contentType string) (*Item, error) {

	if len(content) <= SimpleUploadLimit {
		return c.simpleUpload(ctx, driveID, folderID, name, content, contentType)
	}
	return c.sessionUpload(ctx, driveID, folderID, name, content)
}

// itemPath is where a file goes: under a chosen folder, or at the root of the
// library when nobody chose one.
func itemPath(driveID, folderID, name string) string {
	base := "/drives/" + url.PathEscape(driveID)
	if folderID != "" {
		return base + "/items/" + url.PathEscape(folderID) + ":/" + url.PathEscape(name) + ":"
	}
	return base + "/root:/" + url.PathEscape(name) + ":"
}

func (c *Client) simpleUpload(ctx context.Context, driveID, folderID, name string,
	content []byte, contentType string) (*Item, error) {

	endpoint := apiBase + itemPath(driveID, folderID, name) +
		"/content?%40microsoft.graph.conflictBehavior=rename"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	var item Item
	if err := c.do(req, "the upload", &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// sessionUpload is the large-file path: create a session, PUT ranges into it.
//
// **A dying session leaves a partial session, not a partial file.** Graph does
// not commit an item until the last range lands, so an interrupted upload is a
// session that expires rather than a truncated spreadsheet somebody opens. That
// is why this is the one place a failure mid-way is safe to retry as a whole.
func (c *Client) sessionUpload(ctx context.Context, driveID, folderID, name string,
	content []byte) (*Item, error) {

	create := apiBase + itemPath(driveID, folderID, name) + "/createUploadSession"
	body, _ := json.Marshal(map[string]any{
		"item": map[string]string{"@microsoft.graph.conflictBehavior": "rename"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, create, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := c.do(req, "the upload session", &session); err != nil {
		return nil, err
	}
	if session.UploadURL == "" {
		return nil, &APIError{Op: "the upload session", Message: "Graph returned no upload URL"}
	}

	total := int64(len(content))
	for start := int64(0); start < total; start += ChunkSize {
		end := start + ChunkSize
		if end > total {
			end = total
		}
		chunk := content[start:end]

		// The upload URL carries its own short-lived credential in the query
		// string, so the bearer token is deliberately *not* sent with it —
		// Graph rejects a request that carries both.
		put, err := http.NewRequestWithContext(ctx, http.MethodPut, session.UploadURL,
			bytes.NewReader(chunk))
		if err != nil {
			return nil, err
		}
		put.Header.Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, end-1, total))
		put.ContentLength = int64(len(chunk))

		resp, err := httpClient.Do(put)
		if err != nil {
			// The upload URL is the credential, and *url.Error's text is it.
			return nil, appint.Retryable(integrations.RedactURL(err))
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusAccepted:
			// More ranges wanted; carry on.
		case resp.StatusCode/100 == 2:
			var item Item
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			return &item, nil
		default:
			return nil, newAPIError("the upload session", resp.StatusCode, raw)
		}
	}
	return nil, &APIError{
		Op:      "the upload session",
		Message: "every range was accepted and Graph never committed the file",
	}
}
