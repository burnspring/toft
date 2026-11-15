package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

// `toft fetch` — one request to the deployed app, as you.
//
// **The question after every deploy is "does it answer", and the honest way to
// ask it had three traps.** The URL is the install's answer and not a
// derivation (apps_origin.go); a private app answers an anonymous request with
// "authentication required", which reads as broken; and the key has to go in a
// header the gateway consumes, which nobody guesses. The skill taught a
// `toft status --json | jq -r .url` and `curl -H "Authorization: Bearer …"`
// pipeline, which needs jq, needs the token in the shell — the one thing this
// client never hands over — and was copied wrong often enough to be a verb.
//
// The credential goes only to the address the install named for its apps, and
// the gateway strips it before the request reaches the app.
//
// **The body is bounded by default.** The reader is usually an agent, and a
// page of minified JavaScript printed into its context is the most expensive
// way to learn that the deploy worked. --max-body 0 lifts the bound.

const fetchUsage = `toft fetch [path] — request a path on the deployed app, as you

Prints the status line, then the body. Exits 0 for a 2xx or 3xx answer and 1
otherwise, so it can stand in a script as the check that a deploy works. Under
--json a failing status (or one --expect did not name) adds a final error
object, code unexpected_status. An address that answers nothing is exit 4:
unreachable, or hostname_unresolved when the app's own hostname has no DNS.
The app's Set-Cookie headers are shown (several are newline-separated under
--json); the platform's own session cookies never are.

Flags:
  --app <slug>        Which app
  --method <verb>     The request method (default GET)
  --data <body>       A request body; @file reads it from a file, @- from stdin
  --form k=v          A multipart/form-data field; k=@file sends a file. Repeat
                      for more; implies POST and is not combined with --data
  -o, --output <file> Write the body to a file rather than printing it — whole,
                      with no --max-body bound, and only when the app answered
                      2xx or 3xx (a refusal is printed and no file is written).
                      An existing file is replaced only with --force; -o - is
                      the whole body on stdout
  --force             Let --output replace a file that exists
  --header 'K: V'     A request header; repeat for more
  --max-body <n>      Print at most n bytes of the body (default 8192; 0 is all)
  --timeout <d>       How long to wait for the app's answer (default 60s)
  --expect <code>     Exit 1 unless the status is exactly this
  --anonymous         Send no credential — see what a stranger sees
  --session           As the stored password session — a signed-in browser
  --no-follow         Answer with a redirect itself rather than where it leads
  --json              The status, headers and body as one JSON document

A redirect is followed, and each one is printed before the answer it led to
(under --json, "redirects" and "final_url"), so a 404 at the end of a 303 does
not read as the path's own. A redirect off the install's apps address is not
followed: the answer is the redirect itself, and its Location says where.

The path is from the app's root: toft fetch /healthz. An app on a hostname of
its own is requested there, the address toft status names. Your key is sent to
this install's apps address, or that hostname, only, and the gateway removes it
before the request reaches the app.
`

const defaultFetchMaxBody = 8 << 10

type fetchRequest struct {
	Method    string
	Path      string
	Body      []byte
	Headers   http.Header
	Anonymous bool
	MaxBody   int64
	// Timeout bounds the whole request; zero keeps the client's 60s.
	Timeout time.Duration
	// NoFollow answers with a redirect rather than following it.
	NoFollow bool
	// Sink, when set, receives the whole body instead of Body, unbounded;
	// Bytes still counts it. Used by --output.
	Sink io.Writer
}

// fetchRedirect is one redirect followed on the way to the answer.
type fetchRedirect struct {
	Status   int    `json:"status"`
	URL      string `json:"url"`
	Location string `json:"location"`
}

