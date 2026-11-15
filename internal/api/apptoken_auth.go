package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"toft/internal/auth"

	"golang.org/x/time/rate"

	"toft/internal/database"
)

// appTokenSecretPrefix identifies an app's outbound credential.
//
// Deliberately not a prefix of retiredAppKeyPrefix ("toft_") or of
// userKeySecretPrefix ("toftu_"): "toft_" requires an underscore in the fifth
// position, which "tofta_" does not have, so no prefix test anywhere can mistake
// one class for another. That matters more here than elsewhere, because the
// credentials point in different directions — an toftu_ key is an account and an
// tofta_ token is an app calling out. A route reachable by one must never be
// reachable by the other.
const appTokenSecretPrefix = "tofta_"

// The app contract this build speaks. A mismatch is a named refusal rather than
// best-effort tolerance, the same reasoning as runner.CheckVersion: a partial
// upgrade must fail loudly rather than half-work in ways nobody traces back to
// a version skew.
const appContractVersion = "1"

const appContractHeader = "X-Toft-App-Contract"

// Per-token rate limit. Transport protection, not an email policy: it does not
// care who the recipients are, does not accumulate across a day, and resets in
// sixty seconds. The decision that there is no send *quota* stands — the ledger
// is the control there.
const (
	appTokenRatePerMinute = 60
	appTokenBurst         = 10
)

// The store routes have a bucket of their own. One URL per object is the design
// — a tree is a walk over the one-object routes — and sixty objects a minute,
// shared with an app's notifications, would push an app's authors towards an
// object-store credential in its environment, which is what signing exists to
// prevent. Not unlimited: every mint is an audit row.
const (
	appStoreRatePerMinute = 600
	appStoreBurst         = 100
	appStorePathPrefix    = "/api/v1/app/store/"
)

// Reading the app's own runs has a bucket of its own too, at the store's rate,
// and for the same kind of reason: wait() is a poll, and an app with a few
// waits open spent the sixty a minute between them — then every start, data
// read and notify the app made was refused rate_limited for as long as they
// polled (jobs-regression #14). A read of a run row is cheap and writes
// nothing; starts stay on the main bucket.
const (
	appRunsRatePerMinute = 600
	appRunsBurst         = 100
	appRunsPathPrefix    = "/api/v1/app/job-runs"
)

// Token requests have a bucket of their own, low because every one writes a
// ledger row and a well-behaved SDK asks once an hour per kind; apart from the
// main bucket so a loop fetching tokens cannot starve the app's notifications.
const (
	appTokensRatePerMinute = 30
	appTokensBurst         = 10
	appTokensPathPrefix    = "/api/v1/app/tokens/"
)

type appTokenCtxKey struct{}

// appTokenFromContext returns the app this request is authenticated as, or nil.
func appTokenFromContext(ctx context.Context) *database.ResolvedAppToken {
	v, _ := ctx.Value(appTokenCtxKey{}).(*database.ResolvedAppToken)
	return v
}

// appTokenFromRequest extracts a presented app token, or "".
func appTokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer "+appTokenSecretPrefix) {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if k := r.Header.Get("X-API-Key"); strings.HasPrefix(k, appTokenSecretPrefix) {
		return k
	}
	return ""
}

