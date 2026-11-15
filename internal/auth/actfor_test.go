package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The act-for blob is the only thing standing between "an app relayed a header"
// and "the platform believes a person did something". Every test here is a way
// that could go wrong.

func TestActForRoundTrips(t *testing.T) {
	j := NewJWTService("test-secret")
	expiry := time.Now().Add(time.Hour).Truncate(time.Second).UTC()

	blob := j.MintActFor("priya", "app-1", 7, expiry)
	got, err := j.VerifyActFor(blob, "app-1")
	if err != nil {
		t.Fatalf("verifying a blob we just minted: %v", err)
	}
	if got.UserID != "priya" || got.AppID != "app-1" || got.TokenVersion != 7 {
		t.Errorf("got %+v, want priya/app-1/7", got)
	}
	if !got.ExpiresAt.Equal(expiry) {
		t.Errorf("expiry = %v, want %v", got.ExpiresAt, expiry)
	}
	// The app relays this verbatim and must not be able to read it. Not
	// encryption — an app that decoded the base64 would find the user id — but
	// it must at least not be handed the id in the clear, and nothing in the
	// SDK ever parses it.
	if strings.Contains(blob, "priya") {
		t.Error("the blob spells the user id out in plain text")
	}
}

// A different secret is a different platform. This is the property that makes
// the whole thing work: an app cannot mint one, because it does not have the
// key.
func TestActForRefusesAnotherPlatformsSignature(t *testing.T) {
	mine := NewJWTService("test-secret")
	theirs := NewJWTService("some-other-secret")

	blob := theirs.MintActFor("priya", "app-1", 1, time.Now().Add(time.Hour))
	if _, err := mine.VerifyActFor(blob, "app-1"); !errors.Is(err, ErrActForInvalid) {
		t.Errorf("err = %v, want ErrActForInvalid", err)
	}
}

