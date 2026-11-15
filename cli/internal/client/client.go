// Package client is the CLI's side of the platform API.
//
// It is deliberately thin: one HTTP client, one error type that carries the
// server's own sentence, and a struct per response the commands actually read.
// The platform refuses in the administrator's words — "Administrators have
// switched off deploying apps from an uploaded archive on this install." — and a client
// that replaced those with its own would be worse at the one job an error
// message has.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client talks to one install as one identity.
type Client struct {
	BaseURL string
	Token   string
	// Cookie presents Token as a signed-in session rather than a bearer key.
	Cookie bool
	// OnRenew is told when the install renewed the session (see Observe).
	OnRenew func(token string, expires time.Time)
	// OnReconnect is told when a deploy watch lost a stream that had been
	// working and is retrying (WatchProgress), so the caller can say so rather
	// than go quiet. Called once per outage, not once per retry.
	OnReconnect func(err error)
	// UserAgent names the client and its version, so an install's audit log and
	// its per-key usage can tell `toft` apart from a script somebody wrote.
	UserAgent string

	// Context bounds every request: the run's own, cancelled by the first
	// Ctrl-C or SIGTERM. Without it an upload ran to completion after the
	// signal and the install started the deploy the person had just tried to
	// abort (code review 2026-11-11, L-1). Nil is context.Background().
	Context context.Context

	HTTP *http.Client

	// mu guards Token once requests are in flight: while a deploy is watched
	// the build-log follower and the progress stream share this client on two
	// goroutines, and a renewed session is written by whichever answer carried
	// it (security review 2026-11-14, CS-12).
	mu sync.Mutex
}

// CurrentToken is Token, read under the lock that a session renewal writes it
// under. Use it, not the field, wherever a request may be in flight.
func (c *Client) CurrentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Token
}

func (c *Client) ctx() context.Context {
	if c.Context != nil {
		return c.Context
	}
	return context.Background()
}

// New builds a client. The timeout is generous and applies to ordinary calls
// only — the two streaming reads (progress, logs) build their own requests with
// no deadline, because a build that takes twenty minutes is a build, not a hang.
func New(baseURL, token, userAgent string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		UserAgent: userAgent,
		HTTP:      &http.Client{Timeout: 60 * time.Second, CheckRedirect: credentialRedirect},
	}
}

// credentialRedirect is the API client's redirect policy: the credential
// follows a redirect only within the install's origin.
//
// Go's own policy keeps Authorization and Cookie when the target's hostname is
// the first request's or a subdomain of it, whatever the port or scheme — so a
// `toft api` call the platform redirected to an app, which answered with a
// Location on another port of the install's host, re-sent the personal key
// there, or in clear text on an http downgrade (security review 2026-11-14,
// CS-8). Off the origin the two headers are deleted and the redirect followed
// without them. The one exception is the same hostname going http -> https on
// the default ports, for an install configured by its http address.
func credentialRedirect(next *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !credentialMayFollow(via[0].URL, next.URL) {
		next.Header.Del("Authorization")
		next.Header.Del("Cookie")
	}
	return nil
}

func credentialMayFollow(from, to *url.URL) bool {
	if SameOrigin(from, to) {
		return true
	}
	return strings.EqualFold(from.Scheme, "http") && strings.EqualFold(to.Scheme, "https") &&
		strings.EqualFold(from.Hostname(), to.Hostname()) &&
		(from.Port() == "" || from.Port() == "80") && (to.Port() == "" || to.Port() == "443")
}

