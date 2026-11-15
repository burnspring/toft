package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/auth"
	"toft/internal/database"
)

// `toft login` — a device flow, in the shape RFC 8628 established and `gh` made
// familiar. A terminal asks, a person approves in a browser, and the terminal
// collects an ordinary personal API key.
//
// **What it mints is an `toftu_` key and nothing else.** The same rows the keys
// page lists, owned by the approving user, revocable where every other
// credential is revoked. A personal key is an identity; a CLI that accepted a
// per-app credential would be asking one that says "may reach this app" to
// answer "who is deploying". No third key class, no
// second authentication path.
//
// Four properties hold, and three of them fall out of where these routes are
// mounted rather than from anything written here:
//
//   - **The mint is authorized by a browser session, never by a key.** These
//     live under /api/v1/auth, where a presented key is ignored and the caller
//     is anonymous, so a stolen toftu_ cannot mint a fresh one. That is the
//     allow-list's central rule and this must not be its exception.
//   - **The mint writes audit_log in the same transaction**, like every other
//     authorization-relevant mutation — twice, in fact: once when the human
//     decides and once when the key is created.
//   - **Off means absent.** Where personal keys are switched off, the start
//     route answers the same 404 the rest of the capability answers.
//   - **A code is short-lived and single-use.** Ten minutes, one approval, and
//     a per-code back-off for a client polling faster than it was told to.
//     There is no per-IP bucket: the platform imposes no request rate limit,
//     and volume belongs to whatever is in front of it (ADR §75).
//
// Why the device flow rather than a loopback redirect: loopback needs no typing
// and is phishing-resistant, because the credential returns over 127.0.0.1
// rather than through a human — and it requires the browser and the terminal to
// be the same machine, which for an internal platform is a coin flip. The
// session where the browser door is most annoying is exactly the one where
// somebody is ssh'd into a build box. The residual risk, that a person can be
// talked into approving a code that is not theirs, is inherent to the flow and
// is accepted rather than designed around; the mitigations are the ones above,
// plus the machine named on the approval screen and the key visible afterwards.

const (
	// deviceCodeTTL bounds how long an unanswered request stays approvable. A
	// code that outlives the terminal session it belongs to is a code somebody
	// can be talked into approving later.
	deviceCodeTTL = 10 * time.Minute

	// devicePollInterval is what the client is told to wait between polls, and
	// devicePollFloor is what the server will actually tolerate. The gap
	// absorbs jitter: a client sleeping exactly 5s arrives at 4.98s often
	// enough that a strict floor would tell a well-behaved client to slow down.
	devicePollInterval = 5 * time.Second
	devicePollFloor    = 4 * time.Second

	// deviceUserCodeAlphabet has no vowels, so no code is ever a word, and none
	// of 0/O/1/I/L, so none is ever misread off a terminal onto a web page. 29
	// characters over 8 positions is about 39 bits, which with a ten-minute
	// life, and a lookup that needs a signed-in session, is not a thing to
	// guess at.
	deviceUserCodeAlphabet = "BCDFGHJKMNPQRSTVWXZ23456789"
	deviceUserCodeLen      = 8
)

// generateUserCode is the eight characters a person reads off their terminal,
// hyphenated in the middle because that is how people transcribe them.
func generateUserCode() (string, error) {
	limit := big.NewInt(int64(len(deviceUserCodeAlphabet)))
	var b strings.Builder
	for i := 0; i < deviceUserCodeLen; i++ {
		if i == deviceUserCodeLen/2 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b.WriteByte(deviceUserCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// generateDeviceCode is the secret half — 32 bytes, held by the client and
// stored only as a SHA-256, like every other credential in this schema.
func generateDeviceCode() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// personalKeysAbsent answers the routes that exist only because personal keys
// do. Off means **absent**: 404 with the same sentence the rest of the
// capability uses, so the flow looks like something this install does not offer
// rather than something it offers and denies.
func (s *Server) personalKeysAbsent(w http.ResponseWriter) bool {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return true
	}
	if !settings.APIKeysEnabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "ask an administrator to turn personal API keys on for this install; toft login needs them"})
		return true
	}
	return false
}

