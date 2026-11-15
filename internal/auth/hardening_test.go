package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Only explicitly no-object actions may pass with obj == nil. A blanket
// allow made Authorize(p, ActionDelete, nil, …) succeed for any authenticated
// principal. Task 1.9.
func TestAuthorizeNilObjectAllowlist(t *testing.T) {
	admin := &Principal{Type: "user", ID: "u1", SystemRole: "publisher"}

	if d := Authorize(admin, ActionPublish, nil, nil); !d.Allow {
		t.Errorf("publish with nil object should be allowed for a publisher: %+v", d)
	}
	for _, action := range []Action{ActionDelete, ActionEditContent, ActionManageGrants, ActionTransferOwnership} {
		if d := Authorize(admin, action, nil, nil); d.Allow {
			t.Errorf("%s with nil object must be denied, got allow", action)
		}
	}
}

// Tokens must carry exp, iss and aud; hand-rolled HS256 tokens without them
// (or from another issuer sharing the secret) are rejected.
func TestValidateRequiresExpIssuerAudience(t *testing.T) {
	svc := NewJWTService("test-secret-key-32-bytes-long!!!")

	sign := func(claims *Claims) string {
		t.Helper()
		tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-secret-key-32-bytes-long!!!"))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	full := func() *Claims {
		return &Claims{
			UserID: "u1",
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
				Issuer:    jwtIssuer,
				Audience:  jwt.ClaimStrings{jwtAudience},
			},
		}
	}

	if _, err := svc.Validate(sign(full())); err != nil {
		t.Fatalf("complete token rejected: %v", err)
	}

	c := full()
	c.ExpiresAt = nil
	if _, err := svc.Validate(sign(c)); err == nil {
		t.Error("token without exp was accepted")
	}

	c = full()
	c.Issuer = "some-other-service"
	if _, err := svc.Validate(sign(c)); err == nil {
		t.Error("token from a foreign issuer was accepted")
	}

	c = full()
	c.Audience = nil
	if _, err := svc.Validate(sign(c)); err == nil {
		t.Error("token without audience was accepted")
	}
}
