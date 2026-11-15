package auth

import (
	"testing"
	"time"
)

func TestASigninDeviceNamesOneAccountAndExpires(t *testing.T) {
	j := NewJWTService("a-test-secret-that-is-long-enough-for-hs256")
	now := time.Now()
	set := now.Add(-time.Hour)
	tok := j.MintSigninDevice("user-1", set, now)

	if id := j.VerifySigninDevice(tok, "user-1", set, now); id == "" {
		t.Fatal("a fresh token for its own account did not verify")
	}
	if id := j.VerifySigninDevice(tok, "user-2", set, now); id != "" {
		t.Error("a token verified for another account")
	}
	if id := j.VerifySigninDevice(tok, "user-1", set, now.Add(SigninDeviceTTL+time.Minute)); id != "" {
		t.Error("an expired token verified")
	}
	if id := j.VerifySigninDevice(tok+"x", "user-1", set, now); id != "" {
		t.Error("a tampered token verified")
	}
	other := NewJWTService("another-secret-that-is-also-long-enough-xx")
	if id := other.VerifySigninDevice(tok, "user-1", set, now); id != "" {
		t.Error("a token verified under another install's secret")
	}
	if a, b := j.MintSigninDevice("user-1", set, now), j.MintSigninDevice("user-1", set, now); j.VerifySigninDevice(a, "user-1", set, now) == j.VerifySigninDevice(b, "user-1", set, now) {
		t.Error("two browsers share one device id")
	}
}

// A token is bound to the password it proved: once the password is set again,
// the token verifies as "" (security review 2026-11-14, SI-9).
func TestASigninDeviceDiesWithItsPassword(t *testing.T) {
	j := NewJWTService("a-test-secret-that-is-long-enough-for-hs256")
	now := time.Now()
	a := now.Add(-time.Hour)
	tok := j.MintSigninDevice("user-1", a, now)
	if j.VerifySigninDevice(tok, "user-1", a, now) == "" {
		t.Fatal("a token did not verify under its own password stamp")
	}
	if j.VerifySigninDevice(tok, "user-1", now, now) != "" {
		t.Error("a token verified under another password stamp")
	}
}
