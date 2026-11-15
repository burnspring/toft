package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testPolicy(absolute, idle time.Duration) SessionPolicy {
	return SessionPolicy{Absolute: absolute, Idle: idle}
}

// The rule, in the three shapes it takes.
func TestExpiryIsTheLesserOfTheTwoClocks(t *testing.T) {
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)

	t.Run("idle off is a flat lifetime from sign-in", func(t *testing.T) {
		p := testPolicy(8*time.Hour, 0)
		// Six hours in, and the answer is still the sign-in deadline: with no
		// idle window there is nothing to slide.
		if got, want := p.Expiry(start, start.Add(6*time.Hour)), start.Add(8*time.Hour); !got.Equal(want) {
			t.Errorf("exp = %v, want %v", got, want)
		}
	})

	t.Run("idle on slides with the request", func(t *testing.T) {
		p := testPolicy(8*time.Hour, 30*time.Minute)
		now := start.Add(2 * time.Hour)
		if got, want := p.Expiry(start, now), now.Add(30*time.Minute); !got.Equal(want) {
			t.Errorf("exp = %v, want %v", got, want)
		}
	})

	t.Run("the ceiling wins in the last stretch", func(t *testing.T) {
		p := testPolicy(8*time.Hour, 30*time.Minute)
		// Ten minutes from the ceiling, so a full idle window would overrun it.
		now := start.Add(7*time.Hour + 50*time.Minute)
		if got, want := p.Expiry(start, now), start.Add(8*time.Hour); !got.Equal(want) {
			t.Errorf("exp = %v, want %v", got, want)
		}
	})
}

// The renewal threshold: a quarter of the idle window, so an active browser
// gets a Set-Cookie every seven and a half minutes at the default rather than
// one on every response.
func TestShouldRenewWaitsAQuarterOfTheIdleWindow(t *testing.T) {
	p := testPolicy(8*time.Hour, 30*time.Minute)
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	c := claimsAt(start, start, p)
	if p.ShouldRenew(c, start.Add(7*time.Minute)) {
		t.Error("renewed inside the first quarter of the idle window")
	}
	if !p.ShouldRenew(c, start.Add(8*time.Minute)) {
		t.Error("did not renew past the first quarter of the idle window")
	}
}

// With the idle timeout off nothing renews, which is what makes `0` today's
// behaviour exactly rather than approximately.
func TestNothingRenewsWithTheIdleTimeoutOff(t *testing.T) {
	p := testPolicy(8*time.Hour, 0)
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	c := claimsAt(start, start, p)

	for _, after := range []time.Duration{time.Minute, time.Hour, 7 * time.Hour} {
		if p.ShouldRenew(c, start.Add(after)) {
			t.Errorf("renewed %v in with the idle timeout off", after)
		}
	}
}

// Near the ceiling `exp` is pinned, so a renewal would buy nothing and a
// Set-Cookie on every request would be pure noise.
func TestRenewalStopsWhenItWouldBuyNothing(t *testing.T) {
	p := testPolicy(8*time.Hour, 30*time.Minute)
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	// Issued at 7h30 into the session, so its exp is already the ceiling.
	c := claimsAt(start, start.Add(7*time.Hour+30*time.Minute), p)

	if !c.ExpiresAt.Time.Equal(start.Add(8 * time.Hour)) {
		t.Fatalf("fixture: exp = %v, want the ceiling", c.ExpiresAt.Time)
	}
	if p.ShouldRenew(c, start.Add(7*time.Hour+50*time.Minute)) {
		t.Error("renewed a token whose exp is already pinned at the ceiling")
	}
}

