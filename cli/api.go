package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"toft/cli/internal/client"
)

// `toft api` — the authenticated escape hatch.
//
// **The gap this closes is not "some verb is missing"; it is that the way
// round a missing verb was to scrape a credential out of a file.** Several
// operations live only in the HTTP API — app visibility details, environment
// variables, app visibility, release pruning — and reaching them needs the
// token. Nothing prints it: `toft login --status` says who and where, never the
// key. So the way an agent actually got there was
//
//	TOK=$(grep -m1 -oE 'toft[ud]?_[A-Za-z0-9_.-]+' "$APPDATA/toft/credentials.toml")
//
// a regex over a private file whose path and format are an implementation
// detail, differ per platform, and will break silently the day the storage
// format changes. That is worse than any of the missing verbs, because it is
// the mechanism by which every other gap compounds.
//
// **A passthrough rather than `toft token --print`**, and the difference is the
// whole point: the credential stays inside the client. Printing it puts a
// long-lived secret into a shell variable, a process table, and whatever
// captured the output, to make one call. This makes the call.
//
// It widens nothing. The key already reaches every route it reaches; the only
// thing that changes is that a caller no longer has to hold the key to use it.
// Which is also why there is no allow-list of paths here — a list would be a
// second, staler copy of the authorization the install already applies on every
// request, and the refusals stay the install's own.
const apiUsage = `toft api — call this install's HTTP API with the stored credential

Usage:
  toft api <method> <path>               GET, POST, PATCH, PUT or DELETE
  toft api GET /api/v1/apps
  toft api POST /api/v1/apps/<id>/job-runs --data '{"job":"digest", ...}'
  toft api PATCH /api/v1/apps/<id> --data @patch.json
  cat body.json | toft api POST /api/v1/apps/<id>/job-runs --data @-

Flags:
  --data <json>       Request body. @file reads a file, @- reads stdin.
  --install <url>     Which install. Otherwise the default from toft login.
  --json              No effect: the answer is already the platform's own JSON.

The response body goes to stdout exactly as the install sent it — a refusal's
too, when it is JSON, with no second document beside it. The exit code is
the usual contract, and it is coarser than the status: 2 for a path that matches
no route, a 404, a 400 or a 409 name_taken; 3 for a credential or permission
refusal or any other 409 (the app's state refused); 4 when the install could
not be reached; 1 for the rest — a 5xx, a 422, a 409 that is a wait. The jobs
codes outrank their status: jobs_unavailable and run_still_open are 3, queue_full
is 1, and a parameter or job the install refuses (param_*, no_such_job) is 2.
Read the body for the detail: it is the platform's own JSON, and its code says which.

A path is taken as written. One without a leading slash is read as relative to
/api/v1/, so ` + "`toft api GET apps`" + ` and ` + "`toft api GET /api/v1/apps`" + ` are the same call.
A path matching no route on the install is exit 2, naming the path. Paths
under apps/ take the app's id, not its slug: ` + "`toft status --json`" + ` .app.id, or
` + "`toft api GET apps/by-slug/<slug>`" + `.

On Windows, prefer the relative form: Git Bash and other MSYS shells rewrite an
argument beginning with a slash into a Windows path before ` + "`toft`" + ` is started.
` + "`toft api GET apps`" + ` is never rewritten; MSYS_NO_PATHCONV=1 stops the rewriting.

Which routes exist: ` + "`toft api GET admin/openapi.json`" + ` (an administrator's key)
lists every path and method on this install, and marks with x-toft-key-denied
the ones a key cannot reach — those need ` + "`--session`" + `. It names no request
bodies; a route given a wrong one says which field it wanted.

This is the way round a verb ` + "`toft`" + ` does not have. Where there is a verb, use
it: it validates arguments, reports failures in sentences, and will keep working
when the route behind it changes.
`

// apiMethods is the closed set. Not because the API refuses the others, but
// because a typo'd method silently becoming a GET is a request that reads
// something when the caller meant to write, and the reverse is worse.
var apiMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPatch: true,
	http.MethodPut: true, http.MethodDelete: true,
}

