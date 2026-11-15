package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Run tokens are the only write path from inside a workload back to the
// platform, and they exist as a distinct token type rather than a reused
// session token for three reasons: they are scoped to one run, they expire
// when that run's deadline does rather than in eight hours, and their audience
// restricts them to callback endpoints so a leaked one cannot act as a user.
//
// There is one of them here, and there used to be two.
//
//	Run  reports progress, logs, consumption, and the structured result. This
//	     is the credential app code sees, and the only one this package mints
//	     for a workload.
//
// The other — the bootstrap credential that fetches the app's *decrypted
// environment* — is no longer the platform's to issue. It is a projected
// ServiceAccount token: issued by kubelet, bound to the pod, scoped to an
// audience the API server refuses, rotated in place, and verified by the
// platform with a TokenReview. The property that bought is the one no signing
// scheme here could: **an infrastructure-issued credential is rotated by the
// infrastructure, so nothing in an immutable pod spec has to expire** — which
// is what makes an in-place container restart possible at all.
//
// The narrowing the split existed for is unchanged. App code is *given* only
// the run token, so a leak's blast radius is still "this run's progress bar"
// rather than "this app's secrets". What changed is that the runner proves its
// entitlement to ask with something it was issued rather than something it was
// handed.
const (
	// AudienceRun is accepted by progress, log, consumption, and result
	// endpoints, and by nothing else.
	AudienceRun = "toft-runner-run"
	// AudienceHostBootstrap is what a host run presents to bootstrap, in place
	// of the projected ServiceAccount token a pod has (ADR §106). Accepted by
	// the bootstrap route and nothing else, and only once per run — the route
	// records the first use.
	AudienceHostBootstrap = "toft-bootstrap-host"
)

// HostBootstrapTTL bounds a host run's bootstrap credential. Minutes, because it
// is presented seconds after it is written; a single use is what actually
// closes it.
const HostBootstrapTTL = 5 * time.Minute

// MintHostBootstrapToken issues the credential the host backend writes into a
// run's own directory for its runner to present once.
func (j *JWTService) MintHostBootstrapToken(appID, runID, workloadName string) (string, error) {
	if appID == "" || runID == "" {
		return "", fmt.Errorf("a host bootstrap credential needs an app and a run")
	}
	return j.mintRunToken(RunTokenParams{AppID: appID, RunID: runID, WorkloadName: workloadName},
		AudienceHostBootstrap, HostBootstrapTTL)
}

// RunClaims identify one workload run.
type RunClaims struct {
	// RunID is a job run's ID. Empty for a long-lived service workload, which
	// has no run to report against but still ships logs and consumption.
	RunID string `json:"run_id,omitempty"`
	AppID string `json:"app_id"`
	// WorkloadName is the backend's name for this workload, so log and
	// consumption reports are attributable to one instance of one release.
	WorkloadName string `json:"workload_name"`
	// Host is set by bootstrap auth, never carried in a token: the caller
	// proved itself with a host credential rather than through the cluster,
	// so its release is on this machine rather than behind a presigned URL.
	Host bool `json:"-"`
	jwt.RegisteredClaims
}

// RunTokenParams describes a token to mint.
type RunTokenParams struct {
	RunID        string
	AppID        string
	WorkloadName string
	// TTL is how long the token is valid. For a run token this is the run's
	// timeout: a token from a finished run is then inert, so a leaked one
	// found later cannot be replayed.
	TTL time.Duration
}

// MintRunToken issues the credential app code may hold: progress, logs,
// consumption, and result reporting, and nothing else.
func (j *JWTService) MintRunToken(p RunTokenParams) (string, error) {
	if p.AppID == "" {
		return "", fmt.Errorf("a run token needs an app")
	}
	// Only an *unset* TTL defaults. A negative one means the run's deadline
	// has already passed, and the honest answer is a token that is already
	// expired — normalising it into a fresh hour would hand a stale run a
	// working credential, which is the opposite of tying the token's life to
	// the run's.
	ttl := p.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	return j.mintRunToken(p, AudienceRun, ttl)
}

func (j *JWTService) mintRunToken(p RunTokenParams, audience string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := &RunClaims{
		RunID:        p.RunID,
		AppID:        p.AppID,
		WorkloadName: p.WorkloadName,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    jwtIssuer,
			// Deliberately NOT jwtAudience: a run token must never satisfy the
			// session validator, and a session token must never satisfy this
			// one. The audience is what keeps the two populations disjoint.
			Audience: jwt.ClaimStrings{audience},
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(j.secret)
}

// ValidateRunToken parses a runner credential and requires the given audience.
//
// Passing the audience in — rather than accepting either — is what stops the
// progress endpoint quietly accepting a bootstrap token, and the bootstrap
// endpoint accepting the token app code holds. Widening this is how the
// credential narrowing gets undone.
func (j *JWTService) ValidateRunToken(tokenStr, audience string) (*RunClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &RunClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return j.secret, nil
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(audience),
	)
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*RunClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid run token")
	}
	if claims.AppID == "" {
		return nil, fmt.Errorf("run token carries no app")
	}
	return claims, nil
}
