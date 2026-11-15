package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The app grant: how a session that exists on the platform's origin becomes a
// session on the apps origin, without either cookie ever spanning both.
//
// WHY THIS EXISTS AT ALL.
//
// Toft's app→platform boundary was built as one host on two ports, because
// a browser ignores the port when scoping a cookie and includes it in a JS
// origin: the session follows the viewer to the apps listener for free, while
// app JavaScript still cannot read `/api/v1` as them. That works on a machine
// with a reverse proxy in front and nowhere else — a load balancer routes by
// host and path, not by listener port, so every install behind an ALB, an
// nginx-ingress or an F5 silently collapses to one origin and loses the
// boundary.
//
// Two hostnames restore it, and cost the free part: a cookie set on
// `toft.example.com` is host-only and never reaches `apps.example.com`, so
// the gateway sees an anonymous viewer, bounces to the platform, is told the
// person is signed in, comes back, and finds nothing again. A login page that
// can never succeed.
//
// The industry answer is not to widen the cookie with a Domain attribute —
// that hands the platform session to every host in the zone, forever, including
// ones added later and dangling records somebody takes over. It is a per-origin
// session established by a redirect handshake, which is what Cloudflare Access,
// Google IAP and oauth2-proxy all do. This is that handshake's credential.
//
// THREE PROPERTIES, each a refusal below.
//
//   - **Bound to one origin.** The apps origin is signed, so a grant minted for
//     one install's apps host buys nothing at another's, and nothing at the
//     platform's own origin either.
//   - **Bound to one session.** The token version is signed and re-checked live
//     by the redeemer, so a grant cannot outlive the sign-out, demotion or lock
//     that revoked the session it was cut from.
//   - **Bound to one browser.** The gateway that starts the handshake leaves a
//     random nonce in a host-only cookie on the redeeming origin and passes
//     it along; the nonce is signed in, and the redeemer compares it with the
//     cookie. Without it a signed-in person could mint a grant for themselves
//     and navigate somebody else's browser to it, swapping that browser's app
//     session for their own.
//   - **Short.** Seconds, not the session's lifetime — because unlike the
//     act-for blob, which travels in a header, this one travels in a URL, and a
//     URL lands in proxy logs, SWG logs, browser history and Referer.
//
// WHAT IT DELIBERATELY IS NOT: single-use. Enforcing that needs shared state,
// and state is what lets a replica that never saw the mint verify it — the same
// reasoning that keeps the act-for blob stateless. The compensating controls are
// the short window, the origin binding, and the redeemer answering with
// `Referrer-Policy: no-referrer` and an immediate redirect that replaces the
// grant-bearing URL. An operator who wants single-use gets it by shortening
// AppGrantTTL, not by adding a table to a request path.

const (
	// AppGrantTTL is how long a grant is worth anything. Long enough for a
	// browser to follow two redirects on a bad connection; short enough that a
	// grant recovered from a log is almost certainly already dead.
	AppGrantTTL = 30 * time.Second

	// g2 added the session start, g3 the browser nonce. An older grant is
	// refused as malformed, which costs nothing: a grant lives thirty seconds.
	appGrantVersion = "g3"
)

var (
	// ErrAppGrantInvalid covers every malformed, mis-signed or mis-bound grant.
	// One error for all of them on purpose: telling a caller *which* field failed
	// is telling them how to forge the next one.
	ErrAppGrantInvalid = errors.New("invalid app grant")
	// ErrAppGrantExpired is separated because it is the one failure a legitimate
	// browser can hit — a slow redirect, a machine asleep between hops — and it
	// deserves a retry rather than a refusal.
	ErrAppGrantExpired = errors.New("app grant expired")
)

