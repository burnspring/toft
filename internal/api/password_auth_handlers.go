package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"toft/internal/database"
	"toft/internal/password"
	"toft/internal/telemetry"
)

// Local sign-in: the built-in administrator's username and password.
//
// This is the one credential the platform holds itself, and it exists for two
// moments — the install that has no identity provider configured yet, and the
// install whose provider has broken. Everything about it is shaped by the fact
// that the default password is published:
//
//   - While `must_change` is set, verifying the password mints **no session**.
//     It buys exactly one thing: the right to replace itself. So knowing the
//     documented default cannot get anyone in quietly — it can only take the
//     account away from the operator, loudly, in the audit log.
//   - The provider can be switched off from the admin UI and cannot be switched
//     back on from it. Re-enabling is `toft local-admin enable`, which
//     needs a shell on the box. A door that a compromised admin session can
//     reopen is not closed.
//   - Disabled means absent: these routes answer 404, the login page never
//     mentions it, and nothing hints that a password form is one setting away.

// passwordCredentials is the login shape. `username` accepts the login or the
// email address, because people do not reliably know which one they were given.
type passwordCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// NewPassword is present only on the change request.
	NewPassword string `json:"new_password"`
	// Redirect is where the page wants to land after sign-in. It is validated
	// here, by the same rule the provider callbacks use, and the validated value
	// is what the response carries — the browser never navigates to the raw one.
	Redirect string `json:"redirect"`
}

// genericLoginFailure is the only thing a failed attempt is ever told.
//
// Not "no such user" and not "wrong password": either one turns the login form
// into an oracle for which accounts exist, and this install's usernames are
// people's work identities.
const genericLoginFailure = "incorrect username or password"

// localProviderEnabled resolves the local provider and reports whether it is
// offered at all.
//
// The error case and the disabled case are the same answer on purpose — a
// caller cannot distinguish "switched off" from "never existed", which is what
// makes the capability absent rather than dormant.
func (s *Server) localProviderEnabled() bool {
	p, err := s.authProvider(database.LocalProviderID)
	if err != nil {
		if !errors.Is(err, errNoSuchProvider) {
			log.Printf("local sign-in: reading the provider: %v", err)
		}
		return false
	}
	return p.Enabled
}

// handlePasswordLogin verifies a password and either mints a session or, when
// the password must be changed first, says so and mints nothing.
func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	if !s.localProviderEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "sign in through this install's single sign-on instead; an administrator can switch local sign-in back on"})
		return
	}

	var body passwordCredentials
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	user, cred := s.verifyPassword(w, r, body.Username, body.Password)
	if user == nil {
		return
	}

	// The published default gets no session. The person proved they have the
	// documentation, which is not the same as being the administrator.
	if cred.MustChange {
		writeJSON(w, http.StatusOK, map[string]any{
			"must_change_password": true,
			"reason":               "This account still has its initial password. Choose a new one to finish signing in.",
		})
		// A distinct outcome rather than a success, because it is one: the
		// password verified and nothing was minted but the right to replace it.
		// A graph that counted this as a sign-in would say an install using its
		// published default password was signing people in fine.
		telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.SigninMustChange)
		// Audited as a failure for the same reason it is a distinct metric
		// outcome: nothing was minted. A `session.signin` row here would put
		// "the administrator signed in" in the ledger for an install whose
		// password is still the published default.
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, UserID: user.ID,
			Provider: database.ProviderKindPassword, Reason: telemetry.SigninMustChange,
		})
		return
	}

	telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.OutcomeSucceeded)
	s.issuePasswordSession(w, r, user, body.Redirect)
}

