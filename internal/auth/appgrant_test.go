package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const testNonce = "n0nce-n0nce-n0nce-n0nce"

func grantService() *JWTService {
	return NewJWTService("test-secret-that-is-long-enough-for-hmac")
}

func TestAppGrantRoundTrip(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)

	blob := s.MintAppGrant("user-1", "https://apps.example.com", 7, now, testNonce, now)
	g, err := s.VerifyAppGrant(blob, "https://apps.example.com", now)
	if err != nil {
		t.Fatalf("VerifyAppGrant: %v", err)
	}
	if g.UserID != "user-1" || g.TokenVersion != 7 || g.Nonce != testNonce {
		t.Errorf("grant = %+v, want user-1 / 7 / %s", g, testNonce)
	}
}

// A grant from before the browser nonce (g2) carries none and is refused, as
// is a grant minted without one.
func TestAppGrantWithoutANonceIsRefused(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)
	payload := strings.Join([]string{"g2", "user-1", "https://apps.example.com", "1",
		"1700000030", "1700000000"}, "|")
	if _, err := s.VerifyAppGrant(payload+"|"+s.signAppGrant(payload), "https://apps.example.com", now); !errors.Is(err, ErrAppGrantInvalid) {
		t.Errorf("g2 grant = %v, want ErrAppGrantInvalid", err)
	}
	for _, bad := range []string{"", "short", "has|a|bar-in-it-long-enough"} {
		blob := s.MintAppGrant("user-1", "https://apps.example.com", 1, now, bad, now)
		if _, err := s.VerifyAppGrant(blob, "https://apps.example.com", now); !errors.Is(err, ErrAppGrantInvalid) {
			t.Errorf("nonce %q = %v, want ErrAppGrantInvalid", bad, err)
		}
	}
}

// The binding that makes a leaked grant worthless elsewhere. Without it, a grant
// cut for one install's apps host would establish a session at another's — or at
// the platform's own origin, which is the whole boundary being defended.
func TestAppGrantIsBoundToOneOrigin(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)
	blob := s.MintAppGrant("user-1", "https://apps.example.com", 1, now, testNonce, now)

	for _, wrong := range []string{
		"https://apps.evil.com",
		"https://toft.example.com", // the platform's own origin
		"http://apps.example.com",  // scheme is part of an origin
		"https://apps.example.com.evil.com",
	} {
		if _, err := s.VerifyAppGrant(blob, wrong, now); !errors.Is(err, ErrAppGrantInvalid) {
			t.Errorf("grant verified at %s (want ErrAppGrantInvalid, got %v)", wrong, err)
		}
	}

	// Case and a trailing slash are how the same origin gets written two ways by
	// configuration and by a browser; they must not be a refusal.
	for _, same := range []string{"https://APPS.example.com", "https://apps.example.com/"} {
		if _, err := s.VerifyAppGrant(blob, same, now); err != nil {
			t.Errorf("grant refused at equivalent origin %s: %v", same, err)
		}
	}
}

func TestAppGrantExpires(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)
	blob := s.MintAppGrant("user-1", "https://apps.example.com", 1, now, testNonce, now)

	if _, err := s.VerifyAppGrant(blob, "https://apps.example.com", now.Add(AppGrantTTL-time.Second)); err != nil {
		t.Errorf("grant refused inside its window: %v", err)
	}
	_, err := s.VerifyAppGrant(blob, "https://apps.example.com", now.Add(AppGrantTTL+time.Second))
	if !errors.Is(err, ErrAppGrantExpired) {
		t.Errorf("expired grant gave %v, want ErrAppGrantExpired", err)
	}
}

// A grant is a bearer credential in a URL. Every field must be under the
// signature, or an attacker edits the one they want and keeps the rest.
func TestAppGrantRejectsTamperedFields(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)
	blob := s.MintAppGrant("user-1", "https://apps.example.com", 1, now, testNonce, now)
	parts := strings.Split(blob, "|")

	tamper := func(i int, v string) string {
		c := append([]string(nil), parts...)
		c[i] = v
		return strings.Join(c, "|")
	}
	cases := map[string]string{
		"another user":       tamper(1, "user-2"),
		"another origin":     tamper(2, "https://apps.evil.com"),
		"older tokenversion": tamper(3, "0"),
		"later expiry":       tamper(4, "9999999999"),
		"earlier start":      tamper(5, "1600000000"),
		"another nonce":      tamper(6, "another-nonce-another"),
		"forged signature":   tamper(7, "AAAA"),
	}
	for name, bad := range cases {
		if _, err := s.VerifyAppGrant(bad, "https://apps.example.com", now); err == nil {
			t.Errorf("%s: tampered grant verified", name)
		}
	}
}

// A key that signs several kinds of blob must not let one be presented as
// another. The domain-separation prefix is what prevents it.
func TestAppGrantIsNotAnActForBlob(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)
	blob := s.MintAppGrant("user-1", "https://apps.example.com", 1, now, testNonce, now)

	if _, err := s.VerifyActFor(blob, "app-1"); err == nil {
		t.Error("an app grant verified as an act-for blob")
	}
}

func TestAppGrantRejectsGarbage(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)
	for _, bad := range []string{"", "not-a-grant", "g1|only|three", strings.Repeat("|", 7)} {
		if _, err := s.VerifyAppGrant(bad, "https://apps.example.com", now); err == nil {
			t.Errorf("verified garbage %q", bad)
		}
	}
}

// The grant carries the platform session's start, and the redeemer mints from
// it: that is the only way the apps-origin cookie can inherit the sign-in's
// clock, because the two origins share nothing else.
func TestAppGrantCarriesTheSessionStart(t *testing.T) {
	s := grantService()
	now := time.Unix(1_700_000_000, 0)
	start := now.Add(-3 * time.Hour)
	blob := s.MintAppGrant("user-1", "https://apps.example.com", 1, start, testNonce, now)
	g, err := s.VerifyAppGrant(blob, "https://apps.example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	if !g.SessionStart.Equal(start) {
		t.Fatalf("SessionStart = %v, want %v", g.SessionStart, start)
	}
}

// Generate stamps auth_time now unless told otherwise; told otherwise, both
// auth_time and the ceiling-derived exp follow the earlier clock.
func TestGenerateHonoursAnEarlierAuthTime(t *testing.T) {
	s := grantService()
	policy := SessionPolicy{Absolute: 8 * time.Hour}
	start := time.Now().UTC().Add(-7 * time.Hour).Truncate(time.Second)
	sess, err := s.Generate(GenerateParams{UserID: "u", Policy: policy, AuthTime: start})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := s.Validate(sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !claims.SessionStart().Equal(start) {
		t.Errorf("auth_time = %v, want %v", claims.SessionStart(), start)
	}
	if want := start.Add(8 * time.Hour); !sess.ExpiresAt.Equal(want) {
		t.Errorf("exp = %v, want the ceiling from the earlier start, %v", sess.ExpiresAt, want)
	}
}