// installOrigin is the address this install answers to, for a link handed back
// to the caller that just reached it.
//
// The configured public base URL wins, because an operator who has said where
// the install lives has said it for a reason. Failing that it is derived from
// this very request — which is sound here in a way it would not be for an
// address put in somebody else's email: the recipient of this URL is the client
// that just dialled it, so the worst a forged Host achieves is telling a caller
// to visit the address it already used.
func (s *Server) installOrigin(r *http.Request) string {
	if base := s.db.ResolvePublicBaseURL(s.cfg.BaseURL); base != "" {
		return base
	}
	if origin := forwardedOrigin(r); origin != "" {
		return origin
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// maxDeviceDisplayLen bounds what a client may put on the approval screen.
// Both fields are untrusted strings that a human reads to decide whether to
// hand over a credential, and a hostname long enough to push the rest of the
// sentence off the page is the shape of the trick.
const maxDeviceDisplayLen = 64

// sanitizeDeviceDisplay keeps a reported hostname or version to one short line
// of printable characters. Escaping happens at render time — this is so the
// approval screen cannot be laid out by whoever ran the client.
func sanitizeDeviceDisplay(v string) string {
	v = strings.TrimSpace(v)
	var b strings.Builder
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxDeviceDisplayLen {
			break
		}
	}
	return b.String()
}

// handleCLIDeviceStart begins a login. Unauthenticated by necessity — the whole
// point is that the terminal has no credential yet. Not rate-limited here,
// like every other way in: ADR §75 puts volume at the address in front.
func (s *Server) handleCLIDeviceStart(w http.ResponseWriter, r *http.Request) {
	if s.personalKeysAbsent(w) {
		return
	}

	var req struct {
		Hostname string `json:"hostname"`
		Version  string `json:"version"`
	}
	// A body is optional: the two fields are display material for the approval
	// screen, and a client that sends neither gets a screen that says so rather
	// than a refusal.
	json.NewDecoder(r.Body).Decode(&req)

	userCode, err := generateUserCode()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	deviceCode, err := generateDeviceCode()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	now := time.Now().UTC()
	pending := &database.DeviceCode{
		UserCode:      userCode,
		Hostname:      sanitizeDeviceDisplay(req.Hostname),
		ClientVersion: sanitizeDeviceDisplay(req.Version),
		Client:        database.DeviceClientLP,
		RequestedFrom: clientIP(r),
		CreatedAt:     now,
		ExpiresAt:     now.Add(deviceCodeTTL),
	}
	if err := s.db.CreateDeviceCode(pending, hashAPIKey(deviceCode)); errors.Is(err, database.ErrTooManyPendingCodes) {
		w.Header().Set("Retry-After", strconv.Itoa(int(deviceCodeTTL.Seconds())))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "too many sign-in requests are waiting to be approved — try again in a few minutes"})
		return
	} else if err != nil {
		log.Printf("cli login: recording a device code: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	origin := s.installOrigin(r)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          origin + "/cli",
		"verification_uri_complete": origin + "/cli?code=" + userCode,
		"interval":                  int(devicePollInterval.Seconds()),
		"expires_in":                int(deviceCodeTTL.Seconds()),
	})
}

// deviceRequestView is what the approval screen is allowed to know: which
// machine asked, when the request dies, and whether it is still open. Never the
// device code, which is the client's secret and is not this page's business.
type deviceRequestView struct {
	UserCode      string `json:"user_code"`
	Hostname      string `json:"hostname"`
	ClientVersion string `json:"client_version"`
	// Which program is asking — `toft` or `rsconnect` — and from where. The page
	// words itself by the first and shows the second, because "approve only a
	// machine you are sitting at" needs something on screen to check against.
	Client        string    `json:"client"`
	RequestedFrom string    `json:"requested_from"`
	ExpiresAt     time.Time `json:"expires_at"`
	Status        string    `json:"status"`
}

// The four states a request can be in, from the outside. `pending` is the
// answer to nearly every poll and is deliberately not an error: waiting for a
// human is what this flow does, and a client that had to read a 4xx as "carry
// on" would be one bug away from treating a real refusal as normal.
const (
	deviceStatusPending  = "pending"
	deviceStatusApproved = "approved"
	deviceStatusDenied   = "denied"
	deviceStatusExpired  = "expired"
)

// Expiry is tested first: an approval the client did not collect inside the
// ten minutes is expired, not approved, and ClaimDeviceCode refuses it on the
// same clause.
func deviceStatus(d *database.DeviceCode, now time.Time) string {
	switch {
	case !d.ExpiresAt.After(now):
		return deviceStatusExpired
	case d.ApprovedAt != nil:
		return deviceStatusApproved
	case d.DeniedAt != nil:
		return deviceStatusDenied
	}
	return deviceStatusPending
}

