package auth

import (
	"strings"
	"testing"
	"time"
)

// The two properties the whole design rests on, and each of them is a refusal.
//
// Without the app binding, an app could relay a role header it received to
// another app and be believed there. Without the expiry, a header captured once
// by something already on the network path is good forever.
func TestARoleIsBoundToOneAppAndOneMoment(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	now := time.Now().UTC()

	keyA := j.RoleKey("app-a", "app-a-r1")
	keyB := j.RoleKey("app-b", "app-b-r1")
	if keyA == "" || keyB == "" {
		t.Fatal("no key was derived")
	}
	if keyA == keyB {
		t.Fatal("two apps derived the same key — app A could mint for app B")
	}

	blob := j.MintRole(RoleEditor, "app-a", "app-a-r1", now)
	if got := VerifyRole(keyA, blob, "app-a", now); got != RoleEditor {
		t.Fatalf("the app that was minted for reads %q, want %q", got, RoleEditor)
	}

	// Replayed at another app, verified with that app's key: the MAC fails
	// before the app id is even read.
	if got := VerifyRole(keyB, blob, "app-b", now); got != RoleUnknown {
		t.Errorf("app B accepted app A's blob as %q", got)
	}
	// And with the *right* key but the wrong app named — the belt to the
	// signature's braces, so a future key derivation that stopped including the
	// app id still cannot be relayed.
	if got := VerifyRole(keyA, blob, "app-b", now); got != RoleUnknown {
		t.Errorf("a blob for app-a verified against app-b as %q", got)
	}

	// Past its expiry.
	after := now.Add(RoleTTL).Add(time.Second)
	if got := VerifyRole(keyA, blob, "app-a", after); got != RoleUnknown {
		t.Errorf("an expired blob verified as %q", got)
	}
	// And exactly at it: the boundary is not after itself.
	if got := VerifyRole(keyA, blob, "app-a", now.Add(RoleTTL)); got != RoleUnknown {
		t.Errorf("a blob verified at its own expiry as %q", got)
	}
}

// A redeploy is a new workload name, so every assertion minted for the previous
// start stops verifying. That is what "per start" means, and it is the reason
// nothing has to be revoked when an app is redeployed.
func TestARedeployInvalidatesTheKeyItReplaced(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	now := time.Now().UTC()

	old := j.MintRole(RoleOwner, "app-a", "app-a-r1", now)
	newKey := j.RoleKey("app-a", "app-a-r2")

	if got := VerifyRole(newKey, old, "app-a", now); got != RoleUnknown {
		t.Errorf("a blob from the previous start verified against the new one as %q", got)
	}
	// The same start derives the same key twice, which is what lets kubelet
	// restart a container in place without every assertion in flight going
	// stale.
	if j.RoleKey("app-a", "app-a-r1") != j.RoleKey("app-a", "app-a-r1") {
		t.Error("the derivation is not stable for one workload")
	}
}

// Every failure is one word. Four ways to fail with one safe answer is
// deliberate: an app author debugging why their editor check refuses does not
// need four branches, and a verification that distinguished them would be a
// verification whose failure modes are an oracle.
func TestEveryFailureIsUnknownAndUnknownIsNeverPrivileged(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	now := time.Now().UTC()
	key := j.RoleKey("app-a", "app-a-r1")
	good := j.MintRole(RoleOwner, "app-a", "app-a-r1", now)

	cases := map[string]string{
		"absent header":     "",
		"not a blob":        "owner",
		"no signature":      "v1.owner.YXBwLWE.9999999999",
		"wrong version":     strings.Replace(good, "v1.", "v2.", 1),
		"truncated payload": strings.SplitN(good, ".", 2)[1],
		"flipped role":      strings.Replace(good, ".owner.", ".editor.", 1),
	}
	for name, blob := range cases {
		if got := VerifyRole(key, blob, "app-a", now); got != RoleUnknown {
			t.Errorf("%s verified as %q, want %q", name, got, RoleUnknown)
		}
	}
	// No key at all — an app deployed before this existed, or one the platform
	// could not derive a key for.
	if got := VerifyRole("", good, "app-a", now); got != RoleUnknown {
		t.Errorf("a workload with no key read %q", got)
	}
}