// appTokenAuth authenticates a workload on /api/v1/app/*.
//
// It is the only way into this subtree. A session cookie and a personal key
// are both refused — not because they are invalid credentials, but
// because they are the wrong *direction*: this surface exists for an app to act
// as itself, and a route that also accepted a person's session would sooner or
// later be given a handler that assumed one.
//
// No handler behind this calls auth.Authorize, which is a different rule from
// the rest of the codebase and is stated here rather than left to be noticed.
// It is safe only because no route in the subtree takes an app id: the token is
// the app, so there is no id to confuse with somebody else's.
func (s *Server) appTokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := appTokenFromRequest(r)
		if secret == "" {
			// A job run's own credential, on the one part of this surface a
			// job may reach: sending (ADR §108). Tried only when no app token
			// was presented, and refused by name everywhere else.
			if resolved, handled := s.jobSendAuth(w, r); handled {
				if resolved != nil {
					ctx := context.WithValue(r.Context(), appTokenCtxKey{}, resolved)
					next.ServeHTTP(w, r.WithContext(ctx))
				}
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "this endpoint is for a deployed app, authenticated with the app " +
					"token Toft injects as TOFT_APP_TOKEN",
				"code": "no_app_token",
				// A person's key reaches here through a URL an SDK built for the
				// app (logs_url, fifth 2026-11-03 black-box list).
				"hint": "call it from the app itself, which has TOFT_APP_TOKEN — a person's key reaches the same answer through the /api/v1/apps/<app> routes (toft logs, toft job runs)"})
			return
		}
		if v := r.Header.Get(appContractHeader); v != "" && v != appContractVersion {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "this Toft speaks app contract " + appContractVersion +
					" and the SDK in this app speaks " + v +
					" — redeploy the app to pick up a matching SDK",
				"code": "contract_mismatch", "hint": "redeploy the app — toft redeploy — so it builds with the SDK this install ships"})
			return
		}

		resolved, err := s.db.ResolveAppToken(hashAPIKey(secret))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if resolved == nil {
			// Flat, with no detail. Unlike a disabled API key — whose holder
			// already has the key and learns nothing from being told — a token
			// that resolves to nothing may well be someone guessing.
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "invalid app token", "code": "invalid_app_token", "hint": "send the TOFT_APP_TOKEN this install gave the running app, as Authorization: Bearer <token>"})
			return
		}
		if resolved.AppStatus == database.AppStatusStopped {
			// Named, because this one is fixable and the app's authors can fix
			// it. `sleeping` deliberately passes: a request can wake a slept app
			// and the very next thing it does may be to send.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this app is stopped — a stopped app does nothing, including notify",
				"code":  "app_stopped", "hint": "start the app with toft start, then send again"})
			return
		}

		if resolved.AppLocked {
			// The same answer jobSendAuth gives a locked app's run.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this app is locked", "code": "app_locked"})
			return
		}

		bucket, perMinute, burst := resolved.Token.ID, appTokenRatePerMinute, appTokenBurst
		if strings.HasPrefix(r.URL.Path, appStorePathPrefix) {
			bucket, perMinute, burst = "store:"+resolved.Token.ID, appStoreRatePerMinute, appStoreBurst
		} else if strings.HasPrefix(r.URL.Path, appTokensPathPrefix) {
			bucket, perMinute, burst = "tokens:"+resolved.Token.ID, appTokensRatePerMinute, appTokensBurst
		} else if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, appRunsPathPrefix) {
			bucket, perMinute, burst = "runs:"+resolved.Token.ID, appRunsRatePerMinute, appRunsBurst
		}
		if !s.allowAppTokenAt(bucket, perMinute, burst) {
			w.Header().Set("Retry-After", retryAfterFor(perMinute))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "this app is calling Toft faster than " +
					strconv.Itoa(perMinute) + " times a minute",
				"code": "rate_limited"})
			return
		}

		s.touchAppToken(resolved.Token.ID)
		ctx := context.WithValue(r.Context(), appTokenCtxKey{}, resolved)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// allowAppToken is the per-token bucket. In memory and node-local, like every
// other counter on a request path here.
func (s *Server) allowAppToken(id string) bool {
	return s.allowAppTokenAt(id, appTokenRatePerMinute, appTokenBurst)
}

// retryAfterFor is how long until a bucket refilling perMinute tokens has one
// again, in whole seconds and never less than one.
//
// **Not the minute the rate is stated in.** The SDKs honour Retry-After, and
// the answer used to be a flat 60: an app that spent its burst of ten in one
// page view -- `/who` reading identity, role and capabilities does -- slept a
// full minute on its next call while one token was a second away, and the
// visitor got the gateway's 60-second timeout instead of a page.
func retryAfterFor(perMinute int) string {
	if perMinute <= 0 {
		return "60"
	}
	secs := (60 + perMinute - 1) / perMinute
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}

// allowAppTokenAt is a bucket of a given size. The key names the bucket, so a
// token's store calls and its other calls never share one.
func (s *Server) allowAppTokenAt(key string, perMinute, burst int) bool {
	s.sweepAppTokenState(time.Now())
	v, _ := s.appTokenLimiters.LoadOrStore(key,
		rate.NewLimiter(rate.Limit(float64(perMinute)/60.0), burst))
	limiter, ok := v.(*rate.Limiter)
	if !ok {
		return true
	}
	return limiter.Allow()
}

// appTokenSweepEvery is how often the per-token maps are swept.
const appTokenSweepEvery = time.Minute

// sweepAppTokenState drops what no longer constrains anything, at most once a
// minute. The keys are a token per deploy and a bucket per job run, so without
// it both maps grew for the life of the process. A limiter that has refilled to
// its burst is indistinguishable from the fresh one LoadOrStore would make,
// and a touch older than the minute it throttles for throttles nothing.
func (s *Server) sweepAppTokenState(now time.Time) {
	last := s.appTokenSwept.Load()
	if now.UnixNano()-last < int64(appTokenSweepEvery) ||
		!s.appTokenSwept.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	s.appTokenLimiters.Range(func(k, v any) bool {
		if l, ok := v.(*rate.Limiter); ok && l.TokensAt(now) >= float64(l.Burst()) {
			s.appTokenLimiters.Delete(k)
		}
		return true
	})
	s.appTokenTouched.Range(func(k, v any) bool {
		if t, ok := v.(time.Time); ok && now.Sub(t) >= time.Minute {
			s.appTokenTouched.Delete(k)
		}
		return true
	})
}

// touchAppToken records last-use, throttled per token so an app's notification
// does not add a database write to every call. The same budget and the same
// idiom as touchUserAPIKey.
func (s *Server) touchAppToken(id string) {
	now := time.Now()
	if last, ok := s.appTokenTouched.Load(id); ok {
		if t, ok := last.(time.Time); ok && now.Sub(t) < time.Minute {
			return
		}
	}
	s.appTokenTouched.Store(id, now)
	go func() {
		if err := s.db.TouchAppToken(id); err != nil {
			// Nothing depends on this: it is an operator convenience, and
			// failing the app's call over it would be absurd.
			_ = err
		}
	}()
}