// Error is a refusal from the platform, carrying its own words.
//
// Code is the machine-readable half where the server sends one
// (`source_not_allowed`, `uploads_not_allowed`, `slow_down`). Commands switch on
// it; humans read Message.
//
// Hint and Retryable are the rest of the envelope every refusal carries: the
// next step in the install's words, and whether trying again could work.
// Retryable is nil when the install did not say — one older than the envelope,
// or a proxy's page in front of it — and [Error.ShouldRetry] answers from the
// status then.
type Error struct {
	Status    int
	Code      string
	Message   string
	Hint      string
	Retryable *bool
	// Reason is a deploy's failure reason, on a refusal made at the upload
	// (manifest_invalid, runtime_version, limit_exceeds_ceiling, …) — what a
	// deploy's result document carries in `reason` once it has a row.
	Reason string
	// DeploymentID is the failed deployment a refusal at the upload recorded,
	// when the install names one — the id `toft logs --deploy` takes.
	DeploymentID string
	// Body is the refusal as the install sent it, for the one caller that
	// passes the install's JSON through rather than rendering it: `toft api`.
	Body []byte
}

// ShouldRetry is the install's answer when it gave one, and otherwise the
// status's: 429 and the 5xx that are not "not implemented".
func (e *Error) ShouldRetry() bool {
	if e.Retryable != nil {
		return *e.Retryable
	}
	return e.Status == http.StatusTooManyRequests ||
		(e.Status >= 500 && e.Status != http.StatusNotImplemented)
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the install answered %d", e.Status)
}

// Unauthorized reports a credential problem rather than a refusal: no token, a
// revoked one, an expired one. It is exit code 3, and it is a different page
// from "the deploy failed".
func (e *Error) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// NotFound is its own question often enough to be worth a method: on this API a
// 404 is as often "you may not see that" or "this install does not offer that"
// as it is "no such thing".
func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// Do issues a request and decodes the answer into out, which may be nil.
func (c *Client) Do(method, path string, body any, out any) error {
	req, err := c.request(method, path, body)
	if err != nil {
		return err
	}
	return c.send(req, out)
}

