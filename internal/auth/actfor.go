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

// The act-for token: how a run started from inside an app is attributed to the
// person who pressed the button, without the app ever asserting who that is.
//
// At the moment an app calls start(), four things are simultaneously true: a
// browser request is in flight, the proxy has validated a session, the app
// process holds its outbound token, and the platform is receiving the call.
// That coincidence is what makes attribution free — and it is exactly what a
// decoupled queue would destroy, because a payload drained later can only carry
// *self-asserted* identity, which is the one kind that may never be accepted.
//
// So the proxy mints a blob over (user, app, token version, expiry), signs it
// with the platform's own key, and sets it as a request header. The app relays
// it verbatim; the platform verifies its own signature and resolves the user
// itself. The app cannot mint one, cannot alter one, and cannot read who the
// viewer is — which preserves the existing contract that X-Toft-Session
// "is a correlation id, not identity".
//
// Three properties are load-bearing, and each is a refusal somewhere below.
//
//   - **Bound to one app.** The app id is signed, so relaying a viewer's blob
//     from app A to app B buys nothing: B's verification names B.
//   - **Bound to one session.** The expiry is the *session's* expiry, not a
//     short independent clock, and the token version is checked live against
//     the database. The blob is a narrowed capability of that session and is
//     valid for exactly as long as the person could have pressed the button
//     themselves.
//   - **Stateless.** No table, which is what lets a node that never saw the
//     session issued verify a blob minted by another one.
//
// The property that falls out is the valuable one: the platform holds the real
// principal at the moment it decides. Authorization runs against the person's
// actual permissions, never against the app's claim about them, so an app
// cannot launder a viewer into authority they do not have.

// ActForHeader carries the blob from the proxy, through the app, back to
// /api/v1/app/*.
const ActForHeader = "X-Toft-Act-For"

// actForVersion prefixes every blob for a signed-in person. A version that is
// not this one is refused rather than parsed leniently: the fields are
// positional, and a future format misread as this one is the kind of mistake
// that ends with the wrong user id.
const actForVersion = "a1"

// actForAnonVersion prefixes the blob minted for a visitor with no session:
// *somebody caused this and they are nobody in particular*.
//
// A version of its own rather than an empty user field in a1, and the reason is
// the sentence above about a1: an empty user id in a positional user slot is
// precisely the mistake that comment forbids, arriving by invitation. The two
// formats have different field counts, so a blob of one shape cannot be read as
// the other even before the version is checked.
//
// Why it exists at all: today the *absence* of a blob means the app is acting as
// itself, which is the most authority the app has — so an anonymous visitor
// pressing a button got the app's own authority rather than none, not because
// anything was broken but because the system had no way to say "a person caused
// this and they are nobody". This is that way.
//
// **It is a narrowing and never a grant.** It carries the app id and an expiry
// and no subject; verification resolves it to no principal; authorization then
// runs against anonymous, which on a public app is ActionRead and nothing else.
// It cannot be relayed to another app, it authorizes strictly less than the
// app's own token, and there is no route it opens.
const actForAnonVersion = "a2"

// ActFor is a verified act-for token's contents.
type ActFor struct {
	UserID string
	AppID  string
	// TokenVersion is users.token_version at mint time, compared against the
	// live value so a logout, a demotion or a lock kills the blob with the
	// session it came from.
	//
	// Zero and meaningless on an anonymous blob, which narrows no session.
	TokenVersion int
	ExpiresAt    time.Time
	// Anonymous marks a blob minted for a visitor with no session. UserID is
	// empty, and a caller that wants a person must refuse rather than read the
	// empty string as one — which is why [JWTService.VerifyActFor]'s callers go
	// through one helper that fails closed.
	Anonymous bool
}

var (
	// ErrActForInvalid covers every way a blob fails to be one this platform
	// minted: wrong shape, wrong version, bad signature. Flat on purpose — a
	// caller presenting a forgery learns only that it was refused.
	ErrActForInvalid = errors.New("this act-for token was not issued by Toft")
	// ErrActForExpired is separated because it is the one failure an app can do
	// something about: the person's session ended, and the answer is that they
	// sign in again rather than that anything is broken.
	ErrActForExpired = errors.New("this act-for token has expired — its session has ended")
	// ErrActForWrongApp is a blob minted for a different app. Named because the
	// mistake it usually indicates is an app relaying a header it received from
	// somewhere other than its own gateway request.
	ErrActForWrongApp = errors.New("this act-for token was issued for a different app")
)

// MintActFor signs a blob for one viewer of one app.
//
// expiry is the session's own expiry. Passing anything else — a fixed five
// minutes, say — breaks the archetypal app: Streamlit talks over a WebSocket,
// so headers arrive at the handshake and the app holds them for the life of the
// browser session, which is hours. A short TTL would make the button work for
// five minutes and then fail in a way nobody could reproduce.
func (j *JWTService) MintActFor(userID, appID string, tokenVersion int, expiry time.Time) string {
	payload := actForPayload(userID, appID, tokenVersion, expiry)
	return payload + "." + j.actForSignature(payload)
}