type fetchResult struct {
	URL       string            `json:"url"`
	Status    int               `json:"status"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	Bytes     int64             `json:"bytes"`
	Truncated bool              `json:"truncated"`
	Millis    int64             `json:"elapsed_ms"`
	// Redirects are the hops followed, in order, [] when there were none;
	// FinalURL is where the answer came from — URL itself when nothing
	// redirected. URL stays the address requested.
	Redirects []fetchRedirect `json:"redirects"`
	FinalURL  string          `json:"final_url"`
	// Platform names the platform's own page answering in the app's place —
	// "stopped", "locked", "deploy_failed", the install's X-Toft-Interstitial
	// key — when that is what came back. Absent when the app answered.
	Platform string `json:"platform_page,omitempty"`
	// Output is the file --output wrote the body to; Body is then empty.
	Output string `json:"output,omitempty"`
	// DeployKey is that the credential sent was a deploy key, which the
	// gateway never takes — so a 401 is not "drop --anonymous" (BB-111).
	DeployKey bool `json:"-"`
}

// platformStates maps the heading on each of the gateway's interstitial pages
// (internal/api/proxy_handler.go) to the key its X-Toft-Interstitial header
// carries — for an install from before the header, which is read by content.
var platformStates = map[string]string{"Stopped": "stopped", "Locked": "locked",
	"Building": "building", "Deploy failed": "deploy_failed", "Out of memory": "out_of_memory",
	"Crashed": "crashed", "Sleeping": "sleeping", "Waking": "waking",
	"Unavailable": "unavailable", "Runs jobs": "batch"}

// platformPage recognises the gateway's interstitial and returns its state as
// the install names it ("stopped", "deploy_failed"). The header is the answer;
// an install older than the header is recognised by a 503 of HTML whose title
// is "<app> — <state>".
func platformPage(r *fetchResult) string {
	if key := r.Headers[interstitialHeader]; key != "" {
		return key
	}
	// Two more of the gateway's own answers, which carry no header (BB-014):
	// its refusal of a request with no credential for an app that is not
	// public — the app is never asked — and its run status pages under
	// /__toft/, which are the platform's whatever the app serves.
	if r.Status == http.StatusUnauthorized && strings.TrimSpace(r.Body) == gatewaySignInRefusal {
		return "sign_in_required"
	}
	if !r.ok() && isPlatformPath(r.FinalURL, r.URL) {
		return "run_page"
	}
	if r.Status != http.StatusServiceUnavailable || !strings.HasPrefix(r.Headers["Content-Type"], "text/html") {
		return ""
	}
	for heading, key := range platformStates {
		if strings.Contains(r.Body, " — "+heading+"</title>") {
			return key
		}
	}
	return ""
}

func (r *fetchResult) ok() bool { return r.Status >= 200 && r.Status < 400 }

// gatewaySignInRefusal is the body of the gateway's 401 to a request with no
// credential for an app that is not public (internal/api/proxy_handler.go).
const gatewaySignInRefusal = "authentication required"

// isPlatformPath is whether the address is a run status page —
// /__toft/runs/<id> — which the gateway serves itself and never hands to the
// app. Only with an id: /__toft/runs/, /__toft/runs and /__toft/ are proxied to
// the app, and the app's own 404 there is the app's (BB-014, third list).
func isPlatformPath(urls ...string) bool {
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		_, id, found := strings.Cut(u.Path, "/__toft/runs/")
		return found && strings.Trim(id, "/") != ""
	}
	return false
}

// fetchedHeaders are the response headers worth a reader's attention. All of
// them would be noise; these say what came back and where it would send you.
var fetchedHeaders = []string{"Content-Type", "Content-Length", "Location", "Retry-After", "WWW-Authenticate", interstitialHeader}

// interstitialHeader is set by the gateway on its own page answering in an
// app's place, and on nothing else.
const interstitialHeader = "X-Toft-Interstitial"

// fetchApp makes one request to the deployed app.
func (s *session) fetchApp(ctx context.Context, app *appSummary, req fetchRequest) (*fetchResult, error) {
	path := req.Path
	if path == "" {
		path = "/"
	}
	if err := refuseAbsoluteURL(path); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if err := refuseDotSegments(path); err != nil {
		return nil, err
	}
	base, ownHost := s.fetchBase(app)
	url := strings.TrimRight(base, "/") + path
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, orDefault(req.Method, http.MethodGet), url, body)
	if err != nil {
		return nil, usageCodef("invalid_path", "%q is not a path toft can request: %v", req.Path, err)
	}
	for k, vs := range req.Headers {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	hr.Header.Set("User-Agent", s.client.UserAgent)
	token := s.client.CurrentToken()
	switch {
	case req.Anonymous || token == "":
	case !s.client.Cookie && config.IsDeployKeyToken(token):
		// A deploy key deploys and never opens, so the gateway treats a
		// request under one as anonymous either way (deployKeyNeverOpens).
		// Not sent at all: an install from before the gateway stripped it
		// would hand the install-wide owner credential to the app's code.
	case s.client.Cookie:
		// Under --session the request is a signed-in browser's: the gateway
		// resolves the person and asserts them to the app, so who() answers
		// with a name rather than the None a key gets.
		hr.AddCookie(&http.Cookie{Name: client.AppSessionCookie, Value: token})
		// On an app's own hostname the gateway reads that host's cookie,
		// which is __Host- prefixed when the install's cookies are secure —
		// something only the install knows. It reads one name and ignores the
		// other, so both go (BB-006).
		if ownHost {
			hr.AddCookie(&http.Cookie{Name: "__Host-" + client.AppSessionCookie, Value: token})
		}
	default:
		hr.Header.Set("Authorization", "Bearer "+token)
	}

	started := time.Now()
	// A copy, so one fetch's patience and its redirect policy do not become
	// every later call's.
	c := *s.client.HTTP
	if req.Timeout > 0 {
		c.Timeout = req.Timeout
	}
	var hops []fetchRedirect
	c.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if req.NoFollow {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		// The credential goes to the install's apps address and nowhere else,
		// and a redirect off it is the app's choice of where to send us: the
		// answer is the redirect itself, printed with its Location.
		if !client.SameOrigin(via[0].URL, next.URL) {
			return http.ErrUseLastResponse
		}
		hop := fetchRedirect{URL: via[len(via)-1].URL.String(), Location: next.URL.String()}
		if next.Response != nil {
			hop.Status = next.Response.StatusCode
		}
		hops = append(hops, hop)
		return nil
	}
	resp, err := c.Do(hr)
	if err == nil {
		s.client.Observe(resp)
	}
	if err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
			// Coded and retryable: the app may answer the next one (BB-037).
			return nil, &failedError{msg: fmt.Sprintf("the app at %s did not answer within %s",
				url, orDuration(req.Timeout, c.Timeout)), code: "timeout", retryable: true,
				hint: "try again, or pass a longer --timeout; `toft logs` shows what the app was doing"}
		}
		return nil, appUnreachable(url, ownHost, app.Slug, err)
	}
	defer resp.Body.Close()

	limit := req.MaxBody
	var reader io.Reader = resp.Body
	if limit > 0 {
		reader = io.LimitReader(resp.Body, limit+1)
	}
	var raw []byte
	var sunk int64
	if req.Sink != nil {
		sunk, err = io.Copy(req.Sink, resp.Body)
	} else {
		raw, err = io.ReadAll(reader)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the app's answer: %w", err)
	}
	// Both always present, [] and the URL itself when nothing redirected, so
	// a reader never tests for a key to learn that (BB-040).
	if hops == nil {
		hops = []fetchRedirect{}
	}
	result := &fetchResult{URL: url, Status: resp.StatusCode, Headers: map[string]string{},
		Millis: time.Since(started).Milliseconds(), Redirects: hops, FinalURL: resp.Request.URL.String(),
		DeployKey: !req.Anonymous && !s.client.Cookie && config.IsDeployKeyToken(s.client.Token)}
	if req.Sink != nil {
		result.Bytes = sunk
	} else if limit > 0 && int64(len(raw)) > limit {
		raw = raw[:limit]
		result.Truncated = true
		n, _ := io.Copy(io.Discard, resp.Body)
		result.Bytes = limit + 1 + n
	} else {
		result.Bytes = int64(len(raw))
	}
	result.Body = string(raw)
	for _, h := range fetchedHeaders {
		if v := resp.Header.Get(h); v != "" {
			result.Headers[h] = v
		}
	}
	if cookies := appSetCookies(resp.Header); len(cookies) > 0 {
		result.Headers["Set-Cookie"] = strings.Join(cookies, "\n")
	}
	return result, nil
}

// appSetCookies are the app's own Set-Cookie headers, as the gateway passed
// them on (path rewritten under /apps/<slug>) — what a browser would store.
// **The platform's own are left out**: a `toft_`-named cookie, `__Host-` or
// not, is a session the gateway renewed, and its value is a credential this
// client never prints (BB-103, 2026-11-04 black-box list). Several are joined
// with a newline, the one character no cookie can carry.
func appSetCookies(h http.Header) []string {
	var out []string
	for _, c := range h.Values("Set-Cookie") {
		name, _, _ := strings.Cut(c, "=")
		name = strings.TrimPrefix(strings.TrimSpace(name), "__Host-")
		name = strings.TrimPrefix(name, "__Secure-")
		if strings.HasPrefix(name, platformCookiePrefix) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// platformCookiePrefix is the install's own cookie names' prefix
// (internal/auth.CookiePrefix): its sessions, never an app's.
const platformCookiePrefix = "toft_"

// appUnreachable is a fetch that never reached anything that could answer —
// a name that does not resolve, a connection refused. **Exit 4 and coded**, as
// every transport failure in this client is (BB-101, 2026-11-04 black-box
// list): before it, it was an uncoded exit 1 that read as the app failing.
//
// An app's own hostname whose name does not resolve is its own code,
// hostname_unresolved: the install switched the name on, and DNS for it does
// not point anywhere yet — an administrator's DNS record, not the app or the
// install, and no retry changes it.
func appUnreachable(rawURL string, ownHost bool, slug string, err error) error {
	host := rawURL
	if u, perr := url.Parse(rawURL); perr == nil {
		host = u.Hostname()
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	var dns *net.DNSError
	notFound := errors.As(err, &dns) && dns.IsNotFound
	switch {
	case notFound && ownHost:
		return &appUnreachableError{
			msg:  fmt.Sprintf("the app's hostname %s does not resolve (%v) — its DNS does not point at this install yet", host, err),
			code: "hostname_unresolved",
			hint: "point " + host + "'s DNS at this install's apps address; until it resolves, " +
				"`toft hostname --off --app " + slug + "` serves the app at the apps address again",
			err: err,
		}
	case notFound:
		return &appUnreachableError{
			msg:  fmt.Sprintf("the app at %s did not answer: %s does not resolve (%v)", rawURL, host, err),
			code: "unreachable",
			hint: "the install's apps address does not resolve from here — `toft status --app " + slug + "` names it",
			err:  err,
		}
	}
	return &appUnreachableError{
		msg:  fmt.Sprintf("the app at %s did not answer: %v", rawURL, err),
		code: "unreachable", retryable: true,
		hint: "retry once it answers; `toft status --app " + slug + "` says whether the app is running",
		err:  err,
	}
}

// appUnreachableError is a fetch that reached nothing: exit 4, kind
// unreachable, with its own code and next step.
type appUnreachableError struct {
	msg, code, hint string
	retryable       bool
	err             error
}

func (e *appUnreachableError) Error() string { return e.msg }
func (e *appUnreachableError) Unwrap() error { return e.err }

// fetchBase is the address a fetch goes to: the app's own hostname when the
// install says it answers on one, and the apps address otherwise. Requested at
// /apps/<slug>/, an app on a hostname answers only a 308 to it (BB-006). The
// hostname is the install's to name — an administrator set it, and its DNS
// points at this install's gateway — so the credential going there is still
// the install's apps address, on another name.
func (s *session) fetchBase(app *appSummary) (string, bool) {
	gateway := s.appURL(app)
	if app.PublicURL == "" {
		return gateway, false
	}
	pub, err := url.Parse(app.PublicURL)
	gw, gerr := url.Parse(gateway)
	if err != nil || gerr != nil || pub.Host == "" || (pub.Scheme != "http" && pub.Scheme != "https") {
		return gateway, false
	}
	if strings.EqualFold(pub.Host, gw.Host) || strings.Trim(pub.Path, "/") != "" {
		return gateway, false
	}
	return strings.TrimRight(app.PublicURL, "/"), true
}

// headerList collects repeated --header 'K: V'.
type headerList struct{ h http.Header }

func (l *headerList) String() string { return "" }

func (l *headerList) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("--header takes 'Name: value', not %q", v)
	}
	if l.h == nil {
		l.h = http.Header{}
	}
	l.h.Add(strings.TrimSpace(k), strings.TrimSpace(val))
	return nil
}

var _ flag.Value = (*headerList)(nil)

func cmdFetch(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("fetch", fetchUsage)
	g.register(fs)
	method := fs.String("method", http.MethodGet, "the request method")
	data := fs.String("data", "", "a request body; @file or @-")
	var headers headerList
	fs.Var(&headers, "header", "a request header, 'Name: value'")
	maxBody := fs.Int64("max-body", defaultFetchMaxBody, "print at most this many bytes of the body")
	timeout := fs.Duration("timeout", 60*time.Second, "how long to wait for the app's answer")
	expect := fs.Int("expect", 0, "exit 1 unless the status is this")
	anonymous := fs.Bool("anonymous", false, "send no credential")
	noFollow := fs.Bool("no-follow", false, "answer with a redirect rather than following it")
	var form formList
	fs.Var(&form, "form", "a multipart field, k=v or k=@file")
	output := fs.String("output", "", "write the body to this file")
	fs.StringVar(output, "o", "", "write the body to this file")
	force := fs.Bool("force", false, "let --output replace an existing file")
	path, rest := takeOperand(args)
	if err := parseAfter(fs, rest, path); err != nil {
		return skipHelp(err)
	}
	path = operandOr(path, fs)

	var body []byte
	if *data != "" {
		var err error
		switch {
		case *data == "@-":
			body, err = readAllCtx(e.ctx, os.Stdin)
		case strings.HasPrefix(*data, "@"):
			body, err = os.ReadFile((*data)[1:])
		default:
			body = []byte(*data)
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		if err != nil {
			return usageCodef("unreadable_file", "reading --data: %v", err)
		}
		// Nothing on stdin is a pipe that did not run, not a body: refused
		// as `api --data @-` and `env set --from-file -` refuse it (BB-021).
		if *data == "@-" && len(body) == 0 {
			return usageHinted("empty_input", "pipe the body in — `printf '{\"k\":1}' | toft fetch /path --method POST --data @-` — or leave --data off to send none",
				"--data @- read nothing from stdin")
		}
	}

	if len(form) > 0 {
		if *data != "" {
			return usageCodef("conflicting_flags", "--form and --data are two ways to send a body; use one")
		}
		ct, b, err := form.encode()
		if err != nil {
			return usageCodef("unreadable_file", "reading --form: %v", err)
		}
		body = b
		if headers.h == nil {
			headers.h = http.Header{}
		}
		headers.h.Set("Content-Type", ct)
		if !flagsSet(fs)["method"] {
			*method = http.MethodPost
		}
	}

	if *output == "-" && g.json {
		return usageCodef("conflicting_flags", "-o - puts the body on stdout, where --json would put its document — choose one")
	}

	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	freq := fetchRequest{Method: strings.ToUpper(*method), Path: path, Body: body,
		Headers: headers.h, Anonymous: *anonymous, MaxBody: *maxBody, Timeout: *timeout, NoFollow: *noFollow}
	var sink *outputFile
	if *output != "" {
		if sink, err = newOutputFile(*output, *force, e.out.Stdout); err != nil {
			return err
		}
		defer sink.discard()
		freq.Sink = sink.f
	}
	got, err := s.fetchApp(e.ctx, app, freq)
	if err != nil {
		return err
	}
	if sink != nil {
		// A refusal is not the file that was asked for: it is read back for
		// printing, bounded as any body is, and nothing is written.
		if !got.ok() {
			got.Body, got.Truncated = sink.head(*maxBody)
		} else if err := sink.keep(); err != nil {
			return usageCodef("unwritable_file", "--output: %v", err)
		} else if *output != "-" {
			got.Output = *output
		} else {
			got.Body, got.Truncated = "", false
		}
	}
	got.Platform = platformPage(got)
	if !g.json {
		for _, hop := range got.Redirects {
			e.out.Dim("%d %s  %s → %s", hop.Status, http.StatusText(hop.Status), hop.URL, hop.Location)
		}
	}

	if g.json {
		json.NewEncoder(e.out.Stdout).Encode(got)
	} else if got.Platform != "" && !(got.Platform == "run_page" && got.ok()) {
		// **The app did not answer; the platform did, in its place.** Five
		// kilobytes of the interstitial's HTML is the most expensive way to
		// say so, and the reader wants the state and the next step. A run
		// page that answered is what was asked for, and is printed as any
		// page is (BB-014, third list).
		e.out.Say("%s  %s", statusTone(e, got.Status), e.out.Dimmed(got.URL))
		// Said once: when a refusal follows, it says this (stopped, a run
		// page that is not there — BB-040, fourth 2026-11-03 list), and the
		// line is only for an answer that is not one.
		refusalFollows := (*expect != 0 && got.Status != *expect) || (*expect == 0 && !got.ok())
		if !refusalFollows && got.Platform == "batch" {
			// A 200: the address's own answer, the jobs it runs (BB-6).
			e.out.Answer("this app runs jobs and serves no pages; its address lists each job's schedule, next run and last outcome — `toft job list --app %s`", app.Slug)
		} else if !refusalFollows {
			e.out.Answer("the platform answered in the app's place: %s — `toft status --app %s` says why",
				strings.ReplaceAll(got.Platform, "_", " "), app.Slug)
		}
	} else {
		e.out.Say("%s  %s", statusTone(e, got.Status), e.out.Dimmed(orDefault(got.FinalURL, got.URL)))
		for _, h := range fetchedHeaders {
			if v, ok := got.Headers[h]; ok {
				e.out.Dim("%s: %s", h, v)
			}
		}
		if v, ok := got.Headers["Set-Cookie"]; ok {
			for _, c := range strings.Split(v, "\n") {
				e.out.Dim("Set-Cookie: %s", c)
			}
		}
		if got.Output != "" {
			e.out.Say("%s %d bytes written to %s", e.out.OK("✓"), got.Bytes, got.Output)
		}
		fmt.Fprint(e.out.Stdout, got.Body)
		if got.Body != "" && !strings.HasSuffix(got.Body, "\n") {
			fmt.Fprintln(e.out.Stdout)
		}
		if got.Truncated {
			e.out.Dim("… %d of %d bytes shown; --max-body 0 prints all of it.", *maxBody, got.Bytes)
		}
		if got.Status == http.StatusUnauthorized && *anonymous {
			e.out.Dim("The app is not public, which is what an anonymous request is told.")
		}
	}
	if *expect != 0 {
		if got.Status != *expect {
			if got.Platform != "" {
				return platformAnswered(got, app.Slug, fmt.Sprintf(", not %d", *expect))
			}
			return unexpectedStatus(got, app.Slug, fmt.Sprintf("the app answered %d, not %d", got.Status, *expect))
		}
		return nil
	}
	if got.Platform != "" && !got.ok() {
		return platformAnswered(got, app.Slug, "")
	}
	if !got.ok() {
		return unexpectedStatus(got, app.Slug, fmt.Sprintf("the app answered %d %s", got.Status, http.StatusText(got.Status)))
	}
	return nil
}

// unexpectedStatus is a fetch the app answered with a status that fails it —
// not the --expect, or without one, not a 2xx or 3xx. **A refusal, so it is
// coded** and, under --json, a final error object follows the result, as a
// platform page's does (BB-104, 2026-11-04 black-box list): before it, the
// sentence was on stderr alone. Exit 1 as before; retryable only for the
// statuses that say a later try may differ (502, 503, 504).
func unexpectedStatus(got *fetchResult, slug, msg string) error {
	hint := "the app's own output is in `toft logs --app " + slug + "`"
	if got.Status >= 400 && got.Status < 500 {
		hint = "the app refused the request — check the path, method and body; `toft logs --app " + slug + "` shows what it said"
	}
	retry := got.Status == http.StatusBadGateway || got.Status == http.StatusServiceUnavailable ||
		got.Status == http.StatusGatewayTimeout
	return &failedError{msg: msg, code: "unexpected_status", hint: hint, retryable: retry}
}

// platformAnswered is a fetch the gateway answered in the app's place — a
// batch app with no web service, a stopped one, one still building. **Not
// "the app answered 503"**: the app was never asked (BB-049). The code is the
// gateway's own word for the state (X-Toft-Interstitial), and it is retryable
// where the card itself promises a change (building, waking, restarting).
func platformAnswered(got *fetchResult, slug, suffix string) error {
	why := "`toft status --app " + slug + "` says why"
	switch got.Platform {
	case "stopped":
		why = "toft start --app " + slug
	case "batch":
		why = "it runs jobs and serves no pages — `toft job list --app " + slug + "`"
	case "sign_in_required":
		why = "the app is not public, so a request with no credential is refused before it reaches the app — drop --anonymous, or `toft visibility --app " + slug + "`"
		if got.DeployKey {
			why = deployKeyNeverOpens(slug)
		}
	case "run_page":
		why = "the run status pages are the platform's; `toft job runs --app " + slug + "` lists the runs that exist"
	}
	return &failedError{
		msg: fmt.Sprintf("the platform answered %d in the app's place (%s)%s — %s",
			got.Status, strings.ReplaceAll(got.Platform, "_", " "), suffix, why),
		code: got.Platform, hint: why, retryable: platformWillChange[got.Platform]}
}

// platformWillChange are the gateway's states that pass on their own — the
// cards that refresh themselves (internal/api/proxy_handler.go).
var platformWillChange = map[string]bool{"building": true, "restarting": true, "waking": true,
	"unavailable": true, "moving": true}

// orDuration is d, or fallback when d is zero.
func orDuration(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// resultFailedError is a command that got its answer, and the answer is a
// failure — the app answered 500, a check found a problem. Exit 1, the platform
// did the thing and it went wrong; under --json the result document already
// says so in full, so report() adds the sentence and no second document.
type resultFailedError struct{ msg string }

func (e *resultFailedError) Error() string { return e.msg }

// verifyDeploy is `toft deploy --verify`: the app answering a path, after the
// platform has said the deploy succeeded.
//
// **The readiness probe and this are different questions.** The platform
// probes until the process answers *something*; a page that throws on its first
// real request, or a route the developer forgot to register, passes that probe
// and fails this. It retries a 5xx and a refused connection inside the window,
// because a new release can still be warming behind the flip, and stops at the
// first answer that is not one of those — a 404 is an answer, and waiting will
// not change it.
func verifyDeploy(ctx context.Context, s *session, app *appSummary, path string, window time.Duration) (*fetchResult, error) {
	deadline := time.Now().Add(window)
	for {
		got, err := s.fetchApp(ctx, app, fetchRequest{Path: path, MaxBody: 2 << 10})
		// This client's own refusal of the path — an absolute URL, a dot
		// segment — is not something a retry changes: retried for the whole
		// window it ended in "roll back", over a mistyped flag, on a healthy
		// release (code review 2026-11-11, L-4).
		var usage *usageError
		if errors.As(err, &usage) {
			return nil, err
		}
		retry := err != nil || got.Status >= 500
		if !retry || time.Now().After(deadline) {
			return got, err
		}
		select {
		case <-ctx.Done():
			return got, err
		case <-time.After(2 * time.Second):
		}
	}
}

// verifyFailed is the error a failed --verify returns, whichever way it failed.
//
// **It names the release as serving and the command that undoes it.** The
// flip has already happened by the time the path is asked, so the app is now
// answering with what failed; a sentence that stopped at the status left the
// reader to work out that much, and what to type.
func verifyFailed(path, slug string, got *fetchResult, err error) error {
	// A deploy key's 401 says nothing about the release: the gateway never
	// takes that key, so there is nothing to roll back (BB-111).
	if err == nil && got != nil && got.DeployKey && got.Status == http.StatusUnauthorized {
		return &resultFailedError{fmt.Sprintf("deployed, but %s could not be asked: %s", path, deployKeyNeverOpens(slug))}
	}
	next := " — this release is serving; " + verifyNext(slug) + " puts the previous one back"
	if firstDeploys[slug] {
		next = " — this is the app's first deployment, so there is nothing to roll back to; " +
			verifyNext(slug) + " shows what it said"
	}
	if err != nil {
		return &resultFailedError{fmt.Sprintf("deployed, but %s did not answer: %v%s", path, err, next)}
	}
	return &resultFailedError{fmt.Sprintf("deployed, but %s answered %d %s%s", path, got.Status, http.StatusText(got.Status), next)}
}

// deployKeyNeverOpens is why a request under a deploy key was refused at the
// gateway: the key deploys apps and never opens one.
func deployKeyNeverOpens(slug string) string {
	return "a deploy key deploys an app but never opens it, so the gateway treats this request as anonymous — " +
		"check through the app with a personal key (toft login) of an account granted on it, make the app public, " +
		"or stop at the deploy; `toft status --app " + slug + "` says whether it is running"
}

// verifyNext is the command a failed --verify names, and the `next` field of
// the result object under --json.
func verifyNext(slug string) string {
	if firstDeploys[slug] {
		return "toft logs --app " + slug
	}
	return "toft rollback --yes --app " + slug
}

// firstDeploys are the apps whose watched deploy is their first — `toft
// create` marks the one it made — so there is no earlier release to roll back
// to. One command per process; create clears its mark when the watch ends.
var firstDeploys = map[string]bool{}

// verifyFlags registers --verify and --verify-timeout on a command that
// watches a deploy, into the env the watch reads them from.
func (e *env) verifyFlags(fs *flagSet) {
	fs.StringVar(&e.verify, "verify", "", "once it succeeds, request this path and fail unless it answers")
	fs.DurationVar(&e.verifyWithin, "verify-timeout", 30*time.Second, "how long a 5xx may be retried with --verify")
}

// refuseAbsoluteURL refuses a whole URL where a path belongs. Joined to the
// app's address it became `/apps/<slug>/https://example.com/` — a request this
// app's router answers with a 404 that reads as the app being broken, for what
// was in fact a fetch of somewhere else, which this command never makes.
func refuseAbsoluteURL(path string) error {
	p, _, _ := strings.Cut(path, "?")
	if strings.Contains(p, "://") || strings.HasPrefix(p, "//") {
		return usageCodef("invalid_path", "%q is a URL, and toft fetch requests a path of this app — "+
			"write the path from the app's root, such as /api/health; to reach another address use curl", path)
	}
	return nil
}

// refuseDotSegments refuses a `.` or `..` segment in the path, before
// anything is sent. Sent raw, `/../other-app/api/health` leaves this app's
// base path for whatever a proxy or the install makes of it — another app's,
// or the platform's — and a 200 from there reads as this app answering. The
// query string is not a path and is not checked. `toft store` refuses the
// same segments for the same reason.
func refuseDotSegments(path string) error {
	p, _, _ := strings.Cut(path, "?")
	p, _, _ = strings.Cut(p, "#")
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || strings.EqualFold(seg, "%2e%2e") || strings.EqualFold(seg, "%2e") {
			return usageCodef("invalid_path", "%q has a %q segment — a fetch stays inside the app; "+
				"write the path from the app's root", path, seg)
		}
	}
	return nil
}

// statusTone is "200 OK" coloured by its class — green, cyan for a redirect,
// yellow for the caller's mistake, red for the app's. Colour only: the words
// are the same with it off.
func statusTone(e *env, code int) string {
	text := fmt.Sprintf("%d %s", code, http.StatusText(code))
	switch {
	case code >= 500:
		return e.out.Fail(text)
	case code >= 400:
		return e.out.Caution(text)
	case code >= 300:
		return e.out.Info(text)
	case code >= 200:
		return e.out.OK(text)
	}
	return text
}
