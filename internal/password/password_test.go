package password

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	hash, err := Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !Verify(hash, "correct-horse-battery-staple") {
		t.Error("Verify rejected the password it was derived from")
	}
	if Verify(hash, "correct-horse-battery-stapl") {
		t.Error("Verify accepted a near-miss")
	}
	if Verify(hash, "") {
		t.Error("Verify accepted an empty password")
	}
}

// Two hashes of one password must differ, or the store leaks which accounts
// share a password.
func TestHashIsSalted(t *testing.T) {
	a, err := Hash(Default)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := Hash(Default)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical — the salt is not random")
	}
	if !Verify(a, Default) || !Verify(b, Default) {
		t.Error("a salted hash failed to verify")
	}
}

// A stored value that cannot be parsed is a failed verification, never a passed
// one. The empty string matters most: it is what a missing or truncated column
// looks like, and accepting it would authenticate everybody.
func TestVerifyFailsClosedOnMalformedHashes(t *testing.T) {
	for _, encoded := range []string{
		"",
		"$",
		"not-a-hash",
		"$pbkdf2-sha512$i=210000$onlythreefields",
		"$pbkdf2-sha512$i=0$c2FsdA$aGFzaA",
		"$pbkdf2-sha512$i=abc$c2FsdA$aGFzaA",
		"$pbkdf2-sha512$i=210000$c2FsdA$",              // no derived key
		"$pbkdf2-sha256$i=600000$c2FsdA$aGFzaA",        // another algorithm
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA", // a plausible one we do not implement
		"$pbkdf2-sha512$i=210000$!!!$aGFzaA",           // unparseable salt
	} {
		if Verify(encoded, "anything") {
			t.Errorf("Verify(%q, …) = true, want false", encoded)
		}
	}
}

// The iteration count travels inside the hash, which is what makes raising the
// cost later a one-line change rather than a migration.
func TestVerifyHonorsTheStoredIterationCount(t *testing.T) {
	original := iterationCount
	t.Cleanup(func() { iterationCount = original })

	SetIterationsForTesting(1000)
	cheap, err := Hash("a-password-that-is-long-enough")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.Contains(cheap, "i=1000$") {
		t.Fatalf("hash does not record its cost: %s", cheap)
	}

	SetIterationsForTesting(2000)
	if !Verify(cheap, "a-password-that-is-long-enough") {
		t.Error("a hash written at the old cost stopped verifying after the cost changed")
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		wantErr  error
	}{
		{"long enough", "a-password-that-is-long-enough", nil},
		{"exactly the minimum", strings.Repeat("x", MinLength), nil},
		{"one short", strings.Repeat("x", MinLength-1), ErrTooShort},
		{"empty", "", ErrTooShort},
		{"the published default", Default, ErrIsDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Validate(tc.in); got != tc.wantErr {
				t.Errorf("Validate(%q) = %v, want %v", tc.in, got, tc.wantErr)
			}
		})
	}
}

// The default must fail its own policy, or `enable --password toft` would
// quietly restore the state the forced change exists to end.
func TestDefaultIsNotAValidChoice(t *testing.T) {
	if Validate(Default) == nil {
		t.Error("the published default passes Validate")
	}
}

func TestGenerate(t *testing.T) {
	const ambiguous = "0O1lI"
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(pw) != 24 {
			t.Fatalf("length = %d, want 24", len(pw))
		}
		if err := Validate(pw); err != nil {
			t.Errorf("a generated password fails the policy: %v", err)
		}
		if strings.ContainsAny(pw, ambiguous) {
			t.Errorf("generated %q contains a character that is misread when retyped", pw)
		}
		if seen[pw] {
			t.Fatalf("Generate repeated itself: %q", pw)
		}
		seen[pw] = true
	}
}

// A failed lookup must cost what a wrong password costs, or the response time
// reveals whether an account exists.
func TestVerifyDummyDoesWork(t *testing.T) {
	if VerifyDummy("anything") {
		t.Error("VerifyDummy reported success")
	}
}
