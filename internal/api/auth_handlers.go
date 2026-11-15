package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"

	"toft/internal/auth"
	"toft/internal/authprovider"
	"toft/internal/database"
	"toft/internal/telemetry"
)

// Sign-in, for any configured provider.
//
// One login handler and one callback handler, parameterized by provider id.
// There used to be a GitHub-shaped pair, which is why `users.id` used to be a
// GitHub numeric id — a hardcoded provider leaks into the schema eventually.
//
// The routes are `/api/v1/auth/{provider}/login` and `…/{provider}/callback`,
// and `github` remains a valid provider id, so an install with
// GITHUB_CLIENT_ID set keeps the URLs it already registered at GitHub.

const (
	oauthStateCookie    = auth.CookiePrefix + "oauth_state"
	oauthVerifierCookie = auth.CookiePrefix + "oauth_verifier"
	oauthRedirectCookie = auth.CookiePrefix + "oauth_redirect"
	// Ten minutes is the whole budget for a human to complete a sign-in at the
	// provider. Long enough for a password plus an MFA prompt, short enough
	// that an abandoned attempt is not a credential lying around.
	oauthCookieTTL = 600
)

// loginProviderDTO is what the login page needs, and no more. Client ids are
// public by construction but the page has no use for one, so it does not get
// one.
type loginProviderDTO struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
}

type loginOptionsResponse struct {
	// Providers is every way in, including local password sign-in when it is
	// enabled — the page tells that one apart by `kind: "password"` and renders a
	// form instead of a button. There is no separate flag for it: a second way of
	// expressing "this install offers local sign-in" is a second thing to keep in
	// step with the provider row that actually decides.
	Providers []loginProviderDTO `json:"providers"`
	// AutoSelect names the provider the page should redirect to immediately —
	// set only when it is the single way in. A typical enterprise install has
	// exactly one IdP, and asking someone to click "Sign in with Single
	// sign-on" on a page with one button is a step that exists for no reason.
	//
	// Computed here rather than in the SPA so the rule has one home, and so a
	// second provider appearing makes the page stop redirecting without a
	// frontend change.
	AutoSelect string `json:"auto_select,omitempty"`

	// The refusal messages, in the administrator's words. Served to anonymous
	// callers because that is exactly who reads them: a locked user or someone
	// with no account is not signed in, and the login page is the only surface
	// left that can explain what happened. They are notices an admin wrote to
	// be shown to people being turned away — nothing about them is a secret.
	LockedMessage    string `json:"locked_message"`
	NoAccountMessage string `json:"no_account_message"`
	// The third refusal a person can be shown here: their account exists and
	// nobody has activated it yet. Served to anonymous callers for the same
	// reason as the other two — the person reading it is not signed in, and
	// this page is the only surface left that can explain what happened.
	PendingMessage string `json:"pending_activation_message"`
}

// Refusal reasons, as they appear in `?denied=` on the login page. Constants
// because the same strings are written here and read by the SPA, and a typo
// would show the wrong sentence to someone who has just been locked out.
const (
	deniedLocked    = "locked"
	deniedNoAccount = "no_account"
	// deniedPending is an account that exists and has not been activated. It is
	// deliberately not folded into deniedNoAccount: "there is no account for
	// you" and "your account is in a queue" are different things to be told,
	// and only one of them means wait.
	deniedPending = "pending_activation"
	// deniedGroupRefused is a provider that gates on directory groups turning
	// somebody away. One reason for both of the gate's refusals: the person is
	// told the same thing either way, because the difference between "your
	// groups do not permit this" and "your provider sent no groups" is an
	// administrator's problem and not theirs — and spelling it out to the
	// browser would tell an attacker which half of the policy they tripped.
	deniedGroupRefused = "group_not_permitted"
)

// accessCopy is what to tell someone this platform is turning away at the door,
// in the administrator's words. One struct rather than a growing tuple: the
// family has gained a member twice now, and each time every call site had to be
// edited to ignore one more return value.
type accessCopy struct {
	Locked    string
	NoAccount string
	Pending   string
}

// accessMessages returns what to tell someone who is being refused. Failing to
// read the settings falls back to the defaults rather than to silence: the one
// outcome worse than a generic message is a blank page.
func (s *Server) accessMessages() accessCopy {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		log.Printf("settings: reading access messages, falling back to defaults: %v", err)
		return accessCopy{
			Locked:    database.DefaultLockedMessage,
			NoAccount: database.DefaultNoAccountMessage,
			Pending:   database.DefaultPendingActivationMessage,
		}
	}
	return accessCopy{
		Locked:    settings.LockedMessage,
		NoAccount: settings.NoAccountMessage,
		Pending:   settings.PendingActivationMessage,
	}
}

