package auth

import "testing"

// The jti is the session id the apps gateway forwards as X-Toft-Session:
// minted once per sign-in, so requests replaying one cookie share an id and a
// second login gets a fresh one. An empty or reused id would collapse two
// logins' scratch directories into one.
func TestEveryLoginMintsADistinctSessionID(t *testing.T) {
	svc := NewJWTService("test-secret")

	mint := func() string {
		t.Helper()
		sess, err := svc.Generate(GenerateParams{UserID: "u1", Login: "u1", Policy: DefaultSessionPolicy()})
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		claims, err := svc.Validate(sess.Token)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		return claims.ID
	}

	first, second := mint(), mint()
	if first == "" {
		t.Fatal("no session id was minted")
	}
	if first == second {
		t.Errorf("two logins share a session id: %q", first)
	}
}
