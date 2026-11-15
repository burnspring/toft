package auth

import (
	"strings"
	"testing"
	"time"
)

func runTokenService() *JWTService {
	return NewJWTService("a-test-secret-at-least-32-bytes-long!!")
}

// This package mints exactly one workload credential, and the audience is what
// keeps it from being anything else. The bootstrap credential is no longer
// minted here at all — it is a projected ServiceAccount token the cluster
// issues and the platform verifies — so an audience nobody signs is one nobody
// can present.
func TestOnlyTheRunAudienceIsMinted(t *testing.T) {
	j := runTokenService()
	p := RunTokenParams{RunID: "run-1", AppID: "app-1", WorkloadName: "demo-abc", TTL: time.Hour}

	run, err := j.MintRunToken(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.ValidateRunToken(run, AudienceRun); err != nil {
		t.Errorf("run token rejected by its own audience: %v", err)
	}
	// The audience the bootstrap route used to accept. Nothing mints it now,
	// and the route does not read JWTs at all — but a token claiming it must
	// still be worth nothing here.
	if _, err := j.ValidateRunToken(run, "toft-runner-bootstrap"); err == nil {
		t.Error("a run token validated for an audience nothing mints")
	}
}

// A runner credential must never satisfy the session validator. If it did, a
// token deliberately handed to app code would be a user session.
func TestRunTokensAreNotSessionTokens(t *testing.T) {
	j := runTokenService()

	run, _ := j.MintRunToken(RunTokenParams{RunID: "run-1", AppID: "app-1", TTL: time.Hour})
	if _, err := j.Validate(run); err == nil {
		t.Error("a run token was accepted as a user session")
	}

}

// And the reverse: a user's session must not be usable to report against a run
// or to fetch a workload's environment.
func TestSessionTokensAreNotRunTokens(t *testing.T) {
	j := runTokenService()

	session, err := j.Generate(GenerateParams{UserID: "alice", Login: "alice", SystemRole: "system_admin", Policy: DefaultSessionPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	for _, audience := range []string{AudienceRun} {
		if _, err := j.ValidateRunToken(session.Token, audience); err == nil {
			t.Errorf("a user session was accepted as a %s credential", audience)
		}
	}
}

// A token from a finished run is inert. The TTL is the run's timeout precisely
// so that a leaked token found later cannot be replayed.
func TestAnExpiredRunTokenIsInert(t *testing.T) {
	j := runTokenService()

	tok, err := j.MintRunToken(RunTokenParams{RunID: "run-1", AppID: "app-1", TTL: -time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.ValidateRunToken(tok, AudienceRun); err == nil {
		t.Fatal("an expired run token was accepted")
	}
}

func TestRunTokenCarriesItsIdentity(t *testing.T) {
	j := runTokenService()

	tok, _ := j.MintRunToken(RunTokenParams{
		RunID: "run-7", AppID: "app-1", WorkloadName: "demo-abc123", TTL: time.Hour,
	})
	claims, err := j.ValidateRunToken(tok, AudienceRun)
	if err != nil {
		t.Fatal(err)
	}
	if claims.RunID != "run-7" || claims.AppID != "app-1" || claims.WorkloadName != "demo-abc123" {
		t.Errorf("claims = %+v", claims)
	}
}

// A service workload has no run, but still ships logs and consumption — so an
// empty run ID is legitimate while an empty app is not.
func TestServiceWorkloadTokensNeedNoRunButDoNeedAnApp(t *testing.T) {
	j := runTokenService()

	tok, err := j.MintRunToken(RunTokenParams{AppID: "app-1", WorkloadName: "demo-abc", TTL: time.Hour})
	if err != nil {
		t.Fatalf("a service workload token should mint: %v", err)
	}
	claims, err := j.ValidateRunToken(tok, AudienceRun)
	if err != nil {
		t.Fatal(err)
	}
	if claims.RunID != "" {
		t.Errorf("run id = %q, want empty", claims.RunID)
	}

	if _, err := j.MintRunToken(RunTokenParams{RunID: "run-1", TTL: time.Hour}); err == nil {
		t.Error("a token with no app was minted; every report must be attributable")
	}
}

// A token signed with another secret must not validate — the guard against a
// different HS256 system sharing a key by accident.
func TestRunTokenRejectsAForeignSignature(t *testing.T) {
	mine := runTokenService()
	theirs := NewJWTService("a-completely-different-secret-32-bytes")

	tok, _ := theirs.MintRunToken(RunTokenParams{RunID: "run-1", AppID: "app-1", TTL: time.Hour})
	if _, err := mine.ValidateRunToken(tok, AudienceRun); err == nil {
		t.Error("a token signed with another secret was accepted")
	}
}

func TestRunTokenRejectsGarbage(t *testing.T) {
	j := runTokenService()
	for _, bad := range []string{"", "not.a.token", strings.Repeat("x", 200)} {
		if _, err := j.ValidateRunToken(bad, AudienceRun); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