// DoWithHeaders is Do with headers that belong to one request rather than to
// the client — an Idempotency-Key names one start, and a key set on the client
// would make every later call a duplicate of the first.
func (c *Client) DoWithHeaders(method, path string, header http.Header, body any, out any) error {
	req, err := c.request(method, path, body)
	if err != nil {
		return err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return c.send(req, out)
}

func (c *Client) request(method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(c.ctx(), method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)
	return req, nil
}

// authorize attaches the credential, when there is one. The device-flow routes
// are called with none — deliberately, since they run before there is a
// credential and are mounted where a presented one would be ignored anyway.
func (c *Client) authorize(req *http.Request) {
	req.Header.Set("User-Agent", c.UserAgent)
	token := c.CurrentToken()
	switch {
	case token == "":
	case c.Cookie:
		// A session is the browser's credential and travels the browser's way.
		// No Origin is sent, which is what a non-browser caller looks like to
		// the origin check — and the check exists for browsers, which cannot
		// suppress it.
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	default:
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// The platform's two session cookies (internal/auth/cookies.go): one on the
// API's path, one on the apps gateway's. The same token goes in both.
const (
	SessionCookie    = "toft_session"
	AppSessionCookie = "toft_app_session"
)

func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &UnreachableError{URL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	c.Observe(resp)
	return decode(resp, out)
}

// Observe keeps a session alive across the install's renewals.
//
// A session slides: the install answers a request that is due a renewal with a
// fresh token in Set-Cookie (auth.RenewIfDue), exactly as it does a browser,
// and the old one stops being worth anything at its own expiry. A client that
// kept presenting the token it signed in with would be signed out half an hour
// into an unattended run however busy it had been — so every answer is read
// for a newer one, and OnRenew is told so the next invocation starts from it.
//
// Only an answer from the host the request was sent to is read. A redirect can
// end anywhere, and a Set-Cookie from wherever it ended is that host's choice
// of session, not the install's: taking it stored a stranger's token as ours.
func (c *Client) Observe(resp *http.Response) {
	if !c.Cookie || resp == nil || !answeredWhereSent(resp) {
		return
	}
	for _, ck := range resp.Cookies() {
		if (ck.Name != SessionCookie && ck.Name != AppSessionCookie) || ck.Value == "" {
			continue
		}
		c.mu.Lock()
		changed := ck.Value != c.Token
		c.Token = ck.Value
		c.mu.Unlock()
		if !changed {
			continue
		}
		// Outside the lock: OnRenew writes the stored session, and a slow
		// disk must not hold every other request's authorize.
		if c.OnRenew != nil {
			var expires time.Time
			if ck.MaxAge > 0 {
				expires = time.Now().Add(time.Duration(ck.MaxAge) * time.Second).UTC()
			}
			c.OnRenew(ck.Value, expires)
		}
		return
	}
}

// answeredWhereSent reports whether the final answer came from the scheme and
// host of the first request in its redirect chain.
func answeredWhereSent(resp *http.Response) bool {
	last := resp.Request
	if last == nil || last.URL == nil {
		return false
	}
	first := last
	for first.Response != nil && first.Response.Request != nil {
		first = first.Response.Request
	}
	return SameOrigin(first.URL, last.URL)
}

// SameOrigin is scheme and host (with port) alike, case aside.
func SameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// Locate asks a route that answers with a redirect — a run's output, a
// presigned URL — where it points, without following it: the credential must
// not travel to an object store. A refusal is the install's, as Do's is.
func (c *Client) Locate(path string) (string, error) {
	req, err := c.request(http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	hc := *c.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "", &UnreachableError{URL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	c.Observe(resp)
	if resp.StatusCode >= 400 {
		return "", errorFrom(resp)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", &DecodeError{Err: fmt.Errorf("%s answered %d with no Location", path, resp.StatusCode)}
	}
	return loc.String(), nil
}

func decode(resp *http.Response, out any) error {
	if resp.StatusCode >= 400 {
		return errorFrom(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &DecodeError{Err: err}
	}
	return nil
}

// DecodeError is a 2xx this binary could not read.
//
// The standard library's sentence names a Go type — "cannot unmarshal object
// into Go value of type []main.jobRun" — which describes this program's
// internals to somebody who did not write it and cannot act on it. The cause is
// almost always one thing: the install is newer than the client and answers in
// a shape this version was not built for. Saying *that* names an action, and
// the library's text is kept on the end because when the guess is wrong it is
// the only evidence of what actually arrived.
type DecodeError struct{ Err error }

func (e *DecodeError) Error() string {
	return "this install sent an answer this version of toft does not understand — " +
		"upgrade toft, or check that its version matches the install (" + e.Err.Error() + ")"
}

func (e *DecodeError) Unwrap() error { return e.Err }

func errorFrom(resp *http.Response) error {
	var body struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		Hint      string `json:"hint"`
		Retryable *bool  `json:"retryable"`
		Reason    string `json:"reason"`
		DeployID  string `json:"deployment_id"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	isJSON := json.Unmarshal(raw, &body) == nil
	if !isJSON && gatewayDown(resp.StatusCode) {
		return gatewayError(resp)
	}
	if !isJSON && looksLikeHTML(resp, raw) {
		// A web page is not this API's refusal: the address names some
		// other server, or a path in front of the install. Its HTML echoed
		// as the error said nothing (L4, 2026-11-04 black-box list).
		where := "the server"
		if resp.Request != nil && resp.Request.URL != nil {
			where = resp.Request.URL.Scheme + "://" + resp.Request.URL.Host
		}
		return &Error{Status: resp.StatusCode, Code: "not_toft", Body: raw,
			Message: fmt.Sprintf("%s answered %d with a web page, not Toft's API — is this the install's address?",
				where, resp.StatusCode),
			Hint: "`toft login --status` shows the address in use; check it, or TOFT_URL"}
	}
	if body.Error == "" {
		body.Error = strings.TrimSpace(string(raw))
	}
	return &Error{Status: resp.StatusCode, Code: body.Code, Message: body.Error,
		Hint: body.Hint, Retryable: body.Retryable, Reason: body.Reason, DeploymentID: body.DeployID, Body: raw}
}

// looksLikeHTML is a body served as a page, or one that begins as one.
func looksLikeHTML(resp *http.Response, raw []byte) bool {
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte("<"))
}

// gatewayDown is the three statuses a proxy in front of the install answers with
// while the install itself is not there — a redeploy, a crash, a pod not ready.
func gatewayDown(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

// gatewayError is a 502/503/504 that is not the install's JSON: the proxy's own
// page, which means the install did not answer. Exit 4 with a sentence, never
// the proxy's HTML echoed as the error.
func gatewayError(resp *http.Response) error {
	where := ""
	if resp.Request != nil && resp.Request.URL != nil {
		where = resp.Request.URL.Scheme + "://" + resp.Request.URL.Host
	}
	return &UnreachableError{URL: where, Err: fmt.Errorf("the install answered %d (%s) — "+
		"it may be restarting or redeploying; try again shortly",
		resp.StatusCode, strings.ToLower(http.StatusText(resp.StatusCode)))}
}

// UnreachableError is "the install did not answer", which is exit code 4 and a
// different page in a red pipeline from "the deploy failed".
type UnreachableError struct {
	URL string
	Err error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("could not reach %s: %v", e.URL, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// ShouldRetry is false for a host name that does not resolve: NXDOMAIN is
// the name being wrong, not the install being down, and it answers the same
// on every retry. A timeout, a refused connection or a resolver that could
// not be reached (a DNS timeout, a temporary failure) may pass.
func (e *UnreachableError) ShouldRetry() bool {
	var dns *net.DNSError
	if errors.As(e.Err, &dns) && dns.IsNotFound {
		return false
	}
	return true
}

// UploadProgress is called as the bundle travels, with bytes sent and the
// total. It is called from the writing goroutine, so it must not block.
type UploadProgress func(sent, total int64)

// Upload posts a bundle as multipart/form-data, reporting progress as it goes.
//
// The body is streamed rather than buffered: a 90MB bundle held twice in memory
// on a laptop is the kind of thing that works on the machine it was written on.
// A pipe means the multipart writer and the HTTP client run concurrently, and
// the meter counts what has actually left.
func (c *Client) Upload(path, filename string, content io.Reader, size int64, onProgress UploadProgress, out any) error {
	return c.UploadWithFields(path, filename, content, size, nil, onProgress, out)
}

// UploadWithFields is Upload with form values beside the file.
//
// The create-from-a-bundle route takes the name, the slug and the execution
// mode as form values rather than as a JSON body, because a multipart request
// has nowhere else to put them. They are written before the file part: a server
// reading the form in order should not have to buffer the whole archive to find
// out what the app is called.
func (c *Client) UploadWithFields(path, filename string, content io.Reader, size int64, fields map[string]string, onProgress UploadProgress, out any) error {
	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)

	go func() {
		for k, v := range fields {
			if err := form.WriteField(k, v); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		part, err := form.CreateFormFile("file", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		var sent int64
		buf := make([]byte, 256<<10)
		for {
			n, readErr := content.Read(buf)
			if n > 0 {
				if _, err := part.Write(buf[:n]); err != nil {
					pw.CloseWithError(err)
					return
				}
				sent += int64(n)
				if onProgress != nil {
					onProgress(sent, size)
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				pw.CloseWithError(readErr)
				return
			}
		}
		if err := form.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()

	req, err := http.NewRequestWithContext(c.ctx(), http.MethodPost, c.BaseURL+path, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	c.authorize(req)

	// No client timeout on an upload: the deadline that matters is the user's
	// patience, and a 60-second cap would fail every large bundle on a slow
	// connection at the exact moment the bytes were nearly there.
	httpClient := &http.Client{Transport: c.HTTP.Transport, CheckRedirect: c.HTTP.CheckRedirect}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &UnreachableError{URL: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	c.Observe(resp)
	return decode(resp, out)
}

// Query builds a path with escaped parameters, so a slug with a slash in it
// cannot become a different route.
func Query(path string, params map[string]string) string {
	if len(params) == 0 {
		return path
	}
	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	return path + "?" + v.Encode()
}