// denyToLogin sends a refused sign-in back to the login page with a reason,
// which is where the message an administrator wrote is rendered. A bare
// http.Error would be an unstyled page with the platform's own wording on it,
// and no way back in.
func (s *Server) denyToLogin(w http.ResponseWriter, r *http.Request, reason string) {
	s.clearOAuthCookies(w)
	// The session cookies too: a locked user arriving with a stale session
	// must not be bounced between a login page and a dead session.
	s.clearSessionCookies(w)
	http.Redirect(w, r, s.platformBaseFor(r)+"/login?denied="+url.QueryEscape(reason), http.StatusTemporaryRedirect)
}

func (s *Server) handleLoginOptions(w http.ResponseWriter, r *http.Request) {
	providers, err := s.loginProviders()
	if err != nil {
		log.Printf("listing login providers: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	msgs := s.accessMessages()
	resp := loginOptionsResponse{
		Providers:        make([]loginProviderDTO, 0, len(providers)),
		LockedMessage:    msgs.Locked,
		NoAccountMessage: msgs.NoAccount,
		PendingMessage:   msgs.Pending,
	}
	for _, p := range providers {
		resp.Providers = append(resp.Providers, loginProviderDTO{
			ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName,
		})
	}
	// Only a provider the browser can be *sent* to is worth redirecting to.
	// Password sign-in is a form on this page, so a lone password provider means
	// the page renders it — auto-selecting it would be a redirect to here.
	if len(resp.Providers) == 1 && resp.Providers[0].Kind != authprovider.KindPassword {
		resp.AutoSelect = resp.Providers[0].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleProviderLogin starts an authorization code flow.
func (s *Server) handleProviderLogin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "provider")
	redirect := s.safeRedirect(r, r.URL.Query().Get("redirect"))

	provider, err := s.authProvider(id)
	if errors.Is(err, errNoSuchProvider) {
		http.Error(w, "unknown sign-in provider", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("resolving provider %s: %v", id, err)
		http.Error(w, "sign-in is misconfigured — contact an administrator", http.StatusInternalServerError)
		return
	}
	if !provider.Enabled {
		http.Error(w, "that sign-in provider is disabled", http.StatusForbidden)
		return
	}
	// Password sign-in has nowhere to send the browser: the form is on the login
	// page. A GET here is a bookmark or a hand-typed URL, so it lands there
	// rather than on an error about a flow that does not apply.
	if provider.Kind == authprovider.KindPassword {
		dest := s.platformBaseFor(r) + "/login"
		if redirect != "" {
			dest += "?redirect=" + url.QueryEscape(redirect)
		}
		http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
		return
	}
	if provider.ClientID == "" || provider.ClientSecret == "" {
		http.Error(w, "that sign-in provider is not fully configured", http.StatusServiceUnavailable)
		return
	}

	// State binds the callback to this browser *and* to this provider. Without
	// the provider half, a state minted for one provider could be replayed at
	// another's callback, where a hostile provider's assertion would be
	// accepted as the trusted one's.
	nonce := generateState()
	s.setOAuthCookie(w, oauthStateCookie, provider.ID+"|"+nonce)

	verifier := ""
	if provider.UsesPKCE() {
		verifier = oauth2.GenerateVerifier()
		s.setOAuthCookie(w, oauthVerifierCookie, verifier)
	}

	// Validated on the way in as well as on the way out, so an unusable value
	// never reaches the cookie.
	if redirect != "" {
		s.setOAuthCookie(w, oauthRedirectCookie, redirect)
	}

	authURL, err := provider.AuthURLFor(r.Context(), nonce, s.redirectURIFor(r, provider.ID), verifier)
	if err != nil {
		// Almost always a bad issuer or an unreachable IdP. Logged in full,
		// summarized to the browser: the detail names internal hosts.
		log.Printf("provider %s: building authorization URL: %v", provider.ID, err)
		http.Error(w, "could not reach the sign-in provider — contact an administrator", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

// handleProviderCallback completes the flow: verify state, redeem the code, map
// the identity onto a user, mint a session.
func (s *Server) handleProviderCallback(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "provider")
	q := r.URL.Query()

	// The provider may report a refusal instead of a code — a denied consent
	// screen, or an IdP-side policy block. Not an error on our side, so it
	// returns the user to the login page with the reason.
	if providerErr := q.Get("error"); providerErr != "" {
		log.Printf("provider %s returned error=%s description=%q", id, providerErr, q.Get("error_description"))
		s.clearOAuthCookies(w)
		http.Redirect(w, r, s.platformBaseFor(r)+"/login?error="+url.QueryEscape(providerErr), http.StatusTemporaryRedirect)
		return
	}

	provider, err := s.authProvider(id)
	if err != nil {
		if !errors.Is(err, errNoSuchProvider) {
			log.Printf("resolving provider %s: %v", id, err)
		}
		http.Error(w, "unknown sign-in provider", http.StatusNotFound)
		return
	}
	// A password provider has no authorization code flow, so it has no callback.
	// 404 rather than an explanation: the URL is not part of anything.
	if provider.Kind == authprovider.KindPassword {
		http.Error(w, "unknown sign-in provider", http.StatusNotFound)
		return
	}
	// The same refusal the login redirect gives, because this is the other half
	// of the same door: a person who built the authorize URL by hand, or whose
	// browser still holds a state cookie from before the provider was switched
	// off, arrives here with a code and must not be minted a session through a
	// provider the audit log says is closed. Exchange refuses too, so the rule
	// holds for a caller that never came through here.
	if !provider.Enabled {
		http.Error(w, "that sign-in provider is disabled", http.StatusForbidden)
		return
	}

	stateValue := s.readOAuthCookie(r, oauthStateCookie)
	if stateValue == "" {
		log.Printf("OAuth callback (%s): no state cookie, or more than one", id)
		http.Error(w, "invalid state - cookie missing. Try clearing cookies and signing in again.", http.StatusBadRequest)
		return
	}
	cookieProvider, cookieNonce, ok := strings.Cut(stateValue, "|")
	if !ok || cookieProvider != id ||
		subtle.ConstantTimeCompare([]byte(cookieNonce), []byte(q.Get("state"))) != 1 {
		// Never log the live value: the log must not become a CSRF bypass
		// channel.
		log.Printf("OAuth callback (%s): state mismatch (cookie provider=%q, nonce len=%d, query len=%d)",
			id, cookieProvider, len(cookieNonce), len(q.Get("state")))
		http.Error(w, "invalid state - mismatch", http.StatusBadRequest)
		return
	}

	verifier := s.readOAuthCookie(r, oauthVerifierCookie)
	if provider.UsesPKCE() && verifier == "" {
		http.Error(w, "invalid state - the sign-in attempt is incomplete. Try signing in again.", http.StatusBadRequest)
		return
	}

	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	profile, err := provider.Exchange(r.Context(), code, s.redirectURIFor(r, provider.ID), verifier)
	if err != nil {
		log.Printf("provider %s: exchange failed: %v", provider.ID, err)
		s.clearOAuthCookies(w)
		http.Error(w, "authentication failed", http.StatusInternalServerError)
		return
	}

	// The domain policy is what keeps a multi-tenant provider (Google, GitHub)
	// from meaning "anyone on the internet". Refused with the reason, because
	// "authentication failed" would send the user to reset a password that
	// worked fine.
	// An allow-list is evidence about an address, so it is only evidence when the
	// provider has not said the address is unverified: a multi-tenant IdP that
	// lets a person assert someone else's address would otherwise walk through
	// an allowed-domain gate.
	if !provider.EmailAllowed(profile.Email) || (provider.RestrictsEmail() && profile.EmailUnverified) {
		log.Printf("provider %s: refusing %s — outside the allowed email domains", provider.ID, profile.Email)
		// No account was resolved, so there is no id to attribute this to — the
		// address the provider vouched for is the only handle on who tried.
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, Provider: provider.Kind,
			Reason: signinDomainRefused, Attempted: profile.Email,
		})
		s.clearOAuthCookies(w)
		http.Error(w, "your account's email domain is not permitted to sign in here — contact an administrator",
			http.StatusForbidden)
		return
	}

	// The directory gate, and it sits **before** ResolveIdentity deliberately:
	// a refused person creates no account, no pending row and no queue for an
	// administrator to work through. A gate that provisioned the people it
	// rejected would fill the People page with everyone in the tenant.
	//
	// After the domain check, so an address from the wrong tenant is still
	// refused with the domain's reason — the cheaper and more specific answer.
	if decision := provider.GroupsPermitted(profile); decision != authprovider.GroupsAdmitted {
		reason := signinGroupRefused
		if decision == authprovider.GroupsClaimAbsent {
			// Said out loud in the log as well as the audit row, because this
			// is the one that is almost never about the person: it is a scope
			// nobody added, an Entra overage, or an app configured to omit the
			// claim, and it will be refusing everybody rather than somebody.
			reason = signinGroupClaimAbsent
			log.Printf("provider %s: refusing %s — this provider permits only certain directory groups and the sign-in asserted no group claim at all; check the provider is configured to send it",
				provider.ID, profile.Email)
		} else {
			log.Printf("provider %s: refusing %s — none of their directory groups is permitted to sign in",
				provider.ID, profile.Email)
		}
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, Provider: provider.Kind,
			Reason: reason, Attempted: profile.Email,
		})
		s.denyToLogin(w, r, deniedGroupRefused)
		return
	}

	user, outcome, err := s.db.ResolveIdentity(provider.ID, database.IdentityProfile{
		Subject:       profile.Subject,
		Login:         profile.Login,
		Name:          profile.Name,
		Email:         profile.Email,
		EmailVerified: profile.EmailVerified,
		AvatarURL:     profile.AvatarURL,
	}, database.SignInPolicy{
		DefaultRole:         provider.DefaultRole,
		LinkByVerifiedEmail: provider.LinkByVerifiedEmail,
		AutoProvision:       provider.AutoProvision,
	}, auditEntryFor(r))
	if errors.Is(err, database.ErrNoAccount) {
		// Authentication worked; this install does not hand out accounts. Not
		// an error — a policy outcome, and the person is told so in the
		// administrator's words rather than shown a failure.
		log.Printf("provider %s: refusing %s — no account and auto-provisioning is off", provider.ID, profile.Email)
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, Provider: provider.Kind,
			Reason: signinNotProvisioned, Attempted: profile.Email,
		})
		s.denyToLogin(w, r, deniedNoAccount)
		return
	}
	if err != nil {
		log.Printf("provider %s: resolving identity: %v", provider.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if outcome != database.SignInExisting {
		log.Printf("sign-in: %s user %s via %s", outcome, user.ID, provider.ID)
	}

	// Locked accounts get no session, whichever provider vouched for them.
	if user.Locked() {
		log.Printf("provider %s: refusing locked user %s", provider.ID, user.ID)
		telemetry.RecordSignin(r.Context(), provider.Kind, telemetry.SigninLocked)
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, UserID: user.ID,
			Provider: provider.Kind, Reason: telemetry.SigninLocked,
		})
		s.denyToLogin(w, r, deniedLocked)
		return
	}

	// An account nobody has activated gets no session either, and this is where
	// it is refused rather than in the resolver: the row has to be written
	// first, or the administrator has no queue to approve from and the person
	// would be told "no account" forever.
	//
	// Every sign-in, not only the first. There is no "you have already been
	// told" state to keep, and an account that goes quiet on the second attempt
	// reads as the platform breaking rather than as a queue.
	if user.PendingActivation() {
		if outcome == database.SignInProvisioned {
			// Once, on the sign-in that created the row. Every later attempt is
			// refused in silence — the queue is the record, and a nudge that
			// repeated each time somebody retried is a nudge people filter.
			s.notifyActivationPending(user, provider.DisplayName)
		}
		log.Printf("provider %s: refusing user %s — awaiting activation", provider.ID, user.ID)
		telemetry.RecordSignin(r.Context(), provider.Kind, telemetry.SigninPending)
		s.recordSignin(r, signinAudit{
			Action: ActionSigninFailed, UserID: user.ID,
			Provider: provider.Kind, Reason: telemetry.SigninPending,
		})
		s.denyToLogin(w, r, deniedPending)
		return
	}

	// Directory group membership, before the session is minted and after every
	// refusal above — a person who is not getting in has no membership worth
	// reconciling, and a locked account's groups are not this code's business.
	//
	// `mintSession` re-reads the group list and the token version from the
	// database, so committing before this call is the whole of the ordering
	// requirement: the session about to be issued carries what was just written.
	s.syncDirectoryGroups(r, provider, user, profile)

	sess, err := s.mintSession(user)
	if err != nil {
		log.Printf("minting session for %s: %v", user.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	telemetry.RecordSignin(r.Context(), provider.Kind, telemetry.OutcomeSucceeded)
	s.recordSignin(r, signinAudit{
		Action: ActionSignin, UserID: user.ID, Provider: provider.Kind,
	})

	redirectTo := s.platformBaseFor(r) + "/"
	if v := s.readOAuthCookie(r, oauthRedirectCookie); v != "" {
		// Re-validated on the way out too. The cookie is HttpOnly and
		// server-set, but this is the line that actually sends the user
		// somewhere.
		if target := s.safeRedirect(r, v); target != "" {
			redirectTo = target
		}
	}

	s.clearOAuthCookies(w)
	s.setSessionCookies(w, sess)
	http.Redirect(w, r, redirectTo, http.StatusTemporaryRedirect)
}

// syncDirectoryGroups reconciles this person's directory memberships against the
// group claim the provider just asserted.
//
// **It never fails the sign-in.** Authentication succeeded; this is an
// authorization refresh, and refusing to let somebody in because a membership
// write failed would trade a stale group for a locked-out person. A failure is
// logged and the person signs in with the membership already on their row,
// which their next sign-in fixes.
//
// An absent claim reaches the reconciler as absent and reconciles nothing —
// that distinction is carried all the way from the token rather than collapsed
// here, because collapsing it is how an install strips every directory
// membership it has.
func (s *Server) syncDirectoryGroups(r *http.Request, provider *authprovider.Provider, user *database.User, profile *authprovider.Profile) {
	res, err := s.db.ReconcileDirectoryMemberships(r.Context(), database.DirectorySync{
		UserID:     user.ID,
		ProviderID: provider.ID,
		Claimed:    profile.Groups,
		Asserted:   profile.GroupsAsserted,
	}, requestMeta(r.Context()))
	if err != nil {
		log.Printf("provider %s: reconciling directory groups for %s: %v", provider.ID, user.ID, err)
		return
	}
	if res.Changed() {
		log.Printf("provider %s: directory groups for %s: +%d -%d",
			provider.ID, user.ID, len(res.Added), len(res.Removed))
	}
	if len(res.Capped) > 0 {
		// Recorded in the audit log by the reconciler; said here too, because
		// this is the one outcome an operator would want to find by grepping.
		log.Printf("provider %s: %s is claimed into %d more groups than the %d-group cap allows",
			provider.ID, user.ID, len(res.Capped), database.GroupMembershipCap)
	}
}

// mintSession issues the session JWT for a user who has just proved who they
// are. Shared by every sign-in path — provider callbacks and local sign-in —
// so none of them can forget the token version, which is what makes a session
// revocable, or the session policy, which is what gives it a lifetime.
// mintSession is a sign-in: the person has just proved who they are, so the
// session's clock starts now and last_login_at is stamped.
func (s *Server) mintSession(user *database.User) (auth.Session, error) {
	// Last sign-in is reporting, not authorization: a failed stamp is logged
	// and the session still issues rather than locking someone out over it.
	if err := s.db.RecordLogin(user.ID); err != nil {
		log.Printf("recording last login for %s: %v", user.ID, err)
	}
	return s.mintSessionFrom(user, time.Time{})
}

// mintSessionFrom mints a session whose clock started at authTime — the app
// handshake's case, which carries the platform session's start across to the
// apps origin and is not a login. Zero means now.
func (s *Server) mintSessionFrom(user *database.User, authTime time.Time) (auth.Session, error) {
	return s.mintSessionFor(user, authTime, "")
}

// mintSessionFor is mintSessionFrom for an audience: "" is a platform session,
// auth.AppsAudience(origin) the app handshake's.
//
// The order is keepOwnSession's: the token version first, then the role and
// the groups. A role or membership change bumps the version in its own
// transaction, so one that commits after the version is read revokes this
// session, and one before it is in what is read next. Reading the version
// last let a change land in between and the session carry the old role or
// groups under the new version. The role is the row's, never the passed-in
// User's, which the caller read earlier.
func (s *Server) mintSessionFor(user *database.User, authTime time.Time, audience string) (auth.Session, error) {
	tokenVersion, err := s.db.GetTokenVersion(user.ID)
	if err != nil {
		return auth.Session{}, err
	}
	current, err := s.db.GetUser(user.ID)
	if err != nil {
		return auth.Session{}, err
	}
	if current == nil {
		return auth.Session{}, errors.New("minting a session: the user no longer exists")
	}
	groups, err := s.db.GetUserGroupIDs(user.ID)
	if err != nil {
		return auth.Session{}, err
	}
	return s.jwt.Generate(auth.GenerateParams{
		UserID:        user.ID,
		Login:         user.Login,
		AvatarURL:     user.AvatarURL,
		PrincipalType: database.PrincipalTypeUser,
		SystemRole:    current.SystemRole,
		Groups:        groups,
		TokenVersion:  tokenVersion,
		Policy:        s.sessionPolicy(),
		AuthTime:      authTime,
		Audience:      audience,
	})
}

// oauthCookie is the name and path an in-flight sign-in cookie has on this
// install: under `__Host-` at `Path=/` wherever the session itself is
// (hostSessionCookie), because a sibling host — the apps host, or an app's own
// hostname — can toss a plain `Domain=` cookie onto the platform's. A planted
// state cookie, with the attacker's own state in the callback URL, signed the
// victim's browser in as the attacker (security review 2026-11-10, D-2).
func (s *Server) oauthCookie(name string) (string, string) {
	if s.hostSessionCookie() {
		return auth.HostCookiePrefix + name, "/"
	}
	return name, oauthCookiePath
}

// readOAuthCookie is the one copy of an in-flight sign-in cookie, or "": two
// copies is no answer, as two session cookies are none (auth.SessionTokenFor).
func (s *Server) readOAuthCookie(r *http.Request, name string) string {
	name, _ = s.oauthCookie(name)
	var v string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			v = c.Value
			n++
		}
	}
	if n != 1 {
		return ""
	}
	return v
}