// A renewed token is the SAME session: same jti, same auth_time, same ceiling.
// A fresh jti would orphan an app's per-session scratch directory every few
// minutes, which is reported as "the app randomly loses state".
func TestRenewalPreservesTheSessionIdentity(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	p := testPolicy(8*time.Hour, 30*time.Minute)

	first, err := j.Generate(GenerateParams{UserID: "priya", Login: "priya", TokenVersion: 4, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	before, err := j.Validate(first.Token)
	if err != nil {
		t.Fatal(err)
	}

	second, err := j.Renew(before, p)
	if err != nil {
		t.Fatal(err)
	}
	after, err := j.Validate(second.Token)
	if err != nil {
		t.Fatal(err)
	}

	if after.ID != before.ID {
		t.Errorf("jti changed on renewal: %q -> %q", before.ID, after.ID)
	}
	if !after.SessionStart().Equal(before.SessionStart()) {
		t.Errorf("auth_time moved on renewal: %v -> %v", before.SessionStart(), after.SessionStart())
	}
	if after.AuthTime == nil {
		t.Error("a renewed token carries no auth_time, so it has no ceiling")
	}
	if after.UserID != "priya" || after.TokenVersion != 4 {
		t.Errorf("claims were not carried forward: %+v", after)
	}
}

// The ceiling holds across renewals: an hour of renewals cannot extend a
// session past `auth_time + absolute`.
func TestRenewalCannotOutliveTheCeiling(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	p := testPolicy(time.Hour, 30*time.Minute)
	start := time.Now().UTC().Truncate(time.Second).Add(-50 * time.Minute)

	renewed, err := j.Renew(claimsAt(start, start.Add(40*time.Minute), p), p)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := renewed.ExpiresAt, start.Add(time.Hour); !got.Equal(want) {
		t.Errorf("renewed exp = %v, want the ceiling %v", got, want)
	}
}

// The check that is independent of `exp`, and the reason it exists: an
// administrator who shortens the lifetime this morning has to reach the
// sessions minted last night.
func TestTheCeilingIsCheckedIndependentlyOfExp(t *testing.T) {
	states := &fakeVersions{versions: map[string]int{"u1": 1}}
	start := time.Now().UTC().Truncate(time.Second).Add(-6 * time.Hour)
	// A perfectly valid token under the old eight-hour policy: two hours of
	// `exp` left on it.
	c := claimsAt(start, time.Now().UTC(), testPolicy(8*time.Hour, 0))
	c.UserID, c.TokenVersion = "u1", 1

	if !CheckSession(states, c, testPolicy(8*time.Hour, 0)) {
		t.Fatal("a live session was refused under the policy it was minted with")
	}
	if CheckSession(states, c, testPolicy(time.Hour, 0)) {
		t.Error("a shortened absolute lifetime did not reach an outstanding session")
	}
}

// A token minted before `auth_time` existed keeps exactly the deadline it was
// issued with, because for a session that has never been renewed `iat` and
// `auth_time` are the same instant. Nobody is signed out by the upgrade.
func TestPreUpgradeTokensFallBackToIssuedAt(t *testing.T) {
	states := &fakeVersions{versions: map[string]int{"u1": 1}}
	p := testPolicy(8*time.Hour, 0)
	issued := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)

	c := claimsAt(issued, issued, p)
	c.AuthTime = nil // as an old token arrives
	c.UserID, c.TokenVersion = "u1", 1

	if !c.SessionStart().Equal(issued) {
		t.Errorf("SessionStart = %v, want iat %v", c.SessionStart(), issued)
	}
	if !CheckSession(states, c, p) {
		t.Error("a token minted before the claim existed was refused")
	}
}

// Neither claim is fail-closed. Every token this platform mints carries iat,
// so the only thing this refuses is something we did not mint.
func TestClaimsWithNoClockAreRefused(t *testing.T) {
	states := &fakeVersions{versions: map[string]int{"u1": 1}}
	c := &Claims{UserID: "u1", TokenVersion: 1}

	if !c.SessionStart().IsZero() {
		t.Fatalf("SessionStart = %v, want the zero time", c.SessionStart())
	}
	if CheckSession(states, c, testPolicy(8*time.Hour, 0)) {
		t.Error("claims carrying no clock at all were admitted")
	}
}

// claimsAt builds the claims a token minted at authTime and last issued at
// issuedAt would carry under this policy.
func claimsAt(authTime, issuedAt time.Time, p SessionPolicy) *Claims {
	return &Claims{
		AuthTime: jwt.NewNumericDate(authTime),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "session-id",
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(p.Expiry(authTime, issuedAt)),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
}

// Renewal at the middleware, which is the only thing that makes the idle clock
// real: nothing is written down, so being used *is* the renewal.
func TestMiddlewareRenewsAnActiveSession(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	p := testPolicy(8*time.Hour, 30*time.Minute)
	states := &fakeVersions{versions: map[string]int{"u1": 2}}

	// A cookie last issued twenty minutes ago: past the quarter-window
	// threshold, and still inside the idle window, so it is both live and due.
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	c := claimsAt(start, time.Now().UTC().Add(-20*time.Minute), p)
	c.UserID, c.TokenVersion = "u1", 2
	presented, err := j.sign(c)
	if err != nil {
		t.Fatal(err)
	}

	var renewed *Session
	h := Middleware(j, states, Options{
		Policy: func() SessionPolicy { return p },
		Renew:  func(_ http.ResponseWriter, s Session) { renewed = &s },
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: presented.Token})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if renewed == nil {
		t.Fatal("an active session was not renewed")
	}
	if !renewed.ExpiresAt.After(presented.ExpiresAt) {
		t.Errorf("renewed exp %v did not move past %v", renewed.ExpiresAt, presented.ExpiresAt)
	}
	after, err := j.Validate(renewed.Token)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != c.ID {
		t.Errorf("the renewed cookie carries a different session id: %q, want %q", after.ID, c.ID)
	}
	if !after.SessionStart().Equal(start) {
		t.Errorf("the renewed cookie moved the ceiling: %v, want %v", after.SessionStart(), start)
	}
}

// A session past the ceiling is refused, and refused *before* anything renews
// it — otherwise the ceiling would be a suggestion.
func TestMiddlewareRefusesAndDoesNotRenewPastTheCeiling(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	states := &fakeVersions{versions: map[string]int{"u1": 2}}

	// Minted six hours ago under an eight-hour policy, so its own exp is
	// perfectly live. The install now says one hour.
	start := time.Now().UTC().Truncate(time.Second).Add(-6 * time.Hour)
	c := claimsAt(start, time.Now().UTC().Add(-20*time.Minute), testPolicy(8*time.Hour, 30*time.Minute))
	c.UserID, c.TokenVersion = "u1", 2
	presented, err := j.sign(c)
	if err != nil {
		t.Fatal(err)
	}

	var seen *Principal
	renewals := 0
	h := Middleware(j, states, Options{
		Policy: func() SessionPolicy { return testPolicy(time.Hour, 30*time.Minute) },
		Renew:  func(http.ResponseWriter, Session) { renewals++ },
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = GetPrincipal(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: presented.Token})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !seen.IsAnonymous() {
		t.Error("a session past the install's absolute ceiling was still authenticated")
	}
	if renewals != 0 {
		t.Errorf("a session past the ceiling was renewed %d times", renewals)
	}
}

// A *shortened* idle window reaches a session that is already open, on its next
// request. Renewal deliberately moves `exp` backwards for this: a rule that only
// ever moved it forward would leave outstanding tokens living out the window
// they were minted under, which is the same "off means now" failure the absolute
// ceiling exists to avoid.
func TestShorteningTheIdleWindowReachesAnOpenSession(t *testing.T) {
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	// Minted under a thirty-minute window, so its exp is start+30m.
	c := claimsAt(start, start, testPolicy(8*time.Hour, 30*time.Minute))

	// The install now says five minutes. Two minutes in is past a quarter of
	// the *new* window, so the next request re-mints against it.
	tightened := testPolicy(8*time.Hour, 5*time.Minute)
	now := start.Add(2 * time.Minute)
	if !tightened.ShouldRenew(c, now) {
		t.Fatal("a shortened idle window did not reach a session already open")
	}
	if got, want := tightened.Expiry(c.SessionStart(), now), now.Add(5*time.Minute); !got.Equal(want) {
		t.Errorf("re-minted exp = %v, want the new window %v", got, want)
	}
	if !tightened.Expiry(c.SessionStart(), now).Before(c.ExpiresAt.Time) {
		t.Error("the fixture does not exercise a backwards move, so it proves nothing")
	}
}