// handlePasswordChange replaces a password, given the current one.
//
// Unauthenticated by design: the first thing that ever happens on a new install
// is this request, and there is no session yet to authenticate it. The current
// password is the authentication, which is also why the same endpoint serves
// voluntary rotation later.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if !s.localProviderEnabled() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "change the password through this install's single sign-on provider; an administrator can switch local sign-in back on"})
		return
	}

	var body passwordCredentials
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	user, _ := s.verifyPassword(w, r, body.Username, body.Password)
	if user == nil {
		return
	}

	if err := password.Validate(body.NewPassword); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "hint": "choose a password that meets the rule the sentence names"})
		return
	}
	if body.NewPassword == body.Password {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "the new password must be different from the current one", "hint": "choose a different password and send it as new_password"})
		return
	}

	hash, err := password.Hash(body.NewPassword)
	if err != nil {
		log.Printf("local sign-in: hashing a new password: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Audited as the account acting on itself: nobody else was involved, and the
	// alternative — an empty actor — reads in the log like a change that happened
	// on its own.
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = user.ID
	if err := s.db.SetPassword(user.ID, hash, false, audit); err != nil {
		log.Printf("local sign-in: storing a new password for %s: %v", user.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	log.Printf("local sign-in: password changed for %s", user.ID)

	// The device token verifyPassword left is bound to the old password and
	// now verifies as nothing; this browser just proved the new one.
	if fresh, err := s.db.GetPasswordCredential(user.ID); err == nil && fresh != nil {
		s.rememberSigninDevice(w, r, user.ID, fresh.UpdatedAt)
	}

	// SetPassword bumped the token version, revoking every session minted with
	// the old password. The one issued here is minted after that bump.
	s.issuePasswordSession(w, r, user, body.Redirect)
}

// verifyPassword is the shared front half of both handlers: find the account,
// apply the lockout, check the password, apply the account lock.
//
// It writes the response and returns nil on every refusal, so a caller that
// forgets to check gets a nil dereference rather than an authenticated request.
func (s *Server) verifyPassword(w http.ResponseWriter, r *http.Request, username, plain string) (*database.User, *database.PasswordCredential) {
	if username == "" || plain == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a username and password are required", "hint": "send both, like {\"username\": \"admin\", \"password\": \"...\"}"})
		return nil, nil
	}

	user, cred, err := s.db.FindPasswordUser(username)
	if err != nil {
		log.Printf("local sign-in: looking up %q: %v", username, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, nil
	}
	if user == nil || cred == nil {
		// Hash anyway. Otherwise the response time distinguishes a real account
		// from an invented one, and the generic message above would be the only
		// thing not leaking which is which.
		password.VerifyDummy(plain)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": genericLoginFailure})
		telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.SigninNoAccount)
		// The one row with no actor. What was typed is recorded because "four
		// hundred attempts as `root` from one address" is the sentence this
		// table exists to be able to say — and it is not an oracle: the audit
		// log is administrators only, while the *response* stays generic.
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, Provider: database.ProviderKindPassword,
			Reason: telemetry.SigninNoAccount, Attempted: username,
		})
		return nil, nil
	}

	now := time.Now().UTC()
	// A browser that has signed in as this account before is held to its own
	// count rather than the account's, so failures from anywhere else cannot
	// lock it out. See auth/signindevice.go.
	device := s.signinDevice(r, user.ID, cred.UpdatedAt)
	if device != "" {
		if until, ok := s.signinDevices.locked(device, now); ok {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":       "too many failed attempts — try again later",
				"retry_after": int(until.Sub(now).Minutes()) + 1,
			})
			telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.SigninThrottled)
			s.recordSignin(r, signinAudit{
				Action: ActionSigninFailed, UserID: user.ID,
				Provider: database.ProviderKindPassword, Reason: telemetry.SigninThrottled,
			})
			return nil, nil
		}
	} else if cred.Throttled(now) {
		// Told plainly, including when to come back: this is the administrator
		// who mistyped their own password, far more often than it is an attacker,
		// and a bare failure would send them to the CLI for nothing.
		wait := int(cred.LockedUntil.Sub(now).Minutes()) + 1
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":       "too many failed attempts — try again later",
			"retry_after": wait,
		})
		telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.SigninThrottled)
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, UserID: user.ID,
			Provider: database.ProviderKindPassword, Reason: telemetry.SigninThrottled,
		})
		return nil, nil
	}

	if !password.Verify(cred.Hash, plain) {
		if device != "" {
			s.signinDevices.fail(device, now)
		} else {
			lockedUntil, err := s.db.RecordPasswordFailure(user.ID)
			if err != nil {
				log.Printf("local sign-in: recording a failed attempt for %s: %v", user.ID, err)
			}
			if lockedUntil != nil {
				log.Printf("local sign-in: %s locked out until %s after repeated failures", user.ID, lockedUntil.Format(time.RFC3339))
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": genericLoginFailure})
		telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.SigninBadCredential)
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, UserID: user.ID,
			Provider: database.ProviderKindPassword, Reason: telemetry.SigninBadCredential,
		})
		return nil, nil
	}

	// Same rule as every provider callback: a locked account gets no session
	// however well it authenticated, and is told why in the administrator's
	// words.
	if user.Locked() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": s.accessMessages().Locked, "reason": deniedLocked, "hint": "ask an administrator to unlock the account, then sign in again"})
		telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.SigninLocked)
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, UserID: user.ID,
			Provider: database.ProviderKindPassword, Reason: telemetry.SigninLocked,
		})
		return nil, nil
	}

	// And the same rule for an account no administrator has activated. It is
	// nearly unreachable here — a provider provisions these and a provisioned
	// account has no password — but "nearly" is not a reason for a sign-in path
	// to be the one that lets an unactivated account in.
	if user.PendingActivation() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": s.accessMessages().Pending, "reason": deniedPending, "hint": "ask an administrator to activate the account, then sign in again"})
		telemetry.RecordSignin(r.Context(), database.ProviderKindPassword, telemetry.SigninPending)
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, UserID: user.ID,
			Provider: database.ProviderKindPassword, Reason: telemetry.SigninPending,
		})
		return nil, nil
	}

	if err := s.db.ClearPasswordFailures(user.ID); err != nil {
		// Reporting, not authorization: a stale counter must not fail a sign-in
		// that has already succeeded.
		log.Printf("local sign-in: clearing the failure count for %s: %v", user.ID, err)
	}
	if device != "" {
		s.signinDevices.clear(device)
	}
	// Set on every verified password, the must-change case included: what it
	// remembers is that this browser knew the password, which it just did.
	s.rememberSigninDevice(w, r, user.ID, cred.UpdatedAt)
	return user, cred
}

// issuePasswordSession mints and sets the session cookies.
//
// The response is JSON with no redirect. The old dev picker answered a POST with
// a 307, which browsers replay as a POST to the target — the SPA navigates
// itself instead.
// It takes the request only to audit from it — the row needs the address and
// the user agent, which live on request metadata.
func (s *Server) issuePasswordSession(w http.ResponseWriter, r *http.Request, user *database.User, redirect string) {
	sess, err := s.mintSession(user)
	if err != nil {
		log.Printf("local sign-in: minting a session for %s: %v", user.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// After the mint, never before: the ledger records sessions that exist, and
	// a row written ahead of a failing mint would claim one that does not.
	s.recordSignin(r, signinAudit{
		Action: ActionSignin, UserID: user.ID, Provider: database.ProviderKindPassword,
	})
	if err := s.db.RecordPasswordSignIn(user.ID); err != nil {
		// Reporting only — a stamp that failed must not fail a sign-in that
		// succeeded.
		log.Printf("local sign-in: stamping the identity for %s: %v", user.ID, err)
	}
	s.setSessionCookies(w, sess)
	dest := s.safeRedirect(r, redirect)
	if dest == "" {
		dest = "/"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": dest})
}
