package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Issuer/audience pin tokens to this service — defence in depth against a
// token minted by some other HS256 system sharing the secret by accident.
const (
	jwtIssuer   = "toft"
	jwtAudience = "toft"
)

type Claims struct {
	UserID        string   `json:"user_id"`
	Login         string   `json:"login"`
	AvatarURL     string   `json:"avatar_url"`
	PrincipalType string   `json:"principal_type"`
	SystemRole    string   `json:"system_role"`
	Groups        []string `json:"groups,omitempty"`
	OwningUserID  string   `json:"owning_user_id,omitempty"`
	// TokenVersion is users.token_version at mint time. Middleware compares
	// it against the current value so role/group changes and logout revoke
	// outstanding tokens instead of waiting out the expiry.
	TokenVersion int `json:"token_version"`

	// AuthTime is when the person actually authenticated. The OIDC claim of
	// that name, deliberately: assurance recording and re-authentication for
	// privileged actions both want this exact claim, so it is a down payment
	// on those rather than a private invention.
	//
	// `exp` slides forward as a session is used; this never moves. It is what
	// the absolute ceiling is measured from, and carrying it through renewal
	// unchanged is what makes the ceiling a ceiling.
	AuthTime *jwt.NumericDate `json:"auth_time,omitempty"`

	jwt.RegisteredClaims
}

// SessionStart is when this session began.
//
// `iat` is the fallback for a token minted before `auth_time` existed — and for
// a token that has never been renewed the two are the same instant, so an
// outstanding eight-hour session keeps exactly the deadline it was issued with.
// A token carrying neither is refused rather than admitted: the zero time puts
// the ceiling in year 1, and failing closed is the right direction for a claim
// every token this platform mints does carry.
func (c *Claims) SessionStart() time.Time {
	if c.AuthTime != nil {
		return c.AuthTime.Time
	}
	if c.IssuedAt != nil {
		return c.IssuedAt.Time
	}
	return time.Time{}
}

// Session is a minted token and the one fact every cookie-setter needs about
// it. Returned rather than just the string because the cookie's MaxAge and the
// token's `exp` are the same number: they were two constants that happened to
// agree, which is how a session policy comes to be half-applied.
type Session struct {
	Token     string
	ExpiresAt time.Time
}

type JWTService struct {
	secret []byte
}

func NewJWTService(secret string) *JWTService {
	return &JWTService{secret: []byte(secret)}
}

type GenerateParams struct {
	UserID        string
	Login         string
	AvatarURL     string
	PrincipalType string
	SystemRole    string
	Groups        []string
	TokenVersion  int
	// Policy is the install's two numbers. A zero policy would mint a token
	// already expired, which is the right direction to fail in — every caller
	// that mints a session has settings to read.
	Policy SessionPolicy
	// AuthTime is when the person proved who they are. Zero means now, which
	// is every sign-in. The one caller that sets it is the app handshake,
	// which proves nothing new and mints the apps-origin session from the
	// platform session's own start, so the absolute ceiling is the same
	// ceiling on both origins.
	AuthTime time.Time
	// Audience is "" for a platform session, and AppsAudience(origin) for the
	// session the app handshake mints on an apps origin or an app's own host.
	Audience string
}

// AppsAudience is the audience of a session minted for one gateway origin —
// the shared apps origin or one app's hostname — by the app handshake.
//
// **Never the platform's.** That session is minted from a grant that crosses
// the apps origin in a URL, where the app's own code can see it (a service
// worker at scope `/` on an app host intercepts the redeem navigation). When
// it was a platform session, a captured grant redeemed in the author's own
// browser was the viewer's platform session, and the same token was a session
// at every other app host (security review 2026-11-10, D-1). With the origin
// in the audience it is worth exactly what the app could already do as the
// viewer from inside their browser, on its own origin, and nothing anywhere
// else: Validate refuses it, and ValidateFor another origin refuses it.
func AppsAudience(origin string) string { return "toft-apps " + origin }