// handleCLIDeviceLookup is what the approval page reads before it asks anyone
// anything: the machine that is waiting, so the screen names it.
//
// **A cookie session only**, which is what makes it safe for this to be
// findable by an eight-character code: the reader is somebody already signed in
// to this install, and what they learn is a hostname they were about to be shown
// anyway. It is a GET on the same path the flow starts with because it is the
// same resource — POST creates the request, GET reads it.
func (s *Server) handleCLIDeviceLookup(w http.ResponseWriter, r *http.Request) {
	if s.personalKeysAbsent(w) {
		return
	}
	if auth.GetPrincipal(r.Context()) == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	code := normalizeUserCode(r.URL.Query().Get("user_code"))
	d, err := s.db.GetDeviceCodeByUserCode(code)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if d == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no sign-in is waiting on that code — it may have expired, or been typed wrong",
			"code":  "unknown_user_code",
			"hint":  "run toft login again and type the code it prints",
		})
		return
	}
	writeJSON(w, http.StatusOK, deviceRequestView{
		UserCode:      d.UserCode,
		Hostname:      d.Hostname,
		ClientVersion: d.ClientVersion,
		Client:        d.Client,
		RequestedFrom: d.RequestedFrom,
		ExpiresAt:     d.ExpiresAt,
		Status:        deviceStatus(d, time.Now().UTC()),
	})
}

// normalizeUserCode accepts what a person actually types: any case, with or
// without the hyphen, with spaces around it. The stored form is the canonical
// one, so this is where the difference is absorbed rather than in four places.
func normalizeUserCode(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	raw = strings.ReplaceAll(raw, "-", "")
	raw = strings.ReplaceAll(raw, " ", "")
	if len(raw) != deviceUserCodeLen {
		return raw
	}
	return raw[:deviceUserCodeLen/2] + "-" + raw[deviceUserCodeLen/2:]
}

