package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// The sign-in device: what lets a browser that has signed in as an account
// before keep signing in while somebody else is failing at it.
//
// Local sign-in locks an account after ten wrong passwords, and that lock is
// per account — so anyone who can reach the login form can keep the built-in
// administrator, the account that exists to recover the install, locked by
// failing ten times every fifteen minutes. Counting per address instead is what
// ADR §75 refused: behind a load balancer the address is everybody's, and in
// front of a NAT an office and an attacker are the same caller.
//
// So the thing counted is the *browser*. A successful sign-in leaves a signed
// token naming the account, and a request that presents one for the account it
// is signing in as is exempt from the account's lock and throttled on a counter
// of its own. Nobody failing from elsewhere can touch that counter, because they
// do not hold the token, and the token grants nothing — the password is still
// checked every time.
//
// It is also bound to the password it proved: the stamp of when that password
// was set is signed in, so changing the password ends every remembered browser
// at once. Without it a token collected while the old password was known kept
// buying ten guesses at the new one, outside the account's lockout.

// SigninDeviceTTL is how long a browser is remembered. Long, because the case
// it serves is the administrator who signs in rarely and is locked out the day
// it matters.
const SigninDeviceTTL = 90 * 24 * time.Hour

// d2 added the password stamp. A d1 token verifies as "" — the browser is
// simply not remembered until its next correct password.
const signinDeviceVersion = "d2"

// passwordStamp is how a password's set time is written into a token.
func passwordStamp(setAt time.Time) string {
	return strconv.FormatInt(setAt.UTC().UnixMicro(), 10)
}

// MintSigninDevice issues a device token for one account, bound to the
// password set at passwordSetAt.
func (j *JWTService) MintSigninDevice(userID string, passwordSetAt, now time.Time) string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	payload := strings.Join([]string{
		signinDeviceVersion,
		base64.RawURLEncoding.EncodeToString([]byte(userID)),
		strconv.FormatInt(now.Add(SigninDeviceTTL).Unix(), 10),
		base64.RawURLEncoding.EncodeToString(nonce),
		passwordStamp(passwordSetAt),
	}, ".")
	return payload + "." + j.signinDeviceSignature(payload)
}

// VerifySigninDevice returns the device's id when token is a live device token
// for userID, and "" otherwise. The id is what a per-device counter is keyed
// on; it is the nonce, which names one browser and nothing else. A token for a
// password other than the one set at passwordSetAt verifies as "".
func (j *JWTService) VerifySigninDevice(token, userID string, passwordSetAt, now time.Time) string {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return ""
	}
	payload, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(j.signinDeviceSignature(payload))) {
		return ""
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 5 || parts[0] != signinDeviceVersion {
		return ""
	}
	who, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || string(who) != userID {
		return ""
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || now.Unix() >= exp {
		return ""
	}
	if parts[4] != passwordStamp(passwordSetAt) {
		return ""
	}
	return parts[3]
}

// signinDeviceSignature domain-separates from every other use of the secret.
func (j *JWTService) signinDeviceSignature(payload string) string {
	mac := hmac.New(sha256.New, j.secret)
	mac.Write([]byte("toft-signin-device\x00"))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