// The header's vocabulary is GrantedRole's output plus the three the platform
// speaks for itself, and nothing else. A fourth role added to the platform must
// not quietly become a value the apps have never seen.
func TestTheHeadersVocabularyIsTheOneTheProductAlreadySpeaks(t *testing.T) {
	minted := map[string]bool{}
	for _, r := range MintedRoles {
		minted[r] = true
	}

	// Every value GrantedRole can return has to be a word the header carries.
	for _, effective := range []string{
		ObjectRoleOwner, ObjectRoleEditor, ObjectRoleViewer,
		"authenticated", "anonymous", "none", "",
	} {
		got, err := GrantedRole(effective)
		if err != nil {
			t.Fatalf("GrantedRole(%q) returned %v", effective, err)
		}
		if !minted[got] {
			t.Errorf("GrantedRole(%q) = %q, which the role header cannot carry", effective, got)
		}
	}

	// And the platform's own three are exactly the three that are *not*
	// GrantedRole's — so the list has no member nobody can explain.
	granted := map[string]bool{ObjectRoleOwner: true, ObjectRoleEditor: true, ObjectRoleViewer: true}
	platform := map[string]bool{RoleAnonymous: true, RoleSystem: true, RoleApp: true}
	for _, r := range MintedRoles {
		if !granted[r] && !platform[r] {
			t.Errorf("%q is minted but is neither a granted role nor one of the platform's own", r)
		}
	}
	if minted[RoleUnknown] {
		t.Error("unknown is on the wire — it is the SDKs' word for a failure, never a value the platform asserts")
	}
}

// A word the closed list does not contain is refused even when it is correctly
// signed. The app holds its own key, so this is the one forgery an app can
// actually mount — and while an app lying to its own handler crosses no
// privilege, it must not be able to hand code that switches on six values a
// seventh.
func TestASignedWordOutsideTheListIsStillUnknown(t *testing.T) {
	j := NewJWTService("test-secret-key-32-bytes-long!!!")
	now := time.Now().UTC()
	key := j.RoleKey("app-a", "app-a-r1")

	blob := MintRoleWith(key, "superuser", "app-a", now.Add(RoleTTL))
	if got := VerifyRole(key, blob, "app-a", now); got != RoleUnknown {
		t.Errorf("an invented role verified as %q", got)
	}
}

// CallerRole is the one place the word is chosen, and anonymous is in front of
// the ladder rather than on it: *no principal* is the fact an app that wants to
// refuse strangers needs, and the three-word ladder cannot express it.
func TestCallerRolePutsNobodyInFrontOfTheLadder(t *testing.T) {
	obj := &Object{ID: "app-a", OwnerID: "u-owner", Visibility: "public"}
	resolver := fakeResolver{}

	mustRole := func(t *testing.T, p *Principal) string {
		t.Helper()
		got, err := CallerRole(p, obj, resolver)
		if err != nil {
			t.Fatalf("CallerRole returned %v", err)
		}
		return got
	}

	if got := mustRole(t, nil); got != RoleAnonymous {
		t.Errorf("a request with no principal reads %q, want %q", got, RoleAnonymous)
	}
	owner := &Principal{Type: "user", ID: "u-owner", SystemRole: "publisher"}
	if got := mustRole(t, owner); got != RoleOwner {
		t.Errorf("the owner reads %q, want %q", got, RoleOwner)
	}
	// An administrator with no grant is a viewer here, and the header carries
	// no admin bit — an app that genuinely needs to know is asking about a
	// person, which is what apps.viewer_identity is for.
	admin := &Principal{Type: "user", ID: "u-admin", SystemRole: "system_admin"}
	if got := mustRole(t, admin); got != RoleViewer {
		t.Errorf("an administrator with no grant reads %q, want %q", got, RoleViewer)
	}
}

type fakeResolver struct{}

func (fakeResolver) EffectiveRole(objectID, principalType, principalID string, groups []string) (string, error) {
	return "", nil
}