// MintAnonymousActFor signs a blob for a visitor of one app who has no session.
//
// expiry is the install's own absolute session cap rather than a number chosen
// here. There is no session for this blob to expire with, so it needs a lifetime
// from somewhere, and an arbitrary constant in a credential is what this product
// argues against elsewhere: the absolute cap is already the install's answer to
// *how long may one visit's capability live*, and reusing it means one clock
// rather than two. The lifetime is hygiene rather than a control in any case —
// the blob authorizes anonymous, so a captured one buys an attacker exactly what
// they could already do by opening the page.
func (j *JWTService) MintAnonymousActFor(appID string, expiry time.Time) string {
	payload := anonActForPayload(appID, expiry)
	return payload + "." + j.actForSignature(payload)
}

// VerifyActFor checks a blob's signature, expiry, and app binding.
//
// It does *not* check the token version against the database — that needs a
// store, and this package's callers already hold one. VerifyActFor is the
// cryptographic half; the liveness half is CheckActForSession below, and both
// must run. Splitting them is deliberate: a signature check that quietly did a
// database read would be tempting to skip on a hot path.
func (j *JWTService) VerifyActFor(blob, appID string) (*ActFor, error) {
	idx := strings.LastIndexByte(blob, '.')
	if idx < 0 {
		return nil, ErrActForInvalid
	}
	payload, sig := blob[:idx], blob[idx+1:]

	// Constant time, so a forger cannot walk the signature a byte at a time.
	if !hmac.Equal([]byte(sig), []byte(j.actForSignature(payload))) {
		return nil, ErrActForInvalid
	}

	// Only decoded *after* the signature holds. Parsing attacker-controlled
	// structure before authenticating it is how a verifier grows a second,
	// weaker entry point.
	fields := strings.Split(payload, ".")
	out := &ActFor{}
	switch {
	case len(fields) == 5 && fields[0] == actForVersion:
		userID, err := base64.RawURLEncoding.DecodeString(fields[1])
		if err != nil {
			return nil, ErrActForInvalid
		}
		signedApp, err := base64.RawURLEncoding.DecodeString(fields[2])
		if err != nil {
			return nil, ErrActForInvalid
		}
		version, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, ErrActForInvalid
		}
		unix, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return nil, ErrActForInvalid
		}
		// An a1 blob naming nobody is refused rather than quietly resolved as
		// anonymous. The two shapes mean different things and a1's user slot is
		// never empty when this platform minted it, so an empty one is either a
		// forgery attempt or a bug — and reading it as *nobody in particular*
		// would be reading a subject field as an assertion about no subject.
		if len(userID) == 0 {
			return nil, ErrActForInvalid
		}
		out.UserID = string(userID)
		out.AppID = string(signedApp)
		out.TokenVersion = version
		out.ExpiresAt = time.Unix(unix, 0).UTC()

	case len(fields) == 3 && fields[0] == actForAnonVersion:
		signedApp, err := base64.RawURLEncoding.DecodeString(fields[1])
		if err != nil {
			return nil, ErrActForInvalid
		}
		unix, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, ErrActForInvalid
		}
		out.AppID = string(signedApp)
		out.ExpiresAt = time.Unix(unix, 0).UTC()
		out.Anonymous = true

	default:
		return nil, ErrActForInvalid
	}
	// App binding before expiry: a blob relayed to the wrong app is the answer
	// whether or not it has also expired, and it is the more useful one.
	if appID != "" && out.AppID != appID {
		return nil, ErrActForWrongApp
	}
	if !out.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrActForExpired
	}
	return out, nil
}

// CheckActForSession is the liveness half: the session this blob was narrowed
// from must still be one the person could act with.
//
// Same rule as auth.Middleware and the same rule the gateway applies — a stale
// token_version means the session was revoked by a logout, a demotion or a
// group change, and a locked account's sessions are dead everywhere. Without
// this the blob would outlive the session it claims to be a capability of,
// which is the whole reason it is allowed to live as long as it does.
func CheckActForSession(states SessionStates, a *ActFor) bool {
	if states == nil || a == nil {
		return false
	}
	// An anonymous blob narrows no session, so there is nothing here that could
	// be stale: its signature and its expiry are its whole lifetime. Answering
	// true is not a weakening — it authorizes as nobody, and every caller that
	// needs a person refuses it by name before it gets this far.
	if a.Anonymous {
		return true
	}
	current, locked, err := states.GetSessionState(a.UserID)
	return err == nil && !locked && current == a.TokenVersion
}

func actForPayload(userID, appID string, tokenVersion int, expiry time.Time) string {
	// base64 on the two identifiers rather than raw text: the payload is
	// dot-delimited and positional, and an id containing a dot would otherwise
	// let a caller shift every field along by one.
	return fmt.Sprintf("%s.%s.%s.%d.%d",
		actForVersion,
		base64.RawURLEncoding.EncodeToString([]byte(userID)),
		base64.RawURLEncoding.EncodeToString([]byte(appID)),
		tokenVersion,
		expiry.UTC().Unix())
}

// anonActForPayload is three fields to a1's five, which is what makes the two
// unmistakable for each other even before the version prefix is read.
func anonActForPayload(appID string, expiry time.Time) string {
	return fmt.Sprintf("%s.%s.%d",
		actForAnonVersion,
		base64.RawURLEncoding.EncodeToString([]byte(appID)),
		expiry.UTC().Unix())
}

// actForSignature domain-separates from every other use of this secret. The
// same key signs session JWTs and run tokens; without the prefix, a string that
// was valid in one of those contexts could be replayed into this one.
func (j *JWTService) actForSignature(payload string) string {
	mac := hmac.New(sha256.New, j.secret)
	mac.Write([]byte("toft-act-for\x00"))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