// Generate mints a session for somebody who has just proved who they are. This
// is where `auth_time` is set, and it is the only place it is set: renewal
// carries it forward, the handshake passes the one it was handed, and nothing
// else writes it.
func (j *JWTService) Generate(p GenerateParams) (Session, error) {
	if p.PrincipalType == "" {
		p.PrincipalType = "user"
	}
	if p.SystemRole == "" {
		p.SystemRole = "viewer"
	}
	// jti: an opaque id the apps gateway forwards as X-Toft-Session.
	// Minted once per browser login: every request replaying the cookie carries
	// the same id, and a second login (another browser, device, or private
	// window) mints a different one. It says "which login", never "who".
	//
	// **Renew does not mint a new one**, which is the whole reason that method
	// exists rather than a second call to this one. The SDKs key
	// `session_scratch_dir` off this value, so a fresh id every seven minutes
	// would orphan an app's per-session files — a bug that gets reported as
	// "the app randomly loses state" and diagnosed nowhere near here.
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return Session{}, fmt.Errorf("minting session id: %w", err)
	}
	now := time.Now().UTC()
	authTime := now
	if !p.AuthTime.IsZero() {
		authTime = p.AuthTime.UTC()
	}
	claims := &Claims{
		UserID:        p.UserID,
		Login:         p.Login,
		AvatarURL:     p.AvatarURL,
		PrincipalType: p.PrincipalType,
		SystemRole:    p.SystemRole,
		Groups:        p.Groups,
		TokenVersion:  p.TokenVersion,
		AuthTime:      jwt.NewNumericDate(authTime),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        base64.RawURLEncoding.EncodeToString(jti),
			ExpiresAt: jwt.NewNumericDate(p.Policy.Expiry(authTime, now)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{audienceOr(p.Audience)},
		},
	}
	return j.sign(claims)
}

// Renew re-mints a live session under the current policy, sliding `exp`
// forward. It is not a new session and must never look like one.
//
// The `jti` and `auth_time` are carried forward: the same id in the access log
// and in `X-Toft-Session`, the same scratch directory under an SDK, the
// same absolute deadline. Everything else is copied unchanged, which is safe
// precisely because it is not the thing that keeps a session honest — a role
// change, a group change, a demotion or a logout bumps `token_version`, and
// CheckSession kills the session on the next request whatever its claims say.
func (j *JWTService) Renew(c *Claims, policy SessionPolicy) (Session, error) {
	if c == nil {
		return Session{}, fmt.Errorf("renewing a session with no claims")
	}
	now := time.Now().UTC()
	next := *c
	next.AuthTime = jwt.NewNumericDate(c.SessionStart())
	next.ExpiresAt = jwt.NewNumericDate(policy.Expiry(c.SessionStart(), now))
	next.IssuedAt = jwt.NewNumericDate(now)
	next.Issuer = jwtIssuer
	// The audience is the session's own: an apps-origin session renews as
	// one, never into a platform session.
	aud := jwtAudience
	if len(c.Audience) == 1 {
		aud = c.Audience[0]
	}
	next.Audience = jwt.ClaimStrings{aud}
	return j.sign(&next)
}

func audienceOr(a string) string {
	if a == "" {
		return jwtAudience
	}
	return a
}

func (j *JWTService) sign(claims *Claims) (Session, error) {
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(j.secret)
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, ExpiresAt: claims.ExpiresAt.Time}, nil
}

// Validate accepts a platform session only. ValidateFor names another
// audience: an apps-origin session (AppsAudience) is never a platform one.
func (j *JWTService) Validate(tokenStr string) (*Claims, error) {
	return j.ValidateFor(tokenStr, jwtAudience)
}

// ValidateFor is Validate for a session minted for audience.
func (j *JWTService) ValidateFor(tokenStr, audience string) (*Claims, error) {
	// The HMAC assertion closes alg:none and RS256→HS256 confusion; the
	// options make exp/iss/aud mandatory rather than validated-when-present.
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return j.secret, nil
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(audience),
	)
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}
