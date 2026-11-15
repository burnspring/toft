package api

import (
	"net/http"
	"strings"

	"toft/internal/auth"
	"toft/internal/version"
)

// The platform's own OpenAPI document, generated from the router.
//
// It exists because a scanner has to be told what to point at, and every other
// way of telling it is a list somebody maintains by hand — which works on the
// day it is written and silently stops covering each route added afterwards.
// `RouteInventory` already asks the router, and `scripts/zap-scan.sh` already
// builds its crawl from that; this is the same answer in the form a scanner
// somebody else runs can read, on an install nobody has a checkout of.
//
// **It is a target list, not a contract.** Paths, methods and path parameters
// are real — they come from chi. Request and response schemas do not exist in
// machine-readable form anywhere in this codebase, and writing them by hand
// would be a second specification that goes stale silently, which is the exact
// failure this file exists to remove. So the document carries none, says so in
// its own description, and promises nothing about stability. A client generated
// from it is a client generated from a scan target.

const openAPIVersion = "3.1.0"

// openAPIDescription is on the document itself rather than only in the docs,
// because the reader who most needs it is a person who was handed the URL and
// never saw this repository.
const openAPIDescription = `Every route the Toft platform listener serves, generated from the router itself at request time.

This document is a target list for security scanners and inventory tools. It names paths, methods and path parameters, and deliberately carries no request or response schemas: none exist in machine-readable form, and hand-written ones would drift from the router without anybody noticing.

It is not an API contract. Routes may change between releases, and nothing here is a promise of stability. The supported client is the ` + "`toft`" + ` CLI.

Extensions: x-toft-handler names the Go function serving the route, x-toft-source its file and line when the document is generated in a checkout, and x-toft-key-denied marks a route a request carrying only a personal API key is refused on — outside the key allow-list, a route that needs a browser session, or a route that takes an app's or a workload's own token. A route that answers anyone (the public configuration, sign-in, the provider list) is not marked: a key there is ignored and the answer is the anonymous one, which a keyed scan still reaches. GET /api/v1/auth/me answers the key as its owner.

x-toft-key-denied answers for a full-scope key, which is what a key is unless somebody chose otherwise. A read-scoped key additionally reaches no operation but GET: every other method on an allowed path answers 403. A scan run with one will therefore find the whole write surface refusing, and should not report it as tested.`

// OpenAPIDoc is the document. Exported because `toft openapi` marshals it
// outside this package; nothing constructs one but OpenAPIDocument.
type OpenAPIDoc struct {
	OpenAPI    string                          `json:"openapi"`
	Info       openAPIInfo                     `json:"info"`
	Servers    []openAPIServer                 `json:"servers,omitempty"`
	Security   []map[string][]string           `json:"security"`
	Components openAPIComponents               `json:"components"`
	Paths      map[string]map[string]openAPIOp `json:"paths"`
}

type openAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type openAPIServer struct {
	URL string `json:"url"`
}

type openAPIComponents struct {
	SecuritySchemes map[string]openAPIScheme `json:"securitySchemes"`
}

