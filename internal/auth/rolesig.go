package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The role header: how an app is told what the caller in front of it may do,
// without ever being told who they are.
//
// Every request the platform forwards to an app carries one, always, and the
// app decides what to do about it. The platform enforces nothing new — this is
// vocabulary, not a gate. What it replaces is an app that must protect an
// expensive endpoint either exposing it to everyone who can open the page, or
// calling /api/v1/app/viewer, which an administrator has to switch on per app
// and which resolves a whole *person* to answer a question about a role.
//
// **A role is not an identity, and that distinction is the whole argument.**
// `owner`, `editor`, `viewer` is what the caller may do to this deployment. It
// names nobody, it is the same word for a thousand different people, and it
// does not survive the request. Everything that identifies a person — a name,
// an email, a user id, a group list — stays behind apps.viewer_identity,
// unchanged, and nothing here adds a field to it.
//
// **The platform still never believes a header.** Wherever the *platform*
// decides, it re-resolves live: act-for verification checks token_version
// against the database, job starts authorize against the resolved principal,
// and /api/v1/app/viewer reads the role at the moment it answers. No header
// minted here is ever read by an authorization path. What changed is only that
// the app may be *told*.
//
// # Why it is signed rather than a word
//
// Stripping an inbound header stops a visitor forging one. It does nothing
// about the case that matters: deployed apps bind 127.0.0.1 in shared mode and
// app-to-app isolation there is an accepted risk, so any other app on the box
// can open a socket to an app's port and send whatever headers it likes without
// the gateway ever seeing the request. An unsigned role header would be
// trustworthy only on the assumption that the gateway is the sole path to the
// app, and that assumption is false in the mode most installs run.
//
// So it is a signed assertion the app verifies itself, and three properties are
// load-bearing:
//
//   - **Bound to one app.** The app id is inside the signature, so a blob
//     minted for app A and replayed at app B fails at B.
//   - **Short-lived.** [RoleTTL] is seconds. The only parties that see a
//     forwarded header are the app and whatever sits on the network path to it,
//     so the window this closes is capture-and-replay by something already
//     inside the boundary — bounded rather than eliminated, and bounded by a
//     number an operator does not get to raise.
//   - **Verified with no round trip.** The workload holds a per-app, per-start
//     key ([JWTService.RoleKey]) and checks the MAC locally. A verification
//     that needed a call to the platform would be a request per request, which
//     is not a feature anybody would turn on.
//
// **The symmetry is stated rather than hidden.** An HMAC key in the workload's
// environment is a key the app could also mint with — but a forged header is
// one the app made for itself, and an app that wants to ignore the role can
// simply ignore the role. No privilege is crossed by an app lying to its own
// handler. What the per-app key buys is the property that matters: app A cannot
// mint a header for app B, because A does not hold B's key. An asymmetric
// signature would remove even the self-minting, at the cost of a dependency the
// Python SDK does not have — hmac and hashlib are standard library in all three
// languages and ed25519 is standard library in one.

// RoleHeader carries the signed role assertion from the platform to the app. It
// is the third platform header, beside [ActForHeader] and X-Toft-Session,
// and like both of those it is deleted from every inbound request before any
// path can set it.
const RoleHeader = "X-Toft-Role"

// roleVersion prefixes every blob, for the same reason actForVersion does: the
// fields are positional and a future format misread as this one is how a
// verifier grows a second, weaker entry point.
const roleVersion = "v1"

// RoleTTL is how long a role assertion is good for.
//
// Seconds, not a session, and compiled in: it is the bound on capture-and-replay
// by something already inside the network boundary, and an operator who could
// raise it could raise it to a day. Two minutes is enough to absorb clock skew
// between a platform node and a workload node and nothing else.
//
// **The app verifies on arrival.** For an ordinary request that is the same
// moment; for a WebSocket, headers arrive at the handshake and the app holds the
// connection for hours, so the role word belongs to the handshake and the app
// caches its own decision from there. An SDK that re-verified a stored header an
// hour later would fail, correctly — the assertion was about one request.
const RoleTTL = 120 * time.Second