// handleCLIDeviceApprove is the human's answer. **Cookie session only** — this
// is the authorization for a credential mint, and the whole design rests on it
// not being reachable by a credential.
func (s *Server) handleCLIDeviceApprove(w http.ResponseWriter, r *http.Request) {
	if s.personalKeysAbsent(w) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req struct {
		UserCode string `json:"user_code"`
		Approve  bool   `json:"approve"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	code := normalizeUserCode(req.UserCode)

	d, err := s.db.GetDeviceCodeByUserCode(code)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if d == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no sign-in is waiting on that code — it may have expired, or been typed wrong",
			"code":  "unknown_user_code",
			"hint":  "run toft login again and type the code it prints",
		})
		return
	}

	// The cap is checked here, where the person who would have to act on it is
	// looking at a screen, rather than only at collection time where the answer
	// would surface in a terminal on another machine.
	if req.Approve {
		n, err := s.db.CountUserAPIKeys(p.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if n >= maxAPIKeysPerUser {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "you can have at most 10 API keys — revoke one first",
				"code":  "key_limit_reached",
			})
			return
		}
	}

	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	audit.Action = "user.cli_login_approve"
	if !req.Approve {
		audit.Action = "user.cli_login_deny"
	}
	audit.TargetType = "user"
	audit.TargetID = p.ID
	// The machine and the code, never anything that could be replayed: the
	// device code is not here, and the key does not exist yet.
	audit.After = jsonValStr(map[string]interface{}{
		"user_code": d.UserCode, "hostname": d.Hostname, "client_version": d.ClientVersion,
	})

	decided, err := s.db.DecideDeviceCode(code, p.ID, req.Approve, audit)
	if err != nil {
		log.Printf("cli login: recording a decision: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !decided {
		// Answered already, or dead. Both are "there is nothing here to
		// decide", and telling them apart would only invite a second attempt.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "that sign-in is no longer waiting — it was already answered, or it expired",
			"code":  "not_pending",
			"hint":  "run toft login again to start a new sign-in",
		})
		return
	}

	status := deviceStatusApproved
	if !req.Approve {
		status = deviceStatusDenied
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// handleCLIDeviceToken is the poll. It answers the state of the request every
// time, and on the one poll that finds an approval it mints the key and returns
// the secret — exactly once, because the claim deletes the request in the same
// transaction.
func (s *Server) handleCLIDeviceToken(w http.ResponseWriter, r *http.Request) {
	if s.personalKeysAbsent(w) {
		return
	}

	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_code is required", "hint": "send the device_code that POST /api/v1/auth/cli/device returned, like {\"device_code\": \"...\"}"})
		return
	}
	hash := hashAPIKey(req.DeviceCode)

	d, err := s.db.PollDeviceCode(hash)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Unknown, or already collected. The second is why this is not an error
	// worth distinguishing: a request whose key has been handed over is gone,
	// and saying "collected" would tell a caller that did not collect it that
	// somebody else did.
	if d == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "that sign-in is not waiting — start a new one with `toft login`",
			"code":  "unknown_device_code",
			"hint":  "run toft login again to start a new sign-in",
		})
		return
	}

	now := time.Now().UTC()
	// A client polling faster than the interval it was given. Told to back off
	// rather than served, and *before* the state is read, so a tight loop
	// cannot be used to watch the table. The refused poll is deliberately not
	// stamped — see TouchDeviceCode — or a client that polled too fast would be
	// told to slow down forever.
	if d.LastPolledAt != nil && now.Sub(*d.LastPolledAt) < devicePollFloor {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": fmt.Sprintf("polling faster than the %ds interval", int(devicePollInterval.Seconds())),
			"code":  "slow_down",
		})
		return
	}
	if err := s.db.TouchDeviceCode(hash); err != nil {
		log.Printf("cli login: stamping a poll: %v", err)
	}

	switch deviceStatus(d, now) {
	case deviceStatusPending:
		writeJSON(w, http.StatusOK, map[string]string{"status": deviceStatusPending})
		return
	case deviceStatusDenied, deviceStatusExpired:
		status := deviceStatus(d, now)
		// Nothing will ever collect this, so it stops being pollable now rather
		// than at the next sweep.
		if err := s.db.DeleteDeviceCode(hash); err != nil {
			log.Printf("cli login: dropping a %s request: %v", status, err)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
		return
	}

	s.mintDeviceKey(w, d, hash)
}

// mintDeviceKey is the approved case: an ordinary personal key, named for the
// machine that asked, created and audited in the same transaction that destroys
// the request it came from.
func (s *Server) mintDeviceKey(w http.ResponseWriter, d *database.DeviceCode, hash string) {
	owner, err := s.db.GetUser(d.ApprovedUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if owner == nil {
		// The approver's account went away between the approval and the poll.
		// Refusing is the only honest answer — there is nobody for this key to
		// be the identity of.
		s.db.DeleteDeviceCode(hash)
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "the account that approved this sign-in no longer exists",
			"code":  "access_denied",
			"hint":  "run toft login again and approve it with an account that exists",
		})
		return
	}

	name, err := s.deviceKeyName(owner.ID, d.Hostname)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	secret, err := generateUserAPIKeySecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	expires, err := s.signInKeyExpiry()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	key := database.UserAPIKey{
		ID:        uuid.New().String(),
		UserID:    owner.ID,
		Name:      name,
		KeyHash:   hashAPIKey(secret),
		KeyPrefix: keyDisplayPrefix(secret),
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expires,
	}

	// No request metadata: this write is not made by the person, it is made by
	// the poll that collects what the person already approved. The row that
	// records *who decided* is the approval's, written when they were looking
	// at the screen.
	audit := database.AuditEntry{
		ActorType:  database.PrincipalTypeUser,
		ActorID:    owner.ID,
		Action:     "user.api_key_create",
		TargetType: "user",
		TargetID:   owner.ID,
		After: jsonValStr(map[string]interface{}{
			"key_id": key.ID, "name": key.Name, "key_prefix": key.KeyPrefix,
			"expires_at": key.ExpiresAt, "via": "cli_login", "user_code": d.UserCode,
		}),
	}

	claimed, err := s.db.ClaimDeviceCode(hash, &key, audit)
	if err != nil {
		log.Printf("cli login: minting a key: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !claimed {
		// Another poll got here first. Single use means the second caller gets
		// nothing, and the answer is the same one an unknown code gets.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "that sign-in is not waiting — start a new one with `toft login`",
			"code":  "unknown_device_code",
			"hint":  "run toft login again to start a new sign-in",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": deviceStatusApproved,
		// The plaintext, for the only time it will ever be readable.
		"token": secret,
		"user": map[string]string{
			"id": owner.ID, "login": owner.Login, "email": owner.Email, "name": owner.Name,
		},
		"key": map[string]interface{}{
			"id": key.ID, "name": key.Name, "expires_at": key.ExpiresAt,
		},
	})
}

// deviceKeyName picks a name that says which machine holds this key, and that
// the owner does not already have — (user_id, name) is unique, and a developer
// signing in twice from the same laptop should get a second key rather than an
// error about a name they never chose.
func (s *Server) deviceKeyName(userID, hostname string) (string, error) {
	base := "toft"
	if hostname != "" {
		base = "toft on " + hostname
	}
	return s.uniqueKeyName(userID, base)
}

// uniqueKeyName is base, or base with the first free "(n)" after it, among the
// owner's keys. Shared with rsconnect's sign-in, which names its key for the
// same reason.
func (s *Server) uniqueKeyName(userID, base string) (string, error) {
	existing, err := s.db.ListUserAPIKeys(userID, 1)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(existing))
	for _, k := range existing {
		taken[k.Name] = true
	}
	if !taken[base] {
		return base, nil
	}
	for i := 2; i <= maxAPIKeysPerUser+1; i++ {
		candidate := fmt.Sprintf("%s (%d)", base, i)
		if !taken[candidate] {
			return candidate, nil
		}
	}
	// Unreachable while the per-user cap is below the loop bound, and a
	// distinct name beats an error if that ever stops being true.
	return fmt.Sprintf("%s (%d)", base, time.Now().UTC().Unix()), nil
}