type openAPIScheme struct {
	Type        string `json:"type"`
	Scheme      string `json:"scheme,omitempty"`
	In          string `json:"in,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type openAPIOp struct {
	OperationID string                     `json:"operationId"`
	Summary     string                     `json:"summary,omitempty"`
	Parameters  []openAPIParam             `json:"parameters,omitempty"`
	Responses   map[string]openAPIResponse `json:"responses"`

	// The three extensions, and each is here because a scanner's output is
	// worth more with it than without.
	Handler string `json:"x-toft-handler,omitempty"`
	Source  string `json:"x-toft-source,omitempty"`
	// KeyDenied is set only when true. Absent means the personal-key guard
	// admits the route, not that the question was not asked.
	KeyDenied bool `json:"x-toft-key-denied,omitempty"`
}

type openAPIParam struct {
	Name     string            `json:"name"`
	In       string            `json:"in"`
	Required bool              `json:"required"`
	Schema   map[string]string `json:"schema"`
}

type openAPIResponse struct {
	Description string `json:"description"`
}

// ScannableRoutes is the platform listener's routes as a scan target: every
// route the walk yields, less the two kinds that are not addressable.
//
// Exported because three readers need the same list and a second definition of
// it would be a coverage gap nobody could see: the document below, the drift
// test beside it, and `scripts/zap/crawl.py` through the document.
//
// A wildcard is not a path — "/apps/{slug}/*" as a URL asks the proxy for a
// file called "*" — and the four methods dropped here are the ones a scanner
// generates for itself rather than reads from a list.
func ScannableRoutes(routes []Route) []Route {
	skipMethod := map[string]bool{"CONNECT": true, "TRACE": true, "OPTIONS": true, "HEAD": true}
	kept := make([]Route, 0, len(routes))
	for _, r := range routes {
		if r.Router != RouterPlatform || skipMethod[r.Method] || strings.HasSuffix(r.Pattern, "/*") {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// OpenAPIDocument builds the document. serverURL is the install's own address
// and may be empty, in which case the document carries no `servers` — a
// generated file has no way to know where it will be served from, and a wrong
// server URL is worse than none because a scanner would use it.
func OpenAPIDocument(serverURL string) (*OpenAPIDoc, error) {
	routes, err := RouteInventory()
	if err != nil {
		return nil, err
	}

	doc := &OpenAPIDoc{
		OpenAPI: openAPIVersion,
		Info: openAPIInfo{
			Title:       "Toft platform API",
			Version:     version.Version,
			Description: openAPIDescription,
		},
		// Both, and neither is per-operation: which credential authorizes a
		// given route is a question this document does not answer, and
		// pretending otherwise would put an authorization claim in a file whose
		// whole point is that it makes no claims.
		Security: []map[string][]string{{"bearerAuth": {}}, {"cookieAuth": {}}},
		Components: openAPIComponents{SecuritySchemes: map[string]openAPIScheme{
			"bearerAuth": {
				Type:        "http",
				Scheme:      "bearer",
				Description: "A personal API key (toftu_) or a deploy key (toftd_).",
			},
			"cookieAuth": {
				Type:        "apiKey",
				In:          "cookie",
				Name:        "toft_session",
				Description: "A browser session. Minted by an authentication and by nothing else.",
			},
		}},
		Paths: map[string]map[string]openAPIOp{},
	}
	if serverURL != "" {
		doc.Servers = []openAPIServer{{URL: strings.TrimSuffix(serverURL, "/")}}
	}

	for _, r := range ScannableRoutes(routes) {
		ops, ok := doc.Paths[r.Pattern]
		if !ok {
			ops = map[string]openAPIOp{}
			doc.Paths[r.Pattern] = ops
		}
		ops[strings.ToLower(r.Method)] = openAPIOp{
			OperationID: operationID(r.Method, r.Pattern),
			Summary:     r.Handler,
			Parameters:  pathParameters(r.Pattern),
			Responses: map[string]openAPIResponse{
				"default": {Description: "Not described. This document carries no response schemas."},
			},
			Handler:   r.Handler,
			Source:    r.Source,
			KeyDenied: keyDenied(r.Method, r.Pattern),
		}
	}
	return doc, nil
}

// keyDenied answers "is a request carrying only a personal key refused here".
//
// Inside the guarded /api/v1 group the allow-list is the whole answer:
// apiKeyRouteGuard refuses anything missing from it. Outside it, a route is
// answered to a key only when keyAnsweredOutsideGuard says so — /auth/me, which
// authenticates the key, and the public routes where a key is ignored and the
// caller is answered as anonymous. Every other route outside the group (a
// session-only sign-in step, an app's or a workload's token route, the git
// webhook's HMAC) refuses a key, which is what the mark says. Marking the
// public ones and /auth/me denied was a scan target list that skipped routes a
// key reaches (D-24, fourth 2026-11-03 black-box list).
func keyDenied(method, pattern string) bool {
	if !strings.HasPrefix(pattern, "/api/v1/") && pattern != "/api/v1" {
		return false
	}
	key := method + " " + pattern
	return !apiKeyAllowedRoutes[key] && !keyAnsweredOutsideGuard[key]
}

// pathParameters turns chi's {name} segments into OpenAPI's, which spell them
// the same way — so this is extraction, never translation. Every one is a
// string: the router does not type them, and a type here would be an assertion
// this document has no business making.
func pathParameters(pattern string) []openAPIParam {
	var params []openAPIParam
	for _, segment := range strings.Split(pattern, "/") {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		params = append(params, openAPIParam{
			Name:     segment[1 : len(segment)-1],
			In:       "path",
			Required: true,
			Schema:   map[string]string{"type": "string"},
		})
	}
	return params
}

// operationID is derived from the method and the path rather than from the
// handler name, because two routes can share a handler — GET /apps/{appID} and
// GET /apps/by-slug/{slug} are one function — and an operationId must be
// unique or the document is invalid.
func operationID(method, pattern string) string {
	id := strings.ToLower(method)
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" {
			continue
		}
		segment = strings.NewReplacer("{", "", "}", "", ".", "_", "-", "_").Replace(segment)
		id += "_" + segment
	}
	return id
}

// handleOpenAPI serves the document to an administrator, and 404s everyone
// else — the same gate and the same status as the estate readers, for a
// sharper version of the same reason. This enumerates every admin route on the
// install in one response: it is the attack surface written down, and an
// install that published it to a stranger would have given away the map for
// nothing. The routes are already visible to anyone holding a session, and that
// is a different audience.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	doc, err := OpenAPIDocument(s.cfg.BaseURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