func (s *Server) setOAuthCookie(w http.ResponseWriter, name, value string) {
	name, path := s.oauthCookie(name)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oauthCookieTTL,
	})
}

// clearOAuthCookies expires the in-flight sign-in state. Called on every exit
// from the callback, success or failure: a leftover verifier or state is a
// credential for an attempt that is over.
func (s *Server) clearOAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{oauthStateCookie, oauthVerifierCookie, oauthRedirectCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: oauthCookiePath, MaxAge: -1, HttpOnly: true,
			Secure: s.secureCookies(),
		})
		if s.secureCookies() {
			http.SetCookie(w, &http.Cookie{
				Name: auth.HostCookiePrefix + name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
				Secure: true,
			})
		}
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Revoke, don't just clear: without the bump a captured token stays
	// valid until exp. This invalidates the user's sessions on all devices.
	if token := s.sessionToken(r); token != "" {
		// Live, not merely signed: a token already revoked — a stale tab, a
		// cookie captured before a sign-out — must not end the sessions
		// minted after it, nor audit a sign-out nobody made.
		if claims, err := s.jwt.Validate(token); err == nil && auth.CheckSession(s.db, claims, s.sessionPolicy()) {
			if err := s.db.BumpTokenVersion(claims.UserID); err != nil {
				log.Printf("logout: bumping token version for %s: %v", claims.UserID, err)
			}
			// Only for a session that existed. Clearing cookies that were never
			// valid is not somebody signing out, and a row for it would make
			// the ledger's sign-outs outnumber its sign-ins.
			s.recordSignin(r, signinAudit{Action: ActionSignout, UserID: claims.UserID})
		}
	}
	s.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	// An API key answers here too, so a script can verify its token against
	// something before assuming it works — and find out *which* key it is
	// holding, which is the question when three of them are in one CI config.
	// apiKeyAuth has already checked the key's state and its owner's lock, so
	// there is no liveness rule to re-apply: unlike the cookie below, nothing
	// about this credential was decided in the past.
	if p := auth.GetPrincipal(r.Context()); p.IsAPIKey() {
		user, err := s.db.GetUser(p.ID)
		if err != nil || user == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "user not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id": user.ID, "login": user.Login, "name": user.Name,
			"email": user.Email, "avatar_url": user.AvatarURL,
			"system_role": user.SystemRole, "created_at": user.CreatedAt,
			// Named "via" rather than folded into the user, because the answer
			// to "who am I" is the person and the answer to "how did I get
			// here" is the credential.
			"via":       map[string]string{"api_key_id": p.APIKeyID, "api_key_name": p.APIKeyName},
			"time_zone": s.timeZoneFor(user.ID),
		})
		return
	}

	token := s.sessionToken(r)
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	claims, err := s.jwt.Validate(token)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid session"})
		return
	}

	user, err := s.db.GetUser(claims.UserID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "user not found"})
		return
	}

	// This handler sits outside the session middleware — it reads the cookie
	// itself — so it has to apply the same liveness rule, or a locked user's
	// browser is told it is signed in and then 401s on every other call. The
	// lock is checked first and named, because "you are locked" is the one
	// refusal here that the person can act on.
	if user.Locked() {
		s.clearSessionCookies(w)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": s.accessMessages().Locked, "reason": deniedLocked,
			"hint": "ask an administrator to unlock the account, then sign in again",
		})
		return
	}
	if !auth.CheckSession(s.db, claims, s.sessionPolicy()) {
		// Revoked: a role or group change, a logout elsewhere, a deleted user.
		s.clearSessionCookies(w)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid session"})
		return
	}

	// Which external accounts reach this user. Shown on the account page so
	// someone with two providers linked can see both, and so a support question
	// about "which login is this" has an answer.
	identities, err := s.db.ListUserIdentities(user.ID)
	if err != nil {
		log.Printf("listing identities for %s: %v", user.ID, err)
		identities = nil
	}
	logins := make([]map[string]any, 0, len(identities))
	for _, i := range identities {
		logins = append(logins, map[string]any{
			"provider_id": i.ProviderID, "email": i.Email, "login": i.Login,
			"name": i.Name, "last_login_at": i.LastLoginAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":          user.ID,
		"login":       user.Login,
		"name":        user.Name,
		"email":       user.Email,
		"avatar_url":  user.AvatarURL,
		"system_role": user.SystemRole,
		"created_at":  user.CreatedAt,
		"groups":      claims.Groups,
		"identities":  logins,
		"time_zone":   s.timeZoneFor(user.ID),
	})
}

