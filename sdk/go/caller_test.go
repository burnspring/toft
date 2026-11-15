package toft

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// The ladder is people only, and everything it cannot place is false.
//
// The temptation this refuses is ranking `system` and `app` somewhere: they are
// not more or less authorized than a person, they are a different kind of
// caller, and putting Toft's scheduler above an owner — or an app key above
// one — would be a guess the SDK has no business making on an app's behalf.
func TestAtLeastAnswersOnlyAboutPeople(t *testing.T) {
	cases := []struct {
		role    string
		atLeast map[string]bool
	}{
		{role: RoleOwner, atLeast: map[string]bool{
			RoleAnonymous: true, RoleViewer: true, RoleEditor: true, RoleOwner: true}},
		{role: RoleEditor, atLeast: map[string]bool{
			RoleAnonymous: true, RoleViewer: true, RoleEditor: true, RoleOwner: false}},
		{role: RoleViewer, atLeast: map[string]bool{
			RoleAnonymous: true, RoleViewer: true, RoleEditor: false, RoleOwner: false}},
		{role: RoleAnonymous, atLeast: map[string]bool{
			RoleAnonymous: true, RoleViewer: false, RoleEditor: false, RoleOwner: false}},
		// Not on the ladder at all.
		{role: RoleSystem, atLeast: map[string]bool{
			RoleAnonymous: false, RoleViewer: false, RoleEditor: false, RoleOwner: false}},
		{role: RoleApp, atLeast: map[string]bool{
			RoleAnonymous: false, RoleViewer: false, RoleEditor: false, RoleOwner: false}},
		// The one that protects everything else.
		{role: RoleUnknown, atLeast: map[string]bool{
			RoleAnonymous: false, RoleViewer: false, RoleEditor: false, RoleOwner: false}},
	}
	for _, tc := range cases {
		c := Caller{Role: tc.role}
		for want, expected := range tc.atLeast {
			if got := c.AtLeast(want); got != expected {
				t.Errorf("Caller{%q}.AtLeast(%q) = %v, want %v", tc.role, want, got, expected)
			}
		}
	}

	// AtLeast against something that is not a rung is false however authorized
	// the caller is — an app asking "at least system?" has asked the wrong
	// question and must not be told yes.
	if (Caller{Role: RoleOwner}).AtLeast(RoleSystem) {
		t.Error("an owner is 'at least system', which puts a person on a ladder they are not on")
	}
}

// A missing header refuses, and that is the whole safety property: the one
// deployment where it is missing is the one nobody configured.
func TestAnUnverifiableCallerIsUnknownAndUnknownRefuses(t *testing.T) {
	t.Setenv(EnvRoleKey, "")

	c := CallerFor(http.Header{})
	if c.Role != RoleUnknown {
		t.Errorf("off-platform role = %q, want %q", c.Role, RoleUnknown)
	}
	if c.AtLeast(RoleAnonymous) || c.IsPerson() || c.SignedIn() || c.IsSystem() {
		t.Error("unknown answered yes to something")
	}

	// A key but a header that is not one of ours.
	t.Setenv(EnvRoleKey, "test-role-key")
	forged := http.Header{}
	forged.Set(RoleHeader, "owner")
	if got := CallerFor(forged).Role; got != RoleUnknown {
		t.Errorf("a bare word in the header read as %q — the header is a signed assertion, not a word", got)
	}
}

// A blob signed with this app's own key still cannot introduce a word the SDK
// does not know. The app holds the key, so this is the one forgery it can
// actually mount — and while an app lying to its own handler crosses no
// privilege, app code branching on six values must not be handed a seventh.
func TestASignedWordTheSDKDoesNotKnowIsUnknown(t *testing.T) {
	key := "test-role-key"
	now := time.Now()
	blob := mintForTest(key, "superuser", now.Add(time.Minute))

	if got := verifyRole(key, blob, now); got != RoleUnknown {
		t.Errorf("an invented role verified as %q", got)
	}
	// And the same mint with a known word works, so the test above is about the
	// word rather than about a broken signature.
	if got := verifyRole(key, mintForTest(key, RoleEditor, now.Add(time.Minute)), now); got != RoleEditor {
		t.Fatalf("a valid blob verified as %q — this test proves nothing", got)
	}
}

// SignedIn is the one bit a public app gains: a signed-in reader is tellable
// from a stranger. It names nobody, and it is exactly what "refuse anonymous
// visitors" is.
func TestSignedInSeparatesAStrangerFromAReader(t *testing.T) {
	if (Caller{Role: RoleAnonymous}).SignedIn() {
		t.Error("an anonymous visitor read as signed in")
	}
	if !(Caller{Role: RoleViewer}).SignedIn() {
		t.Error("a viewer did not read as signed in")
	}
	if (Caller{Role: RoleSystem}).SignedIn() || (Caller{Role: RoleUnknown}).SignedIn() {
		t.Error("something that is not a person read as signed in")
	}
}

// The scheduled-task line from the doc comment, run: an endpoint serving both a
// person and its own schedule needs both halves, and neither admits the other.
func TestAScheduledFiringIsNotAPersonAndAPersonIsNotASchedule(t *testing.T) {
	admits := func(c Caller) bool { return c.Role == RoleSystem || c.AtLeast(RoleEditor) }

	for role, want := range map[string]bool{
		RoleSystem:    true,
		RoleOwner:     true,
		RoleEditor:    true,
		RoleViewer:    false,
		RoleAnonymous: false,
		RoleApp:       false,
		RoleUnknown:   false,
	} {
		if got := admits(Caller{Role: role}); got != want {
			t.Errorf("%q admitted = %v, want %v", role, got, want)
		}
	}
}

// mintForTest is the platform's mint, spelled here so this package's tests need
// no import of the platform. sdk/rolevector_test.go is what proves the two
// spellings agree; this one only needs *a* valid blob.
func mintForTest(key, role string, expiry time.Time) string {
	payload := roleVersion + "." + role + "." +
		base64.RawURLEncoding.EncodeToString([]byte("app-a")) + "." +
		strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("toft-role\x00"))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
