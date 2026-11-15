package notify

import (
	"net/url"
	"strings"
)

// AppPageURL is an app's page *in Toft* — the platform's own origin, the
// SPA route, the thing a person opens to decide something about an app.
//
// It is the twin of builder.PublicURL and the two are not interchangeable:
// that one is where a *visitor* reaches the running app, on the apps origin,
// and it is keyed by slug because that is what the proxy resolves. This one is
// where an *owner* reaches the app's record, on the platform origin, and it is
// keyed by **id** because that is what the SPA resolves — `/apps/:id` fetches
// `GET /api/v1/apps/{id}`, which looks the app up by id and 404s on anything
// else.
//
// The distinction is not cosmetic and it is why this function exists. Every
// link in the platform's outbound mail pointed at `base + "/apps/" + slug`,
// which lands on neither page: on a split-origin install the platform origin
// recognises a real slug and redirects it to the apps origin, and on a
// single-origin one the proxy claims it before the SPA is ever served. So an
// owner asked to decide an access request followed a button to their own app's
// front door, or to a 404, and never to the queue. The id has no such
// collision — no app's id is another app's slug, so `/apps/{id}` falls through
// to the SPA on both shapes.
//
// It lives here rather than beside PublicURL because internal/builder imports
// internal/events, so events cannot import builder; notify is the package both
// callers already have. The interstitial's way back uses it too — not a
// notification, but the same address and the same trap.
//
// Empty when either half is missing, which every caller reads as "there is no
// address to give" rather than as a URL with a hole in it.
func AppPageURL(baseURL, appID string) string {
	base := strings.TrimRight(baseURL, "/")
	if base == "" || appID == "" {
		return ""
	}
	return base + "/apps/" + url.PathEscape(appID)
}