func cmdAPI(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("api", apiUsage)
	g.register(fs)
	data := fs.String("data", "", "request body; @file or @- for stdin")

	// Flags go before or after the two operands, as on every command — and a
	// global flag written before `api` arrives here ahead of them (M3).
	if err := parseOperands(fs, args, 2); err != nil {
		return skipHelp(err)
	}
	method, path := fs.Arg(0), fs.Arg(1)
	if method == "" || path == "" {
		return usageCodef("missing_argument", "toft api needs a method and a path — `toft api GET /api/v1/apps`")
	}

	method = strings.ToUpper(method)
	if !apiMethods[method] {
		return usageCodef("invalid_method", "toft api does not send %s — GET, POST, PATCH, PUT or DELETE", method)
	}
	// Named before the prefix rule, because the prefix rule is what hides it:
	// a rewritten path has lost its leading slash, so it gets "/api/v1/" put in
	// front of a drive letter and travels to the install as a route nobody
	// registered. See paths.go — the install cannot diagnose this, because by
	// the time it arrives the evidence is gone.
	if rewrittenByTheShell(path) {
		return shellRewroteIt("the path", path, "toft api "+method+" apps")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/api/v1/" + path
	}

	body, err := apiBody(e.ctx, *data)
	if err != nil {
		return err
	}
	if body != nil && method == http.MethodGet {
		return usageCodef("conflicting_flags", "a GET carries no body — drop --data, or name the method that writes")
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	// json.RawMessage both ways: out as the caller typed it, back as the
	// install sent it. Neither half is re-encoded, so a field this binary has
	// never heard of survives the round trip — which is most of the reason to
	// have this command at all.
	var out json.RawMessage
	var send any
	if body != nil {
		send = json.RawMessage(body)
	}
	if err := s.client.Do(method, path, send, &out); err != nil {
		// **A path that is not the API is the caller's mistake, not a version
		// skew.** Everywhere else in this client the path is the client's own
		// choice, so an answer it cannot decode really does mean the install is
		// a version it does not understand — and DecodeError says exactly that,
		// and says to upgrade `toft`. Here the path came from the caller, and
		// `/apps` or `/login` reaches the SPA and answers HTML with a 200. The
		// generic sentence sends somebody to upgrade a binary that is fine.
		var decode *client.DecodeError
		if errors.As(err, &decode) {
			return usageCodef("not_json", "%s %s answered with something that is not JSON, so it is "+
				"not an API route — the SPA and the apps gateway both answer HTML on "+
				"paths of their own.\n"+
				"    API paths live under /api/, and a path without a leading slash is "+
				"read as relative to /api/v1/: `toft api %s apps`", method, path, method)
		}
		// **A refusal's body is the answer too.** The install's JSON goes to
		// stdout as it would on a 2xx — the code, the hint, any field this
		// binary does not name — and the exit code still says it was refused.
		// It is then the document, so the client's own envelope is not written
		// beside it: one answer, one document.
		var apiErr *client.Error
		if errors.As(err, &apiErr) && json.Valid(apiErr.Body) {
			fmt.Fprintln(e.out.Stdout, strings.TrimSpace(string(apiErr.Body)))
			e.json = false
		}
		// A slug where the route takes an id is the common 404 here, and the
		// path is taken as written — so the way to the id is the hint under
		// the refusal, rather than the slug being quietly swapped for one. It
		// replaces whatever hint the install sent: the install's is for an id
		// that names nothing, and this path has already shown it holds a slug.
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			if slug, ok := slugInAppsPath(path); ok {
				apiErr.Hint = "`toft api GET apps/by-slug/" + slug + "` answers with the app's id (.app.id) — " +
					"apps/ paths take the id, not the slug"
			}
		}
		return err
	}
	if len(out) == 0 {
		return nil
	}
	fmt.Fprintln(e.out.Stdout, strings.TrimSpace(string(out)))
	return nil
}

// slugInAppsPath reports the segment after /api/v1/apps/ when it is not an
// app id — a UUID — and not one of the collection's own words.
func slugInAppsPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/v1/apps/")
	if !ok {
		return "", false
	}
	seg, _, _ := strings.Cut(rest, "/")
	seg, _, _ = strings.Cut(seg, "?")
	if seg == "" || seg == "by-slug" || appIDPattern.MatchString(seg) {
		return "", false
	}
	return seg, true
}

var appIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// apiBody resolves --data: a literal, @file, or @- for stdin.
//
// The JSON is validated here rather than sent blind. A malformed body would
// otherwise come back as the install's own 400, which is a correct answer to
// the wrong question — the caller wants to know their file is broken, not that
// the platform disliked it.
func apiBody(ctx context.Context, data string) ([]byte, error) {
	if data == "" {
		return nil, nil
	}
	raw := []byte(data)
	switch {
	case data == "@-":
		b, err := readAllCtx(ctx, os.Stdin)
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("reading the body from stdin: %w", err)
		}
		raw = b
	case strings.HasPrefix(data, "@"):
		b, err := os.ReadFile(data[1:])
		if err != nil {
			return nil, usageCodef("unreadable_file", "could not read the body from %s: %v", data[1:], err)
		}
		raw = b
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, usageCodef("empty_input", "--data was given and is empty")
	}
	if !json.Valid(raw) {
		return nil, usageHinted("invalid_json", `quote the body for the shell — --data '{"name": "x"}' — or put it in a file and pass --data @body.json`,
			"--data is not valid JSON — the install would refuse it "+
				"and the refusal would be about the wrong thing")
	}
	return raw, nil
}