func generateState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// An all-zero state would silently collapse the OAuth CSRF
		// protection; a broken system entropy source is not survivable.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// writeJSON encodes data as the response body with the given status.
//
// An error body is completed into the one envelope every refusal carries —
// `code`, `retryable` and, where one is worth naming, `hint` — see
// error_envelope.go.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	if _, verbatim := w.(verbatimErrorWriter); status >= 400 && !verbatim {
		data = completeErrorBody(status, data)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// appOwnerID answers which person owns an app this principal is creating, or ""
// when there is no person behind it.
//
// **One function, and every creation path calls it**, for the reason
// `auditActor` is one function: three call sites deriving an owner three ways
// is three chances to get it wrong, and this one *was* wrong. All three read
// the *session's* JWT claims — which a personal API key does not set, because
// it is not a session. So `POST /apps`, `POST /apps/upload` and the gallery's
// one-click deploy each created an app with **no owner at all** when an `toftu_`
// key made the request, and `apps_one_owner` refused the insert with a 500.
// Every one of those routes is on the personal-key allow-list, so every one of
// them was meant to work; the read went to the wrong place, not the route.
//
// **The principal is the source, and the claims never were.** A credential that
// is not a cookie session has no claims and a perfectly good principal sitting
// in the same context — which is what `auditActor` had always read, and why the
// audit row named the right person on an app whose owner column was empty.
//
// The three classes, and each is a different answer rather than a special case:
//
//   - A **personal key is a person** — the principal *is* the user, ID and all
//     (see auth.Principal's own comment), and the key is only which credential
//     they spoke through. So a key-created app is owned by the key's owner,
//     exactly as if they had used a browser.
//   - A **service account** acts on behalf of somebody: its own ID is not a
//     person's and must never land in `owner_user_id`, so the owning user is
//     the answer and "" when it has none.
//   - A **deploy key is nobody** (ADR §51). There is no person to fall back on,
//     which is why `POST /apps` demands `owner_group_id` from it — and why ""
//     here is the right answer rather than a failure: the caller turns it into
//     the refusal that names the field.
func appOwnerID(p *auth.Principal) string {
	switch {
	case p == nil || p.IsDeployKey():
		return ""
	case p.IsServiceAccount():
		return p.OwningUserID
	case p.IsUser():
		return p.ID
	default:
		return ""
	}
}

func getAppID(r *http.Request) string {
	return chi.URLParam(r, "appID")
}

// kindLabel names a provider kind for a message. Small, but it keeps the
// vocabulary in one place as kinds are added.
func kindLabel(kind string) string {
	switch kind {
	case authprovider.KindGitHub:
		return "GitHub OAuth"
	default:
		return "OpenID Connect"
	}
}
