package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/version"
	"toft/sdk"
)

// The app SDK, served to the client that vendors it.
//
// The SDK ships inside this binary and is installed by the build, which is what
// keeps an app's copy in step with the API it is talking to (ADR §6). That
// leaves one thing unanswered, and Go made it sharp: **an app's own tooling
// cannot see the SDK at all** until the platform has built it. Python and Node
// fail at import, so an app still edits and lints locally; Go fails at compile,
// so `go build`, `go vet`, `gopls` and the app's own CI all stop on the import
// line.
//
// The answer is deliberately not a registry. A published package is a second
// version of the SDK, and a developer would then be building against whichever
// of the two their package manager resolved rather than against the install
// they deploy to — which is precisely the skew `X-Toft-App-Contract`
// exists to make loud. So the *install* serves its own copy, `toft sdk vendor`
// writes it out, and the bytes a developer compiles against are by construction
// the bytes the build would have used.
//
// **It is authenticated, and that is a decision rather than a default.** The
// payload is not a secret — it is in this binary, in the runner image, and in
// every Python and Node release on the install. What an anonymous route would
// add is a version string on the unauthenticated surface, for a consumer that
// already holds a credential and already has one resolution order for it. There
// is exactly one unauthenticated route in this product and it is the config
// probe the chart depends on; a second one needs a better reason than saving
// the client a header it is already sending.

// handleGetSDK answers with one language's SDK as text.
func (s *Server) handleGetSDK(w http.ResponseWriter, r *http.Request) {
	// Every handler authorizes for itself: auth.Middleware lets an anonymous
	// caller through with a nil principal, so an absent check here is an open
	// route rather than a closed one.
	if auth.GetPrincipal(r.Context()) == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	lang := strings.ToLower(chi.URLParam(r, "lang"))
	payload, err := sdk.Files(lang)
	if err != nil {
		// Naming what this binary does carry, rather than only what it does
		// not: the caller is a client that guessed, and the set is short.
		carried := append([]string(nil), sdk.Languages()...)
		sort.Strings(carried)
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no SDK for " + lang + ". This install carries: " + strings.Join(carried, ", "),
			"code":  "sdk_language_unknown", "hint": "ask for one of the languages the sentence lists, like toft sdk vendor --lang python",
		})
		return
	}

	writeJSON(w, http.StatusOK, sdkResponse{
		Payload: payload,
		// The install's own release, not the SDK's declared version. It is what
		// a developer needs to answer "is my vendored copy still the one this
		// install would have built me", and it is the number every other
		// surface here already reports.
		Version: version.Version,
		// And the SDK's own, which is the number its package declares and
		// the one a developer compares with what they import.
		SDKVersion: sdk.Version(lang),
	})
}

// sdkResponse is the payload plus the install's version, flattened so a client
// reads one object rather than reaching through a wrapper.
type sdkResponse struct {
	sdk.Payload
	Version    string `json:"version"`
	SDKVersion string `json:"sdk_version,omitempty"`
}
