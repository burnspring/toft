// Package password hashes and verifies the only password Toft stores: the
// built-in local administrator's.
//
// Everybody else authenticates at an identity provider, which is the point —
// this exists so that an install with no provider configured yet still has one
// door, and so that a locked-out install has a way back in. One password, one
// account, one purpose.
//
// # Why PBKDF2 and not argon2
//
// PBKDF2-HMAC-SHA512 at 210,000 iterations is an OWASP-recommended
// configuration and it is in the standard library (`crypto/pbkdf2`, Go 1.24).
// argon2id is the better primitive on paper, and it would mean adding a
// dependency to hash exactly one secret. For a credential that exists to be
// replaced by SSO, the stdlib answer is the right trade.
//
// SHA-512 rather than SHA-256 for a second reason: the recommended cost for
// SHA-512 (210,000) takes about half as long as the recommended cost for
// SHA-256 (600,000), and every wrong password costs the server that time. A
// verification too expensive to perform is a way to attack the platform with
// its own login form.
//
// # The encoded form
//
// Hashes are stored in a PHC-style string:
//
//	$pbkdf2-sha512$i=210000$<base64 salt>$<base64 hash>
//
// The parameters travel with the hash, so raising the iteration count later
// still verifies every password stored before the change. Nothing outside this
// package parses that string.
package password

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

const (
	// Default is the published password the built-in administrator ships with.
	// It is documented, not secret, and it is why the account is created with
	// must_change set: signing in with this string cannot mint a session, only
	// the opportunity to replace it.
	Default = "toft"

	// MinLength is the floor for a password someone chooses. Eight characters
	// with no composition rules is NIST SP 800-63B's minimum for a
	// user-chosen secret — length is what resists guessing, and character-class
	// requirements mostly produce `Password1!`.
	//
	// It is a floor and not the advice: Generate still produces 24 characters,
	// which is what every install starts with and what the CLI hands out.
	MinLength = 8

	// iterations, saltLen and keyLen are the current cost parameters. Raising
	// iterations is safe: existing hashes carry their own.
	iterations = 210_000
	saltLen    = 16
	keyLen     = 64

	algorithm = "pbkdf2-sha512"
)

// iterationCount is the cost new hashes are written at.
//
// A variable rather than a constant for exactly one reason: every test database
// bootstraps the built-in administrator, and at production cost that is a
// quarter of a second added to each one. internal/database/dbtest lowers it.
// Doing so is safe in both directions, because the count travels inside the
// encoded hash — a hash written at test cost still verifies afterwards, and one
// written at production cost is unaffected.
var iterationCount = iterations

// SetIterationsForTesting lowers the hashing cost. Called from dbtest, which is
// imported only by tests. No production path calls it, and the name is what
// keeps that true.
func SetIterationsForTesting(n int) {
	if n > 0 {
		iterationCount = n
	}
}

// ErrTooShort and friends are the reasons Validate refuses, kept as values so
// the API layer can pass the sentence straight to the person typing.
var (
	ErrTooShort  = fmt.Errorf("a password must be at least %d characters", MinLength)
	ErrIsDefault = errors.New("that is the published default password — choose something else")
)

// Hash derives a new hash with a fresh random salt.
func Hash(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	key, err := pbkdf2.Key(sha512.New, plain, salt, iterationCount, keyLen)
	if err != nil {
		return "", fmt.Errorf("deriving key: %w", err)
	}
	return fmt.Sprintf("$%s$i=%d$%s$%s", algorithm, iterationCount,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether plain produces the stored hash.
//
// It fails closed and silently: a malformed, empty or unknown-algorithm stored
// value is a failed verification, never an accepted one and never an error the
// caller might mistake for success. The comparison is constant-time.
func Verify(encoded, plain string) bool {
	parts := strings.Split(encoded, "$")
	// Leading "$" produces an empty first field: ["", alg, "i=N", salt, hash].
	if len(parts) != 5 || parts[0] != "" || parts[1] != algorithm {
		return false
	}
	iter, err := strconv.Atoi(strings.TrimPrefix(parts[2], "i="))
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha512.New, plain, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// VerifyDummy performs the same work as Verify against a throwaway hash.
//
// Called when no account matched, so that "no such user" and "wrong password"
// cost the same. Without it, the response time answers a question the generic
// error message is there to refuse — whether an account exists.
func VerifyDummy(plain string) bool {
	return Verify(dummyHash(), plain)
}

// dummyHash is a valid hash of a value nothing knows, used only to burn the
// same CPU a real verification would. Computed on first use rather than at init,
// so a server that never sees a failed login never pays for it.
var dummyHash = sync.OnceValue(func() string {
	h, err := Hash("this string authenticates nothing")
	if err != nil {
		// Only reachable if the system CSPRNG fails, in which case nothing else
		// here works either. An empty hash still fails closed in Verify.
		return ""
	}
	return h
})

// Validate applies the policy to a password someone is choosing. The refusals
// are sentences because they are shown to the person typing, who can act on
// "at least 8 characters" and cannot act on "invalid password".
func Validate(plain string) error {
	// The default is the *only* thing refusing `toft`. At the old floor of
	// twelve it failed the length rule too, and this check was the better of two
	// refusals; at eight it is nine characters and passes, so ErrIsDefault is
	// load-bearing and TestDefaultIsNotAValidChoice is what keeps it that way.
	// Someone retyping the published password has made a specific mistake, and
	// "that is the published default" tells them what it was.
	if plain == Default {
		return ErrIsDefault
	}
	if len(plain) < MinLength {
		return ErrTooShort
	}
	return nil
}

// Generate returns a random password for the CLI to print once.
//
// The alphabet omits characters that are misread when someone copies a password
// out of a terminal by hand — 0/O, 1/l/I — because this string is going to be
// read aloud or retyped at least once.
func Generate() (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const length = 24
	// Rejection sampling rather than `% len(alphabet)`: 256 is not a multiple of
	// 57, so the modulo would quietly favor the first few letters.
	limit := byte(256 / len(alphabet) * len(alphabet))
	out := make([]byte, 0, length)
	buf := make([]byte, length)
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generating password: %w", err)
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == length {
				break
			}
		}
	}
	return string(out), nil
}
