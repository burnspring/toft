package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// The apps gateway's own pages — the 404 card, the status cards and the
// run-status page — are the platform's documents on the apps origin, and they
// carry headers written for them rather than the SPA's.
//
// Not the SPA's, because the SPA's would break them: `script-src 'self'`
// refuses the status card's inline refresh, and `frame-ancestors 'none'` with
// `X-Frame-Options: DENY` blanks the app page's preview iframe, which shows the
// waking card before it shows the app. So a status card is exactly as framable
// as the app it stands in for, and the 404 card — which stands in for no app —
// is framable by nobody. None of this is behind app_security_headers: the
// platform's own pages carrying the platform's headers is not a choice about
// anybody else's code. HSTS is already on the writer from the router.

// gatewayPageHeaders sets what every gateway page carries.
func gatewayPageHeaders(w http.ResponseWriter, csp string) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Content-Security-Policy", csp)
}

// The cards' policy, without its framing line: `default-src 'none'` and a
// hash for each inline block. The wordmark is inline SVG, so no `img-src`.
//
// **The hashes come from the render, never from the Go source.** html/template
// strips comments inside <script> and <style> when it renders, and the card's
// script opens with a comment, so a hash of the constant is the hash of a
// script the browser never receives: it would refuse it, the card would stop
// refreshing, and <noscript> would not appear because scripting is still on.
// The bodies hold no template actions, so one render at init stands for every
// render; TestTheCardHashesMatchWhatIsServed renders two different cards and
// holds them to it.
var (
	notFoundCardCSP     string // the 404 card
	listOnlyCardCSP     string // the list holder's 403 card (BB-95)
	fileNotFoundCardCSP string // a storage link's 404 card (store_link_handlers.go)
	statusCardCSP       string // a status card that does not refresh
	statusCardLiveCSP   string // one that does, and so runs its script
)

func init() {
	var nf bytes.Buffer
	if err := notFoundTmpl.Execute(&nf, struct{ PlatformLink string }{"https://toft.example"}); err != nil {
		panic("gateway 404 card does not render: " + err.Error())
	}
	notFoundCardCSP = cardPolicy(inlineBlocks(nf.String(), "style"), nil)

	var lo bytes.Buffer
	if err := listOnlyTmpl.Execute(&lo, struct{ Name, AppPage string }{"app", "https://toft.example/apps/x"}); err != nil {
		panic("gateway list-holder card does not render: " + err.Error())
	}
	listOnlyCardCSP = cardPolicy(inlineBlocks(lo.String(), "style"), nil)

	var fnf bytes.Buffer
	if err := fileNotFoundTmpl.Execute(&fnf, struct{ PlatformLink string }{"https://toft.example"}); err != nil {
		panic("storage link 404 card does not render: " + err.Error())
	}
	fileNotFoundCardCSP = cardPolicy(inlineBlocks(fnf.String(), "style"), nil)

	var card bytes.Buffer
	if err := interstitialTmpl.Execute(&card, interstitialData{
		AppName: "app", Status: "Waking", Message: "m", DotClass: "tone-attention", Refresh: true,
	}); err != nil {
		panic("status card does not render: " + err.Error())
	}
	styles, scripts := inlineBlocks(card.String(), "style"), inlineBlocks(card.String(), "script")
	statusCardCSP = cardPolicy(styles, nil)
	statusCardLiveCSP = cardPolicy(styles, scripts)
}

// cardPolicy is the directives before the framing line.
func cardPolicy(styles, scripts []string) string {
	parts := []string{"default-src 'none'", "style-src " + hashSources(styles)}
	if len(scripts) > 0 {
		parts = append(parts, "script-src "+hashSources(scripts))
	}
	parts = append(parts, "base-uri 'none'", "form-action 'none'")
	return strings.Join(parts, "; ")
}

func hashSources(bodies []string) string {
	srcs := make([]string, len(bodies))
	for i, b := range bodies {
		sum := sha256.Sum256([]byte(b))
		srcs[i] = "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	return strings.Join(srcs, " ")
}

// inlineBlocks is the text inside each bare <tag>…</tag> in a rendered page —
// exactly the bytes a browser hashes.
func inlineBlocks(page, tag string) []string {
	open, end := "<"+tag+">", "</"+tag+">"
	var out []string
	for {
		i := strings.Index(page, open)
		if i < 0 {
			return out
		}
		page = page[i+len(open):]
		j := strings.Index(page, end)
		if j < 0 {
			return out
		}
		out = append(out, page[:j])
		page = page[j+len(end):]
	}
}

// withFrame appends a card's framing line, and 'none' where there is no app
// whose answer to borrow.
func withFrame(policy, frame string) string {
	if frame == "" {
		frame = "frame-ancestors 'none'"
	}
	return policy + "; " + frame
}