// Every field is signed, so altering any of them invalidates the whole blob —
// which is what stops an app promoting its viewer, extending their session, or
// pointing the blob at another app.
func TestActForRefusesEveryTamper(t *testing.T) {
	j := NewJWTService("test-secret")
	blob := j.MintActFor("priya", "app-1", 1, time.Now().Add(time.Hour))
	parts := strings.Split(blob, ".")
	if len(parts) != 6 {
		t.Fatalf("unexpected blob shape: %q", blob)
	}

	tampers := map[string]string{
		"a different user":    strings.Join(append([]string{parts[0], "c2FtCg"}, parts[2:]...), "."),
		"a different app":     strings.Join(append([]string{parts[0], parts[1], "YXBwLTI"}, parts[3:]...), "."),
		"a bumped version":    strings.Join(append([]string{parts[0], parts[1], parts[2], "99"}, parts[4:]...), "."),
		"a stretched expiry":  strings.Join(append(append([]string{}, parts[:4]...), "99999999999", parts[5]), "."),
		"a swapped signature": strings.Join(append(append([]string{}, parts[:5]...), "AAAA"), "."),
	}
	for name, blob := range tampers {
		if _, err := j.VerifyActFor(blob, "app-1"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The app id is signed, so a blob a viewer's browser produced for app A buys
// nothing when app A relays it to app B. This is the case an attacker actually
// has: any app they can view mints one for them.
func TestActForIsBoundToOneApp(t *testing.T) {
	j := NewJWTService("test-secret")
	blob := j.MintActFor("priya", "app-a", 1, time.Now().Add(time.Hour))

	if _, err := j.VerifyActFor(blob, "app-b"); !errors.Is(err, ErrActForWrongApp) {
		t.Errorf("err = %v, want ErrActForWrongApp", err)
	}
	if _, err := j.VerifyActFor(blob, "app-a"); err != nil {
		t.Errorf("the app it was minted for was refused: %v", err)
	}
}

func TestActForExpires(t *testing.T) {
	j := NewJWTService("test-secret")
	blob := j.MintActFor("priya", "app-1", 1, time.Now().Add(-time.Minute))

	// Separated from the flat refusal because it is the one failure an app can
	// render usefully: nothing is broken, the person's session ended.
	if _, err := j.VerifyActFor(blob, "app-1"); !errors.Is(err, ErrActForExpired) {
		t.Errorf("err = %v, want ErrActForExpired", err)
	}
}

// The secret also signs session JWTs and run tokens. Domain separation is what
// stops a string that was valid in one of those contexts being replayed here.
func TestActForIsDomainSeparated(t *testing.T) {
	j := NewJWTService("test-secret")
	sess, err := j.Generate(GenerateParams{UserID: "priya", Login: "priya", TokenVersion: 1, Policy: DefaultSessionPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.VerifyActFor(sess.Token, "app-1"); err == nil {
		t.Error("a session JWT was accepted as an act-for blob")
	}
}

// fakeStates answers the liveness question the signature cannot.
type fakeStates struct {
	version int
	locked  bool
	err     error
}

func (f fakeStates) GetSessionState(userID string) (int, bool, error) {
	return f.version, f.locked, f.err
}

// The blob is a narrowed capability of one session, never an independent
// credential — so a logout, a demotion, a group change or a lock kills it, and
// that is what makes binding its expiry to the session's own safe rather than
// generous.
func TestActForDiesWithItsSession(t *testing.T) {
	j := NewJWTService("test-secret")
	blob := j.MintActFor("priya", "app-1", 5, time.Now().Add(time.Hour))
	viewer, err := j.VerifyActFor(blob, "app-1")
	if err != nil {
		t.Fatal(err)
	}

	if !CheckActForSession(fakeStates{version: 5}, viewer) {
		t.Error("a live session was rejected")
	}
	if CheckActForSession(fakeStates{version: 6}, viewer) {
		t.Error("a bumped token version must invalidate the blob")
	}
	if CheckActForSession(fakeStates{version: 5, locked: true}, viewer) {
		t.Error("a locked account's blob must be refused")
	}
	if CheckActForSession(fakeStates{err: errors.New("database down")}, viewer) {
		t.Error("an unreadable session state must fail closed")
	}
	if CheckActForSession(nil, viewer) {
		t.Error("no store means no answer, which means no")
	}
}

// The anonymous blob is a narrowing and never a grant: it names an app and an
// expiry and no subject, and it cannot be mistaken for the shape that does name
// one.
func TestAnAnonymousActForNamesNobodyAndCannotBeReadAsA1(t *testing.T) {
	j := NewJWTService("test-secret")
	expiry := time.Now().Add(time.Hour)

	blob := j.MintAnonymousActFor("app-a", expiry)
	got, err := j.VerifyActFor(blob, "app-a")
	if err != nil {
		t.Fatalf("verifying an anonymous blob: %v", err)
	}
	if !got.Anonymous {
		t.Error("the blob did not come back marked anonymous")
	}
	if got.UserID != "" {
		t.Errorf("an anonymous blob named %q", got.UserID)
	}
	if got.AppID != "app-a" {
		t.Errorf("app = %q, want app-a", got.AppID)
	}

	// Bound to one app, exactly like a1: relaying it to another app buys
	// nothing.
	if _, err := j.VerifyActFor(blob, "app-b"); !errors.Is(err, ErrActForWrongApp) {
		t.Errorf("app B accepted app A's anonymous blob: %v", err)
	}

	// There is no session behind it, so the liveness check has nothing to find
	// stale — and answering true there is not a weakening, because every caller
	// that needs a person refuses it by name first.
	if !CheckActForSession(fakeStates{}, got) {
		t.Error("an anonymous blob failed a session check it has no session for")
	}
}

// a1's user slot is never empty when this platform minted one, so an empty one
// is either a forgery attempt or a bug. Reading it as *nobody in particular*
// would be reading a subject field as an assertion about no subject — which is
// exactly the mistake a version prefix exists to prevent.
func TestAnEmptyUserInA1IsRefusedRatherThanReadAsAnonymous(t *testing.T) {
	j := NewJWTService("test-secret")
	blob := j.MintActFor("", "app-a", 1, time.Now().Add(time.Hour))

	if _, err := j.VerifyActFor(blob, "app-a"); !errors.Is(err, ErrActForInvalid) {
		t.Errorf("an a1 blob naming nobody was not refused: %v", err)
	}
}

// An anonymous blob expires like everything else, and it is signed by the same
// key — so neither a forged one nor a stale one is accepted.
func TestAnAnonymousActForIsSignedAndExpires(t *testing.T) {
	mine := NewJWTService("test-secret")
	theirs := NewJWTService("some-other-secret")

	forged := theirs.MintAnonymousActFor("app-a", time.Now().Add(time.Hour))
	if _, err := mine.VerifyActFor(forged, "app-a"); !errors.Is(err, ErrActForInvalid) {
		t.Errorf("a blob signed by another key was accepted: %v", err)
	}

	stale := mine.MintAnonymousActFor("app-a", time.Now().Add(-time.Minute))
	if _, err := mine.VerifyActFor(stale, "app-a"); !errors.Is(err, ErrActForExpired) {
		t.Errorf("an expired anonymous blob was accepted: %v", err)
	}
}