// The six words an app is ever told, and there is no seventh.
//
// The authorizer's ladder gained a seventh rung in ObjectRoleList and this list
// did not, which is the point: a list holder never reaches a workload, so no
// request bearing that role is ever forwarded and no header ever carries it.
// [GrantedRole] refuses it rather than collapsing it here.
//
// The three person-words are exactly [GrantedRole]'s output, because that
// function is already the one place six values become three and a second
// mapping is how two answers to one question come to exist. An administrator
// with no grant reads as RoleViewer, exactly as GrantedRole says today, and the
// header carries no admin bit — an app that genuinely needs to know is asking
// about a person, which is what apps.viewer_identity is for.
const (
	// RoleAnonymous is a request with no principal: a stranger on a public app.
	RoleAnonymous = "anonymous"
	// RoleViewer, RoleEditor and RoleOwner are GrantedRole's three, spelled the
	// same as the words the Sharing tab granted from.
	RoleViewer = ObjectRoleViewer
	RoleEditor = ObjectRoleEditor
	RoleOwner  = ObjectRoleOwner
	// RoleSystem is the platform acting on its own. Today its only use is a
	// scheduled task firing on its schedule.
	RoleSystem = "system"
	// RoleApp is the app acting as itself: an app-started job's callback. A
	// machine calling an app is an account and gets its grant's word. Kept apart from RoleSystem because the
	// run tables already distinguish the platform firing a schedule from the app
	// calling itself, and collapsing them at the edge would make the header
	// disagree with the history.
	RoleApp = "app"
	// RoleUnknown is never minted and never appears on the wire. It is what an
	// SDK reports for an absent header, an absent key, a bad MAC or an expired
	// blob — four different failures with one safe answer, because an app author
	// debugging why their editor check refuses does not need four branches, and
	// a verification that distinguished them would be a verification whose
	// failure modes are an oracle. It is never privileged: the helper an app
	// calls is a comparison, and a missing header must refuse, because the one
	// deployment where it is missing is the one nobody configured.
	RoleUnknown = "unknown"
)

// MintedRoles is every word this platform will ever put on the wire, in
// ascending authority order for the person-words. RoleUnknown is deliberately
// absent: it is the SDKs' word for a failure, not a value the platform asserts.
//
// The list exists so a test can compare it against [GrantedRole]'s output — a
// fourth role added to the platform must not quietly become a value the apps
// have never seen.
var MintedRoles = []string{RoleAnonymous, RoleViewer, RoleEditor, RoleOwner, RoleSystem, RoleApp}

// RoleMinter is what a caller outside this package needs in order to put a role
// on a request it is making to an app. *JWTService is the only implementation;
// the interface exists so internal/cronjobs can take one and a test can pass
// nil to mean "this install mints no role", which is what an install with no
// signing key would be.
type RoleMinter interface {
	MintRole(role, appID, workload string, now time.Time) string
	RoleKey(appID, workload string) string
}