// AppGrant is what a verified grant resolves to. It carries no role, no groups
// and no claims: the redeemer looks the person up and mints their session from
// the database, so a demotion between mint and redeem is honoured.
//
// SessionStart is the one thing about time it does carry: when the platform
// session it was cut from began. The two origins do not share a cookie — that
// is why the grant exists — so this is the only way the apps-origin session
// can inherit the sign-in's `auth_time` rather than restart the absolute
// ceiling from the moment of the handshake.
type AppGrant struct {
	UserID       string
	Origin       string
	TokenVersion int
	SessionStart time.Time
	// Nonce is the value the starting browser was given in a cookie; the
	// redeemer compares the two.
	Nonce string
}

// MintAppGrant signs a grant for one user at one apps origin.
//
// origin is the scheme and host the browser will present it at — "https://apps.example.com" —
// and it is signed rather than merely checked, so the grant is worthless anywhere else.
// sessionStart is the platform session's own; the redeemer mints from it.
// nonce is the starting browser's (ValidAppGrantNonce); a grant with none is
// refused at verification.
func (s *JWTService) MintAppGrant(userID, origin string, tokenVersion int, sessionStart time.Time, nonce string, now time.Time) string {
	exp := now.Add(AppGrantTTL).Unix()
	payload := strings.Join([]string{
		appGrantVersion,
		userID,
		normalizeGrantOrigin(origin),
		strconv.Itoa(tokenVersion),
		strconv.FormatInt(exp, 10),
		strconv.FormatInt(sessionStart.UTC().Unix(), 10),
		nonce,
	}, "|")
	return payload + "|" + s.signAppGrant(payload)
}

// VerifyAppGrant checks a grant against the origin it is being presented at.
//
// The caller must still check the token version against the database — this
// function proves what was signed, not that it is still true. That split is the
// same one act-for makes, and for the same reason: the signature is stateless
// and revocation is not.
func (s *JWTService) VerifyAppGrant(blob, origin string, now time.Time) (*AppGrant, error) {
	parts := strings.Split(blob, "|")
	if len(parts) != 8 || parts[0] != appGrantVersion {
		return nil, ErrAppGrantInvalid
	}
	payload := strings.Join(parts[:7], "|")
	if !hmac.Equal([]byte(parts[7]), []byte(s.signAppGrant(payload))) {
		return nil, ErrAppGrantInvalid
	}

	// Compared after the signature, so a mismatch cannot be used as an oracle
	// for guessing at signatures.
	if parts[2] != normalizeGrantOrigin(origin) {
		return nil, ErrAppGrantInvalid
	}

	tokenVersion, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil, ErrAppGrantInvalid
	}
	exp, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return nil, ErrAppGrantInvalid
	}
	if now.Unix() > exp {
		return nil, ErrAppGrantExpired
	}
	if parts[1] == "" {
		return nil, ErrAppGrantInvalid
	}
	start, err := strconv.ParseInt(parts[5], 10, 64)
	if err != nil || start <= 0 {
		return nil, ErrAppGrantInvalid
	}

	if !ValidAppGrantNonce(parts[6]) {
		return nil, ErrAppGrantInvalid
	}

	return &AppGrant{UserID: parts[1], Origin: parts[2], TokenVersion: tokenVersion,
		SessionStart: time.Unix(start, 0).UTC(), Nonce: parts[6]}, nil
}

// ValidAppGrantNonce is the shape a handshake nonce must have: 16 to 64
// characters of base64url, so it can never carry the grant's field separator.
func ValidAppGrantNonce(n string) bool {
	if len(n) < 16 || len(n) > 64 {
		return false
	}
	for _, c := range n {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *JWTService) signAppGrant(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	// Domain-separated from every other thing this key signs. Without the
	// prefix, a blob minted for one purpose could be presented as another
	// wherever the field layouts happen to line up.
	fmt.Fprintf(mac, "toft.app-grant.v1|%s", payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// normalizeGrantOrigin reduces an origin to the form both sides will agree on.
// A trailing slash and letter case are the two ways the same origin gets written
// differently by configuration and by a browser.
func normalizeGrantOrigin(origin string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(origin), "/"))
}