// jobSendPaths is the part of /api/v1/app a job run may reach with its run
// token (ADR §108): sending a message or a delivery through the app's attached
// integrations, reading back what became of one, getting a file connection's
// access token where the connection allows it, and **the stores its app
// maps**, at the mappings' own levels. The list is the specification — not
// data, not starting jobs, not identity.
//
// The stores are here because a job holds its app's storage everywhere else:
// on a cluster the run's pod mounts the same mappings. Where nothing is
// mounted — ECS, the one-box bundle — the signed-URL surface is the only way
// to a store, and without it a nightly job could not read or write one at all.
//
// **And the app's own address** (`/api/v1/app/url`), because a job's report
// email is the commonest place a link to the app is written, and a link built
// from a request does not exist in a job. It is the one read here, and it
// reads only what every message the app sends already carries.
var jobSendPaths = []string{"/api/v1/app/notify/", "/api/v1/app/deliver/", "/api/v1/app/sends/",
	"/api/v1/app/store/", appTokensPathPrefix, appURLPath}

func isJobSendPath(path string) bool {
	for _, p := range jobSendPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// jobSendOnlyWhere names the run a job_send_only refusal is about — "job
// "refresh", run 1a2b3c4d" — from the run the token names. The job's name is
// not in the token, so it is read; a run that cannot be read is named by its ID
// alone rather than failing the refusal.
func (s *Server) jobSendOnlyWhere(runID string) string {
	if run, err := s.db.GetJobRun(runID); err == nil && run != nil && run.JobName != "" {
		return fmt.Sprintf("job %q, run %s", run.JobName, shortID(runID))
	}
	return "run " + shortID(runID)
}

// writeJobSendOnly refuses a job's run token on a route that is not one of the
// send routes. It is a deliberate rule, not a broken deploy, and the body says
// so: it names the job and the run, and the hint gives the two ways round it,
// in words every SDK's spelling of the run output fits (drills, 2026-10-28).
func writeJobSendOnly(w http.ResponseWriter, where string) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "this is a job (" + where + "), and a job is deliberately refused this — it is not a " +
			"broken deploy. A job may send through its app's integrations, get a file connection's " +
			"access token, and reach the stores its app maps, and do nothing else on this surface — " +
			"app data, identity and starting jobs belong to the app's own process",
		"code":      "job_send_only",
		"retryable": false,
		"hint": "do this from the app's service process, or carry the result out of the run " +
			"with the SDK's write-output-JSON call (run.write_output_json, run.writeOutputJson, " +
			"Run.WriteOutputJSON, run_write_output) and let an endpoint fold it in"})
}

// jobSendAuth resolves a job run's credential into the same shape an app token
// resolves to, so every send handler — and the integration gate, the ledger and
// the idempotency window behind it — treats a job's send exactly as its app's.
//
// It answers handled=false when the request carries no run token at all, so the
// caller's own refusal stands. With one, it either resolves (handled, resolved)
// or has already written the refusal (handled, nil).
//
// **A job holds no app token, and still holds none** (ADR §27 D1's half that
// stands): what it presents is its run token, bounded by the run's own
// deadline, and accepted here only while the run is in flight. A token from a
// finished run opens nothing.
func (s *Server) jobSendAuth(w http.ResponseWriter, r *http.Request) (*database.ResolvedAppToken, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") || s.jwt == nil {
		return nil, false
	}
	claims, err := s.jwt.ValidateRunToken(strings.TrimPrefix(h, "Bearer "), auth.AudienceRun)
	if err != nil || claims.RunID == "" {
		return nil, false
	}
	if !isJobSendPath(r.URL.Path) {
		writeJobSendOnly(w, s.jobSendOnlyWhere(claims.RunID))
		return nil, true
	}
	run, err := s.db.GetJobRun(claims.RunID)
	if err != nil || run == nil || run.AppID != claims.AppID ||
		(run.Status != database.JobRunRunning && run.Status != database.JobRunClaimed) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "this run is not in flight, so its credential sends nothing",
			"code":  "run_not_in_flight", "hint": "send only while the run is running; start a new run to send again"})
		return nil, true
	}
	app, err := s.db.GetApp(claims.AppID)
	if err != nil || app == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid run token"})
		return nil, true
	}
	if app.Locked() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this app is locked", "code": "app_locked"})
		return nil, true
	}
	bucket := "run:" + run.ID
	if !s.allowAppToken(bucket) {
		w.Header().Set("Retry-After", retryAfterFor(appTokenRatePerMinute))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "this run is calling Toft faster than " +
				strconv.Itoa(appTokenRatePerMinute) + " times a minute",
			"code": "rate_limited"})
		return nil, true
	}
	return &database.ResolvedAppToken{
		Token:     database.AppToken{ID: bucket, AppID: app.ID, Workload: run.ID},
		AppSlug:   app.Slug,
		AppName:   app.Name,
		AppStatus: app.Status,
		OwnerID:   app.OwnerID,
	}, true
}