// RoleKey is the key one workload verifies its role headers with: per app, per
// start, derived and never stored.
//
// Derived from the platform's signing key, the app id and the workload's own
// name, so a redeploy — which is a new workload name — invalidates every blob
// minted for the previous start, and nothing in audit_log, the API or the UI can
// read it back. It follows the rule the runner's own credentials follow: no key
// reaches a workload that names anything but that workload.
//
// Returned as text because its only destination is an environment variable.
func (j *JWTService) RoleKey(appID, workload string) string {
	if appID == "" || workload == "" {
		return ""
	}
	mac := hmac.New(sha256.New, j.secret)
	mac.Write([]byte("toft-role-key\x00"))
	mac.Write([]byte(roleKeyPayload(appID, workload)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// MintRole signs one role assertion for one app's current workload.
//
// Empty when there is no key to sign with — an app with no current release, or
// an install whose workload was started before this existed. Absent is
// [RoleUnknown] at the far end, which refuses, and that is the right direction
// to fail in.
func (j *JWTService) MintRole(role, appID, workload string, now time.Time) string {
	key := j.RoleKey(appID, workload)
	if key == "" || role == "" {
		return ""
	}
	return MintRoleWith(key, role, appID, now.Add(RoleTTL))
}

// MintRoleWith is the mint with the key already in hand, and it is the exact
// computation each SDK's verify mirrors.
//
// Separate from [JWTService.MintRole] so that the platform-side and app-side
// spellings of the payload live in one function rather than two, and so a test
// can compare a MAC computed here against a MAC computed by each SDK over the
// same input. That comparison is the only way a signing change in Go becomes a
// test failure rather than three apps that stop trusting anything.
func MintRoleWith(key, role, appID string, expiry time.Time) string {
	payload := rolePayload(role, appID, expiry)
	return payload + "." + roleSignature(key, payload)
}

// VerifyRole checks a blob against one app's key and returns the word inside it.
//
// The platform does not call this — nothing here is read by an authorization
// path — but it is the reference implementation the three SDKs mirror, and the
// one a test in this repository can run against a vector each SDK also asserts.
//
// Every failure is [RoleUnknown]: wrong key, wrong app, wrong shape, expired.
func VerifyRole(key, blob, appID string, now time.Time) string {
	if key == "" || blob == "" {
		return RoleUnknown
	}
	idx := strings.LastIndexByte(blob, '.')
	if idx < 0 {
		return RoleUnknown
	}
	payload, sig := blob[:idx], blob[idx+1:]

	// Constant time, and before anything is parsed: structure decoded ahead of
	// authentication is how a verifier grows a second entry point.
	if !hmac.Equal([]byte(sig), []byte(roleSignature(key, payload))) {
		return RoleUnknown
	}

	fields := strings.Split(payload, ".")
	if len(fields) != 4 || fields[0] != roleVersion {
		return RoleUnknown
	}
	signedApp, err := base64.RawURLEncoding.DecodeString(fields[2])
	if err != nil || (appID != "" && string(signedApp) != appID) {
		return RoleUnknown
	}
	unix, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || !time.Unix(unix, 0).UTC().After(now.UTC()) {
		return RoleUnknown
	}
	// The word itself is checked against the closed list, so a blob signed with
	// this app's key — which the app itself holds — cannot introduce a seventh
	// value into code that switches on six.
	for _, known := range MintedRoles {
		if fields[1] == known {
			return known
		}
	}
	return RoleUnknown
}

// CallerRole is the word the platform tells an app about one person, and the
// only place that word is chosen.
//
// It is [GrantedRole] over [EffectiveObjectRole] with the anonymous case in
// front, because *no principal* is a value the three-word ladder cannot express
// and is exactly the fact an app that wants to refuse strangers needs.
//
// [ObjectRoleList] never reaches here and the error says so. A list holder
// cannot read the object, so no path that resolves a caller for a workload can
// legitimately hold one — the six words stay six, and the seventh rung of the
// authorizer's ladder is refused at this seam rather than folded into `viewer`.
// Every caller treats the error as "do not forward this request".
func CallerRole(p *Principal, obj *Object, resolver RoleResolver) (string, error) {
	if p.IsAnonymous() {
		return RoleAnonymous, nil
	}
	return GrantedRole(EffectiveObjectRole(p, obj, resolver))
}

func rolePayload(role, appID string, expiry time.Time) string {
	// base64 on the app id for the reason actForPayload gives: the payload is
	// dot-delimited and positional, and an id containing a dot would otherwise
	// let a caller shift every field along by one. The role is not encoded — it
	// is one of six literals, checked against the list on the way out.
	return fmt.Sprintf("%s.%s.%s.%d",
		roleVersion,
		role,
		base64.RawURLEncoding.EncodeToString([]byte(appID)),
		expiry.UTC().Unix())
}

func roleKeyPayload(appID, workload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(appID)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(workload))
}

// roleSignature domain-separates from every other use of a secret in this
// product, the same way actForSignature does. The key here is already derived
// and already per-app, so the prefix is defence in depth rather than the only
// separation — but a derived key that is one HMAC away from the platform secret
// is exactly the kind of thing that ends up reused.
func roleSignature(key, payload string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("toft-role\x00"))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
